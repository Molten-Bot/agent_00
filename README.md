# Agent_00

The highest velocity way to make code changes. Run AI coding agents against GitHub repositories, prompt to PR. Product details live at
[molten.bot/agent_00](https://molten.bot/agent_00).

## Quick Start

### Docker

```bash
docker run -p 7777:7777 \
  --cap-drop=ALL \
  --security-opt seccomp=unconfined \
  --security-opt apparmor=unconfined \
  moltenai/agent_00:latest
```

### Docker Compose with Prompt Dictation

```bash
cp .env.example .env
docker compose up
```

For a local image build instead of the published Docker Hub image:

```bash
docker compose -f docker-compose.yml -f docker-compose.local.yml up --build
```

The Compose stack runs `agent_00` with a `linuxserver/faster-whisper`
sidecar. The web UI probes `faster-whisper:10300`; when it is reachable, the
Prompt Studio shows a microphone button that appends dictated text to the
prompt field. The Compose sidecar disables Docker log capture because
`wyoming-faster-whisper` logs transcript text at INFO level.
The bundled sidecar defaults to the `base-int8` model and English language
hints for more reliable short-form dictation; set both `WHISPER_LANG=auto` and
`WHISPER_SPEECH_LANGUAGE=auto` to use Whisper language detection.

### Workspace storage

Retained task checkouts need disk capacity. For long-running Docker installations,
mount a persistent volume at the configured workspace base. Do not point both
`HARNESS_WORKSPACE_RAM_BASE` and `HARNESS_WORKSPACE_DISK_BASE` at the same small
tmpfs: accumulated tasks can fill it and prevent even `git clone` from starting.
Linux workspace allocation checks for at least 512 MiB free on each allocation
and falls back to the configured disk base when the preferred base is full.
This reserve is an early check, not a limit on an individual build's disk usage.
Storage exhaustion requires an infrastructure repair and does not launch another
coding task on the same exhausted filesystem.

Codex sandboxing needs nested user/mount namespaces supported by the container
runtime. The image uses Codex's bundled bubblewrap helper. The startup warning
about missing `bwrap` on PATH does not itself mean command execution failed.
Docker's default seccomp profile blocks namespace creation, and its default
AppArmor profile blocks the sandbox mounts. Compose and the Docker command above
relax these two outer profiles for the agent container. This reduces Docker's
defense in depth; the container runs as `node` with all Linux capabilities
dropped, and Codex keeps its `workspace-write` filesystem enforcement.
Hosts with custom security policies can use profiles that permit bubblewrap's
nested namespaces and mounts instead. Host user namespace support is still
required.

Agent work can write only inside its allocated task directory (the parent of
that task's cloned repositories). Temporary files, home directories, and build
caches live under `.moltenhub-agent-io` inside that directory. The harness
overrides Codex's writable roots, excludes global `/tmp` and implicit `$TMPDIR`
access, and sets approvals to `never` so commands cannot escalate outside the
task. Existing operator config cannot add extra writable roots. Multi-repository
tasks share their own task parent; other tasks remain read-only.

Claude runs through `codex sandbox` with the same task boundary, including its
child processes. This requires the Codex CLI even when selecting Claude; both
CLIs are included in the Docker image. Missing or failed sandbox tooling stops
the run. The trusted Codex CLI still saves login rotation and session state to
the persistent `CODEX_HOME`; model-issued commands cannot write there.

Keep the bundled helper in this image: Debian trixie's `bubblewrap` 0.12.0 fails
to mount `/proc` under Docker's masked proc paths in our container smoke check,
while Codex 0.147.0's bundled helper succeeds with the security options above.

Recreate an existing container to apply security options; restarting it does not
change them. For Compose, run `docker compose up -d --force-recreate agent_00`.
Pulling a newer image also does not change an existing container's security
options. Check the container you actually dispatch tasks to:

```bash
docker inspect YOUR_CONTAINER --format '{{json .HostConfig.SecurityOpt}}'
```

If this prints `null` or `[]`, recreate it with the two security options above
(or equivalent custom profiles), retaining its configuration/workspace volumes.
Check the sandbox in the actual container before dispatching work:

```bash
docker compose exec agent_00 codex sandbox \
  -c 'sandbox_mode="workspace-write"' -- /bin/true
```

To verify a real authenticated task against a locally built image:

```bash
docker build -t agent00-local:smoke .
./docker/smoke-test.sh agent00-local:smoke YOUR_AUTHENTICATED_CONTAINER
```

The smoke check runs an isolated container with the documented security options,
checks Codex and Claude filesystem boundaries, then invokes Codex with the
harness's task environment and command. It requires the source container's
persisted login at `/workspace/config/home/.codex`, shares its session lock, and
verifies that a shell task creates the expected file. It makes a provider request
but does not clone, push, create a PR, or launch the Hub daemon. It does not change
the source container's security options; the source still needs recreation if
its own sandbox check fails.

Run the repository build, full test suite and Go race tests inside the task
sandbox as a separate check; this requires no provider login:

```bash
./docker/validate-sandbox.sh agent00-local:smoke
```

This uses the current source, keeps PID isolation and task-only writable roots,
and runs the container with all Linux capabilities dropped. The runtime includes
GCC and libc headers for cgo and `go test -race`; the shipped harness is still
built with `CGO_ENABLED=0`. Bootstrap reads its own valid environment variables.
Malformed Compose `KEY:value` entries are ignored, matching the documented
environment format; bootstrap does not recover them from another process.

A namespace or sandbox mount permission failure is an infrastructure blocker,
not a prompt or repository failure. The runner does not disable the Codex sandbox
on errors.
For jobs that fetch source sites or dependencies, the operator can enable
`sandbox_workspace_write.network_access = true` in the persistent Codex
`config.toml`; filesystem enforcement remains enabled. The image installs both
Chromium and its headless shell so default Playwright launches work.

### Persistent Codex authentication

Sign in once through the UI. Login and every Codex task use the same persistent
`CODEX_HOME` (default `/workspace/config/home/.codex` in Docker). Codex saves
rotated tokens there for subsequent tasks; credentials are never copied into
task worktrees. The Compose configuration volume retains the login across
container restarts. Mount `/workspace/config` when using plain `docker run` if
you also need credentials to survive container replacement. Custom `CODEX_HOME`
locations must be writable and mounted if persistence across replacement is needed.

Codex invocations sharing a home run one at a time under a process-safe session
lock, including device login. Git preparation and other task stages may still
run concurrently. Waiting for the lock is cancelable and does not consume the
agent execution timeout. The child retains the lock if the harness exits;
the kernel releases it after the child exits. Do not delete the lock file to
unblock tasks. Run external Codex clients with a separate
login/home rather than sharing this container's credentials across machines.

If a login is revoked, expired, or rejected, tasks stop with an authentication
requirement and do not launch an automatic repair agent. Complete device login
again in the UI, or replace the file-based login using the same `CODEX_HOME`
inside the container. Keyring-backed logins should recover through the UI.
A fresh login clears the blocked credential generation. Existing already-used
refresh tokens need this one-time recovery after upgrading; persistence cannot
restore a token that the provider has invalidated.

### Local Build

Requires Go `1.26.5` or newer plus `git`, `gh`, and the Codex CLI. Claude tasks
also require the Claude CLI; Codex provides their filesystem sandbox.

```bash
go build -o bin/harness ./cmd/harness
./bin/harness hub
```

Local `harness hub` listens on `127.0.0.1:7777` by default.

With GitHub auth configured, local hub mode also watches GitHub review-request
notifications by default. When the authenticated GitHub user is still requested
as a PR reviewer, the harness queues the bundled `code-review` task and posts a
summary comment back to the original PR. Auto-merge is off by default; opt in
with `review_watch.auto_merge: true` in the runtime config.

Post-task PR review cycles are also off by default. Configure
`review_watch.review_level` as `off`, `low`, `medium`, or `high` to run 0 or up
to 1, 3, or 6 review passes after a task creates its pull request and
initial checks finish. A pass with no negative findings ends the cycle early.
Actionable findings are repaired and checked before the next pass; final-pass
findings are repaired and checked without an extra pass.

### Go Module

Agent_00 is distributed as a Go module from this Git repository. Install
the latest stable release from the renamed module path.

```bash
go get github.com/Molten-Bot/agent_00@latest
go install github.com/Molten-Bot/agent_00/cmd/harness@latest
```

## Bundled Tools

The Docker image includes `railsmith`, an npm CLI for creating and maintaining
repository `AGENTS.md` guardrails. Container startup also seeds a Codex skill
from the package's `AGENT_GUIDE.md` into the persisted Codex home so agents can
activate Railsmith guidance during coding sessions.

```bash
railsmith guide
railsmith doctor --root .
railsmith diff --root . --mode detailed
railsmith check --root .
```

The image also includes `git-changes-by-day`, a Go CLI for exporting git
history to CSV. Agents can use it when a task needs per-commit change data.

```bash
git-changes-by-day -repo /path/to/repo -text-out /tmp/commit-text.csv
```

The CSV includes UTC datetime/date columns, commit metadata, changed file
counts, and line change counts.

The image includes Playwright's full Chromium build without the legacy
headless-shell download. For headless runs, select Playwright's new headless
mode with `channel: "chromium"` in `playwright.config` or the equivalent launch
option.

## Environment Variables

Useful environment variables:

- `GITHUB_TOKEN` or `GH_TOKEN`: GitHub auth for clone, push, PRs, and checks.
- `MOLTEN_HUB_TOKEN`: remote Hub agent token.
- `MOLTEN_HUB_REGION`: `na` or `eu`; defaults to `na`.
- `MOLTEN_HUB_URL`: explicit hosted Hub API URL,
  `https://na.hub.molten.bot/v1` or `https://eu.hub.molten.bot/v1`.
- `MOLTEN_HUB_SESSION_KEY`: runtime config session key; defaults to `main`.
- `HARNESS_AGENT_HARNESS`: default agent harness.
- `HARNESS_AGENT_COMMAND`: default agent executable.
- `OPENAI_API_KEY`: Codex login bootstrap.
- `MOLTEN_HUB_DEFAULT_REPOSITORY`: optional repository prefill for Prompt
  Studio; omitted leaves the repository field empty.
- `WHISPER_SPEECH_HOST`: optional speech sidecar host; defaults to
  `faster-whisper`.
- `WHISPER_SPEECH_PORT`: optional speech sidecar Wyoming port; defaults to
  `10300`.
- `WHISPER_SPEECH_LANGUAGE`: optional speech language hint; defaults to
  `en`. Set to `auto` to use Whisper language detection.
- `WHISPER_SPEECH_TIMEOUT_SECONDS`: optional speech transcription timeout in
  seconds; defaults to `120`.
- `WHISPER_SPEECH_DISABLED`: set to `true` to hide prompt dictation.

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
