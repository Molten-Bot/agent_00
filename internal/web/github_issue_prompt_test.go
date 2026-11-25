package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/githubassets"
)

const issuePromptCommentPages = `[{"user":{"login":"reviewer"},"body":"First page requirement","created_at":"2026-01-02T03:04:05Z","html_url":"https://github.com/acme/repo/issues/17#issuecomment-1"}]
[{"user":{"login":"maintainer"},"body":"Second page clarification","created_at":"2026-01-03T03:04:05Z","html_url":"https://github.com/acme/repo/issues/17#issuecomment-2"}]`

func TestIssueAdmissionBuildsFreshPrompt(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"automatic", "manual"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newAutomationGitHub()
			f.comments = issuePromptCommentPages
			f.issue.Body += "\n![Screenshot](https://github.com/user-attachments/assets/shot-id)"
			f.comments = strings.Replace(f.comments, "Second page clarification", "Second page clarification [spec](https://github.com/user-attachments/files/42/spec.pdf)", 1)
			// Exercise URL fallback in both the prompt and persisted metadata.
			f.issue.URL = ""
			s := automationServer(t, f)
			if mode == "automatic" {
				reconcileOrAdmitTestIssue(context.Background(), s)
			} else {
				run := issueRunConfig(GitHubRepo{FullName: "acme/repo"}, f.issue, config.GitHubIssueLinkClose)
				run.Prompt = "Stale browser description"
				run.GitHubIssue.Title = "Stale browser issue title"
				run.PRTitle = "Stale browser PR title"
				run.Assets = []githubassets.Asset{{URL: "https://github.com/user-attachments/assets/stale", Name: "stale"}}
				run.AgentHarness = "claude"
				body, _ := json.Marshal(run)
				if _, err := s.submitWithIssueClaim(context.Background(), body, f.submit); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.submissions) != 1 {
				t.Fatalf("submissions=%d status=%+v", len(f.submissions), s.issueAutomation.status)
			}
			run := f.submissions[0]
			if run.GitHubIssue.Title != f.issue.Title || run.PRTitle != f.issue.Title {
				t.Fatalf("issue title = %q, PR title = %q, want %q", run.GitHubIssue.Title, run.PRTitle, f.issue.Title)
			}
			if len(run.Assets) != 2 || run.Assets[0].Source != "Issue description" || run.Assets[1].Source != "Comment 2" || !strings.Contains(run.Prompt, "Task assets (2)") {
				t.Fatalf("missing authoritative issue attachments: %+v", run.Assets)
			}
			for _, want := range []string{
				"Fix backlog", "Customer task", "Issue URL: https://github.com/acme/repo/issues/17",
				"Issue comments (2)", "Comment 1 by @reviewer", "Comment 2 by @maintainer",
				"First page requirement", "Second page clarification", "2026-01-02T03:04:05Z",
				"https://github.com/acme/repo/issues/17#issuecomment-2", "Assignees: @octocat",
				"reserved branch agent00-issue-17", "PR targets main", "attempt 1",
				"committing, pushing", "repository instructions",
			} {
				if !strings.Contains(run.Prompt, want) {
					t.Errorf("prompt missing %q: %s", want, run.Prompt)
				}
			}
			if strings.Contains(run.Prompt, "Stale browser") || run.GitHubIssue.URL != "https://github.com/acme/repo/issues/17" {
				t.Fatalf("stale prompt or missing URL: %+v", run)
			}
			if mode == "manual" {
				if run.AgentHarness != "claude" || !strings.Contains(run.Prompt, "closing reference") {
					t.Fatal("manual runtime or link mode was lost")
				}
			} else if !strings.Contains(run.Prompt, "without requesting automatic closure") {
				t.Fatal("reference mode instructions missing")
			}
			views, comments := 0, 0
			for _, call := range f.trace {
				if strings.HasPrefix(call, "issue view ") {
					views++
				}
				if strings.Contains(call, "/issues/17/comments") {
					comments++
				}
			}
			if views != 1 || comments != 1 {
				t.Fatalf("redundant context fetches: issue=%d comments=%d", views, comments)
			}
			records, err := s.IssueClaims.List()
			if err != nil || len(records) != 1 || records[0].Run.Prompt != run.Prompt || len(records[0].Run.Assets) != 2 {
				t.Fatalf("submitted context not persisted: %v", err)
			}
		})
	}
}

func TestIssueRetryRefreshesPromptAndPreservesOptions(t *testing.T) {
	t.Parallel()
	for _, localRecord := range []bool{true, false} {
		name := "another instance"
		if localRecord {
			name = "saved run"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAutomationGitHub()
			f.issue.Body += "\nhttps://github.com/user-attachments/assets/old"
			s := automationServer(t, f)
			run := issueRunConfig(GitHubRepo{FullName: "acme/repo"}, f.issue, config.GitHubIssueLinkClose)
			run.AgentHarness = "claude"
			run.TargetSubdir = "src"
			body, _ := json.Marshal(run)
			if _, err := s.submitWithIssueClaim(context.Background(), body, f.submit); err != nil {
				t.Fatal(err)
			}
			id := f.submissions[0].GitHubIssue.Claim.ID
			f.issue.Title = "Updated issue title"
			f.issue.Body = "Updated description"
			f.comments = issuePromptCommentPages
			f.comments = strings.Replace(f.comments, "Second page clarification", "Second page clarification https://github.com/user-attachments/files/42/new.pdf", 1)
			if !localRecord {
				s = automationServer(t, f)
			}
			if response := retryIssue(t, s, id, true); response.Code != 202 {
				t.Fatalf("retry: %d %s", response.Code, response.Body.String())
			}
			retried := f.submissions[1]
			if retried.GitHubIssue.Title != f.issue.Title || retried.PRTitle != f.issue.Title {
				t.Fatalf("retry issue title = %q, PR title = %q, want %q", retried.GitHubIssue.Title, retried.PRTitle, f.issue.Title)
			}
			if len(retried.Assets) != 1 || retried.Assets[0].Name != "new.pdf" || retried.Assets[0].Source != "Comment 2" {
				t.Fatalf("retry did not refresh attachments: %+v", retried.Assets)
			}
			for _, want := range []string{"Updated issue title", "Updated description", "Second page clarification", "attempt 2", "reserved branch agent00-issue-17", "PR targets main"} {
				if !strings.Contains(retried.Prompt, want) {
					t.Errorf("retry prompt missing %q", want)
				}
			}
			if strings.Contains(retried.Prompt, "Customer task") || strings.Contains(retried.Prompt, "Fix backlog") {
				t.Fatal("retry kept old issue content")
			}
			if localRecord && (retried.AgentHarness != "claude" || retried.TargetSubdir != "src" || retried.GitHubIssue.LinkMode != config.GitHubIssueLinkClose) {
				t.Fatal("retry lost saved runtime options")
			}
		})
	}
}

func TestIssueContextFetchFailureDoesNotReserveOrSubmit(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"issue view", "/issues/17/comments", "malformed comments"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			f := newAutomationGitHub()
			f.fail = failure
			if failure == "malformed comments" {
				f.comments = `[{"body":`
			}
			s := automationServer(t, f)
			reconcileOrAdmitTestIssue(context.Background(), s)
			if len(f.submissions) != 0 || f.branch || len(f.tags) != 0 || s.issueAutomation.status.Error == "" {
				t.Fatalf("incomplete context admitted: status=%+v", s.issueAutomation.status)
			}
		})
	}
}

func TestIssuePromptEmptyContent(t *testing.T) {
	t.Parallel()
	run := issueRunConfig(GitHubRepo{FullName: "acme/repo"}, GitHubIssue{Number: 17, Title: "No-op"}, config.GitHubIssueLinkReference)
	for _, want := range []string{"No-op", "No description provided.", "Issue comments (0)", "No comments yet.", "If no code change is needed, explain why."} {
		if !strings.Contains(run.Prompt, want) {
			t.Errorf("empty issue prompt missing %q", want)
		}
	}
}
