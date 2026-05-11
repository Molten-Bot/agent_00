# MoltenHub Code

Run AI coding agents against GitHub repositories, publish their changes, and
watch pull request checks. Product details live at
[molten.bot/code](https://molten.bot/code).

## Quick Start

### Docker

```bash
mkdir -p ./.moltenhub
docker run --rm -p 7777:7777 \
  -e GITHUB_TOKEN \
  -v "$PWD/.moltenhub:/workspace/config" \
  moltenai/moltenhub-code:latest
```

The image defaults to `with-config`, which uses `/workspace/config` as the
persistent config and CLI auth home:

- `MOLTEN_HUB_TOKEN` present: write or update `config.json`, then start
  `harness hub`.
- `config.json` with Hub fields: start `harness hub --config`.
- `config.json` with run fields: execute `harness run --config`.
- `init.json` present and `config.json` absent: start `harness hub --init`.
- No config files: start local Hub onboarding UI on port `7777`.

Compose example:

```yaml
services:
  codex:
    image: moltenai/moltenhub-code:latest
    ports:
      - "3331:7777"
    volumes:
      - ./.moltenhub:/workspace/config
    environment:
      GITHUB_TOKEN: ${GITHUB_TOKEN}
      MOLTEN_HUB_TOKEN: ${MOLTEN_HUB_TOKEN}
      MOLTEN_HUB_REGION: na
      HARNESS_AGENT_HARNESS: codex
      OPENAI_API_KEY: ${OPENAI_API_KEY}
```

Compose environment must use mapping syntax (`KEY: value`) or list syntax
(`KEY=value`). List entries like `KEY:value` are malformed.

The runtime image includes Go `1.26.2`, Node `25.9.0`, Python 3, `git`, `gh`,
`jq`, `rg`, Playwright Chromium, and these agent CLIs:

- `@openai/codex`
- `@anthropic-ai/claude-code`
- `@augmentcode/auggie`
- `@mariozechner/pi-coding-agent`
- `opencode-ai`

More Docker config details: [docker/config/README.md](docker/config/README.md).

### Local Build

Requires Go `1.26.2` or newer plus `git`, `gh`, and the selected agent CLI.

```bash
go build -o bin/harness ./cmd/harness
./bin/harness hub
```

Local `harness hub` listens on `127.0.0.1:7777` by default.

## CLI

```bash
harness run --config run.example.json
harness multiplex --config ./tasks --parallel 2
harness hub --config ./.moltenhub/config.json
harness hub --init ./.moltenhub/init.json
```

- `run` executes one repository task.
- `multiplex` runs multiple config files or config directories.
- `hub` starts the local UI and optional remote Hub transport.

## Run Config

Run configs are JSON or JSONC. Minimal example:

```json
{
  "repo": "git@github.com:owner/repo.git",
  "agentHarness": "codex",
  "prompt": "Update README setup instructions."
}
```

Common fields:

- `repo`, `repoUrl`, or `repos`: target repository or repositories.
- `baseBranch`: branch to clone; omit for repository default branch.
- `branch`: alias for `baseBranch`.
- `targetSubdir`: repository subdirectory for single-repo work; defaults to `.`.
- `prompt`: task sent to the agent.
- `agentHarness`: `codex`, `claude`, `auggie`, `pi`, or `opencode`.
- `agentCommand`: optional executable override.
- `responseMode`: defaults to `caveman-full`; set `off` for normal prose.
- `images`: base64 prompt images; supported by `codex` and `pi`.
- `review`: PR review selector using `prNumber`, `prUrl`, or `headBranch`.
- `commitMessage`, `prTitle`, `prBody`, `labels`, `reviewers`: optional PR
  metadata.

Use camelCase field names. Snake_case run-config fields are rejected.

See [run.example.json](run.example.json) for a commented template.

## Runtime Behavior

Each `harness run`:

1. Checks required tools and selected agent CLI.
2. Verifies GitHub auth with `gh auth status`.
3. Creates an isolated workspace under `/workspace`.
4. Clones each repo at `baseBranch`, or the repository default branch when
   omitted.
5. Bootstraps an empty `main` branch when a new GitHub repo has no refs.
6. Creates a `moltenhub-...` work branch from `main` or `master`; for other
   branches it works directly on that branch.
7. Probes publish access before agent execution. Public GitHub repos may fall
   back to a fork when direct push is denied.
8. Runs the selected agent in `targetSubdir` for one repo, or the workspace root
   for multi-repo runs.
9. Commits changed repos, pushes branches, opens or reuses PRs, and watches
   required checks.
10. If checks fail, runs up to three focused remediation attempts and pushes
    follow-up commits.

Harness-created commits include:

```text
Co-authored-by: Molten Bot 000 <260473928+moltenbot000@users.noreply.github.com>
```

If no repository changes remain after the agent runs, the task exits
successfully with `status=no_changes`.

## Hub Configuration

Useful environment variables:

- `GITHUB_TOKEN` or `GH_TOKEN`: GitHub auth for clone, push, PRs, and checks.
- `MOLTEN_HUB_TOKEN`: remote Hub agent token.
- `MOLTEN_HUB_REGION`: `na` or `eu`; defaults to `na`.
- `MOLTEN_HUB_URL`: explicit hosted Hub API URL,
  `https://na.hub.molten.bot/v1` or `https://eu.hub.molten.bot/v1`.
- `MOLTEN_HUB_SESSION_KEY`: runtime config session key; defaults to `main`.
- `HARNESS_AGENT_HARNESS`: default agent harness.
- `HARNESS_AGENT_COMMAND`: default agent executable.
- `OPENAI_API_KEY`: Codex login bootstrap; also usable by OpenCode providers.
- `AUGMENT_SESSION_AUTH`: Auggie session JSON from `auggie token print`.
- `PI_PROVIDER_AUTH` or `PI_AUTH_JSON`: Pi provider auth bootstrap.

Hub OpenAPI:

- Live: [`https://na.hub.molten.bot/openapi.yaml`](https://na.hub.molten.bot/openapi.yaml)
- Offline snapshot: [na.hub.molten.bot.openapi.yaml](na.hub.molten.bot.openapi.yaml)

## Response Modes

Supported `responseMode` values:

- `default`
- `off`
- `caveman-lite`
- `caveman-full`
- `caveman-ultra`
- `caveman-wenyan-lite`
- `caveman-wenyan-full`
- `caveman-wenyan-ultra`

Omitted or `default` maps to `caveman-full`. The harness prepends the bundled
[Caveman skill](skills/caveman/SKILL.md) to the agent prompt unless
`responseMode` is `off`.

## Development

```bash
go test ./...
```

There is no separate dependency install step for the Go module. Dependencies are
declared in [go.mod](go.mod).
