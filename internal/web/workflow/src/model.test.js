import test from 'node:test';
import assert from 'node:assert/strict';
import { exampleWorkflow, validConnection, validateWorkflow, serializeDraft, readDraft } from './model.js';
test('example is valid and survives saving without transient selection state', () => {
  const draft = exampleWorkflow();
  assert.deepEqual(validateWorkflow(draft.nodes,draft.edges), []);
  draft.nodes[0].selected = true;
  const restored = readDraft(JSON.stringify(serializeDraft(draft.name,draft.nodes,draft.edges)));
  assert.equal(restored.nodes[0].selected, undefined);
  assert.deepEqual(validateWorkflow(restored.nodes,restored.edges), []);
});
test('connections reject self links, occupied ports, terminal outputs, and cycles', () => {
  const { nodes,edges } = exampleWorkflow();
  for (const connection of [
    {source:'agent',target:'agent',sourceHandle:'out'},
    {source:'trigger',target:'approval',sourceHandle:'out'},
    {source:'action',target:'approval',sourceHandle:'out'},
    {source:'approval',target:'trigger',sourceHandle:'out'},
    {source:'approval',target:'agent',sourceHandle:'out'},
  ]) assert.equal(validConnection(connection,nodes,edges),false);
  assert.equal(validConnection({source:'approval',target:'action',sourceHandle:'out'},nodes,edges.filter(e => e.source !== 'approval')),true);
  assert.equal(validConnection({source:'approval',target:'agent',sourceHandle:'out'},nodes,edges.filter(e => e.source !== 'approval')),false);
});
test('validation identifies missing branches and disconnected steps', () => {
  const {nodes,edges} = exampleWorkflow();
  const errors = validateWorkflow(nodes,edges.filter(e => e.sourceHandle !== 'no'));
  assert.ok(errors.some(e => e.includes('both branches')));
  assert.ok(errors.some(e => e.includes('disconnected')));
  assert.ok(validateWorkflow([],[]).length >= 2);
});
test('invalid saved drafts fail safely', () => {
  assert.throws(() => readDraft('{'));
  assert.throws(() => readDraft(JSON.stringify({version:2,nodes:[],edges:[]})));
  const draft = exampleWorkflow(); draft.nodes[0].data.kind = '__proto__';
  assert.throws(() => readDraft(JSON.stringify(draft)));
});
