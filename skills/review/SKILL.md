---
name: review
description: Review pull requests and code changes for actionable correctness, regression, security, compatibility, and test issues. Use for read-only PR review passes that must produce concise human findings plus a machine-readable verdict.
---

# Review Pull Requests

Inspect the effective diff against its merge base. Read the original task, PR metadata, discussion, changed code, callers, tests, configuration, migrations, and checks that affect the change.

Do not edit files, commit, push, or rewrite the pull request.

Prioritize actionable correctness, regression, security/privacy, data-loss, compatibility, concurrency, error-handling, performance, and missing-test risks. Verify claims from repository evidence. Do not report speculative concerns or pre-existing issues unrelated to the change.

Order findings by Critical, High, Medium, then Low. Every finding must identify a changed file and line, explain a concrete problem the author can fix, and be concise enough to drive a repair pass.

Return exactly two markdown sections, `**Positive**` and `**Negative**`, with bullet points only. Include at most three positive bullets and six negative bullets. If there are no findings, use exactly `- No material issues found.` under Negative.

End with a fenced JSON object using this shape:

```json
{"status":"clean|findings|blocked","mergeReady":false,"summary":"short verdict","positives":["short positive point"],"findings":[{"severity":"Medium","path":"path/to/file.go","line":123,"title":"specific actionable finding"}]}
```

Keep the JSON consistent with the markdown. Set `status` to `clean` and `mergeReady` to `true` only when there are no material findings or blockers.
