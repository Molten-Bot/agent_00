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
const cycleArrow = { type: "arrowclosed", width: 20, height: 20 };

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
      "A fresh review agent checks each revision",
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
  const positions = steps.map(([id], index) => w.positions?.[id] || { x: 80, y: index * 145 });
  const side = (from, to) => {
    const dx = to.x - from.x, dy = to.y - from.y;
    return Math.abs(dx) > Math.abs(dy) ? (dx > 0 ? "right" : "left") : (dy >= 0 ? "bottom" : "top");
  };
  return {
    nodes: steps.map(([id, label, detail], index) => ({
      id,
      type: "workflow",
      position: positions[index],
      data: { sourcePosition: index < steps.length - 1 ? side(positions[index], positions[index + 1]) : "bottom", targetPosition: index > 0 ? side(positions[index], positions[index - 1]) : "top", label, detail, kind: id, icon: id === "trigger" ? triggerIcon : id === "task" ? w.task.kind : id, last: index === steps.length - 1, enabled: w.enabled },
    })),
    edges: steps.slice(1).map(([target], index) => {
      const source = steps[index][0];
      const edge = { id: `${source}-${target}`, source, target, type: "smoothstep" };
      if (source !== "task" || target !== "review") return edge;
      const label = `${w.reviewCycles} ${w.reviewCycles === 1 ? "cycle" : "cycles"}`;
      return {
        ...edge,
        markerStart: cycleArrow,
        markerEnd: cycleArrow,
        label,
        labelStyle: { fontSize: 11, fontWeight: 600 },
        ariaLabel: `Agent task and review, up to ${label}`,
      };
    }),
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

// Workflow execution is ordered; arrange only the currently visible steps.
export function automaticLayout(workflow, direction, measuredNodes = []) {
  const sizes = new Map(measuredNodes.map(node => [node.id, node.measured || node]));
  const positions = { ...workflow.positions };
  let offset = 80;
  for (const node of diagram(workflow).nodes) {
    positions[node.id] = direction === "horizontal" ? { x: offset, y: 80 } : { x: 80, y: offset };
    const size = sizes.get(node.id);
    offset += (direction === "horizontal" ? size?.width || 230 : size?.height || 140) + 64;
  }
  return positions;
}
