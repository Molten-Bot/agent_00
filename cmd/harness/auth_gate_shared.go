package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/stringsx"
	"github.com/Molten-Bot/agent_00/internal/web"
)

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	return ansiEscapePattern.ReplaceAllString(text, "")
}

// scanProcessLines reads r line by line, closing it once the scan ends, and
// calls ingest for each line. It is shared by the auth gates' device-auth and
// login output readers, which need the same oversized line buffer to handle
// long single-line JSON status payloads some agent CLIs emit.
func scanProcessLines(r io.ReadCloser, ingest func(string)) {
	defer r.Close()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		ingest(scanner.Text())
	}
}

const githubTokenPasteConfigureMessage = "GitHub token is required."
const githubTokenValidationTimeout = 12 * time.Second
const githubStarTimeout = 12 * time.Second
const agent00Repository = config.DefaultRepositoryOwner + "/" + config.DefaultRepositoryName
const agent00StarPath = "/user/starred/" + agent00Repository

var githubTokenValidationMu sync.Mutex
var githubStarAPIBaseURL = "https://api.github.com"
var githubStarHTTPClient = http.DefaultClient

func readyAgentAuthState() web.AgentAuthState {
	return web.AgentAuthState{
		Required: false,
		Ready:    true,
		State:    "ready",
		Message:  "Agent auth is ready.",
	}
}

func githubTokenNeedsConfigureState(harness, message string) web.AgentAuthState {
	message = stringsx.FirstNonEmpty(
		message,
		"GitHub token is required.",
	)
	return web.AgentAuthState{
		Harness:              harness,
		Required:             true,
		Ready:                false,
		State:                "needs_configure",
		Message:              message,
		ConfigureCommand:     claudeGitHubConfigureCommand,
		ConfigurePlaceholder: claudeGitHubConfigurePlaceholder,
		UpdatedAt:            time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func firstConfiguredGitHubToken(runtimeConfigPath string, initCfg localruntime.InitConfig) (value string, source string) {
	if env := configuredGitHubTokenFromEnv(); env != "" {
		return env, "environment"
	}
	if persisted := localruntime.ReadRuntimeConfigString(runtimeConfigPath, "github_token", "githubToken", "GITHUB_TOKEN"); persisted != "" {
		return persisted, "runtime config"
	}
	if init := strings.TrimSpace(initCfg.GitHubToken); init != "" {
		return init, "init config"
	}
	return "", ""
}

type configuredGitHubToken struct {
	value  string
	source string
}

func configuredGitHubTokens(runtimeConfigPath string, initCfg localruntime.InitConfig) []configuredGitHubToken {
	candidates := []configuredGitHubToken{
		{value: strings.TrimSpace(os.Getenv("GH_TOKEN")), source: "environment"},
		{value: strings.TrimSpace(os.Getenv("GITHUB_TOKEN")), source: "environment"},
		{value: malformedDockerComposeGitHubTokenFromEnv(), source: "environment"},
		{value: localruntime.ReadRuntimeConfigString(runtimeConfigPath, "github_token", "githubToken", "GITHUB_TOKEN"), source: "runtime config"},
		{value: strings.TrimSpace(initCfg.GitHubToken), source: "init config"},
	}

	seen := make(map[string]struct{}, len(candidates))
	filtered := make([]configuredGitHubToken, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.value = strings.TrimSpace(candidate.value)
		if candidate.value == "" {
			continue
		}
		if _, ok := seen[candidate.value]; ok {
			continue
		}
		seen[candidate.value] = struct{}{}
		filtered = append(filtered, candidate)
	}
	return filtered
}

func validatedGitHubToken(ctx context.Context, runner execx.Runner, runtimeConfigPath string, initCfg localruntime.InitConfig) (string, error) {
	candidates := configuredGitHubTokens(runtimeConfigPath, initCfg)
	if len(candidates) == 0 {
		return "", fmt.Errorf("github token is required")
	}

	var validationErrs []string
	for _, candidate := range candidates {
		if err := validateGitHubToken(ctx, runner, candidate.value); err != nil {
			validationErrs = append(validationErrs, fmt.Sprintf("%s GitHub token is invalid: %v", candidate.source, err))
			continue
		}
		return candidate.value, nil
	}

	return "", errors.New(strings.Join(validationErrs, "; "))
}

func setGitHubTokenEnvironment(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("github token is required")
	}
	if err := os.Setenv("GITHUB_TOKEN", token); err != nil {
		return err
	}
	if err := os.Setenv("GH_TOKEN", token); err != nil {
		return err
	}
	return nil
}

func githubTokenRequirementState(ctx context.Context, runner execx.Runner, harness, runtimeConfigPath string, initCfg localruntime.InitConfig) (bool, web.AgentAuthState) {
	_, state, blocked := githubTokenRequirement(ctx, runner, harness, runtimeConfigPath, initCfg)
	return blocked, state
}

func githubTokenRequirement(ctx context.Context, runner execx.Runner, harness, runtimeConfigPath string, initCfg localruntime.InitConfig) (string, web.AgentAuthState, bool) {
	githubToken, err := validatedGitHubToken(ctx, runner, runtimeConfigPath, initCfg)
	if err != nil {
		if strings.TrimSpace(err.Error()) == "github token is required" {
			return "", githubTokenNeedsConfigureState(harness, ""), true
		}
		return "", githubTokenNeedsConfigureState(harness, err.Error()), true
	}
	if strings.TrimSpace(githubToken) == "" {
		return "", githubTokenNeedsConfigureState(harness, ""), true
	}
	if err := setGitHubTokenEnvironment(githubToken); err != nil {
		return "", githubTokenNeedsConfigureState(harness, fmt.Sprintf("set github token env: %v", err)), true
	}
	return strings.TrimSpace(githubToken), web.AgentAuthState{}, false
}

func configureGitHubToken(
	ctx context.Context,
	harness, runtimeConfigPath string,
	initCfg localruntime.InitConfig,
	runner execx.Runner,
	rawInput, requiredMessage string,
) (string, web.AgentAuthState, error) {
	token := strings.TrimSpace(rawInput)
	requiredMessage = stringsx.FirstNonEmpty(requiredMessage, githubTokenPasteConfigureMessage)
	if token == "" {
		state := githubTokenNeedsConfigureState(harness, requiredMessage)
		return "", state, fmt.Errorf("github token is required")
	}
	if err := validateGitHubToken(ctx, runner, token); err != nil {
		state := githubTokenNeedsConfigureState(harness, err.Error())
		return "", state, err
	}
	if err := starAgent00Repository(ctx, token); err != nil {
		state := githubTokenNeedsConfigureState(harness, err.Error())
		return "", state, err
	}

	if err := localruntime.SaveRuntimeConfigGitHubToken(runtimeConfigPath, initCfg, token); err != nil {
		state := githubTokenNeedsConfigureState(harness, fmt.Sprintf("save github token: %v", err))
		return "", state, err
	}
	if err := setGitHubTokenEnvironment(token); err != nil {
		state := githubTokenNeedsConfigureState(harness, fmt.Sprintf("set github token env: %v", err))
		return "", state, err
	}

	return token, web.AgentAuthState{}, nil
}

func starAgent00Repository(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("github token is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(githubStarAPIBaseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("github star API base URL is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	starCtx, cancel := context.WithTimeout(ctx, githubStarTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(starCtx, http.MethodPut, baseURL+agent00StarPath, nil)
	if err != nil {
		return fmt.Errorf("star %s: %w", agent00Repository, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := githubStarHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("star %s: %w", agent00Repository, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := strings.TrimSpace(string(body))
	detail = strings.ReplaceAll(detail, token, "[redacted]")
	if detail == "" {
		detail = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("star %s: github API returned %s: %s", agent00Repository, resp.Status, detail)
}

func configureGitHubTokenAndApply(
	ctx context.Context,
	harness, runtimeConfigPath string,
	initCfg localruntime.InitConfig,
	runner execx.Runner,
	rawInput string,
	onFailure func(web.AgentAuthState, error) (web.AgentAuthState, error),
	onSuccess func(string) (web.AgentAuthState, error),
) (web.AgentAuthState, error) {
	token, failureState, err := configureGitHubToken(
		ctx,
		harness,
		runtimeConfigPath,
		initCfg,
		runner,
		rawInput,
		githubTokenPasteConfigureMessage,
	)
	if err != nil {
		if onFailure != nil {
			return onFailure(failureState, err)
		}
		return failureState, err
	}
	if onSuccess == nil {
		return web.AgentAuthState{}, nil
	}
	return onSuccess(token)
}

func validateGitHubToken(ctx context.Context, runner execx.Runner, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("github token is required")
	}
	if runner == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, githubTokenValidationTimeout)
	defer cancel()

	if err := withTemporaryGitHubTokenEnvironment(token, func() error {
		return runGitHubTokenValidationCommand(probeCtx, runner)
	}); err != nil {
		return fmt.Errorf("validate github token: %w", err)
	}
	return nil
}

func gitHubTokenValidationCommand() execx.Command {
	return execx.Command{Name: "gh", Args: []string{"auth", "status", "--hostname", "github.com"}}
}

func runGitHubTokenValidationCommand(ctx context.Context, runner execx.Runner) error {
	_, err := runner.Run(ctx, gitHubTokenValidationCommand())
	return err
}

func withTemporaryGitHubTokenEnvironment(token string, run func() error) error {
	if run == nil {
		return nil
	}

	githubTokenValidationMu.Lock()
	defer githubTokenValidationMu.Unlock()

	previousGH, hadGH := os.LookupEnv("GH_TOKEN")
	previousGitHub, hadGitHub := os.LookupEnv("GITHUB_TOKEN")

	restore := func() error {
		var restoreErr error
		if hadGH {
			restoreErr = errors.Join(restoreErr, os.Setenv("GH_TOKEN", previousGH))
		} else {
			restoreErr = errors.Join(restoreErr, os.Unsetenv("GH_TOKEN"))
		}
		if hadGitHub {
			restoreErr = errors.Join(restoreErr, os.Setenv("GITHUB_TOKEN", previousGitHub))
		} else {
			restoreErr = errors.Join(restoreErr, os.Unsetenv("GITHUB_TOKEN"))
		}
		return restoreErr
	}

	if err := os.Setenv("GH_TOKEN", token); err != nil {
		return fmt.Errorf("set temporary GH_TOKEN: %w", err)
	}
	if err := os.Setenv("GITHUB_TOKEN", token); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			return fmt.Errorf("set temporary GITHUB_TOKEN: %v (restore env failed: %v)", err, restoreErr)
		}
		return fmt.Errorf("set temporary GITHUB_TOKEN: %w", err)
	}

	runErr := run()
	restoreErr := restore()
	if runErr != nil && restoreErr != nil {
		return fmt.Errorf("%v (restore env failed: %v)", runErr, restoreErr)
	}
	if runErr != nil {
		return runErr
	}
	if restoreErr != nil {
		return fmt.Errorf("restore github token env: %w", restoreErr)
	}
	return nil
}
