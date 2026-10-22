package main

import (
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

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
