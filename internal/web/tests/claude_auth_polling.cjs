// Run with: node --test internal/web/tests/claude_auth_polling.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '../static/index.html'), 'utf8');
const start = html.indexOf('    function clearClaudePostSubmitVerifyTimer()');
const end = html.indexOf('    function shouldAutoSubmitClaudeBrowserCode(', start);
assert.ok(start >= 0 && end > start, 'Claude verification functions exist');

test('Claude verification keeps polling until delayed login completes', async () => {
  let nextTimer;
  let requests = 0;
  let ready = false;
  const statuses = [];
  const context = vm.createContext({
    window: { setTimeout(callback) { nextTimer = callback; return 1; } },
    clearTimeout() { nextTimer = undefined; },
    state: { agentAuth: { ready: false } },
    hasClaudeBrowserCodeBeenAccepted: () => !ready,
    requestAgentAuth: async () => ({ ready: ++requests === 3 }),
    applyAgentAuthGate(auth) { ready = auth.ready; context.state.agentAuth = auth; },
    setAgentAuthVerifyPending() {},
    setLocalPromptStatus: (...args) => statuses.push(args),
    currentAgentLabel: () => 'Claude Code',
  });
  vm.runInContext(`
    let claudePostSubmitVerifyTimer = null;
    let claudePostSubmitVerifyInFlight = false;
    let claudePostSubmitVerifyAttempts = 0;
    const CLAUDE_POST_SUBMIT_VERIFY_MAX_ATTEMPTS = 5;
    const CLAUDE_POST_SUBMIT_VERIFY_INTERVAL_MS = 100;
    ${html.slice(start, end)}
    scheduleClaudePostSubmitVerifyPolling();
  `, context);
  for (let attempt = 0; attempt < 3; attempt++) {
    assert.equal(typeof nextTimer, 'function', `verification attempt ${attempt + 1} scheduled`);
    const callback = nextTimer;
    nextTimer = undefined;
    await callback();
  }
  assert.equal(requests, 3);
  assert.equal(ready, true);
  assert.equal(nextTimer, undefined);
  assert.ok(statuses.some(([level]) => level === 'ok'));
});
