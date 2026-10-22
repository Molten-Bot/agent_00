package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/web"
)

func TestRunUsageMissingSubcommand(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = []string{"harness"}

	if code := run(); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
}

func TestRunUsageMissingConfigFlag(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = []string{"harness", "run"}

	if code := run(); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
}

func TestRunConfigLoadFailure(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })

	missing := filepath.Join(t.TempDir(), "missing.json")
	os.Args = []string{"harness", "run", "--config", missing}

	if code := run(); code != 10 {
		t.Fatalf("run() = %d, want 10", code)
	}
}

func TestRunUsageUnknownSubcommand(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = []string{"harness", "unknown"}

	if code := run(); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
}

func TestRunMultiplexUsageMissingConfigFlag(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = []string{"harness", "multiplex"}

	if code := run(); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
}

func TestLoadServerBootConfigWithInitUsesRuntimeConfigLogLevelWhenPresent(t *testing.T) {
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")

	tempDir := t.TempDir()
	initPath := filepath.Join(tempDir, "init.json")
	runtimeConfigPath := filepath.Join(tempDir, "config.json")

	if err := os.WriteFile(initPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write init config: %v", err)
	}
	if err := os.WriteFile(runtimeConfigPath, []byte(`{"log_level":"debug"}`), 0o600); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}

	cfg, exitCode, err := loadServerBootConfig(initPath, "")
	if err != nil {
		t.Fatalf("loadServerBootConfig() error = %v", err)
	}
	if exitCode != app.ExitSuccess {
		t.Fatalf("loadServerBootConfig() exitCode = %d, want %d", exitCode, app.ExitSuccess)
	}
	if got, want := cfg.LogLevel, localruntime.LogLevelDebug.String(); got != want {
		t.Fatalf("LogLevel = %q, want %q", got, want)
	}
}

func TestLoadServerBootConfigWithInitBackfillsMissingRuntimeConfigLogLevel(t *testing.T) {
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")

	tempDir := t.TempDir()
	initPath := filepath.Join(tempDir, "init.json")
	runtimeConfigPath := filepath.Join(tempDir, "config.json")

	if err := os.WriteFile(initPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write init config: %v", err)
	}
	if err := os.WriteFile(runtimeConfigPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}

	cfg, exitCode, err := loadServerBootConfig(initPath, "")
	if err != nil {
		t.Fatalf("loadServerBootConfig() error = %v", err)
	}
	if exitCode != app.ExitSuccess {
		t.Fatalf("loadServerBootConfig() exitCode = %d, want %d", exitCode, app.ExitSuccess)
	}
	if got, want := cfg.LogLevel, localruntime.DefaultLogLevel; got != want {
		t.Fatalf("LogLevel = %q, want %q", got, want)
	}

	data, err := os.ReadFile(runtimeConfigPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got, want := doc["log_level"], any(localruntime.DefaultLogLevel); got != want {
		t.Fatalf("config.json log_level = %#v, want %q", got, want)
	}
}

func TestLoadServerBootConfigAcceptsMalformedDockerComposeGitHubEnv(t *testing.T) {
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN:github_token_env_token", "")

	configPath := filepath.Join(t.TempDir(), "missing.json")
	cfg, exitCode, err := loadServerBootConfig("", configPath)
	if err != nil {
		t.Fatalf("loadServerBootConfig() error = %v", err)
	}
	if exitCode != app.ExitSuccess {
		t.Fatalf("loadServerBootConfig() exitCode = %d, want %d", exitCode, app.ExitSuccess)
	}
	if got, want := cfg.GitHubToken, "github_token_env_token"; got != want {
		t.Fatalf("GitHubToken = %q, want %q", got, want)
	}
}

func TestLoadServerBootConfigRejectsBothInitAndConfig(t *testing.T) {
	t.Parallel()

	_, exitCode, err := loadServerBootConfig("init.json", "config.json")
	if err == nil || !strings.Contains(err.Error(), "provide only one of --init or --config") {
		t.Fatalf("loadServerBootConfig(both) error = %v, want mutually exclusive error", err)
	}
	if exitCode != app.ExitUsage {
		t.Fatalf("loadServerBootConfig(both) exitCode = %d, want %d", exitCode, app.ExitUsage)
	}
}

func TestLoadServerBootConfigWithMissingInitFallsBackToExistingRuntimeConfig(t *testing.T) {
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	tempDir := t.TempDir()
	initPath := filepath.Join(tempDir, "init.json")
	runtimeConfigPath := filepath.Join(tempDir, "config.json")
	if err := os.WriteFile(runtimeConfigPath, []byte(`{"agent_harness":"claude"}`), 0o600); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}

	cfg, exitCode, err := loadServerBootConfig(initPath, "")
	if err != nil {
		t.Fatalf("loadServerBootConfig() error = %v", err)
	}
	if exitCode != app.ExitSuccess {
		t.Fatalf("loadServerBootConfig() exitCode = %d, want %d", exitCode, app.ExitSuccess)
	}
	if got, want := cfg.AgentHarness, "claude"; got != want {
		t.Fatalf("AgentHarness = %q, want %q", got, want)
	}
}

func TestLoadServerBootConfigWithMissingInitAndMissingRuntimeConfigUsesDefault(t *testing.T) {
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	tempDir := t.TempDir()
	initPath := filepath.Join(tempDir, "init.json")

	cfg, exitCode, err := loadServerBootConfig(initPath, "")
	if err != nil {
		t.Fatalf("loadServerBootConfig() error = %v", err)
	}
	if exitCode != app.ExitSuccess {
		t.Fatalf("loadServerBootConfig() exitCode = %d, want %d", exitCode, app.ExitSuccess)
	}
	if got, want := cfg.RuntimeConfigPath, filepath.Join(tempDir, "config.json"); got != want {
		t.Fatalf("RuntimeConfigPath = %q, want %q", got, want)
	}
}

func TestLoadServerBootConfigWithInvalidInitJSONReturnsConfigError(t *testing.T) {
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")

	initPath := filepath.Join(t.TempDir(), "init.json")
	if err := os.WriteFile(initPath, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("write init config: %v", err)
	}

	_, exitCode, err := loadServerBootConfig(initPath, "")
	if err == nil || !strings.Contains(err.Error(), "init config error") {
		t.Fatalf("loadServerBootConfig(invalid init) error = %v, want init config error", err)
	}
	if exitCode != app.ExitConfig {
		t.Fatalf("loadServerBootConfig(invalid init) exitCode = %d, want %d", exitCode, app.ExitConfig)
	}
}

func TestLoadServerBootConfigDefaultsWhenNoInitOrConfigProvided(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", envPath)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	cfg, exitCode, err := loadServerBootConfig("", "")
	if err != nil {
		t.Fatalf("loadServerBootConfig() error = %v", err)
	}
	if exitCode != app.ExitSuccess {
		t.Fatalf("loadServerBootConfig() exitCode = %d, want %d", exitCode, app.ExitSuccess)
	}
	if got, want := cfg.RuntimeConfigPath, envPath; got != want {
		t.Fatalf("RuntimeConfigPath = %q, want %q", got, want)
	}
}

func TestLoadServerBootRuntimeConfigOrDefaultPropagatesNonNotExistError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}

	_, exitCode, err := loadServerBootRuntimeConfigOrDefault(path)
	if err == nil || !strings.Contains(err.Error(), "parse runtime config") {
		t.Fatalf("loadServerBootRuntimeConfigOrDefault(malformed) error = %v, want parse error", err)
	}
	if exitCode != app.ExitConfig {
		t.Fatalf("loadServerBootRuntimeConfigOrDefault(malformed) exitCode = %d, want %d", exitCode, app.ExitConfig)
	}
}

func TestLoadServerBootRuntimeConfigOrDefaultLoadsExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agent_harness":"claude"}`), 0o600); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}

	cfg, exitCode, err := loadServerBootRuntimeConfigOrDefault(path)
	if err != nil {
		t.Fatalf("loadServerBootRuntimeConfigOrDefault() error = %v", err)
	}
	if exitCode != app.ExitSuccess {
		t.Fatalf("loadServerBootRuntimeConfigOrDefault() exitCode = %d, want %d", exitCode, app.ExitSuccess)
	}
	if got, want := cfg.AgentHarness, "claude"; got != want {
		t.Fatalf("AgentHarness = %q, want %q", got, want)
	}
	if got, want := cfg.RuntimeConfigPath, path; got != want {
		t.Fatalf("RuntimeConfigPath = %q, want %q", got, want)
	}
}

func TestLoadServerBootRuntimeConfigOrDefaultResolvesEmptyPathUsingDefault(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", envPath)

	cfg, exitCode, err := loadServerBootRuntimeConfigOrDefault("")
	if err != nil {
		t.Fatalf("loadServerBootRuntimeConfigOrDefault() error = %v", err)
	}
	if exitCode != app.ExitSuccess {
		t.Fatalf("loadServerBootRuntimeConfigOrDefault() exitCode = %d, want %d", exitCode, app.ExitSuccess)
	}
	if got, want := cfg.RuntimeConfigPath, envPath; got != want {
		t.Fatalf("RuntimeConfigPath = %q, want %q", got, want)
	}
}

func TestFinalizeServerBootConfigPassesThroughError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	cfg, exitCode, err := finalizeServerBootConfig(localruntime.InitConfig{AgentHarness: "claude"}, app.ExitUsage, wantErr)
	if err != wantErr {
		t.Fatalf("finalizeServerBootConfig() error = %v, want %v", err, wantErr)
	}
	if exitCode != app.ExitUsage {
		t.Fatalf("finalizeServerBootConfig() exitCode = %d, want %d", exitCode, app.ExitUsage)
	}
	if cfg.AgentHarness != "claude" {
		t.Fatalf("finalizeServerBootConfig() cfg = %+v, want unchanged input cfg", cfg)
	}
}

func TestFinalizeServerBootConfigReportsSaveTokenFailure(t *testing.T) {
	t.Setenv("GH_TOKEN", "github_token_from_env")
	t.Setenv("GITHUB_TOKEN", "")

	blockerFile := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocker) error = %v", err)
	}
	blockedPath := filepath.Join(blockerFile, "config.json")

	_, exitCode, err := finalizeServerBootConfig(localruntime.InitConfig{RuntimeConfigPath: blockedPath}, app.ExitSuccess, nil)
	if err == nil {
		t.Fatal("finalizeServerBootConfig() error = nil, want save token failure")
	}
	if exitCode != app.ExitConfig {
		t.Fatalf("finalizeServerBootConfig() exitCode = %d, want %d", exitCode, app.ExitConfig)
	}
}

func TestFinalizeServerBootConfigReportsValidateFailure(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	_, exitCode, err := finalizeServerBootConfig(localruntime.InitConfig{Version: "v2"}, app.ExitSuccess, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("finalizeServerBootConfig(invalid version) error = %v, want unsupported version error", err)
	}
	if exitCode != app.ExitConfig {
		t.Fatalf("finalizeServerBootConfig(invalid version) exitCode = %d, want %d", exitCode, app.ExitConfig)
	}
}

func TestEffectiveRuntimeConfigKeepsBootstrapGitHubToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".agent00", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{"agent_harness":"codex"}`), 0o600); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}

	cfg, err := effectiveRuntimeConfig(localruntime.InitConfig{
		RuntimeConfigPath: configPath,
		GitHubToken:       "github_token_env_token",
	})
	if err != nil {
		t.Fatalf("effectiveRuntimeConfig() error = %v", err)
	}
	if got, want := cfg.GitHubToken, "github_token_env_token"; got != want {
		t.Fatalf("GitHubToken = %q, want %q", got, want)
	}
}

func TestMonitorURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: ""},
		{in: ":7777", want: "http://127.0.0.1:7777"},
		{in: "127.0.0.1:7777", want: "http://127.0.0.1:7777"},
		{in: "http://localhost:8080", want: "http://localhost:8080"},
	}

	for _, tt := range tests {
		if got := monitorURL(tt.in); got != tt.want {
			t.Fatalf("monitorURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCollectConfigPathsFilesAndDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.json")
	fileB := filepath.Join(dir, "b.JSON")
	fileTxt := filepath.Join(dir, "notes.txt")
	nestedDir := filepath.Join(dir, "nested")
	fileC := filepath.Join(nestedDir, "c.json")

	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	for _, path := range []string{fileA, fileB, fileTxt, fileC} {
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	got, err := collectConfigPaths([]string{fileA, dir, fileA})
	if err != nil {
		t.Fatalf("collectConfigPaths() error = %v", err)
	}

	want := []string{fileA, fileB, fileC}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collectConfigPaths() = %v, want %v", got, want)
	}
}

func TestLocalTaskLogDir(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), ".log")
	got, ok := localTaskLogDir(root, "local-1712345678-000321")
	if !ok {
		t.Fatal("localTaskLogDir() ok = false, want true")
	}
	want := filepath.Join(root, "local", "1712345678", "000321")
	if got != want {
		t.Fatalf("localTaskLogDir() = %q, want %q", got, want)
	}

	if _, ok := localTaskLogDir(root, "req-abc"); ok {
		t.Fatal("localTaskLogDir(non-local request) ok = true, want false")
	}
}

func TestFailureFollowUpRunConfigIncludesNonLocalRequestLogPaths(t *testing.T) {
	t.Parallel()

	logRoot := filepath.Join(t.TempDir(), ".log")
	failedResult := app.Result{Err: fmt.Errorf("checks failed")}
	failedRunCfg := config.Config{Repos: []string{"git@github.com:acme/repo.git"}}

	cfg := failureFollowUpRunConfig("req-123-abc", failedResult, failedRunCfg, logRoot)
	expectedLogDir := filepath.Join(logRoot, "req", "123", "abc")
	if !strings.Contains(cfg.Prompt, expectedLogDir) {
		t.Fatalf("Prompt missing non-local log dir path %q: %q", expectedLogDir, cfg.Prompt)
	}
	if !strings.Contains(cfg.Prompt, filepath.Join(expectedLogDir, logFileName)) {
		t.Fatalf("Prompt missing non-local log file path: %q", cfg.Prompt)
	}
}

func TestFailureFollowUpRunConfigUsesRequiredPayloadShapeAndLogContext(t *testing.T) {
	t.Parallel()

	logRoot := filepath.Join(t.TempDir(), ".log")
	logDir := filepath.Join(logRoot, "local", "1712345678", "000001")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	logPath := filepath.Join(logDir, logFileName)
	if err := os.WriteFile(logPath, []byte("stage=checks status=failed\ncmd phase=checks text=\"go test failed\"\n"), 0o644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	failedResult := app.Result{
		Err:          fmt.Errorf("clone: repository not found"),
		WorkspaceDir: "/tmp/run-123",
	}
	failedRunCfg := config.Config{
		Repos: []string{
			"git@github.com:acme/repo-a.git",
			"git@github.com:acme/repo-b.git",
			"git@github.com:acme/repo-a.git",
		},
	}

	cfg := failureFollowUpRunConfig("local-1712345678-000001", failedResult, failedRunCfg, logRoot)
	if got, want := cfg.Repos, []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Repos = %v, want %v", got, want)
	}
	if cfg.BaseBranch != "" {
		t.Fatalf("BaseBranch = %q, want empty for remote default", cfg.BaseBranch)
	}
	if cfg.TargetSubdir != "." {
		t.Fatalf("TargetSubdir = %q, want %q", cfg.TargetSubdir, ".")
	}

	expectedLogDir := filepath.Join(logRoot, followUpTaskLogArchiveSubdir, "local", "1712345678", "000001")
	if !strings.Contains(cfg.Prompt, expectedLogDir) {
		t.Fatalf("Prompt missing log dir path %q: %q", expectedLogDir, cfg.Prompt)
	}
	if !strings.Contains(cfg.Prompt, filepath.Join(expectedLogDir, logFileName)) {
		t.Fatalf("Prompt missing log file path: %q", cfg.Prompt)
	}
	if !strings.Contains(cfg.Prompt, failureFollowUpRequiredPrompt) {
		t.Fatalf("Prompt missing required instruction: %q", cfg.Prompt)
	}
}

func TestFailureFollowUpRunConfigForcesRemoteDefaultBranchAndRootTarget(t *testing.T) {
	t.Parallel()

	logRoot := filepath.Join(t.TempDir(), ".log")
	failedResult := app.Result{
		Err:    fmt.Errorf("git: nothing to commit, working tree clean"),
		Branch: "agent00-add-slight-padding-between-prompt-and-lo",
	}
	failedRunCfg := config.Config{
		Repos:        []string{"git@github.com:acme/repo.git"},
		BaseBranch:   "agent00-add-slight-padding-between-prompt-and-lo",
		TargetSubdir: "internal/web",
		Prompt:       "add slight padding between prompt and lower items",
	}

	cfg := failureFollowUpRunConfig("local-1712345678-000001", failedResult, failedRunCfg, logRoot)
	if cfg.BaseBranch != "" {
		t.Fatalf("BaseBranch = %q, want empty for remote default", cfg.BaseBranch)
	}
	if cfg.TargetSubdir != "." {
		t.Fatalf("TargetSubdir = %q, want %q", cfg.TargetSubdir, ".")
	}
	for _, want := range []string{
		"- target_subdir=internal/web",
		"Original task prompt:",
		"add slight padding between prompt and lower items",
	} {
		if !strings.Contains(cfg.Prompt, want) {
			t.Fatalf("Prompt missing %q: %q", want, cfg.Prompt)
		}
	}
}

func TestFailureFollowUpReposAlwaysUsesHarnessRepositoryWhenResultHasSingleRepo(t *testing.T) {
	t.Parallel()

	failedResult := app.Result{
		RepoResults: []app.RepoResult{
			{RepoURL: "git@github.com:acme/failed-result.git"},
			{RepoURL: "git@github.com:acme/failed-result.git"},
		},
	}
	failedRunCfg := config.Config{
		Repos: []string{
			"git@github.com:acme/from-config.git",
		},
	}
	if got, want := failureFollowUpRepos(failedResult, failedRunCfg), []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failureFollowUpRepos() = %v, want %v", got, want)
	}
}

func TestFailureFollowUpReposAlwaysUsesHarnessRepositoryWhenResultAndConfigDiffer(t *testing.T) {
	t.Parallel()

	failedResult := app.Result{
		RepoResults: []app.RepoResult{
			{RepoURL: "git@github.com:acme/from-result.git"},
			{RepoURL: "git@github.com:acme/from-result.git"},
		},
	}
	failedRunCfg := config.Config{
		Repos: []string{
			"git@github.com:acme/from-config.git",
			"git@github.com:acme/other-config.git",
		},
	}
	if got, want := failureFollowUpRepos(failedResult, failedRunCfg), []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failureFollowUpRepos() = %v, want %v", got, want)
	}
}

func TestFailureFollowUpReposAlwaysUsesHarnessRepositoryWhenOnlyResultRepoExists(t *testing.T) {
	t.Parallel()

	failedResult := app.Result{
		RepoResults: []app.RepoResult{
			{RepoURL: "git@github.com:acme/from-result.git"},
		},
	}
	if got, want := failureFollowUpRepos(failedResult, config.Config{}), []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failureFollowUpRepos() = %v, want %v", got, want)
	}
}

func TestFailureFollowUpReposAlwaysUsesHarnessRepositoryWhenResultIsAmbiguous(t *testing.T) {
	t.Parallel()

	failedResult := app.Result{
		RepoResults: []app.RepoResult{
			{RepoURL: "git@github.com:acme/repo-a.git"},
			{RepoURL: "git@github.com:acme/repo-b.git"},
		},
	}
	failedRunCfg := config.Config{
		Repos: []string{
			"git@github.com:acme/from-config.git",
			"git@github.com:acme/other-config.git",
		},
	}

	if got, want := failureFollowUpRepos(failedResult, failedRunCfg), []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failureFollowUpRepos() = %v, want %v", got, want)
	}
}

func TestFailureFollowUpReposUsesHarnessRepositoryWhenNoRepoFound(t *testing.T) {
	t.Parallel()

	if got, want := failureFollowUpRepos(app.Result{}, config.Config{}), []string{config.DefaultRepositoryURL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failureFollowUpRepos() = %v, want %v", got, want)
	}
}

func TestDiagnosticDetailForResult(t *testing.T) {
	t.Parallel()

	if got := diagnosticDetailForResult(execx.Result{Stdout: "\n\n git version 2.47.1 \n"}); got != "git version 2.47.1" {
		t.Fatalf("diagnosticDetailForResult(stdout) = %q", got)
	}
	if got := diagnosticDetailForResult(execx.Result{Stderr: "not logged in\n"}); got != "not logged in" {
		t.Fatalf("diagnosticDetailForResult(stderr) = %q", got)
	}
	if got := diagnosticDetailForResult(execx.Result{}); got != "check completed" {
		t.Fatalf("diagnosticDetailForResult(empty) = %q", got)
	}
}

type stubExecResult struct {
	result execx.Result
	err    error
}

type stubExecRunner struct {
	results map[string]stubExecResult
}

func (r *stubExecRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	if r == nil {
		return execx.Result{}, errors.New("nil runner")
	}
	entry, ok := r.results[stubCommandKey(cmd)]
	if !ok {
		return execx.Result{}, fmt.Errorf("unexpected command: %s", stubCommandKey(cmd))
	}
	return entry.result, entry.err
}

func stubCommandKey(cmd execx.Command) string {
	return fmt.Sprintf("%s %s", cmd.Name, strings.Join(cmd.Args, " "))
}

func assertLogContains(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, want) {
			return
		}
	}
	t.Fatalf("logs missing %q\nlogs:\n%s", want, strings.Join(lines, "\n"))
}

func TestConfigureReviewSettingsPersistsOnlyNonDefaults(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	state, err := configureReviewSettings(localruntime.InitConfig{
		RuntimeConfigPath: configPath,

		AgentHarness: "codex",
	}, web.ReviewSettingsRequest{
		AutoMerge:            true,
		DeleteMergedBranches: true,
		MergeMethod:          "merge",
		ReviewLevel:          "medium",
	})
	if err != nil {
		t.Fatalf("configureReviewSettings() error = %v", err)
	}
	if !state.AutoMerge || !state.DeleteMergedBranches || state.MergeMethod != "merge" || state.ReviewLevel != "medium" {
		t.Fatalf("state = %#v, want auto merge, delete merged branches, and merge method", state)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	reviewDoc, ok := doc["review_watch"].(map[string]any)
	if !ok {
		t.Fatalf("review_watch = %#v, want object", doc["review_watch"])
	}
	if got, ok := reviewDoc["auto_merge"].(bool); !ok || !got {
		t.Fatalf("review_watch.auto_merge = %#v, want true", reviewDoc["auto_merge"])
	}
	if got, ok := reviewDoc["delete_merged_branches"].(bool); !ok || !got {
		t.Fatalf("review_watch.delete_merged_branches = %#v, want true", reviewDoc["delete_merged_branches"])
	}
	if got := strings.TrimSpace(reviewDoc["merge_method"].(string)); got != "merge" {
		t.Fatalf("review_watch.merge_method = %q, want merge", got)
	}
	if got := strings.TrimSpace(reviewDoc["review_level"].(string)); got != "medium" {
		t.Fatalf("review_watch.review_level = %q, want medium", got)
	}

	state, err = configureReviewSettings(localruntime.InitConfig{
		RuntimeConfigPath: configPath,

		AgentHarness: "codex",
	}, web.ReviewSettingsRequest{
		AutoMerge:            false,
		DeleteMergedBranches: false,
		MergeMethod:          "squash",
		ReviewLevel:          "off",
	})
	if err != nil {
		t.Fatalf("configureReviewSettings(defaults) error = %v", err)
	}
	if state.AutoMerge || state.DeleteMergedBranches || state.MergeMethod != "squash" || state.ReviewLevel != "off" {
		t.Fatalf("default state = %#v", state)
	}
	data, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read default config: %v", err)
	}
	doc = map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse default config: %v", err)
	}
	if _, ok := doc["review_watch"]; ok {
		t.Fatalf("review_watch persisted for defaults: %#v", doc["review_watch"])
	}
}

func TestConfigureReviewSettingsRejectsInvalidMergeMethod(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	_, err := configureReviewSettings(localruntime.InitConfig{
		RuntimeConfigPath: configPath,

		AgentHarness: "codex",
	}, web.ReviewSettingsRequest{
		AutoMerge:   true,
		MergeMethod: "octopus",
	})
	if err == nil {
		t.Fatal("configureReviewSettings() error = nil, want non-nil")
	}
	if got := err.Error(); !strings.Contains(got, "merge, squash, or rebase") {
		t.Fatalf("configureReviewSettings() error = %q", got)
	}
}

func TestConfigureReviewSettingsRejectsInvalidReviewLevel(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	_, err := configureReviewSettings(localruntime.InitConfig{
		RuntimeConfigPath: configPath,

		AgentHarness: "codex",
	}, web.ReviewSettingsRequest{
		MergeMethod: "squash",
		ReviewLevel: "extreme",
	})
	if err == nil {
		t.Fatal("configureReviewSettings() error = nil, want non-nil")
	}
	if got := err.Error(); !strings.Contains(got, "off, low, medium, or high") {
		t.Fatalf("configureReviewSettings() error = %q", got)
	}
}
