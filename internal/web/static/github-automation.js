(() => {
  const panel = document.getElementById('settings-panel-github-issues');
  const status = document.getElementById('github-automation-status');
  const list = document.getElementById('github-automation-decisions');
  if (!panel || !status || !list) return;
  let loading = false;
  // Discovery scans one page at a time; retain observed filter failures until
  // a later scan reports that the issue matches again.
  const filtered = new Set();
  async function request(url, options) {
    const response = await fetch(url, { cache: 'no-store', ...options });
    const body = await response.json();
    if (!response.ok) throw new Error(body.error || 'Unable to load automation status.');
    return body;
  }
  function addError(parent, message) {
    if (!message) return;
    const details = document.createElement('details');
    details.className = 'github-automation-error';
    const summary = document.createElement('summary');
    summary.textContent = 'Error details';
    const text = document.createElement('p');
    text.textContent = message;
    details.append(summary, text);
    parent.append(details);
  }
  function addRecoveryDetails(parent, entry, repairs) {
    const lines = [];
    if (entry.workerAlive !== undefined) lines.push(`Worker: ${entry.workerAlive === true ? 'running' : entry.workerAlive === false ? 'stopped' : 'unknown'}`);
    if (entry.unlockPending) lines.push('Unlock pending');
    if (entry.step) lines.push(`Step: ${entry.step}`);
    if (entry.agentAttempts !== undefined) lines.push(`Agent attempts: ${entry.agentAttempts}`);
    for (const [label, value] of [['Step started', entry.stepStartedAt], ['Next retry', entry.nextAttemptAt]]) {
      if (value && !value.startsWith('0001-')) lines.push(`${label}: ${new Date(value).toLocaleString()}`);
    }
    if (entry.prUrl) lines.push(`Verified PR: ${entry.prUrl}`);
    if (entry.recoveryNote) lines.push(entry.recoveryNote);
    if (entry.repairRequestId) {
      const repair = repairs.get(entry.repairRequestId);
      lines.push(`Repair: ${entry.repairRequestId} (${repair?.state || 'pending'})`);
      if (repair?.error) lines.push(repair.error);
      if (repair?.prUrl) lines.push(repair.prUrl);
    }
    if (entry.repairError) lines.push(`Repair queue: ${entry.repairError}`);
    if (!lines.length) return;
    const details = document.createElement('details');
    details.className = 'github-automation-details';
    const summary = document.createElement('summary');
    summary.textContent = 'Recovery details';
    const text = document.createElement('p');
    text.textContent = lines.join('\n');
    details.append(summary, text);
    parent.append(details);
  }
  async function recover(entry, button) {
    button.disabled = true;
    try {
      const query = new URLSearchParams({ repo: entry.repository, number: entry.number });
      const claim = await request(`/api/github/automation/retry?${query}`);
      if (!window.confirm(`Retry ${entry.repository}#${entry.number}? Confirm that its previous worker has stopped on every Agent Hub instance. The existing branch and commits will be preserved.`)) return;
      await request('/api/github/automation/retry', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ repository: entry.repository, number: entry.number, expectedClaimId: claim.expectedClaimId, confirmStopped: true }),
      });
      await refresh();
    } catch (error) { status.textContent = error.message; }
    finally { button.disabled = false; }
  }
  async function refresh() {
    if (loading) return;
    loading = true;
    try {
      const body = await request('/api/github/automation');
      const current = body.status || {};
      const entries = new Map();
      const repairs = new Map((body.repairs || []).map(repair => [repair.requestId, repair]));
      for (const entry of current.issues || []) {
        const key = `${entry.repository}#${entry.number}`;
        if (['wrong assignee', 'missing trigger label', 'closed issue'].includes(entry.reason)) {
          filtered.add(key);
          continue;
        }
        filtered.delete(key);
        entries.set(key, entry);
      }
      for (const claim of body.claims || []) {
        const key = `${claim.repository}#${claim.number}`;
        // A saved claim must not bring a filtered-out issue back into the list.
        if (filtered.has(key)) continue;
        const previous = entries.get(key);
        if (previous?.attempt > claim.attempt) continue;
        entries.set(key, { ...claim, reason: claim.phase, recoverable: claim.workerAlive !== true && claim.phase !== 'fulfilled' });
      }
      status.textContent = current.error ? 'Scan needs attention.' : (current.enabled ? 'Auto-start on' : 'Auto-start off');
      const scanError = document.getElementById('github-automation-error');
      scanError.replaceChildren();
      addError(scanError, current.error);
      list.replaceChildren();
      if (!entries.size) {
        const empty = document.createElement('li');
        empty.className = 'github-automation-empty';
        empty.textContent = 'No matching issues.';
        list.append(empty);
      }
      for (const [key, entry] of entries) {
        const item = document.createElement('li');
        item.className = 'github-automation-issue';
        const info = document.createElement('div');
        info.className = 'github-automation-issue-info';
        const name = document.createElement('strong');
        name.textContent = key;
        const state = document.createElement('span');
        state.className = 'github-automation-issue-state';
        const labels = {
          'associated branch or closing PR': 'Linked work',
          'claimed elsewhere or excluded': 'Reserved',
          'submission blocked': 'Needs attention',
          'blocked by open issue': 'Blocked by another issue',
          blocked: 'Needs attention', submitted: 'Submitted', queued: 'Queued',
          running: 'Running', completed: 'Completed', error: 'Failed', stopped: 'Stopped',
          no_changes: 'No changes', merged: 'Merged', invalid: 'Invalid task', duplicate: 'Already queued',
          reserving: 'Reserved', submitting: 'Submitting',
          fulfilled: 'PR delivered', verifying: 'Checking delivery', backoff: 'Waiting to retry',
          'tracked by recovery': 'Recovery in progress',
        };
        state.textContent = labels[entry.reason] || entry.reason || 'Needs attention';
        info.append(name, state);
        addError(info, entry.error);
        addRecoveryDetails(info, entry, repairs);
        item.append(info);
        const active = ['queued', 'running', 'submitting'].includes(entry.reason);
        if (!active && (entry.recoverable || ['associated branch or closing PR', 'claimed elsewhere or excluded'].includes(entry.reason))) {
          const button = document.createElement('button');
          button.type = 'button';
          button.className = 'prompt-action-button';
          button.textContent = 'Retry';
          button.addEventListener('click', () => recover(entry, button));
          item.append(button);
        }
        list.append(item);
      }
    } catch (error) { status.textContent = error.message; }
    finally { loading = false; }
  }
  document.getElementById('github-automation-refresh')?.addEventListener('click', refresh);
  document.getElementById('github-automation-scan')?.addEventListener('click', async () => {
    try { await request('/api/github/automation/scan', { method: 'POST' }); status.textContent = 'Scanning…'; }
    catch (error) { status.textContent = error.message; }
  });
  new MutationObserver(() => { if (!panel.hidden) refresh(); }).observe(panel, { attributes: true, attributeFilter: ['hidden'] });
  setInterval(() => { if (!panel.hidden && document.visibilityState !== 'hidden') refresh(); }, 15000);
  if (!panel.hidden) refresh();
})();
