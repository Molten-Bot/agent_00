import test from "node:test";
import assert from "node:assert/strict";
import { newWorkflow, diagram, replaceDefinition } from "./model.js";
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
  w.merge.enabled = true;
  w.merge.method = "rebase";
  w.positions.task = { x: 40, y: 50 };
  const graph = diagram(w);
  assert.equal(graph.nodes.length, 6);
  assert.equal(graph.edges.length, 5);
  assert.deepEqual(graph.nodes.find((n) => n.id === "task").position, {
    x: 40,
    y: 50,
  });
  assert.match(graph.nodes.find((n) => n.id === "review").data.detail, /5/);
  assert.match(graph.nodes.find((n) => n.id === "merge").data.detail, /rebase/);
});
