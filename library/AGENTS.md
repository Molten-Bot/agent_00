---
name: moltenhub-library-guide
description: Scoped guidance for library task definitions and the bundled runtime AGENTS.md seed.
---

# Library Scope

This file applies to `library/`. It is also a product artifact: Docker sets `HARNESS_AGENTS_SEED_PATH=/opt/moltenhub/library/AGENTS.md`, and `internal/library/agents_seed_test.go` verifies that this seed keeps runtime tooling and safety guidance.

## Editing Library Tasks

- `library/*.json` files are loaded by `internal/library.LoadCatalog`; keep top-level task names unique and stable.
- Use camelCase fields from `internal/library.TaskDefinition`: `displayName`, `type`, `icon`, `description`, `targetSubdir`, `prompt`, `commitMessage`, `prTitle`, `prBody`, `labels`, `githubHandle`, and `reviewers`.
- Do not add snake_case fields to library task definitions. The library loader intentionally rejects unknown task fields; hub runtime config is a separate snake_case contract.
- Keep prompts specific to repository-changing agent work: required inspection, implementation boundaries, validation, failure reporting, and final response shape.
- Do not put real tokens, repository credentials, private links, or customer data in library tasks, examples, or prompt text.
- If a task references exact tooling, verify it is present in `Dockerfile`, `README.md`, or runtime docs.

## Validation

- For library JSON, seed text, or catalog behavior, run `GOCACHE=/tmp/go-cache go test ./internal/library`.
- For changes that alter prompt expansion or packaged runtime behavior, run `GOCACHE=/tmp/go-cache go test ./...`.
- If railsmith is used to update this file, keep managed markers intact and inspect the final diff.

## Runtime Seed For Dispatched Agents

You are working inside an existing repository. Solve the user's actual problem with the smallest correct change that fits the codebase.

### Repository Work

- Read the relevant code, tests, config, and docs before editing.
- Match the repository's language, framework, naming, structure, formatting, and testing conventions.
- Reuse existing helpers or extension points before adding new abstractions.
- Keep the diff focused. Do not change product code, build logic, package versions, or unrelated docs unless required for the task.
- For implementation or repository-change requests, produce a repository diff unless concrete repository evidence proves no file changes are required.
- Return a no-op only for review/investigation-only tasks or when concrete repository evidence shows no file changes are required.

### Runtime Tooling

- Playwright is installed in the container for local browser testing, screenshots, and comparisons.
- `npm` is available for JavaScript package installs, scripts, tests, and builds.
- Python, `pip`, and `virtualenv` are available for Python workflows and validation.
- Go is available for Go workflows and validation, including `go test` and `go build`.
- `git-changes-by-day` is available for exporting git history to CSV, for example `git-changes-by-day -repo /path/to/repo -text-out /tmp/commit-text.csv`.
- Use the tooling that matches the repository. If tooling is unavailable, continue with useful alternatives and report the validation gap.

### Pull Request Screenshot Handoff

- When asked to add screenshots to PR comments, save PNG or JPEG files under `.moltenhub/pr-comment-screenshots/` with descriptive names such as `before.png` and `after.png`.
- The harness creates or reuses the pull request after the run and posts those saved screenshots as a PR comment. Do not fail only because no pull request exists while still running.

### Secrets And Hub Safety

- YOU ARE NOT ALLOWED TO SHARE: GITHUB PAT and YOUR (AGENTS) AUTH CREDENTIALS.
- Do not expose secrets, tokens, private repository links, customer data, or agent auth credentials in files, logs, commit messages, reports, or final responses.
- Before sharing repository or pull-request links in Hub activity, use `gh repo view OWNER/REPO --json isPrivate,nameWithOwner`; share repo and PR links only when GitHub reports `isPrivate:false`.
- If a repository is not initialized after clone, use only `gh` CLI and `git` tools to create and push a main branch, then continue once git state is ready.

### Failure Reporting

- When failures occur, return `Failure:` and `Error details:` fields with a concrete summary and error detail.
- Do not fail solely because PR creation, remote CI/CD watching, or local validation tooling is unavailable. Finish repository changes and local validation that are possible in the runtime.
