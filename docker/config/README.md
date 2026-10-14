# Docker Runtime Config Mount

`docker-compose.yml` mounts `./.moltenhub` to `/workspace/config`. The existing mount name is retained so saved CLI credentials remain available. The image uses `/workspace/config/home` for persistent Codex and Claude authentication.

With no config file, the container starts `harness serve` on port 7777. Complete agent and GitHub authorization in the dashboard, or supply `GITHUB_TOKEN` / `GH_TOKEN` and optional `OPENAI_API_KEY` through the environment.

- A `config.json` containing task fields (`repo`, `repos`, `repoUrl`, `prompt`, or `libraryTaskName`) runs once with `harness run --config`.
- A `config.json` containing local runtime preferences starts `harness serve --config`.
- An `init.json` supplies initial local runtime preferences when `config.json` is absent.

Local preferences include `agent_harness`, `agent_command`, `log_level`, `dispatcher`, and `review_watch`. Credentials saved during onboarding persist across restarts. Set `HARNESS_UI_LISTEN` to override the container listen address (default `:7777`).

For a single task, copy `run.example.json` to `.moltenhub/config.json` and edit it. For the dashboard, leave that file absent or use local runtime preferences.

For manual bind mounts, use `--user "$(id -u):$(id -g)"` when your host user is not uid/gid 1000 to avoid write-permission failures.
