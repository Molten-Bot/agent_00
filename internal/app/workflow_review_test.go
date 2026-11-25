package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

const cleanReviewOutput = `{"status":"clean","mergeReady":true,"findings":[]}`

type reviewAPIStub struct {
	t            *testing.T
	head         string
	review       githubutil.SubmittedReview
	posts        int
	writeErr     error
	lostResponse bool
}

func (r *reviewAPIStub) handle(cmd execx.Command) (execx.Result, bool, error) {
	if cmd.Name != "gh" {
		return execx.Result{}, false, nil
	}
	if len(cmd.Args) >= 5 && slices.Equal(cmd.Args[:3], []string{"api", "--method", "POST"}) && strings.HasSuffix(cmd.Args[3], "/reviews") {
		r.posts++
		var submitted struct {
			Head        string `json:"commit_id"`
			Event, Body string
		}
		if err := json.Unmarshal([]byte(cmd.Stdin), &submitted); err != nil || submitted.Event != "COMMENT" || submitted.Head == "" {
			r.t.Errorf("invalid submitted review: %+v, %v", submitted, err)
		}
		if r.writeErr != nil {
			return execx.Result{}, true, r.writeErr
		}
		r.review = githubutil.SubmittedReview{
			ID: int64(r.posts), Head: submitted.Head, State: "COMMENTED", Body: submitted.Body,
			URL: "https://github.com/acme/repo/pull/42#pullrequestreview-1", SubmittedAt: "2026-11-25T12:00:00Z",
		}
		if r.lostResponse {
			return execx.Result{}, true, errors.New("response lost")
		}
		data, _ := json.Marshal(r.review)
		return execx.Result{Stdout: string(data)}, true, nil
	}
	if len(cmd.Args) == 3 && cmd.Args[0] == "api" && cmd.Args[1] == "--paginate" && strings.Contains(cmd.Args[2], "/reviews?") {
		if r.review.ID == 0 {
			return execx.Result{Stdout: "[]"}, true, nil
		}
		data, _ := json.Marshal([]githubutil.SubmittedReview{r.review})
		return execx.Result{Stdout: string(data)}, true, nil
	}
	if slices.Contains(cmd.Args, "state,headRefOid,reviewDecision") {
		return execx.Result{Stdout: fmt.Sprintf(`{"state":"OPEN","headRefOid":%q,"reviewDecision":null}`, r.head)}, true, nil
	}
	if len(cmd.Args) >= 4 && cmd.Args[0] == "api" && cmd.Args[1] == "graphql" && strings.Contains(cmd.Args[3], "reviewThreads(first:100") {
		return execx.Result{Stdout: fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"state":"OPEN","headRefOid":%q,"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`, r.head)}, true, nil
	}
	return execx.Result{}, false, nil
}

func TestStandaloneReviewRequiresNativeReceipt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, head, writeback                       string
		lostResponse, writeFails, checksFail, merge bool
	}{
		{name: "acknowledged", head: "abc123", merge: true},
		{name: "lost response reconciled", head: "abc123", merge: true, lostResponse: true},
		{name: "write failed", head: "abc123", writeFails: true},
		{name: "required check failed", head: "abc123", checksFail: true},
		{name: "stale review", head: "new-head"},
		{name: "writeback disabled", head: "abc123", writeback: "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prURL := "https://github.com/acme/repo/pull/42"
			api := reviewAPIStub{t: t, head: tc.head, lostResponse: tc.lostResponse}
			if tc.writeFails {
				api.writeErr = errors.New("upstream timeout")
			}
			merges := 0
			h := New(sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				if res, ok, err := api.handle(cmd); ok {
					return res, err
				}
				if slices.Contains(cmd.Args, "repos/acme/repo/pulls/42/merge") {
					merges++
					if !slices.Contains(cmd.Args, "sha=abc123") {
						t.Errorf("merge was not pinned to reviewed head")
					}
					return execx.Result{Stdout: `{"merged":true}`}, nil
				}
				if slices.Contains(cmd.Args, "state,headRefOid,statusCheckRollup") {
					conclusion := "SUCCESS"
					if tc.checksFail {
						conclusion = "FAILURE"
					}
					return execx.Result{Stdout: fmt.Sprintf(`{"state":"OPEN","headRefOid":%q,"statusCheckRollup":[{"name":"CI","conclusion":%q}]}`, tc.head, conclusion)}, nil
				}
				if strings.Contains(strings.Join(cmd.Args, " "), "mergedAt") {
					return execx.Result{Stdout: `{"state":"OPEN"}`}, nil
				}
				t.Errorf("unexpected command: %v", cmd.Args)
				return execx.Result{}, errors.New("unexpected command")
			}))
			h.Sleep = func(context.Context, time.Duration) error { return nil }
			repo := repoWorkspace{}
			cfg := config.Config{Review: &config.ReviewConfig{PRNumber: 42, AutoMerge: tc.writeback != "off", Writeback: tc.writeback}}
			prepared := &preparedReviewContext{Metadata: reviewPRMetadata{Number: 42, URL: prURL, State: "OPEN", HeadRefOID: "abc123"}}
			runDir := t.TempDir()
			err := h.completeReviewRun(t.Context(), cfg, &repo, prepared, execx.Result{Stdout: cleanReviewOutput}, runDir)
			if (err != nil) != (tc.writeFails || tc.checksFail || tc.head != "abc123") {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if got := merges == 1; got != tc.merge || merges > 1 {
				t.Fatalf("merge calls = %d", merges)
			}
			if tc.merge {
				receipt, err := h.publishSubmittedReview(t.Context(), cfg, repo, prepared, runDir, 1, 0, reviewOutcome{Status: "clean", MergeReady: true}, "retry", nil)
				if err != nil || !receipt.Clean() || api.posts != 1 {
					t.Fatalf("resume wrote another review or lost receipt: %+v, %v, writes %d", receipt, err, api.posts)
				}
			}
			if tc.writeFails {
				_, err := h.publishSubmittedReview(t.Context(), cfg, repo, prepared, runDir, 1, 0, reviewOutcome{Status: "clean", MergeReady: true}, "retry", nil)
				if err == nil || api.posts != 1 {
					t.Fatalf("ambiguous write permitted another submission: %v, writes %d", err, api.posts)
				}
			}
		})
	}
}

func TestReviewFindingThreadsUseDiffLines(t *testing.T) {
	t.Parallel()
	patch := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,2 +10,2 @@\n context\n-old\n+new\n"
	findings := []reviewFinding{
		{Path: "main.go", Line: 11, Title: "Handle the error"},
		{Path: "main.go", Line: 22, Title: "Outside the diff"},
	}
	comments := inlineReviewFindings(patch, findings)
	if len(comments) != 1 || comments[0].Path != "main.go" || comments[0].Line != 11 || comments[0].Side != "RIGHT" {
		t.Fatalf("inline findings = %+v", comments)
	}
}

func TestReviewResolvesOnlyVerifiedHarnessThreads(t *testing.T) {
	t.Parallel()
	const page = `{"data":{"repository":{"pullRequest":{"state":"OPEN","headRefOid":"new",
		"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
			{"id":"verified","isResolved":false,"comments":{"totalCount":1,"nodes":[{"body":"fixed finding","pullRequestReview":{"id":"own-review"}}]}},
			{"id":"unverified","isResolved":false,"comments":{"totalCount":1,"nodes":[{"body":"outstanding finding","pullRequestReview":{"id":"own-review"}}]}},
			{"id":"has-reply","isResolved":false,"comments":{"totalCount":2,"nodes":[{"body":"fixed finding","pullRequestReview":{"id":"own-review"}}]}},
			{"id":"other-reviewer","isResolved":false,"comments":{"totalCount":1,"nodes":[{"body":"unrelated","pullRequestReview":{"id":"other-review"}}]}}
		]}
	}}}}`
	var resolved []string
	h := New(sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if slices.ContainsFunc(cmd.Args, func(s string) bool { return strings.Contains(s, "resolveReviewThread(") }) {
			id := strings.TrimPrefix(cmd.Args[len(cmd.Args)-1], "id=")
			resolved = append(resolved, id)
			return execx.Result{Stdout: fmt.Sprintf(`{"data":{"resolveReviewThread":{"thread":{"id":%q,"isResolved":true}}}}`, id)}, nil
		}
		return execx.Result{Stdout: page}, nil
	}))
	captured := &preparedReviewContext{Metadata: reviewPRMetadata{URL: "https://github.com/acme/repo/pull/42", HeadRefOID: "new"}}
	own, err := h.addressableReviewThreads(t.Context(), repoWorkspace{}, captured, map[string]string{"own-review": "old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.resolveAddressedReviewThreads(t.Context(), repoWorkspace{}, captured, own, []string{"verified", "other-reviewer", "has-reply"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resolved, []string{"verified"}) {
		t.Fatalf("resolved threads = %v", resolved)
	}
}
