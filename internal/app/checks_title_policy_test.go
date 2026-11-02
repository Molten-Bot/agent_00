package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestIsPullRequestTitleCheckName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		check string
		want  bool
	}{
		{"github check-title job", "check-title", true},
		{"spaced pr title job", "PR Title", true},
		{"lint pr title job", "lint-pr-title", true},
		{"plural titles job", "Conventional Titles", true},
		{"semantic pull request action", "amannn/action-semantic-pull-request", true},
		{"semantic pr shorthand", "semantic-pr", true},
		{"unit tests", "Test", false},
		{"type check", "Type Check", false},
		{"substring is not a word", "Entitlements check", false},
		{"empty", "   ", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isPullRequestTitleCheckName(tc.check); got != tc.want {
				t.Fatalf("isPullRequestTitleCheckName(%q) = %v, want %v", tc.check, got, tc.want)
			}
		})
	}
}

func TestFailingCheckNamesUsesLatestReportedState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		summary string
		want    []string
	}{
		{
			name:    "no tabular output",
			summary: "No check output was provided by gh.",
		},
		{
			name:    "all passing",
			summary: "Test\tpass\nType Check\tpass",
		},
		{
			name: "stale failure superseded by pass",
			summary: strings.Join([]string{
				"Refreshing checks status every 10 seconds. Press Ctrl+C to quit.",
				"Build and test\tfail\t23s\thttps://github.com/acme/repo/actions/runs/1/job/11",
				"Build and test\tpass\t22s\thttps://github.com/acme/repo/actions/runs/2/job/22",
			}, "\n"),
		},
		{
			name: "mixed buckets report only failures",
			summary: strings.Join([]string{
				"AI PR QA\tskipping",
				"Test\tpass",
				"UBE Docker Build\tskipping",
				"check-title\tfail",
			}, "\n"),
			want: []string{"check-title"},
		},
		{
			name:    "cancelled counts as failure",
			summary: "Deploy\tcancelled\t0\thttps://example.com/run",
			want:    []string{"Deploy"},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := failingCheckNames(tc.summary); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("failingCheckNames() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestPullRequestTitleCheckFailuresOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		summary string
		want    []string
	}{
		{
			name:    "no failures",
			summary: "Test\tpass\ncheck-title\tpass",
		},
		{
			name:    "title only failure",
			summary: "Test\tpass\ncheck-title\tfail",
			want:    []string{"check-title"},
		},
		{
			name:    "title failure alongside repository failure",
			summary: "Test\tfail\ncheck-title\tfail",
		},
		{
			name:    "repository failure only",
			summary: "Test\tfail",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pullRequestTitleCheckFailuresOnly(tc.summary); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("pullRequestTitleCheckFailuresOnly() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestFailingChecksLogField(t *testing.T) {
	t.Parallel()

	if got := failingChecksLogField("Test\tpass"); got != "" {
		t.Fatalf("failingChecksLogField(passing) = %q, want empty", got)
	}
	if got, want := failingChecksLogField("Test\tfail\ncheck-title\tfail"), ` failing="Test, check-title"`; got != want {
		t.Fatalf("failingChecksLogField() = %q, want %q", got, want)
	}
}

// A title-policy check reads pull-request metadata, so the remediation loop
// can never fix it from the worktree. The run must stop on the first failed
// checks round instead of spending every remediation attempt on an agent that
// has nothing to change.
func TestRunTitleOnlyCheckFailureStopsBeforeRemediation(t *testing.T) {
	t.Parallel()

	cfg := sampleConfig()
	now := time.Date(2026, 4, 2, 15, 4, 5, 0, time.UTC)
	guid := "abcdef123456"
	runDir := testRunDir(guid)
	agentsPath := filepath.Join(runDir, "AGENTS.md")
	repoDir := filepath.Join(runDir, "repo")
	targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
	branch := "agent00-build-api"
	prURL := "https://github.com/acme/repo/pull/42"
	checkOutput := "check-title\tfail\t6s\thttps://github.com/acme/repo/actions/runs/1/job/11"

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
		{cmd: prChecksCommand(repoDir, prURL), res: execx.Result{Stdout: checkOutput + "\n"}, err: errors.New("checks failed")},
	}}

	var logs []string
	h := New(fake)
	h.Now = func() time.Time { return now }
	h.Workspace = testWorkspaceManager(guid)
	h.TargetDirOK = func(path string) bool { return path == targetDir }
	h.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	res := h.Run(context.Background(), cfg)
	if res.Err == nil {
		t.Fatal("Run() err = nil, want pull request title policy failure")
	}
	if res.ExitCode != ExitPR {
		t.Fatalf("ExitCode = %d, want %d", res.ExitCode, ExitPR)
	}
	if !strings.Contains(res.Err.Error(), "check-title") ||
		!strings.Contains(res.Err.Error(), "update the pull request title") {
		t.Fatalf("Run() err = %v, want actionable title-policy detail", res.Err)
	}
	if res.PRURL != prURL {
		t.Fatalf("PRURL = %q, want %q", res.PRURL, prURL)
	}
	if len(fake.exps) != 0 {
		t.Fatalf("unconsumed expectations: %d", len(fake.exps))
	}

	for _, call := range fake.calls {
		if agentPromptIsRemediation(call, 1) {
			t.Fatal("remediation agent was invoked for a title-only check failure")
		}
	}

	joinedLogs := strings.Join(logs, "\n")
	if !strings.Contains(joinedLogs, `failing="check-title"`) {
		t.Fatalf("logs missing failing check names:\n%s", joinedLogs)
	}
	if !strings.Contains(joinedLogs, "stage=checks status=error reason=pull_request_title_policy") {
		t.Fatalf("logs missing title policy reason:\n%s", joinedLogs)
	}
}
