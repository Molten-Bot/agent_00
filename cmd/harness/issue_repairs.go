package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

const issueRepairSource = "issue_repair"
const issueRepairSourceFulfilledReason = "source issue already fulfilled"

var errIssueRepairSourceFulfilled = errors.New(issueRepairSourceFulfilledReason)
var errIssueRepairSourceCheck = errors.New("recheck reopened repair source")

type issueRepairContextKey struct{}
type issueRepairWork struct {
	queue  *issueRepairQueue
	record localruntime.IssueRepairRecord
	lock   *os.File
}
type issueRepairQueue struct {
	mu      sync.Mutex
	store   *localruntime.IssueRepairStore
	claims  *localruntime.IssueClaimStore
	logRoot string
	logf    func(string, ...any)
	runner  execx.Runner
	policy  localruntime.IssueRecoveryConfig
	wake    chan struct{}
}

func (q *issueRepairQueue) capture(_ context.Context, issue localruntime.IssueClaimRecord) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	id := issueRepairID(issue)
	fingerprint := sha256.Sum256([]byte(issue.ID + "\n" + id))
	claimRepairID := hex.EncodeToString(fingerprint[:16])
	records, err := q.store.List()
	var shared *localruntime.IssueRepairRecord
	for i, record := range records {
		if record.ID == claimRepairID {
			return record.RequestID, nil
		}
		if record.ID == id {
			shared = &records[i]
		}
	}
	if shared != nil {
		if shared.State != "blocked" || !isIssueRepairClaimBlock(shared.Error) || isIssueRepairSource(*shared, issue) {
			return shared.RequestID, nil
		}
		// A blocker specific to the source claim does not apply to this claim.
		id = claimRepairID
	}
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(q.store.Root, id+".json")); !errors.Is(statErr, os.ErrNotExist) {
			return "", err
		}
		q.logf("event=issue_repair_read status=error err=%q", err)
	}
	result := app.Result{ExitCode: issue.ExitCode}
	if failure := issue.FailureDetail(); failure != "" {
		result.Err = errors.New(failure)
	}
	run := failureFollowUpRunConfig(issue.RequestID, result, issue.Run, q.logRoot)
	record := localruntime.IssueRepairRecord{ID: id, RequestID: "repair-" + id, SourceClaimID: issue.ID, State: "queued", Run: run}
	err = q.store.Save(record)
	if err == nil {
		// Wake only after durable capture. The timer remains a restart/retry
		// fallback, rather than adding up to a minute to every new repair.
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
	return record.RequestID, err
}

func issueRepairID(issue localruntime.IssueClaimRecord) string {
	detail := failurefollowup.RedactFailureDetail(issue.FailureDetail())
	fingerprint := sha256.Sum256([]byte(issue.Step + "\n" + issue.Incident + "\n" + detail))
	return hex.EncodeToString(fingerprint[:16])
}

func (q *issueRepairQueue) run(ctx context.Context, enqueue localRunEnqueueFunc) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		q.reconcile(ctx, enqueue)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-q.wake:
		}
	}
}

func (q *issueRepairQueue) reconcile(ctx context.Context, enqueue localRunEnqueueFunc) {
	records, err := q.store.List()
	if err != nil {
		q.logf("event=issue_repairs status=error err=%q", err)
	}
	claimBlocked := make(map[string]localruntime.IssueRepairRecord)
	for _, record := range records {
		if record.PRURL != "" && record.State != "completed" && !time.Now().Before(record.NextAttemptAt) {
			if err := q.verifyPR(record); err == nil {
				record.State = "completed"
				record.Error = ""
			} else {
				record.Error = failurefollowup.RedactFailureDetail(err.Error())
				record.NextAttemptAt = time.Now().Add(time.Hour)
			}
			if err := q.store.Save(record); err != nil {
				q.logf("event=issue_repair_verify status=error err=%q", err)
			}
		}
		if issueRepairDispatchFinished(record) || time.Now().Before(record.NextAttemptAt) {
			if record.State == "blocked" && isIssueRepairClaimBlock(record.Error) {
				claimBlocked[record.RequestID] = record
			}
			continue
		}
		lock, err := q.claims.TryLock("repair-" + record.ID)
		if err != nil {
			q.logf("event=issue_repair_lock status=error err=%q", err)
			continue
		}
		if lock == nil {
			continue
		}
		// The worker may have finished after our initial list. Reload under the
		// execution lock before changing its durable state.
		fresh, readErr := q.store.List()
		if readErr != nil {
			q.logf("event=issue_repair_read status=error err=%q", readErr)
		}
		found := false
		for _, current := range fresh {
			if current.ID == record.ID {
				record = current
				found = true
				break
			}
		}
		if !found {
			lock.Close()
			continue
		}
		if issueRepairDispatchFinished(record) || time.Now().Before(record.NextAttemptAt) {
			lock.Close()
			continue
		}
		var blockedReason string
		failure := issueRepairFailureDetail(record.Run.Prompt)
		switch {
		case record.Started:
			blockedReason = "repair worker interrupted; inspect preserved logs before another repair attempt"
		case strings.Contains(failure, localruntime.LegacyIssueOwnershipFailure):
			blockedReason = localruntime.ErrIssueOwnershipChanged.Error()
		default:
			blockedReason, err = q.dependencyBlockReason(record, failure)
			if err != nil {
				q.logf("event=issue_repair_check_source status=error err=%q", err)
				lock.Close()
				continue
			}
		}
		if blockedReason != "" {
			if record.State == "blocked" && record.Error == blockedReason {
				claimBlocked[record.RequestID] = record
				lock.Close()
				continue
			}
			record.State = "blocked"
			record.Error = blockedReason
			if err := q.store.Save(record); err != nil {
				q.logf("event=issue_repair_save status=error err=%q", err)
			} else if isIssueRepairClaimBlock(blockedReason) {
				claimBlocked[record.RequestID] = record
			}
			lock.Close()
			continue
		}
		record.ReopenedRollout = record.ReopenedRollout || isSkippedRolloutRepair(record)
		if record.State == "blocked" {
			record.Error = ""
		}
		work := &issueRepairWork{queue: q, record: record, lock: lock}
		// Save dispatch intent before enqueue. Restart replays this same request ID.
		record.State = "running"
		if err := q.store.Save(record); err != nil {
			lock.Close()
			q.logf("event=issue_repair_save status=error err=%q", err)
			continue
		}
		admissionCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		run := record.Run
		if safe := failurefollowup.FailureDetailForPrompt(failure); safe != failure && failure != "" {
			run.Prompt = strings.Replace(run.Prompt, "\n- error="+strconv.Quote(failure)+"\n", "\n- error="+strconv.Quote(safe)+"\n", 1)
		}
		_, err = enqueue(context.WithValue(admissionCtx, issueRepairContextKey{}, work), run, false, issueRepairSource, true)
		cancel()
		if err != nil {
			lock.Close()
			record.State = "queued"
			record.Error = err.Error()
			record.NextAttemptAt = time.Now().Add(time.Minute)
			if saveErr := q.store.Save(record); saveErr != nil {
				q.logf("event=issue_repair_save status=error err=%q", saveErr)
			}
		}
	}
	q.releaseSharedBlockedRepairReceipts(claimBlocked)
	if err := retainIssueEvidence(q.claims, q.store, q.logRoot, time.Now()); err != nil {
		q.logf("event=issue_evidence_retention status=error err=%q", err)
	}
}

func issueRepairDispatchFinished(record localruntime.IssueRepairRecord) bool {
	return record.State == "completed" || (record.State == "blocked" &&
		(!isSkippedRolloutRepair(record) || issueRepairFailureDetail(record.Run.Prompt) == ""))
}

func isSkippedRolloutRepair(record localruntime.IssueRepairRecord) bool {
	return record.State == "blocked" && record.Error == failurefollowup.ExternalRolloutPrerequisitesReason && !record.Started && record.PRURL == ""
}

func isIssueRepairDependencyBlock(reason string) bool {
	return reason == failurefollowup.OpenDependencyReason || reason == failurefollowup.UnmergedDependencyReason ||
		reason == failurefollowup.ExternalRolloutPrerequisitesReason || reason == failurefollowup.HumanActionRequiredReason
}

func isIssueRepairClaimBlock(reason string) bool {
	return reason == issueRepairSourceFulfilledReason || isIssueRepairDependencyBlock(reason)
}

func (q *issueRepairQueue) releaseSharedBlockedRepairReceipts(blocked map[string]localruntime.IssueRepairRecord) {
	if len(blocked) == 0 {
		return
	}
	issues, err := q.claims.List()
	if err != nil {
		q.logf("event=issue_repair_read_claims status=error err=%q", err)
	}
	for _, issue := range issues {
		repair, ok := blocked[issue.RepairRequestID]
		if !ok || (repair.SourceClaimID == "" && issueRepairSourceLogDir(repair.Run.Prompt) == "") || isIssueRepairSource(repair, issue) {
			continue
		}
		// Older fingerprint-only IDs could give unrelated claims the same
		// receipt. Let normal issue recovery reconsider those claims.
		lock, err := q.claims.TryLock(localruntime.IssueExecutionKey(issue.Run.GitHubIssue))
		if err == nil && lock != nil {
			err = q.claims.Update(issue.ID, func(current *localruntime.IssueClaimRecord) error {
				if current.RepairRequestID == repair.RequestID {
					current.RepairRequestID = ""
				}
				return nil
			})
			lock.Close()
		}
		if err != nil {
			q.logf("event=issue_repair_release_receipt status=error err=%q", err)
		}
	}
}

func (q *issueRepairQueue) dependencyBlockReason(record localruntime.IssueRepairRecord, failure string) (string, error) {
	if reason := failurefollowup.NonRemediableFailureReason(errors.New(failure)); isIssueRepairDependencyBlock(reason) {
		return reason, nil
	}
	// The source claim may have been classified since capture. Its run error
	// also preserves details that were redacted from the repair prompt.
	hasSource := record.SourceClaimID != "" || issueRepairSourceLogDir(record.Run.Prompt) != ""
	issues, err := q.claims.List()
	foundSource := false
	matchedLogs := false
	for _, issue := range issues {
		// Receipts and fingerprints could be shared across claims in older
		// records. Use them only when no explicit source was recorded.
		if hasSource {
			if !isIssueRepairSource(record, issue) {
				continue
			}
			// Admission errors return reopened rollout repairs to the queue.
			if (record.ReopenedRollout || isSkippedRolloutRepair(record)) && !record.Started && record.PRURL == "" &&
				issueRepairSourceFulfilled(issue, issues) {
				return issueRepairSourceFulfilledReason, nil
			}
		} else if issue.RepairRequestID != record.RequestID && issueRepairID(issue) != record.ID {
			continue
		}
		foundSource = true
		switch issue.Incident {
		case localruntime.IssueIncidentUnmergedDependency:
			return failurefollowup.UnmergedDependencyReason, nil
		case localruntime.IssueIncidentOpenDependency:
			return failurefollowup.OpenDependencyReason, nil
		case localruntime.IssueIncidentHumanAction:
			return failurefollowup.HumanActionRequiredReason, nil
		}
		sourceFailure := issue.FailureDetail()
		if reason := failurefollowup.NonRemediableFailureReason(errors.New(sourceFailure)); isIssueRepairDependencyBlock(reason) {
			return reason, nil
		}
		blocked, matched := q.rolloutBlockedFromLogs(issue.RequestID, sourceFailure)
		if !matched && failure != failurefollowup.RedactFailureDetail(sourceFailure) {
			blocked, matched = q.rolloutBlockedFromLogs(issue.RequestID, failure)
		}
		matchedLogs = matchedLogs || matched
		if blocked {
			if issue.Incident == localruntime.IssueIncidentExternalRollout {
				return failurefollowup.ExternalRolloutPrerequisitesReason, nil
			}
			// Record the retry blocker before the outbox stops checking this report.
			if err := q.persistRecoveredRolloutBlock(issue); err != nil {
				return "", err
			}
			return failurefollowup.ExternalRolloutPrerequisitesReason, nil
		}
		if issue.Incident == localruntime.IssueIncidentExternalRollout && !matched && failurefollowup.HasTruncatedErrorDetails(sourceFailure) {
			// Missing stdout cannot disprove a blocker recorded for this report.
			return failurefollowup.ExternalRolloutPrerequisitesReason, nil
		}
	}
	if err != nil && foundSource {
		q.logf("event=issue_repair_check_source status=partial err=%q", err)
		err = nil
	}
	if isSkippedRolloutRepair(record) && failurefollowup.HasTruncatedErrorDetails(failure) && !matchedLogs {
		return failurefollowup.ExternalRolloutPrerequisitesReason, err
	}
	return "", err
}

func issueRepairSourceFulfilled(source localruntime.IssueClaimRecord, issues []localruntime.IssueClaimRecord) bool {
	if source.State == "fulfilled" {
		return true
	}
	issue := source.Run.GitHubIssue
	key := localruntime.IssueExecutionKey(issue)
	for _, current := range issues {
		other := current.Run.GitHubIssue
		if current.State == "fulfilled" && other.Claim.Attempt > issue.Claim.Attempt && localruntime.IssueExecutionKey(other) == key {
			return true
		}
	}
	return false
}

func (q *issueRepairQueue) rolloutBlockedFromLogs(requestID, failure string) (blocked, matched bool) {
	return app.CheckExternalRolloutFailureFromStdout(failure, func(stage string) string {
		return issueRepairAgentStdout(q.logRoot, requestID, stage)
	})
}

func (q *issueRepairQueue) persistRecoveredRolloutBlock(issue localruntime.IssueClaimRecord) error {
	lock, err := q.claims.TryLock(localruntime.IssueExecutionKey(issue.Run.GitHubIssue))
	if err != nil {
		return err
	}
	if lock == nil {
		return errors.New("source issue claim is locked")
	}
	defer lock.Close()
	return q.claims.Update(issue.ID, func(current *localruntime.IssueClaimRecord) error {
		if current.RequestID != issue.RequestID || current.FailureDetail() != issue.FailureDetail() || current.Incident != issue.Incident {
			return errors.New("source issue claim changed during rollout recovery")
		}
		if current.Outcome == "error" && current.State != "fulfilled" {
			current.Incident = localruntime.IssueIncidentExternalRollout
			current.RunError = current.FailureDetail()
			current.NextAttemptAt = time.Time{}
		}
		return nil
	})
}

func issueRepairAgentStdout(logRoot, requestID, stage string) string {
	dir, ok := failurefollowup.TaskLogDir(logRoot, requestID)
	if !ok {
		return ""
	}
	prefix := "task request_id=" + requestID + " cmd phase=" + stage + " name="
	for _, name := range []string{logFileName, legacyTaskLogFileName} {
		if stdout := readIssueRepairAgentStdout(filepath.Join(dir, name), prefix); stdout != "" {
			return stdout
		}
	}
	return ""
}

func readIssueRepairAgentStdout(path, prefix string) string {
	log, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer log.Close()
	scanner := bufio.NewScanner(log)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var stdout strings.Builder
	for scanner.Scan() {
		command, ok := strings.CutPrefix(scanner.Text(), prefix)
		if !ok {
			continue
		}
		_, stream, _ := strings.Cut(command, " stream=")
		// Rendered stdout may have been shortened or filtered.
		if strings.HasPrefix(stream, "stdout text=") {
			return ""
		}
		encoded, ok := strings.CutPrefix(stream, "stdout b64=")
		if !ok {
			continue
		}
		line, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return ""
		}
		stdout.Write(line)
		stdout.WriteByte('\n')
	}
	if scanner.Err() != nil {
		return ""
	}
	return stdout.String()
}

func issueRepairSourceLogDir(prompt string) string {
	_, logSection, _ := strings.Cut(prompt, "\n\nRelevant failing log path(s):\n- ")
	logDir, _, _ := strings.Cut(logSection, "\n")
	if logDir == ".log/local/<request timestamp>/<request sequence>" {
		return ""
	}
	return logDir
}

func isIssueRepairSource(record localruntime.IssueRepairRecord, issue localruntime.IssueClaimRecord) bool {
	if record.SourceClaimID != "" {
		return record.SourceClaimID == issue.ID
	}
	// Legacy prompts store the log directory at capture time. Compare its
	// request-specific suffix so restarts can use a different log root.
	logDir := issueRepairSourceLogDir(record.Run.Prompt)
	subdir, ok := failurefollowup.IdentifierSubdir(issue.RequestID)
	return logDir != "" && ok && (logDir == subdir || strings.HasSuffix(logDir, string(filepath.Separator)+subdir))
}

func issueRepairFailureDetail(prompt string) string {
	// The stored prompt is available even if capture's receipt never made it
	// into the source claim. The exit code was optional in older records.
	_, context, ok := strings.Cut(prompt, "\n\nObserved failure context:\n")
	if !ok {
		return ""
	}
	line, rest, _ := strings.Cut(context, "\n")
	if strings.HasPrefix(line, "- exit_code=") {
		line, _, _ = strings.Cut(rest, "\n")
	}
	quoted, ok := strings.CutPrefix(line, "- error=")
	if !ok {
		return ""
	}
	failure, _ := strconv.Unquote(quoted)
	return failure
}

func (w *issueRepairWork) start() error {
	if w.record.Started {
		return nil
	}
	if w.record.ReopenedRollout {
		// The source may have been fulfilled while the scheduler held this repair.
		issues, err := w.queue.claims.List()
		foundSource := false
		for _, issue := range issues {
			if !isIssueRepairSource(w.record, issue) {
				continue
			}
			foundSource = true
			if issueRepairSourceFulfilled(issue, issues) {
				return errIssueRepairSourceFulfilled
			}
		}
		if err != nil {
			if !foundSource {
				return fmt.Errorf("%w: %w", errIssueRepairSourceCheck, err)
			}
			// List returns valid claims alongside errors for malformed records.
			// An unrelated unreadable claim must not starve this source's repair.
			w.queue.logf("event=issue_repair_check_source status=partial err=%q", err)
		}
	}
	w.queue.mu.Lock()
	defer w.queue.mu.Unlock()
	w.record.Started = true
	w.record.State = "running"
	return w.queue.store.Save(w.record)
}

func (w *issueRepairWork) finish(state string, result app.Result) {
	defer w.lock.Close()
	record := w.record
	record.PRURL = result.PRURL
	record.Branch = result.Branch
	record.State = "blocked"
	record.Error = "repair attempt ended without a verified PR; inspect its logs"
	if result.Err != nil {
		record.Error = failurefollowup.RedactFailureDetail(result.Err.Error())
	}
	if errors.Is(result.Err, errIssueRepairSourceCheck) {
		record.State = "queued"
		record.NextAttemptAt = time.Now().Add(time.Minute)
	}
	if resultHasPR(result) {
		if err := w.queue.verifyPR(record); err == nil {
			record.State = "completed"
			record.Error = ""
		} else {
			record.Error = failurefollowup.RedactFailureDetail(err.Error())
			record.NextAttemptAt = time.Now().Add(time.Minute)
		}
	}
	w.queue.mu.Lock()
	defer w.queue.mu.Unlock()
	if err := w.queue.store.Save(record); err != nil {
		w.queue.logf("event=issue_repair_finish status=error err=%q", err)
	}
}

func (q *issueRepairQueue) verifyPR(record localruntime.IssueRepairRecord) error {
	repository := githubutil.PullRequestRepository(record.PRURL)
	repos := record.Run.RepoList()
	if len(repos) != 1 || repository == "" || !githubutil.RepositoryMatches(repos[0], repository) || record.Branch == "" {
		return fmt.Errorf("repair PR identity could not be verified")
	}
	if q.runner == nil {
		return fmt.Errorf("repair PR verifier unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := q.runner.Run(ctx, execx.Command{Name: "gh", Args: []string{"pr", "view", record.PRURL, "--repo", repository, "--json", "url,state,mergedAt,headRefName"}})
	if err != nil {
		return err
	}
	var pr struct{ URL, State, MergedAt, HeadRefName string }
	if err := json.Unmarshal([]byte(res.Stdout), &pr); err != nil {
		return err
	}
	if pr.URL != record.PRURL || pr.HeadRefName != record.Branch || (!strings.EqualFold(pr.State, "OPEN") && pr.MergedAt == "") {
		return fmt.Errorf("repair PR is not open or merged on its recorded branch")
	}
	return nil
}

// Resolved evidence has a 30-day retention. Never remove a workspace still
// referenced by an unresolved attempt or repair. The journal itself is retained.
func retainIssueEvidence(claims *localruntime.IssueClaimStore, repairs *localruntime.IssueRepairStore, logRoot string, now time.Time) error {
	records, err := claims.List()
	if err != nil {
		return err
	}
	repairRecords, err := repairs.List()
	if err != nil {
		return err
	}
	blockedRepairs := map[string]bool{}
	for _, repair := range repairRecords {
		if repair.State == "completed" {
			continue
		}
		// Repairs skipped before dispatch follow the fulfilled issue's retention window.
		if isSkippedRolloutRepair(repair) || (repair.State == "blocked" && repair.Error == issueRepairSourceFulfilledReason) {
			continue
		}
		blockedRepairs[repair.RequestID] = true
	}
	protectedIssues := map[string]bool{}
	latest := map[string]localruntime.IssueClaimRecord{}
	for _, record := range records {
		key := localruntime.IssueExecutionKey(record.Run.GitHubIssue)
		if blockedRepairs[record.RepairRequestID] {
			protectedIssues[key] = true
		}
		old, ok := latest[key]
		if !ok || record.Run.GitHubIssue.Claim.Attempt > old.Run.GitHubIssue.Claim.Attempt {
			latest[key] = record
		}
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(claims.Root), "issue-workspaces"))
	if err != nil {
		return err
	}
	for _, record := range records {
		key := localruntime.IssueExecutionKey(record.Run.GitHubIssue)
		last := latest[key]
		if protectedIssues[key] || last.State != "fulfilled" || last.VerifiedAt.IsZero() || now.Sub(last.VerifiedAt) < 30*24*time.Hour {
			continue
		}
		err := func() error {
			lock, err := claims.TryLock(key)
			if err != nil {
				return err
			}
			if lock == nil {
				return nil
			}
			defer lock.Close()
			fresh, err := claims.List()
			if err != nil {
				return err
			}
			for _, current := range fresh {
				if localruntime.IssueExecutionKey(current.Run.GitHubIssue) == key && current.Run.GitHubIssue.Claim.Attempt > last.Run.GitHubIssue.Claim.Attempt {
					return nil
				}
			}
			if record.WorkspaceDir != "" {
				path, err := filepath.Abs(record.WorkspaceDir)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(root, path)
				if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
					return fmt.Errorf("refusing cleanup outside persistent issue workspaces")
				}
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
			if path, ok := taskLogDir(logRoot, record.RequestID); ok {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}
