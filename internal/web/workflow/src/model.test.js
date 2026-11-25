import test from "node:test";
import assert from "node:assert/strict";
import { newWorkflow, diagram, replaceDefinition, automaticLayout, incompleteWorkflow } from "./model.js";
test("new workflows are disabled, skip backlog, and default to manual merge", () => {
  for (const kind of ["implement", "review", "repair"]) {
    const w = newWorkflow("acme/repo", kind, "id");
    assert.equal(w.enabled, false);
    assert.equal(w.includeBacklog, false);
    assert.equal(w.merge.enabled, false);
    assert.equal(w.task.kind, kind);
  }
});
test("multiple workflows survive independent updates", () => {
  const a = newWorkflow("acme/repo", "implement", "one"),
    b = newWorkflow("acme/repo", "review", "two");
  let doc = { version: 1, revision: 3, workflows: [] };
  doc = replaceDefinition(replaceDefinition(doc, a), b);
  doc = replaceDefinition(doc, { ...a, name: "Updated" });
  assert.equal(doc.workflows.length, 2);
  assert.equal(doc.workflows[1].name, b.name);
  assert.equal(doc.revision, 3);
});
test("diagram represents actual policy and preserves layout", () => {
  const w = newWorkflow("acme/repo", "implement", "one");
  w.reviewCycles = 5;
  w.reviewAgent = { instructions: "Check error handling", complexity: "advanced" };
  w.merge.enabled = true;
  w.merge.method = "rebase";
  w.positions.task = { x: 40, y: 50 };
  const graph = diagram(w);
  assert.equal(graph.nodes.length, 7);
  assert.equal(graph.edges.length, 6);
  assert.deepEqual(graph.nodes.find((n) => n.id === "task").position, {
    x: 40,
    y: 50,
  });
  assert.equal(graph.nodes.find((n) => n.id === "reviewAgent").data.detail, "Check error handling");
  const reviewEdge = graph.edges.find(edge => edge.id === "review-reviewAgent");
  assert.equal(reviewEdge.data.cycles, 5);
  assert.equal(reviewEdge.ariaLabel, "Review cycles and review agent, up to 5 cycles");
  assert.equal(reviewEdge.markerEnd.type, "arrowclosed");
  assert.equal(graph.edges.filter(edge => edge.markerStart || edge.markerEnd).length, 1);
  assert.match(graph.nodes.find((n) => n.id === "merge").data.detail, /rebase/);
});

test("scheduled tasks use the same agent/review/merge pipeline without GitHub filters", () => {
  const w = newWorkflow("acme/repo", "schedule", "scheduled");
  w.task.libraryTaskName = "audit";
  const graph = diagram(w);
  assert.equal(w.trigger.intervalHours, 24);
  assert.equal(w.enabled, false);
  assert.equal(graph.nodes.length, 3);
  assert.equal(
    graph.nodes.some((n) => n.id === "filters"),
    false,
  );
  assert.match(graph.nodes.find((n) => n.id === "task").data.detail, /audit/);
  assert.match(
    graph.nodes.find((n) => n.id === "trigger").data.detail,
    /24 hours/,
  );
});

test("schedule autosave rejects an invalid optional first-run timestamp", () => {
  const workflow = newWorkflow("acme/repo", "schedule", "scheduled");
  const tasks = [{ name: "audit", prompt: "Audit" }];
  assert.equal(incompleteWorkflow(workflow, tasks), "");
  workflow.trigger.startAt = "0001-01-01T00:00:00.000Z";
  assert.match(incompleteWorkflow(workflow, tasks), /valid first-run date/);
  workflow.trigger.startAt = "not-a-date";
  assert.match(incompleteWorkflow(workflow, tasks), /valid first-run date/);
  workflow.trigger.startAt = "2026-11-24T18:00:00.000Z";
  assert.equal(incompleteWorkflow(workflow, tasks), "");
});

test("template copies are independent across repositories and preserve merge policy", () => {
  const a = newWorkflow("acme/one", "delivery", "first");
  const b = newWorkflow("acme/two", "delivery", "second");
  assert.equal(a.enabled, false);
  assert.equal(a.reviewCycles, 2);
  assert.deepEqual(a.merge, { enabled: true, method: "squash", requireApproval: true, deleteBranch: true });
  a.merge.method = "rebase";
  a.filters.rules.push({field: "label", operator: "is", value: "bug"});
  assert.equal(b.merge.method, "squash");
  assert.equal(b.filters.rules.length, 0);
  assert.equal(b.repository, "acme/two");
  assert.equal(newWorkflow("acme/three", "delivery", "third").merge.method, "squash");
  assert.throws(() => newWorkflow("acme/repo", "missing", "id"), /Unknown workflow template/);
});

test("input workflows compose only active steps and identify their source", () => {
  for (const source of ["prompt", "json", "chat"]) {
    const w = newWorkflow("acme/repo", `app-${source}`, source);
    let graph = diagram(w);
    assert.deepEqual(graph.nodes.map(n => n.id), ["repository", "trigger", "task"]);
    assert.equal(graph.nodes[1].data.icon, source);
    assert.equal(graph.nodes[2].data.icon, "implement");
    assert.equal(graph.nodes[0].targetPosition, undefined);
    assert.equal(graph.nodes[2].sourcePosition, undefined);
    w.reviewCycles = 2;
    w.merge.enabled = true;
    graph = diagram(w);
    assert.deepEqual(graph.nodes.map(n => n.id), ["repository", "trigger", "task", "review", "merge", "reviewAgent"]);
    assert.equal(graph.edges.length, 5);
    assert.equal(graph.nodes[2].sourcePosition, "bottom");
    assert.equal(graph.nodes[4].sourcePosition, undefined);
  }
  const review = newWorkflow("acme/repo", "review", "review");
  review.reviewCycles = 2;
  assert.equal(diagram(review).nodes.some(n => n.id === "review" || n.id === "reviewAgent"), false);
  assert.equal(diagram(review).nodes.find(n => n.id === "task").data.icon, "review");
  assert.equal(diagram(newWorkflow("acme/repo", "repair", "repair")).nodes.find(n => n.id === "task").data.icon, "repair");
});


test("automatic layouts use visible steps and measured sizes without changing execution policy", () => {
  const w = newWorkflow("acme/repo", "delivery", "layout");
  const original = structuredClone(w);
  const nodes = diagram(w).nodes.map(n => ({ ...n, measured: { width: 260, height: 180 } }));
  for (const direction of ["vertical", "horizontal"]) {
    const positions = automaticLayout(w, direction, nodes);
    const graph = diagram({ ...w, positions });
    for (let i = 1; i < graph.nodes.length - 1; i++) {
      const previous = graph.nodes[i - 1], current = graph.nodes[i];
      if (direction === "horizontal") {
        assert.equal(current.position.y, previous.position.y);
        assert.ok(current.position.x > previous.position.x + 260);
        assert.equal(current.targetPosition, "left");
        assert.equal(previous.sourcePosition, "right");
      } else {
        assert.equal(current.position.x, previous.position.x);
        assert.ok(current.position.y > previous.position.y + 180);
        assert.equal(current.targetPosition, "top");
        assert.equal(previous.sourcePosition, "bottom");
      }
    }
    const { review, reviewAgent } = positions;
    assert.deepEqual(reviewAgent, direction === "horizontal"
      ? { x: review.x, y: review.y + 180 + 64 }
      : { x: review.x + 260 + 64, y: review.y });
    assert.deepEqual(graph.edges, diagram(w).edges);
    delete positions.reviewAgent;
    const legacyNodes = diagram({ ...w, positions }).nodes;
    const legacyAgent = legacyNodes.find(n => n.id === "reviewAgent");
    assert.equal(legacyAgent.targetPosition, direction === "horizontal" ? "top" : "left");
  }
  assert.deepEqual(w, original);
  const input = newWorkflow("acme/repo", "app-prompt", "simple");
  assert.deepEqual(Object.keys(automaticLayout(input, "vertical")), ["repository", "trigger", "task"]);
});

test("legacy review agent placement avoids nonadjacent nodes", () => {
  const w = newWorkflow("acme/repo", "delivery", "layout");
  const review = diagram(w).nodes.find(node => node.id === "review").position;
  const initial = diagram(w).nodes.find(node => node.id === "reviewAgent").position;
  w.positions = { repository: initial };
  const agent = diagram(w).nodes.find(node => node.id === "reviewAgent");
  assert.notDeepEqual(agent.position, initial);
  assert.notDeepEqual(agent.position, review);
});
