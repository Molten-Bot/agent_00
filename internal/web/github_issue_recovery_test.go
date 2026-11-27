package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

type recoveryGitHub struct {
	*automationGitHub
	prs             string
	pendingPR       string
	deleteError     bool
	deletes         int
	loseTagResponse bool
	closingPRURL    string
}

func (f *recoveryGitHub) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, "/pulls?") {
		return execx.Result{Stdout: f.prs}, nil
	}
	if _, number, ok := strings.Cut(args, "/pulls/"); ok {
		if f.pendingPR != "" && number == "42" {
			return execx.Result{Stdout: f.pendingPR}, nil
		}
		var prs []json.RawMessage
		if err := json.Unmarshal([]byte(f.prs), &prs); err != nil {
			return execx.Result{}, err
		}
		for _, raw := range prs {
			var pr struct{ Number int }
			if json.Unmarshal(raw, &pr) == nil && fmt.Sprint(pr.Number) == number {
				return execx.Result{Stdout: string(raw)}, nil
			}
		}
		return execx.Result{Stdout: "{}"}, nil
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
	if f.closingPRURL != "" {
		closing := fmt.Sprintf(`"closedByPullRequestsReferences":{"totalCount":1,"nodes":[{"url":%q,"state":"OPEN","merged":false}]}`, f.closingPRURL)
		res.Stdout = strings.Replace(res.Stdout, `"closedByPullRequestsReferences":{"totalCount":0}`, closing, 1)
	}
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
	store := &workflow.Store{Path: filepath.Join(t.TempDir(), "workflows.json")}
	w := workflow.Definition{ID: "issues", Name: "Issues", Repository: "acme/repo", Enabled: true, Trigger: workflow.Trigger{Event: "issue.opened"}, Filters: workflow.Filters{Mode: "all"}, Task: workflow.Task{Kind: "implement", Instructions: "Implement issue", IssueLinkMode: "reference"}, Merge: workflow.Merge{Method: "squash"}}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{w}}); err != nil {
		t.Fatal(err)
	}
	s.Workflows = &workflow.Engine{Store: store, StatePath: filepath.Join(t.TempDir(), "state.json"), Enabled: func() bool { return true }, Allowed: func(string) bool { return true }}
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

func TestIssueRecoveryWithEmptyClaimStoreLeavesGitHubIdle(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.Workflows = nil
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 || len(f.submissions) != 0 {
		t.Fatalf("empty claim store triggered %d GitHub calls and %d submissions", f.calls, len(f.submissions))
	}
}

func TestIssueRecoveryDefersWithoutChangingEvidenceDuringMemoryPressure(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.WorkspaceDir = t.TempDir()
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(record.WorkspaceDir, "validation.log")
	if err := os.WriteFile(evidence, []byte("saved evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(s.IssueClaims.Root, record.ID+".json")
	before, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.calls
	paused := true
	s.MemoryPressure = func() bool { return paused }
	for range 3 {
		if err := s.ReconcileIssueWork(t.Context()); !errors.Is(err, errIssueRecoveryDeferred) {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(receipt)
	if err != nil || string(after) != string(before) || f.calls != calls || f.deletes != 0 || len(f.submissions) != 1 {
		t.Fatalf("paused reconciliation changed claim or contacted GitHub: read err=%v, calls=%d, deletes=%d, submissions=%d",
			err, f.calls-calls, f.deletes, len(f.submissions))
	}
	paused = false
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := latestRecoveryRecord(t, s)
	if got.UnlockPending || got.AgentAttempts != 1 || f.deletes != 1 {
		t.Fatalf("recovery did not resume from saved claim: pending=%t, attempts=%d, deletes=%d", got.UnlockPending, got.AgentAttempts, f.deletes)
	}
	if data, err := os.ReadFile(evidence); err != nil || string(data) != "saved evidence" {
		t.Fatalf("workspace evidence changed: %q, %v", data, err)
	}
}

func TestExplicitIssueRecoveryUsesPersistentOwner(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"start", "recover"} {
		t.Run(action, func(t *testing.T) {
			s, f := recoveryServer(t)
			doc, err := s.Workflows.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			w := &doc.Workflows[0]
			w.Merge.Enabled = true
			w.ReviewCycles = 2
			if doc, err = s.Workflows.Store.Save(doc); err != nil {
				t.Fatal(err)
			}
			reconcileOrAdmitTestIssue(context.Background(), s)
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
			if action == "recover" {
				engine := s.Workflows
				event := latest.Run.Workflow.EventID
				prior := previous.Run.Workflow
				runs, err := engine.Runs()
				if err != nil || len(runs) != 1 || event == prior.EventID || runs[0].ID != event ||
					runs[0].RetryOf != prior.EventID || runs[0].Workflow.ExecutionPolicyHash() != prior.PolicyHash {
					t.Fatalf("recovered task lost its workflow run: %+v, %v", runs, err)
				}
				if err := engine.Complete(prior.EventID, workflow.Outcome{Status: "failed"}); !errors.Is(err, workflow.ErrActivationNotFound) {
					t.Fatalf("stale completion reached retry: %v", err)
				}
				engine.Reconcile = func(id string) string {
					for i, submission := range f.submissions {
						if submission.Workflow.EventID == id {
							return fmt.Sprintf("request-%d", i+1)
						}
					}
					return ""
				}
				engine.Fetch = func(context.Context, string, workflow.ScanNeeds) ([]workflow.Item, error) { return nil, nil }
				engine.Status = func(id string) workflow.Outcome {
					if id != latest.RequestID {
						t.Fatalf("journal queried old request %s", id)
					}
					return workflow.Outcome{Status: "running"}
				}
				if err := engine.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
				runs, err = engine.Runs()
				if err != nil || len(runs) != 1 || runs[0].RequestID != latest.RequestID || runs[0].Status != "running" {
					t.Fatalf("retry did not attach to the current task: %+v, %v", runs, err)
				}
				const prURL = "https://github.com/acme/repo/pull/42"
				if err := engine.Complete(event, workflow.Outcome{Status: "succeeded", PRURL: prURL, ReviewReceipt: recoveredWorkflowReview(*w, prURL)}); err != nil {
					t.Fatal(err)
				}
				engine.InspectPR = func(context.Context, string, string) (workflow.Item, error) {
					return workflow.Item{Open: true, ChecksPassed: true, HeadSHA: "reviewed-head"}, nil
				}
				merged := false
				engine.Merge = func(_ context.Context, r workflow.Run) error {
					merged = r.RequestID == latest.RequestID && r.ID == event && r.MergeHead == "reviewed-head" && r.Workflow.Merge.Method == "squash"
					return nil
				}
				if err := engine.Tick(t.Context()); err != nil || !merged {
					t.Fatalf("successful replacement did not reach merge controller: merged=%t, %v", merged, err)
				}
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
			reconcileOrAdmitTestIssue(context.Background(), s)
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
	reconcileOrAdmitTestIssue(ctx, s)
	previous := latestRecoveryRecord(t, s)
	if previous.State != "retry_wait" || !previous.UnlockPending || previous.AgentAttempts != 0 {
		t.Fatalf("unexpected failed admission state: %s, unlock pending=%t, agent attempts=%d", previous.State, previous.UnlockPending, previous.AgentAttempts)
	}
	previous.Failures = 4
	previous.NextAttemptAt = time.Now().Add(time.Hour)
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
	reconcileOrAdmitTestIssue(ctx, s)
	expireIssueRetry(t, s, latestRecoveryRecord(t, s).ID)
	s.SubmitLocalPrompt = func(context.Context, []byte) (string, error) {
		return "", localruntime.ErrIssueOwnershipChanged
	}
	if err := s.ReconcileIssueWork(ctx); !errors.Is(err, localruntime.ErrIssueOwnershipChanged) {
		t.Fatalf("expected dispatch rejection, got %v", err)
	}
	record := latestRecoveryRecord(t, s)
	if record.State != "blocked" || !record.NextAttemptAt.IsZero() {
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
	reconcileOrAdmitTestIssue(ctx, s)
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

func initRecoveryCheckout(t *testing.T, workspace, branch string) (func(...string) string, string) {
	t.Helper()
	dir := filepath.Join(workspace, "repo")
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
	return git, git("rev-parse", "HEAD")
}

func TestInterruptedIssueRecoveryBlocksUnpublishedWork(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "", 1)
	record.StoppedAt = time.Time{}
	record.WorkspaceDir = t.TempDir()
	branch := record.Run.GitHubIssue.Claim.Branch
	git, _ := initRecoveryCheckout(t, record.WorkspaceDir, branch)
	git("branch", "-m", "renamed")
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	f.prs = recoveryPR("open", "null", branch, "main")
	if err := s.ReconcileIssueWork(t.Context()); !errors.Is(err, app.ErrIssueBranchRecovery) {
		t.Fatalf("reconcile unpublished work: %v", err)
	}
	got := latestRecoveryRecord(t, s)
	if got.State != "blocked" || got.Incident != localruntime.IssueIncidentBranchRecovery || got.PRURL != "" || len(f.submissions) != 1 {
		t.Fatalf("state=%s incident=%s pr=%q submissions=%d", got.State, got.Incident, got.PRURL, len(f.submissions))
	}
}

func TestIssueRecoveryFinalizesInterruptedAttemptAfterOwnerRestored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(ctx, s)
	record := completeRecoveryRecord(t, s, "completed", 1)
	record.WorkspaceDir = t.TempDir()
	_, head := initRecoveryCheckout(t, record.WorkspaceDir, record.Run.GitHubIssue.Claim.Branch)
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
	f.prs = strings.Replace(f.prs, "existing-commits", head, 1)
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
		if strings.HasSuffix(cmd.Args[len(cmd.Args)-1], "/git/ref/heads/agent00-issue-17") {
			return execx.Result{Stdout: `{"object":{"sha":"` + head + `"}}`}, nil
		}
		return f.Run(ctx, cmd)
	})
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	final := latestRecoveryRecord(t, s)
	if final.State != "fulfilled" || final.PRURL != "https://github.com/acme/repo/pull/42" || final.VerifiedAt.IsZero() || !final.NextAttemptAt.IsZero() || len(f.submissions) != 1 {
		t.Fatalf("unexpected final state=%s PR=%q verified=%v retry=%v submissions=%d", final.State, final.PRURL, final.VerifiedAt, final.NextAttemptAt, len(f.submissions))
	}
}

func TestIssueRecoveryUnlocksOnlyAfterWorkerExit(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"completed", "error", "no_changes", "stopped", "warn"} {
		t.Run(outcome, func(t *testing.T) {
			s, f := recoveryServer(t)
			reconcileOrAdmitTestIssue(context.Background(), s)
			record := completeRecoveryRecord(t, s, outcome, 1)
			// This fixture has no worker children; explicitly release the lock so
			// unrelated parallel subprocess launches cannot inherit it until exec.
			lock, err := s.IssueClaims.TryControlLock(localruntime.IssueExecutionKey(record.Run.GitHubIssue))
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
			reconcileOrAdmitTestIssue(context.Background(), s)
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
			if (got.Fulfilled() || got.PRMerged()) != tc.fulfilled || got.PRMerged() != (tc.merged != "null") {
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
				if err := s.ReconcileIssueWork(t.Context()); err != nil {
					t.Fatalf("automatic retry with closed PR: %v", err)
				}
				if blocked := latestRecoveryRecord(t, s); blocked.ID != record.ID || blocked.PRURL != prURL ||
					!strings.Contains(blocked.PRRecoveryBlocker, "reopen the existing pull request") ||
					!blocked.NextAttemptAt.IsZero() || len(f.submissions) != 1 {
					t.Fatalf("closed PR did not stop retries: blocker=%q next attempt=%v submissions=%d", blocked.PRRecoveryBlocker, blocked.NextAttemptAt, len(f.submissions))
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
	reconcileOrAdmitTestIssue(ctx, s)
	record := completeRecoveryRecord(t, s, "completed", 1)
	const trackedURL = "https://github.com/acme/repo/pull/41"
	s.Broker.IngestLog("task status=completed request_id=" + record.RequestID + " pr_url=" + trackedURL)

	f.prs = recoveryPR("closed", `"2026-01-01T00:00:00Z"`, "agent00-issue-17", "main")
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	if got := latestRecoveryRecord(t, s); got.PRMerged() || (got.PRURL != "" && got.PRURL != trackedURL) {
		t.Fatalf("recovery adopted sibling PR: state = %q, PR = %q", got.State, got.PRURL)
	}
	task, ok := s.Broker.Task(record.RequestID)
	if !ok || task.PRURL != trackedURL || task.Status != "completed" {
		t.Fatalf("task after recovery = %+v, want completed with PR %q", task, trackedURL)
	}
	f.prs = strings.Replace(f.prs, `"number":42`, `"number":41`, 1)
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	got := latestRecoveryRecord(t, s)
	releases := s.Broker.Snapshot().Releases
	if !got.PRMerged() || got.PRURL != trackedURL || len(releases) != 1 || releases[0].PRURL != trackedURL ||
		releases[0].MergedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("tracked merge or release missing: state=%q PR=%q releases=%+v", got.State, got.PRURL, releases)
	}
}

func TestIssueRecoveryPublishesVerifiedPRAfterTaskExpires(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(ctx, s)
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

func TestIssueRecoveryReleasesInterruptedExecutionReserve(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := recoveryServer(t)
	reconcileOrAdmitTestIssue(ctx, s)
	record := latestRecoveryRecord(t, s)
	if err := s.IssueClaims.BeginExecution(record); err != nil {
		t.Fatal(err)
	}
	reserve := filepath.Join(s.IssueClaims.Root, ".storage-reserve-"+record.ID)
	if _, err := os.Stat(reserve); err != nil {
		t.Fatal(err)
	}

	s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.IssueClaims.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "interrupted" || got.StoppedAt.IsZero() || got.ExecutionAgentAttempts != nil {
		t.Fatalf("recovered outcome = %q, stopped at %v, execution baseline = %v", got.Outcome, got.StoppedAt, got.ExecutionAgentAttempts)
	}
	if _, err := os.Stat(reserve); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("execution reserve still present or inaccessible: %v", err)
	}
}

func TestIssueRecoveryRetriesUnlockAndAdmissionAfterRestart(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(context.Background(), s)
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
	restarted.Workflows = s.Workflows
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
	if len(f.submissions) != 2 || got.AgentAttempts != 1 || got.Run.GitHubIssue.Claim.Attempt != 2 || !got.PublicationReady || !got.CheckoutComplete {
		t.Fatalf("retry lost lineage: %+v submissions=%d", got, len(f.submissions))
	}
}

func TestLabeledIssueDraftVerificationRetryKeepsWorkflowPolicy(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"error", "completed", workflow.OutcomeReviewPending} {
		t.Run(outcome, func(t *testing.T) {
			testLabeledIssueDraftVerificationRetry(t, outcome)
		})
	}
}

func TestIssueCheckRecoveryWaitsForRateResetThenResubmitsSameClaim(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, localruntime.IssueChecksPending, s.IssueRecovery.Defaults().MaxAttempts)
	record.State = localruntime.IssueChecksPending
	record.UnlockPending = false
	record.PRVerification = "Verify CI and clean review on the draft."
	record.PRURL = "https://github.com/acme/repo/pull/42"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	f.prs = recoveryPR("open", "null", record.Run.GitHubIssue.Claim.Branch, "main")
	now := time.Now().UTC().Truncate(time.Second)
	s.GitHubCooldown = &githubutil.Cooldown{Now: func() time.Time { return now }}
	rate := s.GitHubCooldown.ClassifyFailure(http.StatusTooManyRequests, http.Header{"Retry-After": {"600"}}, "", errors.New("rate limited"))
	before := f.calls
	if err := s.ReconcileIssueWork(t.Context()); !errors.Is(err, rate) && !strings.Contains(fmt.Sprint(err), "GitHub API rate limited") {
		t.Fatalf("reconcile during rate limit = %v", err)
	}
	pending := latestRecoveryRecord(t, s)
	if pending.State != localruntime.IssueChecksPending || !pending.NextAttemptAt.Equal(now.Add(10*time.Minute)) || f.calls != before ||
		len(f.submissions) != 1 || f.deletes != 0 {
		t.Fatalf("rate limit changed claim or polled GitHub: %+v, calls=%d submissions=%d deletes=%d", pending, f.calls-before, len(f.submissions), f.deletes)
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil || f.calls != before {
		t.Fatalf("reconciler ignored retry deadline: calls=%d err=%v", f.calls-before, err)
	}
	now = pending.NextAttemptAt
	expireIssueRetry(t, s, record.ID)
	submitted := 0
	s.SubmitLocalPrompt = func(_ context.Context, body []byte) (string, error) {
		submitted++
		var run config.Config
		if err := json.Unmarshal(body, &run); err != nil {
			return "", err
		}
		if *run.GitHubIssue.Claim != *record.Run.GitHubIssue.Claim || run.Workflow.EventID != record.Run.Workflow.EventID ||
			run.Workflow.PolicyHash != record.Run.Workflow.PolicyHash {
			t.Fatalf("resumed run changed the claim or activation: %+v", run)
		}
		return record.RequestID, nil
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil || submitted != 1 || f.deletes != 0 || len(f.refs) != 1 {
		t.Fatalf("retry after reset: submissions=%d deletes=%d tags=%d err=%v", submitted, f.deletes, len(f.refs), err)
	}
	records, err := s.IssueClaims.List()
	if err != nil || len(records) != 1 || records[0].RequestID != record.RequestID {
		t.Fatalf("retry created another claim or task: %+v, %v", records, err)
	}
}

func TestIssueCheckRecoverySettlesMergedPRBeforeResubmitting(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, localruntime.IssueChecksPending, 1)
	record.State = localruntime.IssueChecksPending
	record.PRURL = "https://github.com/acme/repo/pull/42"
	record.PRVerification = "Verify draft checks"
	doc, err := s.Workflows.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	w := doc.Workflows[0]
	const eventID = "checks-pending-activation"
	record.Run.Workflow = &config.WorkflowRunConfig{ID: w.ID, EventID: eventID, PolicyHash: w.ExecutionPolicyHash()}
	s.Workflows.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	item := workflow.Item{Kind: "issue", Repository: w.Repository, Number: record.Run.GitHubIssue.Number}
	if err := s.Workflows.PrepareIssueRecovery(w.ID, w.ExecutionPolicyHash(), eventID, "", item, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	f.pendingPR = strings.Replace(strings.TrimSuffix(strings.TrimPrefix(recoveryPR("closed", `"2026-01-01T00:02:00Z"`, record.Run.GitHubIssue.Claim.Branch, "main"), "["), "]"), recoveryPRTitle, "Published PR", 1)
	for range 2 {
		if err := s.ReconcileIssueWork(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got := latestRecoveryRecord(t, s)
	if !got.PRMerged() || got.PRTitle != "Published PR" ||
		got.PRVerification != record.PRVerification || !got.VerifiedAt.IsZero() ||
		got.UnlockPending || got.LabelAdded || !got.NextAttemptAt.IsZero() || len(f.submissions) != 1 || f.deletes != 1 {
		t.Fatalf("merge did not settle pending checks: %+v, submissions=%d, label removals=%d", got, len(f.submissions), f.deletes)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var dashboard Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &dashboard); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || dashboard.Stats.TotalSavedSeconds != 120 ||
		len(dashboard.Tasks) != 1 || dashboard.Tasks[0].Status != "merged" {
		t.Fatalf("dashboard did not show the merge: %s", response.Body.String())
	}
}

func TestConfirmedWorkflowMergeSettlesIssueClaim(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"blocked", "retry_wait", localruntime.IssueStateMergedUnverified} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s, _ := recoveryServer(t)
			const id, requestID, eventID = "abcdef0123456789abcdef0123456789", "issue-task", "workflow-activation"
			const prURL = "https://github.com/acme/repo/pull/42"
			mergedAt := time.Now().UTC().Truncate(time.Second)
			record := localruntime.IssueClaimRecord{
				Version: 2, ID: id, RequestID: requestID, State: state, Outcome: "error",
				PRURL: prURL, LabelAdded: true, NextAttemptAt: mergedAt.Add(time.Hour),
				PRVerification: "Verify draft checks", Error: "draft verification interrupted",
				Run: config.Config{
					GitHubIssue: &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17, Claim: &config.GitHubIssueClaim{ID: id}},
					Workflow:    &config.WorkflowRunConfig{ID: "issues", EventID: eventID},
				},
			}
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			run := workflow.Run{ID: eventID, RequestID: requestID, PRURL: prURL,
				Item: workflow.Item{Kind: "issue", Repository: "acme/repo", Number: 17}}
			for range 2 {
				if err := s.RecordIssueMerge(run, workflow.Item{URL: prURL, Merged: true, MergedAt: mergedAt}, true); err != nil {
					t.Fatal(err)
				}
				if err := s.RecordWorkflowMerge(run, mergedAt); err != nil {
					t.Fatal(err)
				}
			}
			// A fresh broker has no task log to override the claim status.
			s.Broker = NewBroker()
			s.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
			got := latestRecoveryRecord(t, s)
			if !got.PRMerged() || got.Outcome != "error" || !got.NextAttemptAt.IsZero() || !got.UnlockPending ||
				got.Error != mergedUnverifiedIssueError || got.FailureDetail() != record.Error || got.PRVerification != record.PRVerification || !got.PRMergedAt.Equal(mergedAt) {
				t.Fatalf("confirmed merge did not settle the claim: %+v", got)
			}
			tasks := s.snapshot().Tasks
			if len(tasks) != 1 || tasks[0].Status != "merged" || tasks[0].PRURL != prURL ||
				tasks[0].Error != "" || tasks[0].RetryAt != "" || !tasks[0].HistoryOnly {
				t.Fatalf("dashboard lost the confirmed merge after restart: %+v", tasks)
			}
		})
	}
}

func TestIssueRecoveryChecksRecordedPRAcrossBranchReuse(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.PRURL = "https://github.com/acme/repo/pull/42"
	record.PRVerification = "Verify draft checks"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	branch := record.Run.GitHubIssue.Claim.Branch
	closed := recoveryPR("closed", "null", branch, "main")
	older := strings.Replace(recoveryPR("closed", `"2026-01-01T00:01:00Z"`, branch, "main"), `"number":42`, `"number":41`, 1)
	f.prs = strings.TrimSuffix(closed, "]") + "," + strings.TrimPrefix(older, "[")
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := latestRecoveryRecord(t, s); got.Fulfilled() || got.PRURL != record.PRURL {
		t.Fatalf("older merge settled the recorded closed PR: %+v", got)
	}
	if response := retryIssue(t, s, record.ID, true); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "reopen the existing pull request") {
		t.Fatalf("retry of closed PR: status=%d body=%s", response.Code, response.Body.String())
	}
	// The linked PR can merge even if branch discovery no longer lists it.
	expireIssueRetry(t, s, record.ID)
	f.prs = older
	f.pendingPR = strings.TrimSuffix(strings.TrimPrefix(recoveryPR("closed", `"2026-01-01T00:02:00Z"`, branch, "main"), "["), "]")
	s.Broker.now = func() time.Time { return time.Now().Add(retainedIssuePRPollInterval + time.Second) }
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := latestRecoveryRecord(t, s); got.Outcome != "merged" || got.PRURL != record.PRURL || len(f.submissions) != 1 {
		t.Fatalf("recorded merge was not settled: %+v, submissions=%d", got, len(f.submissions))
	}
}

func TestIssueRecoveryPreservesFailedVerificationAfterExternalMerge(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"live failed task", "restart", "explicit retry", "legacy fulfilled record", "legacy merged-unverified record"} {
		t.Run(mode, func(t *testing.T) {
			s, f := recoveryServer(t)
			reconcileOrAdmitTestIssue(t.Context(), s)
			record := completeRecoveryRecord(t, s, "error", 1)
			const failure = "pr_verification: reviewed patch evidence is truncated"
			const prURL = "https://github.com/acme/repo/pull/42"
			const eventID = "workflow-activation"
			startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			doc, err := s.Workflows.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			w := doc.Workflows[0]
			record.Run.Workflow = &config.WorkflowRunConfig{ID: w.ID, EventID: eventID, PolicyHash: w.ExecutionPolicyHash()}
			record.StartedAt = startedAt.Add(15 * time.Second)
			engine := s.Workflows
			engine.Now = func() time.Time { return startedAt }
			engine.Reconcile = func(string) string { return record.RequestID }
			engine.Fetch = func(context.Context, string, workflow.ScanNeeds) ([]workflow.Item, error) { return nil, nil }
			item := workflow.Item{Repository: w.Repository, Kind: "issue", Number: record.Run.GitHubIssue.Number}
			if err := engine.PrepareIssueRecovery(w.ID, w.ExecutionPolicyHash(), eventID, "", item, nil); err != nil {
				t.Fatal(err)
			}
			if err := engine.Complete(eventID, workflow.Outcome{Status: "failed", PRURL: prURL, Reason: failure}); err != nil {
				t.Fatal(err)
			}
			if err := engine.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if runs, err := engine.Runs(); err != nil || len(runs) != 0 {
				t.Fatalf("failed activation was not retired: %+v, %v", runs, err)
			}
			record.PRVerification = "Verify CI and submitted review on the published head."
			record.RunError, record.Error = failure, failure
			record.PriorRunFailures = 1
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			f.prs = recoveryPR("open", "null", record.Run.GitHubIssue.Claim.Branch, "main")
			if err := s.ReconcileIssueWork(t.Context()); err != nil {
				t.Fatal(err)
			}
			if pending := latestRecoveryRecord(t, s); pending.State != "retry_wait" || pending.RequestID != record.RequestID {
				t.Fatalf("open draft lost its retry state: %+v", pending)
			}
			expireIssueRetry(t, s, record.ID)
			if mode == "legacy fulfilled record" || mode == "legacy merged-unverified record" {
				if err := s.IssueClaims.Update(record.ID, func(r *localruntime.IssueClaimRecord) error {
					r.PRURL = prURL
					if mode == "legacy fulfilled record" {
						r.State, r.VerifiedAt = "fulfilled", time.Now()
					} else {
						r.State = localruntime.IssueStateMergedUnverified
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.prs = recoveryPR("closed", `"2026-01-01T00:02:00Z"`, record.Run.GitHubIssue.Claim.Branch, "main")
			restarted := automationServer(t, f.automationGitHub)
			restarted.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
			restarted.GitHubIssueRunner, restarted.IssueRecovery, restarted.Workflows = f, s.IssueRecovery, s.Workflows
			if mode == "live failed task" {
				restarted.Broker.IngestLog("task status=start request_id=" + record.RequestID)
				restarted.Broker.IngestLog("task status=error request_id=" + record.RequestID + " pr_url=" + prURL)
			}
			if mode == "explicit retry" {
				if response := retryIssue(t, restarted, record.ID, true); response.Code != http.StatusConflict ||
					!strings.Contains(response.Body.String(), "pull request is already merged") {
					t.Fatalf("retry after merge: status=%d body=%s", response.Code, response.Body.String())
				}
			}
			for range 2 {
				if err := restarted.ReconcileIssueWork(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got := latestRecoveryRecord(t, restarted)
			if !got.PRMerged() || got.State != localruntime.IssueStateMergedUnverified || got.Error != mergedUnverifiedIssueError ||
				got.ID != record.ID || got.RequestID != record.RequestID || got.FailureDetail() != failure ||
				got.PRVerification != record.PRVerification || got.PRURL != prURL || !got.VerifiedAt.IsZero() || !got.NextAttemptAt.IsZero() ||
				got.UnlockPending || issueRunFailures(got) != 0 || len(f.submissions) != 1 || len(restarted.Broker.verifiedTaskPRs) != 0 {
				t.Fatalf("external merge lost failed verification history: %+v, submissions=%d", got, len(f.submissions))
			}
			response := httptest.NewRecorder()
			restarted.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status", nil))
			var dashboard Snapshot
			if err := json.Unmarshal(response.Body.Bytes(), &dashboard); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || dashboard.Stats.TotalSavedSeconds != 120 ||
				len(dashboard.Tasks) != 1 || dashboard.Tasks[0].Status != "merged" || dashboard.Tasks[0].RetryAt != "" {
				t.Fatalf("dashboard did not count the recovered workflow once: %s", response.Body.String())
			}
		})
	}
}

func TestMergedIssueStopsChecksAndReleasesReservation(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := latestRecoveryRecord(t, s)
	record.PRURL = "https://github.com/acme/repo/pull/42"
	record.PRVerification = "Verify draft on the current head."
	record.State, record.Outcome = "queued", localruntime.IssueChecksPending
	record.NextAttemptAt = time.Now().Add(time.Hour)
	if err := s.IssueClaims.BeginExecution(record); err != nil {
		t.Fatal(err)
	}
	reserve := filepath.Join(s.IssueClaims.Root, ".storage-reserve-"+record.ID)
	if _, err := os.Stat(reserve); err != nil {
		t.Fatal(err)
	}
	s.Broker.IngestLog("task status=queued request_id=" + record.RequestID)
	s.StopTask = func(context.Context, string) error {
		t.Fatal("merge cleanup used operator stop")
		return nil
	}
	s.CancelMergedTask = func(requestID string) {
		s.Broker.IngestLog("task status=stopped request_id=" + requestID)
	}
	// The exact linked PR still identifies the task after a branch rename.
	f.prs = recoveryPR("closed", `"2026-01-01T00:00:00Z"`, "renamed-branch", "main")
	lock, err := s.IssueClaims.TryLock(localruntime.IssueExecutionKey(record.Run.GitHubIssue))
	if err != nil || lock == nil {
		t.Fatalf("acquire queued worker lock: %v", err)
	}
	defer lock.Close()
	corrupt := filepath.Join(s.IssueClaims.Root, strings.Repeat("c", 32)+".json")
	if err := os.WriteFile(corrupt, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIssueWork(t.Context()); err == nil || !strings.Contains(err.Error(), filepath.Base(corrupt)) {
		t.Fatalf("reconcile did not report corrupt claim: %v", err)
	}
	queued, err := s.IssueClaims.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := s.Broker.Task(record.RequestID)
	if !queued.PRMerged() || task.Status != "stopped" || f.deletes != 0 {
		t.Fatalf("locked continuation did not stop before cleanup: state=%s task=%s deletes=%d", queued.State, task.Status, f.deletes)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIssueWork(t.Context()); err == nil || !strings.Contains(err.Error(), filepath.Base(corrupt)) {
		t.Fatalf("reconcile did not report corrupt claim: %v", err)
	}
	got, err := s.IssueClaims.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, ok := s.Broker.Task(record.RequestID)
	if !got.PRMerged() || got.PRVerification != record.PRVerification || !got.NextAttemptAt.IsZero() ||
		got.LabelAdded || got.UnlockPending || f.deletes != 1 || len(f.submissions) != 1 || !ok || task.Status != "merged" {
		t.Fatalf("merge left queued checks or lost history: record=%+v task=%+v", got, task)
	}
	if _, err := os.Stat(reserve); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("execution reserve still exists: %v", err)
	}
}

func testLabeledIssueDraftVerificationRetry(t *testing.T, outcome string) {
	t.Helper()
	s, f := recoveryServer(t)
	engine := s.Workflows
	doc, err := engine.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	w := &doc.Workflows[0]
	w.Trigger = workflow.Trigger{Event: "issue.labeled", Value: "ready"}
	w.IncludeBacklog, w.Merge.Enabled = true, true
	w.Task.IssueLinkMode = "close"
	if _, err := engine.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	engine.Fetch = func(context.Context, string, workflow.ScanNeeds) ([]workflow.Item, error) {
		return []workflow.Item{{Kind: "issue", Repository: w.Repository, Number: f.issue.Number, URL: f.issue.URL, Open: true, Labels: []string{"ready"}}}, nil
	}
	// Mirror daemon submission: use the request ID persisted in the claim intent.
	submit := func(ctx context.Context, body []byte) (string, error) {
		if _, err := f.submit(ctx, body); err != nil {
			return "", err
		}
		record, err := s.IssueClaims.Load(f.submissions[len(f.submissions)-1].GitHubIssue.Claim.ID)
		if err != nil {
			return "", err
		}
		if record.Run.GitHubIssue.Claim.Attempt > 1 {
			s.Broker.IngestLog(fmt.Sprintf("task status=queued request_id=%s action=resume_draft_verification", record.RequestID))
		}
		s.Broker.RecordTaskRunConfig(record.RequestID, body)
		return record.RequestID, nil
	}
	s.SubmitLocalPrompt = submit
	engine.Submit = func(ctx context.Context, r workflow.Run) (string, error) {
		run := issueRunConfig(GitHubRepo{FullName: w.Repository, DefaultBranch: "main"}, f.issue, r.Workflow.Task.IssueLinkMode)
		run.Workflow = &config.WorkflowRunConfig{ID: r.Workflow.ID, EventID: r.ID, PolicyHash: r.Workflow.ExecutionPolicyHash()}
		return s.SubmitWorkflowRun(ctx, run, submit)
	}
	engine.Status = func(string) workflow.Outcome { return workflow.Outcome{Status: "running"} }
	if err := engine.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	previous := completeRecoveryRecord(t, s, outcome, 1)
	s.Broker.IngestLog(fmt.Sprintf("task status=error request_id=%s exit_code=1 err=verification-failed", previous.RequestID))
	previous.PublicationReady, previous.CheckoutComplete = true, true
	previous.PRVerification = "Verify CI and the submitted review on the published head."
	const prURL = "https://github.com/acme/repo/pull/42"
	// Successful routing previously saved a resume URL before verification.
	previous.Run.Workflow.ResumePRURL = prURL
	const verificationFailure = "pr_verification: draft verification did not pass"
	status := "failed"
	if outcome == workflow.OutcomeReviewPending {
		status = workflow.OutcomeReviewPending
		previous.PRURL = prURL
	}
	if outcome == "error" || outcome == workflow.OutcomeReviewPending {
		previous.RunError = verificationFailure
	}
	if err := s.IssueClaims.Save(previous); err != nil {
		t.Fatal(err)
	}
	f.prs = recoveryPR("open", "null", previous.Run.GitHubIssue.Claim.Branch, "main")
	f.closingPRURL = prURL
	if err := engine.Complete(previous.Run.Workflow.EventID, workflow.Outcome{Status: status, PRURL: prURL, Reason: verificationFailure, ReviewRequired: true}); err != nil {
		t.Fatal(err)
	}
	merges := 0
	engine.InspectPR = func(context.Context, string, string) (workflow.Item, error) {
		return workflow.Item{Open: true, ChecksPassed: true, HeadSHA: "reviewed-head"}, nil
	}
	engine.Merge = func(context.Context, workflow.Run) error { merges++; return nil }
	if err := engine.Tick(t.Context()); err != nil || merges != 0 {
		t.Fatalf("workflow advanced after failed verification: merges=%d, %v", merges, err)
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if current := latestRecoveryRecord(t, s); current.State != "retry_wait" {
		t.Fatalf("draft alone fulfilled the issue: %s", current.State)
	}
	expireIssueRetry(t, s, previous.ID)
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	current := latestRecoveryRecord(t, s)
	prior, retry := previous.Run.Workflow, current.Run.Workflow
	if len(f.submissions) != 2 || current.RequestID != previous.RequestID || current.PRVerification != previous.PRVerification || !current.PublicationReady ||
		current.Run.GitHubIssue.Claim.Branch != previous.Run.GitHubIssue.Claim.Branch || current.AgentAttempts != 1 ||
		retry.ID != prior.ID || retry.PolicyHash != prior.PolicyHash || retry.EventID == prior.EventID || current.PRURL != prURL {
		t.Fatalf("verification retry lost its accepted work or workflow policy: %+v", current)
	}
	retained, err := s.IssueClaims.Load(previous.ID)
	if err != nil || retained.Outcome != previous.Outcome || retained.RunError != previous.RunError || current.PriorRunFailures != issueRunFailures(retained) {
		t.Fatalf("verification retry lost the prior attempt outcome: %+v, %v", retained, err)
	}
	if tasks := s.Broker.TaskList(); len(tasks) != 1 || tasks[0].RequestID != previous.RequestID ||
		tasks[0].Status != "pending" || tasks[0].Error != "" || tasks[0].ExitCode != 0 {
		t.Fatalf("retry should queue the original task without its previous error: %+v", tasks)
	}
	runs, err := engine.Runs()
	if err != nil || len(runs) != 1 || runs[0].ID != retry.EventID || runs[0].RetryOf != prior.EventID {
		t.Fatalf("retry workflow journal = %+v, %v", runs, err)
	}
	engine.Reconcile = func(id string) string {
		if id == retry.EventID {
			return current.RequestID
		}
		return ""
	}
	if err := engine.Complete(retry.EventID, workflow.Outcome{Status: "succeeded", PRURL: prURL, ReviewRequired: true, ReviewReceipt: recoveredWorkflowReview(*w, prURL)}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Tick(t.Context()); err != nil || merges != 1 {
		t.Fatalf("verified retry did not reach the original merge policy: merges=%d, %v", merges, err)
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

func TestManualIssueReviewRecoveryRetainsDefaultExecution(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	s.Workflows.Enabled = func() bool { return false }
	doc, err := s.Workflows.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.Workflows = nil
	doc.DefaultExecution.ReviewCycles = 10
	doc.DefaultExecution.ReviewAgent.Complexity = "easy"
	doc, err = s.Workflows.Store.Save(doc)
	if err != nil {
		t.Fatal(err)
	}
	s.SubmitLocalPrompt = func(ctx context.Context, body []byte) (string, error) {
		if _, err := f.submit(ctx, body); err != nil {
			return "", err
		}
		run := f.submissions[len(f.submissions)-1]
		record, err := s.IssueClaims.Load(run.GitHubIssue.Claim.ID)
		if err != nil {
			return "", err
		}
		if run.GitHubIssue.Claim.Attempt > 1 {
			s.Broker.IngestLog(fmt.Sprintf("task status=queued request_id=%s action=resume_draft_verification", record.RequestID))
		}
		s.Broker.RecordTaskRunConfig(record.RequestID, body)
		return record.RequestID, nil
	}
	run := issueRunConfig(GitHubRepo{FullName: "acme/repo", DefaultBranch: "main"}, f.issue, "reference")
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.submitInputWorkflow(t.Context(), raw, "prompt", s.SubmitLocalPrompt)
	if err != nil {
		t.Fatal(err)
	}
	before := completeRecoveryRecord(t, s, workflow.OutcomeReviewPending, 1)
	task, ok := s.Broker.Task(id)
	want := TaskReviewPolicy{Name: "Default execution", FixPasses: 10, Complexity: "easy"}
	if !before.Run.Workflow.IsDefaultExecution() || !ok || task.WorkflowTriggered || task.ReviewPolicy != want {
		t.Fatalf("manual claim = %+v, task = %+v", before.Run.Workflow, task)
	}
	legacyID := issueTaskID(before.Run.GitHubIssue, nil)
	const prURL = "https://github.com/acme/repo/pull/42"
	before.PRURL = prURL
	before.NextAttemptAt = time.Now().Add(-time.Minute)
	if err := s.IssueClaims.Save(before); err != nil {
		t.Fatal(err)
	}
	s.Broker.IngestLog(fmt.Sprintf("task status=error request_id=%s pr_url=%s err=%q", id, prURL, "review pending"))
	doc.DefaultExecution.ReviewCycles = 2
	doc.DefaultExecution.ReviewAgent.Complexity = "advanced"
	if _, err := s.Workflows.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	f.prs = recoveryPR("open", "null", before.Run.GitHubIssue.Claim.Branch, "main")
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := latestRecoveryRecord(t, s)
	expected := *before.Run.Workflow
	expected.ResumePRURL = prURL
	if len(f.submissions) != 2 || after.RequestID != id || !reflect.DeepEqual(after.Run.Workflow, &expected) ||
		after.Run.GitHubIssue.Claim.Branch != before.Run.GitHubIssue.Claim.Branch {
		t.Fatalf("retry did not retain task, policy, branch, and PR: %+v", after)
	}
	tasks := s.Broker.TaskList()
	if len(tasks) != 1 || tasks[0].RequestID != id || tasks[0].Status != "pending" || tasks[0].ReviewPolicy != want {
		t.Fatalf("retry did not reuse the visible task: %+v", tasks)
	}
	tasks = s.snapshot().Tasks
	if len(tasks) != 1 || tasks[0].TaskID != legacyID || tasks[0].WorkflowTriggered || tasks[0].WorkflowID != "" ||
		tasks[0].ReviewPolicy != want || len(tasks[0].IssueAttempts) != 2 {
		t.Fatalf("issue snapshot did not retain manual task identity, policy, and attempts: %+v", tasks)
	}
	s.Broker = NewBroker()
	tasks = s.snapshot().Tasks
	if len(tasks) != 1 || tasks[0].TaskID != legacyID || !tasks[0].HistoryOnly || tasks[0].ReviewPolicy != want {
		t.Fatalf("restored issue snapshot did not retain manual task identity and policy: %+v", tasks)
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
	reconcileOrAdmitTestIssue(ctx, s)
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
	reconcileOrAdmitTestIssue(ctx, s)
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
	if !got.UnlockPending || !got.NextUnlockAt.Equal(retryAt) || got.UnlockFailures != waiting.UnlockFailures ||
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
	reconcileOrAdmitTestIssue(ctx, s)
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
	reconcileOrAdmitTestIssue(ctx, s)
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
		wantDelay := s.IssueRecovery.Defaults().RetryDelay(got.ID, 1+failures)
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

func TestLegacyIssueClaimRequiresExplicitRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	engine := s.Workflows
	s.Workflows = nil
	reconcileOrAdmitTestIssue(ctx, s)
	s.Workflows = engine
	record := completeRecoveryRecord(t, s, "error", 1)
	settings, _ := s.ReviewSettingsStatus(ctx)
	settings.GitHubIssueRepositories = localruntime.GitHubIssueRepositories{
		"acme/repo": {AutoStart: true, ExclusionLabel: "agent_00-in-progress"},
	}
	s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) { return settings, nil }
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	blocked := latestRecoveryRecord(t, s)
	if blocked.State != "blocked" || blocked.Error != "automatic issue retry requires an enabled workflow" || blocked.UnlockPending || len(f.submissions) != 1 {
		t.Fatalf("legacy claim did not finish cleanup and block retry: %+v", blocked)
	}
	response := retryIssue(t, s, record.ID, true)
	if response.Code != 202 || len(f.submissions) != 2 {
		t.Fatalf("explicit retry = %d %s", response.Code, response.Body.String())
	}
}

func TestIssueRecoveryUsesWorkflowExclusionLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(ctx, s)
	record := completeRecoveryRecord(t, s, "error", 1)
	const exclusion = localruntime.DefaultGitHubIssueExclusionLabel
	// Retrying a workflow continues to work when the Issues view is disabled.
	s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
		return ReviewSettingsState{}, nil
	}
	record.LabelAdded = false
	record.UnlockPending = false
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	f.issue.Labels = []GitHubIssueLabel{{Name: exclusion}}
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
	if len(f.submissions) != 1 || f.deletes != 0 || !githubIssueHasAnyLabel(f.issue, []string{exclusion}) {
		t.Fatalf("expected workflow exclusion label to be preserved: submissions=%d deletes=%d labels=%v", len(f.submissions), f.deletes, f.issue.Labels)
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
			reconcileOrAdmitTestIssue(context.Background(), s)
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
	reconcileOrAdmitTestIssue(context.Background(), s)
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
			reconcileOrAdmitTestIssue(context.Background(), s)
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
			restarted.Workflows = s.Workflows
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
				git, head := initRecoveryCheckout(t, got.WorkspaceDir, branch)
				dir := filepath.Join(got.WorkspaceDir, "repo")
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
	t.Run("no pending verification", func(t *testing.T) {
		testIssueRecoveryCorrectedPR(t, "", "closed", "main", "reopen")
	})
	t.Run("pending verification", func(t *testing.T) {
		testIssueRecoveryCorrectedPR(t, "Verify CI on the published head.", "closed", "main", "reopen")
	})
	t.Run("retargeted PR", func(t *testing.T) {
		testIssueRecoveryCorrectedPR(t, "", "open", "other", "retarget")
	})
}

func testIssueRecoveryCorrectedPR(t *testing.T, verification, blockedState, blockedBase, action string) {
	t.Helper()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "error", 0)
	record.Incident = localruntime.IssueIncidentBranchRecovery
	record.ExitCode = app.ExitClone
	record.PRVerification = verification
	record.WorkspaceDir = t.TempDir()
	record.PRURL = "https://github.com/acme/repo/pull/42"
	record.PRRecoveryBlocker = action + " the existing pull request before using Recover / retry"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	branch := record.Run.GitHubIssue.Claim.Branch
	f.prs = recoveryPR(blockedState, "null", branch, blockedBase)
	if response := retryIssue(t, s, record.ID, true); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), action+" the existing pull request") {
		t.Fatalf("blocked PR: status=%d body=%s", response.Code, response.Body.String())
	}
	f.prs = recoveryPR("open", "null", branch, "main")
	openPR := f.prs
	f.prs = strings.Replace(openPR, "existing-commits", "different-commits", 1)
	if response := retryIssue(t, s, record.ID, true); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "reconcile the remote ref") {
		t.Fatalf("PR head mismatch: status=%d body=%s", response.Code, response.Body.String())
	}
	f.prs = openPR
	response := retryIssue(t, s, record.ID, true)
	previous, err := s.IssueClaims.Load(record.ID)
	if err != nil || previous.PRRecoveryBlocker != "" {
		t.Fatalf("explicit recovery retained obsolete PR blocker: blocker=%q err=%v", previous.PRRecoveryBlocker, err)
	}
	if tasks := s.snapshot().Tasks; len(tasks) != 1 || tasks[0].IssuePRRecoveryBlocker != "" {
		t.Fatalf("task card still exposes an obsolete PR blocker: %+v", tasks)
	}
	if verification != "" {
		got := latestRecoveryRecord(t, s)
		if response.Code != http.StatusAccepted || len(f.submissions) != 2 || got.ID == record.ID ||
			got.PRVerification != verification || got.State == "fulfilled" {
			t.Fatalf("reopened PR needs verification: status=%d body=%s record=%+v", response.Code, response.Body.String(), got)
		}
		return
	}
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"prUrl":"https://github.com/acme/repo/pull/42"`) {
		t.Fatalf("reopened PR: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := latestRecoveryRecord(t, s); got.ID != record.ID || got.State != "fulfilled" || len(f.submissions) != 1 {
		t.Fatalf("expected original attempt fulfilled: claim=%s state=%s submissions=%d", got.ID, got.State, len(f.submissions))
	}
}

func TestIssueRecoveryCleansUpLabelWhenAdmissionIsBlocked(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing ref during admission", "missing ref after restart", "closed PR after restart"} {
		restart := strings.HasSuffix(name, "after restart")
		t.Run(name, func(t *testing.T) {
			s, f := recoveryServer(t)
			ctx := t.Context()
			var interrupted localruntime.IssueClaimRecord
			s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				res, err := f.Run(ctx, cmd)
				if err == nil && strings.Contains(strings.Join(cmd.Args, " "), "--method POST repos/acme/repo/issues/17/labels") {
					if name == "closed PR after restart" {
						f.prs = recoveryPR("closed", "null", "agent00-issue-17", "other")
					} else {
						f.branch = false
					}
					if restart {
						interrupted = latestRecoveryRecord(t, s)
						return execx.Result{}, context.Canceled
					}
				}
				return res, err
			})
			reconcileOrAdmitTestIssue(ctx, s)
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

func TestIssueRecoveryCleansUpLabelWithUnreadableWorkflowJournal(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"pending cleanup", "stopped worker", "worker exited before recording outcome"} {
		t.Run(name, func(t *testing.T) {
			s, f := recoveryServer(t)
			reconcileOrAdmitTestIssue(t.Context(), s)
			record := completeRecoveryRecord(t, s, "error", 1)
			record.PRURL, record.UnlockPending = "", name == "pending cleanup"
			if name == "worker exited before recording outcome" {
				record.StoppedAt, record.Outcome = time.Time{}, ""
			}
			record.NextAttemptAt = time.Now().Add(time.Hour)
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.Workflows.StatePath, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.ReconcileIssueWork(t.Context()); err == nil || !strings.Contains(err.Error(), "read workflow journal") {
				t.Fatalf("journal failure was lost: %v", err)
			}
			got := latestRecoveryRecord(t, s)
			if got.UnlockPending || got.LabelAdded || f.deletes != 1 || len(f.submissions) != 1 ||
				!got.NextAttemptAt.Equal(record.NextAttemptAt) {
				t.Fatalf("journal failure blocked cleanup or changed retry state: %+v", got)
			}
		})
	}
}

func TestIssueRecoveryBoundsRetainedLinkedPRPolling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ claimState, prState string }{
		{"fulfilled", "open"},
		{"blocked", "open"},
		{"verifying", "closed"},
	} {
		t.Run(tc.claimState+"/"+tc.prState, func(t *testing.T) {
			s, f := recoveryServer(t)
			reconcileOrAdmitTestIssue(t.Context(), s)
			record := completeRecoveryRecord(t, s, "completed", 1)
			record.State, record.PRURL = tc.claimState, "https://github.com/acme/repo/pull/42"
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			f.prs = recoveryPR(tc.prState, "null", record.Run.GitHubIssue.Claim.Branch, "main")
			now := time.Now()
			s.Broker = NewBroker()
			s.Broker.now = func() time.Time { return now }
			polls := 0
			s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				if strings.Contains(strings.Join(cmd.Args, " "), "/pulls/42") {
					polls++
				}
				return f.Run(ctx, cmd)
			})
			for range 3 {
				if err := s.ReconcileIssueWork(t.Context()); err != nil {
					t.Fatal(err)
				}
				now = now.Add(time.Minute)
			}
			if polls != 1 || f.deletes != 1 {
				t.Fatalf("retained pull request polled %d times, label removed %d times", polls, f.deletes)
			}
			if tc.claimState == "fulfilled" {
				now = now.Add(defaultClosedTaskRetention)
				if len(s.snapshot().Tasks) != 0 {
					t.Fatal("fulfilled task card did not expire")
				}
				if err := s.ReconcileIssueWork(t.Context()); err != nil || polls != 1 {
					t.Fatalf("expired task still polled: polls=%d err=%v", polls, err)
				}
				return
			}
			now = now.Add(retainedIssuePRPollInterval)
			f.prs = recoveryPR("closed", `"2026-01-01T00:00:00Z"`, record.Run.GitHubIssue.Claim.Branch, "main")
			if err := s.ReconcileIssueWork(t.Context()); err != nil {
				t.Fatal(err)
			}
			if polls != 2 || !latestRecoveryRecord(t, s).PRMerged() {
				t.Fatalf("deferred poll missed a later merge: polls=%d", polls)
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
	reconcileOrAdmitTestIssue(context.Background(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.Incident = "missing_pr"
	record.Error = "codex: codex reported failure: Failure: " + report
	record.NextAttemptAt = time.Now().Add(time.Hour)
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
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
	reconcileOrAdmitTestIssue(ctx, s)
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
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.Incident = "missing_pr"
	record.RunError = "claude: claude reported failure: Failure: Equipment audit remains blocked. " +
		"Error details: This runtime had no approved inspection record or authorized access to matching equipment logs."
	record.NextAttemptAt = time.Now().Add(-time.Minute)
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
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
	reconcileOrAdmitTestIssue(t.Context(), s)
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
	reconcileOrAdmitTestIssue(t.Context(), s)
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
		matchedLogs, retry   bool
		legacyErrorFieldOnly bool
		logsRestored         bool
		recoveryStatusOnly   bool
	}{
		{name: "ambiguous preserved report", evidence: ambiguous, retry: true},
		{name: "legacy error field", evidence: ambiguous, retry: true, legacyErrorFieldOnly: true},
		{name: "legacy recovery status", evidence: "temporary GitHub verification failure", legacyErrorFieldOnly: true, recoveryStatusOnly: true},
		{name: "confirmed preserved report", evidence: confirmed},
		{name: "confirmed legacy error field", evidence: confirmed, legacyErrorFieldOnly: true},
		{name: "confirmed secret only", evidence: prefix + "The shared Secrets Manager key requires external provisioning.", legacyErrorFieldOnly: true},
		{name: "matching stdout reports a failed check", evidence: truncated, matchedLogs: true, retry: true},
		{name: "no matching stdout for truncated report", evidence: truncated},
		{name: "stdout restored after legacy error is overwritten", evidence: truncated, retry: true, legacyErrorFieldOnly: true, logsRestored: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := recoveryServer(t)
			reconcileOrAdmitTestIssue(t.Context(), s)
			record := completeRecoveryRecord(t, s, "error", 1)
			record.State = "blocked"
			record.Incident = localruntime.IssueIncidentExternalRollout
			record.Error = issueExplicitRetryError(record.Incident)
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
			for pass := range 2 {
				if err := s.ReconcileIssueWork(t.Context()); err != nil {
					t.Fatal(err)
				}
				if pass == 0 && tc.logsRestored {
					got := latestRecoveryRecord(t, s)
					if got.State != "blocked" || got.Incident != record.Incident ||
						got.Error != issueExplicitRetryError(record.Incident) || got.RunError != tc.evidence {
						t.Fatalf("legacy failure not preserved while stdout is unavailable: state=%q incident=%q error=%q run error=%q",
							got.State, got.Incident, got.Error, got.RunError)
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
			if tc.retry {
				if got.Incident != "missing_pr" || got.State != "retry_wait" || got.NextAttemptAt.IsZero() {
					t.Fatalf("stale rollout block remained: incident=%q state=%q", got.Incident, got.State)
				}
			} else if got.Incident != localruntime.IssueIncidentExternalRollout || got.State != "blocked" || !got.NextAttemptAt.IsZero() {
				t.Fatalf("rollout block was lost: incident=%q state=%q", got.Incident, got.State)
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
	reconcileOrAdmitTestIssue(context.Background(), s)
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
	reconcileOrAdmitTestIssue(context.Background(), s)
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
	reconcileOrAdmitTestIssue(context.Background(), s)
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
	reconcileOrAdmitTestIssue(context.Background(), s)
	completeRecoveryRecord(t, s, "completed", 1)
	f.prs = recoveryPR("open", "null", "agent00-issue-17", "main")
	reconcileOrAdmitTestIssue(context.Background(), s)
	reconcileOrAdmitTestIssue(context.Background(), s)
	if len(f.submissions) != 1 || s.issueAutomation.status.Error != "" || latestRecoveryRecord(t, s).State != "fulfilled" {
		t.Fatalf("delivered issue treated as failed admission: %+v", s.issueAutomation.status)
	}
}

func TestIssueRecoveryKeepsPausedAndUncertainWorkersBlocked(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"paused", "uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			s, f := recoveryServer(t)
			reconcileOrAdmitTestIssue(context.Background(), s)
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

func TestIssueRecoveryBlocksLegacyDiskExhaustionUntilExplicitRetry(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(context.Background(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.RunError = "codex: workspace storage ran out of space while the agent was running"
	record.Error = "workflow retry budget exhausted; explicit retry required"
	record.NextAttemptAt = time.Now().Add(time.Hour)
	record.WorkspaceDir = filepath.Join(t.TempDir(), "retained")
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.ReconcileIssueWork(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := latestRecoveryRecord(t, s)
	if got.State != "blocked" || got.Incident != localruntime.IssueIncidentWorkspaceStorage ||
		!strings.Contains(got.Error, "select Retry") || !got.NextAttemptAt.IsZero() || got.UnlockPending ||
		got.FailureDetail() != record.RunError || issueRunFailures(got) != 0 || len(f.submissions) != 1 {
		t.Fatalf("state=%s failure=%q submissions=%d", got.State, got.FailureDetail(), len(f.submissions))
	}
	status := httptest.NewRecorder()
	s.handleIssueAutomationStatus(status, httptest.NewRequest(http.MethodGet, "/api/github/automation", nil))
	for _, field := range []string{`"phase":"blocked"`, `"incident":"workspace_storage"`, `"runFailures":0`} {
		if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), field) {
			t.Fatalf("queue status missing %s: %d %s", field, status.Code, status.Body.String())
		}
	}
	if response := retryIssue(t, s, got.ID, true); response.Code != http.StatusAccepted {
		t.Fatalf("explicit retry = %d %s", response.Code, response.Body.String())
	}
	next := latestRecoveryRecord(t, s)
	if len(f.submissions) != 2 || next.WorkspaceDir != got.WorkspaceDir || next.AgentAttempts != 0 {
		t.Fatalf("retry did not retain the workspace and reset the cycle: %+v", next)
	}
}

func TestIssueRecoveryDoesNotBlockOnDiskErrorQuotedInAgentReport(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.RunError = "codex: codex reported failure: Failure: test assertions failed. Error details: Recovered from ENOSPC before running tests."
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := latestRecoveryRecord(t, s)
	if got.State != "retry_wait" || got.Incident == localruntime.IssueIncidentWorkspaceStorage || len(f.submissions) != 1 {
		t.Fatalf("recovery state=%q incident=%q submissions=%d", got.State, got.Incident, len(f.submissions))
	}
}

func TestPreparedIssueRecoveryRechecksEligibility(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"new exclusion", "closed", "own label"} {
		t.Run(scenario, func(t *testing.T) {
			s, f := recoveryServer(t)
			ctx := context.Background()
			f.fail = "/labels?"
			reconcileOrAdmitTestIssue(ctx, s)
			record := latestRecoveryRecord(t, s)
			if record.IssueNodeID == "" || record.Outcome != "" || len(f.submissions) != 0 {
				t.Fatalf("expected prepared admission: %+v", record)
			}
			f.fail = ""
			switch scenario {
			case "new exclusion":
				record.LabelAdded = false
				record.UnlockPending = false
				f.issue.Labels = append(f.issue.Labels, GitHubIssueLabel{Name: localruntime.DefaultGitHubIssueExclusionLabel})
			case "closed":
				f.issue.State = "closed"
			case "own label":
				record.LabelAdded = true
				f.issue.Labels = append(f.issue.Labels, GitHubIssueLabel{Name: record.Run.GitHubIssue.Claim.ExclusionLabel})
			}
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

func TestIssuePRMetadataFailureBlocksFulfillmentAndRetriesOriginalPR(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	doc, err := s.Workflows.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	w := doc.Workflows[0]
	run := issueRunConfig(GitHubRepo{FullName: w.Repository, DefaultBranch: "main"}, f.issue, "reference")
	run.Workflow = &config.WorkflowRunConfig{ID: w.ID, EventID: "original", PolicyHash: w.ExecutionPolicyHash(), PRAssignee: "maintainer"}
	if _, err := s.SubmitWorkflowRun(t.Context(), run, f.submit); err != nil {
		t.Fatal(err)
	}
	previous := completeRecoveryRecord(t, s, "error", 1)
	const prURL = "https://github.com/acme/repo/pull/42"
	f.prs = recoveryPR("open", "null", previous.Run.GitHubIssue.Claim.Branch, "main")
	f.closingPRURL = prURL
	previous.PRURL = prURL
	previous.RunError = "pr: workflow PR metadata update failed: GitHub did not apply assignee"
	if err := s.IssueClaims.Save(previous); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if current := latestRecoveryRecord(t, s); current.State != "blocked" || current.Error != previous.RunError {
		t.Fatalf("metadata failure must remain visible: state=%s error=%q", current.State, current.Error)
	}
	response := retryIssue(t, s, previous.ID, true)
	if response.Code != http.StatusAccepted || len(f.submissions) != 2 {
		t.Fatalf("retry = %d %s, submissions=%d", response.Code, response.Body.String(), len(f.submissions))
	}
	policy := f.submissions[1].Workflow
	if policy.ResumePRURL != previous.PRURL || policy.PRAssignee != run.Workflow.PRAssignee ||
		policy.PolicyHash != run.Workflow.PolicyHash || policy.EventID == run.Workflow.EventID {
		t.Fatalf("retry lost the original PR or workflow policy: %+v", policy)
	}
}

func TestIssuePRMetadataFailureSettlesOperatorMerge(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	const prURL = "https://github.com/acme/repo/pull/42"
	const failure = "pr: workflow PR metadata update failed: GitHub did not apply assignee"
	record.PRURL, record.RunError = prURL, failure
	doc, err := s.Workflows.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	w := doc.Workflows[0]
	const eventID = "metadata-failure-activation"
	record.Run.Workflow = &config.WorkflowRunConfig{ID: w.ID, EventID: eventID, PolicyHash: w.ExecutionPolicyHash()}
	s.Workflows.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	item := workflow.Item{Kind: "issue", Repository: w.Repository, Number: record.Run.GitHubIssue.Number}
	if err := s.Workflows.PrepareIssueRecovery(w.ID, w.ExecutionPolicyHash(), eventID, "", item, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	f.pendingPR = strings.Replace(strings.TrimSuffix(strings.TrimPrefix(recoveryPR("closed", `"2026-01-01T00:02:00Z"`, record.Run.GitHubIssue.Claim.Branch, "main"), "["), "]"), recoveryPRTitle, "Published PR", 1)
	for range 2 {
		if err := s.ReconcileIssueWork(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got := latestRecoveryRecord(t, s)
	if !got.Fulfilled() || got.Outcome != "merged" || got.Error != "" || got.FailureDetail() != failure ||
		got.UnlockPending || !got.NextAttemptAt.IsZero() || len(f.submissions) != 1 {
		t.Fatalf("merge did not settle metadata failure: %+v, submissions=%d", got, len(f.submissions))
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var dashboard Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &dashboard); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || dashboard.Stats.TotalSavedSeconds != 120 ||
		len(dashboard.Tasks) != 1 || dashboard.Tasks[0].Status != "merged" {
		t.Fatalf("dashboard missed recovered workflow time: %s", response.Body.String())
	}
}

func TestIssuePRMetadataTaskRerunWithRoutingCorrection(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	doc, err := s.Workflows.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.Workflows[0].Task.PRAssignee = "maintainer"
	if doc, err = s.Workflows.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	w := doc.Workflows[0]
	run := issueRunConfig(GitHubRepo{FullName: w.Repository, DefaultBranch: "main"}, f.issue, "reference")
	run.Workflow = &config.WorkflowRunConfig{ID: w.ID, EventID: "original", PolicyHash: w.ExecutionPolicyHash(), PRAssignee: "maintainer"}
	if _, err := s.SubmitWorkflowRun(t.Context(), run, f.submit); err != nil {
		t.Fatal(err)
	}
	previous := completeRecoveryRecord(t, s, "error", 1)
	const prURL = "https://github.com/acme/repo/pull/42"
	f.prs = recoveryPR("open", "null", previous.Run.GitHubIssue.Claim.Branch, "main")
	f.closingPRURL = prURL
	previous.PRURL = prURL
	previous.RunError = "pr: workflow PR metadata update failed: GitHub did not apply assignee"
	if err := s.IssueClaims.Save(previous); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if current := latestRecoveryRecord(t, s); current.State != "blocked" || current.Error != previous.RunError {
		t.Fatalf("metadata failure must remain visible: state=%s error=%q", current.State, current.Error)
	}
	doc.Workflows[0].Task.PRAssignee = "replacement"
	doc.Workflows[0].Task.PRLabels = []string{"tracking"}
	if doc, err = s.Workflows.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	originalSettings := s.ReviewSettingsStatus
	s.ReviewSettingsStatus = func(ctx context.Context) (ReviewSettingsState, error) {
		settings, err := originalSettings(ctx)
		settings.GitHubIssuesEnabled = false
		return settings, err
	}
	id, err := s.submitParsedWithIssueClaim(t.Context(), nil, previous.Run, f.submit)
	if err != nil || id == "" || len(f.submissions) != 2 {
		t.Fatalf("task rerun failed: %q %v", id, err)
	}
	policy := f.submissions[1].Workflow
	if policy.ResumePRURL != previous.PRURL || policy.PRAssignee != "replacement" || len(policy.PRLabels) != 1 ||
		policy.PolicyHash != doc.Workflows[0].ExecutionPolicyHash() || policy.EventID == run.Workflow.EventID {
		t.Fatalf("retry lost the original PR or workflow policy: %+v", policy)
	}
}

func TestIssuePRMetadataStoppedTaskRerunWithRoutingCorrection(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	doc, err := s.Workflows.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	doc.Workflows[0].Task.PRAssignee = "maintainer"
	if doc, err = s.Workflows.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	w := doc.Workflows[0]
	run := issueRunConfig(GitHubRepo{FullName: w.Repository, DefaultBranch: "main"}, f.issue, "reference")
	run.Workflow = &config.WorkflowRunConfig{ID: w.ID, EventID: "original", PolicyHash: w.ExecutionPolicyHash(), PRAssignee: "maintainer"}
	if _, err := s.SubmitWorkflowRun(t.Context(), run, f.submit); err != nil {
		t.Fatal(err)
	}
	previous := completeRecoveryRecord(t, s, "error", 1)
	const prURL = "https://github.com/acme/repo/pull/42"
	f.prs = recoveryPR("open", "null", previous.Run.GitHubIssue.Claim.Branch, "main")
	f.closingPRURL = prURL
	previous.PRURL = prURL
	previous.Outcome = "stopped"
	previous.RunError = "operator stopped task"
	previous.Run.Workflow.ResumePRURL = prURL
	if err := s.IssueClaims.Save(previous); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIssueWork(t.Context()); err != nil {
		t.Fatal(err)
	}
	if current := latestRecoveryRecord(t, s); current.State != "blocked" || current.Error != previous.RunError {
		t.Fatalf("metadata failure must remain visible: state=%s error=%q", current.State, current.Error)
	}
	doc.Workflows[0].Task.PRAssignee = "replacement"
	doc.Workflows[0].Task.PRLabels = []string{"tracking"}
	if doc, err = s.Workflows.Store.Save(doc); err != nil {
		t.Fatal(err)
	}
	originalSettings := s.ReviewSettingsStatus
	s.ReviewSettingsStatus = func(ctx context.Context) (ReviewSettingsState, error) {
		settings, err := originalSettings(ctx)
		settings.GitHubIssuesEnabled = false
		return settings, err
	}
	id, err := s.submitParsedWithIssueClaim(t.Context(), nil, previous.Run, f.submit)
	if err != nil || id == "" || len(f.submissions) != 2 {
		t.Fatalf("task rerun failed: %q %v", id, err)
	}
	policy := f.submissions[1].Workflow
	if policy.ResumePRURL != previous.PRURL || policy.PRAssignee != "replacement" || len(policy.PRLabels) != 1 ||
		policy.PolicyHash != doc.Workflows[0].ExecutionPolicyHash() || policy.EventID == run.Workflow.EventID {
		t.Fatalf("retry lost the original PR or workflow policy: %+v", policy)
	}
}

func TestDisabledRepositoryPausesMonitorAndRetainedClaimAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(ctx, s)
	record := completeRecoveryRecord(t, s, localruntime.IssueChecksPending, 1)
	record.State = localruntime.IssueChecksPending
	record.PRURL = "https://github.com/acme/repo/pull/42"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	f.prs = recoveryPR("open", "null", record.Run.GitHubIssue.Claim.Branch, "main")

	settings, err := s.ReviewSettingsStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.DisabledRepositories = []string{"https://github.com/ACME/Repo.git"}
	s.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) { return settings, nil }
	s.Broker.IngestLog("task status=completed request_id=monitored pr_url=https://github.com/acme/repo/pull/42")
	runner := &stubPRMonitorRunner{result: execx.Result{Stdout: `{"state":"OPEN","title":"Monitored PR"}`}}
	poll := func(server Server) {
		t.Helper()
		monitor := PRMergeMonitor{Broker: server.Broker, Runner: runner, Logf: t.Logf, RepositoryAllowed: server.RepositoryAllowed}
		for range 2 {
			monitor.pollOnce(ctx)
			waitForHubUITest(t, time.Second, func() bool {
				monitor.mu.Lock()
				defer monitor.mu.Unlock()
				return len(monitor.inFlight) == 0
			})
		}
	}
	var requests []string
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
		requests = append(requests, strings.Join(cmd.Args, " "))
		return f.Run(ctx, cmd)
	})
	if err := s.ReconcileIssueWork(ctx); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 || !strings.Contains(requests[0], "/git/matching-refs/tags/") ||
		!strings.Contains(requests[1], "/git/tags/") || !strings.HasPrefix(requests[2], "issue view") ||
		!strings.Contains(requests[3], "--method DELETE") || f.deletes != 1 {
		t.Fatalf("expected only owned label cleanup, got %v", requests)
	}

	restarted := NewServer("", s.Broker)
	restarted.IssueClaims = &localruntime.IssueClaimStore{Root: s.IssueClaims.Root}
	restarted.IssueRecovery = s.IssueRecovery
	restarted.Workflows = s.Workflows
	restarted.GitHubIssueRunner = s.GitHubIssueRunner
	restarted.ReviewSettingsStatus = s.ReviewSettingsStatus
	restarted.SubmitLocalPrompt = f.submit
	before := len(requests)
	for _, server := range []Server{s, restarted} {
		poll(server)
		for range 2 {
			if err := server.ReconcileIssueWork(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range []string{"/api/github/issues?repo=acme/repo", "/api/github/issues/17?repo=acme/repo",
			"/api/workflows/pr-options?repo=acme/repo", "/api/github/automation/retry?repo=acme/repo&number=17"} {
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s returned %d, want 403", path, response.Code)
			}
		}
	}
	got := latestRecoveryRecord(t, restarted)
	if got.ID != record.ID || got.RequestID != record.RequestID || got.PRURL != record.PRURL ||
		got.State != record.State || got.LabelAdded || got.UnlockPending || len(runner.Commands()) != 0 || len(requests) != before || f.deletes != 1 {
		t.Fatalf("disabled repository changed retained work or polled GitHub, record=%+v monitor=%d issue=%d cleanup=%d",
			got, len(runner.Commands()), len(requests)-before, f.deletes)
	}

	settings.DisabledRepositories = nil
	submitted := 0
	restarted.SubmitLocalPrompt = func(_ context.Context, body []byte) (string, error) {
		submitted++
		var run config.Config
		if err := json.Unmarshal(body, &run); err != nil {
			return "", err
		}
		if run.GitHubIssue.Claim.ID != record.ID || run.Workflow.EventID != record.Run.Workflow.EventID {
			t.Fatal("reenabling changed the claim or activation")
		}
		return record.RequestID, nil
	}
	if err := restarted.ReconcileIssueWork(ctx); err != nil || submitted != 1 {
		t.Fatalf("reenabled claim resumed %d times, error=%v", submitted, err)
	}
	poll(restarted)
	if len(runner.Commands()) != 2 || len(restarted.Broker.TaskList()) != 1 {
		t.Fatalf("expected resumed polling of the same task, lookups=%d tasks=%d", len(runner.Commands()), len(restarted.Broker.TaskList()))
	}
	response := httptest.NewRecorder()
	restarted.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/github/issues/17?repo=acme/repo", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("reenabled repository detail returned %d: %s", response.Code, response.Body)
	}
}

func TestIssueRecoveryRecordsReleaseWithoutMergeTimestamp(t *testing.T) {
	t.Parallel()
	for _, state := range []string{localruntime.IssueStateMerged, localruntime.IssueStateMergedUnverified} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s, _ := recoveryServer(t)
			reconcileOrAdmitTestIssue(t.Context(), s)
			record := completeRecoveryRecord(t, s, "error", 1)
			record.PRURL = "https://github.com/acme/repo/pull/42"
			verified := state == localruntime.IssueStateMerged
			if !verified {
				record.PRVerification = "Draft verification incomplete"
			}
			record.State = state
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			s.Broker.now = func() time.Time { return now }
			s.Broker.RecordTaskRunConfig(record.RequestID, []byte(`{}`))
			s.Broker.IngestLog("task status=error request_id=" + record.RequestID + " pr_url=" + record.PRURL)
			cleanups := 0
			s.CloseTask = func(_ context.Context, requestID string) error {
				if requestID != record.RequestID {
					t.Fatalf("cleanup targeted another task: %s", requestID)
				}
				cleanups++
				return nil
			}
			for range 2 {
				if err := s.ReconcileIssueWork(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			_, hasConfig := s.Broker.TaskRunConfig(record.RequestID)
			if (cleanups == 1) != verified || cleanups > 1 || hasConfig == verified {
				t.Fatalf("verified=%t cleanups=%d run config retained=%t", verified, cleanups, hasConfig)
			}
			task, _ := s.Broker.Task(record.RequestID)
			releases := s.Broker.Snapshot().Releases
			if task.Status != "merged" || len(releases) != 1 || releases[0].PRURL != record.PRURL ||
				releases[0].MergedAt != now.Format(time.RFC3339) {
				t.Fatalf("merge without timestamp lost its release: task=%+v releases=%+v", task, releases)
			}
		})
	}
}

func TestIssueRecoveryBoundsMergeTimestampBackfill(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.State = localruntime.IssueStateMerged
	record.PRURL = "https://github.com/acme/repo/pull/42"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.Broker.now = func() time.Time { return now }
	polls := 0
	s.GitHubIssueRunner = githubIssueRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
		if strings.Contains(strings.Join(cmd.Args, " "), "/pulls/42") {
			polls++
			return execx.Result{}, errors.New("merge timestamp unavailable")
		}
		return f.Run(ctx, cmd)
	})
	for _, step := range []struct {
		advance time.Duration
		polls   int
	}{
		{0, 1},
		{time.Minute, 1},
		{retainedIssuePRPollInterval, 2},
		{defaultClosedTaskRetention, 2},
	} {
		now = now.Add(step.advance)
		if err := s.ReconcileIssueWork(t.Context()); err != nil || polls != step.polls {
			t.Fatalf("backfill after %v: polls=%d want=%d err=%v", step.advance, polls, step.polls, err)
		}
	}
	if tasks := s.snapshot().Tasks; len(tasks) != 0 {
		t.Fatalf("terminal task card did not expire: %+v", tasks)
	}
}

func TestIssueRecoveryDisablesTaskRetryWhenLinkedPRBlocksRecovery(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, "error", 1)
	record.PRURL = "https://github.com/acme/repo/pull/42"
	record.State, record.Incident = "blocked", localruntime.IssueIncidentHumanAction
	record.RunError = "worker requires human action"
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(record.Run)
	if err != nil {
		t.Fatal(err)
	}
	s.Broker.RecordTaskRunConfig(record.RequestID, body)
	s.Broker.IngestLog("task status=error request_id=" + record.RequestID + " pr_url=" + record.PRURL)
	now := time.Now().Add(-time.Second)
	s.Broker.now = func() time.Time { return now }
	for i, tc := range []struct{ state, base, blocker string }{
		{"closed", "main", "reopen the existing pull request"},
		{"open", "other", "retarget the existing pull request"},
		{"open", "main", ""},
	} {
		t.Run(tc.state+"/"+tc.base, func(t *testing.T) {
			if i > 0 {
				now = now.Add(retainedIssuePRPollInterval)
			}
			f.prs = recoveryPR(tc.state, "null", record.Run.GitHubIssue.Claim.Branch, tc.base)
			if err := s.ReconcileIssueWork(t.Context()); err != nil {
				t.Fatal(err)
			}
			tasks := s.snapshot().Tasks
			if len(tasks) != 1 || tasks[0].CanRerun != (tc.blocker == "") || !strings.Contains(tasks[0].Error, tc.blocker) ||
				tasks[0].IssueAttempts[0].Error != record.RunError {
				t.Fatalf("wrong retry eligibility or missing attempt evidence: %+v", tasks)
			}
		})
	}
}

func TestIssueRecoveryResumesChecksOnlyAfterLinkedPRBlockerClears(t *testing.T) {
	t.Parallel()
	s, f := recoveryServer(t)
	reconcileOrAdmitTestIssue(t.Context(), s)
	record := completeRecoveryRecord(t, s, localruntime.IssueChecksPending, 1)
	record.PRURL = "https://github.com/acme/repo/pull/42"
	record.State, record.UnlockPending = localruntime.IssueChecksPending, false
	if err := s.IssueClaims.Save(record); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.Broker.now = func() time.Time { return now }

	for _, tc := range []struct{ state, base, blocker string }{
		{"closed", "main", "reopen the existing pull request"},
		{"open", "other", "retarget the existing pull request"},
		{"open", "main", ""},
	} {
		t.Run(tc.state+"/"+tc.base, func(t *testing.T) {
			now = now.Add(retainedIssuePRPollInterval)
			f.prs = recoveryPR(tc.state, "null", record.Run.GitHubIssue.Claim.Branch, tc.base)
			before := len(f.submissions)
			if err := s.ReconcileIssueWork(t.Context()); err != nil {
				t.Fatal(err)
			}
			claim := latestRecoveryRecord(t, s)
			delta := len(f.submissions) - before
			if tc.blocker == "" {
				if delta != 1 || claim.PRRecoveryBlocker != "" || claim.RequestID != record.RequestID {
					t.Fatalf("corrected PR did not resume the same task: claim=%+v, submissions=%d", claim, delta)
				}
				return
			}
			tasks := s.snapshot().Tasks
			if delta != 0 || !strings.Contains(claim.PRRecoveryBlocker, tc.blocker) || len(tasks) != 1 ||
				tasks[0].IssueState != "blocked" || tasks[0].CanRerun || !strings.Contains(tasks[0].Error, tc.blocker) {
				t.Fatalf("blocked PR resumed or rendered as active: blocker=%q, submissions=%d, tasks=%+v", claim.PRRecoveryBlocker, delta, tasks)
			}
		})
	}
}

func TestIssueRecoveryPausesFailedAttemptWhileLinkedPRIsBlocked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state, base, action string
		linked              bool
	}{
		{"closed", "main", "reopen the existing pull request", true},
		{"open", "other", "retarget the existing pull request", true},
		{"closed", "main", "reopen the existing pull request", false},
		{"open", "other", "retarget the existing pull request", false},
	} {
		t.Run(fmt.Sprintf("%s/%s/linked=%t", tc.state, tc.base, tc.linked), func(t *testing.T) {
			t.Parallel()
			s, f := recoveryServer(t)
			reconcileOrAdmitTestIssue(t.Context(), s)
			record := completeRecoveryRecord(t, s, "error", 1)
			const prURL = "https://github.com/acme/repo/pull/42"
			if tc.linked {
				record.PRURL = prURL
			}
			record.PRVerification = "Verify the draft checks."
			record.RunError = "previous verification attempt failed"
			record.NextAttemptAt = time.Now().Add(-time.Minute)
			if err := s.IssueClaims.Save(record); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			s.Broker.now = func() time.Time { return now }
			f.prs = recoveryPR(tc.state, "null", record.Run.GitHubIssue.Claim.Branch, tc.base)
			if err := s.ReconcileIssueWork(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := latestRecoveryRecord(t, s)
			if len(f.submissions) != 1 || got.PRURL != prURL || got.Incident != record.Incident || got.RunError != record.RunError ||
				!strings.Contains(got.PRRecoveryBlocker, tc.action) {
				t.Fatalf("linked PR blocker changed the failed attempt or dispatched a retry: %+v", got)
			}
			tasks := s.snapshot().Tasks
			if len(tasks) != 1 || tasks[0].CanRerun || tasks[0].IssuePRRecoveryBlocker != got.PRRecoveryBlocker {
				t.Fatalf("blocked task is missing its recovery action: %+v", tasks)
			}
			f.prs = recoveryPR("open", "null", record.Run.GitHubIssue.Claim.Branch, "main")
			now = now.Add(retainedIssuePRPollInterval)
			if err := s.ReconcileIssueWork(t.Context()); err != nil {
				t.Fatal(err)
			}
			got = latestRecoveryRecord(t, s)
			if len(f.submissions) != 2 || got.PRRecoveryBlocker != "" || got.PRURL != prURL ||
				issueTaskID(got.Run.GitHubIssue, got.Run.Workflow) != issueTaskID(record.Run.GitHubIssue, record.Run.Workflow) {
				t.Fatalf("corrected PR did not resume the same issue task: %+v", got)
			}
		})
	}
}
