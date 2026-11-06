package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

// titleGateOnEdited is the trigger block a title gate needs for a retitle to
// re-run it.
const titleGateOnEdited = "on:\n  pull_request:\n    types: [opened, edited, reopened]\n"

const titleGateWorkflow = `name: PR Title
on:
  pull_request:
    types: [opened, edited, reopened]
jobs:
  check-title:
    runs-on: ubuntu-latest
    steps:
      - name: Validate title
        uses: amannn/action-semantic-pull-request@v5
        with:
          types: |
            feat
            chore
            fix
            hotfix
            refactor
          requireScope: true
          scopes: |
            #\d+
            ABC-\d+
            deps
            deps-dev
`

func writeTitleGateWorkflow(t *testing.T, content string) string {
	t.Helper()
	repoDir := t.TempDir()
	dir := filepath.Join(repoDir, ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create workflow dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pr-title-check.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return repoDir
}

func TestDetectPRTitleGate(t *testing.T) {
	t.Parallel()
	repoDir := writeTitleGateWorkflow(t, titleGateWorkflow)

	gate, found := detectPRTitleGate(repoDir)
	if !found {
		t.Fatal("expected a title gate to be detected")
	}
	if gate.WorkflowPath != ".github/workflows/pr-title-check.yaml" {
		t.Fatalf("workflow path = %q", gate.WorkflowPath)
	}
	if !gate.RequireScope {
		t.Fatal("requireScope was not parsed")
	}
	if want := []string{"feat", "chore", "fix", "hotfix", "refactor"}; !reflect.DeepEqual(gate.Types, want) {
		t.Fatalf("types = %v, want %v", gate.Types, want)
	}
	if want := []string{`#\d+`, `ABC-\d+`, "deps", "deps-dev"}; !reflect.DeepEqual(gate.Scopes, want) {
		t.Fatalf("scopes = %v, want %v", gate.Scopes, want)
	}
}

func TestDetectPRTitleGateVariants(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		workflow string
		found    bool
		gate     prTitleGate
	}{
		{
			name: "inline flow sequence",
			workflow: titleGateOnEdited + "jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n" +
				"        with:\n" +
				"          types: [feat, chore]\n" +
				"          requireScope: 'false'\n",
			found: true,
			gate:  prTitleGate{Types: []string{"feat", "chore"}},
		},
		{
			name: "dash list with comment",
			workflow: titleGateOnEdited + "jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n" +
				"        with:\n" +
				"          types:\n" +
				"            # allowed release types\n" +
				"            - feat\n" +
				"            - fix\n",
			found: true,
			gate:  prTitleGate{Types: []string{"feat", "fix"}},
		},
		{
			name: "step without inputs still gates",
			workflow: titleGateOnEdited + "jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n" +
				"  other:\n    steps: []\n",
			found: true,
		},
		{
			name: "pull_request_target with quoted on key",
			workflow: "\"on\":\n  pull_request_target:\n    types:\n      - opened\n      - edited\n" +
				"jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n" +
				"        with:\n          types: [feat]\n",
			found: true,
			gate:  prTitleGate{Types: []string{"feat"}},
		},
		{
			name: "types omit edited",
			workflow: "on:\n  pull_request:\n    types: [opened, synchronize]\n" +
				"jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n        with:\n          types: [feat]\n",
		},
		{
			name: "default pull_request trigger without types",
			workflow: "on:\n  pull_request:\njobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n",
		},
		{
			name: "inline trigger list keeps default types",
			workflow: "on: [pull_request]\njobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n",
		},
		{
			name: "edited only on an unrelated event",
			workflow: "on:\n  issues:\n    types: [edited]\n  pull_request:\n    types: [opened]\n" +
				"jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n",
		},
		{
			name:     "unrelated workflow",
			workflow: titleGateOnEdited + "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate, found := detectPRTitleGate(writeTitleGateWorkflow(t, tc.workflow))
			if found != tc.found {
				t.Fatalf("found = %v, want %v", found, tc.found)
			}
			if !found {
				return
			}
			gate.WorkflowPath = ""
			if !reflect.DeepEqual(gate, tc.gate) {
				t.Fatalf("gate = %+v, want %+v", gate, tc.gate)
			}
		})
	}
}

func TestDetectPRTitleGateMissingWorkflowsDir(t *testing.T) {
	t.Parallel()
	if _, found := detectPRTitleGate(t.TempDir()); found {
		t.Fatal("expected no gate without a workflows directory")
	}
	if _, found := detectPRTitleGate(""); found {
		t.Fatal("expected no gate for an empty repo dir")
	}
}

func TestConformingPRTitle(t *testing.T) {
	t.Parallel()
	strictGate := prTitleGate{
		Types:        []string{"feat", "chore", "fix", "hotfix", "refactor"},
		Scopes:       []string{`#\d+`, `ABC-\d+`, "deps", "deps-dev"},
		RequireScope: true,
	}
	for _, tc := range []struct {
		name  string
		gate  prTitleGate
		title string
		pr    int
		want  string
		ok    bool
	}{
		{
			name:  "rejected type and scope",
			gate:  strictGate,
			title: "ci(automation): tighten release checks",
			pr:    42,
			want:  "chore(#42): tighten release checks",
			ok:    true,
		},
		{
			name:  "missing scope only",
			gate:  strictGate,
			title: "fix: restore nightly job",
			pr:    12,
			want:  "fix(#12): restore nightly job",
			ok:    true,
		},
		{
			name:  "no conventional prefix",
			gate:  strictGate,
			title: "Harden CI validation checks",
			pr:    7,
			want:  "chore(#7): Harden CI validation checks",
			ok:    true,
		},
		{
			name:  "already conforming",
			gate:  strictGate,
			title: "chore(deps): bump toolchain",
			pr:    9,
		},
		{
			name:  "scope required but underivable",
			gate:  prTitleGate{Types: []string{"chore"}, Scopes: []string{`ABC-\d+`}, RequireScope: true},
			title: "ci: tighten checks",
		},
		{
			name:  "optional scope is dropped when rejected",
			gate:  prTitleGate{Types: []string{"chore"}, Scopes: []string{"api"}},
			title: "chore(automation): tighten checks",
			want:  "chore: tighten checks",
			ok:    true,
		},
		{
			name:  "literal scope allowlist is never guessed",
			gate:  prTitleGate{Types: []string{"chore"}, Scopes: []string{"api", "web"}, RequireScope: true},
			title: "ci: tighten checks",
			pr:    42,
		},
		{
			name:  "deps is never guessed",
			gate:  prTitleGate{Types: []string{"chore"}, Scopes: []string{"deps", "deps-dev"}, RequireScope: true},
			title: "ci: bump toolchain",
		},
		{
			name:  "required scope without a pr number",
			gate:  prTitleGate{Types: []string{"chore"}, RequireScope: true},
			title: "ci: tighten checks",
		},
		{
			name:  "required scope with an open allowlist uses the pr number",
			gate:  prTitleGate{Types: []string{"chore"}, RequireScope: true},
			title: "ci: tighten checks",
			pr:    42,
			want:  "chore(#42): tighten checks",
			ok:    true,
		},
		{
			name:  "gate without inputs keeps the title",
			title: "ci(automation): tighten checks",
		},
		{
			name:  "empty subject",
			gate:  strictGate,
			title: "   ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := conformingPRTitle(tc.title, tc.gate, tc.pr)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("conformingPRTitle(%q) = (%q, %v), want (%q, %v)", tc.title, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestPRTitleGateOnlyFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		res     execx.Result
		summary string
		want    bool
	}{
		{
			name:    "truncated summary hides another failure visible in raw output",
			res:     execx.Result{Stdout: "check-title\tfail\t6s\t\nType Check\tfail\t1m\t"},
			summary: "check-title\tfail\t6s\t\nType Check\tfa",
		},
		{
			name:    "failure only in the summary still blocks",
			res:     execx.Result{Stdout: "check-title\tfail\t6s\t"},
			summary: "Type Check\tfail\t1m\t",
		},
		{
			name: "raw output only",
			res:  execx.Result{Stderr: "check-title\tfail\t6s\t"},
			want: true,
		},
		{
			name:    "only the title gate is red",
			summary: "check-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint\nAI PR QA\tskipping\t0\t",
			want:    true,
		},
		{
			name:    "another check is also red",
			summary: "check-title\tfail\t6s\t\nType Check\tfail\t1m\t",
		},
		{
			name:    "title gate recovered in a later row",
			summary: "check-title\tfail\t6s\t\ncheck-title\tpass\t6s\t",
		},
		{
			name:    "nothing is red",
			summary: "Lint & Format\tpass\t35s\t",
		},
		{
			name:    "non tabular output",
			summary: "no checks reported on the branch",
		},
		{
			name:    "pr title gate named by the action",
			summary: "Semantic Pull Request Title\tfail\t3s\t\nUnit tests\tpass\t1m\t",
			want:    true,
		},
		{
			name:    "title-like check that is not a gate",
			summary: "title\tfail\t3s\t",
		},
		{
			name:    "a red commit title linter blocks the repair",
			summary: "check-title\tfail\t6s\t\nLint commit titles\tfail\t4s\t",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := prTitleGateOnlyFailure(tc.res, tc.summary); got != tc.want {
				t.Fatalf("prTitleGateOnlyFailure = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCapturedChecksHaveTitleGateFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		res  execx.Result
		want bool
	}{
		{name: "failing title gate in stdout", res: execx.Result{Stdout: "check-title\tfail\t6s\t"}, want: true},
		{name: "failing title gate in stderr", res: execx.Result{Stderr: "check-title\ttimed_out\t6s\t"}, want: true},
		{name: "other check failing keeps the fallback", res: execx.Result{Stdout: "Type Check\tfail\t1m\t"}},
		{name: "all passing", res: execx.Result{Stdout: "Lint\tpass\t1s\t"}},
		{name: "pending only", res: execx.Result{Stdout: "check-title\tpending\t0\t"}},
		{name: "no table", res: execx.Result{Stderr: "no required checks reported on the 'topic' branch"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := capturedChecksHaveTitleGateFailures(tc.res); got != tc.want {
				t.Fatalf("capturedChecksHaveTitleGateFailures = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRepairPRTitleGateFailureRetitlesPullRequest(t *testing.T) {
	t.Parallel()
	repo := repoWorkspace{
		URL:              "https://github.com/acme/repo",
		Dir:              writeTitleGateWorkflow(t, titleGateWorkflow),
		RelDir:           "repo",
		PRURL:            "https://github.com/acme/repo/pull/42",
		PRCreatedThisRun: true,
	}
	want := "chore(#42): tighten release checks"
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prTitleViewCommand(repo.Dir, repo.PRURL), res: execx.Result{Stdout: `{"title":"ci(automation): tighten release checks"}`}},
		{cmd: prEditTitleCommand(repo.Dir, repo.PRURL, want), res: execx.Result{}},
	}}

	var logs []string
	h := New(fake)
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	repaired, err := h.repairPRTitleGateFailure(
		context.Background(),
		repo,
		execx.Result{},
		"check-title\tfail\t6s\thttps://example.com/job\nLint\tpass\t1s\t",
	)
	if err != nil || !repaired {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (true, nil)", repaired, err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "stage=pr status=ok action=retitle reason=title_gate") {
		t.Fatalf("missing retitle log: %v", logs)
	}
}

func TestRepairPRTitleGateFailureRetitlesPullRequestReusedOnHarnessBranch(t *testing.T) {
	t.Parallel()
	// A re-run on the same harness branch adopts the pull request an earlier
	// run opened, so the harness still owns its title.
	repo := repoWorkspace{
		URL:    "https://github.com/acme/repo",
		Dir:    writeTitleGateWorkflow(t, titleGateWorkflow),
		RelDir: "repo",
		Branch: "agent00-build-api",
		PRURL:  "https://github.com/acme/repo/pull/42",
	}
	want := "chore(#42): tighten release checks"
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prTitleViewCommand(repo.Dir, repo.PRURL), res: execx.Result{Stdout: `{"title":"ci(automation): tighten release checks"}`}},
		{cmd: prEditTitleCommand(repo.Dir, repo.PRURL, want), res: execx.Result{}},
	}}

	h := New(fake)
	h.Logf = func(string, ...any) {}

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, execx.Result{}, "check-title\tfail\t6s\t")
	if err != nil || !repaired {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (true, nil)", repaired, err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}
}

func TestRepairPRTitleGateFailureSkipsWhenNotApplicable(t *testing.T) {
	t.Parallel()
	gateDir := writeTitleGateWorkflow(t, titleGateWorkflow)
	prURL := "https://github.com/acme/repo/pull/42"
	for _, tc := range []struct {
		name    string
		repo    repoWorkspace
		summary string
	}{
		{
			name:    "no pull request",
			repo:    repoWorkspace{Dir: gateDir, PRCreatedThisRun: true},
			summary: "check-title\tfail\t6s\t",
		},
		{
			name:    "pull request was not created by this run",
			repo:    repoWorkspace{Dir: gateDir, PRURL: prURL},
			summary: "check-title\tfail\t6s\t",
		},
		{
			name:    "branch the harness did not name",
			repo:    repoWorkspace{Dir: gateDir, PRURL: prURL, Branch: "feature/login"},
			summary: "check-title\tfail\t6s\t",
		},
		{
			name:    "pr feedback target",
			repo:    repoWorkspace{Dir: gateDir, PRURL: prURL, PRCreatedThisRun: true, RequireExistingPR: true},
			summary: "check-title\tfail\t6s\t",
		},
		{
			name:    "other checks also failing",
			repo:    repoWorkspace{Dir: gateDir, PRURL: prURL, PRCreatedThisRun: true},
			summary: "check-title\tfail\t6s\t\nType Check\tfail\t1m\t",
		},
		{
			name:    "no title gate in the repository",
			repo:    repoWorkspace{Dir: t.TempDir(), PRURL: prURL, PRCreatedThisRun: true},
			summary: "check-title\tfail\t6s\t",
		},
		{
			name:    "gate does not re-run on a title edit",
			repo:    repoWorkspace{Dir: writeTitleGateWorkflow(t, strings.Replace(titleGateWorkflow, ", edited", "", 1)), PRURL: prURL, PRCreatedThisRun: true},
			summary: "check-title\tfail\t6s\t",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{t: t}
			h := New(fake)
			h.Logf = func(string, ...any) {}
			repaired, err := h.repairPRTitleGateFailure(context.Background(), tc.repo, execx.Result{}, tc.summary)
			if repaired || err != nil {
				t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (false, nil)", repaired, err)
			}
		})
	}
}

func TestRepairPRTitleGateFailurePropagatesTitleLookupError(t *testing.T) {
	t.Parallel()
	repo := repoWorkspace{
		Dir:              writeTitleGateWorkflow(t, titleGateWorkflow),
		PRURL:            "https://github.com/acme/repo/pull/42",
		PRCreatedThisRun: true,
	}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prTitleViewCommand(repo.Dir, repo.PRURL), err: errors.New("exit status 1")},
	}}
	h := New(fake)
	h.Logf = func(string, ...any) {}

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, execx.Result{}, "check-title\tfail\t6s\t")
	if repaired || err == nil {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (false, error)", repaired, err)
	}
}

func TestRepairPRTitleGateFailureKeepsTitleWhenLookupIsEmpty(t *testing.T) {
	t.Parallel()
	repo := repoWorkspace{
		Dir:              writeTitleGateWorkflow(t, titleGateWorkflow),
		PRURL:            "https://github.com/acme/repo/pull/42",
		PRCreatedThisRun: true,
	}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prTitleViewCommand(repo.Dir, repo.PRURL), res: execx.Result{Stdout: `{"title":""}`}},
	}}
	h := New(fake)
	h.Logf = func(string, ...any) {}

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, execx.Result{}, "check-title\tfail\t6s\t")
	if err != nil || repaired {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (false, nil)", repaired, err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}
}

func TestRunRetitlesPullRequestWhenOnlyTitleGateFails(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	cfg.PRTitle = "ci(automation): harden CI validation checks"
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "titlegate0001"
	runDir := testRunDir(guid)
	agentsPath := filepath.Join(runDir, "AGENTS.md")
	repoDir := filepath.Join(runDir, "repo")
	targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
	branch := "agent00-build-api"
	prURL := "https://github.com/acme/repo/pull/42"
	repairedTitle := "chore(#42): harden CI validation checks"

	workflowDir := filepath.Join(repoDir, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatalf("create workflow dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "pr-title-check.yaml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	// The gate is red while everything else is green, which is what the watch
	// prints before it exits nonzero.
	checksTable := "check-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint"
	redChecks := execx.Result{Stdout: checksTable}
	checksJSON := `[{"name":"check-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`

	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: execx.Command{Name: "git", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "codex", Args: []string{"--help"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"auth", "status"}}},
		{cmd: cloneCommand(cfg, repoDir)},
		{cmd: branchCommand(repoDir, branch)},
		{cmd: pushDryRunCommand(repoDir, branch)},
		{cmd: codexCommand(targetDir, withAgentsPrompt(cfg.Prompt, agentsPath))},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: " M file.go\n"}},
		{cmd: addCommand(repoDir)},
		{cmd: commitCommand(repoDir, cfg.CommitMessage)},
		{cmd: pushCommand(repoDir, branch)},
		{cmd: prCreateCommand(repoDir, cfg, branch), res: execx.Result{Stdout: prURL + "\n"}},
		{cmd: prChecksCommand(repoDir, prURL), res: redChecks, err: errors.New("exit status 1")},
		{cmd: prChecksJSONCommand(repoDir, prURL, false), res: execx.Result{Stdout: checksJSON}},
		{cmd: prTitleViewCommand(repoDir, prURL), res: execx.Result{Stdout: `{"title":"` + cfg.PRTitle + `"}`}},
		{cmd: prEditTitleCommand(repoDir, prURL, repairedTitle)},
		{cmd: prChecksCommand(repoDir, prURL), res: execx.Result{Stdout: "check-title\tpass\t6s\t\nLint & Format\tpass\t35s\t"}},
	}}

	var logs []string
	h := New(fake)
	h.Now = func() time.Time { return now }
	h.Workspace = testWorkspaceManager(guid)
	h.TargetDirOK = func(path string) bool { return path == targetDir }
	h.Sleep = func(context.Context, time.Duration) error { return nil }
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	res := h.Run(context.Background(), cfg)
	if res.Err != nil || res.ExitCode != ExitSuccess {
		t.Fatalf("Run() = (exit %d, %v), want (%d, nil)", res.ExitCode, res.Err, ExitSuccess)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}

	joined := strings.Join(logs, "\n")
	if want := "stage=pr status=ok action=retitle reason=title_gate repo=" + cfg.RepoURL; !strings.Contains(joined, want) {
		t.Fatalf("missing log %q in:\n%s", want, joined)
	}
	if strings.Contains(joined, "reason=latest_snapshot") {
		t.Fatalf("checks were declared green while a check was red:\n%s", joined)
	}
	if strings.Contains(joined, "mode=remediation") {
		t.Fatalf("a remediation agent ran for a pull-request title gate:\n%s", joined)
	}
}

func TestRunWaitsForTitleGateRerunAfterRetitle(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	cfg.PRTitle = "ci(automation): harden CI validation checks"
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "titlegate0002"
	runDir := testRunDir(guid)
	agentsPath := filepath.Join(runDir, "AGENTS.md")
	repoDir := filepath.Join(runDir, "repo")
	targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
	branch := "agent00-build-api"
	prURL := "https://github.com/acme/repo/pull/42"
	repairedTitle := "chore(#42): harden CI validation checks"

	workflowDir := filepath.Join(repoDir, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatalf("create workflow dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "pr-title-check.yaml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	checksTable := "check-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint"
	redChecks := execx.Result{Stdout: checksTable}
	checksJSON := `[{"name":"check-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`

	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: execx.Command{Name: "git", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "codex", Args: []string{"--help"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"auth", "status"}}},
		{cmd: cloneCommand(cfg, repoDir)},
		{cmd: branchCommand(repoDir, branch)},
		{cmd: pushDryRunCommand(repoDir, branch)},
		{cmd: codexCommand(targetDir, withAgentsPrompt(cfg.Prompt, agentsPath))},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: " M file.go\n"}},
		{cmd: addCommand(repoDir)},
		{cmd: commitCommand(repoDir, cfg.CommitMessage)},
		{cmd: pushCommand(repoDir, branch)},
		{cmd: prCreateCommand(repoDir, cfg, branch), res: execx.Result{Stdout: prURL + "\n"}},
		{cmd: prChecksCommand(repoDir, prURL), res: redChecks, err: errors.New("exit status 1")},
		{cmd: prChecksJSONCommand(repoDir, prURL, false), res: execx.Result{Stdout: checksJSON}},
		{cmd: prTitleViewCommand(repoDir, prURL), res: execx.Result{Stdout: `{"title":"` + cfg.PRTitle + `"}`}},
		{cmd: prEditTitleCommand(repoDir, prURL, repairedTitle)},
		// The edited-title run has not replaced the stale red one yet.
		{cmd: prChecksCommand(repoDir, prURL), res: redChecks, err: errors.New("exit status 1")},
		{cmd: prChecksJSONCommand(repoDir, prURL, false), res: execx.Result{Stdout: checksJSON}},
		{cmd: prChecksCommand(repoDir, prURL), res: execx.Result{Stdout: "check-title\tpass\t6s\t\nLint & Format\tpass\t35s\t"}},
	}}

	var logs []string
	h := New(fake)
	h.Now = func() time.Time { return now }
	h.Workspace = testWorkspaceManager(guid)
	h.TargetDirOK = func(path string) bool { return path == targetDir }
	h.Sleep = func(context.Context, time.Duration) error { return nil }
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	res := h.Run(context.Background(), cfg)
	if res.Err != nil || res.ExitCode != ExitSuccess {
		t.Fatalf("Run() = (exit %d, %v), want (%d, nil)", res.ExitCode, res.Err, ExitSuccess)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "stage=checks status=waiting reason=title_gate_rerun") {
		t.Fatalf("missing title gate rerun wait log:\n%s", joined)
	}
	if strings.Count(joined, "action=retitle reason=title_gate") != 1 {
		t.Fatalf("the title was repaired more than once:\n%s", joined)
	}
	if strings.Contains(joined, "mode=remediation") {
		t.Fatalf("a remediation agent ran for a pull-request title gate:\n%s", joined)
	}
}

func TestRunRepairsTitleGateAgainForARecreatedPullRequest(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	cfg.PRTitle = "ci(automation): harden CI validation checks"
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "titlegate0004"
	runDir := testRunDir(guid)
	agentsPath := filepath.Join(runDir, "AGENTS.md")
	repoDir := filepath.Join(runDir, "repo")
	targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
	branch := "agent00-build-api"
	recreatedBranch := collisionAvoidanceBranchName(branch, runBranchCollisionSuffix(now, guid))
	prURL := "https://github.com/acme/repo/pull/42"
	recreatedPRURL := "https://github.com/acme/repo/pull/43"

	workflowDir := filepath.Join(repoDir, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatalf("create workflow dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "pr-title-check.yaml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	redChecks := execx.Result{Stdout: "check-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint"}
	checksJSON := `[{"name":"check-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`
	greenChecks := execx.Result{Stdout: "check-title\tpass\t6s\t\nLint & Format\tpass\t35s\t"}

	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: execx.Command{Name: "git", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "codex", Args: []string{"--help"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"auth", "status"}}},
		{cmd: cloneCommand(cfg, repoDir)},
		{cmd: branchCommand(repoDir, branch)},
		{cmd: pushDryRunCommand(repoDir, branch)},
		{cmd: codexCommand(targetDir, withAgentsPrompt(cfg.Prompt, agentsPath))},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: " M file.go\n"}},
		{cmd: addCommand(repoDir)},
		{cmd: commitCommand(repoDir, cfg.CommitMessage)},
		{cmd: pushCommand(repoDir, branch)},
		{cmd: prCreateCommand(repoDir, cfg, branch), res: execx.Result{Stdout: prURL + "\n"}},
		{cmd: prChecksCommand(repoDir, prURL), res: redChecks, err: errors.New("exit status 1")},
		{cmd: prChecksJSONCommand(repoDir, prURL, false), res: execx.Result{Stdout: checksJSON}},
		{cmd: prTitleViewCommand(repoDir, prURL), res: execx.Result{Stdout: `{"title":"` + cfg.PRTitle + `"}`}},
		{cmd: prEditTitleCommand(repoDir, prURL, "chore(#42): harden CI validation checks")},
		// The pull request is closed before the gate re-runs, so the harness
		// opens a replacement that carries the configured title again.
		{cmd: prChecksCommand(repoDir, prURL), err: errors.New("no checks reported on the 'agent00-build-api' branch")},
		{cmd: prStateViewCommand(repoDir, prURL), res: execx.Result{Stdout: `{"url":"` + prURL + `","state":"closed"}`}},
		{cmd: branchMoveCommand(repoDir, recreatedBranch)},
		{cmd: pushToRemoteCommand(repoDir, publishRemoteOrigin, recreatedBranch)},
		{cmd: prCreateWithOptionsCommand(repoDir, cfg, cfg.BaseBranch, recreatedBranch, ""), res: execx.Result{Stdout: recreatedPRURL + "\n"}},
		{cmd: prChecksCommand(repoDir, recreatedPRURL), res: redChecks, err: errors.New("exit status 1")},
		{cmd: prChecksJSONCommand(repoDir, recreatedPRURL, false), res: execx.Result{Stdout: checksJSON}},
		{cmd: prTitleViewCommand(repoDir, recreatedPRURL), res: execx.Result{Stdout: `{"title":"` + cfg.PRTitle + `"}`}},
		{cmd: prEditTitleCommand(repoDir, recreatedPRURL, "chore(#43): harden CI validation checks")},
		{cmd: prChecksCommand(repoDir, recreatedPRURL), res: greenChecks},
	}}

	var logs []string
	h := New(fake)
	h.Now = func() time.Time { return now }
	h.Workspace = testWorkspaceManager(guid)
	h.TargetDirOK = func(path string) bool { return path == targetDir }
	h.Sleep = func(context.Context, time.Duration) error { return nil }
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	res := h.Run(context.Background(), cfg)
	if res.Err != nil || res.ExitCode != ExitSuccess {
		t.Fatalf("Run() = (exit %d, %v), want (%d, nil)", res.ExitCode, res.Err, ExitSuccess)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}

	joined := strings.Join(logs, "\n")
	if got := strings.Count(joined, "action=retitle reason=title_gate"); got != 2 {
		t.Fatalf("retitle count = %d, want 2 (one per pull request):\n%s", got, joined)
	}
	if strings.Contains(joined, "mode=remediation") {
		t.Fatalf("a remediation agent ran for a pull-request title gate:\n%s", joined)
	}
}

func TestRunRepairsTitleGateMissedByRequiredOnlySnapshot(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	cfg.PRTitle = "ci(automation): harden CI validation checks"
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "titlegate0003"
	runDir := testRunDir(guid)
	agentsPath := filepath.Join(runDir, "AGENTS.md")
	repoDir := filepath.Join(runDir, "repo")
	targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
	branch := "agent00-build-api"
	prURL := "https://github.com/acme/repo/pull/42"
	repairedTitle := "chore(#42): harden CI validation checks"

	workflowDir := filepath.Join(repoDir, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatalf("create workflow dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "pr-title-check.yaml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	// The watch times out with the red gate already streamed, while the
	// required-only snapshot is green because the gate is not a required check.
	timedOutChecks := execx.Result{Stdout: "check-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint"}
	requiredJSON := `[{"name":"Lint & Format","bucket":"pass"}]`
	anyJSON := `[{"name":"check-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`

	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: execx.Command{Name: "git", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"--version"}}},
		{cmd: execx.Command{Name: "codex", Args: []string{"--help"}}},
		{cmd: execx.Command{Name: "gh", Args: []string{"auth", "status"}}},
		{cmd: cloneCommand(cfg, repoDir)},
		{cmd: branchCommand(repoDir, branch)},
		{cmd: pushDryRunCommand(repoDir, branch)},
		{cmd: codexCommand(targetDir, withAgentsPrompt(cfg.Prompt, agentsPath))},
		{cmd: statusCommand(repoDir), res: execx.Result{Stdout: " M file.go\n"}},
		{cmd: addCommand(repoDir)},
		{cmd: commitCommand(repoDir, cfg.CommitMessage)},
		{cmd: pushCommand(repoDir, branch)},
		{cmd: prCreateCommand(repoDir, cfg, branch), res: execx.Result{Stdout: prURL + "\n"}},
		{cmd: prChecksCommand(repoDir, prURL), res: timedOutChecks, err: errPRChecksWatchTimeout},
		{cmd: prChecksJSONCommand(repoDir, prURL, true), res: execx.Result{Stdout: requiredJSON}},
		{cmd: prChecksJSONCommand(repoDir, prURL, false), res: execx.Result{Stdout: anyJSON}},
		{cmd: prTitleViewCommand(repoDir, prURL), res: execx.Result{Stdout: `{"title":"` + cfg.PRTitle + `"}`}},
		{cmd: prEditTitleCommand(repoDir, prURL, repairedTitle)},
		{cmd: prChecksCommand(repoDir, prURL), res: execx.Result{Stdout: "check-title\tpass\t6s\t\nLint & Format\tpass\t35s\t"}},
	}}

	var logs []string
	h := New(fake)
	h.Now = func() time.Time { return now }
	h.Workspace = testWorkspaceManager(guid)
	h.TargetDirOK = func(path string) bool { return path == targetDir }
	h.Sleep = func(context.Context, time.Duration) error { return nil }
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	res := h.Run(context.Background(), cfg)
	if res.Err != nil || res.ExitCode != ExitSuccess {
		t.Fatalf("Run() = (exit %d, %v), want (%d, nil)", res.ExitCode, res.Err, ExitSuccess)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}

	joined := strings.Join(logs, "\n")
	for _, want := range []string{
		"stage=checks status=warn action=watch_timeout_snapshot reason=title_gate_failed",
		"stage=pr status=ok action=retitle reason=title_gate repo=" + cfg.RepoURL,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing log %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "status=ok reason=watch_timeout") {
		t.Fatalf("checks were declared green while the title gate was red:\n%s", joined)
	}
	if strings.Contains(joined, "mode=remediation") {
		t.Fatalf("a remediation agent ran for a pull-request title gate:\n%s", joined)
	}
}

func TestIsPRTitleGateCheckName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"check-title":                          true,
		"PR Title":                             true,
		"Validate PR title":                    true,
		"Semantic Pull Request Title":          true,
		"lint_pr_titles":                       true,
		"conventional-title":                   true,
		"Lint PR title (conventional commits)": true,
		"Title":                                false,
		"Sprint title sync":                    false,
		"Type Check":                           false,
		"Lint commit titles":                   false,
		"commit-title-check":                   false,
		"":                                     false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isPRTitleGateCheckName(name); got != want {
				t.Fatalf("isPRTitleGateCheckName(%q) = %v, want %v", name, got, want)
			}
		})
	}
}
