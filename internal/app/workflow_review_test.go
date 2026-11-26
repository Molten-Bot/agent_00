package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

const cleanReviewOutput = `{"status":"clean","mergeReady":true,"findings":[]}`

type reviewAPIStub struct {
	t            *testing.T
	head         string
	review       githubutil.SubmittedReview
	posts        int
	writeErr     error
	lostResponse bool
	hideReview   bool
	comments     []inlineReviewFinding
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
			Comments    []inlineReviewFinding
		}
		if err := json.Unmarshal([]byte(cmd.Stdin), &submitted); err != nil || submitted.Event != "COMMENT" || submitted.Head == "" {
			r.t.Errorf("invalid submitted review: %+v, %v", submitted, err)
		}
		if r.writeErr != nil {
			return execx.Result{}, true, r.writeErr
		}
		r.comments = submitted.Comments
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
		if r.review.ID == 0 || r.hideReview {
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
			agentRes := execx.Result{Stdout: cleanReviewOutput, Stderr: "GitHub publication connection diagnostic"}
			err := h.completeReviewRun(t.Context(), cfg, &repo, prepared, agentRes, runDir)
			if (err != nil) != (tc.writeFails || tc.checksFail || tc.head != "abc123") {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if got := merges == 1; got != tc.merge || merges > 1 {
				t.Fatalf("merge calls = %d", merges)
			}
			if tc.merge {
				if strings.Contains(api.review.Body, agentRes.Stderr) {
					t.Fatal("publication diagnostics leaked into the clean review")
				}
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

func TestStandaloneFindingReviewReconcilesAmbiguousWrite(t *testing.T) {
	t.Chdir(t.TempDir())
	runDir := filepath.Join(".agent00", "issue-workspaces", "review")
	repo := repoWorkspace{Dir: filepath.Join(runDir, "repo")}
	if err := os.MkdirAll(repo.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const finding = `{"status":"findings","mergeReady":false,"findings":[{"severity":"Medium","path":"main.go","line":11,"title":"Handle the error"}]}`
	const patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,2 +10,2 @@\n context\n-old\n+new\n"
	for _, tc := range []struct {
		name, diff   string
		wantComments int
	}{
		{name: "inline", diff: patch, wantComments: 1},
		{name: "summary only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prURL := "https://github.com/acme/repo/pull/42"
			api := reviewAPIStub{t: t, lostResponse: true, hideReview: true}
			h := New(sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				if cmd.Dir != repo.Dir {
					t.Errorf("command ran in %q, want %q", cmd.Dir, repo.Dir)
				}
				if res, ok, err := api.handle(cmd); ok {
					return res, err
				}
				if slices.ContainsFunc(cmd.Args, func(arg string) bool { return strings.Contains(arg, "state,mergedAt,") }) {
					return execx.Result{Stdout: `{"state":"OPEN"}`}, nil
				}
				t.Errorf("unexpected command: %v", cmd.Args)
				return execx.Result{}, errors.New("unexpected command")
			}))
			h.Sleep = func(context.Context, time.Duration) error { return nil }
			cfg := config.Config{Review: &config.ReviewConfig{PRNumber: 42}}
			passDir := filepath.Join(runDir, tc.name)
			if err := os.Mkdir(passDir, 0o700); err != nil {
				t.Fatal(err)
			}
			prepared := &preparedReviewContext{Diff: tc.diff, Metadata: reviewPRMetadata{URL: prURL, State: "OPEN", HeadRefOID: "abc123"}}
			agentRes := execx.Result{Stdout: "Checking the diff.\n\n```json\n" + finding + "\n```"}
			if err := h.completeReviewRun(t.Context(), cfg, &repo, prepared, agentRes, passDir); err == nil || !strings.Contains(err.Error(), "waiting for acknowledged submitted review") {
				t.Fatalf("initial completion = %v, want unacknowledged review", err)
			}
			api.hideReview = false
			// A fresh agent result cannot replace the verdict already submitted for the pass.
			if err := h.completeReviewRun(t.Context(), cfg, &repo, prepared, execx.Result{Stdout: cleanReviewOutput}, passDir); err != nil {
				t.Fatalf("reconcile review: %v", err)
			}
			if api.posts != 1 || repo.ReviewReceipt != nil || len(api.comments) != tc.wantComments ||
				!strings.HasPrefix(api.review.Body, "<!-- agent00-review ") ||
				!strings.Contains(api.review.Body, "Handle the error") || strings.Contains(api.review.Body, "Checking the diff") {
				t.Fatalf("posts = %d, inline = %+v, body = %q, clean receipt = %+v", api.posts, api.comments, api.review.Body, repo.ReviewReceipt)
			}
		})
	}
}

func TestStandaloneReviewPublishesOnlyCodeVerdicts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, output, wantError string }{
		{"progress only", "Checking the diff.", "no valid structured outcome"},
		{"stderr verdict only", "", "no valid structured outcome"},
		{"inspection blocked", `{"status":"blocked","mergeReady":false,"summary":"Checkout is incomplete","findings":[]}`, "review inspection blocked: Checkout is incomplete"},
		{"publication error as finding", `{"status":"findings","mergeReady":false,"findings":[{"title":"Could not connect to GitHub to publish"}]}`, "requires a file, line, and title"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				t.Errorf("invalid or blocked verdict attempted a GitHub command: %v", cmd.Args)
				return execx.Result{}, errors.New("unexpected command")
			}))
			h.Logf = func(format string, args ...any) {
				if strings.Contains(fmt.Sprintf(format, args...), "review_not_clean") {
					t.Error("invalid or blocked verdict was treated as a code finding")
				}
			}
			cfg := config.Config{Review: &config.ReviewConfig{PRNumber: 42}}
			prepared := &preparedReviewContext{Metadata: reviewPRMetadata{URL: "https://github.com/acme/repo/pull/42", State: "OPEN", HeadRefOID: "abc123"}}
			err := h.completeReviewRun(t.Context(), cfg, &repoWorkspace{}, prepared, execx.Result{Stdout: tc.output, Stderr: cleanReviewOutput}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("completion = %v, want %q", err, tc.wantError)
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

func TestWorkflowRepairsNativeFindingAcrossActivations(t *testing.T) {
	root := t.TempDir()
	const prURL = "https://github.com/acme/repo/pull/42"
	const repoURL = "https://github.com/acme/repo.git"
	const threadID = "native-finding"
	const finding = `{"status":"findings","mergeReady":false,"findings":[{"severity":"high","path":"divide.sh","line":1,"title":"Handle division by zero"}]}`
	const fixedScript = "if [ \"$1\" = 0 ]; then echo 'cannot divide by zero'; else echo \"$((10 / $1))\"; fi\n"
	origin, seed := filepath.Join(root, "origin.git"), filepath.Join(root, "seed")
	osRunner := execx.OSRunner{}
	runGit := func(cmd execx.Command) (execx.Result, error) {
		cmd.Args = append([]string{"-c", "user.name=Automation", "-c", "user.email=automation@example.test", "-c", "commit.gpgsign=false"}, cmd.Args...)
		return osRunner.Run(t.Context(), cmd)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		res, err := runGit(gitCommand(dir, args...))
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(root, "init", "--bare", "--initial-branch=main", origin)
	git(root, "clone", origin, seed)
	git(seed, "commit", "--allow-empty", "-m", "Initial commit")
	git(seed, "push", "origin", "main")
	git(seed, "switch", "-c", "feature")
	write(seed, "divide.sh", "echo \"$((10 / $1))\"\n")
	git(seed, "add", "divide.sh")
	git(seed, "commit", "-m", "Implement division")
	git(seed, "push", "origin", "feature")
	oldHead := git(seed, "rev-parse", "HEAD")
	git(origin, "update-ref", "refs/pull/42/head", oldHead)
	if _, err := osRunner.Run(t.Context(), execx.Command{Name: "sh", Dir: seed, Args: []string{"divide.sh", "0"}}); err == nil {
		t.Fatal("original head must reproduce division by zero")
	}

	api := &reviewAPIStub{t: t, head: oldHead}
	var firstReview githubutil.SubmittedReview
	var firstComment inlineReviewFinding
	var draft, ciPassed, resolved, merged = true, false, false, false
	var resolutions, readyCalls, merges, pageReads int
	runner := sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		reply := func(raw string) (execx.Result, error) { return execx.Result{Stdout: raw}, nil }
		args := strings.Join(cmd.Args, " ")
		switch cmd.Name {
		case "git":
			for i, arg := range cmd.Args {
				if cmd.Args[0] == "clone" && arg == repoURL {
					cmd.Args[i] = origin
				}
			}
			res, err := runGit(cmd)
			if err == nil && cmd.Args[0] == "push" && !slices.Contains(cmd.Args, "--dry-run") {
				api.head = git(origin, "rev-parse", "refs/heads/feature")
				git(origin, "update-ref", "refs/pull/42/head", api.head)
			}
			return res, err
		case "codex":
			if args == "--help" {
				return reply("")
			}
			if !strings.Contains(cmd.Stdin, threadID) || !strings.Contains(cmd.Stdin, "Handle division by zero") {
				t.Fatalf("agent prompt lacked original thread identity and finding")
			}
			if strings.Contains(cmd.Stdin, "Post-task review pass") {
				fixed, err := os.ReadFile(filepath.Join(cmd.Dir, "divide.sh"))
				if err != nil || string(fixed) != fixedScript || api.head == oldHead {
					t.Fatalf("verification did not inspect the published repair: %v, head=%s", err, api.head)
				}
				return reply(`{"status":"clean","mergeReady":true,"findings":[],"verifiedThreadIds":["native-finding","unknown"]}`)
			}
			write(cmd.Dir, "divide.sh", fixedScript)
			write(cmd.Dir, "test-divide.sh", "set -e\ntest \"$(sh divide.sh 0)\" = 'cannot divide by zero'\ntest \"$(sh divide.sh 2)\" = 5\n")
			return reply("Fixed division by zero and added regression coverage.")
		case "gh":
			switch {
			case strings.Contains(args, "reviewThreads(first:100"):
				page := `"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"next"}`
				if slices.Contains(cmd.Args, "cursor=next") {
					pageReads++
					page = fmt.Sprintf(`"nodes":[{"id":%q,"isResolved":%t,"isOutdated":%t,"comments":{"totalCount":1,"nodes":[{"id":"original-comment","body":%q,"path":%q,"originalLine":%d,"viewerDidAuthor":true,"pullRequestReview":{"id":"original-review","body":%q,"state":"COMMENTED","submittedAt":"2026-11-25","viewerDidAuthor":true,"commit":{"oid":%q}}}]}}],"pageInfo":{"hasNextPage":false}`,
						threadID, resolved, api.head != oldHead, firstComment.Body, firstComment.Path, firstComment.Line, firstReview.Body, oldHead)
				}
				return reply(fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"state":"OPEN","headRefOid":%q,"reviewThreads":{%s}}}}}`, api.head, page))
			case strings.Contains(args, "resolveReviewThread("):
				if !slices.Contains(cmd.Args, "id="+threadID) || api.review.Head == oldHead || resolved {
					t.Fatalf("unexpected thread resolution: %v", cmd.Args)
				}
				resolutions++
				resolved = true
				return execx.Result{}, errors.New("response lost after GitHub resolved the thread")
			case strings.Contains(args, "/reviews?"):
				data, _ := json.Marshal([]githubutil.SubmittedReview{firstReview, api.review})
				return reply(string(data))
			case strings.Contains(args, "closingIssuesReferences"):
				return reply("1")
			case strings.Contains(args, "/pulls/42/merge"):
				if !resolved || draft || !ciPassed || !slices.Contains(cmd.Args, "sha="+api.head) {
					t.Fatalf("merge attempted before resolution, readiness, and current-head CI")
				}
				merges++
				merged = true
				return reply(`{"merged":true}`)
			case strings.HasPrefix(args, "pr view "):
				state, conclusion := "OPEN", "FAILURE"
				if merged {
					state = "MERGED"
				}
				if ciPassed {
					conclusion = "SUCCESS"
				}
				return reply(fmt.Sprintf(`{"number":42,"url":%q,"state":%q,"baseRefName":"main","headRefName":"feature","headRefOid":%q,"reviewDecision":"","isDraft":%t,"mergeable":"MERGEABLE","statusCheckRollup":[{"name":"CI","conclusion":%q}]}`, prURL, state, api.head, draft, conclusion))
			case strings.HasPrefix(args, "pr checks "):
				return reply("CI\tpass\n")
			case strings.HasPrefix(args, "pr ready "):
				if !resolved || pageReads < 3 {
					t.Fatal("marked ready before re-reading thread resolution")
				}
				readyCalls++
				draft = false
				return reply("")
			}
			if res, ok, err := api.handle(cmd); ok {
				return res, err
			}
			if cmd.Args[0] == "api" {
				return reply("[]")
			}
			if args == "--version" || strings.HasPrefix(args, "auth ") {
				return reply("")
			}
		}
		t.Fatalf("unexpected command: %s %s", cmd.Name, args)
		return execx.Result{}, nil
	})

	makeWorkflow := func(id, event, task string) workflow.Definition {
		return workflow.Definition{ID: id, Name: id, Repository: "acme/repo", Enabled: true, IncludeBacklog: true,
			Trigger: workflow.Trigger{Event: event}, Filters: workflow.Filters{Mode: "all"},
			Task:  workflow.Task{Kind: task, Instructions: "Complete the task.", IssueLinkMode: "reference"},
			Merge: workflow.Merge{Method: "squash"}}
	}
	issue := makeWorkflow("implement", "issue.opened", "implement")
	review := makeWorkflow("review", "pr.opened", "review")
	repair := makeWorkflow("repair", "pr.checks_failed", "repair")
	repair.Merge = workflow.Merge{Enabled: true, Method: "squash"}
	store := &workflow.Store{Path: filepath.Join(root, "workflows.json")}
	if err := store.Initialize(workflow.Document{Version: workflow.Version, Workflows: []workflow.Definition{issue, review, repair}}); err != nil {
		t.Fatal(err)
	}
	prExists := false
	newEngine := func() *workflow.Engine {
		gh := workflow.GitHub{Runner: runner}
		return &workflow.Engine{Store: store, StatePath: filepath.Join(root, "workflow-state.json"),
			Enabled: func() bool { return true }, Allowed: func(string) bool { return true },
			InspectPR: gh.Inspect, Merge: gh.Merge, FinishReview: New(runner).ResumeReviewGate,
			Fetch: func(_ context.Context, _ string, _ workflow.ScanNeeds) ([]workflow.Item, error) {
				items := []workflow.Item{{Number: 1, Kind: "issue", Repository: "acme/repo", Open: true}}
				if prExists && !merged {
					items = append(items, workflow.Item{Number: 42, Kind: "pr", Repository: "acme/repo", Open: true, URL: prURL, ChecksFailed: firstReview.ID != 0 && api.head == oldHead})
				}
				return items, nil
			},
			Submit: func(ctx context.Context, r workflow.Run) (string, error) {
				outcome := workflow.Outcome{PRURL: prURL, Status: "succeeded"}
				switch r.Workflow.ID {
				case issue.ID:
					prExists = true
				case review.ID:
					h := New(runner)
					captured := &preparedReviewContext{
						Diff:     git(seed, "diff", "main", oldHead),
						Metadata: reviewPRMetadata{URL: prURL, HeadRefOID: oldHead, State: "OPEN"},
					}
					if err := h.completeReviewRun(ctx, config.Config{Review: &config.ReviewConfig{PRNumber: 42}}, &repoWorkspace{Dir: seed}, captured, execx.Result{Stdout: finding}, t.TempDir()); err != nil {
						t.Fatal(err)
					}
					if len(api.comments) != 1 {
						t.Fatalf("expected a native inline finding, got %+v", api.comments)
					}
					firstReview, firstComment = api.review, api.comments[0]
				case repair.ID:
					h := New(runner)
					h.Workspace = testWorkspaceManager("native-finding-repair")
					cfg := config.Config{RepoURL: repoURL, BaseBranch: "feature", Prompt: "Repair PR feedback.", RequiresNonDefaultBranch: true,
						Workflow: &config.WorkflowRunConfig{ID: r.Workflow.ID, EventID: r.ID, PolicyHash: r.Workflow.ExecutionPolicyHash(), RepairPRURL: prURL}}
					result := h.Run(ctx, cfg)
					if result.Err != nil {
						t.Fatal(result.Err)
					}
					cmd := execx.Command{Name: "sh", Dir: filepath.Join(result.WorkspaceDir, "repo"), Args: []string{"test-divide.sh"}}
					if res, err := osRunner.Run(ctx, cmd); err != nil {
						t.Fatalf("repaired head failed regression tests: %v: %s", err, res.Stderr)
					}
					if err := os.RemoveAll(result.WorkspaceDir); err != nil {
						t.Fatal(err)
					}
					outcome.ReviewReceipt, outcome.ReviewRequired, outcome.PendingReviewGate = result.ReviewReceipt, result.ReviewRequired, result.PendingReviewGate
					if outcome.ReviewReceipt == nil || !outcome.ReviewReceipt.Clean() || outcome.ReviewReceipt.Head != api.head ||
						len(outcome.ReviewReceipt.VerifiedThreads) != 1 || outcome.PendingReviewGate == nil || !outcome.ReviewRequired {
						t.Fatalf("repair did not hand off head-bound evidence: %+v", outcome)
					}
				}
				if err := workflow.SaveReviewOutcome(root, r.ID, outcome); err != nil {
					t.Fatal(err)
				}
				return r.ID, nil
			},
			Status: func(id string) workflow.Outcome {
				outcome, err := workflow.LoadReviewOutcome(root, id)
				if err != nil {
					t.Fatal(err)
				}
				return outcome
			},
		}
	}
	tick := func(e *workflow.Engine) {
		t.Helper()
		if err := e.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	engine := newEngine()
	tick(engine) // Issue implementation creates the PR.
	tick(engine) // The independent native review publishes a finding.
	tick(engine) // Repair the existing PR and publish clean confirmation.
	tick(engine) // The server applies the resolution, but the response is lost.
	if resolutions != 1 || readyCalls != 0 || merges != 0 {
		runs, err := engine.Runs()
		t.Fatalf("unexpected progress after interrupted write: %d/%d/%d, runs=%+v, err=%v", resolutions, readyCalls, merges, runs, err)
	}
	engine = newEngine()
	tick(engine) // Recover the acknowledgement from the live thread.
	tick(engine) // Completed writeback still waits for current-head CI.
	if resolutions != 1 || readyCalls != 1 || merges != 0 {
		t.Fatalf("unexpected progress before CI: %d/%d/%d", resolutions, readyCalls, merges)
	}
	ciPassed = true
	pagesBeforeMerge := pageReads
	tick(engine)
	tick(engine)
	if merges != 1 || api.posts != 2 || pageReads <= pagesBeforeMerge {
		t.Fatalf("merges=%d, reviews=%d, merge thread pages=%d", merges, api.posts, pageReads-pagesBeforeMerge)
	}
}
