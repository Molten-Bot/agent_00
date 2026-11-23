export const events = [
  "issue.opened",
  "issue.assigned",
  "issue.labeled",
  "pr.opened",
  "pr.assigned",
  "pr.labeled",
  "pr.ready",
  "pr.review_requested",
  "pr.changes_requested",
  "pr.checks_failed",
];
export function newWorkflow(
  repository,
  kind = "implement",
  id = crypto.randomUUID(),
) {
  return {
    id,
    name: {
      implement: "Implement issues",
      review: "Review pull requests",
      repair: "Repair failed checks",
    }[kind],
    repository,
    enabled: false,
    includeBacklog: false,
    trigger: {
      event: {
        implement: "issue.labeled",
        review: "pr.review_requested",
        repair: "pr.checks_failed",
      }[kind],
      value:
        kind === "implement"
          ? "ready-for-agent"
          : kind === "review"
            ? "@me"
            : "",
    },
    filters: { mode: "all", rules: [] },
    task: {
      kind,
      instructions: {
        implement: "Implement the issue and run the relevant tests.",
        review:
          "Review the pull request for correctness, regressions, and missing tests.",
        repair:
          "Read the failed checks and review feedback, fix the root cause, and run the relevant tests.",
      }[kind],
      assignCreator: false,
      issueLinkMode: "reference",
      draft: false,
    },
    reviewCycles: 0,
    merge: {
      enabled: false,
      method: "squash",
      requireApproval: true,
      deleteBranch: false,
    },
    positions: {},
  };
}
export function diagram(w) {
  const steps = [
    ["repository", "Repository", w.repository || "Choose a repository"],
    [
      "trigger",
      "Trigger",
      w.trigger.event.replaceAll(".", " · ").replaceAll("_", " ") +
        (w.trigger.value ? ` · ${w.trigger.value}` : ""),
    ],
    [
      "filters",
      "Filters",
      w.filters.rules.length
        ? `${w.filters.mode === "all" ? "All" : "Any"} of ${w.filters.rules.length} rules`
        : "All open items matching the trigger",
    ],
    [
      "task",
      w.task.kind === "review"
        ? "Review PR"
        : w.task.kind === "repair"
          ? "Repair PR"
          : "Agent task",
      w.task.instructions,
    ],
    [
      "review",
      "Review cycles",
      w.reviewCycles
        ? `Up to ${w.reviewCycles} repair cycles`
        : "No post-task review cycles",
    ],
    [
      "merge",
      "Merge",
      w.merge.enabled
        ? `${w.merge.method} · checks${w.merge.requireApproval ? " + approval" : ""}${w.merge.deleteBranch ? " · delete branch" : ""}`
        : "Leave the pull request for a human",
    ],
  ];
  return {
    nodes: steps.map(([id, label, detail], index) => ({
      id,
      type: "workflow",
      position: w.positions?.[id] || { x: 80, y: index * 145 },
      data: { label, detail, kind: id, enabled: w.enabled },
    })),
    edges: steps
      .slice(1)
      .map(([id], index) => ({
        id: `${steps[index][0]}-${id}`,
        source: steps[index][0],
        target: id,
        type: "smoothstep",
      })),
  };
}
export function replaceDefinition(document, definition) {
  return {
    ...document,
    workflows: document.workflows.some((w) => w.id === definition.id)
      ? document.workflows.map((w) => (w.id === definition.id ? definition : w))
      : [...document.workflows, definition],
  };
}
