package main

import (
	"context"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

// Test-only conveniences over the production auth gate constructors.

func newClaudeAuthGate(ctx context.Context, command string, logf func(string, ...any)) *claudeAuthGate {
	return newClaudeAuthGateWithContextAndConfigAndRunner(ctx, nil, command, "", localruntime.InitConfig{}, logf)
}

func newClaudeAuthGateWithConfig(command, runtimeConfigPath string, initCfg localruntime.InitConfig) *claudeAuthGate {
	return newClaudeAuthGateWithContextAndConfigAndRunner(context.Background(), nil, command, runtimeConfigPath, initCfg, nil)
}

func newClaudeAuthGateWithContextAndConfig(
	ctx context.Context,
	command, runtimeConfigPath string,
	initCfg localruntime.InitConfig,
	logf func(string, ...any),
) *claudeAuthGate {
	return newClaudeAuthGateWithContextAndConfigAndRunner(ctx, nil, command, runtimeConfigPath, initCfg, logf)
}

func newCodexAuthGate(
	ctx context.Context,
	runner execx.Runner,
	command string,
	logf func(string, ...any),
) *codexAuthGate {
	return newCodexAuthGateWithConfig(ctx, runner, command, "", localruntime.InitConfig{}, false, logf)
}
