package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/web"
)

func (g *selectableAgentAuthGate) Diagnostics(ctx context.Context) (web.AgentAuthDiagnostics, error) {
	state, err := g.Status(ctx)
	if err != nil {
		return web.AgentAuthDiagnostics{}, err
	}
	g.mu.Lock()
	delegate := g.delegate
	g.mu.Unlock()
	if provider, ok := delegate.(interface {
		Diagnostics(context.Context) (web.AgentAuthDiagnostics, error)
	}); ok {
		return provider.Diagnostics(ctx)
	}
	return web.AgentAuthDiagnostics{
		Harness: state.Harness, State: state.State, Ready: state.Ready, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Checks: []web.AuthDiagnosticCheck{{Code: "AUTH_STATE_ONLY", Status: "info", Description: "Detailed diagnostics are available for Claude; use the authentication endpoint for this runtime's setup flow."}},
	}, nil
}

func (g *claudeAuthGate) Diagnostics(ctx context.Context) (web.AgentAuthDiagnostics, error) {
	// Do not start login, submit input, or copy credential contents into the response.
	g.mu.Lock()
	result := web.AgentAuthDiagnostics{
		Harness: "claude", State: g.state, Ready: g.ready,
		CheckedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		LoginRunning: g.procRunning, LoginURLAvailable: g.authURL != "",
		BrowserCodeSubmitted: g.pendingBrowserCode != "",
		LastLoginExitCode:    g.lastLoginExitCode,
		Checks:               []web.AuthDiagnosticCheck{},
	}
	command, runner, loginError := g.command, g.runner, g.lastLoginErrorCode
	g.mu.Unlock()
	add := func(code, status, description, action string) {
		result.Checks = append(result.Checks, web.AuthDiagnosticCheck{Code: code, Status: status, Description: description, NextAction: action})
	}
	g.rejectedMu.Lock()
	rejected := g.rejectedCredentials != "" && g.rejectedCredentials == g.credentialsFingerprint()
	g.rejectedMu.Unlock()
	if rejected {
		add("CLAUDE_AUTH_REJECTED", "error", "A task rejected the currently configured Claude credentials.", "Complete a fresh browser sign-in, then rerun the failed task.")
	}
	// Diagnostics stay read-only: report the broken configuration file without
	// restoring it. The auth gate and the task path perform the repair.
	if configDir, err := agentruntime.ClaudeHome(os.Environ()); err == nil {
		switch restorable, backup, stateErr := agentruntime.ClaudeConfigRestorable(configDir); {
		case stateErr != nil:
			add("CLAUDE_CONFIG_FILE_UNREADABLE", "error", "The Claude configuration backups could not be inspected.", "Check the runtime user's permissions on the Claude configuration directory.")
		case restorable:
			add(
				"CLAUDE_CONFIG_FILE_MISSING",
				"error",
				"The Claude configuration file is missing while Claude backups are present, so the CLI refuses to start.",
				fmt.Sprintf("Restore it with: cp %q %q", backup, agentruntime.ClaudeConfigPath(configDir)),
			)
		}
	}
	credentials := &web.AuthCredentialDiagnostics{}
	result.Credentials = credentials
	if path := claudeCredentialsPath(); path != "" {
		credentials.Present = true
		data, err := os.ReadFile(path)
		var payload struct {
			OAuth *struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    *int64 `json:"expiresAt"`
			} `json:"claudeAiOauth"`
		}
		if err != nil {
			add("CLAUDE_CREDENTIALS_UNREADABLE", "error", "The credentials file cannot be read.", "Check the runtime user's file permissions.")
		} else if json.Unmarshal(data, &payload) != nil {
			add("CLAUDE_CREDENTIALS_INVALID", "error", "The credentials file is not valid Claude credentials JSON.", "Complete browser sign-in to replace the invalid file.")
		} else {
			credentials.ValidJSON = true
			if oauth := payload.OAuth; oauth != nil {
				credentials.HasAccessToken = strings.TrimSpace(oauth.AccessToken) != ""
				credentials.HasRefreshToken = strings.TrimSpace(oauth.RefreshToken) != ""
				credentials.ExpiresAt = oauth.ExpiresAt
				credentials.Expired = oauth.ExpiresAt != nil && *oauth.ExpiresAt <= time.Now().UnixMilli()
				if !credentials.HasAccessToken {
					add("CLAUDE_CREDENTIALS_EMPTY", "error", "The credentials file contains no access token.", "Complete a fresh browser sign-in.")
				} else if credentials.Expired {
					severity, action := "error", "Complete a fresh browser sign-in."
					if credentials.HasRefreshToken {
						severity, action = "warning", "Claude must refresh this session before inference. If refresh fails, complete a fresh browser sign-in."
					}
					add("CLAUDE_ACCESS_TOKEN_EXPIRED", severity, "The stored access token has expired.", action)
				}
			}
		}
	}
	if loginError != "" {
		description := "The Claude login process failed."
		if loginError == "CLAUDE_BROWSER_CODE_INVALID" {
			description = "Claude rejected the browser authorization code."
		}
		add(loginError, "error", description, "Start a new browser sign-in and paste the complete code from that same login attempt.")
	}
	if result.LoginRunning {
		add("CLAUDE_LOGIN_PENDING", "info", "The Claude browser login process is running.", "Complete the browser sign-in and submit its code in the app.")
	}
	if runner == nil {
		runner = execx.OSRunner{}
	}
	if command == "" {
		command = "claude"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := runner.Run(probeCtx, execx.Command{Name: command, Args: []string{"auth", "status"}})
	cli := &web.AuthCLIDiagnostics{}
	result.CLI = cli
	if err == nil {
		code := 0
		cli.ExitCode = &code
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			cli.ExitCode = &code
		}
	}
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal([]byte(output.Stdout), &status) == nil && status.LoggedIn != nil {
		cli.StatusKnown, cli.LoggedIn = true, *status.LoggedIn
		if cli.LoggedIn {
			add("CLAUDE_CLI_LOGGED_IN", "info", "Claude reports a stored login. This does not verify token refresh or an authenticated inference request.", "")
		} else {
			add("CLAUDE_CLI_LOGGED_OUT", "error", "Claude reports that it is logged out.", "Complete browser sign-in in this runtime.")
		}
	} else {
		code := "CLAUDE_STATUS_UNAVAILABLE"
		if probeCtx.Err() != nil {
			code = "CLAUDE_STATUS_TIMEOUT"
		}
		add(code, "error", "Claude CLI authentication status could not be read.", "Check the configured Claude executable and runtime logs.")
	}
	return result, nil
}
