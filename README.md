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

Open **Settings → Libraries** to add a private library repository
and choose its name. Its tasks appear under that source name. Explicit task
`displayName` values are preserved; otherwise titles derive from the source and slug.

**Settings → Features → Bundled library** controls built-in task availability.
Custom tasks remain available. Workflows resolve prompts from the currently
available catalog; an unavailable task produces an explicit activation failure.

### Recurring library tasks

Create custom schedules in **Workflow** with a **Scheduled task** template, or
use the schedule shortcut beside a selected Library task to open a workflow
draft carrying its repository, prompt context, branch, directory, reviewers,
and draft-PR option. **Run** in Library still launches a one-time task.

Select the **Agent task → Prompt source** to use inline instructions or a library
prompt. Library prompts work with GitHub triggers as well as scheduled triggers.
Their current definition is expanded at each activation; the resolved prompt is
recorded with that task so later library edits do not change an existing run.
Optional context extends the library prompt. Required context and non-default
branch requirements are checked before enabling a workflow and again at execution.

A scheduled trigger repeats every **1 minute–365 days**. Drag the unit scale
(minute, hour, day, week, month), then enter the repeat count. Months are fixed
30-day intervals. An optional first-run date and time is also available. The editor uses your
local timezone and saves an absolute timestamp. Without a first-run time, the
first occurrence is due one interval after enabling. Changing the interval or
first-run time resets the cursor; prompt and layout edits preserve it. Scheduled
triggers run directly against the repository without issue/PR filters, then use
the same review and merge stages as other workflows.

Keep the native app running; due work is checked every 30 seconds. Missed
intervals coalesce into one catch-up run. Scheduled work waits while that
repository has an active task. A failed submission is recorded and retried at
the next interval. Pausing preserves the cursor; disabling the Workflow feature
or excluding the repository prevents new occurrences and merges.

Create a scheduled workflow to replace any legacy `config.json` library
schedule. Legacy schedules are left on disk for reference.

### GitHub issue automation

Enable **Settings → Features → Workflow (beta)** and open Workflow in the bottom
navigation. Create an **Implement issues** workflow for a selected repository,
choose its trigger and filters, save the draft, then explicitly enable it.
Repositories may have no workflows or multiple independent workflows. The GitHub
Issues feature controls the issue browser separately from workflow execution.

Workflow polling checks every 30 seconds. Existing matching items are skipped on
the first scan unless **Include existing matches** is enabled. Workflow stores
start empty, and existing saved workflows retain their settings.

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
survive startup. Maintenance runs at startup and during issue recovery.
Once the latest claim is verified as fulfilled, maintenance removes checkouts and
regenerable caches from both `issue-workspaces/` and the older
`issue-queue/issue-workspaces/` layout. This includes superseded failed and
stopped attempts. Active workers, blocked or retryable work, explicit resume
targets, and pending verification, review, or repair keep their workspaces.
A stopped task with unresolved issue evidence remains available for recovery.
Task logs and review receipts expire 30 days after fulfillment. Claim and repair
journals remain available for audit. The shared daemon log rotates at 64 MiB
during maintenance and keeps one previous file. Cleanup reports reclaimed bytes,
protected workspaces, and errors in the daemon log. Maintenance retries failed
passes independently of delivery. Workspaces without a matching claim remain
available for operator inspection.

The local recovery protocol uses inherited Unix file locks and records process
groups. A surviving agent or uncertain process termination prevents takeover.
Legacy claims and ownership changes require **Retry** in **Settings → GitHub Issues**, after stopping
the previous worker on every instance. Automatic cross-machine failover is not
performed. Restarting the owning instance recovers durable dispatch intents for enabled workflows.

Background recovery verifies PR delivery for existing claims. An enabled workflow
can retry an interrupted or failed run, reuse preserved local changes and remote
commits, and resume publication once the journal confirms agent success. Other
claims require explicit retry. Legacy journals without this checkpoint rerun the agent before
publishing, because pending changes alone do not prove a successful result.
It permits three agent attempts per failure cycle. Consecutive unsuccessful
runs increase the retry delay fivefold, starting at one minute plus jitter and
capped at one hour by default. The count follows automatic retries, including
runs that fail before starting the agent or while publishing a PR. Admission
and infrastructure failures also back off and honor GitHub retry/reset headers.
The local `/api/github/automation` endpoint exposes the run and recovery failure
counts and next retry time for each claim. Auth and signing gates do not consume
agent attempts. Workspace storage failures do not consume agent attempts or
workflow retries. Exhausted attempts, explicit stops/pauses, and no-change results
without a PR remain visible blockers requiring review or explicit retry.
Verified PR delivery ends the failure streak. A manual recovery starts a new
failure cycle and refreshes issue content.

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

Where GitHub issue statuses are available, automatic workflows pick up Todo
issues. Studio defaults to Todo and has an All open filter for manually starting
backlog issues. An admitted issue moves to In progress, then In review once its
pull request is published. With issue auto-close enabled, a successfully delivered
and merged pull request moves the issue to Done. Studio keeps retries and stage
history on the originating issue task.

Repository rules must permit creation of ownership tags, branches, and issue
labels. Permission or storage failures remain explicit blockers. Progress cannot
be guaranteed during a permanent outage or loss of persistent storage; unresolved
issues are never intentionally treated as successful delivery.

Upgrade **all** instances before enabling backlog processing: older versions
ignore these ownership rules. Automation stays off unless already enabled, but
an enabled installation will scan its backlog after upgrading.

### Workspace storage

PR reviews and runs with post-task review passes use `HARNESS_WORKSPACE_DISK_BASE`
(default `/tmp`) unless a durable workspace root is pinned. Keep review storage
outside `/dev`: native read-only sandboxes replace that mount, hiding checkouts
under `/dev/shm`. Reviews retain their read-only filesystem restrictions.

Retained task checkouts need disk capacity. For long-running Docker installations,
mount a persistent volume at the configured workspace base. Do not point both
`HARNESS_WORKSPACE_RAM_BASE` and `HARNESS_WORKSPACE_DISK_BASE` at the same small
tmpfs: accumulated tasks can fill it and prevent even `git clone` from starting.
Linux workspace allocation checks for at least 512 MiB free on each allocation
and falls back to the configured disk base when the preferred base is full.
This reserve is an early check, not a limit on an individual build's disk usage.
The daemon records the largest workspace size observed at task completion for each
repository or combination of repositories. It stores these estimates by repository
hash in `.<runtime-config-filename>.task-disk-usage.json` beside the runtime config.
Tasks without a history reserve 512 MiB. When available space cannot cover a
task's estimate alongside other reservations on the same filesystem, the task
stays queued until space becomes available. Force start overrides this admission
check.

Issue workspaces live under `issue-workspaces/` beside the runtime config. With
default Compose settings, they use the host disk behind
`${AGENT_00_CONFIG_DIR:-./.agent00}:/workspace/config`. A nested mount takes
precedence over that bind mount and over `HARNESS_WORKSPACE_DISK_BASE`. If an
older deployment or Compose override mounts `/workspace/config/issue-workspaces`
as tmpfs, stop task execution and copy retained workspaces to the host config
directory before removing the override and recreating the container. Keep
the same directory layout so saved claims can find their workspaces.

The daemon logs issue workspace capacity and free bytes at startup and when
queueing an issue. On Linux it rejects tmpfs and ramfs for issue workspaces.
Tasks wait for enough free space on their filesystem. The pre-staging check
measures the checkout path itself. A nested mount or filesystem quota can report
less space there than the host reports elsewhere. The 512 MiB check measures
available filesystem space, and the scheduler also accounts for concurrent
reservations. For staging failures, compare free space on the checkout mount
with host-wide disk readings. Force start can override the historical estimate,
and it still requires at least 512 MiB free for issue workspaces. If a task hits disk exhaustion, it remains blocked with its
workspace retained. Free disk space or expand the workspace filesystem, then
select **Retry** in **Settings → GitHub Issues**. Preserve unresolved checkouts
and journals when freeing space. If the claim journal is also full, keep the
daemon running while freeing space so it can save the blocked outcome.

#### Memory pressure during Go or browser validation

One worker can run several Go compiler, Node, and Chromium processes at once.
On Linux, the daemon logs `host_memory_pressure` when it defers normal worker
admission, workflow scans, issue reconciliation, and PR monitoring. On hosts
with at least 2.25 GiB RAM, it pauses at or below 128 MiB available, or at or
below 512 MiB available with at most 128 MiB free swap. It resumes on normal
ticks once 768 MiB is available, even if swap stays full. Smaller hosts use
thresholds scaled to installed memory. Running workers can finish and record
their outcomes. Deferred polls leave claims, retry counts, and review evidence intact.
A GitHub command logged with `context deadline exceeded` reached its command
or scan deadline. Workflow commands have a 45-second deadline. Issue
reconciliation and PR state lookups have 30-second deadlines. Correlate kill
messages with deadlines and kernel OOM events before attributing them to memory
pressure.

Sample while validation is running and again after recovery. Use the workspace
path from the `stage=workspace` log and the service environment for any
preinstalled browser path.

```sh
grep -E '^(MemAvailable|SwapTotal|SwapFree|Shmem):' /proc/meminfo
df -hT /tmp /dev/shm "$task_dir"
du -sh "$task_dir"/.agent00-agent-io/cache/* "$task_dir"/.agent00-agent-io/tmp
ps -eo pid,ppid,rss,comm --sort=-rss | head -20
```

`Shmem` includes tmpfs and other shared memory. Use `df` to see which mounts
consume space and `du` to locate generated caches on each mount. Use PID and
PPID to identify task descendants. Shared pages can appear in several RSS
values, so summing RSS can overcount. Check the mount and size of
`PLAYWRIGHT_BROWSERS_PATH` separately for a preinstalled browser. For a
container with its own memory limit, also check `memory.current`,
`memory.max`, and the OOM counters in `memory.events` in its cgroup v2 directory.

Set both `HARNESS_WORKSPACE_RAM_BASE` and `HARNESS_WORKSPACE_DISK_BASE` to
disk-backed locations for sustained Go and browser work. Verify with `df -T`
because `/tmp` can be tmpfs too. Keep any downloads moved out of stopped
workspaces on disk. The bundled image already supplies Go and Playwright
Chromium. For native installs, provision the repository's Go version on the
service PATH and set `PLAYWRIGHT_BROWSERS_PATH` to a disk-backed installation
matching the Playwright package version. These trusted tools can be reused
across tasks. Each task keeps its own writable Go build and module caches.
Use `go test -p 2` or Playwright's `--workers=1` when validation needs less
process concurrency on a small host.
Keep active workspaces, unresolved checkouts, journals, and logs in place
when releasing generated data from stopped workers.

### Agent sandbox

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

Edit **Default execution** in the Workflow tab to set the review fix-pass budget
and reviewer complexity for Prompt, Chat, JSON, Library, and manual GitHub Issue
starts. New installations and older definitions without this policy start with
one fix pass and a Standard reviewer. Set 10 passes and Easy for delivery smoke
tests. Tasks and issue claims retain the effective policy they captured.
Enable **Custom workflow execution (beta)** in Settings → Automation to run
repository workflows. Automatic merge still requires its separate workflow gate.
The React Flow editor defines an executable pipeline: repository → trigger →
filters → task → bounded review cycles → optional merge. Select a node to edit
its policy and drag nodes to arrange the canvas. Connections describe this
pipeline; arbitrary branches or custom executable node types are not supported.
Valid workflow settings save automatically, including when you leave the editor.
Incomplete changes leave the last saved version in place. Moving nodes alone
does not trigger a save. Enable and Pause control whether the workflow runs.
Preview matches is read-only.

The `app.prompt`, `app.json`, `app.chat`, and `app.library` triggers customize
single-repository submissions from their respective app sources. Enable at most
one matching workflow per repository and source. A matching workflow adds
instructions and routing. The effective policy uses the higher fix-pass budget
and reviewer complexity, with instructions from both policies.

Choose a repository and a **Workflow template** to create an editable, disabled
workflow. Templates cover labeled or assigned issues, requested PR reviews,
requested changes, failed checks, scheduled tasks, issue-to-merge, and
review-and-merge. Merge templates require successful checks and human approval;
review-and-merge also requires a clean agent review. Explicitly enable
each workflow when ready. Multiple copies can target the same repository.
Templates are independent copies: editing one does not change other workflows.
The versioned catalog is `internal/web/workflow/src/templates.json`; its definitions
are checked against the backend workflow schema in tests.

Definitions live in **`workflows.json` beside the runtime `config.json`** (normally
`.agent00/workflows.json`). Export downloads this same portable format. Version 2
uses camelCase fields with top-level `version`, `revision`, `defaultExecution`,
and `workflows`.
IDs remain stable when renaming a workflow. Saves use atomic replacement and
revision checks; stale browser writes receive HTTP 409. Unknown fields, invalid
policies, and unsupported future versions fail closed without overwriting files.
Version 1 definitions are read compatibly and upgraded on save; the original file
is retained as `workflows.json.v1.bak`. Unsupported future versions are rejected.
An empty `workflows` array is valid. Keep exported definitions in your repository
if you want Git history; credentials and runtime history do not belong in them.

Agents can manage workflows through the running app's local HTTP API. Read
`GET /api/workflows` to get the current revision and definitions. Submit the
updated document to `PUT /api/workflows` with that revision to create, edit,
enable, pause, or remove a workflow. On HTTP 409, read again and reapply your
change to the newer document. Send a single definition to
`POST /api/workflows/preview` to check matches before enabling. Use
`GET /api/workflows/schedules` for schedule status and `GET /api/state` for task
activity. `POST /api/workflows/scan` queues an immediate poll and returns HTTP
202. Polls use the usual feature and repository gates, trigger deduplication,
and schedule due times. Overlapping scan requests can share a polling cycle.

```json
{
  "version": 2,
  "revision": 0,
  "defaultExecution": {"reviewCycles": 10, "reviewAgent": {"complexity": "easy"}},
  "workflows": [{
    "id": "implement-ready-issues",
    "name": "Implement ready issues",
    "repository": "acme/project",
    "enabled": false,
    "includeBacklog": false,
    "trigger": {"event": "issue.labeled", "value": "ready-for-agent"},
    "filters": {"mode": "all", "rules": [
      {"field": "assignee", "operator": "is", "value": "@me"},
      {"field": "label", "operator": "isNot", "value": "blocked"}
    ]},
    "task": {
      "kind": "implement",
      "instructions": "Implement the issue and run relevant tests.",
      "assignCreator": false,
      "issueLinkMode": "close",
      "draft": false
    },
    "reviewCycles": 2,
    "merge": {"enabled": false, "method": "squash", "requireApproval": true, "deleteBranch": false}
  }]
}
```

Supported triggers: scheduled intervals, issue opened/assigned/labeled and PR opened/assigned/labeled,
ready, review requested, changes requested, or failed checks. Assignment and
review-request triggers accept a GitHub login or `@me`; label triggers take a
label. Filters support all/any groups, exact case-insensitive is/isNot comparisons,
labels, authors, assignees, bots, issue types, PR reviewers, base branches, drafts,
and linked issues. Boolean values use `"true"`/`"false"`. Linked issues means
GitHub's closing-issue relationships. Triggers are observed state transitions,
not a webhook event replay: changes that occur entirely between polls can be
missed, and a match must become false before it can activate again. For GitHub triggers, definition
policy edits establish a new baseline; renaming and moving nodes do not.

To react to a failed GitHub Actions workflow, choose **GitHub Actions failure**
and enter the workflow filename (for example, `deploy-prod.yml`) or numeric ID.
In JSON, use `"trigger": {"event":"action.failed","value":"deploy-prod.yml"}`.
Configure an implementation prompt or library task. The app polls the latest 100
runs of that workflow with the configured GitHub account. Each completed
`failure` attempt can activate once. Existing failures are baselined unless
`includeBacklog` is enabled. Failures on `agent00-` work branches are skipped to
avoid triggering repair work from its own checks. The task receives the run URL,
attempt, head branch, and commit for diagnosis, and opens any fix from the
configured task base branch or repository default.

Issue tasks retain durable claims, reserved branches, fresh issue comments, and
recovery. PR tasks either review and comment or repair the existing head branch.
The default allows 1–10 repair passes, plus a final confirmation review when
the last pass makes a fix. Review-only tasks perform one review. Merge supports squash/merge/rebase and optional branch
deletion. It waits for checks, a non-draft PR, no requested changes, and optional
human approval from an owner, member, or collaborator on the current head, then binds the merge to the observed head commit. PRs with no
checks remain waiting. Review writeback success alone never authorizes merging:
a review must explicitly report a clean result. GitHub branch protection applies.

Scheduled definitions use `trigger: {"event":"schedule","intervalHours":24}`
and optionally a `startAt` RFC 3339 timestamp. Agent steps reference a prompt with
`task.libraryTaskName`; `task.instructions` then supplies additional context.
`task.baseBranch`, `targetSubdir`, and `reviewers` retain per-workflow task options.

`workflow-state.json` is private runtime state for schedule cursors, trigger deduplication,
and work that still needs dispatch recovery or merge coordination. The app removes
finished activations from this file during normal processing, including older records
on the first poll after upgrade. Keep the file on restart. A dispatch interrupted before its receipt is known
is reconciled from task history and otherwise held for inspection, never blindly
repeated. The global repository filter gates task admission and execution; commit
signing remains global. Turning off Workflow stops custom activations and merges,
while already started tasks retain their captured policy and can finish.

In Settings → Features, **Default task complexity** starts at Standard. Each workflow’s
Agent task can inherit that setting or select Easy, Standard, or Advanced. The
server captures the effective level when it queues a task and reuses it for
retries. A standalone run config can also specify `"complexity": "advanced"`.

Codex queries the installed CLI’s app-server model catalog with the task environment.
Standard uses the recommended model. Easy favors a model offering lower reasoning
effort and Advanced favors one offering higher effort. Among models offering the same effort, the catalog’s descriptions of simple or
complex tasks take priority, followed by its recommended model. Image tasks require image support when the catalog reports input
modalities. If discovery is unavailable, Codex uses its configured CLI default.
An empty compatible catalog or an explicit provider rejection fails the task.

The Claude Code CLI uses the provider’s configured Haiku, Sonnet, or Opus model
for Easy, Standard, or Advanced respectively. The standalone CLI integration
uses the corresponding CLI alias as its fallback when no provider model override
is set. The alias can resolve to different versions across accounts and providers.
Expanded task details show the requested level, model identifier passed to the CLI,
CLI version, and resolution source. For Codex’s default fallback they show “CLI
default”. A rejected model surfaces the CLI’s error in the task log.

The isolated editor source is in `internal/web/workflow`. To update its checked-in
Go-embedded assets, run:

```bash
npm ci --prefix internal/web/workflow
npm test --prefix internal/web/workflow
npm run build --prefix internal/web/workflow
go build -o bin/harness ./cmd/harness
```

The editor is loaded only when Workflow opens. No Node server or runtime CDN is
required. Generated assets and dependency license notices live in `internal/web/static`.

#### Workflow smoke tests

`scripts/workflow-smoke/matrix.json` lists small live scenarios for every workflow
template, including fixture setup, expected outcomes, and merge prerequisites.
Give each testing agent its own prefix and an explicitly designated disposable
repository. The runner uses the running app's API and emits a JSON report with
`pass`, `fail`, and `blocked` results:

```bash
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a --action install
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a --action enable --case labeled
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a --action scan
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a --action observe
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a --action pause --case labeled
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a --action cleanup
```

The default check validates previews and rejected inputs without dispatching.
Install creates disabled copies and verifies revision conflicts while preserving
existing definitions. Use the fixture steps in the matrix and enable individual
cases with `--case`. Scan queues an app poll. Observe reads recent monitor
events. Inspect the PR and checks to verify changes and CI outcomes.
Cleanup removes only unchanged disabled copies. Save private reports outside the
repository with `--output /tmp/workflow-report.json`. Review-request and human
approval scenarios need a second GitHub identity because authors cannot approve
their own PRs. Keep those scenarios marked blocked when that prerequisite is
unavailable rather than weakening their approval policy.

For Prompt, JSON, and Chat, install the definitions and enable one app case.
Use the same disposable repository and prefix for each command. The explicit
`submit` action queues one real task and waits up to 30 minutes (`--timeout`
in seconds). For example, after install:

```bash
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a --action enable --case app-json
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a --action submit --case app-json
python3 scripts/workflow-smoke/run.py --repo example/workflow-tests --prefix smoke-agent-a --action pause --case app-json
```

Use `app-prompt` or `app-chat` for the other sources. The submit report checks
the returned request ID against the workflow event and task, then uses the
authenticated `gh` CLI to verify the unique marker file in that task's PR.
Disabled Chat or JSON settings produce a blocked result before submission.
The observe action reports app cases as blocked and points to the submit action
for request-specific verification.

Shared browser libraries (Lucide, Marked, xterm, and Emoji Mart) are pinned in
`internal/web/vendor/package.json` and its lockfile. After updating those pins, run
`npm ci --prefix internal/web/vendor` and `npm run build --prefix internal/web/vendor`
to refresh their vendored browser assets and license notices. Neither frontend
package directory is required at runtime.



The web UI sends manual GA4 page views and engagement events. Keep Enhanced
Measurement disabled on its GA4 web stream to prevent duplicate history-based
page views and automatic collection of raw link and form URLs.

To review PRs automatically, enable a workflow with the `pr.review_requested`
trigger for the repository.

For a manual `code-review` task, select the PR's head branch or include its PR
URL. The review is read-only and posts back to that PR; a clean worktree is a
successful review, not a reason to queue a repair task. Free-text requests such
as "fix the comments in the review" also use the selected head branch or PR URL
to update the existing PR. If that target cannot be resolved or updated, the
harness reports the targeting error instead of opening a replacement PR.

Set a workflow's `reviewCycles` to run review-and-repair passes after a task
creates its pull request and initial checks finish. Each pass starts a fresh
agent against the current PR. Select the **Review agent** node to add review
instructions or choose a separate complexity. Exported workflows store these
options as `reviewAgent.instructions` and `reviewAgent.complexity`. When review
complexity is unset, the reviewer uses the agent task's complexity.

A pass with no negative findings ends the cycle early. Actionable findings
are repaired and checked before the next pass, including a clean confirmation
after the final repair.
If findings remain after the repair budget is exhausted, the completed task
stays successful and its pull request stays in draft for human review.

#### Published review gate

Workflow reviews, PR repairs, and implementation runs with review cycles need
a submitted GitHub review on the exact PR head before automated merge. A clean
review is posted with the `COMMENT` event, a structured verdict, and “No material
issues found.” The task workspace records the review ID, head, run, pass, selected
policy, and result. The server archives the outcome in `review-receipts` next
to its runtime configuration before workspace cleanup. The review agent
returns a structured code verdict. The harness publishes one native review per
completed pass with writeback enabled and reconciles lost responses against
submitted reviews. Publication failures are reported as harness errors.
Conversation comments provide feedback only.
Human approval, when required, remains a separate requirement.

Code findings on the diff are submitted as review threads. Findings that
require a branch or history repair outside review scope leave the gate blocked.
Workflow repairs carry the original native findings into the repair prompt,
even when a different activation published them. Every PR repair workflow runs
an independent confirmation pass on the PR head, including when its fix-pass
budget is zero. A clean pass can resolve an app-authored thread from an earlier
head if it verified the specific fix and the original comment has no replies
or changes since verification. The workflow retains the clean receipt and
resolution evidence if GitHub writeback is interrupted. Before readiness and
merge, the harness rechecks the head, submitted review, change requests, and
every page of conversations, including outdated threads. A changed head needs
a fresh clean review. The task UI shows the review link and pending reason.

##### Enforce it for external clients

1. Install a dedicated GitHub App with Checks write and Pull requests read.
   Keep its installation credential in the trusted harness environment and
   refresh it before expiration. Give the review writer Pull requests write.
   Keep both the publisher and merge identities off branch bypass lists.
2. On the target branch, activate a ruleset requiring pull requests, resolved
   conversations, and the `agent00 / review-policy` status check. Select the
   dedicated GitHub App as the check source. Keep existing CI and human approval
   rules. The implementation supports direct merge, without a merge queue on
   this branch.
3. Set `HARNESS_REVIEW_CHECK_REPOSITORIES` to space-separated `owner/repo`
   values, `HARNESS_REVIEW_CHECK_APP_ID` to the app's numeric ID, and
   `HARNESS_REVIEW_CHECK_TOKEN` to its installation token. Agent_00 verifies
   the active target rules and publishes the check only after verifying the
   native review and conversations.
4. With repository or organization administrative access, verify active target
   rules with `gh api --paginate "repos/$repo/rules/branches/$branch"` (URL-encode
   branch names with slashes). For each applicable ruleset ID, view its
   `bypass_actors` in Settings → Rules → Rulesets or with
   `gh api "repos/$repo/rulesets/$ruleset_id?includes_parents=true"`.
   Confirm that the check's `integration_id` is the installed app ID and that
   the bypass list is empty or contains only explicitly authorized operators.
   Verify the actor type, ID, and bypass mode. An omitted list means the
   inspecting credential cannot verify bypass. Authorized bypass is separate
   from passing the gate and follows GitHub's ruleset audit behavior.

A required check applies to every PR targeting the ruleset. Route unrelated PRs
through a standalone Agent_00 review after their authors are ready, or scope
the ruleset to a dedicated automation target branch and route ordinary PRs to
an unscoped branch. GitHub checks attach to commits. Use distinct head SHAs for
independently reviewed PRs. A controller should also verify the published
review belongs to its PR and poll the head-bound check before requesting merge.

[GitHub rules API](https://docs.github.com/en/rest/repos/rules) ·
[GitHub Checks API](https://docs.github.com/en/rest/checks/runs) ·
[GitHub pull request review API](https://docs.github.com/en/rest/pulls/reviews)

##### App-managed verification

Run the local integration tests to exercise the review agent selection, native
writeback, workflow restart, and app-owned merge command against simulated
GitHub responses.

```sh
go test ./internal/app -run 'TestAppManagedMergeAfterIndependentReviewAndRepair|TestProcessChangedRepoReleasesPRFromDraftAfterCleanFinalReviewCycle|TestStandaloneReviewRequiresNativeReceipt'
go test ./internal/workflow -run TestImplementationMergeRequiresPersistedReceiptOnCurrentHead
```

The delivery smoke case uses 10 review cycles and the `easy` reviewer preset.
In an app-managed smoke run, verify that green CI, a submitted clean review,
and zero unresolved threads lead to one `gh pr merge --match-head-commit`
call. The `review-merge` case exercises optional human approval separately.

##### Disposable repository verification

Use an explicitly designated disposable repository and a normal merge identity.
Set one required CI job, the rules above, and the dedicated app. Save command
results, including the merge exit status and head SHA, for each case.

1. Open a draft with green CI and no submitted review. Mark it ready from a
   separate client. `gh pr merge --squash --match-head-commit "$head"` must fail
   because the required review check is missing.
2. Run a standalone Agent_00 review with automatic merge off against head A.
   Verify the submitted review ID, its `commit_id`, and the app's successful
   check from the PR’s Checks tab or GitHub APIs. On one fixture, merge A
   with the same command. On a second fixture, push B before merging. The A
   precondition must fail, and B must remain blocked until fresh evidence exists.
3. On a separate fixture with an explicitly authorized bypass actor, repeat
   the green-CI, missing-review attempt as the normal identity. Then use
   `gh pr merge --admin --squash --match-head-commit "$head"` as the authorized
   admin. Record the result and the matching ruleset bypass actor. A permitted
   bypass is an external merge with the review gate still incomplete.

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
