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

	"github.com/Molten-Bot/agent_00/internal/config"
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
  pr-title:
    runs-on: ubuntu-latest
    steps:
      - name: Validate title
        uses: amannn/action-semantic-pull-request@v5
        with:
          types: |
            feat
            chore
            fix
            release
            refactor
          requireScope: true
          scopes: |
            #\d+
            ABC-\d+
            deps
            deps-dev
`

// titleGateLiteralScopeWorkflow requires a scope the harness can never
// synthesize, so a rejected title has no repair it can derive.
const titleGateLiteralScopeWorkflow = titleGateOnEdited + `jobs:
  pr-title:
    runs-on: ubuntu-latest
    steps:
      - uses: amannn/action-semantic-pull-request@v5
        with:
          types: [feat, chore]
          requireScope: true
          scopes: [api, web]
`

func writeTitleGateWorkflow(t *testing.T, content string) string {
	t.Helper()
	repoDir := t.TempDir()
	dir := filepath.Join(repoDir, ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create workflow dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "semantic-pr-title.yml"), []byte(content), 0o644); err != nil {
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
	if gate.WorkflowPath != ".github/workflows/semantic-pr-title.yml" {
		t.Fatalf("workflow path = %q", gate.WorkflowPath)
	}
	if !gate.RequireScope {
		t.Fatal("requireScope was not parsed")
	}
	if want := []string{"feat", "chore", "fix", "release", "refactor"}; !reflect.DeepEqual(gate.Types, want) {
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
			name: "inline flow sequence with a trailing comment",
			workflow: titleGateOnEdited + "jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n" +
				"        with:\n" +
				"          types: [feat, chore] # allowed release types\n" +
				"          requireScope: true # scopes are mandatory\n",
			found: true,
			gate:  prTitleGate{Types: []string{"feat", "chore"}, RequireScope: true},
		},
		{
			name: "flow sequence closed on a later line",
			workflow: titleGateOnEdited + "jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n" +
				"        with:\n" +
				"          types: [\n" +
				"            feat,\n" +
				"            chore,\n" +
				"          ]\n" +
				"          scopes: [\n" +
				"            '#\\d+',\n" +
				"            deps,\n" +
				"            ]\n",
			found: true,
			gate:  prTitleGate{Types: []string{"feat", "chore"}, Scopes: []string{`#\d+`, "deps"}},
		},
		{
			name: "dash list with comment",
			workflow: titleGateOnEdited + "jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n" +
				"        with:\n" +
				"          types:\n" +
				"            # allowed release types\n" +
				"            - feat # new behavior\n" +
				"            - fix\n",
			found: true,
			gate:  prTitleGate{Types: []string{"feat", "fix"}},
		},
		{
			name: "subject pattern and disallowed scopes",
			workflow: titleGateOnEdited + "jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n" +
				"        with:\n" +
				"          types: [feat]\n" +
				"          subjectPattern: '^[a-z].*[^.]$'\n" +
				"          disallowScopes: [release, deps]\n",
			found: true,
			gate: prTitleGate{
				Types:          []string{"feat"},
				DisallowScopes: []string{"release", "deps"},
				SubjectPattern: `^[a-z].*[^.]$`,
			},
		},
		{
			name: "block scalar subject pattern starting with a character class",
			workflow: titleGateOnEdited + "jobs:\n  title:\n    steps:\n" +
				"      - uses: amannn/action-semantic-pull-request@v5\n" +
				"        with:\n" +
				"          subjectPattern: |\n" +
				"            [a-z].*, and more\n",
			found: true,
			gate:  prTitleGate{SubjectPattern: "[a-z].*, and more"},
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
		Types:        []string{"feat", "chore", "fix", "release", "refactor"},
		Scopes:       []string{`#\d+`, `ABC-\d+`, "deps", "deps-dev"},
		RequireScope: true,
	}
	for _, tc := range []struct {
		name    string
		gate    prTitleGate
		title   string
		numbers []int
		want    string
		ok      bool
	}{
		{
			name:    "rejected type and scope",
			gate:    strictGate,
			title:   "ci(automation): tighten release checks",
			numbers: []int{42},
			want:    "chore(#42): tighten release checks",
			ok:      true,
		},
		{
			name:    "missing scope only",
			gate:    strictGate,
			title:   "fix: restore nightly job",
			numbers: []int{12},
			want:    "fix(#12): restore nightly job",
			ok:      true,
		},
		{
			name:    "no conventional prefix",
			gate:    strictGate,
			title:   "Harden CI validation checks",
			numbers: []int{7},
			want:    "chore(#7): Harden CI validation checks",
			ok:      true,
		},
		{
			name:    "already conforming",
			gate:    strictGate,
			title:   "chore(deps): bump toolchain",
			numbers: []int{9},
		},
		{
			name:    "originating issue number wins over the pull request number",
			gate:    strictGate,
			title:   "ci: tighten release checks",
			numbers: []int{17, 42},
			want:    "chore(#17): tighten release checks",
			ok:      true,
		},
		{
			name:    "rejected issue number falls back to the pull request number",
			gate:    prTitleGate{Types: []string{"chore"}, Scopes: []string{`#4\d`}, RequireScope: true},
			title:   "ci: tighten release checks",
			numbers: []int{17, 42},
			want:    "chore(#42): tighten release checks",
			ok:      true,
		},
		{
			name:    "disallowed scope is replaced",
			gate:    prTitleGate{Types: []string{"chore"}, DisallowScopes: []string{"deps", "release"}, RequireScope: true},
			title:   "chore(release): tighten release checks",
			numbers: []int{42},
			want:    "chore(#42): tighten release checks",
			ok:      true,
		},
		{
			name:    "every candidate scope is disallowed",
			gate:    prTitleGate{Types: []string{"chore"}, DisallowScopes: []string{`#\d+`, "release"}, RequireScope: true},
			title:   "ci(release): tighten release checks",
			numbers: []int{42},
		},
		{
			name:    "subject the gate rejects keeps the title",
			gate:    prTitleGate{Types: []string{"chore"}, SubjectPattern: `^[a-z].*[^.]$`},
			title:   "ci: Tighten release checks",
			numbers: []int{42},
		},
		{
			name:    "subject the gate accepts is rewritten",
			gate:    prTitleGate{Types: []string{"chore"}, SubjectPattern: `^[a-z].*[^.]$`},
			title:   "ci: tighten release checks",
			numbers: []int{42},
			want:    "chore: tighten release checks",
			ok:      true,
		},
		{
			name:    "subject pattern go cannot compile is not enforced",
			gate:    prTitleGate{Types: []string{"chore"}, SubjectPattern: `^(?![A-Z]).+$`},
			title:   "ci: Tighten release checks",
			numbers: []int{42},
			want:    "chore: Tighten release checks",
			ok:      true,
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
			name:    "literal scope allowlist is never guessed",
			gate:    prTitleGate{Types: []string{"chore"}, Scopes: []string{"api", "web"}, RequireScope: true},
			title:   "ci: tighten checks",
			numbers: []int{42},
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
			name:    "required scope with an open allowlist uses the pr number",
			gate:    prTitleGate{Types: []string{"chore"}, RequireScope: true},
			title:   "ci: tighten checks",
			numbers: []int{42},
			want:    "chore(#42): tighten checks",
			ok:      true,
		},
		{
			name:  "gate without inputs keeps a title the default types allow",
			title: "ci(automation): tighten checks",
		},
		{
			name:  "gate without inputs replaces a type outside the default list",
			title: "wip(automation): tighten checks",
			want:  "chore(automation): tighten checks",
			ok:    true,
		},
		{
			name:  "gate without inputs supplies a type for a plain title",
			title: "Tighten checks",
			want:  "chore: Tighten checks",
			ok:    true,
		},
		{
			name:    "breaking change marker survives the rewrite",
			gate:    strictGate,
			title:   "ci(automation)!: drop the legacy endpoint",
			numbers: []int{42},
			want:    "chore(#42)!: drop the legacy endpoint",
			ok:      true,
		},
		{
			name:  "breaking change marker survives a type-only rewrite",
			gate:  prTitleGate{Types: []string{"chore"}},
			title: "ci!: drop the legacy endpoint",
			want:  "chore!: drop the legacy endpoint",
			ok:    true,
		},
		{
			name:  "breaking change marker alone is not a rewrite",
			gate:  prTitleGate{Types: []string{"chore"}},
			title: "chore!: drop the legacy endpoint",
		},
		{
			name:    "type outside the known release meanings is never stamped",
			gate:    prTitleGate{Types: []string{"release", "major"}, RequireScope: true},
			title:   "ci(automation): tighten release checks",
			numbers: []int{42},
		},
		{
			name:  "empty subject",
			gate:  strictGate,
			title: "   ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := conformingPRTitle(tc.title, tc.gate, tc.numbers)
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
			res:     execx.Result{Stdout: "pr-title\tfail\t6s\t\nType Check\tfail\t1m\t"},
			summary: "pr-title\tfail\t6s\t\nType Check\tfa",
		},
		{
			name:    "fresher summary clears a check the watch saw red",
			res:     execx.Result{Stdout: "pr-title\tfail\t6s\t\nType Check\tfail\t1m\t"},
			summary: "Type Check\tpass\t2m\t\npr-title\tfail\t6s\t",
			want:    true,
		},
		{
			// The summary can be a snapshot kept from an earlier no-report
			// retry while the watch output was refreshed, so a pending row
			// never clears a failure another source reported.
			name:    "pending summary row does not clear a failure the watch saw",
			res:     execx.Result{Stdout: "pr-title\tfail\t6s\t\nType Check\tfail\t1m\t"},
			summary: "pr-title\tfail\t6s\t\nType Check\tpending\t0\t",
		},
		{
			name:    "a later conclusive row still resolves a pending check",
			res:     execx.Result{Stdout: "pr-title\tfail\t6s\t\nType Check\tpending\t0\t"},
			summary: "pr-title\tfail\t6s\t\nType Check\tfail\t1m\t",
		},
		{
			name:    "failure only in the summary still blocks",
			res:     execx.Result{Stdout: "pr-title\tfail\t6s\t"},
			summary: "Type Check\tfail\t1m\t",
		},
		{
			name: "raw output only",
			res:  execx.Result{Stderr: "pr-title\tfail\t6s\t"},
			want: true,
		},
		{
			name:    "only the title gate is red",
			summary: "pr-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint\nAI PR QA\tskipping\t0\t",
			want:    true,
		},
		{
			name:    "another check is also red",
			summary: "pr-title\tfail\t6s\t\nType Check\tfail\t1m\t",
		},
		{
			name:    "title gate recovered in a later row",
			summary: "pr-title\tfail\t6s\t\npr-title\tpass\t6s\t",
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
			summary: "pr-title\tfail\t6s\t\nLint commit titles\tfail\t4s\t",
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
		{name: "failing title gate in stdout", res: execx.Result{Stdout: "pr-title\tfail\t6s\t"}, want: true},
		{name: "failing title gate in stderr", res: execx.Result{Stderr: "pr-title\ttimed_out\t6s\t"}, want: true},
		{name: "other check failing keeps the fallback", res: execx.Result{Stdout: "Type Check\tfail\t1m\t"}},
		{name: "all passing", res: execx.Result{Stdout: "Lint\tpass\t1s\t"}},
		{name: "pending only", res: execx.Result{Stdout: "pr-title\tpending\t0\t"}},
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

func TestTitleGateBlocksChecksSuccess(t *testing.T) {
	t.Parallel()
	gateDir := writeTitleGateWorkflow(t, titleGateWorkflow)
	literalScopeDir := writeTitleGateWorkflow(t, titleGateLiteralScopeWorkflow)
	prURL := "https://github.com/acme/repo/pull/42"
	redGate := execx.Result{Stdout: "pr-title\tfail\t6s\t"}
	rejectedTitle := execx.Result{Stdout: `{"title":"ci(automation): tighten release checks"}`}
	for _, tc := range []struct {
		name string
		repo repoWorkspace
		res  execx.Result
		exps []expectedRun
		want bool
	}{
		{
			name: "gate the harness repairs blocks the snapshot",
			repo: repoWorkspace{Dir: gateDir, PRURL: prURL, PRCreatedThisRun: true},
			res:  redGate,
			exps: []expectedRun{{cmd: prTitleViewCommand(gateDir, prURL), res: rejectedTitle}},
			want: true,
		},
		{
			name: "title the harness cannot conform keeps the passing snapshot",
			repo: repoWorkspace{Dir: literalScopeDir, PRURL: prURL, PRCreatedThisRun: true},
			res:  redGate,
			exps: []expectedRun{{cmd: prTitleViewCommand(literalScopeDir, prURL), res: rejectedTitle}},
		},
		{
			name: "title lookup failure keeps the passing snapshot",
			repo: repoWorkspace{Dir: gateDir, PRURL: prURL, PRCreatedThisRun: true},
			res:  redGate,
			exps: []expectedRun{{cmd: prTitleViewCommand(gateDir, prURL), err: errors.New("exit status 1")}},
		},
		{
			name: "pr feedback target keeps the passing snapshot",
			repo: repoWorkspace{Dir: gateDir, PRURL: prURL, PRCreatedThisRun: true, RequireExistingPR: true},
			res:  redGate,
		},
		{
			name: "branch the harness did not name keeps the passing snapshot",
			repo: repoWorkspace{Dir: gateDir, PRURL: prURL, Branch: "feature/login"},
			res:  redGate,
		},
		{
			name: "gate the harness cannot re-run keeps the passing snapshot",
			repo: repoWorkspace{
				Dir:              writeTitleGateWorkflow(t, strings.Replace(titleGateWorkflow, ", edited", "", 1)),
				PRURL:            prURL,
				PRCreatedThisRun: true,
			},
			res: redGate,
		},
		{
			name: "green gate keeps the passing snapshot",
			repo: repoWorkspace{Dir: gateDir, PRURL: prURL, PRCreatedThisRun: true},
			res:  execx.Result{Stdout: "pr-title\tpass\t6s\t"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{t: t, exps: tc.exps}
			h := New(fake)
			h.Logf = func(string, ...any) {}
			got, plan := h.titleGateBlocksChecksSuccess(context.Background(), tc.repo, nil, tc.res, "watch_timeout_snapshot", 1)
			if got != tc.want {
				t.Fatalf("titleGateBlocksChecksSuccess = %v, want %v", got, tc.want)
			}
			if got != (plan != nil) {
				t.Fatalf("plan = %+v, want one only when the gate blocks", plan)
			}
			if len(fake.exps) != 0 {
				t.Fatalf("unused expectations: %+v", fake.exps)
			}
		})
	}
}

func TestPRTitleScopeNumbers(t *testing.T) {
	t.Parallel()
	prURL := "https://github.com/acme/repo/pull/42"
	for _, tc := range []struct {
		name  string
		prURL string
		issue *config.GitHubIssueConfig
		want  []int
	}{
		{name: "no issue", prURL: prURL, want: []int{42}},
		{
			name:  "issue in the same repository leads",
			prURL: prURL,
			issue: &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17},
			want:  []int{17, 42},
		},
		{
			name:  "repository match ignores case",
			prURL: prURL,
			issue: &config.GitHubIssueConfig{Repository: "Acme/Repo", Number: 17},
			want:  []int{17, 42},
		},
		{
			name:  "issue in another repository is dropped",
			prURL: prURL,
			issue: &config.GitHubIssueConfig{Repository: "acme/other", Number: 17},
			want:  []int{42},
		},
		{
			name:  "issue without a number is dropped",
			prURL: prURL,
			issue: &config.GitHubIssueConfig{Repository: "acme/repo"},
			want:  []int{42},
		},
		{
			name:  "pull request url that carries no number",
			prURL: "42",
			issue: &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := prTitleScopeNumbers(tc.prURL, tc.issue); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("prTitleScopeNumbers = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRepairPRTitleGateFailureScopesTheOriginatingIssue(t *testing.T) {
	t.Parallel()
	repo := repoWorkspace{
		URL:              "https://github.com/acme/repo",
		Dir:              writeTitleGateWorkflow(t, titleGateWorkflow),
		RelDir:           "repo",
		PRURL:            "https://github.com/acme/repo/pull/42",
		PRCreatedThisRun: true,
	}
	want := "chore(#17): tighten release checks"
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prTitleViewCommand(repo.Dir, repo.PRURL), res: execx.Result{Stdout: `{"title":"ci(automation): tighten release checks"}`}},
		{cmd: prEditTitleCommand(repo.Dir, repo.PRURL, want), res: execx.Result{}},
	}}

	h := New(fake)
	h.Logf = func(string, ...any) {}

	repaired, err := h.repairPRTitleGateFailure(
		context.Background(),
		repo,
		&config.GitHubIssueConfig{Repository: "acme/repo", Number: 17},
		execx.Result{},
		"pr-title\tfail\t6s\t",
		nil,
	)
	if err != nil || !repaired {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (true, nil)", repaired, err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
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
		nil,
		execx.Result{},
		"pr-title\tfail\t6s\thttps://example.com/job\nLint\tpass\t1s\t",
		nil,
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

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, nil, execx.Result{}, "pr-title\tfail\t6s\t", nil)
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
			summary: "pr-title\tfail\t6s\t",
		},
		{
			name:    "pull request was not created by this run",
			repo:    repoWorkspace{Dir: gateDir, PRURL: prURL},
			summary: "pr-title\tfail\t6s\t",
		},
		{
			name:    "branch the harness did not name",
			repo:    repoWorkspace{Dir: gateDir, PRURL: prURL, Branch: "feature/login"},
			summary: "pr-title\tfail\t6s\t",
		},
		{
			name:    "pr feedback target",
			repo:    repoWorkspace{Dir: gateDir, PRURL: prURL, PRCreatedThisRun: true, RequireExistingPR: true},
			summary: "pr-title\tfail\t6s\t",
		},
		{
			name:    "other checks also failing",
			repo:    repoWorkspace{Dir: gateDir, PRURL: prURL, PRCreatedThisRun: true},
			summary: "pr-title\tfail\t6s\t\nType Check\tfail\t1m\t",
		},
		{
			name:    "no title gate in the repository",
			repo:    repoWorkspace{Dir: t.TempDir(), PRURL: prURL, PRCreatedThisRun: true},
			summary: "pr-title\tfail\t6s\t",
		},
		{
			name:    "gate does not re-run on a title edit",
			repo:    repoWorkspace{Dir: writeTitleGateWorkflow(t, strings.Replace(titleGateWorkflow, ", edited", "", 1)), PRURL: prURL, PRCreatedThisRun: true},
			summary: "pr-title\tfail\t6s\t",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{t: t}
			h := New(fake)
			h.Logf = func(string, ...any) {}
			repaired, err := h.repairPRTitleGateFailure(context.Background(), tc.repo, nil, execx.Result{}, tc.summary, nil)
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

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, nil, execx.Result{}, "pr-title\tfail\t6s\t", nil)
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

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, nil, execx.Result{}, "pr-title\tfail\t6s\t", nil)
	if err != nil || repaired {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (false, nil)", repaired, err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}
}

func TestRepairPRTitleGateFailureRetriesTransientEditFailures(t *testing.T) {
	t.Parallel()
	repo := repoWorkspace{
		Dir:              writeTitleGateWorkflow(t, titleGateWorkflow),
		PRURL:            "https://github.com/acme/repo/pull/42",
		PRCreatedThisRun: true,
	}
	want := "chore(#42): tighten release checks"
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prTitleViewCommand(repo.Dir, repo.PRURL), err: errors.New("HTTP 502: Bad gateway")},
		{cmd: prTitleViewCommand(repo.Dir, repo.PRURL), res: execx.Result{Stdout: `{"title":"ci(automation): tighten release checks"}`}},
		{cmd: prEditTitleCommand(repo.Dir, repo.PRURL, want), err: errors.New("i/o timeout")},
		{cmd: prEditTitleCommand(repo.Dir, repo.PRURL, want), res: execx.Result{}},
	}}

	var slept []time.Duration
	h := New(fake)
	h.Logf = func(string, ...any) {}
	h.Sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, nil, execx.Result{}, "pr-title\tfail\t6s\t", nil)
	if err != nil || !repaired {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (true, nil)", repaired, err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}
	if len(slept) != 2 {
		t.Fatalf("retry sleeps = %v, want two", slept)
	}
	for _, d := range slept {
		if d != prTitleGateCommandRetryDelay {
			t.Fatalf("retry delay = %s, want %s", d, prTitleGateCommandRetryDelay)
		}
	}
}

func TestRepairPRTitleGateFailureRetriesRateLimitedEdits(t *testing.T) {
	t.Parallel()
	// GitHub reports rate limiting with the same HTTP 403 it denies with, so a
	// throttled edit must be retried instead of releasing the run with a red
	// gate nothing else can clear.
	repo := repoWorkspace{
		Dir:              writeTitleGateWorkflow(t, titleGateWorkflow),
		PRURL:            "https://github.com/acme/repo/pull/42",
		PRCreatedThisRun: true,
	}
	want := "chore(#42): tighten release checks"
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prTitleViewCommand(repo.Dir, repo.PRURL), res: execx.Result{Stdout: `{"title":"ci(automation): tighten release checks"}`}},
		{
			cmd: prEditTitleCommand(repo.Dir, repo.PRURL, want),
			res: execx.Result{Stderr: "HTTP 403: You have exceeded a secondary rate limit."},
			err: errors.New("exit status 1"),
		},
		{cmd: prEditTitleCommand(repo.Dir, repo.PRURL, want), res: execx.Result{}},
	}}

	var logs []string
	h := New(fake)
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	h.Sleep = func(context.Context, time.Duration) error { return nil }

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, nil, execx.Result{}, "pr-title\tfail\t6s\t", nil)
	if err != nil || !repaired {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (true, nil)", repaired, err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "status=retry action=retitle reason=github_rate_limited") {
		t.Fatalf("missing rate-limit retry log: %v", logs)
	}
}

func TestRepairPRTitleGateFailureReusesPlannedRepair(t *testing.T) {
	t.Parallel()
	// The snapshot veto already resolved the rewrite from the live title, so
	// the repair edits straight away instead of spending a second gh pr view.
	repo := repoWorkspace{
		Dir:              writeTitleGateWorkflow(t, titleGateWorkflow),
		PRURL:            "https://github.com/acme/repo/pull/42",
		PRCreatedThisRun: true,
	}
	planned := prTitleGateRepair{
		Title: "chore(#42): tighten release checks",
		Gate:  prTitleGate{WorkflowPath: ".github/workflows/semantic-pr-title.yml"},
	}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prEditTitleCommand(repo.Dir, repo.PRURL, planned.Title), res: execx.Result{}},
	}}

	h := New(fake)
	h.Logf = func(string, ...any) {}

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, nil, execx.Result{}, "pr-title\tfail\t6s\t", &planned)
	if err != nil || !repaired {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (true, nil)", repaired, err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}
}

func TestRepairPRTitleGateFailureReportsForbiddenEdits(t *testing.T) {
	t.Parallel()
	repo := repoWorkspace{
		Dir:              writeTitleGateWorkflow(t, titleGateWorkflow),
		PRURL:            "https://github.com/acme/repo/pull/42",
		PRCreatedThisRun: true,
	}
	fake := &fakeRunner{t: t, exps: []expectedRun{
		{cmd: prTitleViewCommand(repo.Dir, repo.PRURL), res: execx.Result{Stdout: `{"title":"ci(automation): tighten release checks"}`}},
		{
			cmd: prEditTitleCommand(repo.Dir, repo.PRURL, "chore(#42): tighten release checks"),
			res: execx.Result{Stderr: "HTTP 403: Resource not accessible by integration"},
			err: errors.New("exit status 1"),
		},
	}}

	h := New(fake)
	h.Logf = func(string, ...any) {}
	h.Sleep = func(context.Context, time.Duration) error { return nil }

	repaired, err := h.repairPRTitleGateFailure(context.Background(), repo, nil, execx.Result{}, "pr-title\tfail\t6s\t", nil)
	if repaired || !errors.Is(err, errPRTitleGateNotPermitted) {
		t.Fatalf("repairPRTitleGateFailure = (%v, %v), want (false, errPRTitleGateNotPermitted)", repaired, err)
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
	if err := os.WriteFile(filepath.Join(workflowDir, "semantic-pr-title.yml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	// The gate is red while everything else is green, which is what the watch
	// prints before it exits nonzero.
	checksTable := "pr-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint"
	redChecks := execx.Result{Stdout: checksTable}
	checksJSON := `[{"name":"pr-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`

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
		{cmd: prChecksCommand(repoDir, prURL), res: execx.Result{Stdout: "pr-title\tpass\t6s\t\nLint & Format\tpass\t35s\t"}},
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

func TestRunStopsWithoutRemediationWhenTheRetitleFails(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	cfg.PRTitle = "ci(automation): harden CI validation checks"
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "titlegate0005"
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
	if err := os.WriteFile(filepath.Join(workflowDir, "semantic-pr-title.yml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	redChecks := execx.Result{Stdout: "pr-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\t"}
	checksJSON := `[{"name":"pr-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`

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
		{cmd: prEditTitleCommand(repoDir, prURL, repairedTitle), err: errors.New("exit status 1")},
	}}

	var logs []string
	h := New(fake)
	h.Now = func() time.Time { return now }
	h.Workspace = testWorkspaceManager(guid)
	h.TargetDirOK = func(path string) bool { return path == targetDir }
	h.Sleep = func(context.Context, time.Duration) error { return nil }
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	res := h.Run(context.Background(), cfg)
	if res.ExitCode != ExitPR || res.Err == nil {
		t.Fatalf("Run() = (exit %d, %v), want (%d, error)", res.ExitCode, res.Err, ExitPR)
	}
	if !strings.Contains(res.Err.Error(), "pull request title gate repair failed") {
		t.Fatalf("error = %v, want the title gate repair failure", res.Err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "stage=pr status=warn action=retitle reason=failed") {
		t.Fatalf("missing retitle failure log:\n%s", joined)
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
	if err := os.WriteFile(filepath.Join(workflowDir, "semantic-pr-title.yml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	checksTable := "pr-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint"
	redChecks := execx.Result{Stdout: checksTable}
	checksJSON := `[{"name":"pr-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`

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
		{cmd: prChecksCommand(repoDir, prURL), res: execx.Result{Stdout: "pr-title\tpass\t6s\t\nLint & Format\tpass\t35s\t"}},
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

func TestRunStopsWithoutRemediationWhenTheTitleGateRecheckBudgetRunsOut(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	cfg.PRTitle = "ci(automation): harden CI validation checks"
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "titlegate0008"
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
	if err := os.WriteFile(filepath.Join(workflowDir, "semantic-pr-title.yml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	// Only the gate is red, so no snapshot reconciliation runs between rounds.
	redChecks := execx.Result{Stdout: "pr-title\tfail\t6s\thttps://example.com/job"}

	exps := []expectedRun{
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
		{cmd: prTitleViewCommand(repoDir, prURL), res: execx.Result{Stdout: `{"title":"` + cfg.PRTitle + `"}`}},
		{cmd: prEditTitleCommand(repoDir, prURL, repairedTitle)},
	}
	// The regenerated gate run stays red through every recheck, plus the round
	// that finds the budget spent.
	for i := 0; i <= maxPRTitleGateRechecks; i++ {
		exps = append(exps, expectedRun{cmd: prChecksCommand(repoDir, prURL), res: redChecks, err: errors.New("exit status 1")})
	}
	fake := &fakeRunner{t: t, exps: exps}

	var logs []string
	h := New(fake)
	h.Now = func() time.Time { return now }
	h.Workspace = testWorkspaceManager(guid)
	h.TargetDirOK = func(path string) bool { return path == targetDir }
	h.Sleep = func(context.Context, time.Duration) error { return nil }
	h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	res := h.Run(context.Background(), cfg)
	if res.ExitCode != ExitPR || res.Err == nil {
		t.Fatalf("Run() = (exit %d, %v), want (%d, error)", res.ExitCode, res.Err, ExitPR)
	}
	if !strings.Contains(res.Err.Error(), "pull request title gate still failing") {
		t.Fatalf("error = %v, want the unresolved title gate failure", res.Err)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unused expectations: %+v", fake.exps)
	}

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "stage=checks status=failed reason=title_gate_unresolved") {
		t.Fatalf("missing unresolved title gate log:\n%s", joined)
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
	if err := os.WriteFile(filepath.Join(workflowDir, "semantic-pr-title.yml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	redChecks := execx.Result{Stdout: "pr-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint"}
	checksJSON := `[{"name":"pr-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`
	greenChecks := execx.Result{Stdout: "pr-title\tpass\t6s\t\nLint & Format\tpass\t35s\t"}

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
	if err := os.WriteFile(filepath.Join(workflowDir, "semantic-pr-title.yml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	// The watch times out with the red gate already streamed, while the
	// required-only snapshot is green because the gate is not a required check.
	timedOutChecks := execx.Result{Stdout: "pr-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint"}
	requiredJSON := `[{"name":"Lint & Format","bucket":"pass"}]`
	anyJSON := `[{"name":"pr-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`

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
		// The snapshot veto proves a repair exists before it rejects the green
		// required-only snapshot, and the repair reuses that plan instead of
		// looking the title up a second time.
		{cmd: prTitleViewCommand(repoDir, prURL), res: execx.Result{Stdout: `{"title":"` + cfg.PRTitle + `"}`}},
		{cmd: prChecksJSONCommand(repoDir, prURL, false), res: execx.Result{Stdout: anyJSON}},
		{cmd: prEditTitleCommand(repoDir, prURL, repairedTitle)},
		{cmd: prChecksCommand(repoDir, prURL), res: execx.Result{Stdout: "pr-title\tpass\t6s\t\nLint & Format\tpass\t35s\t"}},
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

func TestRunKeepsGreenChecksWhenTheTitleGateIsUnrepairable(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	cfg.PRTitle = "ci(automation): harden CI validation checks"
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "titlegate0006"
	runDir := testRunDir(guid)
	agentsPath := filepath.Join(runDir, "AGENTS.md")
	repoDir := filepath.Join(runDir, "repo")
	targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
	branch := "agent00-build-api"
	prURL := "https://github.com/acme/repo/pull/42"

	workflowDir := filepath.Join(repoDir, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatalf("create workflow dir: %v", err)
	}
	// The gate demands a scope from a literal allowlist, which the harness
	// never guesses, so no rewrite can clear it.
	if err := os.WriteFile(filepath.Join(workflowDir, "semantic-pr-title.yml"), []byte(titleGateLiteralScopeWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	timedOutChecks := execx.Result{Stdout: "pr-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\thttps://example.com/lint"}
	requiredJSON := `[{"name":"Lint & Format","bucket":"pass"}]`

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
		{cmd: prTitleViewCommand(repoDir, prURL), res: execx.Result{Stdout: `{"title":"` + cfg.PRTitle + `"}`}},
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
	if !strings.Contains(joined, "stage=checks status=ok reason=watch_timeout") {
		t.Fatalf("green required checks were not accepted:\n%s", joined)
	}
	if strings.Contains(joined, "reason=title_gate_failed") {
		t.Fatalf("an unrepairable title gate vetoed the green snapshot:\n%s", joined)
	}
	if strings.Contains(joined, "mode=remediation") {
		t.Fatalf("a remediation agent ran for a pull-request title gate:\n%s", joined)
	}
}

func TestRunKeepsGreenChecksWhenTheRetitleIsForbidden(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	cfg.PRTitle = "ci(automation): harden CI validation checks"
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "titlegate0007"
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
	if err := os.WriteFile(filepath.Join(workflowDir, "semantic-pr-title.yml"), []byte(titleGateWorkflow), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	redChecks := execx.Result{Stdout: "pr-title\tfail\t6s\thttps://example.com/job\nLint & Format\tpass\t35s\t"}
	checksJSON := `[{"name":"pr-title","bucket":"fail"},{"name":"Lint & Format","bucket":"pass"}]`

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
		{
			cmd: prEditTitleCommand(repoDir, prURL, repairedTitle),
			res: execx.Result{Stderr: "HTTP 403: Resource not accessible by integration"},
			err: errors.New("exit status 1"),
		},
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
	if !strings.Contains(joined, "stage=checks status=warn action=retitle reason=pr_title_edit_not_permitted") {
		t.Fatalf("missing the not-permitted escape log:\n%s", joined)
	}
	if strings.Contains(joined, "mode=remediation") {
		t.Fatalf("a remediation agent ran for a pull-request title gate:\n%s", joined)
	}
}

func TestIsPRTitleGateNotPermitted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		res  execx.Result
		err  error
		want bool
	}{
		{name: "no error"},
		{
			name: "bad credentials",
			res:  execx.Result{Stderr: "gh: Bad credentials (HTTP 401)"},
			err:  errors.New("exit status 1"),
			want: true,
		},
		{
			name: "integration without pull request write access",
			res:  execx.Result{Stderr: "HTTP 403: Resource not accessible by integration"},
			err:  errors.New("exit status 1"),
			want: true,
		},
		{
			name: "transient failure is not a permission problem",
			res:  execx.Result{Stderr: "HTTP 502: Bad gateway"},
			err:  errors.New("exit status 1"),
		},
		{
			name: "primary rate limit is throttling, not a denial",
			res:  execx.Result{Stderr: "HTTP 403: API rate limit exceeded for user ID 1 (https://api.github.com/repos/acme/repo/pulls/42)"},
			err:  errors.New("exit status 1"),
		},
		{
			name: "secondary rate limit is throttling, not a denial",
			res:  execx.Result{Stderr: "HTTP 403: You have exceeded a secondary rate limit. Please wait a few minutes before you try again."},
			err:  errors.New("exit status 1"),
		},
		{
			name: "a 403 reported only by the wrapped error never counts",
			// The wrapped error repeats the gh arguments, title included.
			err: errors.New(`run gh [pr edit 42 --title chore(#42): surface HTTP 403 responses]: exit status 1`),
		},
		{
			name: "a title word never counts as a denial",
			// The wrapped error repeats the gh arguments, title included.
			err: errors.New(`run gh [pr edit 42 --title chore(#42): reject forbidden imports]: exit status 1`),
		},
		{
			name: "an auth phrase reported only by the wrapped error never counts",
			// The wrapped error repeats the gh arguments, title included.
			err: errors.New(`run gh [pr edit 42 --title chore(#42): retry on bad credentials and HTTP 401]: exit status 1`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isPRTitleGateNotPermitted(tc.res, tc.err); got != tc.want {
				t.Fatalf("isPRTitleGateNotPermitted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsPRTitleGateCheckName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"title-check":                          true,
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
