import catalog from "./templates.json" with { type: "json" };
export const workflowTemplates = catalog.templates;
export const events = [
  "schedule",
  "app.prompt",
  "app.json",
  "app.chat",
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
export function newWorkflow(repository, kind = "implement", id = crypto.randomUUID()) {
  const template = workflowTemplates.find((entry) => entry.id === kind);
  if (!template) throw new Error(`Unknown workflow template: ${kind}`);
  return { ...structuredClone(template.definition), id, repository, enabled: false, includeBacklog: false };
}
export function diagram(w) {
  const input = w.trigger.event.startsWith("app.");
  const triggerIcons = { schedule: "calendar", "app.prompt": "prompt", "app.json": "json", "app.chat": "chat" };
  const triggerIcon = triggerIcons[w.trigger.event] || (w.trigger.event.startsWith("pr.") ? "pullRequest" : "issue");
  const steps = [
    ["repository", "Repository", w.repository || "Choose a repository"],
    [
      "trigger",
      input ? `${w.trigger.event.slice(4)} submission` : "Trigger",
      (w.trigger.event === "schedule"
        ? `Every ${w.trigger.intervalHours} hours`
        : w.trigger.event.replaceAll(".", " · ").replaceAll("_", " ")) +
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
      w.task.libraryTaskName
        ? `Library · ${w.task.libraryTaskName}`
        : w.task.instructions || (input ? "Use the submitted task and configuration" : ""),
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
  if (w.trigger.event === "schedule" || input) steps.splice(2, 1);
  if (!w.reviewCycles || w.task.kind === "review") steps.splice(steps.findIndex(([id]) => id === "review"), 1);
  if (!w.merge.enabled) steps.splice(steps.findIndex(([id]) => id === "merge"), 1);
  return {
    nodes: steps.map(([id, label, detail], index) => ({
      id,
      type: "workflow",
      position: w.positions?.[id] || { x: 80, y: index * 145 },
      data: { label, detail, kind: id, icon: id === "trigger" ? triggerIcon : id === "task" ? w.task.kind : id, last: index === steps.length - 1, enabled: w.enabled },
    })),
    edges: steps.slice(1).map(([id], index) => ({
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
