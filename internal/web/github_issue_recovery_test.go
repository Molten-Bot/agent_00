package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

type recoveryGitHub struct {
	*automationGitHub
	prs             string
	deleteError     bool
	deletes         int
	loseTagResponse bool
}

func (f *recoveryGitHub) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, "/pulls?") {
		return execx.Result{Stdout: f.prs}, nil
	}
	if strings.Contains(args, "--method DELETE") {
		f.deletes++
		if f.deleteError {
			return execx.Result{}, errors.New("temporary label removal failure")
		}
		labels := []GitHubIssueLabel{}
		for _, label := range f.issue.Labels {
			if label.Name != localruntime.DefaultGitHubIssueExclusionLabel {
				labels = append(labels, label)
			}
		}
		f.issue.Labels = labels
		return execx.Result{Stdout: `[]`}, nil
	}
	if strings.Contains(args, "/git/ref/heads/") && len(f.submissions) == 0 {
		return execx.Result{Stdout: `{"object":{"sha":"base"}}`}, nil
	}
	res, err := f.automationGitHub.Run(ctx, cmd)
	if err == nil && f.loseTagResponse && strings.Contains(args, "--method POST") && strings.Contains(args, "/git/refs") {
		return execx.Result{}, errors.New("lost tag response")
	}
	return res, err
}
func recoveryServer(t *testing.T) (Server, *recoveryGitHub) {
	t.Helper()
	f := &recoveryGitHub{automationGitHub: newAutomationGitHub(), prs: `[]`}
	s := automationServer(t, f.automationGitHub)
	s.GitHubIssueRunner = f
	s.IssueRecovery = &localruntime.IssueRecoveryConfig{}
	return s, f
}
func latestRecoveryRecord(t *testing.T, s Server) localruntime.IssueClaimRecord {
	t.Helper()
	records, err := s.IssueClaims.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("no durable records")
	}
	latest := records[0]
	for _, record := range records {
		if record.Run.GitHubIssue.Claim.Attempt > latest.Run.GitHubIssue.Claim.Attempt {
			latest = record
		}
	}
	return latest
}
func completeRecoveryRecord(t *testing.T, s Server, outcome string, attempts int) localruntime.IssueClaimRecord {
	t.Helper()
	record := latestRecoveryRecord(t, s)
	record.Outcome = outcome
	record.State = "verifying"
	record.AgentAttempts = attempts
	record.StoppedAt = time.Now()
	record.UnlockPending = true
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	return record
}

const recoveryPRTitle = "Manually opened PR"

func recoveryPR(state, merged, head, base string) string {
	return `[{"number":42,"title":"` + recoveryPRTitle + `","state":"` + state + `","merged_at":` + merged + `,"head":{"ref":"` + head + `","sha":"existing-commits","repo":{"full_name":"acme/repo"}},"base":{"ref":"` + base + `","repo":{"full_name":"acme/repo"}}}]`
}

func TestExplicitIssueRecoveryUsesPersistentOwner(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"start", "recover"} {
		t.Run(action, func(t *testing.T) {
			s, f := recoveryServer(t)
			s.pollGitHubIssueAutoStart(context.Background())
			previous := completeRecoveryRecord(t, s, "stopped", 3)
			previous.PriorRunFailures = 2
			previous.NextAttemptAt = time.Now().Add(time.Hour)
			if err := s.IssueClaims.Save(previous); err != nil {
				t.Fatal(err)
			}
			// Simulate a cached identity left behind by an earlier server configuration.
			s.issueAutomation.instanceID = strings.Repeat("f", 32)
			s.SubmitLocalPrompt = func(ctx context.Context, body []byte) (string, error) {
				run, err := localruntime.ParseRunConfigJSON(body)
				if err != nil {
					return "", err
				}
				record, err := s.IssueClaims.Load(run.GitHubIssue.Claim.ID)
				if err != nil {
					return "", err
				}
				owner, err := s.IssueClaims.Identity()
				if err != nil {
					return "", err
				}
				if record.InstanceID != owner {
					return "", errors.New("issue ownership changed")
				}
				if err := s.ValidateIssueOwnership(ctx, run); err != nil {
					return "", err
				}
				return f.submit(ctx, body)
			}
			if action == "recover" {
				response := retryIssue(t, s, previous.ID, true)
				if response.Code != http.StatusAccepted {
					t.Fatalf("recovery=%d %s", response.Code, response.Body.String())
				}
			} else {
				body, err := json.Marshal(previous.Run)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.submitWithIssueClaim(context.Background(), body, s.SubmitLocalPrompt); err != nil {
					t.Fatalf("explicit start: %v", err)
				}
			}
			latest := latestRecoveryRecord(t, s)
			if len(f.submissions) != 2 || latest.ID == previous.ID || latest.Run.GitHubIssue.Claim.Attempt != previous.Run.GitHubIssue.Claim.Attempt+1 {
				t.Fatal("explicit recovery did not submit a fresh attempt")
			}
			if latest.Run.GitHubIssue.Claim.Branch != previous.Run.GitHubIssue.Claim.Branch || latest.AgentAttempts != 0 || issueRunFailures(latest) != 0 || latest.Outcome != "" {
				t.Fatal("explicit recovery lost the branch or retained the previous retry limit")
			}
		})
	}
}

func TestIssueRecoveryRetryAfterDurableBranchReservationFailure(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"matching intent", "different owner", "previously reserved"} {
		t.Run(scenario, func(t *testing.T) {
			s, f := recoveryServer(t)
			f.fail = "query=mutation"
			s.pollGitHubIssueAutoStart(context.Background())
			record := latestRecoveryRecord(t, s)
			if !record.RemoteRecorded || record.BranchReserved || f.branch || len(f.submissions) != 0 {
				t.Fatalf("expected ownership without branch: %+v", record)
			}
			f.fail = ""
			switch scenario {
			case "different owner":
				record.InstanceID = "another-instance"
			case "previously reserved":
				record.BranchReserved = true
			}
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			response := retryIssue(t, s, record.ID, true)
			if response.Code != http.StatusAccepted || !f.branch || len(f.submissions) != 1 {
				t.Fatalf("recovery: %d %s", response.Code, response.Body.String())
			}
			if claim := f.submissions[0].GitHubIssue.Claim; claim.Attempt != 2 || claim.ID == record.ID {
				t.Fatalf("expected new serialized attempt: %+v", claim)
			}
		})
	}
}

func TestIssueRecoveryBlocksStaleAdmissionUntilExplicitRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	s.SubmitLocalPrompt = func(context.Context, []byte) (string, error) {
		return "", errors.New("issue owner is unavailable or changed: %!w(<nil>)")
	}
	s.pollGitHubIssueAutoStart(ctx)
	previous := latestRecoveryRecord(t, s)
	if previous.State != "retry_wait" || !previous.UnlockPending || previous.AgentAttempts != 0 {
		t.Fatalf("unexpected failed admission state: %s, unlock pending=%t, agent attempts=%d", previous.State, previous.UnlockPending, previous.AgentAttempts)
	}
	previous.Failures = 4
	previous.NextAttemptAt = time.Now().Add(time.Hour)
	if repairableIssueIncident(previous) {
		t.Fatal("historical ownership failure would enqueue a repair")
	}
	if err := s.IssueClaims.Save(previous); err != nil {
		t.Fatal(err)
	}
	owner := strings.Repeat("f", 32)
	if err := os.WriteFile(filepath.Join(s.IssueClaims.Root, "instance"), []byte(`"`+owner+`"`), 0600); err != nil {
		t.Fatal(err)
	}
	s.SubmitLocalPrompt = f.submit
	commands := len(f.trace)
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	blocked := latestRecoveryRecord(t, s)
	if blocked.State != "blocked" || blocked.Error != localruntime.ErrIssueOwnershipChanged.Error() || !blocked.NextAttemptAt.IsZero() || !blocked.UnlockPending {
		t.Fatalf("stale admission state=%s error=%q retry=%v unlock pending=%t", blocked.State, blocked.Error, blocked.NextAttemptAt, blocked.UnlockPending)
	}
	if len(f.trace) != commands || len(f.submissions) != 0 {
		t.Fatal("stale admission accessed GitHub or submitted a worker")
	}
	response := retryIssue(t, s, previous.ID, true)
	if response.Code != http.StatusAccepted {
		t.Fatalf("explicit recovery=%d %s", response.Code, response.Body.String())
	}
	recovered := latestRecoveryRecord(t, s)
	if recovered.InstanceID != owner || recovered.Run.GitHubIssue.Claim.Attempt != 2 || len(f.submissions) != 1 {
		t.Fatal("explicit recovery did not dispatch a new attempt with the current identity")
	}
	if recovered.Run.GitHubIssue.Claim.Branch != previous.Run.GitHubIssue.Claim.Branch || len(f.refs) != 2 {
		t.Fatal("explicit recovery did not preserve the reserved branch and prior ownership tag")
	}
}

func TestIssueRecoveryStopsRetryWhenDispatchRejectsOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	s.SubmitLocalPrompt = func(context.Context, []byte) (string, error) {
		return "", errors.New("temporary dispatch failure")
	}
	s.pollGitHubIssueAutoStart(ctx)
	expireIssueRetry(t, s, latestRecoveryRecord(t, s).ID)
	s.SubmitLocalPrompt = func(context.Context, []byte) (string, error) {
		return "", localruntime.ErrIssueOwnershipChanged
	}
	if err := s.ReconcileIssueWork(ctx); !errors.Is(err, localruntime.ErrIssueOwnershipChanged) {
		t.Fatalf("expected dispatch rejection, got %v", err)
	}
	record := latestRecoveryRecord(t, s)
	if record.State != "blocked" || !record.NextAttemptAt.IsZero() || repairableIssueIncident(record) {
		t.Fatalf("rejected dispatch state=%s retry=%v error=%q", record.State, record.NextAttemptAt, record.Error)
	}
	s.SubmitLocalPrompt = f.submit
	if err := s.ReconcileIssueWork(ctx); err != nil || len(f.submissions) != 0 {
		t.Fatalf("rejected claim retried without confirmation: submissions=%d, error=%v", len(f.submissions), err)
	}
}

func TestIssueRecoveryFinalizesCompletedAttemptAfterOwnerRestored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(ctx)
	record := completeRecoveryRecord(t, s, "completed", 1)
	identityPath := filepath.Join(s.IssueClaims.Root, "instance")
	writeOwner := func(owner string) {
		t.Helper()
		if err := os.WriteFile(identityPath, []byte(`"`+owner+`"`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeOwner(strings.Repeat("f", 32))
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	blocked := latestRecoveryRecord(t, s)
	if blocked.State != "blocked" || blocked.Error != localruntime.ErrIssueOwnershipChanged.Error() || !blocked.UnlockPending || f.deletes != 0 {
		t.Fatalf("completed attempt was not fenced while owner differed: state=%s error=%q unlock pending=%t deletes=%d", blocked.State, blocked.Error, blocked.UnlockPending, f.deletes)
	}

	writeOwner(record.InstanceID)
	f.deleteError = true
	if err := s.ReconcileIssueWork(ctx); err == nil || !strings.Contains(err.Error(), "temporary label removal failure") {
		t.Fatalf("expected label removal failure after owner restored, got %v", err)
	}
	blocked = latestRecoveryRecord(t, s)
	if blocked.State != "blocked" || blocked.Error != localruntime.ErrIssueOwnershipChanged.Error() || !blocked.NextAttemptAt.IsZero() || blocked.NextUnlockAt.IsZero() {
		t.Fatalf("label removal failure lost the dispatch block or cleanup deadline: state=%s error=%q dispatch=%v cleanup=%v", blocked.State, blocked.Error, blocked.NextAttemptAt, blocked.NextUnlockAt)
	}
	blocked.NextUnlockAt = time.Now().Add(-time.Second)
	if err := s.IssueClaims.Save(blocked); err != nil {
		t.Fatal(err)
	}
	f.deleteError = false
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	blocked = latestRecoveryRecord(t, s)
	if blocked.State != "blocked" || blocked.UnlockPending || blocked.LabelAdded || f.deletes != 2 || len(f.submissions) != 1 {
		t.Fatalf("cleanup without PR: state=%s unlock pending=%t label added=%t deletes=%d submissions=%d", blocked.State, blocked.UnlockPending, blocked.LabelAdded, f.deletes, len(f.submissions))
	}

	f.prs = recoveryPR("open", "null", "agent00-issue-17", "main")
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	final := latestRecoveryRecord(t, s)
	if final.State != "fulfilled" || final.PRURL != "https://github.com/acme/repo/pull/42" || final.VerifiedAt.IsZero() || !final.NextAttemptAt.IsZero() || len(f.submissions) != 1 {
		t.Fatalf("unexpected final state=%s PR=%q verified=%v retry=%v submissions=%d", final.State, final.PRURL, final.VerifiedAt, final.NextAttemptAt, len(f.submissions))
	}
}

func TestIssueRecoveryFinalizesInterruptedAttemptAfterOwnerRestored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(ctx)
	record := completeRecoveryRecord(t, s, "completed", 1)
	record.StoppedAt = time.Time{}
	record.Outcome = ""
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(s.IssueClaims.Root, "instance")
	writeOwner := func(owner string) {
		t.Helper()
		if err := os.WriteFile(identityPath, []byte(`"`+owner+`"`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeOwner(strings.Repeat("f", 32))
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	blocked := latestRecoveryRecord(t, s)
	if blocked.State != "blocked" || blocked.Error != localruntime.ErrIssueOwnershipChanged.Error() || !blocked.UnlockPending || f.deletes != 0 {
		t.Fatalf("completed attempt was not fenced while owner differed: state=%s error=%q unlock pending=%t deletes=%d", blocked.State, blocked.Error, blocked.UnlockPending, f.deletes)
	}

	writeOwner(record.InstanceID)
	f.deleteError = true
	if err := s.ReconcileIssueWork(ctx); err == nil || !strings.Contains(err.Error(), "temporary label removal failure") {
		t.Fatalf("expected label removal failure after owner restored, got %v", err)
	}
	blocked = latestRecoveryRecord(t, s)
	if blocked.State != "blocked" || blocked.Error != localruntime.ErrIssueOwnershipChanged.Error() || !blocked.NextAttemptAt.IsZero() || blocked.NextUnlockAt.IsZero() {
		t.Fatalf("label removal failure lost the dispatch block or cleanup deadline: state=%s error=%q dispatch=%v cleanup=%v", blocked.State, blocked.Error, blocked.NextAttemptAt, blocked.NextUnlockAt)
	}
	blocked.NextUnlockAt = time.Now().Add(-time.Second)
	if err := s.IssueClaims.Save(blocked); err != nil {
		t.Fatal(err)
	}
	f.deleteError = false
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	blocked = latestRecoveryRecord(t, s)
	if blocked.State != "blocked" || blocked.UnlockPending || blocked.LabelAdded || f.deletes != 2 || len(f.submissions) != 1 {
		t.Fatalf("cleanup without PR: state=%s unlock pending=%t label added=%t deletes=%d submissions=%d", blocked.State, blocked.UnlockPending, blocked.LabelAdded, f.deletes, len(f.submissions))
	}

	f.prs = recoveryPR("open", "null", "agent00-issue-17", "main")
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	final := latestRecoveryRecord(t, s)
	if final.State != "fulfilled" || final.PRURL != "https://github.com/acme/repo/pull/42" || final.VerifiedAt.IsZero() || !final.NextAttemptAt.IsZero() || len(f.submissions) != 1 {
		t.Fatalf("unexpected final state=%s PR=%q verified=%v retry=%v submissions=%d", final.State, final.PRURL, final.VerifiedAt, final.NextAttemptAt, len(f.submissions))
	}
}

func TestIssueRecoveryScanReportsFiltersForTrackedIssues(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(context.Background())
	record := completeRecoveryRecord(t, s, "stopped", 1)
	for _, reason := range []string{"wrong assignee", "missing trigger label"} {
		f.issue.Assignees = []GitHubIssueUser{{Login: "octocat"}}
		f.issue.Labels = []GitHubIssueLabel{{Name: "ready"}}
		if reason == "wrong assignee" {
			f.issue.Assignees = nil
		} else {
			f.issue.Labels = nil
		}
		s.pollGitHubIssueAutoStart(context.Background())
		decisions := s.issueAutomation.status.Issues
		if len(decisions) != 1 || decisions[0].Reason != reason {
			t.Fatalf("tracked issue filter = %+v, want %q", decisions, reason)
		}
		if got := latestRecoveryRecord(t, s); got.ID != record.ID {
			t.Fatal("filtering replaced the recovery record")
		}
	}
	if len(f.submissions) != 1 {
		t.Fatal("filtering restarted a stopped task")
	}
}

func TestIssueRecoveryUnlocksOnlyAfterWorkerExit(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"completed", "error", "no_changes", "stopped", "warn"} {
		t.Run(outcome, func(t *testing.T) {
			s, f := recoveryServer(t)
			s.pollGitHubIssueAutoStart(context.Background())
			record := completeRecoveryRecord(t, s, outcome, 1)
			lock, err := s.IssueClaims.TryLock(localruntime.IssueExecutionKey(record.Run.GitHubIssue))
			if err != nil || lock == nil {
				t.Fatal(err)
			}
			if err := s.ReconcileIssueWork(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.deletes != 0 {
				t.Fatal("unlocked while worker was alive")
			}
			lock.Close()
			if err := s.ReconcileIssueWork(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := latestRecoveryRecord(t, s)
			if f.deletes != 1 || got.UnlockPending || got.LabelAdded {
				t.Fatalf("unlock failed: %+v deletes=%d", got, f.deletes)
			}
			if got.State == "fulfilled" {
				t.Fatal("missing PR counted as success")
			}
			if !f.branch || len(f.refs) != 1 {
				t.Fatal("unlock destroyed branch or ownership")
			}
			if outcome == "stopped" || outcome == "no_changes" {
				if got.State != "blocked" {
					t.Fatalf("state=%s", got.State)
				}
			}
		})
	}
}

func TestIssueRecoveryVerifiesPRDespiteFailure(t *testing.T) {
	t.Parallel()
	const prURL = "https://github.com/acme/repo/pull/42"
	for _, tc := range []struct {
		name, state, merged, head, base string
		fulfilled                       bool
	}{
		{"open draft allowed", "open", "null", "agent00-issue-17", "main", true},
		{"merged", "closed", `"2026-01-01T00:00:00Z"`, "agent00-issue-17", "main", true},
		{"closed unmerged", "closed", "null", "agent00-issue-17", "main", false},
		{"closed after retarget", "closed", "null", "agent00-issue-17", "other", false},
		{"wrong head", "open", "null", "other", "main", false},
		{"wrong base", "open", "null", "agent00-issue-17", "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := recoveryServer(t)
			s.pollGitHubIssueAutoStart(context.Background())
			record := completeRecoveryRecord(t, s, "error", 1)
			s.Broker.IngestLog("task status=error request_id=" + record.RequestID)
			f.prs = recoveryPR(tc.state, tc.merged, tc.head, tc.base)
			s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				if tc.base != "main" && strings.Contains(strings.Join(cmd.Args, " "), "/pulls?base=main&") {
					return execx.Result{Stdout: `[]`}, nil
				}
				return f.Run(ctx, cmd)
			})
			if err := s.ReconcileIssueWork(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := latestRecoveryRecord(t, s)
			if (got.State == "fulfilled") != tc.fulfilled {
				t.Fatalf("state=%s", got.State)
			}
			if tc.fulfilled && got.PRURL != prURL {
				t.Fatal("missing verified PR")
			}
			task, ok := s.Broker.Task(record.RequestID)
			if !ok {
				t.Fatal("missing task")
			}
			if tc.fulfilled {
				if task.PRURL != prURL || task.PRTitleURL != prURL || task.PRTitle != recoveryPRTitle {
					t.Fatalf("task PR = (%q, %q, %q), want (%q, %q, %q)",
						task.PRURL, task.PRTitleURL, task.PRTitle, prURL, prURL, recoveryPRTitle)
				}
			} else if task.PRURL != "" || task.PRTitle != "" {
				t.Fatalf("unverified PR published to task: (%q, %q)", task.PRURL, task.PRTitle)
			}
			if tc.state == "closed" && tc.merged == "null" {
				response := retryIssue(t, s, record.ID, true)
				if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "reopen the existing pull request") || len(f.submissions) != 1 {
					t.Fatalf("closed PR retry: status=%d body=%s submissions=%d", response.Code, response.Body.String(), len(f.submissions))
				}
				expireIssueRetry(t, s, record.ID)
				if err := s.ReconcileIssueWork(t.Context()); !errors.Is(err, app.ErrIssueBranchRecovery) {
					t.Fatalf("automatic retry with closed PR: %v", err)
				}
				if blocked := latestRecoveryRecord(t, s); blocked.ID != record.ID || blocked.State != "blocked" ||
					!blocked.NextAttemptAt.IsZero() || len(f.submissions) != 1 {
					t.Fatalf("closed PR did not stop retries: state=%s next attempt=%v submissions=%d", blocked.State, blocked.NextAttemptAt, len(f.submissions))
				}
				if tc.base != "main" {
					f.prs = recoveryPR("open", "null", tc.head, tc.base)
					response := retryIssue(t, s, record.ID, true)
					if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "retarget the existing pull request") ||
						len(f.submissions) != 1 {
						t.Fatalf("PR reopened on wrong base: status=%d body=%s submissions=%d", response.Code, response.Body.String(), len(f.submissions))
					}
				}
			}
		})
	}
}

func TestIssueRecoveryKeepsExistingMergeTarget(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(ctx)
	record := completeRecoveryRecord(t, s, "completed", 1)
	const trackedURL = "https://github.com/acme/repo/pull/41"
	s.Broker.IngestLog("task status=completed request_id=" + record.RequestID + " pr_url=" + trackedURL)

	f.prs = recoveryPR("closed", `"2026-01-01T00:00:00Z"`, "agent00-issue-17", "main")
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	if got := latestRecoveryRecord(t, s); got.State != "fulfilled" || got.PRURL != "https://github.com/acme/repo/pull/42" {
		t.Fatalf("recovery state = %q, PR = %q, want fulfilled with PR 42", got.State, got.PRURL)
	}
	task, ok := s.Broker.Task(record.RequestID)
	if !ok || task.PRURL != trackedURL || task.Status != "completed" {
		t.Fatalf("task after recovery = %+v, want completed with PR %q", task, trackedURL)
	}
}

func TestIssueRecoveryPublishesVerifiedPRAfterTaskExpires(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(ctx)
	record := completeRecoveryRecord(t, s, "error", 1)
	now := time.Now().Add(-defaultTerminalTaskTimeout)
	s.Broker.now = func() time.Time { return now }
	s.Broker.IngestLog("task status=error request_id=" + record.RequestID)
	now = time.Now()
	if _, ok := s.Broker.Task(record.RequestID); ok {
		t.Fatal("terminal task should have expired")
	}

	f.prs = recoveryPR("open", "null", "agent00-issue-17", "main")
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(snapshot Snapshot) {
		t.Helper()
		pr := snapshot.VerifiedTaskPRs[record.RequestID]
		if pr.URL != "https://github.com/acme/repo/pull/42" || pr.Title != recoveryPRTitle || pr.VerifiedAt.IsZero() {
			t.Fatalf("verified PR metadata = %+v", pr)
		}
		if len(snapshot.Tasks) != 0 {
			t.Fatalf("snapshot resurrected expired task: %+v", snapshot.Tasks)
		}
	}
	check(s.Broker.StreamSnapshot())

	s.Broker = NewBroker()
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	check(s.Broker.Snapshot())
}

func TestIssueRecoveryRetriesUnlockAndAdmissionAfterRestart(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(context.Background())
	record := completeRecoveryRecord(t, s, "error", 1)
	f.deleteError = true
	if err := s.ReconcileIssueWork(context.Background()); err == nil {
		t.Fatal("unlock failure hidden")
	}
	if !latestRecoveryRecord(t, s).UnlockPending {
		t.Fatal("lost pending unlock")
	}
	restarted := NewServer("", NewBroker())
	restarted.IssueClaims = s.IssueClaims
	restarted.IssueRecovery = s.IssueRecovery
	restarted.GitHubIssueRunner = f
	restarted.SubmitLocalPrompt = f.submit
	restarted.ReviewSettingsStatus = s.ReviewSettingsStatus
	if restarted.instanceID() != s.instanceID() {
		t.Fatal("instance identity changed on restart")
	}
	f.deleteError = false
	if err := restarted.ReconcileIssueWork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 {
		t.Fatalf("retried cleanup before its deadline: deletes=%d", f.deletes)
	}
	if err := s.IssueClaims.Update(record.ID, func(r *localruntime.IssueClaimRecord) error {
		r.NextUnlockAt = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReconcileIssueWork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := latestRecoveryRecord(t, restarted); got.UnlockPending || got.NextAttemptAt.IsZero() || f.deletes != 2 {
		t.Fatalf("cleanup did not finish and start the run cooldown: pending=%t deadline=%v deletes=%d", got.UnlockPending, got.NextAttemptAt, f.deletes)
	}
	if err := s.IssueClaims.Update(record.ID, func(r *localruntime.IssueClaimRecord) error {
		r.NextAttemptAt = time.Now().Add(-time.Second)
		r.PublicationReady = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReconcileIssueWork(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := latestRecoveryRecord(t, s)
	if len(f.submissions) != 2 || got.AgentAttempts != 1 || got.Run.GitHubIssue.Claim.Attempt != 2 || !got.PublicationReady {
		t.Fatalf("retry lost lineage: %+v submissions=%d", got, len(f.submissions))
	}
}

func expireIssueRetry(t *testing.T, s Server, id string) {
	t.Helper()
	if err := s.IssueClaims.Update(id, func(record *localruntime.IssueClaimRecord) error {
		record.NextAttemptAt = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIssueRecoveryDefersCleanupAfterLabelAdmissionRateLimit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, f := recoveryServer(t)
	retryAt := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	requests := 0
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
		requests++
		if strings.Contains(strings.Join(cmd.Args, " "), "--method POST repos/acme/repo/issues/17/labels") {
			return execx.Result{Stdout: "HTTP/2.0 429 Too Many Requests\r\nRetry-After: " + retryAt.Format(http.TimeFormat) + "\r\n\r\n"}, errors.New("rate limited")
		}
		return f.Run(ctx, cmd)
	})
	s.pollGitHubIssueAutoStart(ctx)
	s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
	waiting := latestRecoveryRecord(t, s)
	before := requests
	if err := s.ReconcileIssueWork(ctx); err != nil || requests != before {
		t.Fatalf("reconciliation called GitHub before Retry-After: requests=%d, error=%v", requests-before, err)
	}
	if !waiting.UnlockPending || !waiting.NextAttemptAt.Equal(retryAt) || !waiting.NextUnlockAt.Equal(retryAt) ||
		waiting.UnlockFailures != 0 || len(f.submissions) != 0 {
		t.Fatalf("label admission did not persist both deadlines: %+v", waiting)
	}
}

func TestIssueRecoveryHonorsLabelCleanupRetryAfter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, f := recoveryServer(t)
	dispatches := 0
	s.SubmitLocalPrompt = func(context.Context, []byte) (string, error) {
		dispatches++
		return "", errors.New("temporary dispatch failure")
	}
	s.pollGitHubIssueAutoStart(ctx)
	waiting := latestRecoveryRecord(t, s)
	if !waiting.UnlockPending || !waiting.NextAttemptAt.After(time.Now()) {
		t.Fatalf("failed admission did not leave a label and dispatch cooldown: %+v", waiting)
	}
	requests := 0
	rateLimited := errors.New("rate limited")
	retryAt := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
		requests++
		if strings.Contains(strings.Join(cmd.Args, " "), "--method DELETE") {
			return execx.Result{Stdout: "HTTP/2.0 429 Too Many Requests\r\nRetry-After: " + retryAt.Format(http.TimeFormat) + "\r\n\r\n"}, rateLimited
		}
		return f.Run(ctx, cmd)
	})
	if err := s.ReconcileIssueWork(ctx); !errors.Is(err, rateLimited) {
		t.Fatalf("label cleanup should run during dispatch cooldown, got %v", err)
	}
	s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
	got := latestRecoveryRecord(t, s)
	if !got.UnlockPending || !got.NextUnlockAt.Equal(retryAt) || got.UnlockFailures != 1 ||
		!got.NextAttemptAt.Equal(waiting.NextAttemptAt) || got.Failures != waiting.Failures || got.UnlockError == "" || got.Error != waiting.Error {
		t.Fatalf("cleanup failure changed dispatch backoff or lost Retry-After: %+v", got)
	}
	if err := s.IssueClaims.Update(waiting.ID, func(r *localruntime.IssueClaimRecord) error {
		r.NextAttemptAt = time.Time{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/automation", nil))
	var status struct {
		Claims []struct {
			ID            string
			NextAttemptAt time.Time
			NextUnlockAt  time.Time
			UnlockPending bool
			UnlockError   string
		}
	}
	if response.Code != http.StatusOK {
		t.Fatalf("automation status = %d: %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Claims) != 1 || status.Claims[0].ID != waiting.ID || !status.Claims[0].NextAttemptAt.IsZero() ||
		!status.Claims[0].UnlockPending || status.Claims[0].UnlockError != got.UnlockError || !status.Claims[0].NextUnlockAt.Equal(retryAt) {
		t.Fatalf("automation status omitted independent cleanup deadline: %s", response.Body.String())
	}
	before := requests
	if err := s.ReconcileIssueWork(ctx); err != nil || requests != before {
		t.Fatalf("reconciliation called GitHub during cleanup cooldown: requests=%d, error=%v", requests-before, err)
	}
	if err := s.IssueClaims.Update(waiting.ID, func(r *localruntime.IssueClaimRecord) error {
		r.NextUnlockAt = time.Now().Add(-time.Second)
		r.NextAttemptAt = waiting.NextAttemptAt
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.GitHubCooldown.Now = func() time.Time { return retryAt }
	s.GitHubIssueRunner = f
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	got = latestRecoveryRecord(t, s)
	if got.UnlockPending || got.LabelAdded || !got.NextUnlockAt.IsZero() || got.UnlockFailures != 0 || got.UnlockError != "" || f.deletes != 1 || dispatches != 1 {
		t.Fatalf("due cleanup did not clear label and backoff: %+v deletes=%d dispatches=%d", got, f.deletes, dispatches)
	}
}

func TestIssueRecoveryBacksOffRepeatedFailuresWithoutAgentAttempts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, f := recoveryServer(t)
	s.IssueRecovery.MaxAttempts = 6 // Exercise the full backoff curve within an explicit budget.
	s.pollGitHubIssueAutoStart(ctx)
	for i, seconds := range []int{60, 300, 1500, 3600, 3600} {
		// Preflight or publication can fail without starting another agent.
		record := completeRecoveryRecord(t, s, "error", 0)
		before := time.Now()
		if err := s.ReconcileIssueWork(ctx); err != nil {
			t.Fatal(err)
		}
		after := time.Now()
		waiting := latestRecoveryRecord(t, s)
		delay := time.Duration(seconds) * time.Second
		maxDelay := min(delay+delay/5, time.Hour)
		if waiting.State != "retry_wait" || waiting.ID != record.ID || issueRunFailures(waiting) != i+1 ||
			waiting.NextAttemptAt.Before(before.Add(delay)) || waiting.NextAttemptAt.After(after.Add(maxDelay)) {
			t.Fatalf("failure %d: state=%s count=%d retry=%v, want delay %v", i+1, waiting.State, issueRunFailures(waiting), waiting.NextAttemptAt, delay)
		}
		s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
		if err := s.ReconcileIssueWork(ctx); err != nil || len(f.submissions) != i+1 {
			t.Fatalf("retried before deadline: submissions=%d, error=%v", len(f.submissions), err)
		}
		if got := latestRecoveryRecord(t, s); !got.NextAttemptAt.Equal(waiting.NextAttemptAt) || issueRunFailures(got) != i+1 {
			t.Fatalf("waiting changed deadline or counted the same run twice: %+v", got)
		}
		expireIssueRetry(t, s, record.ID)
		if err := s.ReconcileIssueWork(ctx); err != nil || len(f.submissions) != i+2 {
			t.Fatalf("due retry: submissions=%d, error=%v", len(f.submissions), err)
		}
	}

	completeRecoveryRecord(t, s, "error", 0)
	f.prs = recoveryPR("open", "null", "agent00-issue-17", "main")
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	got := latestRecoveryRecord(t, s)
	if got.State != "fulfilled" || issueRunFailures(got) != 0 || !got.NextAttemptAt.IsZero() || len(f.submissions) != 6 {
		t.Fatalf("verified PR did not end retries: state=%s failures=%d deadline=%v submissions=%d", got.State, issueRunFailures(got), got.NextAttemptAt, len(f.submissions))
	}
}

func TestIssueRecoveryBacksOffFailedSuccessorAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(ctx)
	previous := completeRecoveryRecord(t, s, "error", 0)
	expireIssueRetry(t, s, previous.ID)
	dispatchErr := errors.New("temporary dispatch failure")
	calls := 0
	s.SubmitLocalPrompt = func(context.Context, []byte) (string, error) {
		calls++
		return "", dispatchErr
	}
	for failures := 1; failures <= 2; failures++ {
		before := time.Now()
		if err := s.ReconcileIssueWork(ctx); !errors.Is(err, dispatchErr) {
			t.Fatalf("expected dispatch failure, got %v", err)
		}
		got := latestRecoveryRecord(t, s)
		if got.ID == previous.ID || got.State != "retry_wait" || got.Failures != failures || issueRunFailures(got) != 1 {
			t.Fatalf("cooldown missing on successor: %+v", got)
		}
		wantDelay := issueRetryDelay(got.ID, 1+failures, s.IssueRecovery.Defaults())
		if got.NextAttemptAt.Before(before.Add(wantDelay)) || got.NextAttemptAt.After(time.Now().Add(wantDelay)) {
			t.Fatalf("mixed run/admission failures did not increase delay: %+v", got)
		}
		response := httptest.NewRecorder()
		s.handleIssueAutomationStatus(response, httptest.NewRequest(http.MethodGet, "/api/github/automation", nil))
		var status struct {
			Claims []struct {
				ID               string
				RunFailures      int
				RecoveryFailures int
				NextAttemptAt    time.Time
			}
		}
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		matched := false
		for _, claim := range status.Claims {
			if claim.ID == got.ID {
				matched = claim.RunFailures == 1 && claim.RecoveryFailures == failures && claim.NextAttemptAt.Equal(got.NextAttemptAt)
			}
		}
		if !matched {
			t.Fatal("automation status omitted the successor's counts or deadline")
		}
		if err := s.ReconcileIssueWork(ctx); err != nil || calls != failures || len(f.submissions) != 1 {
			t.Fatalf("cooling claim dispatched: calls=%d, submissions=%d, error=%v", calls, len(f.submissions), err)
		}
		unlocked := latestRecoveryRecord(t, s)
		if unlocked.LabelAdded || unlocked.UnlockPending || githubIssueHasAnyLabel(f.issue, []string{got.Run.GitHubIssue.Claim.ExclusionLabel}) {
			t.Fatal("failed dispatch kept its exclusion label during backoff")
		}
		if !unlocked.NextAttemptAt.Equal(got.NextAttemptAt) || !unlocked.NextAttemptAt.After(time.Now()) || unlocked.Failures != failures {
			t.Fatalf("unlock changed the dispatch cooldown: deadline=%v failures=%d", unlocked.NextAttemptAt, unlocked.Failures)
		}
		expireIssueRetry(t, s, got.ID)
	}
	s.SubmitLocalPrompt = f.submit
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	record := completeRecoveryRecord(t, s, "error", 0)
	expireIssueRetry(t, s, record.ID)
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	successor := latestRecoveryRecord(t, s)
	if successor.Failures != 2 || issueRunFailures(successor) != 2 || issueRetryFailures(successor) != 4 {
		t.Fatalf("successor lost mixed failure streak: %+v", successor)
	}
}

func TestIssueRecoveryPersistsRepairReceiptBeforeFailedAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(ctx)
	previous := completeRecoveryRecord(t, s, "error", 0)
	expireIssueRetry(t, s, previous.ID)
	s.IssueRepair = func(_ context.Context, record localruntime.IssueClaimRecord) (string, error) {
		if record.ID != previous.ID {
			t.Fatalf("repair attempted for claim %s, want %s", record.ID, previous.ID)
		}
		return "repair-request", nil
	}
	dispatchErr := errors.New("temporary dispatch failure")
	s.SubmitLocalPrompt = func(context.Context, []byte) (string, error) {
		return "", dispatchErr
	}
	if err := s.ReconcileIssueWork(ctx); !errors.Is(err, dispatchErr) {
		t.Fatalf("expected dispatch failure, got %v", err)
	}
	if got := latestRecoveryRecord(t, s); got.ID == previous.ID || got.State != "retry_wait" || len(f.submissions) != 1 {
		t.Fatalf("expected deferred successor, got claim %s in state %s", got.ID, got.State)
	}
	saved, err := (&localruntime.IssueClaimStore{Root: s.IssueClaims.Root}).Load(previous.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RepairRequestID != "repair-request" {
		t.Fatalf("saved repair receipt = %q", saved.RepairRequestID)
	}
}

func TestIssueRecoveryRespectsRepositoryAutoStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(ctx)
	record := completeRecoveryRecord(t, s, "error", 1)
	settings, _ := s.ReviewSettingsStatus(ctx)
	settings.GitHubIssueRepositories = localruntime.GitHubIssueRepositories{
		"acme/other": {AutoStart: true, ExclusionLabel: "other-claimed"},
	}
	s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) { return settings, nil }
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	blocked := latestRecoveryRecord(t, s)
	if blocked.State != "blocked" || blocked.Error != "issue automation disabled" || blocked.UnlockPending || len(f.submissions) != 1 {
		t.Fatalf("removed repository did not finish cleanup and block retry: %+v", blocked)
	}
	response := retryIssue(t, s, record.ID, true)
	if response.Code != 202 || len(f.submissions) != 2 {
		t.Fatalf("explicit retry = %d %s", response.Code, response.Body.String())
	}
}

func TestIssueRecoveryUsesCurrentRepositoryExclusionLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(ctx)
	record := completeRecoveryRecord(t, s, "error", 1)
	const exclusion = "team-claimed"
	settings, _ := s.ReviewSettingsStatus(ctx)
	settings.GitHubIssueRepositories = localruntime.GitHubIssueRepositories{
		"acme/repo": {AutoStart: true, ExclusionLabel: exclusion},
	}
	s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) { return settings, nil }
	f.issue.Labels = append(f.issue.Labels, GitHubIssueLabel{Name: exclusion})
	makeRetryDue := func() {
		t.Helper()
		if err := s.IssueClaims.Update(record.ID, func(r *localruntime.IssueClaimRecord) error {
			r.NextAttemptAt = time.Now().Add(-time.Second)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	makeRetryDue()
	if err := s.ReconcileIssueWork(ctx); err == nil || !strings.Contains(err.Error(), "issue was excluded after its worker stopped") {
		t.Fatalf("reconcile with current exclusion label = %v", err)
	}
	if len(f.submissions) != 1 || f.deletes != 1 || !githubIssueHasAnyLabel(f.issue, []string{exclusion}) {
		t.Fatalf("expected only the prior claim label to be cleaned up: submissions=%d deletes=%d labels=%v", len(f.submissions), f.deletes, f.issue.Labels)
	}
	for i, label := range f.issue.Labels {
		if label.Name == exclusion {
			f.issue.Labels = append(f.issue.Labels[:i], f.issue.Labels[i+1:]...)
			break
		}
	}
	makeRetryDue()
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.submissions) != 2 || f.submissions[1].GitHubIssue.Claim.ExclusionLabel != exclusion {
		t.Fatalf("retry did not claim with current exclusion label: submissions=%d", len(f.submissions))
	}
}

func TestIssueRecoveryBudgetStopAndNoChangeDoNotRerun(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"error", "stopped", "no_changes"} {
		t.Run(outcome, func(t *testing.T) {
			s, f := recoveryServer(t)
			s.pollGitHubIssueAutoStart(context.Background())
			completeRecoveryRecord(t, s, outcome, 3)
			for range 2 {
				if err := s.ReconcileIssueWork(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.submissions) != 1 || latestRecoveryRecord(t, s).State != "blocked" {
				t.Fatal("blocked issue reran")
			}
		})
	}
}

func TestIssueRecoveryBoundsFailuresBeforeAgentStarts(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(context.Background())
	record := completeRecoveryRecord(t, s, "error", 0)
	record.PriorRunFailures = 2
	record.RunError = "workflow: write access check failed"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.ReconcileIssueWork(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := latestRecoveryRecord(t, s)
	if len(f.submissions) != 1 || got.State != "blocked" || !strings.Contains(got.Error, "workflow retry budget exhausted") {
		t.Fatalf("pre-agent failures escaped budget: submissions=%d state=%s error=%s", len(f.submissions), got.State, got.Error)
	}
	// Explicit recovery starts a fresh budget after the operator fixes the cause.
	if response := retryIssue(t, s, got.ID, true); response.Code != http.StatusAccepted {
		t.Fatalf("explicit recovery rejected: %d %s", response.Code, response.Body.String())
	}
	if next := latestRecoveryRecord(t, s); next.PriorRunFailures != 0 || len(f.submissions) != 2 {
		t.Fatal("explicit recovery did not reset the workflow budget")
	}
}

func TestIssueRecoveryBlocksReservedBranchConflictsAcrossRestart(t *testing.T) {
	// Run real Git subprocesses before parallel lock tests. Forked children
	// can briefly inherit worker-lock descriptors before exec.
	for _, name := range []string{"legacy rename rejection", "checkout conflict", "missing remote ref"} {
		t.Run(name, func(t *testing.T) {
			s, f := recoveryServer(t)
			s.pollGitHubIssueAutoStart(context.Background())
			record := completeRecoveryRecord(t, s, "error", 0)
			record.State = "retry_wait"
			record.NextAttemptAt = time.Now().Add(-time.Minute)
			switch name {
			case "legacy rename rejection":
				record.RunError = "workflow: reserved issue branch cannot be renamed; explicit recovery is required"
				record.NextAttemptAt = time.Now().Add(time.Hour)
				// Legacy conflicts must stop even if a matching PR is open.
				f.prs = recoveryPR("open", "null", record.Run.GitHubIssue.Claim.Branch, "main")
				record.WorkspaceDir = t.TempDir()
			case "checkout conflict":
				record.Incident = localruntime.IssueIncidentBranchRecovery
				record.RunError = "git: reserved issue branch recovery required: preserve work at the current HEAD, then use Recover / retry"
			case "missing remote ref":
				// Development links can still report the old reserved branch.
				s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
					if strings.Contains(strings.Join(cmd.Args, " "), "/git/matching-refs/heads/") {
						return execx.Result{Stdout: `[{"ref":"refs/heads/agent00-issue-170"}]`}, nil
					}
					return f.Run(ctx, cmd)
				})
			}
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			err := s.ReconcileIssueWork(context.Background())
			if err != nil && !errors.Is(err, app.ErrIssueBranchRecovery) {
				t.Fatal(err)
			}
			got := latestRecoveryRecord(t, s)
			if got.ID != record.ID || got.State != "blocked" || got.Incident != localruntime.IssueIncidentBranchRecovery ||
				got.UnlockPending || !got.NextAttemptAt.IsZero() || got.RunError != record.RunError ||
				!strings.Contains(got.Error, "Recover / retry") || len(f.submissions) != 1 {
				t.Fatalf("branch conflict was not blocked on the same attempt: %+v, submissions=%d", got, len(f.submissions))
			}
			restarted := NewServer("", NewBroker())
			restarted.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
			restarted.IssueRecovery = s.IssueRecovery
			restarted.GitHubIssueRunner = s.GitHubIssueRunner
			restarted.SubmitLocalPrompt = s.SubmitLocalPrompt
			restarted.ReviewSettingsStatus = s.ReviewSettingsStatus
			s = restarted
			calls := f.calls
			for range 3 {
				if err := s.ReconcileIssueWork(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if f.calls != calls || len(f.submissions) != 1 {
				t.Fatalf("blocked attempt polled or dispatched after restart: calls=%d, submissions=%d", f.calls-calls, len(f.submissions))
			}
			if name == "legacy rename rejection" {
				branch := got.Run.GitHubIssue.Claim.Branch
				dir := filepath.Join(got.WorkspaceDir, "repo")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				git := func(args ...string) string {
					t.Helper()
					res, err := (execx.OSRunner{}).Run(t.Context(), execx.Command{Dir: dir, Name: "git", Args: args})
					if err != nil {
						t.Fatal(err)
					}
					return strings.TrimSpace(res.Stdout)
				}
				git("init", "-b", branch)
				git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--no-gpg-sign", "--allow-empty", "-m", "recovered work")
				head := git("rev-parse", "HEAD")
				assertBlocked := func(want string) {
					t.Helper()
					response := retryIssue(t, s, got.ID, true)
					current := latestRecoveryRecord(t, s)
					if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), want) ||
						current.ID != got.ID || current.State != "blocked" || current.PRURL != "" || len(f.submissions) != 1 {
						t.Fatalf("retry status=%d body=%s state=%s PR=%q submissions=%d", response.Code, response.Body.String(), current.State, current.PRURL, len(f.submissions))
					}
				}
				f.prs = `[]`
				git("branch", "-m", "renamed")
				git("branch", branch)
				assertBlocked("in the saved workspace")
				if got := git("branch", "--show-current"); got != "renamed" {
					t.Fatalf("recovery changed the checked-out branch to %q", got)
				}
				git("switch", branch)
				f.prs = recoveryPR("closed", "null", branch, "main")
				assertBlocked("reopen the existing pull request")
				f.prs = recoveryPR("open", "null", branch, "main")
				assertBlocked("publish them on")
				f.prs = strings.Replace(f.prs, `"sha":"existing-commits"`, `"sha":"`+head+`"`, 1)
				assertBlocked("publish them on") // The remote reserved ref still points elsewhere.
				s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
					if strings.HasSuffix(cmd.Args[len(cmd.Args)-1], "/git/ref/heads/"+branch) {
						return execx.Result{Stdout: `{"object":{"sha":"` + head + `"}}`}, nil
					}
					return f.Run(ctx, cmd)
				})
				pending := filepath.Join(dir, "pending.txt")
				if err := os.WriteFile(pending, []byte("unpublished work"), 0600); err != nil {
					t.Fatal(err)
				}
				assertBlocked("publish them on")
				if err := os.Remove(pending); err != nil {
					t.Fatal(err)
				}
				if response := retryIssue(t, s, got.ID, true); response.Code != http.StatusOK ||
					!strings.Contains(response.Body.String(), `"prUrl":"https://github.com/acme/repo/pull/42"`) ||
					strings.Contains(response.Body.String(), "request_id") {
					t.Fatalf("retry after reopening PR: status=%d, body=%s", response.Code, response.Body.String())
				}
				if verified := latestRecoveryRecord(t, s); verified.ID != got.ID || verified.State != "fulfilled" ||
					verified.PRURL == "" || len(f.submissions) != 1 {
					t.Fatalf("reopened PR did not fulfill the same attempt: %+v, submissions=%d", verified, len(f.submissions))
				}
				return
			}
			if name == "missing remote ref" {
				if response := retryIssue(t, s, got.ID, true); response.Code != http.StatusConflict {
					t.Fatalf("retry while ref is missing: status=%d, body=%s", response.Code, response.Body.String())
				}
				s.GitHubIssueRunner = f
			}
			if response := retryIssue(t, s, got.ID, true); response.Code != http.StatusAccepted || len(f.submissions) != 2 {
				t.Fatalf("explicit retry: status=%d, body=%s, submissions=%d", response.Code, response.Body.String(), len(f.submissions))
			}
		})
	}
}

func TestIssueRecoveryVerifiesReopenedPRBeforeCheckout(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(t.Context())
	record := completeRecoveryRecord(t, s, "error", 0)
	record.Incident = localruntime.IssueIncidentBranchRecovery
	record.ExitCode = app.ExitClone
	record.WorkspaceDir = t.TempDir()
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	branch := record.Run.GitHubIssue.Claim.Branch
	f.prs = recoveryPR("closed", "null", branch, "main")
	if response := retryIssue(t, s, record.ID, true); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "reopen the existing pull request") {
		t.Fatalf("closed PR: status=%d body=%s", response.Code, response.Body.String())
	}
	f.prs = recoveryPR("open", "null", branch, "main")
	openPR := f.prs
	f.prs = strings.Replace(openPR, "existing-commits", "different-commits", 1)
	if response := retryIssue(t, s, record.ID, true); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "reconcile the remote ref") {
		t.Fatalf("PR head mismatch: status=%d body=%s", response.Code, response.Body.String())
	}
	f.prs = openPR
	if response := retryIssue(t, s, record.ID, true); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"prUrl":"https://github.com/acme/repo/pull/42"`) {
		t.Fatalf("reopened PR: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := latestRecoveryRecord(t, s); got.ID != record.ID || got.State != "fulfilled" || len(f.submissions) != 1 {
		t.Fatalf("expected original attempt fulfilled: claim=%s state=%s submissions=%d", got.ID, got.State, len(f.submissions))
	}
}

func TestIssueRecoveryCleansUpLabelWhenReservedRefVanishesBeforeDispatch(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		name := "during admission"
		if restart {
			name = "after restart"
		}
		t.Run(name, func(t *testing.T) {
			s, f := recoveryServer(t)
			ctx := t.Context()
			var interrupted localruntime.IssueClaimRecord
			s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				res, err := f.Run(ctx, cmd)
				if err == nil && strings.Contains(strings.Join(cmd.Args, " "), "--method POST repos/acme/repo/issues/17/labels") {
					f.branch = false
					if restart {
						interrupted = latestRecoveryRecord(t, s)
						return execx.Result{}, context.Canceled
					}
				}
				return res, err
			})
			s.pollGitHubIssueAutoStart(ctx)
			record := latestRecoveryRecord(t, s)
			if restart {
				// Restore the receipt an abrupt restart would find on disk.
				if err := s.IssueClaims.Save(interrupted); err != nil {
					t.Fatal(err)
				}
				s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
				if err := s.ReconcileIssueWork(ctx); !errors.Is(err, app.ErrIssueBranchRecovery) {
					t.Fatalf("reconcile missing ref: %v", err)
				}
			}
			blocked := latestRecoveryRecord(t, s)
			if blocked.State != "blocked" || !blocked.UnlockPending || !blocked.LabelAdded || len(f.submissions) != 0 {
				t.Fatalf("missing ref did not block dispatch and schedule cleanup: state=%s unlock=%t label=%t submissions=%d",
					blocked.State, blocked.UnlockPending, blocked.LabelAdded, len(f.submissions))
			}
			for range 2 {
				if err := s.ReconcileIssueWork(ctx); err != nil {
					t.Fatal(err)
				}
			}
			cleaned := latestRecoveryRecord(t, s)
			if cleaned.ID != record.ID || cleaned.State != "blocked" || cleaned.UnlockPending || cleaned.LabelAdded ||
				!cleaned.NextAttemptAt.IsZero() || f.deletes != 1 || len(f.submissions) != 0 ||
				githubIssueHasAnyLabel(f.issue, []string{record.Run.GitHubIssue.Claim.ExclusionLabel}) {
				t.Fatalf("blocked claim did not clean up its label: state=%s unlock=%t label=%t deletes=%d submissions=%d",
					cleaned.State, cleaned.UnlockPending, cleaned.LabelAdded, f.deletes, len(f.submissions))
			}
		})
	}
}

func TestIssueRecoveryBlocksPrerequisiteWithoutRepair(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		report   string
		incident string
		want     string
	}{
		{
			name:     "human action",
			report:   "Human action remains pending. Error details: An operator must confirm the shipment.",
			incident: localruntime.IssueIncidentHumanAction,
			want:     "required human action is pending; explicit retry required after the handoff",
		},
		{
			name:     "unmerged pull request",
			report:   "Delivery blocked by unmerged #12. Error details: Required changes are pending review.",
			incident: localruntime.IssueIncidentUnmergedDependency,
			want:     "required pull request is unmerged; explicit retry required after merge",
		},
		{
			name:     "open dependency",
			report:   "Export remains blocked. Error details: [#72](https://github.com/example/project/issues/72) is still open and blocks the export.",
			incident: localruntime.IssueIncidentOpenDependency,
			want:     "required dependency is still open; explicit retry required after resolution",
		},
		{
			name:     "open dependency with multiline details",
			report:   "Export remains blocked. Error details: #72 is still open and blocks the export | Verification is pending.",
			incident: localruntime.IssueIncidentOpenDependency,
			want:     "required dependency is still open; explicit retry required after resolution",
		},
		{
			name:     "open dependency missing rollout evidence",
			report:   "Production rollout for `#81` remains blocked. Error details: #72 is still open. Its proposed configuration lacks account metadata required for evidence reads. Those evidence reads are required for the rollout.",
			incident: localruntime.IssueIncidentOpenDependency,
			want:     "required dependency is still open; explicit retry required after resolution",
		},
		{
			name:     "open dependency with log sync failure",
			report:   "Production rollout for #81 remains blocked. Error details: #72 is still open. Its proposed configuration lacks account metadata required for evidence reads. Credential setup and verification remain pending. API and MCP evidence stay disabled by default.\npersist task logs: sync terminal.log: invalid argument",
			incident: localruntime.IssueIncidentOpenDependency,
			want:     "required dependency is still open; explicit retry required after resolution",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testIssueRecoveryBlocksPrerequisite(t, tc.report, tc.incident, tc.want)
		})
	}
}

func testIssueRecoveryBlocksPrerequisite(t *testing.T, report, incident, want string) {
	t.Helper()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(context.Background())
	record := completeRecoveryRecord(t, s, "error", 1)
	record.Incident = "missing_pr"
	record.Error = "codex: codex reported failure: Failure: " + report
	record.NextAttemptAt = time.Now().Add(time.Hour)
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	s.IssueRepair = func(context.Context, localruntime.IssueClaimRecord) (string, error) {
		t.Fatal("dependency failure queued a harness repair")
		return "", nil
	}
	f.deleteError = true
	if err := s.ReconcileIssueWork(context.Background()); err == nil {
		t.Fatal("expected temporary unlock failure")
	}
	if err := s.IssueClaims.Update(record.ID, func(r *localruntime.IssueClaimRecord) error {
		if !r.NextAttemptAt.IsZero() || r.NextUnlockAt.IsZero() {
			t.Fatalf("dependency failure scheduled dispatch or lost cleanup deadline: dispatch=%v cleanup=%v", r.NextAttemptAt, r.NextUnlockAt)
		}
		r.NextUnlockAt = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.deleteError = false
	for range 2 {
		if err := s.ReconcileIssueWork(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := latestRecoveryRecord(t, s)
	if got.State != "blocked" || got.UnlockPending || got.LabelAdded ||
		got.RepairRequestID != "" || got.AgentAttempts != 1 || !got.NextAttemptAt.IsZero() ||
		got.Incident != incident || got.Error != want || len(f.submissions) != 1 {
		t.Fatalf("unexpected dependency recovery state: state=%q error=%q attempts=%d submissions=%d", got.State, got.Error, got.AgentAttempts, len(f.submissions))
	}
	response := retryIssue(t, s, record.ID, true)
	if response.Code != http.StatusAccepted || len(f.submissions) != 2 {
		t.Fatalf("explicit retry: status=%d submissions=%d response=%s", response.Code, len(f.submissions), response.Body.String())
	}
}

func TestIssueRecoveryBlocksExternalRolloutWithoutRepair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(ctx)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.Incident = "missing_pr"
	const detail = "codex: codex reported failure: Failure: #42 production deployment is still incomplete. " +
		"Error details: Secrets Manager provisioning, live verification, and production IAM approval are outstanding. " +
		"CLI evidence switches default to disabled."
	// Older records may carry the terminal error only in Error.
	record.Error = detail
	record.NextAttemptAt = time.Now().Add(time.Hour)
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	s.IssueRepair = func(context.Context, localruntime.IssueClaimRecord) (string, error) {
		t.Fatal("external provisioning failure queued a harness repair")
		return "", nil
	}
	for range 2 {
		if err := s.ReconcileIssueWork(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := latestRecoveryRecord(t, s)
	if got.State != "blocked" || got.FailureDetail() != detail || got.UnlockPending || got.LabelAdded ||
		got.RepairRequestID != "" || got.AgentAttempts != 1 || !got.NextAttemptAt.IsZero() ||
		!strings.Contains(got.Error, "explicit retry required after provisioning") || len(f.submissions) != 1 {
		t.Fatalf("unexpected rollout recovery state: state=%q error=%q attempts=%d submissions=%d", got.State, got.Error, got.AgentAttempts, len(f.submissions))
	}
	response := retryIssue(t, s, record.ID, true)
	if response.Code != http.StatusAccepted || len(f.submissions) != 2 {
		t.Fatalf("explicit retry: status=%d submissions=%d response=%s", response.Code, len(f.submissions), response.Body.String())
	}
}

func TestIssueRecoveryBlocksMissingExternalEvidenceWithoutRepair(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(t.Context())
	record := completeRecoveryRecord(t, s, "error", 1)
	record.Incident = "missing_pr"
	record.RunError = "claude: claude reported failure: Failure: Equipment audit remains blocked. " +
		"Error details: This runtime had no approved inspection record or authorized access to matching equipment logs."
	record.NextAttemptAt = time.Now().Add(-time.Minute)
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	s.IssueRepair = func(context.Context, localruntime.IssueClaimRecord) (string, error) {
		t.Fatal("missing external evidence queued a harness repair")
		return "", nil
	}
	for range 2 {
		if err := s.ReconcileIssueWork(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got := latestRecoveryRecord(t, s)
	if got.State != "blocked" || got.Incident != localruntime.IssueIncidentExternalEvidence ||
		got.RunError != record.RunError || !got.NextAttemptAt.IsZero() || len(f.submissions) != 1 {
		t.Fatalf("unexpected recovery state: state=%q incident=%q submissions=%d", got.State, got.Incident, len(f.submissions))
	}
	if response := retryIssue(t, s, record.ID, true); response.Code != http.StatusAccepted || len(f.submissions) != 2 {
		t.Fatalf("explicit retry: status=%d submissions=%d response=%s", response.Code, len(f.submissions), response.Body.String())
	}
}

func TestIssueRecoveryChecksPreservedRolloutBeforeOverdueRetry(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(t.Context())
	record := completeRecoveryRecord(t, s, "error", 1)
	record.Incident = "missing_pr"
	record.RunError = "codex: codex reported failure: Failure: Production rollout remains pending. " +
		"Error details: Scoped IAM and shared reference key still require external provisioning. | [error details truncated]"
	record.NextAttemptAt = time.Now().Add(-time.Minute)
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	reads := 0
	s.IssueExternalPrerequisiteFromLogs = func(requestID, failure string) (string, bool) {
		reads++
		if requestID != record.RequestID || failure != record.RunError {
			t.Fatalf("stdout lookup used request %q and failure %q", requestID, failure)
		}
		return failurefollowup.ExternalRolloutPrerequisitesReason, true
	}
	s.IssueRepair = func(context.Context, localruntime.IssueClaimRecord) (string, error) {
		t.Fatal("external provisioning failure queued a harness repair")
		return "", nil
	}
	for range 2 {
		if err := s.ReconcileIssueWork(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got := latestRecoveryRecord(t, s)
	if reads != 2 || len(f.submissions) != 1 || got.Incident != localruntime.IssueIncidentExternalRollout ||
		got.State != "blocked" || !got.NextAttemptAt.IsZero() || got.FailureDetail() != record.RunError {
		t.Fatalf("overdue claim was not durably blocked: state=%q incident=%q reads=%d submissions=%d",
			got.State, got.Incident, reads, len(f.submissions))
	}
}

func TestIssueRecoveryChecksPreservedHumanHandoffBeforeOverdueRetry(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(t.Context())
	record := completeRecoveryRecord(t, s, "error", 1)
	record.Incident = "missing_pr"
	const summary = "Failure: Human action remains pending."
	const fullReport = summary + "\nError details:\nAn operator must confirm the shipment.\n" +
		"The delivery needs a replacement date.\nSend confirmation to the requestor.\nRecord the agreed time."
	const displayed = "Error details: An operator must confirm the shipment. | The delivery needs a replacement date. | " +
		"Send confirmation to the requestor." + failurefollowup.TruncatedErrorDetails
	record.RunError = "codex: codex reported failure: " + summary + " " + displayed
	record.NextAttemptAt = time.Now().Add(-time.Minute)
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	reads := 0
	s.IssueExternalPrerequisiteFromLogs = func(requestID, failure string) (string, bool) {
		reads++
		if requestID != record.RequestID || failure != record.RunError {
			t.Fatalf("stdout lookup used request %q and failure %q", requestID, failure)
		}
		return app.ExternalPrerequisiteFailureFromStdout(failure, func(stage string) string {
			if stage != "codex" {
				t.Fatalf("stdout lookup stage = %q, want codex", stage)
			}
			return fullReport
		})
	}
	s.IssueRepair = func(context.Context, localruntime.IssueClaimRecord) (string, error) {
		t.Fatal("human handoff failure queued a harness repair")
		return "", nil
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := latestRecoveryRecord(t, s)
	if reads != 1 || len(f.submissions) != 1 || got.Incident != localruntime.IssueIncidentHumanAction ||
		got.State != "blocked" || !got.NextAttemptAt.IsZero() || got.FailureDetail() != record.RunError {
		t.Fatalf("overdue claim was not durably blocked: state=%q incident=%q reads=%d submissions=%d",
			got.State, got.Incident, reads, len(f.submissions))
	}
}

func TestIssueRecoveryRechecksStoredRolloutIncident(t *testing.T) {
	t.Parallel()
	const prefix = "codex: codex reported failure: Failure: Production rollout remains pending. Error details: "
	const ambiguous = prefix + "IAM setup and Secrets Manager configuration are outstanding."
	const confirmed = prefix + "IAM setup and Secrets Manager configuration require external provisioning."
	const truncated = confirmed + " | Service readiness still needs confirmation. | CloudWatch access is pending." + failurefollowup.TruncatedErrorDetails
	for _, tc := range []struct {
		name, evidence       string
		matchedLogs, repair  bool
		legacyErrorFieldOnly bool
		logsRestored         bool
		recoveryStatusOnly   bool
	}{
		{name: "ambiguous preserved report", evidence: ambiguous, repair: true},
		{name: "legacy error field", evidence: ambiguous, repair: true, legacyErrorFieldOnly: true},
		{name: "legacy recovery status", evidence: "temporary GitHub verification failure", legacyErrorFieldOnly: true, recoveryStatusOnly: true},
		{name: "confirmed preserved report", evidence: confirmed},
		{name: "confirmed legacy error field", evidence: confirmed, legacyErrorFieldOnly: true},
		{name: "confirmed secret only", evidence: prefix + "The shared Secrets Manager key requires external provisioning.", legacyErrorFieldOnly: true},
		{name: "matching stdout reports a failed check", evidence: truncated, matchedLogs: true, repair: true},
		{name: "no matching stdout for truncated report", evidence: truncated},
		{name: "stdout restored after legacy error is overwritten", evidence: truncated, repair: true, legacyErrorFieldOnly: true, logsRestored: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := recoveryServer(t)
			s.pollGitHubIssueAutoStart(t.Context())
			record := completeRecoveryRecord(t, s, "error", 1)
			record.State = "blocked"
			record.Incident = localruntime.IssueIncidentExternalRollout
			record.Error = issueDependencyRetryError(record.Incident)
			if tc.legacyErrorFieldOnly {
				record.Error = tc.evidence
			} else {
				record.RunError = tc.evidence
			}
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			matchedLogs := tc.matchedLogs
			s.IssueExternalPrerequisiteFromLogs = func(requestID, failure string) (string, bool) {
				if requestID != record.RequestID || failure != truncated {
					t.Fatalf("unexpected stdout lookup for %q and %q", requestID, failure)
				}
				return "", matchedLogs
			}
			calls := 0
			s.IssueRepair = func(_ context.Context, issue localruntime.IssueClaimRecord) (string, error) {
				calls++
				if issue.Incident != "missing_pr" || issue.FailureDetail() != tc.evidence {
					t.Fatalf("unexpected repair evidence: incident=%q detail=%q", issue.Incident, issue.FailureDetail())
				}
				return "repair-test", nil
			}
			for pass := range 2 {
				if err := s.ReconcileIssueWork(t.Context()); err != nil {
					t.Fatal(err)
				}
				if pass == 0 && tc.logsRestored {
					got := latestRecoveryRecord(t, s)
					if got.State != "blocked" || got.Incident != record.Incident ||
						got.Error != issueDependencyRetryError(record.Incident) || got.RunError != tc.evidence || calls != 0 {
						t.Fatalf("legacy failure not preserved while stdout is unavailable: state=%q incident=%q error=%q run error=%q repair calls=%d",
							got.State, got.Incident, got.Error, got.RunError, calls)
					}
					matchedLogs = true
				}
			}
			got := latestRecoveryRecord(t, s)
			wantRunError := tc.evidence
			if tc.recoveryStatusOnly {
				wantRunError = ""
			}
			if got.RunError != wantRunError || len(f.submissions) != 1 {
				t.Fatalf("lost evidence or submitted a replacement worker: detail=%q submissions=%d", got.RunError, len(f.submissions))
			}
			if tc.repair {
				if calls != 1 || got.RepairRequestID != "repair-test" || got.State != "retry_wait" || got.NextAttemptAt.IsZero() {
					t.Fatalf("stale rollout block remained: calls=%d receipt=%q state=%q", calls, got.RepairRequestID, got.State)
				}
			} else if calls != 0 || got.Incident != localruntime.IssueIncidentExternalRollout || got.State != "blocked" || !got.NextAttemptAt.IsZero() {
				t.Fatalf("rollout block was lost: calls=%d incident=%q state=%q", calls, got.Incident, got.State)
			}
		})
	}
}

func TestIssueRecoveryHandlesAmbiguousReservationAndFastCompletion(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	f.ambiguous = true
	f.loseTagResponse = true
	s.SubmitLocalPrompt = func(ctx context.Context, body []byte) (string, error) {
		id, err := f.submit(ctx, body)
		if err != nil {
			return "", err
		}
		record := latestRecoveryRecord(t, s)
		record.State = "verifying"
		record.Outcome = "no_changes"
		record.StoppedAt = time.Now()
		return id, s.IssueClaims.Save(record)
	}
	s.pollGitHubIssueAutoStart(context.Background())
	got := latestRecoveryRecord(t, s)
	if len(f.submissions) != 1 || got.Outcome != "no_changes" || got.Phase != "submitted" {
		t.Fatalf("ambiguous mutation or fast receipt failed: %+v", got)
	}
	if strings.Index(strings.Join(f.trace, "\n"), "/git/refs") > strings.Index(strings.Join(f.trace, "\n"), "createLinkedBranch") {
		t.Fatal("branch reserved before ownership metadata")
	}
}

func TestIssueRecoveryCapturesAdmissionBeforeGitHubFails(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	f.fail = "issue view"
	s.pollGitHubIssueAutoStart(context.Background())
	record := latestRecoveryRecord(t, s)
	if record.Version != 2 || record.Incident != "admission" || record.Failures != 1 ||
		!record.NextAttemptAt.After(time.Now()) || len(f.submissions) != 0 {
		t.Fatalf("admission failure lost: %+v", record)
	}
	f.fail = ""
	expireIssueRetry(t, s, record.ID)
	if err := s.ReconcileIssueWork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.submissions) != 1 {
		t.Fatal("observed issue not recovered")
	}
}

func TestIssueRecoveryCorruptionAndStatusDoNotHideHealthyClaims(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(context.Background())
	completeRecoveryRecord(t, s, "error", 1)
	if err := os.WriteFile(filepath.Join(s.IssueClaims.Root, strings.Repeat("f", 32)+".json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	f.prs = recoveryPR("open", "null", "agent00-issue-17", "main")
	if err := s.ReconcileIssueWork(context.Background()); err == nil {
		t.Fatal("corrupt record not reported")
	}
	response := httptest.NewRecorder()
	s.handleIssueAutomationStatus(response, httptest.NewRequest("GET", "/", nil))
	var body struct {
		Claims []struct {
			Phase string
			PRURL string `json:"prUrl"`
		}
		Status struct{ Error string }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Claims) != 1 || body.Claims[0].Phase != "fulfilled" || body.Status.Error == "" {
		t.Fatalf("status=%s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "Customer task") {
		t.Fatal("status leaked prompt")
	}
}

func TestIssueRecoveryIndependentInstancesAndStaleOwnership(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	other := automationServer(t, f.automationGitHub)
	other.GitHubIssueRunner = f
	other.IssueRecovery = &localruntime.IssueRecoveryConfig{}
	results := make(chan error, 2)
	initialIssue := f.issue
	for _, server := range []Server{s, other} {
		go func(s Server) {
			settings, _ := s.ReviewSettingsStatus(context.Background())
			run := issueRunConfig(GitHubRepo{FullName: "acme/repo", DefaultBranch: "main"}, initialIssue, "reference")
			_, err := s.claimAndSubmitIssue(context.Background(), run, settings, "octocat", f.submit)
			results <- err
		}(server)
	}
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 || len(f.submissions) != 1 {
		t.Fatalf("duplicate admission: successes=%d submissions=%d", successes, len(f.submissions))
	}
	for _, server := range []Server{s, other} {
		record := latestRecoveryRecord(t, server)
		err := server.ValidateIssueOwnership(context.Background(), record.Run)
		if record.ID == f.submissions[0].GitHubIssue.Claim.ID {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("stale owner allowed publication")
		}
	}
}

func TestIssueRecoveryPaginationCheckpoint(t *testing.T) {
	t.Parallel()
	s, _ := recoveryServer(t)
	if _, err := s.IssueClaims.ScanPage("acme/repo", 12); err != nil {
		t.Fatal(err)
	}
	restarted := &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
	if page, err := restarted.ScanPage("acme/repo", 0); err != nil || page != 12 {
		t.Fatalf("checkpoint=%d %v", page, err)
	}
}

func TestIssueRecoveryDeliveredIssueDoesNotBecomeScanError(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(context.Background())
	completeRecoveryRecord(t, s, "completed", 1)
	f.prs = recoveryPR("open", "null", "agent00-issue-17", "main")
	s.pollGitHubIssueAutoStart(context.Background())
	s.pollGitHubIssueAutoStart(context.Background())
	if len(f.submissions) != 1 || s.issueAutomation.status.Error != "" || latestRecoveryRecord(t, s).State != "fulfilled" {
		t.Fatalf("delivered issue treated as failed admission: %+v", s.issueAutomation.status)
	}
}

func TestIssueRecoveryKeepsPausedAndUncertainWorkersBlocked(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"paused", "uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			s, f := recoveryServer(t)
			s.pollGitHubIssueAutoStart(context.Background())
			record := latestRecoveryRecord(t, s)
			if scenario == "paused" {
				record.Control = "paused"
			} else {
				record.ProcessUncertain = true
			}
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			if err := s.ReconcileIssueWork(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.submissions) != 1 {
				t.Fatal("paused or uncertain worker restarted")
			}
			if scenario == "uncertain" && f.deletes != 0 {
				t.Fatal("uncertain worker unlocked")
			}
		})
	}
}

func TestIssueRecoveryDoesNotRepairDiskExhaustionAfterStatusChanges(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(context.Background())
	record := completeRecoveryRecord(t, s, "error", 3)
	record.RunError = "git: write failed: no space left on device"
	record.Error = "agent retry budget exhausted; explicit retry required"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	s.IssueRepair = func(context.Context, localruntime.IssueClaimRecord) (string, error) {
		t.Fatal("disk exhaustion queued an application repair after its status changed")
		return "", nil
	}
	for range 2 {
		if err := s.ReconcileIssueWork(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := latestRecoveryRecord(t, s)
	if got.State != "blocked" || got.FailureDetail() != record.RunError || len(f.submissions) != 1 {
		t.Fatalf("state=%s failure=%q submissions=%d", got.State, got.FailureDetail(), len(f.submissions))
	}
}

func TestPreparedIssueRecoveryRechecksEligibility(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"new exclusion", "closed", "own label"} {
		t.Run(scenario, func(t *testing.T) {
			s, f := recoveryServer(t)
			ctx := context.Background()
			f.fail = "/labels?"
			s.pollGitHubIssueAutoStart(ctx)
			record := latestRecoveryRecord(t, s)
			if record.IssueNodeID == "" || record.Outcome != "" || len(f.submissions) != 0 {
				t.Fatalf("expected prepared admission: %+v", record)
			}
			f.fail = ""
			settings, _ := s.ReviewSettingsStatus(ctx)
			switch scenario {
			case "new exclusion":
				settings.GitHubIssueAutoStartExclusionLabel = "new-exclusion"
				f.issue.Labels = append(f.issue.Labels, GitHubIssueLabel{Name: "new-exclusion"})
			case "closed":
				f.issue.State = "closed"
			case "own label":
				record.LabelAdded = true
				f.issue.Labels = append(f.issue.Labels, GitHubIssueLabel{Name: record.Run.GitHubIssue.Claim.ExclusionLabel})
			}
			s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) { return settings, nil }
			record.NextAttemptAt = time.Time{}
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			err := s.ReconcileIssueWork(ctx)
			if scenario == "own label" {
				if err != nil || len(f.submissions) != 1 {
					t.Fatalf("owned label blocked recovery: %v submissions=%d", err, len(f.submissions))
				}
			} else if err == nil || !strings.Contains(err.Error(), "issue is ineligible") || len(f.submissions) != 0 {
				t.Fatalf("ineligible issue resumed: %v submissions=%d", err, len(f.submissions))
			}
		})
	}
}

func TestThrottledDiscoveryResumesOnce(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"user", "/issues?state=open"} {
		t.Run(endpoint, func(t *testing.T) {
			s, f := recoveryServer(t)
			now := time.Now()
			s.GitHubCooldown.Now = func() time.Time { return now }
			calls := 0
			limited := true
			s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				calls++
				if limited && strings.Contains(cmd.Args[len(cmd.Args)-1], endpoint) {
					return execx.Result{Stdout: "HTTP/2.0 429 Too Many Requests\r\nRetry-After: 120\r\n\r\n"}, errors.New("exit status 1")
				}
				return f.Run(ctx, cmd)
			})
			s.pollGitHubIssueAutoStart(t.Context())
			before := calls
			s.pollGitHubIssueAutoStart(t.Context())
			status := s.snapshot().GitHubRate
			if before == 0 || calls != before || len(f.submissions) != 0 || status.RetryAt == nil || !status.RetryAt.Equal(now.Add(2*time.Minute)) {
				t.Fatal(calls, before, status)
			}
			limited = false
			now = now.Add(2 * time.Minute)
			s.pollGitHubIssueAutoStart(t.Context())
			s.pollGitHubIssueAutoStart(t.Context())
			if len(f.submissions) != 1 || latestRecoveryRecord(t, s).Run.GitHubIssue.Number != 17 {
				t.Fatal("issue was lost or duplicated", s.issueAutomation.status)
			}
		})
	}
}
