---
name: review
description: Review pull requests and code changes for actionable correctness, regression, security, compatibility, and test issues. Use for read-only PR review passes that must produce concise human findings plus a machine-readable verdict.
---

# Review Pull Requests

Inspect the effective diff against its merge base. Read the original task, PR metadata, discussion, changed code, callers, tests, configuration, migrations, and checks that affect the change.

Work from the prepared PR context and the local checkout. Do not edit files, commit, push, or rewrite the pull request. The harness alone publishes the review and manages GitHub retries. Do not post reviews, comments, or progress updates, and do not attempt or check whether you can publish. These instructions apply even when authenticated GitHub tools are available or the task context asks you to post.

Prioritize actionable correctness, regression, security/privacy, data-loss, compatibility, concurrency, error-handling, performance, and missing-test risks. Verify claims from repository evidence. Do not report speculative concerns or pre-existing issues unrelated to the change.

Order findings by Critical, High, Medium, then Low. Every finding must identify a changed file and line, explain a concrete problem the author can fix, and be concise enough to drive a repair pass.

Return only one JSON object in your final response. Include at most three positive points and six findings. The harness renders the review from these fields.

```json
{"status":"findings","mergeReady":false,"summary":"short verdict","positives":["short positive point"],"findings":[{"severity":"Medium","path":"path/to/file.go","line":123,"title":"specific actionable finding"}]}
```

`status` must be exactly one of `clean`, `findings`, or `blocked`; never copy a list of choices into the value. Use `clean` with `mergeReady: true` and an empty `findings` array when inspection is complete and there are no material issues. Use `findings` with `mergeReady: false` for actionable problems in the PR. Use `blocked` with `mergeReady: false`, an empty `findings` array, and a short explanation in `summary` if you cannot complete the code inspection. GitHub publication, connectivity for publication, and tool permissions for publication have no bearing on this verdict or its findings.

When given original findings keyed by GitHub thread ID, independently check
each one against the current head. Add a top-level `verifiedThreadIds` array
with the exact IDs of findings demonstrably fixed by the current code. Leave
out any ID whose fix you cannot establish. An outdated diff anchor or a clean
overall verdict alone does not verify the original finding.
