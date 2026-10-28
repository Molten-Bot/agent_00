package main

import (
	"context"
	"errors"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

func signingSetupRequired(ctx context.Context, settings localruntime.SigningSettings, applyErr error) bool {
	if applyErr == nil || !errors.Is(applyErr, gitsigning.ErrSetup) {
		return false
	}
	state, err := settings.Status(ctx)
	return err == nil && state.Enabled && !state.Configured
}

func signingSettings(cfg localruntime.InitConfig, runner execx.Runner) localruntime.SigningSettings {
	if cfg.RuntimeConfigPath == "" {
		cfg.RuntimeConfigPath = localruntime.ResolveRuntimeConfigPath("")
	}
	return localruntime.SigningSettings{
		Path: cfg.RuntimeConfigPath, Initial: cfg, Runner: runner,
		GitHub: func() gitsigning.GitHub {
			token, _ := firstConfiguredGitHubToken(cfg.RuntimeConfigPath, cfg)
			return gitsigning.GitHub{Token: token}
		},
	}
}
