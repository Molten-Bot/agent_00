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

	"github.com/Molten-Bot/agent_00/internal/execx"
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
	return `[{"number":42,"title":"` + recoveryPRTitle + `","state":"` + state + `","merged_at":` + merged + `,"head":{"ref":"` + head + `","repo":{"full_name":"acme/repo"}},"base":{"ref":"` + base + `","repo":{"full_name":"acme/repo"}}}]`
}

func TestExplicitIssueRecoveryUsesPersistentOwner(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"start", "recover"} {
		t.Run(action, func(t *testing.T) {
			s, f := recoveryServer(t)
			s.pollGitHubIssueAutoStart(context.Background())
			previous := completeRecoveryRecord(t, s, "stopped", 3)
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
			if latest.Run.GitHubIssue.Claim.Branch != previous.Run.GitHubIssue.Claim.Branch || latest.AgentAttempts != 0 || latest.Outcome != "" {
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
	if blocked.State != "blocked" || blocked.Error != localruntime.ErrIssueOwnershipChanged.Error() || blocked.NextAttemptAt.IsZero() {
		t.Fatalf("label removal failure lost the dispatch block: state=%s error=%q retry=%v", blocked.State, blocked.Error, blocked.NextAttemptAt)
	}
	blocked.NextAttemptAt = time.Now().Add(-time.Second)
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
	if blocked.State != "blocked" || blocked.Error != localruntime.ErrIssueOwnershipChanged.Error() || blocked.NextAttemptAt.IsZero() {
		t.Fatalf("label removal failure lost the dispatch block: state=%s error=%q retry=%v", blocked.State, blocked.Error, blocked.NextAttemptAt)
	}
	blocked.NextAttemptAt = time.Now().Add(-time.Second)
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
		{"wrong head", "open", "null", "other", "main", false},
		{"wrong base", "open", "null", "agent00-issue-17", "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := recoveryServer(t)
			s.pollGitHubIssueAutoStart(context.Background())
			record := completeRecoveryRecord(t, s, "error", 1)
			s.Broker.IngestLog("task status=error request_id=" + record.RequestID)
			f.prs = recoveryPR(tc.state, tc.merged, tc.head, tc.base)
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

func TestIssueRecoveryBlocksUnmergedPrerequisiteWithoutRepair(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.pollGitHubIssueAutoStart(context.Background())
	record := completeRecoveryRecord(t, s, "error", 1)
	record.Incident = "missing_pr"
	record.Error = "codex: codex reported failure: Failure: Delivery blocked by unmerged #12. Error details: Required changes are pending review."
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
		r.NextAttemptAt = time.Now().Add(-time.Second)
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
		!strings.Contains(got.Error, "required pull request is unmerged") || len(f.submissions) != 1 {
		t.Fatalf("unexpected dependency recovery state: state=%q error=%q attempts=%d submissions=%d", got.State, got.Error, got.AgentAttempts, len(f.submissions))
	}
	response := retryIssue(t, s, record.ID, true)
	if response.Code != http.StatusAccepted || len(f.submissions) != 2 {
		t.Fatalf("explicit retry: status=%d submissions=%d response=%s", response.Code, len(f.submissions), response.Body.String())
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
	if record.Version != 2 || record.Incident != "admission" || len(f.submissions) != 0 {
		t.Fatalf("admission failure lost: %+v", record)
	}
	f.fail = ""
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

func TestIssueRecoveryRateHeadersAndPaginationCheckpoint(t *testing.T) {
	t.Parallel()
	body, err := issueAPIResponse("HTTP/2.0 429 Too Many Requests\r\nRetry-After: 600\r\n\r\n{\"message\":\"slow down\"}\n", errors.New("rate limited"))
	var limit *issueRateLimitError
	if !errors.As(err, &limit) || time.Until(limit.RetryAt) < 9*time.Minute || !strings.HasPrefix(body, "{") {
		t.Fatalf("rate response=%s %v", body, err)
	}
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
