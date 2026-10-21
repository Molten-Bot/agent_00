# Docker Runtime Config Mount

`docker-compose.yml` mounts `${AGENT_00_CONFIG_DIR:-./.agent00}` to `/workspace/config`. The image uses `/workspace/config/home` for persistent Codex and Claude authentication. When upgrading an existing installation, set `AGENT_00_CONFIG_DIR` to its current configuration directory before recreating the container, or stop all harness and agent processes sharing that directory and move the whole directory to `.agent00`. Stop older processes before upgrading: session lock filenames have changed, so mixed versions must not share a Codex login concurrently.

With no config file, the container starts `harness serve` on port 7777. Complete agent and GitHub authorization in the dashboard, or supply `GITHUB_TOKEN` / `GH_TOKEN` and optional `OPENAI_API_KEY` through the environment.

- A `config.json` containing task fields (`repo`, `repos`, `repoUrl`, `prompt`, or `libraryTaskName`) runs once with `harness run --config`.
- A `config.json` containing local runtime preferences starts `harness serve --config`.
- An `init.json` supplies initial local runtime preferences when `config.json` is absent.

Local preferences include `agent_harness`, `agent_command`, `log_level`, `scheduler`, and `review_watch`. Existing `dispatcher` settings remain readable and become `scheduler` when settings are saved. Credentials saved during onboarding persist across restarts. Set `HARNESS_UI_LISTEN` to override the container listen address (default `:7777`).

For a single task, copy `run.example.json` to `config.json` inside your config directory and edit it. For the dashboard, leave that file absent or use local runtime preferences. Outside Docker, local preferences use `.agent00/config.json`. For an existing directory, pass `serve --config /path/to/config.json` or set `HARNESS_RUNTIME_CONFIG_PATH`; old locations are no longer discovered automatically.

For manual bind mounts, use `--user "$(id -u):$(id -g)"` when your host user is not uid/gid 1000 to avoid write-permission failures.
