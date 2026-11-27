# AGENTS.md

## Scope

- These instructions apply to the whole repository. A nested `AGENTS.md` closer to the edited file takes precedence for that subtree.
- This file is repository-local guidance for agent_00. The separate `library/AGENTS.md` file is the runtime seed copied into task workspaces for agents working on customer repositories.
- Keep agent instructions evidence-backed. Add commands, paths, and conventions only when they are present in source, tests, docs, Dockerfiles, or CI.

## Project Overview

- agent_00 is a Go module and CLI for running AI coding agents against GitHub repositories and turning prompts into pull requests.
- The executable lives in `cmd/harness`. Shared behavior belongs in focused `internal/*` packages: `app`, `agentruntime`, `config`, `execx`, `localruntime`, `library`, `multiplex`, `slug`, `web`, and `workspace`.
- The local web UI is served from embedded files under `internal/web/static`; the Workflow editor has an isolated npm build under `internal/web/workflow`. Its generated assets are checked into `internal/web/static`, so ordinary Go builds do not require Node.

## Commands

- Use Go `1.26.5` or newer, as declared in `go.mod`.
- Match CI before finishing broad changes:
  - `go mod download`
  - `go build ./...`
  - `go build -o bin/harness ./cmd/harness`
  - `go test ./...`
- For Workflow editor changes, run `npm ci --prefix internal/web/workflow`, `npm test --prefix internal/web/workflow`, and `npm run build --prefix internal/web/workflow`; commit the generated editor assets with their source.
- For focused validation, prefer the smallest relevant package or test first, for example `go test ./internal/config` or `go test ./internal/app -run TestName`, then broaden when the change has wider impact.
- Build and run the local harness with `go build -o bin/harness ./cmd/harness` and `./bin/harness serve`.
- CLI modes are `harness run --config <path-to-json>`, `harness multiplex --config <path-or-dir> [--parallel <n>]`, and `harness serve [--init <path-to-init-json> | --config <path-to-config-json>] [--parallel <n>] [--ui-listen <host:port>] [--ui-automatic]`.

## Workflow Hardening

- At the start of each hardening session, run a controlled end-to-end synthetic in the disposable `Molten-Bot/test_everything` repository before drawing down prioritized `agent_00` issues. The required synthetic is GitHub issue → task → PR → review → ready → app-driven merge. Use a dedicated synthetic issue and a one-line change to a fake fixture file. Keep unrelated `test_everything` workflows disabled so old schedules or labels do not create stray work.
- Configure the synthetic workflow for one review fix pass, an effective Easy reviewer, and automatic merge without human approval. Confirm the saved task policy actually resolves to Easy; the global default execution policy can raise a workflow's requested reviewer complexity. Require successful checks, a clean review on the current head, resolved review threads, and successful draft verification before readiness and merge. Do not manually mark the PR ready or merge it and count that as a pass. Verify the app's terminal merge receipt as well as GitHub's merged state; `mergedBy` alone is inconclusive because the app uses the operator's GitHub credentials.
- For every task linked to a PR, observing that PR in GitHub's merged state is terminal task success (green), regardless of who merged it. Stop retries and release task reservations once the merge is confirmed. Keep any unfinished draft-verification or review evidence in history only; it must not leave the task queued, failed, or awaiting review. Record separately whether the app or an operator performed the merge.
- Keep a small synthetic matrix for the other basic entry points: Prompt, Chat, JSON run config, Library, and manual GitHub Issue start. Exercise each at least once per hardening session, and rerun the affected entry point whenever its routing or workflow policy changes. Use only one-line fake-fixture edits. Include a PR review/repair continuation so retries are shown to retain one task identity and the same PR. After the synthetic changes merge, run the cleanup fixture and verify the disposable repository returns to its original tree.
- Record each synthetic's entry point, issue/task ID, PR number and head SHA, captured/effective workflow policy, checks, review result, resolved-thread count, readiness, app merge receipt, cleanup result, elapsed stage times, and any retry or manual intervention. Mark a synthetic failed or blocked if the app needs a manual ready/merge action; do not hide that intervention in the result.
- After the synthetic suite, draw down prioritized `agent_00` issues with up to three workers when the workflow is repeatable and the host has enough disk and memory. Treat queued tasks as waiting, not running. Respect disk admission; do not force a task past the disk-safety gate. If capacity allows only one or two workers, keep the remainder queued and report the reason.
- If a failure is new or the cause is unclear, spawn one Sol subagent to investigate. Give it the failing stage, expected behavior, relevant commit and check results, and sanitized evidence. Wait for its findings, verify them, then search open and closed GitHub issues for duplicates with `gh issue list --state all --search`. Update an existing ticket when it covers the failure. File a new issue with `gh issue create` when needed. Include the observed failure, reproduction, expected result, and a way to verify the fix. Assign it to the requesting operator and apply the repository's priority label.
- Prioritize delivery blockers over latency. Record elapsed time for queue admission, implementation, PR publication, CI, review, repair, verification, and merge. Record the terminal outcome and any retry, restart, or manual intervention. When a check or PR head is temporarily unavailable after a push, confirm the current GitHub state before treating it as a code failure.
- Let the app implement, publish, review, repair, and merge. Use its retry or recovery controls to resume a retained claim or draft and preserve the captured workflow policy. If the app cannot finish a step, explain the blocker before intervening and record which steps the app completed.
- In the full delivery hardening test, use 10 review cycles and the effective Easy reviewer. Expect real GitHub reviews on each reviewed commit. Verify that the app resolves only findings it confirmed are fixed and merges only after a clean review on the current head, all threads are resolved, required checks pass, and applicable draft verification succeeds. Keep this longer run distinct from the one-pass synthetic smoke.
- When the app is idle after a merge, fast-forward local main, rebuild, and restart with the matching runtime files so the next run exercises the landed code. Honor requests to stop admitting work and record the final state of any open draft or retained claim for the next session.

## Architecture

- Keep CLI argument parsing and terminal concerns in `cmd/harness`; keep reusable orchestration in `internal/app`, local runtime configuration and scheduling in `internal/localruntime`, and agent-specific command construction in `internal/agentruntime`.
- The main harness flow is preflight/auth/workspace/clone, agent runtime execution, git commit/push, PR creation, and optional CI remediation. Preserve that staged logging style when extending the flow.
- Prefer dependency injection over process globals for behavior that tests need to control. Existing patterns include injectable runners, clocks, sleep functions, filesystem callbacks, and HTTP test servers.
- Use `internal/execx.Runner` or `StreamRunner` for subprocess work in higher-level packages instead of direct `os/exec` calls.
- Keep package boundaries small and explicit. Avoid introducing cross-package cycles or broad utility packages when an existing focused package already owns the behavior.
- Use explicit error messages with context. Preserve existing sentinel errors and exit-code behavior when changing harness failure paths.

## Config And Contracts

- Run config JSON uses camelCase fields such as `repoUrl`, `targetSubdir`, `agentHarness`, `agentCommand`, `responseMode`, `libraryTaskName`, `githubHandle`, and `prTitle`. Snake_case run config fields are rejected. JSONC-style line comments are supported.
- Supported agent harnesses are `codex` and `claude`; default is `codex`. Both harnesses accept prompt images. Honor `HARNESS_AGENT_HARNESS` and `HARNESS_AGENT_COMMAND`.
- Supported response modes are `default`, `off`, `caveman-lite`, `caveman-full`, `caveman-ultra`, `caveman-wenyan-lite`, `caveman-wenyan-full`, and `caveman-wenyan-ultra`; omitted or `default` maps to `caveman-full`.
- Library task definitions live in `library/*.json`, use strict camelCase task fields, reject unknown fields, and should keep one top-level task per checked-in file.
- `internal/customlibrary` lets a company pull its own library tasks from a private git repo's `./prompt/*.md` files (frontmatter plus body, same fields as the JSON task schema). `internal/library.LoadCustomizedCatalog` merges these into the catalog from `library.CustomSourcesRoot()` (override with `HARNESS_LIBRARY_CUSTOM_DIR`); `internal/localruntime.CustomLibrarySources` persists configured sources and resyncs them on startup and hourly.

## Tests

- Prefer focused table or simple unit tests in the package being changed. Use `t.Parallel()` when the test does not mutate shared process state.
- Use `t.TempDir()`, fake `execx` runners, injected clocks/filesystem hooks, and `httptest` servers instead of real external services.
- When environment variables or global process state matter, reset them carefully; several packages use `TestMain` for shared cleanup.
- Keep tests aligned with public behavior and contracts rather than incidental implementation details, especially around CLI output, config validation, and local API behavior.

## Security

- Use generic names, account handles, service names, and home-directory paths in checked-in examples and test fixtures. Keep operator-specific identifiers out of public repository content.
- Never print, commit, or include in docs: `GITHUB_TOKEN`, `GH_TOKEN`, `OPENAI_API_KEY`, GitHub PATs or agent auth credentials.
- Failure and no-change follow-up runs execute against the agent_00 repository, so never copy the failed run's task prompt into their prompts or PR bodies. Route it through `failurefollowup.RedactTaskPrompt`, which substitutes `failurefollowup.RedactedTaskPromptNotice`.
- The same rule covers the failed run's other customer-derived identifiers. Keep its workspace path, prompt-derived branch slug, repository URLs, pull request URL, and target subdirectory out of follow-up prompts and PR bodies. The follow-up context blocks in `cmd/harness` carry nothing else: `failureFollowUpFailureContext` emits `exit_code` plus the failure `error`, and both no-change prompts emit only the agent_00-local `request_id`. Each then appends the redaction notice.
- Error text routinely embeds those same identifiers, so run it through `failurefollowup.RedactFailureDetail` before it reaches a follow-up prompt. It substitutes `RedactedURLPlaceholder`, `RedactedPathPlaceholder`, and `RedactedBranchPlaceholder` for repository and pull request URLs, absolute filesystem paths, and `agent00-` branch slugs while leaving the diagnostic wording readable.
- Before sharing repository or PR links in runtime activity, preserve the existing private-repo check using `gh repo view OWNER/REPO --json isPrivate,nameWithOwner`.

## Release

- Do not invent release flows. `deploy-vnext` verifies the same build/test commands before publishing Docker `vnext`; `deploy-prod` creates semver `vX.Y.Z` tags through GoReleaser and promotes Docker `latest`.
- Do not run publishing, tagging, Docker promotion, or credential-affecting commands unless the user explicitly asks for a release operation.
- For a hardening release gated on P0s, verify those fixes before running `deploy-vnext`. Confirm the build and its versioned Docker Hub tag, then use that tag as the source for `deploy-prod`. Verify the production workflow and the published image.

## PR Expectations

- Keep diffs focused on the requested behavior or documentation change.
- Summaries should include what changed, what was validated, and any remaining risk or validation gap.
- For library prompt changes, ensure `internal/library` tests still pass and that checked-in JSON remains canonical.
