import React, { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  ReactFlow,
  ReactFlowProvider,
  Background,
  Controls,
  MiniMap,
  Handle,
  Position,
  useReactFlow,
} from "@xyflow/react";
import {
  Zap,
  Bot,
  GitBranch,
  FolderGit2,
  ListFilter,
  MessageSquareCode,
  Plus,
  Save,
  Download,
  Play,
  Pause,
  DiagramFlowVertical,
  X,
} from "lucide-react";
import "@xyflow/react/style.css";
import "./style.css";
import { events, newWorkflow, diagram, replaceDefinition } from "./model.js";
const icons = {
  repository: FolderGit2,
  trigger: Zap,
  filters: ListFilter,
  task: Bot,
  review: MessageSquareCode,
  merge: GitBranch,
};
const colors = {
  repository: "#66b5eb",
  trigger: "#50bfa5",
  filters: "#e5ae58",
  task: "#a18aff",
  review: "#66b5eb",
  merge: "#de8fbd",
};
function StepNode({ data, selected }) {
  const Icon = icons[data.kind];
  return (
    <div
      className={`wf-node ${selected ? "wf-node-selected" : ""}`}
      style={{ "--step-color": colors[data.kind] }}
    >
      {data.kind !== "repository" && (
        <Handle type="target" position={Position.Top} />
      )}
      <div className="wf-node-top">
        <span className="wf-icon">
          <Icon size={17} />
        </span>
        <span>{data.label}</span>
        <span className="wf-node-status">
          {data.enabled ? "Enabled" : "Draft"}
        </span>
      </div>
      <strong>{data.label}</strong>
      <p>{data.detail}</p>
      {data.kind !== "merge" && (
        <Handle type="source" position={Position.Bottom} />
      )}
    </div>
  );
}
const nodeTypes = { workflow: StepNode };
async function api(path, options) {
  const response = await fetch(path, options);
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "Request failed");
  return data;
}
const jsonOptions = (method, value) => ({
  method,
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(value),
});
function Field({ label, children }) {
  return (
    <label className="wf-field">
      {label}
      {children}
    </label>
  );
}
function Toggle({ label, value, onChange }) {
  return (
    <label className="wf-toggle">
      <input
        type="checkbox"
        checked={value}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span>{label}</span>
    </label>
  );
}
function Editor() {
  const [doc, setDoc] = useState(null),
    [current, setCurrent] = useState(null),
    [repo, setRepo] = useState(""),
    [repos, setRepos] = useState([]),
    [selection, setSelection] = useState("trigger"),
    [dirty, setDirty] = useState(false),
    [notice, setNotice] = useState(""),
    [preview, setPreview] = useState(null),
    [runs, setRuns] = useState([]),
    [busy, setBusy] = useState(false);
  const flow = useReactFlow();
  const graph = current ? diagram(current) : { nodes: [], edges: [] };
  async function reload() {
    try {
      const d = await api("/api/workflows");
      setDoc(d);
      setCurrent(null);
      setDirty(false);
    } catch (e) {
      setNotice(e.message);
    }
  }
  useEffect(() => {
    reload();
    api("/api/github/repos")
      .then((data) => setRepos(data.repos || []))
      .catch((e) => setNotice(e.message));
  }, []);
  useEffect(() => {
    const refresh = () =>
      api("/api/workflows/runs")
        .then(setRuns)
        .catch(() => {});
    refresh();
    const timer = setInterval(refresh, 15000);
    return () => clearInterval(timer);
  }, []);
  useEffect(() => {
    const handler = (e) => {
      if (e.detail === "workflow")
        requestAnimationFrame(() =>
          flow.fitView({ padding: 32, duration: 200 }),
        );
    };
    window.addEventListener("workflow-display", handler);
    return () => window.removeEventListener("workflow-display", handler);
  }, [flow]);
  useEffect(() => {
    const handler = (e) => {
      if (dirty) {
        e.preventDefault();
        e.returnValue = "";
      }
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, [dirty]);
  function edit(update) {
    setCurrent((w) => ({ ...w, ...update }));
    setDirty(true);
    setPreview(null);
  }
  function choose(w) {
    if (dirty) {
      setNotice("Save or discard your changes before switching workflows.");
      return;
    }
    setCurrent(structuredClone(w));
    setSelection("trigger");
    setPreview(null);
    setNotice("");
    setTimeout(() => flow.fitView({ padding: 32 }), 0);
  }
  async function save(enabled = current.enabled) {
    setBusy(true);
    try {
      const next = { ...current, enabled };
      const d = await api(
        "/api/workflows",
        jsonOptions("PUT", replaceDefinition(doc, next)),
      );
      setDoc(d);
      setCurrent(next);
      setDirty(false);
      setNotice(
        enabled
          ? "Workflow enabled. New matching events will start tasks."
          : "Draft saved. This workflow will not start tasks.",
      );
    } catch (e) {
      setNotice(e.message);
    } finally {
      setBusy(false);
    }
  }
  function create(kind) {
    if (!repo.trim()) {
      setNotice("Choose a repository first.");
      return;
    }
    if (dirty) {
      setNotice("Save or discard the current draft first.");
      return;
    }
    setCurrent(newWorkflow(repo.trim(), kind));
    setSelection("trigger");
    setDirty(true);
    setNotice("New draft. Save it before enabling automation.");
    setTimeout(() => flow.fitView({ padding: 32 }), 0);
  }
  async function showPreview() {
    setBusy(true);
    try {
      setPreview(
        await api("/api/workflows/preview", jsonOptions("POST", current)),
      );
      setNotice("Preview only: no tasks were started.");
    } catch (e) {
      setNotice(e.message);
    } finally {
      setBusy(false);
    }
  }
  async function remove() {
    setBusy(true);
    try {
      const d = await api(
        "/api/workflows",
        jsonOptions("PUT", {
          ...doc,
          workflows: doc.workflows.filter((w) => w.id !== current.id),
        }),
      );
      setDoc(d);
      setCurrent(null);
      setDirty(false);
      setNotice("Workflow deleted. Existing task history is retained.");
    } catch (e) {
      setNotice(e.message);
    } finally {
      setBusy(false);
    }
  }
  function exportAll() {
    const data = dirty && current ? replaceDefinition(doc, current) : doc;
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(data, null, 2)], { type: "application/json" }),
    );
    const a = document.createElement("a");
    a.href = url;
    a.download = "workflows.json";
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  const visible = (doc?.workflows || []).filter(
    (w) => !repo || w.repository.toLowerCase() === repo.toLowerCase(),
  );
  const updateTask = (field, value) =>
    edit({ task: { ...current.task, [field]: value } });
  const updateMerge = (field, value) =>
    edit({ merge: { ...current.merge, [field]: value } });
  const ruleFields = current?.trigger.event.startsWith("issue.")
    ? ["label", "author", "assignee", "issueType", "bot"]
    : [
        "label",
        "author",
        "assignee",
        "reviewer",
        "baseBranch",
        "draft",
        "bot",
        "linkedIssue",
      ];
  return (
    <div className="wf-editor">
      <header className="wf-header">
        <div className="wf-heading">
          <div className="wf-eyebrow">
            <DiagramFlowVertical size={15} />
            <h1 id="workflow-title">Workflow</h1>
            <span className="wf-beta">Beta</span>
          </div>
          {current ? (
            <input
              className="wf-name"
              aria-label="Workflow name"
              value={current.name}
              maxLength={100}
              onChange={(e) => edit({ name: e.target.value })}
            />
          ) : (
            <h2>Repository automations</h2>
          )}
        </div>
        <div className="wf-header-actions">
          <span className="wf-save-state">
            {dirty
              ? "Unsaved changes"
              : current?.enabled
                ? "Enabled"
                : current
                  ? "Saved draft"
                  : "Version 1"}
          </span>
          <button
            disabled={!doc}
            onClick={exportAll}
            aria-label="Export workflows JSON"
          >
            <Download size={16} />
          </button>
          {current && (
            <>
              <button disabled={busy} onClick={showPreview}>
                Preview matches
              </button>
              <button disabled={busy} onClick={() => save(false)}>
                <Save size={15} />
                Save draft
              </button>
              <button
                className="wf-primary"
                disabled={busy || dirty}
                onClick={() => save(!current.enabled)}
              >
                {current.enabled ? <Pause size={15} /> : <Play size={15} />}{" "}
                {current.enabled ? "Pause" : "Enable"}
              </button>
            </>
          )}
        </div>
      </header>
      <div className="wf-workspace">
        <aside className="wf-palette" aria-label="Repository workflows">
          <div className="wf-section-label">REPOSITORY</div>
          <Field label="Repository">
            <input
              aria-label="Workflow repository"
              placeholder="owner/repository"
              list="wf-repos"
              value={repo}
              onChange={(e) => {
                if (dirty) {
                  setNotice("Save or discard changes first.");
                  return;
                }
                setRepo(e.target.value);
                setCurrent(null);
              }}
            />
          </Field>
          <datalist id="wf-repos">
            {repos.map((r) => (
              <option key={r.full_name} value={r.full_name} />
            ))}
          </datalist>
          <p className="wf-muted">
            Each repository can have several workflows, or none.
          </p>
          <div className="wf-step-list">
            {visible.map((w) => (
              <button
                key={w.id}
                className={`wf-step ${current?.id === w.id ? "wf-active" : ""}`}
                onClick={() => choose(w)}
              >
                <span>
                  <strong>{w.name}</strong>
                  <small>
                    {w.repository} · {w.enabled ? "Enabled" : "Draft"}
                  </small>
                </span>
              </button>
            ))}
          </div>
          {!visible.length && (
            <p className="wf-muted">No workflows for this repository.</p>
          )}
          <div className="wf-section-label wf-spacer">NEW WORKFLOW</div>
          {["implement", "review", "repair"].map((kind) => (
            <button
              className="wf-template"
              key={kind}
              onClick={() => create(kind)}
              disabled={!doc}
            >
              <Plus size={14} />
              {kind === "implement"
                ? "Implement issues"
                : kind === "review"
                  ? "Review PRs"
                  : "Repair checks"}
            </button>
          ))}
          {doc?.migration && (
            <details className="wf-migration">
              <summary>Migration notes</summary>
              <p>{doc.migration}</p>
            </details>
          )}
        </aside>
        <div className="wf-canvas">
          <div className="wf-canvas-caption">
            {current ? "DEFINITION CANVAS" : "CHOOSE OR CREATE A WORKFLOW"}
            <span>{current?.repository}</span>
          </div>
          <ReactFlow
            nodes={graph.nodes}
            edges={graph.edges}
            nodeTypes={nodeTypes}
            onNodesChange={(changes) => {
              const moves = changes
                .toArray()
                .filter((c) => c.type === "position" && c.position);
              if (moves.length)
                edit({
                  positions: {
                    ...current.positions,
                    ...Object.fromEntries(moves.map((c) => [c.id, c.position])),
                  },
                });
            }}
            onNodeClick={(_, n) => setSelection(n.id)}
            nodesConnectable={false}
            edgesReconnectable={false}
            deleteKeyCode={null}
            fitView
            fitViewOptions={{ padding: 32 }}
            minZoom={0.25}
            maxZoom={1.6}
          >
            <Background gap={22} />
            <Controls showInteractive={false} />
            <MiniMap
              style={{ width: 120, height: 80 }}
              nodeColor={(n) => colors[n.data.kind]}
              pannable
              zoomable
            />
          </ReactFlow>
          {notice && (
            <div className="wf-notice" role="status">
              <div>
                <strong>{notice}</strong>
                <button
                  aria-label="Dismiss message"
                  onClick={() => setNotice("")}
                >
                  <X size={14} />
                </button>
              </div>
              {dirty && (
                <button
                  onClick={() => {
                    setCurrent(
                      doc.workflows.find((w) => w.id === current.id) || null,
                    );
                    setDirty(false);
                    setNotice("Changes discarded.");
                  }}
                >
                  Discard changes
                </button>
              )}
            </div>
          )}
        </div>
        <aside className="wf-inspector" aria-label="Step settings">
          <div className="wf-section-label">{selection.toUpperCase()}</div>
          {!current ? (
            <>
              <h2>Build an automation</h2>
              <p className="wf-muted">
                Choose a repository and start with a template. Workflows start
                as disabled drafts.
              </p>
              <button disabled={!doc} onClick={reload}>
                Reload definitions
              </button>
            </>
          ) : (
            <>
              {selection === "repository" && (
                <>
                  <h2>{current.repository}</h2>
                  <p className="wf-muted">
                    The repository filter and commit signing settings apply to
                    every workflow.
                  </p>
                  <Toggle
                    label="Include existing matching items on first activation"
                    value={current.includeBacklog}
                    onChange={(v) => edit({ includeBacklog: v })}
                  />
                  <p className="wf-muted">
                    Off by default. Otherwise, the first scan establishes a
                    baseline and only new matches start work.
                  </p>
                </>
              )}
              {selection === "trigger" && (
                <>
                  <h2>What starts work?</h2>
                  <Field label="GitHub event">
                    <select
                      value={current.trigger.event}
                      onChange={(e) => {
                        const issue = e.target.value.startsWith("issue.");
                        edit({
                          trigger: { event: e.target.value, value: "" },
                          filters: { mode: "all", rules: [] },
                          task: {
                            ...current.task,
                            kind: issue ? "implement" : "review",
                          },
                        });
                      }}
                    >
                      {events.map((e) => (
                        <option key={e} value={e}>
                          {e.replaceAll(".", " · ").replaceAll("_", " ")}
                        </option>
                      ))}
                    </select>
                  </Field>
                  {/assigned|labeled|review_requested/.test(
                    current.trigger.event,
                  ) && (
                    <Field
                      label={
                        current.trigger.event.endsWith("labeled")
                          ? "Label"
                          : "GitHub login (or @me)"
                      }
                    >
                      <input
                        value={current.trigger.value || ""}
                        onChange={(e) =>
                          edit({
                            trigger: {
                              ...current.trigger,
                              value: e.target.value,
                            },
                          })
                        }
                      />
                    </Field>
                  )}
                  <Toggle
                    label="Include existing matches on first activation"
                    value={current.includeBacklog}
                    onChange={(v) => edit({ includeBacklog: v })}
                  />
                  <p className="wf-muted">
                    GitHub is checked every 30 seconds. A new match starts one
                    task. A match must stop matching before it can activate
                    again.
                  </p>
                </>
              )}
              {selection === "filters" && (
                <>
                  <h2>Which items?</h2>
                  <Field label="Match rules">
                    <select
                      value={current.filters.mode}
                      onChange={(e) =>
                        edit({
                          filters: { ...current.filters, mode: e.target.value },
                        })
                      }
                    >
                      <option value="all">All rules</option>
                      <option value="any">Any rule</option>
                    </select>
                  </Field>
                  {current.filters.rules.map((r, index) => (
                    <div className="wf-rule" key={index}>
                      {["field", "operator", "value"].map((field) => (
                        <Field label={field} key={field}>
                          {field === "value" ? (
                            <input
                              value={r.value}
                              onChange={(e) =>
                                edit({
                                  filters: {
                                    ...current.filters,
                                    rules: current.filters.rules.map(
                                      (old, i) =>
                                        i === index
                                          ? { ...old, value: e.target.value }
                                          : old,
                                    ),
                                  },
                                })
                              }
                            />
                          ) : (
                            <select
                              value={r[field]}
                              onChange={(e) =>
                                edit({
                                  filters: {
                                    ...current.filters,
                                    rules: current.filters.rules.map(
                                      (old, i) =>
                                        i === index
                                          ? { ...old, [field]: e.target.value }
                                          : old,
                                    ),
                                  },
                                })
                              }
                            >
                              {(field === "field"
                                ? ruleFields
                                : ["is", "isNot"]
                              ).map((v) => (
                                <option key={v}>{v}</option>
                              ))}
                            </select>
                          )}
                        </Field>
                      ))}
                      <button
                        onClick={() =>
                          edit({
                            filters: {
                              ...current.filters,
                              rules: current.filters.rules.filter(
                                (_, i) => i !== index,
                              ),
                            },
                          })
                        }
                      >
                        Remove rule
                      </button>
                    </div>
                  ))}
                  <button
                    onClick={() =>
                      edit({
                        filters: {
                          ...current.filters,
                          rules: [
                            ...current.filters.rules,
                            { field: "label", operator: "is", value: "" },
                          ],
                        },
                      })
                    }
                  >
                    <Plus size={14} />
                    Add filter
                  </button>
                  <p className="wf-muted">
                    Use true / false for draft, bot, and linkedIssue. Issue type
                    uses GitHub’s actual type name. Labels support exclusion
                    with isNot.
                  </p>
                </>
              )}
              {selection === "task" && (
                <>
                  <h2>Agent task</h2>
                  {current.trigger.event.startsWith("pr.") && (
                    <Field label="Task kind">
                      <select
                        value={current.task.kind}
                        onChange={(e) => updateTask("kind", e.target.value)}
                      >
                        <option value="review">Review and comment</option>
                        <option value="repair">Repair existing PR</option>
                      </select>
                    </Field>
                  )}
                  <Field label="Instructions">
                    <textarea
                      rows={7}
                      value={current.task.instructions}
                      onChange={(e) =>
                        updateTask("instructions", e.target.value)
                      }
                    />
                  </Field>
                  <Toggle
                    label="Assign created work to me"
                    value={current.task.assignCreator}
                    onChange={(v) => updateTask("assignCreator", v)}
                  />
                  <Toggle
                    label="Create draft pull request"
                    value={current.task.draft}
                    onChange={(v) => updateTask("draft", v)}
                  />
                  <Field label="Issue linking">
                    <select
                      value={current.task.issueLinkMode}
                      onChange={(e) =>
                        updateTask("issueLinkMode", e.target.value)
                      }
                    >
                      <option value="reference">Reference issue</option>
                      <option value="close">Close on merge</option>
                    </select>
                  </Field>
                </>
              )}
              {selection === "review" && (
                <>
                  <h2>Bounded review cycles</h2>
                  <Field label="Maximum repair cycles">
                    <input
                      type="number"
                      min="0"
                      max="10"
                      value={current.reviewCycles}
                      onChange={(e) =>
                        edit({ reviewCycles: Number(e.target.value) })
                      }
                    />
                  </Field>
                  <p className="wf-muted">
                    After implementation or repair, review and fix findings up
                    to this limit. A final review confirms the result.
                    Unresolved findings stop the run. Review-only tasks post one
                    review.
                  </p>
                </>
              )}
              {selection === "merge" && (
                <>
                  <h2>After successful work</h2>
                  <Toggle
                    label="Automatically merge"
                    value={current.merge.enabled}
                    onChange={(v) => updateMerge("enabled", v)}
                  />
                  <Field label="Merge method">
                    <select
                      value={current.merge.method}
                      onChange={(e) => updateMerge("method", e.target.value)}
                    >
                      {["squash", "merge", "rebase"].map((m) => (
                        <option key={m}>{m}</option>
                      ))}
                    </select>
                  </Field>
                  <Toggle
                    label="Require human approval"
                    value={current.merge.requireApproval}
                    onChange={(v) => updateMerge("requireApproval", v)}
                  />
                  <Toggle
                    label="Delete branch after merge"
                    value={current.merge.deleteBranch}
                    onChange={(v) => updateMerge("deleteBranch", v)}
                  />
                  <p className="wf-muted">
                    Waits for successful checks, a ready PR, and no requested
                    changes. GitHub branch protection still applies.
                    Repositories without checks remain waiting.
                  </p>
                </>
              )}
              <div className="wf-spacer">
                <button
                  disabled={busy || current.enabled}
                  className="wf-delete"
                  onClick={remove}
                >
                  Delete draft
                </button>
              </div>
            </>
          )}
        </aside>
      </div>
      {(preview || current) && (
        <details className="wf-activity" open={!!preview}>
          <summary>
            {preview ? "Matching preview" : "Activation history"}{" "}
            {preview
              ? `(${preview.filter((p) => p.matches).length} matches)`
              : ""}
          </summary>
          <div>
            {preview
              ? preview.map((p) => (
                  <p key={p.item.number}>
                    <strong>
                      #{p.item.number} {p.item.title}
                    </strong>{" "}
                    · {p.matches ? "Matches" : "Skipped"} — {p.reason}
                  </p>
                ))
              : runs
                  .filter((r) => r.workflow.id === current?.id)
                  .slice(-30)
                  .reverse()
                  .map((r) => (
                    <p key={r.id}>
                      <strong>
                        #{r.item.number} · {r.status}
                      </strong>{" "}
                      — {r.reason}
                      {r.requestId && ` · Task ${r.requestId}`}
                    </p>
                  ))}
            {preview && (
              <button onClick={() => setPreview(null)}>Close preview</button>
            )}
          </div>
        </details>
      )}
      <footer className="wf-footer">
        <span>Versioned definitions · workflows.json</span>
        <span>
          {current?.enabled ? "Automation enabled" : "Drafts do not run"}
        </span>
      </footer>
    </div>
  );
}
let root;
window.mountWorkflowEditor = () => {
  if (root) return;
  const container = document.getElementById("workflow-editor-root");
  if (!container) return;
  root = createRoot(container);
  root.render(
    <ReactFlowProvider>
      <Editor />
    </ReactFlowProvider>,
  );
};
