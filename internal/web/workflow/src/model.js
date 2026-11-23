export const STORAGE_KEY = 'agent00.workflow.draft.v1';
export const stepTypes = {
  trigger: { label: 'Trigger', description: 'Choose what starts the workflow', color: '#50bfa5', defaultName: 'Issue labeled', detail: 'ready-for-agent' },
  agent: { label: 'Agent', description: 'Give an agent a task', color: '#a18aff', defaultName: 'Implement the issue', detail: 'Read the issue, implement the change, and run the relevant tests.' },
  condition: { label: 'Condition', description: 'Split the path with a rule', color: '#e5ae58', defaultName: 'Checks passed?', detail: 'All required checks pass' },
  approval: { label: 'Human approval', description: 'Pause for a person to review', color: '#66b5eb', defaultName: 'Review the changes', detail: 'Review the implementation before continuing.' },
  action: { label: 'Action', description: 'Finish with an outcome', color: '#de8fbd', defaultName: 'Open a pull request', detail: 'Create a draft pull request' },
};
export function makeNode(kind, position, id = crypto.randomUUID()) {
  const item = stepTypes[kind];
  return { id, type: 'workflow', position, data: { kind, label: item.defaultName, detail: item.detail } };
}
export function exampleWorkflow() {
  return {
    version: 1, name: 'Issue to pull request',
    nodes: [
      makeNode('trigger', { x: 165, y: 0 }, 'trigger'),
      makeNode('agent', { x: 165, y: 160 }, 'agent'),
      makeNode('condition', { x: 165, y: 320 }, 'condition'),
      makeNode('approval', { x: 0, y: 500 }, 'approval'),
      makeNode('action', { x: 330, y: 680 }, 'action'),
    ],
    edges: [
      { id: 'trigger-agent', source: 'trigger', target: 'agent', sourceHandle: 'out' },
      { id: 'agent-condition', source: 'agent', target: 'condition', sourceHandle: 'out' },
      { id: 'condition-approval', source: 'condition', target: 'approval', sourceHandle: 'no', label: 'Needs review' },
      { id: 'approval-action', source: 'approval', target: 'action', sourceHandle: 'out', label: 'Approved' },
      { id: 'condition-action', source: 'condition', target: 'action', sourceHandle: 'yes', label: 'Passed' },
    ],
  };
}
export function validConnection(connection, nodes, edges) {
  const source = nodes.find(n => n.id === connection.source);
  const target = nodes.find(n => n.id === connection.target);
  if (!source || !target || source.id === target.id || source.data.kind === 'action' || target.data.kind === 'trigger') return false;
  if (edges.some(e => e.source === source.id && e.sourceHandle === connection.sourceHandle)) return false;
  const pending = [target.id]; const seen = new Set();
  while (pending.length) {
    const id = pending.pop();
    if (id === source.id) return false;
    if (seen.has(id)) continue;
    seen.add(id);
    edges.filter(e => e.source === id).forEach(e => pending.push(e.target));
  }
  return true;
}
export function validateWorkflow(nodes, edges) {
  const errors = [];
  const triggers = nodes.filter(n => n.data.kind === 'trigger');
  if (triggers.length !== 1) errors.push('Add exactly one trigger.');
  if (!nodes.some(n => n.data.kind === 'action')) errors.push('Add an action to finish the workflow.');
  for (const node of nodes) {
    if (!node.data.label.trim()) errors.push('Every step needs a name.');
    if (!node.data.detail.trim()) errors.push(`${node.data.label || 'Unnamed step'} needs configuration.`);
    const outgoing = edges.filter(e => e.source === node.id);
    if (node.data.kind !== 'action' && !outgoing.length) errors.push(`Connect an output from “${node.data.label}”.`);
    if (node.data.kind === 'condition' && (!outgoing.some(e => e.sourceHandle === 'yes') || !outgoing.some(e => e.sourceHandle === 'no'))) errors.push(`Connect both branches of “${node.data.label}”.`);
  }
  if (triggers.length === 1) {
    const seen = new Set(); const pending = [triggers[0].id];
    while (pending.length) {
      const id = pending.pop(); if (seen.has(id)) continue; seen.add(id);
      edges.filter(e => e.source === id).forEach(e => pending.push(e.target));
    }
    for (const node of nodes) if (!seen.has(node.id)) errors.push(`“${node.data.label}” is disconnected from the trigger.`);
  }
  return [...new Set(errors)];
}
export function serializeDraft(name, nodes, edges) {
  return { version: 1, name, nodes: nodes.map(({ id, type, position, data }) => ({ id, type, position, data })), edges: edges.map(({ id, source, target, sourceHandle, targetHandle, label }) => ({ id, source, target, sourceHandle, targetHandle, label })) };
}
export function readDraft(raw) {
  const draft = JSON.parse(raw);
  if (draft?.version !== 1 || typeof draft.name !== 'string' || !Array.isArray(draft.nodes) || !Array.isArray(draft.edges)) throw new Error('Invalid draft');
  const ids = new Set();
  for (const n of draft.nodes) {
    if (typeof n.id !== 'string' || ids.has(n.id) || n.type !== 'workflow' || !Object.hasOwn(stepTypes, n.data?.kind) || typeof n.data.label !== 'string' || typeof n.data.detail !== 'string' || !Number.isFinite(n.position?.x) || !Number.isFinite(n.position?.y)) throw new Error('Invalid step');
    ids.add(n.id);
  }
  const edgeIds = new Set();
  for (const e of draft.edges) {
    if (typeof e.id !== 'string' || edgeIds.has(e.id) || !ids.has(e.source) || !ids.has(e.target) || (e.label !== undefined && typeof e.label !== 'string')) throw new Error('Invalid connection');
    edgeIds.add(e.id);
  }
  return serializeDraft(draft.name, draft.nodes, draft.edges);
}
