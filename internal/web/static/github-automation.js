(() => {
  const panel = document.getElementById('settings-panel-github-issues');
  const status = document.getElementById('github-automation-status');
  if (!panel || !status) return;
  let loading = false;
  async function request(url, options) {
    const response = await fetch(url, { cache: 'no-store', ...options });
    const body = await response.json();
    if (!response.ok) throw new Error(body.error || 'Unable to load automation status.');
    return body;
  }
  function showError(message) {
    const container = document.getElementById('github-automation-error');
    container.replaceChildren();
    if (!message) return;
    const details = document.createElement('details');
    details.className = 'github-automation-error';
    const summary = document.createElement('summary');
    summary.textContent = 'Error details';
    const text = document.createElement('p');
    text.textContent = message;
    details.append(summary, text);
    container.append(details);
  }
  async function refresh() {
    if (loading) return;
    loading = true;
    try {
      const body = await request('/api/github/automation');
      const current = body.status || {};
      status.textContent = current.error ? 'Scan needs attention.' : (current.enabled ? 'Auto-start on' : 'Auto-start off');
      showError(current.error);
    } catch (error) { status.textContent = error.message; }
    finally { loading = false; }
  }
  document.getElementById('github-automation-refresh')?.addEventListener('click', () => {
    window.trackAnalyticsEvent?.('github_automation_refresh_clicked', { source: 'settings' });
    void refresh();
  });
  document.getElementById('github-automation-scan')?.addEventListener('click', async () => {
    window.trackAnalyticsEvent?.('github_automation_scan_started', { source: 'settings' });
    try {
      await request('/api/github/automation/scan', { method: 'POST' });
      status.textContent = 'Scanning…';
      window.trackAnalyticsEvent?.('github_automation_scan_accepted', { source: 'settings' });
    } catch (error) {
      status.textContent = error.message;
      window.trackAnalyticsEvent?.('github_automation_scan_failed', { source: 'settings' });
    }
  });
  new MutationObserver(() => { if (!panel.hidden) refresh(); }).observe(panel, { attributes: true, attributeFilter: ['hidden'] });
  setInterval(() => { if (!panel.hidden && document.visibilityState !== 'hidden') refresh(); }, 15000);
  if (!panel.hidden) refresh();
})();
