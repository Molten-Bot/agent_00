(() => {
  const panel = document.getElementById('settings-panel-github-issues');
  const status = document.getElementById('github-automation-status');
  const list = document.getElementById('github-automation-decisions');
  if (!panel || !status || !list) return;
  let loading = false;
  async function request(url, options) {
    const response = await fetch(url, { cache: 'no-store', ...options });
    const body = await response.json();
    if (!response.ok) throw new Error(body.error || 'Unable to load automation status.');
    return body;
  }
  function timestamp(value) {
    return value && !value.startsWith('0001-') ? new Date(value).toLocaleString() : 'never';
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
      status.textContent = `${current.enabled ? 'Enabled' : 'Disabled'} · Account: ${current.account || 'unknown'} · Repositories: ${(current.repositories || []).join(', ') || 'none'} · Last scan: ${timestamp(current.lastAttempt)} · Last successful scan: ${timestamp(current.lastSuccess)} · Checked: ${current.scanned || 0} · Skipped: ${current.skipped || 0} · Submitted: ${current.submitted || 0}${current.error ? ` · ${current.error}` : ''}`;
      const entries = new Map();
      const repairs = new Map((body.repairs || []).map(repair => [repair.requestId, repair]));
      for (const entry of current.issues || []) entries.set(`${entry.repository}#${entry.number}`, entry);
      for (const claim of body.claims || []) {
        const key = `${claim.repository}#${claim.number}`;
        const previous = entries.get(key);
        if (previous?.attempt > claim.attempt) continue;
        entries.set(key, { ...claim, reason: claim.phase, recoverable: claim.workerAlive !== true && claim.phase !== 'fulfilled' });
      }
      list.replaceChildren();
      for (const [key, entry] of entries) {
        const item = document.createElement('li');
        item.textContent = `${key}: ${entry.reason || 'unknown'}${entry.error ? ` — ${entry.error}` : ''}`;
        if (entry.workerAlive !== undefined) item.append(` · Worker: ${entry.workerAlive === true ? 'running' : entry.workerAlive === false ? 'stopped' : 'unknown'}`);
        if (entry.unlockPending) item.append(' · Unlock pending');
        if (entry.step) item.append(` · Step: ${entry.step} since ${timestamp(entry.stepStartedAt)}`);
        if (entry.agentAttempts !== undefined) item.append(` · Agent attempts: ${entry.agentAttempts}`);
        if (entry.nextAttemptAt && !entry.nextAttemptAt.startsWith('0001-')) item.append(` · Retry: ${timestamp(entry.nextAttemptAt)}`);
        if (entry.prUrl) item.append(` · Verified PR: ${entry.prUrl}`);
        if (entry.recoveryNote) item.append(` · ${entry.recoveryNote}`);
        if (entry.repairRequestId) {
          const repair = repairs.get(entry.repairRequestId);
          item.append(` · Repair: ${entry.repairRequestId} (${repair?.state || 'pending'})${repair?.error ? ` — ${repair.error}` : ''}${repair?.prUrl ? ` · ${repair.prUrl}` : ''}`);
        }
        if (entry.repairError) item.append(` · Repair queue: ${entry.repairError}`);
        if (entry.recoverable || ['associated branch or closing PR', 'claimed elsewhere or excluded'].includes(entry.reason)) {
          const button = document.createElement('button');
          button.type = 'button'; button.textContent = 'Recover / retry';
          button.addEventListener('click', () => recover(entry, button));
          item.append(' ', button);
        }
        list.append(item);
      }
    } catch (error) { status.textContent = error.message; }
    finally { loading = false; }
  }
  document.getElementById('github-automation-refresh')?.addEventListener('click', refresh);
  document.getElementById('github-automation-scan')?.addEventListener('click', async () => {
    try { await request('/api/github/automation/scan', { method: 'POST' }); status.textContent = 'Scan requested. Results will appear when it finishes.'; }
    catch (error) { status.textContent = error.message; }
  });
  new MutationObserver(() => { if (!panel.hidden) refresh(); }).observe(panel, { attributes: true, attributeFilter: ['hidden'] });
  setInterval(() => { if (!panel.hidden && document.visibilityState !== 'hidden') refresh(); }, 15000);
  if (!panel.hidden) refresh();
})();
