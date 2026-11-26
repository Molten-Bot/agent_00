import React, { createContext, useCallback, useContext, useEffect, useEffectEvent, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  ReactFlow,
  ReactFlowProvider,
  Background,
  BaseEdge,
  EdgeLabelRenderer,
  getSmoothStepPath,
  Controls,
  ControlButton,
  MiniMap,
  Handle,
  Position,
  useUpdateNodeInternals,
  useReactFlow,
} from "@xyflow/react";
import {
  Zap,
  Rows3, Columns3,
  CalendarClock, FileJson, SquarePen, MessageSquare, GitPullRequest, CircleDot, Wrench,
  GitBranch,
  FolderGit2,
  ListFilter,
  MessageSquareCode,
  RefreshCcwDot,
  ChevronDown,
  Plus,
  Save,
  Trash2,
  Download,
  Maximize2,
  Play,
  Pause,
  DiagramFlowVertical,
  X,
} from "lucide-react";
import "@xyflow/react/style.css";
import "./style.css";
import { ReviewPasses } from "./review-passes.jsx";
import { ComplexitySlider } from "./complexity-slider.jsx";
import { ScheduleInterval } from "./schedule-interval.jsx";
import { RepositoryPicker } from "./repository-picker.jsx";
import { actionFailedEvent, events, newWorkflow, diagram, replaceDefinition, workflowTemplates, workflowSettings, incompleteWorkflow, automaticLayout } from "./model.js";
const icons = {
  calendar: CalendarClock, json: FileJson, prompt: SquarePen, chat: MessageSquare, pullRequest: GitPullRequest, issue: CircleDot, repair: Wrench,
  repository: FolderGit2,
  trigger: Zap,
  filters: ListFilter,
  review: MessageSquareCode,
  merge: GitBranch,
};
const colors = {
  repository: "#66b5eb",
  trigger: "#50bfa5",
  filters: "#e5ae58",
  task: "#a18aff",
  review: "#66b5eb",
  reviewAgent: "#a18aff",
  merge: "#de8fbd",
};
const AgentLogoContext = createContext("");
function StepNode({ id, data, selected, sourcePosition, targetPosition }) {
  const updateNodeInternals = useUpdateNodeInternals();
  useEffect(() => { updateNodeInternals(id); }, [id, sourcePosition, targetPosition, data.reviewAgentSide, updateNodeInternals]);
  const agentLogoURL = useContext(AgentLogoContext);
  const Icon = icons[data.icon] || icons[data.kind];
  return (
    <div
      className={`wf-node ${selected ? "wf-node-selected" : ""}`}
      style={{ "--step-color": colors[data.kind] }}
    >
      {targetPosition && (
        <Handle type="target" position={targetPosition} />
      )}
      <div className="wf-node-top">
        <span className="wf-icon">
          {Icon ? <Icon size={17} /> : <img src={agentLogoURL} width={17} height={17} alt="" draggable={false} />}
        </span>
        <span>{data.label}</span>
        <span className="wf-node-status">
          {data.enabled ? "Enabled" : "Draft"}
        </span>
      </div>
      <strong>{data.label}</strong>
      <p>{data.detail}</p>
      {sourcePosition && (
        <Handle type="source" position={sourcePosition} />
      )}
      {data.reviewAgentSide && (
        <Handle id="reviewAgent" type="source" position={data.reviewAgentSide} />
      )}
    </div>
  );
}
const nodeTypes = { workflow: StepNode };
function ReviewCycleEdge(props) {
  const [path, x, y] = getSmoothStepPath(props);
  const { sourceX, sourceY, targetX, targetY, sourcePosition } = props;
  const horizontal = sourcePosition === Position.Left || sourcePosition === Position.Right;
  const returnX = (sourceX + targetX) / 2 + (horizontal ? 0 : 48);
  const returnY = (sourceY + targetY) / 2 + (horizontal ? 48 : 0);
  const returnPath = `M ${targetX},${targetY} Q ${returnX},${returnY} ${sourceX},${sourceY}`;
  return (
    <>
      <BaseEdge path={path} markerEnd={props.markerEnd} />
      <BaseEdge path={returnPath} markerEnd={props.markerEnd} interactionWidth={0} />
      <EdgeLabelRenderer>
        <span className="wf-review-cycle-label" aria-hidden="true"
          style={{ transform: `translate(${x}px, ${y}px) translate(-50%, -50%)` }}>
          {props.data.cycles}
          <RefreshCcwDot size={12} aria-hidden="true" />
        </span>
      </EdgeLabelRenderer>
    </>
  );
}
const edgeTypes = { reviewCycle: ReviewCycleEdge };
async function api(path, options) {
  const response = await fetch(path, options);
  const data = await response.json();
  if (!response.ok) {
    const error = new Error(data.error || "Request failed");
    error.status = response.status;
    throw error;
  }
  return data;
}
const jsonOptions = (method, value) => ({
  method,
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(value),
});
function repositoryDiscoveryError(error) {
  const message = error.message;
  if (error.status === 429 || /rate limit|too many requests|\b(?:HTTP |status=)429\b/i.test(message)) {
    const retryAt = message.match(/\bretry at (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)\b/i)?.[1];
    return retryAt && Number.isFinite(Date.parse(retryAt))
      ? `GitHub rate limited repository discovery. Retry after ${retryAt.replace("T", " ").replace("Z", " UTC")}.`
      : "GitHub rate limited repository discovery. Retry after the rate limit resets.";
  }
  return "Unable to discover GitHub repositories. Retry or check repository access in Settings.";
}
const autosaveDelay = 400;
// Browsers limit the combined body size of outstanding keepalive requests.
const canKeepAlive = (body) => new TextEncoder().encode(body).length <= 64 * 1024;
const pendingWorkflowSaveKey = "hubui.workflowPendingSave.v1";
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
function WorkflowCanvas({ workflow, saving, onInit, onDragChange, onMove, onSelect, onArrange }) {
  const flow = useReactFlow();
  const pointerDragging = useRef(false);
  const graph = useMemo(() => workflow ? diagram(workflow) : { nodes: [], edges: [] }, [workflow]);
  const [layout, setLayout] = useState({ graph, nodes: graph.nodes });

  if (layout.graph !== graph) {
    const previous = new Map(layout.nodes.map(node => [node.id, node]));
    const nodes = graph.nodes.map(node => {
      const existing = previous.get(node.id);
      if (!existing) return node;
      return {
        ...node,
        measured: existing.measured,
        selected: existing.selected,
        dragging: existing.dragging,
        position: existing.dragging || pointerDragging.current ? existing.position : node.position,
      };
    });
    setLayout({ graph, nodes });
  }

  useEffect(() => {
    const handler = (e) => {
      if (e.detail !== "workflow") return;
      requestAnimationFrame(() => flow.fitView({ padding: 32, duration: 200 }));
    };
    window.addEventListener("workflow-display", handler);
    return () => window.removeEventListener("workflow-display", handler);
  }, []);

  useEffect(() => () => {
    pointerDragging.current = false;
    onDragChange(false);
  }, [onDragChange]);

  const handleNodesChange = useCallback(changes => {
    setLayout(previous => ({ ...previous, nodes: changes.applyTo(previous.nodes) }));
    const positions = changes.toArray().filter(change => change.type === "position" && change.position);
    if (positions.length) onDragChange(pointerDragging.current || positions.some(change => change.dragging));
    // Keep handles on the same side during a drag and reroute when it ends.
    const moves = positions.filter(change => !change.dragging);
    if (moves.length) onMove(Object.fromEntries(moves.map(change => [change.id, change.position])));
  }, [onDragChange, onMove]);

  function arrange(direction) {
    if (!workflow) return;
    onArrange(automaticLayout(workflow, direction, flow.getNodes()));
    requestAnimationFrame(() => requestAnimationFrame(() => flow.fitView({ padding: 32, duration: 200 })));
  }

  return (
    <ReactFlow
      proOptions={{ hideAttribution: true }}
      defaultMarkerColor={null}
      nodes={layout.nodes}
      edges={graph.edges}
      nodeTypes={nodeTypes}
      edgeTypes={edgeTypes}
      onInit={onInit}
      onNodesChange={handleNodesChange}
      onNodeClick={(_, node) => onSelect(node.id)}
      onNodeDragStart={() => { pointerDragging.current = true; onDragChange(true); }}
      onNodeDragStop={() => { pointerDragging.current = false; onDragChange(false); }}
      nodesDraggable={!saving}
      nodesConnectable={false}
      edgesReconnectable={false}
      deleteKeyCode={null}
      fitView
      fitViewOptions={{ padding: 32 }}
      minZoom={0.25}
      maxZoom={1.6}
    >
      <Background gap={22} />
      <Controls showInteractive={false}>
        <ControlButton aria-label="Arrange workflow vertically" title="Automatic vertical layout" disabled={!workflow} onClick={() => arrange("vertical")}>
          <Rows3 />
        </ControlButton>
        <ControlButton aria-label="Arrange workflow horizontally" title="Automatic horizontal layout" disabled={!workflow} onClick={() => arrange("horizontal")}>
          <Columns3 />
        </ControlButton>
      </Controls>
      <MiniMap
        style={{ width: 120, height: 80 }}
        nodeColor={(node) => colors[node.data.kind]}
        pannable
        zoomable
      />
    </ReactFlow>
  );
}
function Editor({ defaultComplexity }) {
  const [editor, setEditor] = useState({ doc: null, current: null, savedSettings: null });
  const latest = useRef(editor);
  const write = useRef(null);
  const updateEditor = useCallback((update) => {
    latest.current = { ...latest.current, ...update };
    setEditor(latest.current);
  }, []);
  const { doc, current, savedSettings } = editor;
  const [repo, setRepo] = useState(""),
    [repos, setRepos] = useState([]),
    [reposLoading, setReposLoading] = useState(true),
    [reposError, setReposError] = useState(""),
    [libraryTasks, setLibraryTasks] = useState([]),
    [scheduleStatuses, setScheduleStatuses] = useState([]),
    [selection, setSelection] = useState("trigger"),
    [notice, setNotice] = useState(""),
    [saveError, setSaveError] = useState(""),
    [preview, setPreview] = useState(null),
    [busy, setBusy] = useState(false),
    [saving, setSaving] = useState(false),
    [templatesOpen, setTemplatesOpen] = useState(true);
  const fitView = useRef(null);
  const reposRequestPending = useRef(false);
  const [dragging, setDragging] = useState(false);
  const settings = current && workflowSettings(current);
  const dirty = !!current && settings !== savedSettings;
  const incomplete = dirty ? incompleteWorkflow(current, libraryTasks) : "";
  function openDefinition(definition, saved = false) {
    updateEditor({
      current: definition,
      savedSettings: saved ? workflowSettings(definition) : null,
    });
    setPreview(null);
    setSaveError("");
    setNotice("");
  }
  async function changeRepository(repository, onChanged) {
    if (dragging || !await flush()) return;
    setRepo(repository);
    openDefinition(null);
    onChanged?.();
  }
  async function reload() {
    try {
      let d = await api("/api/workflows");
      updateEditor({ doc: d });
      let pending;
      try {
        pending = JSON.parse(sessionStorage.getItem(pendingWorkflowSaveKey) || "null");
      } catch (_err) {
        return;
      }
      if (pending?.workflow?.id) {
        const saved = d.workflows.find((workflow) => workflow.id === pending.workflow.id);
        if (saved && workflowSettings(saved) === workflowSettings(pending.workflow)) {
          sessionStorage.removeItem(pendingWorkflowSaveKey);
        } else {
          for (let attempt = 0; attempt < 3; attempt += 1) {
            try {
              const before = d.workflows.find((w) => w.id === pending.workflow.id);
              if (JSON.stringify(before ?? null) !== JSON.stringify(pending.base)) throw new Error("Workflow changed elsewhere. Reload the saved workflow before editing.");
              d = await api("/api/workflows", jsonOptions("PUT", replaceDefinition(d, pending.workflow)));
              sessionStorage.removeItem(pendingWorkflowSaveKey);
              break;
            } catch (e) {
              if (e.status !== 409 || attempt === 2) throw e;
              d = await api("/api/workflows");
            }
          }
        }
      }
      updateEditor({ doc: d });
    } catch (e) {
      setNotice(e.message);
    }
  }
  async function loadRepos() {
    if (reposRequestPending.current) return;
    reposRequestPending.current = true;
    setReposLoading(true);
    try {
      const data = await api("/api/github/repos", { cache: "no-store" });
      if (data.ok === false) throw new Error(data.error || "Repository discovery failed");
      const visibility = window.RepositoryVisibility;
      const discovered = data.repos ?? [];
      setRepos(visibility?.filter(discovered) ?? discovered);
      setReposError("");
    } catch (e) {
      setReposError(repositoryDiscoveryError(e));
    } finally {
      reposRequestPending.current = false;
      setReposLoading(false);
    }
  }
  useEffect(() => {
    reload();
    api("/api/library")
      .then((data) => setLibraryTasks(data.tasks || []))
      .catch((e) => setNotice(e.message));
    loadRepos();
    const refreshRepositories = () => {
      const enabled = window.RepositoryVisibility?.repositories();
      if (enabled !== null && enabled !== undefined) {
        setRepos(enabled);
      } else {
        void loadRepos();
      }
    };
    window.addEventListener("repository-visibility-changed", refreshRepositories);
    return () => window.removeEventListener("repository-visibility-changed", refreshRepositories);
  }, []);
  useEffect(() => {
    const refresh = () => {
      void api("/api/workflows/schedules")
        .then((data) => setScheduleStatuses(Array.isArray(data) ? data : []))
        .catch(() => {});
    };
    refresh();
    const timer = setInterval(refresh, 15000);
    return () => clearInterval(timer);
  }, []);
  const flushAutosave = useEffectEvent(() => { void flush(); });
  useEffect(() => {
    const handler = (e) => {
      if (e.detail !== "workflow") flushAutosave();
    };
    window.addEventListener("workflow-display", handler);
    return () => window.removeEventListener("workflow-display", handler);
  }, []);
  const warnIfSaveNeedsTime = useEffectEvent((e) => {
    const { doc, current, savedSettings } = latest.current;
    const pending = current && workflowSettings(current);
    const ready = doc && pending && (pending !== savedSettings || write.current?.settings)
      && !incompleteWorkflow(current, libraryTasks);
    // If a newer edit needs the next revision or the request exceeds the
    // keepalive limit, give the current page time to finish saving.
    if (ready && (write.current
      ? pending !== write.current.settings || !write.current.keepalive
      : !canKeepAlive(JSON.stringify(replaceDefinition(doc, current))))) {
      void flush();
      e.preventDefault();
      e.returnValue = "";
    }
  });
  useEffect(() => {
    const flushOnHide = () => flushAutosave();
    const flushOnBackground = () => {
      if (document.visibilityState === "hidden") flushAutosave();
    };
    const warnOnUnload = (e) => warnIfSaveNeedsTime(e);
    window.addEventListener("pagehide", flushOnHide);
    window.addEventListener("beforeunload", warnOnUnload);
    document.addEventListener("visibilitychange", flushOnBackground);
    return () => {
      window.removeEventListener("pagehide", flushOnHide);
      window.removeEventListener("beforeunload", warnOnUnload);
      document.removeEventListener("visibilitychange", flushOnBackground);
    };
  }, []);
  useEffect(() => {
    if (dragging || !dirty || incomplete) return;
    const timer = setTimeout(() => flushAutosave(), autosaveDelay);
    return () => clearTimeout(timer);
  }, [settings, dirty, incomplete, dragging]);
  const receiveSchedule = useEffectEvent(async () => {
    const seed = window.pendingWorkflowSchedule;
    if (!seed || !latest.current.doc || !await flush() || window.pendingWorkflowSchedule !== seed) return;
    delete window.pendingWorkflowSchedule;
    const repository =
      seed.workflowRepository ||
      seed.repos[0]
        .replace(/^https:\/\/github.com\//, "")
        .replace(/^git@github.com:/, "")
        .replace(/\.git$/, "");
    const draft = newWorkflow(repository, "schedule");
    draft.name = `Scheduled ${seed.librarytaskname}`;
    draft.task = {
      ...draft.task,
      libraryTaskName: seed.librarytaskname,
      instructions: seed.promptcontext || "",
      baseBranch: seed.branch || "",
      targetSubdir: seed.targetsubdir || ".",
      reviewers: seed.reviewers || [],
      draft: !!seed.draft,
    };
    setRepo(repository);
    openDefinition(draft);
    setSelection("trigger");
    setTimeout(() => fitView.current?.({ padding: 32 }), 0);
  });
  const loaded = !!doc;
  useEffect(() => {
    const receive = () => { void receiveSchedule(); };
    receive();
    window.addEventListener("workflow-create-schedule", receive);
    return () =>
      window.removeEventListener("workflow-create-schedule", receive);
  }, [loaded]);
  const scheduled = current?.trigger.event === "schedule";
  const inputTriggered = current?.trigger.event.startsWith("app.");
  const actionFailed = current?.trigger.event === actionFailedEvent;
  const libraryTask = libraryTasks.find(
    (t) => t.name === current?.task.libraryTaskName,
  );
  const instructionsLabel = current?.task.libraryTaskName
    ? (libraryTask?.promptContextLabel || "Additional context") +
      (libraryTask?.promptContextRequired ? " (required)" : " (optional)")
    : inputTriggered ? "Additional instructions (optional)" : "Instructions";
  const scheduleStatus = scheduleStatuses.find(
    (s) => s.workflowId === current?.id,
  );
  function changeTrigger(event) {
    edit({
      trigger:
        event === "schedule"
          ? { event, intervalHours: 24 }
          : { event, value: "" },
      includeBacklog: false,
      filters: { mode: "all", rules: [] },
      task: {
        ...current.task,
        kind: event.startsWith("pr.") ? "review" : "implement",
        ...(event.startsWith("app.") ? { libraryTaskName: undefined } : {}),
      },
    });
  }
  function moveNodes(positions) {
    const definition = latest.current.current;
    if (!definition) return;
    updateEditor({
      current: { ...definition, positions: { ...definition.positions, ...positions } },
    });
  }
  function edit(update) {
    updateEditor({ current: { ...latest.current.current, ...update } });
    setPreview(null);
    setSaveError("");
    setNotice("");
  }
  async function choose(id) {
    if (dragging) { setNotice("Finish moving the workflow before switching workflows."); return; }
    if (id === latest.current.current?.id) { fitView.current?.({ padding: 32, duration: 200 }); return; }
    if (!await flush()) return;
    const definition = latest.current.doc.workflows.find((w) => w.id === id);
    if (!definition) return;
    setRepo(definition.repository);
    openDefinition(definition, true);
    setSelection("trigger");
    setTimeout(() => fitView.current?.({ padding: 32 }), 0);
  }
  function writeDocument(document, settings, onSaved) {
    const request = jsonOptions("PUT", document);
    const keepalive = canKeepAlive(request.body);
    const workflow = document.workflows.find((candidate) => workflowSettings(candidate) === settings);
    const journal = workflow ? JSON.stringify({ workflow, base: latest.current.doc.workflows.find((w) => w.id === workflow.id) ?? null }) : "";
    if (journal) {
      try { sessionStorage.setItem(pendingWorkflowSaveKey, journal); } catch (_err) {}
    }
    setSaving(true);
    setSaveError("");
    const result = api("/api/workflows", { ...request, keepalive })
      .then((doc) => {
        if (journal) {
          try {
            if (sessionStorage.getItem(pendingWorkflowSaveKey) === journal) sessionStorage.removeItem(pendingWorkflowSaveKey);
          } catch (_err) {}
        }
        updateEditor({ doc });
        onSaved();
        return true;
      })
      .catch((e) => {
        if (e.status === 409) {
          return api("/api/workflows")
            .then((doc) => {
              const base = latest.current.doc.workflows.find((w) => w.id === workflow?.id);
              const remote = doc.workflows.find((w) => w.id === workflow?.id);
              if (!workflow || JSON.stringify(base) !== JSON.stringify(remote)) {
                setSaveError("Workflow changed elsewhere. Reload before saving.");
                if (journal && sessionStorage.getItem(pendingWorkflowSaveKey) === journal) sessionStorage.removeItem(pendingWorkflowSaveKey);
                return false;
              }
              updateEditor({ doc });
              if (write.current) write.current.conflict = true;
              setSaveError("");
              return false;
            })
            .catch((reloadError) => {
              setSaveError(reloadError.message);
              return false;
            });
        }
        setSaveError(e.message);
        return false;
      })
      .finally(() => {
        write.current = null;
        setSaving(false);
      });
    write.current = { result, settings, keepalive };
    return write.current;
  }
  function saveDefinition(definition, toggle = false) {
    const settings = workflowSettings(definition);
    const attemptedSettings = toggle ? workflowSettings(latest.current.current) : settings;
    return writeDocument(replaceDefinition(latest.current.doc, definition), attemptedSettings, () => {
      updateEditor({
        savedSettings: settings,
        ...(toggle && { current: { ...latest.current.current, enabled: definition.enabled } }),
      });
      if (toggle) setNotice(definition.enabled ? "Workflow enabled." : "Workflow paused.");
    });
  }
  async function flush() {
    // All workflow mutations share the document revision. Recheck the current
    // draft once each write finishes because edits can arrive while it runs.
    let conflictRetries = 0;
    for (;;) {
      let pending = write.current;
      if (!pending) {
        const { doc, current, savedSettings } = latest.current;
        if (!doc || !current || workflowSettings(current) === savedSettings || incompleteWorkflow(current, libraryTasks)) return true;
        pending = saveDefinition(current);
      }
      if (!await pending.result) {
        if (pending.conflict && conflictRetries < 3) {
          conflictRetries += 1;
          continue;
        }
        const current = latest.current.current;
        if (!current || !pending.settings || workflowSettings(current) === pending.settings) return false;
      }
    }
  }
  function toggleEnabled() {
    if (!write.current) saveDefinition({ ...latest.current.current, enabled: !current.enabled }, true);
  }
  async function create(kind) {
    if (dragging) return;
    if (!repo.trim()) {
      setNotice("Choose a repository first.");
      return;
    }
    if (!await flush()) return;
    openDefinition(newWorkflow(repo.trim(), kind));
    setSelection("trigger");
    setTimeout(() => fitView.current?.({ padding: 32 }), 0);
  }
  async function showPreview() {
    setBusy(true);
    try {
      const matches = await api("/api/workflows/preview", jsonOptions("POST", current));
      if (workflowSettings(current) === workflowSettings(latest.current.current || {})) {
        setPreview(matches);
        setNotice("Preview only: no tasks were started.");
      }
    } catch (e) {
      if (current.id === latest.current.current?.id) setNotice(e.message);
    } finally {
      setBusy(false);
    }
  }
  function remove() {
    if (write.current) return;
    const workflows = doc.workflows.filter((w) => w.id !== current.id);
    const removed = () => {
      openDefinition(null);
      setNotice("Workflow deleted.");
    };
    if (workflows.length === doc.workflows.length) removed();
    else writeDocument({ ...doc, workflows }, null, removed);
  }
  function exportAll() {
    const data = current ? replaceDefinition(doc, current) : doc;
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
    (w) => repos.some(r => r.full_name.toLowerCase() === w.repository.toLowerCase())
      && (!repo || w.repository.toLowerCase() === repo.toLowerCase()),
  );
  const updateTask = (field, value) =>
    edit({ task: { ...current.task, [field]: value } });
  const updateReviewAgent = (field, value) => {
    const reviewAgent = { ...current.reviewAgent, [field]: value };
    if (!value) delete reviewAgent[field];
    edit({ reviewAgent: Object.keys(reviewAgent).length ? reviewAgent : undefined });
  };
  function updateReviewCycles(reviewCycles) {
    edit({ reviewCycles });
    if (reviewCycles && !current.reviewCycles) {
      requestAnimationFrame(() => requestAnimationFrame(() => fitView.current?.({ padding: 32 })));
    }
  }
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
  const templatesLabel = templatesOpen ? "Hide workflow templates" : "Show workflow templates";
  const message = incomplete || saveError || notice;
  return (
    <div className="wf-editor">
      <header className="panel-header prompt-titlebar">
        <div className="panel-header-copy">
          <span id="workflow-title" className="panel-section-title prompt-title-list">
            <span className="prompt-title-mode active">
              <DiagramFlowVertical className="prompt-title-icon" aria-hidden="true" />
              <span>Workflow</span>
            </span>
          </span>
        </div>
        <div className="prompt-titlebar-actions wf-header-actions">
          {current && (
            <input
              className="prompt-control wf-name"
              aria-label="Workflow name"
              value={current.name}
              maxLength={100}
              onChange={(e) => edit({ name: e.target.value })}
            />
          )}
          {current && (
            <span className="wf-save-state" role="status">
              {incomplete ? "Incomplete" : saving ? "Saving…" : dirty ? "Unsaved changes" : ""}
            </span>
          )}
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
                {scheduled ? "Preview schedule" : "Preview matches"}
              </button>
              <button type="button" aria-label="Delete draft" title="Delete draft" className="wf-delete" disabled={busy || dragging || saving || current.enabled} onClick={remove}>
                <Trash2 size={15} aria-hidden="true" />
              </button>
              <button
                type="button"
                className="wf-primary"
                aria-label={current.enabled ? "Pause" : "Enable"}
                title={current.enabled ? "Pause" : "Enable"}
                disabled={busy || dragging || saving || dirty}
                onClick={toggleEnabled}
              >
                {current.enabled ? <Pause size={15} aria-hidden="true" /> : <Play size={15} aria-hidden="true" />}
              </button>
            </>
          )}
          <button type="button" data-workflow-fullscreen aria-label="Maximize Workflow" title="Maximize Workflow"
            aria-controls="task-fullscreen" aria-expanded="false">
            <Maximize2 size={16} aria-hidden="true" />
          </button>
        </div>
      </header>
      <div className="wf-workspace">
        <aside className="wf-palette" aria-label="Repository workflows">
          <div className="wf-section-label">REPOSITORY</div>
          <RepositoryPicker
            repositories={repos}
            value={repo}
            loading={reposLoading}
            error={reposError}
            onRetry={loadRepos}
            autoSelect={!dirty}
            onChange={changeRepository}
          />
          <div className="wf-step-list">
            {visible.map((w) => {
              const status = w.enabled ? "Running" : "Paused";
              const StatusIcon = w.enabled ? Play : Pause;
              return (
              <button
                key={w.id}
                className={`wf-step ${current?.id === w.id ? "wf-active" : ""}`}
                aria-label={`${w.name}, ${w.repository}, ${status}`}
                onClick={() => choose(w.id)}
              >
                <span>
                  <strong>{w.name}</strong>
                  <small>
                    {w.repository} · {status}
                  </small>
                </span>
                <StatusIcon size={14} aria-hidden="true" />
              </button>
              );
            })}
          </div>
          <div className="wf-templates-heading">
            <span className="wf-section-label">WORKFLOW TEMPLATES</span>
            <button type="button" className="wf-templates-toggle" aria-label={templatesLabel} title={templatesLabel}
              aria-expanded={templatesOpen} aria-controls="wf-templates" onClick={() => setTemplatesOpen(open => !open)}>
              <ChevronDown size={14} aria-hidden="true" />
            </button>
          </div>
          <div id="wf-templates" hidden={!templatesOpen}>
            {workflowTemplates.map((template) => (
              <button
                className="wf-template wf-template-card"
                key={template.id}
                aria-label={template.name}
                onClick={() => create(template.id)}
                disabled={!doc || !repos.some((r) => r.full_name === repo)}
              >
                <Plus size={14} />
                <span><strong>{template.name}</strong><small>{template.description}</small></span>
              </button>
            ))}
          </div>
        </aside>
        <div className="wf-canvas">
          <div className="wf-canvas-caption">
            {current ? "DEFINITION CANVAS" : "CHOOSE OR CREATE A WORKFLOW"}
            <span>{current?.repository}</span>
          </div>
          {/* Keep late drag events in the store for the workflow where they started. */}
          <ReactFlowProvider key={current?.id}>
            <WorkflowCanvas
              workflow={current}
              saving={saving}
              onInit={flow => { fitView.current = flow.fitView; }}
              onDragChange={setDragging}
              onMove={moveNodes}
              onSelect={setSelection}
              onArrange={moveNodes}
            />
          </ReactFlowProvider>
          {message && (
            <div className={incomplete || saveError ? "wf-notice wf-notice-error" : "wf-notice"} role="status">
              <div>
                <strong>{message}</strong>
                {!incomplete && <button
                  aria-label="Dismiss message"
                  onClick={() => { setNotice(""); setSaveError(""); }}
                >
                  <X size={14} />
                </button>}
              </div>
              {dirty && !saving && (
                <button
                  onClick={() => {
                    const stored = doc.workflows.find((w) => w.id === current.id);
                    openDefinition(stored || null, !!stored);
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
          {current && <div className="wf-composition">
            <span className="wf-section-label">COMPOSE STEPS</span>
            {current.task.kind !== "review" && <Toggle label="Review step" value={current.reviewCycles > 0} onChange={(enabled) => { updateReviewCycles(enabled ? 1 : 0); setSelection(enabled ? "review" : "task"); }} />}
            <Toggle label="Merge step" value={current.merge.enabled} onChange={(enabled) => { updateMerge("enabled", enabled); setSelection(enabled ? "merge" : "task"); }} />
          </div>}

          <div className="wf-section-label">{selection === "reviewAgent" ? "REVIEW AGENT" : selection.toUpperCase()}</div>
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
                  {!scheduled && !inputTriggered && (
                    <>
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
                </>
              )}
              {selection === "trigger" && (
                <>
                  <h2>What starts work?</h2>
                  <Field label="Trigger type">
                    <select
                      value={current.trigger.event}
                      onChange={(e) => changeTrigger(e.target.value)}
                    >
                      {events.map((e) => (
                        <option key={e} value={e}>
                          {e === "schedule"
                            ? "Scheduled"
                            : e === actionFailedEvent ? "GitHub Actions failure"
                              : e.startsWith("app.") ? `${e.slice(4)} submission` : e.replaceAll(".", " · ").replaceAll("_", " ")}
                        </option>
                      ))}
                    </select>
                  </Field>
                  {actionFailed && (
                    <Field label="Workflow filename or ID">
                      <input
                        placeholder="deploy-prod.yml"
                        value={current.trigger.value || ""}
                        onChange={(e) =>
                          edit({ trigger: { ...current.trigger, value: e.target.value } })
                        }
                      />
                    </Field>
                  )}
                  {scheduled && (
                    <>
                      <ScheduleInterval
                        key={current.id}
                        hours={current.trigger.intervalHours}
                        onChange={(intervalHours) => edit({ trigger: { ...current.trigger, intervalHours } })}
                      />
                      <Field
                        label={`First run (optional, ${Intl.DateTimeFormat().resolvedOptions().timeZone})`}
                      >
                        <input
                          type="datetime-local"
                          value={
                            current.trigger.startAt
                              ? localDateInput(current.trigger.startAt)
                              : ""
                          }
                          onChange={(e) =>
                            edit({
                              trigger: {
                                ...current.trigger,
                                startAt: e.target.value
                                  ? new Date(e.target.value).toISOString()
                                  : undefined,
                              },
                            })
                          }
                        />
                      </Field>
                      <p className="wf-muted">
                        Without a first-run time, the first run is one interval
                        after enabling. Keep the app running. Missed intervals
                        produce one catch-up run; overlapping runs wait.
                      </p>
                      <p className="wf-muted">
                        {dirty
                          ? "Schedule updates after valid changes are saved."
                          : scheduleStatus?.nextRunAt &&
                              !scheduleStatus.nextRunAt.startsWith("0001")
                            ? `Next due: ${new Date(scheduleStatus.nextRunAt).toLocaleString()}${scheduleStatus.enabled ? "" : " (paused)"}`
                            : "The next due time is set when enabled."}
                      </p>
                    </>
                  )}
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
                  {!scheduled && !inputTriggered && (
                    <>
                      <Toggle
                        label="Include existing matches on first activation"
                        value={current.includeBacklog}
                        onChange={(v) => edit({ includeBacklog: v })}
                      />
                      <p className="wf-muted">
                        {actionFailed
                          ? "Checks the latest 100 runs of this workflow every 30 seconds. Each failed attempt can start work once."
                          : "GitHub is checked every 30 seconds. A match must stop matching before it can activate again."}
                      </p>
                    </>
                  )}
                </>
              )}
              {selection === "filters" && !scheduled && !inputTriggered && !actionFailed && (
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
                  <ComplexitySlider
                    value={current.task.complexity}
                    defaultComplexity={defaultComplexity}
                    onChange={(value) => updateTask("complexity", value)}
                  />
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
                  {inputTriggered && <p className="wf-muted">The submitted prompt, images, and JSON options are kept. Instructions below add context. Without an enabled custom workflow, existing configuration defaults apply.</p>}
                  {!inputTriggered && <Field label="Prompt source">
                    <select
                      aria-label="Prompt source"
                      value={current.task.libraryTaskName || ""}
                      onChange={(e) =>
                        edit({
                          task: {
                            ...current.task,
                            libraryTaskName: e.target.value || undefined,
                            instructions: e.target.value
                              ? ""
                              : current.task.instructions,
                          },
                        })
                      }
                    >
                      <option value="">Inline instructions</option>
                      {current.task.libraryTaskName && !libraryTask && (
                        <option value={current.task.libraryTaskName}>
                          {current.task.libraryTaskName} (unavailable)
                        </option>
                      )}
                      {libraryTasks.map((t) => (
                        <option value={t.name} key={t.name}>
                          {t.displayName || t.name}
                          {t.sourceName ? ` · ${t.sourceName}` : ""}
                        </option>
                      ))}
                    </select>
                  </Field>}
                  {current.task.libraryTaskName && (
                    <>
                      <p className="wf-muted">
                        The current library prompt is resolved at each run. Its
                        expanded instructions are recorded with the task.
                      </p>
                      {libraryTask && (
                        <details className="wf-prompt-preview">
                          <summary>Preview library prompt</summary>
                          <pre>{libraryTask.prompt}</pre>
                        </details>
                      )}
                    </>
                  )}
                  <Field label={instructionsLabel}>
                    <textarea
                      aria-label={instructionsLabel}
                      rows={6}
                      value={current.task.instructions}
                      onChange={(e) =>
                        updateTask("instructions", e.target.value)
                      }
                      placeholder={libraryTask?.promptContextPlaceholder || ""}
                    />
                  </Field>
                  {!current.trigger.event.startsWith("pr.") && (
                    <Field
                      label={
                        libraryTask?.requiresNonDefaultBranch
                          ? "Base branch (non-default required)"
                          : "Base branch (optional)"
                      }
                    >
                      <input
                        value={current.task.baseBranch || ""}
                        onChange={(e) =>
                          updateTask("baseBranch", e.target.value)
                        }
                        placeholder="Repository default"
                      />
                    </Field>
                  )}
                  <Field label="Target directory (optional)">
                    <input
                      value={current.task.targetSubdir || ""}
                      onChange={(e) =>
                        updateTask("targetSubdir", e.target.value)
                      }
                      placeholder="Library default or ."
                    />
                  </Field>
                  <Field label="PR reviewers (comma-separated)">
                    <input
                      value={(current.task.reviewers || []).join(", ")}
                      onChange={(e) =>
                        updateTask(
                          "reviewers",
                          e.target.value.split(",").map((v) => v.trim()),
                        )
                      }
                      onBlur={() => {
                        if (current.task.reviewers?.some((reviewer) => !reviewer)) {
                          updateTask("reviewers", current.task.reviewers.filter(Boolean));
                        }
                      }}
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
                  <ReviewPasses value={current.reviewCycles} onChange={updateReviewCycles} />
                </>
              )}
              {selection === "reviewAgent" && (
                <>
                  <h2>Review agent</h2>
                  <ComplexitySlider
                    label="Review complexity"
                    inheritedLabel="Use agent task complexity"
                    value={current.reviewAgent?.complexity}
                    defaultComplexity={current.task.complexity || defaultComplexity}
                    onChange={(value) => updateReviewAgent("complexity", value)}
                  />
                  <Field label="Review prompt (optional)">
                    <textarea
                      aria-label="Review prompt (optional)"
                      rows={6}
                      value={current.reviewAgent?.instructions || ""}
                      onChange={(e) => updateReviewAgent("instructions", e.target.value)}
                      placeholder="Check for regressions, missing tests, and unintended changes"
                    />
                  </Field>
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
            </>
          )}
        </aside>
      </div>
      {preview && (
        <details className="wf-preview" open>
          <summary>Matching preview ({preview.filter((p) => p.matches).length} matches)</summary>
          <div>
            {preview.map((p) => (
              <p key={p.item.actionRunId ?? p.item.number}>
                <strong>
                  {p.item.kind === "action" ? `Run #${p.item.actionRunId}, attempt ${p.item.actionRunAttempt} · `
                    : p.item.kind === "schedule" ? "" : `#${p.item.number} `}
                  {p.item.title}
                </strong>{" "}
                · {p.matches ? "Matches" : "Skipped"} — {p.reason}
              </p>
            ))}
            <button onClick={() => setPreview(null)}>Close preview</button>
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
function localDateInput(value) {
  const d = new Date(value);
  return new Date(d.getTime() - d.getTimezoneOffset() * 60000)
    .toISOString()
    .slice(0, 16);
}
let root;
let renderedAgentLogoURL;
let renderedDefaultComplexity;
window.mountWorkflowEditor = (agentLogoURL, defaultComplexity) => {
  if (root && renderedAgentLogoURL === agentLogoURL && renderedDefaultComplexity === defaultComplexity) return;
  const container = document.getElementById("workflow-editor-root");
  if (!container) return;
  root ??= createRoot(container);
  renderedAgentLogoURL = agentLogoURL;
  renderedDefaultComplexity = defaultComplexity;
  root.render(
    <AgentLogoContext.Provider value={agentLogoURL}>
      <ReactFlowProvider>
        <Editor defaultComplexity={defaultComplexity} />
      </ReactFlowProvider>
    </AgentLogoContext.Provider>,
  );
};
