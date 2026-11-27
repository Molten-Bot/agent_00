package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workflow"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

const issueEvidenceRetention = 30 * 24 * time.Hour

var issueWorkspaceName = regexp.MustCompile(`^[a-f0-9]{32}$`)

type issueRetentionStats struct {
	reclaimedBytes uint64
	protected      int
}

type issueArtifactClaim struct {
	store   *localruntime.IssueClaimStore
	key     string
	expires time.Time // Zero means recovery or audit evidence is still needed.
}

// Run under the daemon's instance lock and issue admission mutex. The older
// queue layout may have its own daemon and journal, so lock it and read both
// journals before acting on workspace IDs shared by the two layouts.
func retainIssueEvidence(ctx context.Context, claims *localruntime.IssueClaimStore, repairs *localruntime.IssueRepairStore, logRoot string, engine *workflow.Engine, now time.Time) (issueRetentionStats, error) {
	var stats issueRetentionStats
	base, err := filepath.Abs(filepath.Dir(claims.Root))
	if err != nil {
		return stats, err
	}
	legacy := filepath.Join(base, "issue-queue")
	if filepath.Base(base) == "issue-queue" {
		legacy = filepath.Dir(base)
	}
	legacyClaims := &localruntime.IssueClaimStore{Root: filepath.Join(legacy, "issue-claims")}
	bases := []string{base}
	stores := []*localruntime.IssueClaimStore{claims}
	repairStores := []*localruntime.IssueRepairStore{repairs}
	engines := []*workflow.Engine{engine}
	info, err := os.Lstat(legacy)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return stats, err
	case !info.IsDir():
		return stats, fmt.Errorf("issue queue path %s is not a directory", legacy)
	default:
		// With no legacy journal there is no legacy daemon to lock. A daemon
		// starting there must acquire this same lock before loading any claims.
		journal, err := openIssueArtifactRoot(legacyClaims.Root)
		if err != nil {
			return stats, err
		}
		if journal != nil {
			journal.Close()
		}
		lock, err := legacyClaims.TryControlLock("instance")
		if err != nil {
			return stats, err
		}
		if lock == nil {
			return stats, fmt.Errorf("issue runtime at %s is in use", legacy)
		}
		defer lock.Close()
		bases = append(bases, legacy)
		stores = append(stores, legacyClaims)
		repairStores = append(repairStores, &localruntime.IssueRepairStore{Root: filepath.Join(legacy, "issue-repairs")})
		engines = append(engines, &workflow.Engine{StatePath: filepath.Join(legacy, "workflow-state.json")})
	}

	workspaceClaims := map[string][]issueArtifactClaim{}
	logClaims := map[string][]issueArtifactClaim{}
	reviewClaims := map[string][]issueArtifactClaim{}
	protectedRuns := map[string]bool{}
	for _, engine := range engines {
		if engine == nil {
			continue
		}
		runs, err := engine.Runs()
		if err != nil {
			return stats, fmt.Errorf("read workflow recovery state: %w", err)
		}
		for _, run := range runs {
			protectedRuns[run.ID] = true
			if run.RequestID != "" {
				protectedRuns[run.RequestID] = true
			}
		}
	}
	settledRepairs := map[string]bool{}
	repairSources := map[string]bool{}
	for _, store := range repairStores {
		repairs, err := store.List()
		if err != nil {
			return stats, err
		}
		for _, repair := range repairs {
			settled := repair.State == "completed" || isSkippedRolloutRepair(repair) ||
				(repair.State == "blocked" && repair.Error == issueRepairSourceFulfilledReason)
			if previous, exists := settledRepairs[repair.RequestID]; exists {
				settled = settled && previous
			}
			settledRepairs[repair.RequestID] = settled
			if !settled {
				repairSources[repair.SourceClaimID] = true
			}
		}
	}
	for _, store := range stores {
		records, err := store.List()
		if err != nil {
			return stats, err
		}
		latest := map[string]localruntime.IssueClaimRecord{}
		protectedIssues := map[string]bool{}
		for _, record := range records {
			key := localruntime.IssueExecutionKey(record.Run.GitHubIssue)
			old := latest[key]
			if old.ID == "" || record.Run.GitHubIssue.Claim.Attempt > old.Run.GitHubIssue.Claim.Attempt ||
				(record.Run.GitHubIssue.Claim.Attempt == old.Run.GitHubIssue.Claim.Attempt && record.UpdatedAt.After(old.UpdatedAt)) {
				latest[key] = record
			}
			policy := record.Run.Workflow
			if (record.RepairRequestID != "" && !settledRepairs[record.RepairRequestID]) ||
				repairSources[record.ID] || protectedRuns[record.RequestID] ||
				(policy != nil && protectedRuns[policy.EventID]) {
				protectedIssues[key] = true
			}
		}
		for _, record := range records {
			key := localruntime.IssueExecutionKey(record.Run.GitHubIssue)
			last := latest[key]
			ref := issueArtifactClaim{store: store, key: key}
			if !protectedIssues[key] && !last.VerifiedAt.IsZero() && last.Control == "" && last.Fulfilled() &&
				!last.UnlockPending && !last.LabelAdded && last.Outcome != workflow.OutcomeReviewPending &&
				(!last.ReviewRequired || last.ReviewHead != "") && record.PRVerification == "" &&
				!record.UnlockPending && !record.LabelAdded && record.ExecutionAgentAttempts == nil &&
				(record.Fulfilled() || supersededIssueOutcome(record, last)) {
				stopped, err := localruntime.IssueProcessesStopped(record)
				if err == nil && stopped {
					pending, err := issueReviewRetained(bases, record, last.ID == record.ID)
					if err != nil {
						return stats, err
					}
					if !pending {
						ref.expires = last.VerifiedAt.Add(issueEvidenceRetention)
					}
				}
			}
			workspaceClaims[record.ID] = append(workspaceClaims[record.ID], ref)
			// Retries can reference an earlier ID. Match names in both roots,
			// including records written with a different absolute mount path.
			dir, _ := filepath.Abs(record.WorkspaceDir)
			name, parent := filepath.Base(dir), filepath.Dir(dir)
			if name != record.ID && filepath.Base(parent) == "issue-workspaces" && issueWorkspaceName.MatchString(name) {
				if ref.expires.IsZero() {
					workspaceClaims[name] = append(workspaceClaims[name], ref)
				} else {
					for _, root := range bases {
						if parent == filepath.Join(root, "issue-workspaces") {
							workspaceClaims[dir] = append(workspaceClaims[dir], ref)
						}
					}
				}
			}
			if path, ok := taskLogDir(".", record.RequestID); ok && path != fallbackLogSubdir {
				logClaims[path] = append(logClaims[path], ref)
			}
			if record.RequestID != "" {
				name := filepath.Base(workflow.ReviewOutcomePath(".", record.RequestID))
				reviewClaims[name] = append(reviewClaims[name], ref)
			}
		}
	}

	var problems []error
	for _, root := range bases {
		if ctx.Err() != nil {
			break
		}
		problems = append(problems,
			retainIssueWorkspaces(ctx, root, workspaceClaims, now, &stats),
			expireIssueArtifacts(ctx, filepath.Join(root, "review-receipts"), reviewClaims, now, &stats, nil))
	}
	if logRoot != "" {
		problems = append(problems, expireIssueArtifacts(ctx, logRoot, logClaims, now, &stats, []string{logFileName, legacyTaskLogFileName}))
	}
	if len(bases) > 1 {
		dir, err := openIssueArtifactRoot(filepath.Join(legacy, "logs"))
		if err != nil {
			problems = append(problems, err)
		} else if dir != nil {
			dir.Close()
			problems = append(problems, expireIssueArtifacts(ctx, filepath.Join(legacy, "logs", ".log"), logClaims, now, &stats, []string{logFileName, legacyTaskLogFileName}))
		}
	}
	return stats, errors.Join(problems...)
}

func issueReviewRetained(bases []string, record localruntime.IssueClaimRecord, latest bool) (bool, error) {
	if record.RequestID == "" {
		return false, nil
	}
	policy := record.Run.Workflow
	for _, root := range bases {
		outcome, err := workflow.LoadReviewOutcome(root, record.RequestID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("read review outcome for %s: %w", record.ID, err)
		}
		// The workflow journal tracks outstanding writeback. Its older task
		// receipt can still say writeback is pending after the engine finished.
		if (policy == nil || policy.IsDefaultExecution()) &&
			(outcome.Status == workflow.OutcomeReviewPending || outcome.PendingReviewGate != nil) {
			return true, nil
		}
		if latest && outcome.ReviewRequired && (outcome.ReviewReceipt == nil || !outcome.ReviewReceipt.Clean()) {
			return true, nil
		}
	}
	return false, nil
}

func supersededIssueOutcome(record, last localruntime.IssueClaimRecord) bool {
	if record.Run.GitHubIssue.Claim.Attempt >= last.Run.GitHubIssue.Claim.Attempt || record.PRMerged() {
		return false
	}
	switch record.Outcome {
	case "completed", "error", "interrupted", "no_changes", "stopped":
		return true
	default:
		return false
	}
}

// Hold each referring issue's worker lock through deletion. The admission
// mutex prevents a retry from adding or reusing a reference during the sweep.
func withTerminalIssueArtifacts(refs []issueArtifactClaim, clean func(time.Time) error) (bool, error) {
	if len(refs) == 0 {
		return false, nil
	}
	var expires time.Time
	for _, ref := range refs {
		if ref.expires.IsZero() {
			return false, nil
		}
		if ref.expires.After(expires) {
			expires = ref.expires
		}
	}
	locked := map[*localruntime.IssueClaimStore]map[string]bool{}
	for _, ref := range refs {
		if locked[ref.store] == nil {
			locked[ref.store] = map[string]bool{}
		}
		if locked[ref.store][ref.key] {
			continue
		}
		lock, err := ref.store.TryControlLock(ref.key)
		if err != nil || lock == nil {
			return false, err
		}
		defer lock.Close()
		locked[ref.store][ref.key] = true
	}
	return true, clean(expires)
}

func retainIssueWorkspaces(ctx context.Context, base string, claims map[string][]issueArtifactClaim, now time.Time, stats *issueRetentionStats) error {
	root, err := openIssueArtifactRoot(filepath.Join(base, "issue-workspaces"))
	if err != nil || root == nil {
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	var problems []error
	for _, entry := range entries {
		if ctx.Err() != nil {
			break
		}
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(root.Name(), name)
		refs := append(append([]issueArtifactClaim(nil), claims[name]...), claims[path]...)
		cleaned, err := withTerminalIssueArtifacts(refs, func(expires time.Time) error {
			if err := workspace.CheckNoMountsWithin(path); err != nil {
				return err
			}
			if !now.Before(expires) {
				return reclaimIssueArtifact(root, name, stats)
			}
			contents, err := fs.ReadDir(root.FS(), name)
			if err != nil {
				return err
			}
			var problems []error
			keepWorkspace := false
			for _, child := range contents {
				if !child.IsDir() && strings.HasPrefix(child.Name(), "review-") &&
					(strings.HasSuffix(child.Name(), ".receipt.json") || strings.HasSuffix(child.Name(), ".pending.json")) {
					keepWorkspace = true
					continue
				}
				problems = append(problems, reclaimIssueArtifact(root, filepath.Join(name, child.Name()), stats))
			}
			if err := errors.Join(problems...); err != nil {
				return err
			}
			if !keepWorkspace {
				return reclaimIssueArtifact(root, name, stats)
			}
			return nil
		})
		if err != nil {
			problems = append(problems, fmt.Errorf("clean issue workspace %s: %w", filepath.Join(root.Name(), name), err))
		}
		if !cleaned {
			stats.protected++
		}
	}
	return errors.Join(problems...)
}

func expireIssueArtifacts(ctx context.Context, path string, claims map[string][]issueArtifactClaim, now time.Time, stats *issueRetentionStats, files []string) error {
	if ctx.Err() != nil {
		return nil
	}
	root, err := openIssueArtifactRoot(path)
	if err != nil || root == nil {
		return err
	}
	defer root.Close()
	var problems []error
	for name, refs := range claims {
		if ctx.Err() != nil {
			break
		}
		// Request IDs map to directory paths that can share prefixes. Remove
		// only the known task log files, so another request's logs stay intact.
		_, err := withTerminalIssueArtifacts(refs, func(expires time.Time) error {
			if now.Before(expires) {
				return nil
			}
			if len(files) == 0 {
				return reclaimIssueFile(root, name, stats)
			}
			// Reject symlinks in the task-log path even if they stay within
			// the log root. A symlink can redirect into another task's logs.
			for dir := name; dir != "."; dir = filepath.Dir(dir) {
				info, err := root.Lstat(dir)
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if !info.IsDir() {
					return fmt.Errorf("issue log path %s is not a directory", dir)
				}
			}
			var problems []error
			for _, file := range files {
				problems = append(problems, reclaimIssueFile(root, filepath.Join(name, file), stats))
			}
			if err := errors.Join(problems...); err != nil {
				return err
			}
			entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(name))
			if err != nil || len(entries) != 0 {
				return err
			}
			return root.Remove(name)
		})
		if err != nil {
			problems = append(problems, fmt.Errorf("expire issue evidence at %s: %w", filepath.Join(path, name), err))
		}
	}
	return errors.Join(problems...)
}

func openIssueArtifactRoot(path string) (*os.Root, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("issue artifact path %s is not a directory", path)
	}
	return os.OpenRoot(path)
}

func reclaimIssueFile(root *os.Root, name string, stats *issueRetentionStats) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("expected a file at %s", name)
	}
	return reclaimIssueArtifact(root, name, stats)
}

func reclaimIssueArtifact(root *os.Root, name string, stats *issueRetentionStats) error {
	if _, err := root.Lstat(name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	path := filepath.Join(root.Name(), name)
	bytes, err := workspace.DiskUsage(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrPermission) {
		return err
	}
	if err := removeWorkspaceArtifact(root, name); err != nil {
		remaining, _ := workspace.DiskUsage(path)
		stats.reclaimedBytes += bytes - min(bytes, remaining)
		return err
	}
	stats.reclaimedBytes += bytes
	return nil
}
