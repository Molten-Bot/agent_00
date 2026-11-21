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

The **deploy-vnext** GitHub Actions workflow has an **Include ARM64 image**
checkbox, unchecked by default. Leave it unchecked to publish AMD64 only and
skip ARM emulation; check it to publish both AMD64 and ARM64. This applies to
both the `vnext` tag and the dated build tag. An AMD64-only release replaces
the previous `vnext` manifest, so ARM users should use a dated tag from a build
that included ARM64. **deploy-prod** promotes the architectures present in its
selected source tag; choose a build with ARM64 to include it in `latest`.

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

#### Microphone requirements

Whisper dictation records through `navigator.mediaDevices.getUserMedia`, which
browsers expose only to secure contexts. Reaching the UI over plain `http://`
on a LAN address (for example `http://192.168.1.10:7777`) disables microphone
capture in every browser, including Safari, Chrome, and Firefox. Put the UI
behind HTTPS (a tunnel or reverse proxy) to record from a phone; on the machine
running the harness, `http://localhost:<port>` is already a secure context.

Browsers that do expose `getUserMedia` can still refuse it when the button is
tapped - permission blocked, no usable input, or an iOS browser that does not
hand WKWebView the microphone. Third-party iOS browsers vary by version, so the
UI does not assume the outcome: it tries to record, and only falls back when the
request is actually refused. Safari on iOS remains the most reliable option for
Whisper.

On iOS and Android, when capture is unavailable - up front or after a refusal -
the microphone button stays usable and points at native dictation: it focuses
the prompt and explains the limitation so the keyboard's dictation key
(microphone icon) can be used to speak into the prompt instead. A refusal is
not final: dismissing a permission prompt looks the same as blocking it, so the
next tap asks for the microphone again. The same fallback covers an unreachable
whisper sidecar on phones and tablets, where native dictation needs no server
and the software keyboard carries a dictation key; a desktop keyboard has no
such key, so there the button disappears with the sidecar as before. Set
`WHISPER_SPEECH_DISABLED` to hide the button entirely.

### Library settings

Open **Settings → Library** to add a private library repository and choose its
name. Its tasks appear under that name in tabs to the left of the built-in
categories. An explicit task `displayName` is preserved; otherwise the task title
is derived from the source name and task slug.

Turn off **Show default library** and save to hide built-in tasks from the
picker. The setting persists across restarts and leaves custom tasks available.
Existing schedules for built-in tasks continue to run; delete a schedule to stop
its recurring runs.

### Recurring library tasks

On the Library page, choose a repository and use the clock beside a task to
schedule it every 1–168 hours. **Schedules** opens the list to edit the interval
or delete a recurrence. **Run** continues to launch a one-time task.

Schedules are stored in the app's `config.json` under `library_schedules`, keyed
by task slug and canonical GitHub repository (`task-slug@owner/repo`). SSH and
HTTPS URLs for the same repository share one schedule. Each run uses the current
library prompt and the saved branch, directory, and reviewers.

The first run is due one interval after saving; editing resets the next run time.
Keep the app running for schedules to execute (checked every 30 seconds). After
downtime, one overdue run resumes without replaying every missed interval. A run
waits while the same task and repository are already pending, running, or paused.
Submission failures appear in the schedules dialog and retry at the next interval.
Deleting a schedule leaves any run already started intact.

### GitHub issue automation

In **Settings → GitHub Issues**, enable GitHub Issues and automatic matching.
For assigned, labeled work, keep **Only issues assigned to me** enabled and add
`ready-for-agent` to **Trigger labels**. Any selected label matches; an empty
list allows any label. Assignment uses the same `gh` account as repository
and issue discovery. Disabled repositories are excluded.

The service checks immediately and every minute, including the existing open
backlog. Each matching issue is journaled before admission. Excluded issues
remain excluded; development associations and open dependencies block new work
without hiding an already tracked issue. Discovery checkpoints one page per
repository and rotates large repository lists. Repository discovery refreshes
every five minutes. **Scan now** requests an early scan.

Task recovery belongs to the specific task on the **GitHub Issues** page. Select
the issue there to review and retry its work when recovery is required.
A successful task status alone is not proof of delivery: automation verifies an
open (including draft) or merged PR for the reserved head and intended base.
Closed, unmerged PRs do not count as delivery.

Before queueing an automatic start, manual issue start, or explicit retry, the
harness fetches the current title, description, and all paginated comments from
GitHub. It builds the task prompt with the issue link, comment authors and
timestamps, reserved branch and PR base, and configured issue linking behavior.
Browser text and saved retry prompts are replaced with this fresh snapshot;
existing runtime options are retained. The agent receives the usual shared and
repository instructions and focuses on coding and validation while the harness
handles ownership, commits, pushes, and PR creation. A failed issue or comment
fetch blocks admission rather than queueing an incomplete prompt.

GitHub-hosted attachments in the description and comments become explicit
`assets` on the run config (URL, name, and source comment), with an asset count
on the task and a list in its prompt. Manual starts, automatic starts, and issue
retries all discover attachments from the fresh snapshot and deduplicate URLs.
Before the agent starts, the harness downloads them to `task-assets/` in the run
workspace, outside the repository. PNG, JPEG, GIF, and WebP images use the existing
agent image path support. Documents and other files are supplied as local paths;
the agent must use available readers and report formats it cannot inspect.
This does not install document converters or guarantee support for every format.

Downloads use the current `gh` credential for GitHub requests, with credentials
removed on storage redirects. Only GitHub upload URLs and known storage redirects
are fetched; other web links remain in the prompt. Limits are 32 attachments,
25 MiB per file, and 100 MiB total per run. Missing, inaccessible, oversized, or
HTML responses fail the run at the `assets` stage before agent execution. Retrying
downloads the references again; downloaded bytes are not embedded in saved configs.

Every automatic or manual issue start reserves `agent00-issue-<number>` as a
linked branch before queueing. Only the owner can start. New claims publish
immutable attempt metadata before branch creation, and ambiguous GitHub responses
are reconciled by exact claim identity. Tasks retain their reserved branch and
original PR base; collisions never generate a second branch.

The configurable execution exclusion label defaults to `agent_00-in-progress`
and cannot also be a trigger label. After the worker and its subprocesses stop,
automation removes the label it added. Removal failures are persisted and retried.
Warnings or stale heartbeats alone never unlock a running task. Branches and
ownership tags remain intact: unlocking execution does not release the issue to
another instance. Do not delete claim tags to clear a reservation.

Durable state lives beside runtime `config.json`: `issue-claims/`,
`issue-repairs/`, `issue-workspaces/`, `repair-workspaces/`, and `logs/.log/`.
Keep this directory on persistent storage. The standard Compose configuration
already mounts `/workspace/config`. Only one daemon may use the same runtime
directory; separate instances coordinate through GitHub ownership tags. Logs
survive startup. Unresolved work and evidence are retained; fulfilled issue
artifacts become eligible for cleanup after 30 days, provided no unresolved
repair still needs them. Journals remain available for audit.

The local recovery protocol uses inherited Unix file locks and records process
groups. A surviving agent or uncertain process termination prevents takeover.
Legacy claims and ownership changes require **Retry** in **Settings → GitHub Issues**, after stopping
the previous worker on every instance. Automatic cross-machine failover is not
performed. Restarting the owning instance recovers its durable dispatch intents.

Background recovery verifies PR delivery before retrying, reuses preserved local
changes and remote commits, and resumes publication once the journal confirms
agent success. Legacy journals without this checkpoint rerun the agent before
publishing, because pending changes alone do not prove a successful result.
It permits three agent attempts per failure cycle, with initial delays
of one and five minutes plus jitter. Infrastructure checks back off up to an
hour and honor GitHub retry/reset headers. Auth and signing gates do not consume
agent attempts. Exhausted attempts, explicit stops/pauses, and no-change results
without a PR remain visible blockers requiring review or explicit retry. A
manual recovery resets the agent-attempt budget and refreshes issue content.

A watchdog stops a single workflow step after two hours. Advancing to another
step resets its timer; streaming output, heartbeats, and retries in the same
step do not. There is no total task-duration limit. Configure defaults in the
runtime configuration (these are server settings, not run-config fields):

```json
"issue_recovery": {
  "max_attempts": 3,
  "step_timeout_seconds": 7200,
  "retry_delay_seconds": 60,
  "max_backoff_seconds": 3600
}
```

Suspected harness failures enqueue persistent, deduplicated agent_00 repair
tasks based on local logs. Enqueue failures remain queued; failed or interrupted
repair executions remain visible for investigation and do not spawn recursive
repair tasks. Customer prompts and identifiers are redacted from repair prompts
and PR context. A repair PR does not resolve the original issue, automatically
merge/deploy a fix, or reset its retry budget.

Repository rules must permit creation of ownership tags, branches, and issue
labels. Permission or storage failures remain explicit blockers. Progress cannot
be guaranteed during a permanent outage or loss of persistent storage; unresolved
issues are never intentionally treated as successful delivery.

Upgrade **all** instances before enabling backlog processing: older versions
ignore these ownership rules. Automation stays off unless already enabled, but
an enabled installation will scan its backlog after upgrading.

### Workspace storage

Retained task checkouts need disk capacity. For long-running Docker installations,
mount a persistent volume at the configured workspace base. Do not point both
`HARNESS_WORKSPACE_RAM_BASE` and `HARNESS_WORKSPACE_DISK_BASE` at the same small
tmpfs: accumulated tasks can fill it and prevent even `git clone` from starting.
Linux workspace allocation checks for at least 512 MiB free on each allocation
and falls back to the configured disk base when the preferred base is full.
This reserve is an early check, not a limit on an individual build's disk usage.
The daemon records the largest workspace size observed at task completion for each
repository or combination of repositories. It stores these estimates by repository
hash in `task-disk-usage.json` beside the runtime config. Tasks without a history
reserve 512 MiB. When available space cannot cover a task's estimate alongside
other reservations on the same filesystem, the task stays queued until space
becomes available. Force start overrides this admission check.
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
that task's cloned repositories) and, for Claude, the one scratch directory
inside the persistent configuration directory described below. Temporary files,
home directories, and build caches live under `.agent00-agent-io` inside that
directory. The harness overrides Codex's writable roots, excludes global `/tmp`
and implicit `$TMPDIR` access, and sets approvals to `never` so commands cannot
escalate outside the task. Existing operator config cannot add extra writable
roots. Multi-repository tasks share their own task parent; other tasks remain
read-only.

Claude runs through `codex sandbox` with the same task boundary, including its
child processes. This requires the Codex CLI even when selecting Claude; both
CLIs are included in the Docker image. Missing or failed sandbox tooling stops
the run. The trusted Codex CLI still saves login rotation and session state to
the persistent `CODEX_HOME`; model-issued commands cannot write there.

Claude runs also get one directory inside the persistent `CLAUDE_CONFIG_DIR` as
a writable root: `session-env`, the per-session scratch directory the CLI writes
to the first time it uses a tool. Without that grant the CLI hits a read-only
filesystem on its first tool call and runs the rest of the task without it. The
stored credentials and Claude's own configuration backups stay unwritable.
Read-only review passes get the same single grant, because a review boots the
same CLI tool runtime. The harness creates that directory before starting the
agent, since the sandbox cannot create it inside an otherwise read-only
configuration directory; a configuration directory the harness itself cannot
write to drops the grant instead of failing the run. The CLI fixes this path,
so concurrent tasks and review passes sharing a configuration directory share
that one scratch directory.

Keep the bundled helper in this image: Debian trixie's `bubblewrap` 0.12.0 fails
to mount `/proc` under Docker's masked proc paths in our container smoke check,
while Codex 0.147.0's bundled helper succeeds with the security options above.

Recreate an existing container to apply security options; restarting it does not
change them. For Compose, run `docker compose up -d --force-recreate agent_00`.
Pulling a newer image also does not change an existing container's security
options. Check the container you actually run tasks in:

```bash
docker inspect YOUR_CONTAINER --format '{{json .HostConfig.SecurityOpt}}'
```

If this prints `null` or `[]`, recreate it with the two security options above
(or equivalent custom profiles), retaining its configuration/workspace volumes.
Check the sandbox in the actual container before running work:

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
but does not clone, push, create a PR, or launch the local server. It does not change
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

### Optional commit signing

Open **Settings → Commit signing** to paste an SSH private key and its passphrase
(if protected), then choose **Save signing settings**. Ed25519, RSA, and ECDSA keys
are supported. The app validates the key and tests local signing. Copy the public
key shown in Settings into the same GitHub account under **SSH and GPG keys → New
SSH key**, choosing **Signing Key**, then use **Check registration**. Enable **Sign
commits for all tasks** and save again. GitHub authentication still uses the
existing token; the signing key is a separate credential.

Signing is off by default. When enabled, it applies to tasks that start after
saving, including scheduled tasks, review fixes, agent-created commits, merges,
and empty-repository initialization. The signing identity uses the authenticated
GitHub account and its ID-based noreply email. Setup errors stop the task, and
unsigned or invalid newly created commits stop publication. Existing commits are
not rewritten; this setting does not repair an existing PR's unsigned history or
resolve its review conversations. Confirm GitHub's **Verified** badge on a new PR.

The app saves an **unlocked private key** for unattended signing, even when the
imported key has a passphrase. The passphrase is never saved. Keys live in a
`signing/` directory beside the runtime `config.json`, with directory permissions
`0700` and key permissions `0600`; the JSON stores only metadata and a key reference.
The public key and fingerprint are visible in Settings; the private key is never
returned to the browser. Disabling retains the key. Replacing it retains previous
key files so running tasks can finish with their original signing configuration.
Treat backups of this configuration directory as credentials.

Native installations need Git 2.34+ and `ssh-keygen` on PATH. The Docker image
already includes the required tools; keep the whole configuration directory on
its persistent volume so `config.json` and `signing/` survive recreation. The
`run` and `multiplex` CLI modes read the same signing settings from
`HARNESS_RUNTIME_CONFIG_PATH` (default `./.agent00/config.json`). No signing secrets
belong in task JSON. Git settings are applied to subprocesses without changing
the host's global Git configuration.

Enable or disable signing in **Settings → Commit signing**. The saved preference
applies across restarts and to `serve`, `run`, and `multiplex`.
`HARNESS_COMMIT_SIGNING_ENABLED` is no longer used; remove it from existing
Compose configurations and manage signing in the app.

When signing is required but setup is incomplete, `serve` still starts and the
local UI stays reachable. The browser opens on **Settings → Commit signing**,
that section states why signing is blocked, and task submissions are refused
until a signing key is saved; no restart is needed once it is. Startup still
stops on signing configuration that the UI cannot fix, such as an unreadable
runtime configuration or an environment-supplied private key that fails the import, identity, or
registration checks. Tasks also recheck signing before running, so a key that is
saved but unregistered or unusable still stops the task rather than producing
unsigned commits.

To provision a key at boot, also set `HARNESS_COMMIT_SIGNING_PRIVATE_KEY` to the
literal multiline SSH private key, or `HARNESS_COMMIT_SIGNING_PRIVATE_KEY_FILE`
to a readable path **inside the container** (for example, a mounted secret).
Use only one key source. Set `HARNESS_COMMIT_SIGNING_PASSPHRASE` when the supplied
key is encrypted. The same Settings import, identity, and registration checks
apply; register the public key on GitHub before starting with signing enabled.
These variables work in `serve`, `run`, and `multiplex` and are read at startup.
The key is copied into the managed signing directory; only metadata is written
to runtime JSON, and the passphrase is never saved. Provisioning a private key
does not enable signing; the saved Settings preference still controls it.

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

### Claude authentication diagnostics

Claude login and tasks share the persistent `CLAUDE_CONFIG_DIR` (default
`$HOME/.claude`). Tasks keep their own temporary home and caches, but do not
copy Claude credentials: a refresh in one task must be visible to the next.
The Docker configuration volume retains the shared session across restarts.

The Claude CLI refuses to start when `.claude.json` is missing from that
directory, whatever credentials are configured. When Claude's own
`backups/.claude.json.backup.*` copies are still present, the harness restores
the newest readable backup before running a task and when reporting auth
status; it never overwrites an existing configuration file, and a first run
with no file and no backups is left to the CLI. If the file cannot be restored
(for example a read-only configuration volume), the task stops at the
authentication stage with the `cp` command to run, instead of failing later
with an opaque agent exit status.

The local UI exposes these debugging endpoints:

- `GET /healthz`: service health.
- `GET /api/agent-auth`: current login state and interactive sign-in challenge.
- `GET /api/agent-auth/diagnostics`: read-only Claude diagnostics, including
  `checks[].code`, severity (`status`), description, suggested next action,
  CLI login status and exit code, login-process state, and credential expiry.
  It excludes tokens, browser codes, login URLs, account identity, and raw CLI
  output, and sends `Cache-Control: no-store`.
- `GET /api/state`: task statuses, exit codes, errors, and `tasks[].logs[].text`.

For example, with the default local port:

```bash
curl -s http://127.0.0.1:7777/api/agent-auth/diagnostics
```

Diagnostic codes include `CLAUDE_AUTH_REJECTED`, `CLAUDE_CREDENTIALS_EMPTY`,
`CLAUDE_ACCESS_TOKEN_EXPIRED`, `CLAUDE_BROWSER_CODE_INVALID`,
`CLAUDE_CONFIG_FILE_MISSING`, `CLAUDE_LOGIN_PROCESS_FAILED`, and
`CLAUDE_STATUS_UNAVAILABLE`.
A successful HTTP response means diagnostics were collected, not that login
is healthy. Inspect the checks. `cli.logged_in` means Claude has a stored login;
it does not prove a refresh or inference request will succeed. An expired access
token with a refresh token is a warning until Claude attempts refresh.

### Local Build

Requires Go `1.26.5` or newer plus `git`, `gh`, and the Codex CLI. Claude tasks
also require the Claude CLI; Codex provides their filesystem sandbox.

```bash
go build -o bin/harness ./cmd/harness
./bin/harness serve
```

Local `harness serve` listens on `127.0.0.1:7777` by default.

With GitHub auth configured, local server mode also watches GitHub review-request
notifications by default. When the authenticated GitHub user is still requested
as a PR reviewer, the harness queues the bundled `code-review` task and posts a
summary comment back to the original PR. Auto-merge is off by default; opt in
with `review_watch.auto_merge: true` in the runtime config.

For a manual `code-review` task, select the PR's head branch or include its PR
URL. The review is read-only and posts back to that PR; a clean worktree is a
successful review, not a reason to queue a repair task. Free-text requests such
as "fix the comments in the review" also use the selected head branch or PR URL
to update the existing PR. If that target cannot be resolved or updated, the
harness reports the targeting error instead of opening a replacement PR.

Post-task PR review cycles are also off by default. Configure
`review_watch.review_level` as `off`, `low`, `medium`, or `high` to run 0 or up
to 2, 5, or 10 review-and-repair passes after a task creates its pull request and
initial checks finish. A pass with no negative findings ends the cycle early.
Actionable findings are repaired and checked before the next pass, including a
clean confirmation after the final repair. If findings remain after the repair
budget is exhausted, the completed task stays successful and its pull request
stays in draft for human review.

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
- `HARNESS_AGENT_HARNESS`: default agent harness.
- `HARNESS_AGENT_COMMAND`: default agent executable.
- `OPENAI_API_KEY`: Codex login bootstrap.
- `HARNESS_DEFAULT_REPOSITORY`: optional repository prefill for Prompt
  Studio; omitted leaves the repository field empty.
- `WHISPER_SPEECH_HOST`: optional speech sidecar host; defaults to
  `faster-whisper`.
- `WHISPER_SPEECH_PORT`: optional speech sidecar Wyoming port; defaults to
  `10300`.
- `WHISPER_SPEECH_LANGUAGE`: optional speech language hint; defaults to
  `en`. Set to `auto` to use Whisper language detection.
- `WHISPER_SPEECH_TIMEOUT_SECONDS`: optional speech transcription timeout in
  seconds; defaults to `120`.
- `WHISPER_SPEECH_DISABLED`: set to `true` to hide prompt dictation, including
  the native keyboard dictation fallback button.

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
