package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func TestFailedIssueCanBeStartedOrRerun(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"/api/local-prompt", "/api/tasks/request-1/rerun"} {
		for _, status := range []string{"error", "invalid", "stopped"} {
			t.Run(endpoint+"/"+status, func(t *testing.T) {
				f := newAutomationGitHub()
				s := automationServer(t, f)
				run := issueRunConfig(GitHubRepo{FullName: "acme/repo", DefaultBranch: "main"}, f.issue, "reference")
				run.AgentHarness = "claude"
				body, _ := json.Marshal(run)
				if _, err := s.submitWithIssueClaim(context.Background(), body, f.submit); err != nil {
					t.Fatal(err)
				}
				original := f.submissions[0].GitHubIssue.Claim
				saved, _ := json.Marshal(f.submissions[0])
				s.Broker.RecordTaskRunConfig("request-1", saved)
				s.Broker.IngestLog("task status=" + status + " request_id=request-1")
				f.issue.Body = "Updated issue requirements"
				s.SubmitTaskRerun = func(ctx context.Context, previous string, body []byte, force bool) (string, error) {
					if previous != "request-1" || force {
						t.Fatalf("rerun previous=%q force=%t", previous, force)
					}
					id, err := f.submit(ctx, body)
					s.Broker.RecordTaskRunConfig(id, body)
					return id, err
				}
				s.pollGitHubIssueAutoStart(context.Background())
				if len(f.submissions) != 1 {
					t.Fatal("background poll restarted a failed task")
				}
				response := httptest.NewRecorder()
				s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body))))
				if response.Code != http.StatusAccepted || len(f.submissions) != 2 {
					t.Fatalf("restart: %d %s submissions=%d", response.Code, response.Body.String(), len(f.submissions))
				}
				retried := f.submissions[1]
				claim := retried.GitHubIssue.Claim
				if claim.ID == original.ID || claim.Attempt != 2 || claim.Branch != original.Branch || retried.BaseBranch != run.BaseBranch || retried.AgentHarness != "claude" {
					t.Fatalf("restart lost branch/options or reused claim: %+v", retried)
				}
				if !strings.Contains(retried.Prompt, f.issue.Body) {
					t.Fatal("restart did not refresh the issue")
				}
				if len(f.refs) != 2 || !strings.Contains(strings.Join(f.trace, "\n"), `"object":"existing-commits"`) {
					t.Fatal("restart did not reserve a new attempt at the existing branch tip")
				}
				response = httptest.NewRecorder()
				s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body))))
				if response.Code == http.StatusAccepted || len(f.submissions) != 2 {
					t.Fatal("repeated start admitted a duplicate worker")
				}
			})
		}
	}
}

func TestIssueStartRecoversPriorAttempts(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"missing task", "restarted instance", "new remote owner", "blocked admission", "completed", "no_changes", "stale supplied claim", "active"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAutomationGitHub()
			s := automationServer(t, f)
			s.pollGitHubIssueAutoStart(context.Background())
			saved, _ := json.Marshal(f.submissions[0])
			if scenario != "missing task" {
				s.Broker.RecordTaskRunConfig("request-1", saved)
				status := "error"
				if scenario == "completed" || scenario == "no_changes" || scenario == "active" {
					status = scenario
				}
				s.Broker.IngestLog("task status=" + status + " request_id=request-1")
			}
			switch scenario {
			case "restarted instance":
				restarted := automationServer(t, f)
				restarted.IssueClaims = s.IssueClaims
				restarted.Broker = s.Broker
				s = restarted
			case "new remote owner":
				other := automationServer(t, f)
				if response := retryIssue(t, other, f.submissions[0].GitHubIssue.Claim.ID, true); response.Code != http.StatusAccepted {
					t.Fatalf("other instance retry: %s", response.Body.String())
				}
			case "blocked admission":
				records, _ := s.IssueClaims.List()
				records[0].Phase = "blocked"
				if err := s.IssueClaims.Save(records[0]); err != nil {
					t.Fatal(err)
				}
			}
			run := issueRunConfig(GitHubRepo{FullName: "acme/repo", DefaultBranch: "main"}, f.issue, "reference")
			if scenario == "stale supplied claim" {
				run = f.submissions[0]
				claim := *run.GitHubIssue.Claim
				claim.ID = newIssueClaimID()
				run.GitHubIssue.Claim = &claim
			}
			body, _ := json.Marshal(run)
			before := len(f.submissions)
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/local-prompt", strings.NewReader(string(body))))
			if scenario == "active" {
				if !strings.Contains(response.Body.String(), "active local task") {
					t.Fatalf("active worker not detected: %s", response.Body.String())
				}
			} else if response.Code != http.StatusAccepted || len(f.submissions) != before+1 {
				t.Fatalf("start did not recover: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestIssueRecoveryAfterBranchReservationFailure(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	s := automationServer(t, f)
	f.fail = "query=mutation"
	s.pollGitHubIssueAutoStart(context.Background())
	records, err := s.IssueClaims.List()
	if err != nil || len(records) != 1 || f.branch || len(f.submissions) != 0 {
		t.Fatalf("failed reservation: records=%v branch=%t err=%v", records, f.branch, err)
	}
	f.fail = ""
	if response := retryIssue(t, s, "unrecorded", true); response.Code != http.StatusAccepted {
		t.Fatalf("recovery: %d %s", response.Code, response.Body.String())
	}
	if !f.branch || len(f.submissions) != 1 || f.submissions[0].GitHubIssue.Claim.Attempt != 1 {
		t.Fatal("recovery failed to reserve and submit the issue")
	}
}

func TestFailedIssueConcurrentRestartsAdmitOnce(t *testing.T) {
	t.Parallel()
	f := newAutomationGitHub()
	s := automationServer(t, f)
	s.pollGitHubIssueAutoStart(context.Background())
	saved, _ := json.Marshal(f.submissions[0])
	s.Broker.RecordTaskRunConfig("request-1", saved)
	s.Broker.IngestLog("task status=error request_id=request-1")
	run := issueRunConfig(GitHubRepo{FullName: "acme/repo", DefaultBranch: "main"}, f.issue, "reference")
	body, _ := json.Marshal(run)
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := s.submitWithIssueClaim(context.Background(), body, func(ctx context.Context, body []byte) (string, error) {
				id, err := f.submit(ctx, body)
				s.Broker.RecordTaskRunConfig(id, body)
				return id, err
			})
			errors <- err
		}()
	}
	accepted := 0
	for range 2 {
		if <-errors == nil {
			accepted++
		}
	}
	if accepted != 1 || len(f.submissions) != 2 {
		t.Fatalf("accepted=%d submissions=%d", accepted, len(f.submissions))
	}
	// Durable records remain available for audit and explicit recovery.
	records, err := s.IssueClaims.List()
	if err != nil || len(records) != 2 || records[0].Phase != "submitted" || records[1].Phase != "submitted" {
		t.Fatalf("receipts=%+v err=%v", records, err)
	}
}

func TestIssueStartIgnoresStaleDevelopmentLinks(t *testing.T) {
	t.Parallel()
	for _, durable := range []bool{false, true} {
		for _, scenario := range []string{"deleted branch", "extra links", "unlinked branch", "no local history"} {
			t.Run(fmt.Sprintf("durable=%t/%s", durable, scenario), func(t *testing.T) {
				f := newAutomationGitHub()
				s := automationServer(t, f)
				if durable {
					var backend *recoveryGitHub
					s, backend = recoveryServer(t)
					f = backend.automationGitHub
				}
				s.pollGitHubIssueAutoStart(context.Background())
				if len(f.submissions) != 1 {
					t.Fatal("initial task not submitted")
				}
				switch scenario {
				case "deleted branch":
					f.branch = false
				case "extra links":
					f.extraLinks = true
				case "unlinked branch":
					f.unlinked = true
				case "no local history":
					s.IssueClaims = &localruntime.IssueClaimStore{Root: t.TempDir()}
				}
				run := issueRunConfig(GitHubRepo{FullName: "acme/repo", DefaultBranch: "main"}, f.issue, "reference")
				body, _ := json.Marshal(run)
				response := httptest.NewRecorder()
				s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/local-prompt", strings.NewReader(string(body))))
				if response.Code != http.StatusAccepted || len(f.submissions) != 2 || !f.branch {
					t.Fatalf("start: %d %s submissions=%d", response.Code, response.Body.String(), len(f.submissions))
				}
				if f.submissions[1].GitHubIssue.Claim.Attempt != 2 {
					t.Fatal("start did not advance the attempt")
				}
			})
		}
	}
}
