import React, { useCallback, useEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { ReactFlow, ReactFlowProvider, Background, Controls, MiniMap, Handle, Position, useReactFlow, addEdge } from '@xyflow/react';
import { Zap, Bot, GitBranch, UserRoundCheck, Send, Plus, Search, Save, Download, Check, CircleCheck, X, Trash2, MousePointer2, ArrowDown, DiagramFlowVertical, Maximize, CircleAlert } from 'lucide-react';
import '@xyflow/react/style.css';
import './style.css';
import { STORAGE_KEY, stepTypes, makeNode, exampleWorkflow, validConnection, validateWorkflow, serializeDraft, readDraft } from './model.js';
const icons = { trigger: Zap, agent: Bot, condition: GitBranch, approval: UserRoundCheck, action: Send };
function StepNode({ data, selected }) {
  const spec = stepTypes[data.kind]; const Icon = icons[data.kind];
  return <div className={`wf-node ${selected ? 'wf-node-selected' : ''}`} style={{ '--step-color': spec.color }}>
    {data.kind !== 'trigger' && <Handle type="target" position={Position.Top} id="in" aria-label="Step input" />}
    <div className="wf-node-top"><span className="wf-icon"><Icon size={17}/></span><span>{spec.label}</span><span className="wf-node-status">Draft</span></div>
    <strong>{data.label || 'Untitled step'}</strong>
    <p>{data.detail || 'Select to configure'}</p>
    {data.kind === 'condition' ? <div className="wf-branches"><span>No</span><span>Yes</span><Handle type="source" position={Position.Bottom} id="no" style={{left:'25%'}} aria-label="No branch"/><Handle type="source" position={Position.Bottom} id="yes" style={{left:'75%'}} aria-label="Yes branch"/></div> : data.kind !== 'action' && <Handle type="source" position={Position.Bottom} id="out" aria-label="Step output"/>}
  </div>;
}
const nodeTypes = { workflow: StepNode };
const defaultEdgeOptions = { type: 'smoothstep', style: { strokeWidth: 1.6 }, interactionWidth: 24 };
function loadInitial() {
  try { const raw = localStorage.getItem(STORAGE_KEY); return raw ? { draft: readDraft(raw), saved: true } : { draft: exampleWorkflow() }; }
  catch { return { draft: exampleWorkflow(), warning: 'Your saved draft could not be loaded. Showing the example instead.' }; }
}
function Editor() {
  const [initial] = useState(loadInitial);
  const [nodes, setNodes] = useState(initial.draft.nodes);
  const [edges, setEdges] = useState(initial.draft.edges);
  const [name, setName] = useState(initial.draft.name);
  const [query, setQuery] = useState('');
  const [selection, setSelection] = useState(null);
  const [saved, setSaved] = useState(Boolean(initial.saved));
  const [notice, setNotice] = useState(initial.warning ? { error: true, messages: [initial.warning] } : null);
  const flow = useReactFlow();
  const canvas = useRef(null);
  const selected = nodes.find(n => n.id === selection);
  const selectedEdge = edges.find(e => e.id === selection);
  const dirty = () => { setSaved(false); setNotice(null); };
  const onNodesChange = useCallback(changes => {
    setNodes(current => changes.applyTo(current));
    if (changes.toArray().some(c => c.type !== 'select' && c.type !== 'dimensions')) { setSaved(false); setNotice(null); }
  }, []);
  const onEdgesChange = useCallback(changes => {
    setEdges(current => changes.applyTo(current));
    if (changes.toArray().some(c => c.type !== 'select')) { setSaved(false); setNotice(null); }
  }, []);
  useEffect(() => {
    const onDisplay = event => { if (event.detail === 'workflow') requestAnimationFrame(() => flow.fitView({ padding: 0.18, duration: 200 })); };
    window.addEventListener('workflow-display', onDisplay);
    return () => window.removeEventListener('workflow-display', onDisplay);
  }, [flow]);
  const onConnect = useCallback(connection => {
    if (!validConnection(connection, nodes, edges)) return;
    const label = connection.sourceHandle === 'yes' ? 'Yes' : connection.sourceHandle === 'no' ? 'No' : undefined;
    setEdges(current => addEdge({ ...connection, label }, current)); dirty();
  }, [nodes, edges]);
  function addStep(kind, position) {
    const bounds = canvas.current.getBoundingClientRect();
    const point = position || flow.screenToFlowPosition({ x: bounds.left + bounds.width / 2 - 100, y: bounds.top + bounds.height / 2 - 50 });
    const node = makeNode(kind, point);
    setNodes(current => [...current.map(n => ({ ...n, selected: false })), { ...node, selected: true }]);
    setSelection(node.id); dirty();
  }
  function updateStep(field, value) { setNodes(current => current.map(n => n.id === selection ? { ...n, data: { ...n.data, [field]: value } } : n)); dirty(); }
  function removeSelected() {
    setNodes(current => current.filter(n => n.id !== selection));
    setEdges(current => current.filter(e => e.id !== selection && e.source !== selection && e.target !== selection));
    setSelection(null); dirty();
  }
  function save() {
    try { localStorage.setItem(STORAGE_KEY, JSON.stringify(serializeDraft(name, nodes, edges))); setSaved(true); setNotice({ messages: ['Draft saved in this browser.'] }); }
    catch { setNotice({ error: true, messages: ['Unable to save in this browser. Export your draft to keep a copy.'] }); }
  }
  function validate() {
    const errors = validateWorkflow(nodes, edges);
    setNotice({ error: errors.length > 0, messages: errors.length ? errors : ['Workflow structure looks good. This POC does not execute steps.'] });
  }
  function exportDraft() {
    const url = URL.createObjectURL(new Blob([JSON.stringify(serializeDraft(name, nodes, edges), null, 2)], { type: 'application/json' }));
    const link = document.createElement('a'); link.href = url; link.download = 'workflow.json'; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  return <div className="wf-editor">
    <header className="wf-header">
      <div className="wf-heading"><div className="wf-eyebrow"><DiagramFlowVertical size={15}/><h1 id="workflow-title">Workflow</h1><span className="wf-beta">Beta</span></div><input className="wf-name" aria-label="Workflow name" value={name} maxLength={100} onChange={e => { setName(e.target.value); dirty(); }}/></div>
      <div className="wf-header-actions"><span className="wf-save-state">{saved ? <Check size={13}/> : <span className="wf-dot"/>}{saved ? 'Saved locally' : 'Unsaved draft'}</span><button onClick={exportDraft} title="Export workflow JSON" aria-label="Export workflow JSON"><Download size={16}/></button><button onClick={validate}><CircleCheck size={15}/>Validate</button><button className="wf-primary" onClick={save}><Save size={15}/>Save draft</button></div>
    </header>
    <div className="wf-workspace">
      <aside className="wf-palette" aria-label="Step palette">
        <div className="wf-section-label">BUILD YOUR WORKFLOW</div><h2>Add a step</h2><p className="wf-muted">Click or drag onto the canvas.</p>
        <label className="wf-search"><Search size={14}/><input aria-label="Search steps" placeholder="Search steps…" value={query} onChange={e => setQuery(e.target.value)}/></label>
        <div className="wf-step-list">{Object.entries(stepTypes).filter(([,s]) => `${s.label} ${s.description}`.toLowerCase().includes(query.toLowerCase())).map(([kind,spec]) => { const Icon = icons[kind]; return <button className="wf-step" key={kind} onClick={() => addStep(kind)} draggable onDragStart={e => { e.dataTransfer.setData('application/agent00-step', kind); e.dataTransfer.effectAllowed = 'copy'; }} style={{ '--step-color':spec.color }}><span className="wf-icon"><Icon size={18}/></span><span><strong>{spec.label}</strong><small>{spec.description}</small></span><Plus size={13}/></button>; })}</div>
        <div className="wf-palette-note"><span className="wf-section-label">A GOOD PLACE TO START</span><p>A trigger starts the flow. Connect steps to define what happens next.</p><div><span>Trigger</span><ArrowDown size={12}/><span>Agent</span><ArrowDown size={12}/><span>Action</span></div></div>
      </aside>
      <div className="wf-canvas" ref={canvas} onDragOver={e => { e.preventDefault(); e.dataTransfer.dropEffect = 'copy'; }} onDrop={e => { e.preventDefault(); const kind = e.dataTransfer.getData('application/agent00-step'); if (Object.hasOwn(stepTypes,kind)) addStep(kind, flow.screenToFlowPosition({x:e.clientX,y:e.clientY})); }}>
        <div className="wf-canvas-caption"><span className="wf-dot"/>DEFINITION CANVAS<span>{nodes.length} steps · {edges.length} connections</span></div>
        <ReactFlow nodes={nodes} edges={edges} nodeTypes={nodeTypes} onNodesChange={onNodesChange} onEdgesChange={onEdgesChange} onConnect={onConnect} isValidConnection={connection => validConnection(connection,nodes,edges)} defaultEdgeOptions={defaultEdgeOptions} onNodeClick={(_,n) => setSelection(n.id)} onEdgeClick={(_,e) => setSelection(e.id)} onPaneClick={() => setSelection(null)} onSelectionChange={({nodes,edges}) => { if (nodes.length === 1) setSelection(nodes[0].id); else if (edges.length === 1) setSelection(edges[0].id); else setSelection(null); }} fitView fitViewOptions={{padding:0.18}} minZoom={0.25} maxZoom={1.6} deleteKeyCode={['Backspace','Delete']}>
          <Background gap={22} size={1}/><Controls showInteractive={false}/><MiniMap nodeColor={n => stepTypes[n.data.kind]?.color || '#888'} pannable zoomable/>
        </ReactFlow>
        {notice && <div className={`wf-notice ${notice.error ? 'wf-notice-error' : ''}`} role="status"><div>{notice.error ? <CircleAlert size={17}/> : <CircleCheck size={17}/>}<strong>{notice.error ? 'Needs attention' : 'All set'}</strong><button onClick={() => setNotice(null)} aria-label="Dismiss message"><X size={14}/></button></div><ul>{notice.messages.map(message => <li key={message}>{message}</li>)}</ul></div>}
      </div>
      <aside className="wf-inspector" aria-label="Step settings">
        <div className="wf-inspector-heading"><span className="wf-section-label">{selectedEdge ? 'CONNECTION' : 'STEP SETTINGS'}</span>{selection && <button onClick={() => setSelection(null)} aria-label="Close step settings"><X size={15}/></button>}</div>
        {selected ? <><div className="wf-inspector-title" style={{'--step-color':stepTypes[selected.data.kind].color}}><span className="wf-icon">{React.createElement(icons[selected.data.kind],{size:20})}</span><h2>{stepTypes[selected.data.kind].label}</h2></div><label className="wf-field">Step name<input value={selected.data.label} maxLength={100} onChange={e => updateStep('label',e.target.value)}/></label><label className="wf-field">{selected.data.kind === 'agent' ? 'Instructions' : selected.data.kind === 'condition' ? 'Rule' : selected.data.kind === 'trigger' ? 'Trigger label' : selected.data.kind === 'approval' ? 'Review instructions' : 'Action'}<textarea aria-label="Step configuration" rows={6} value={selected.data.detail} onChange={e => updateStep('detail',e.target.value)} placeholder="Describe this step…"/></label><p className="wf-muted">{selected.data.kind === 'condition' ? 'Connect the Yes and No outputs to choose what happens for each result.' : selected.data.kind === 'approval' ? 'In a running workflow, this step would wait for a person to approve.' : 'Drag a connection from a step’s output dot to another step’s input dot.'}</p><button className="wf-delete" onClick={removeSelected}><Trash2 size={14}/>Delete step</button></> : selectedEdge ? <><h2>Edit connection</h2><label className="wf-field">Label<input value={selectedEdge.label || ''} maxLength={60} onChange={e => { setEdges(current => current.map(edge => edge.id === selection ? {...edge,label:e.target.value} : edge)); dirty(); }}/></label><button className="wf-delete" onClick={removeSelected}><Trash2 size={14}/>Delete connection</button></> : <div className="wf-inspector-empty"><span><MousePointer2 size={24}/></span><h2>Make it your workflow</h2><p>Select a step to edit its instructions, or select a connection to label the path.</p><div className="wf-key-hint"><kbd>⌫</kbd> Delete selected element</div><button onClick={() => flow.fitView({padding:0.18,duration:200})}><Maximize size={14}/>Fit all steps</button></div>}
      </aside>
    </div>
    <footer className="wf-footer"><span><span className="wf-dot"/>Editor preview · Steps do not run yet</span><span>Drafts stay in this browser</span></footer>
  </div>;
}
let root;
window.mountWorkflowEditor = () => {
  if (root) return;
  const container = document.getElementById('workflow-editor-root');
  if (!container) return;
  root = createRoot(container);
  root.render(<ReactFlowProvider><Editor/></ReactFlowProvider>);
};
