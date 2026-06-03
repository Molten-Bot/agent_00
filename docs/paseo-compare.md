# Paseo Feature Gap Comparison

This compares features described in `docs/paseo.md` against current Molten Hub Code behavior. Ranking favors utility for this app's prompt-to-PR workflow, then ease of adding with existing Go, static web UI, hub dispatch, and GitHub automation patterns.

## Current Baseline

Molten Hub Code already has:

- Local `harness hub` daemon with embedded web UI, local prompt submission, task stream snapshots, task logs, reruns, task controls, library tasks, GitHub repo picker, agent auth setup, hub setup, and review settings (`cmd/harness/main.go`, `internal/web/server.go`, `internal/web/broker.go`).
- Prompt-to-PR execution flow: preflight, auth, workspace creation, clone, generated branch, agent run, commit, push, PR creation, PR screenshot comments, required check watching, and CI remediation (`internal/app/harness.go`).
- Bounded parallel task execution through `harness multiplex` and hub `--parallel` (`internal/multiplex/multiplex.go`, `cmd/harness/main.go`).
- Codex and Claude runtime support, with image prompts only for Codex (`internal/agentruntime/runtime.go`, `internal/config/config.go`).
- GitHub review-request watcher with summary-comment writeback and optional auto-merge (`README.md`, `internal/githubreview`, `cmd/harness/main.go`).
- Prompt dictation through a Wyoming/faster-whisper sidecar (`README.md`, `internal/web/speech.go`).

## Stack-Ranked Gaps

| Rank | Paseo feature this app lacks or only partly has | Utility | Add effort | Why ranked here |
| --- | --- | --- | --- | --- |
| 1 | Structured agent timeline and tool-call model | Very high | Medium | Current UI mostly derives task status from log lines and stores raw task logs. Rich events for tool calls, permission requests, file edits, and attention states would make long PR runs easier to trust and debug. Existing `Broker` snapshots and `/api/stream` give a place to add this incrementally. |
| 2 | Workspace diff and file browser panes | Very high | Medium | App creates local clones and PR branches, but UI does not expose browsable files or diffs. Adding read-only file tree and `git diff` endpoints would directly improve review before PR handoff. Existing static UI and workspace paths in task state make a narrow version feasible. |
| 3 | First-class PR pane with checks, diff stats, review state, mergeability | Very high | Medium | Harness already creates PRs, watches checks, writes review comments, and can auto-merge review tasks. A PR pane would surface data already queried via `gh` instead of burying it in logs. Start with PR URL, checks summary, branch/base, and latest remediation attempt. |
| 4 | Provider registry beyond Codex and Claude | High | Medium | Paseo abstracts Claude, Codex, OpenCode, Copilot, pi, ACP, and custom providers. This app has a compact two-runtime map. Registry metadata for command, package, prompt-image support, and auth posture would make OpenCode/Copilot addition cleaner without changing core harness flow. |
| 5 | Worktree-based parallel workspaces | High | Hard | This app clones per run into generated workspaces. Paseo uses git worktrees, base refs, setup hooks, terminal launchers, and per-worktree services. Worktrees could reduce clone cost and help side-by-side attempts, but would touch workspace lifecycle, branch handling, cleanup, and publish safety. |
| 6 | Loop runner with verifier pass/fail | High | Medium | Harness already does CI remediation loops for PR checks. A generic local loop mode could reuse config loading, agent runs, and task logs to repeat prompt plus verification command until pass/fail. Useful for test-hardening tasks and less broad than full schedules. |
| 7 | Scheduled tasks | Medium-high | Medium | Paseo schedules one-shot or recurring prompts. Molten Hub Code has long-running hub mode and task enqueue paths, so interval schedules are natural. Cron/history/pause/resume raise complexity, but simple persisted interval tasks are tractable. |
| 8 | Scriptable CLI parity for hub tasks | Medium-high | Easy | Current CLI has `run`, `multiplex`, and `hub`, but no small commands for listing tasks, streaming one task, rerunning, stopping, or reading local UI state. Existing HTTP endpoints and `Broker` JSON make `harness tasks list|logs|rerun|stop` easy and useful for automation. |
| 9 | Terminal pane and managed terminal sessions | Medium | Hard | UI renders task logs with xterm, but no interactive shell attach/send-key model. Real terminal multiplexing would require process lifecycle, PTY handling, binary stream routing, permissions, and resize support. Useful, but less central than prompt-to-PR visibility. |
| 10 | Attachments beyond Codex prompt images | Medium | Medium | Config supports prompt images, and task UI can show them. Paseo also handles files, assistant file links, secure downloads, and binary transfer. File attachments would help screenshots/specs, but cross-provider support and storage rules need care. |
| 11 | Desktop launcher packaging | Medium | Medium | `docs/NATIVE_DESKTOP.md` already recommends native packaging and optional thin launcher. This is useful for setup, but current Go binary plus local UI already covers runtime needs. Packaging work is mostly release/product surface, not core agent capability. |
| 12 | MCP server for agents controlling agents | Medium | Hard | Paseo exposes orchestration tools through MCP. Molten Hub Code already has Hub/A2A skill registration and dispatch, so a small MCP facade is possible, but a full safe orchestration contract needs task controls, auth model, and concurrency semantics. |
| 13 | Cross-device E2EE relay and pairing | Medium | Hard | Molten Hub remote transport connects runtime to hosted Molten Hub, but does not expose Paseo-style untrusted relay pairing into the local daemon. High engineering and security cost; less urgent because hosted Hub already covers remote dispatch. |
| 14 | Realtime voice control and local TTS | Low-medium | Hard | App has prompt dictation, but not conversational voice mode, hidden control agents, local TTS, or voice readiness state. Useful for accessibility and mobile-style control, but much less tied to prompt-to-PR completion than visibility and PR workflows. |
| 15 | Browser preview panes and per-workspace service proxy | Low-medium | Hard | Broker detects live app/preview URLs in logs, but app does not manage per-workspace services or reverse-proxy them. Valuable for UI tasks, but requires port allocation, process supervision, and proxy routing before browser panes make sense. |

## Best Easy Adds

1. `harness tasks` CLI commands.
   - Use existing local HTTP API: `/api/state`, `/api/stream`, `/api/tasks/{id}/...`.
   - Start with `list`, `logs`, `rerun`, `stop`.
   - Utility: makes hub mode scriptable without new daemon architecture.

2. Provider registry metadata.
   - Extend `internal/agentruntime` definitions with display name, package, image support, auth mode, and UI label.
   - Utility: prepares OpenCode/Copilot/custom-command support and reduces one-off auth/UI branching.

3. PR summary pane.
   - Add a task detail section that shows PR URL, branch, repo, latest checks state, and remediation attempt count.
   - Utility: high because existing harness work centers on PR outcome.

4. Read-only diff endpoint and UI tab.
   - Add endpoint for `git diff --stat` plus bounded patch text for selected task workspace.
   - Utility: lets user inspect changes before/after PR creation with limited write risk.

5. Generic loop mode.
   - Add a config wrapper with prompt, verification command, max attempts, and optional verifier library task.
   - Utility: reuses existing CI remediation mental model for local checks.

## Best High-Utility Bets

1. Structured task timeline.
   - Best product payoff. Current log parsing works, but timeline events would support better UI, richer Hub activity, and future permissions/tool-call views.

2. PR pane plus diff pane.
   - Best fit for prompt-to-PR identity. These make the app feel less like a log monitor and more like a shipping cockpit.

3. Worktree/service model.
   - Biggest capability jump for parallel UI/product work, but should wait until workspace cleanup, branch policy, and service supervision design are explicit.

## Features Paseo Has But This App Partly Covers

- Self-hosted daemon: covered locally by `harness hub`, but Paseo has longer-lived session persistence and richer client protocol.
- Multi-provider runtime: partly covered by Codex/Claude only.
- Cross-device access: partly covered by hosted Molten Hub transport, not Paseo-style direct daemon clients with pairing and E2EE relay.
- Workspace UI: partly covered by task dashboard, log console, repo picker, library prompts, and review settings; missing file/diff/browser/terminal panes.
- Voice: partly covered by dictation; missing realtime voice control and TTS.
- GitHub/PR workflow: strongly covered in automation; missing rich UI panes for PR metadata.
- Attachments: partly covered by Codex prompt images; missing general file transfer and assistant file links.
