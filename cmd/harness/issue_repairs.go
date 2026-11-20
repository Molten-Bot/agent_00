package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

func (q *issueRepairQueue) capture(_ context.Context, issue localruntime.IssueClaimRecord) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	detail := failurefollowup.RedactFailureDetail(issue.Error)
	fingerprint := sha256.Sum256([]byte(issue.Step + "\n" + issue.Incident + "\n" + detail))
	id := hex.EncodeToString(fingerprint[:16])
	records, err := q.store.List()
	for _, record := range records {
		if record.ID == id {
			return record.RequestID, nil
		}
	}
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(q.store.Root, id+".json")); !errors.Is(statErr, os.ErrNotExist) {
			return "", err
		}
		q.logf("event=issue_repair_read status=error err=%q", err)
	}
	run := failureFollowUpRunConfig(issue.RequestID, app.Result{Err: errors.New(issue.Error)}, issue.Run, q.logRoot)
	record := localruntime.IssueRepairRecord{ID: id, RequestID: "repair-" + id, State: "queued", Run: run}
	return record.RequestID, q.store.Save(record)
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
		}
	}
}

func (q *issueRepairQueue) reconcile(ctx context.Context, enqueue localRunEnqueueFunc) {
	records, err := q.store.List()
	if err != nil {
		q.logf("event=issue_repairs status=error err=%q", err)
	}
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
		if record.State == "completed" || record.State == "blocked" || time.Now().Before(record.NextAttemptAt) {
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
		if record.State == "completed" || record.State == "blocked" || time.Now().Before(record.NextAttemptAt) {
			lock.Close()
			continue
		}
		if record.Started {
			record.State = "blocked"
			record.Error = "repair worker interrupted; inspect preserved logs before another repair attempt"
			if err := q.store.Save(record); err != nil {
				q.logf("event=issue_repair_save status=error err=%q", err)
			}
			lock.Close()
			continue
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
		_, err = enqueue(context.WithValue(admissionCtx, issueRepairContextKey{}, work), record.Run, false, issueRepairSource, true)
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
	if err := retainIssueEvidence(q.claims, q.store, q.logRoot, time.Now()); err != nil {
		q.logf("event=issue_evidence_retention status=error err=%q", err)
	}
}

func (w *issueRepairWork) start() error {
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
	if len(repos) != 1 || repository == "" || !strings.EqualFold(strings.TrimSuffix(strings.TrimPrefix(repos[0], "https://github.com/"), ".git"), repository) || record.Branch == "" {
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
		if repair.State != "completed" {
			blockedRepairs[repair.RequestID] = true
		}
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
