(() => {
  const element = name => document.getElementById(`commit-signing-${name}`);
  if (!element('section')) return;
  const enabled = element('enabled');
  const privateKey = element('private-key');
  const passphrase = element('passphrase');
  const publicKey = element('public-key');
  const status = element('status');
  let configured = false;
  let busy = false;
  function clearSecrets() { privateKey.value = ''; passphrase.value = ''; }
  function setBusy(value) {
    busy = value;
    for (const name of ['save', 'private-key', 'passphrase']) element(name).disabled = value;
    enabled.disabled = value || !configured;
    element('verify').disabled = value || !configured;
    element('copy').disabled = value || !publicKey.value;
  }
  function render(settings) {
    if (!settings) return;
    configured = Boolean(settings.configured);
    enabled.checked = Boolean(settings.enabled);
    publicKey.value = settings.public_key || '';
    element('account').textContent = settings.account ? `GitHub account: ${settings.account}` : '';
    element('fingerprint').textContent = settings.fingerprint ? `Fingerprint: ${settings.fingerprint}` : '';
    const labels = {
      not_configured: 'No signing key configured.',
      unchecked: 'Registration has not been checked in this session.',
      not_registered: 'Add this public key to GitHub as a Signing Key before enabling.',
      unavailable: 'GitHub registration could not be checked. Try Check registration again.',
      not_ready: 'Signing needs attention. Check the message below.',
      registered: 'Registered with GitHub. Local signing works.',
    };
    element('registration').textContent = labels[settings.registration] || labels.unchecked;
    setBusy(busy);
  }
  async function request(path, payload) {
    const response = await fetch(`/api/commit-signing${path}`, {
      method: payload === undefined ? 'GET' : 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      cache: 'no-store',
      ...(payload === undefined ? {} : { body: JSON.stringify(payload) }),
    });
    const body = await response.json();
    render(body.settings);
    if (!response.ok) throw new Error(body.error || 'Unable to update commit signing.');
    return body.settings;
  }
  async function load() {
    if (busy) return;
    clearSecrets();
    setBusy(true);
    status.textContent = '';
    try { await request(''); }
    catch (error) { status.textContent = error.message || 'Unable to load commit signing.'; }
    finally { setBusy(false); }
  }
  element('save').addEventListener('click', async () => {
    if (busy) return;
    setBusy(true);
    status.textContent = 'Saving signing settings…';
    try {
      const pending = request('', { enabled: enabled.checked, private_key: privateKey.value, passphrase: passphrase.value });
      clearSecrets();
      const settings = await pending;
      status.textContent = settings.message || 'Signing settings saved.';
    } catch (error) { status.textContent = error.message || 'Unable to save signing settings.'; }
    finally { clearSecrets(); setBusy(false); }
  });
  passphrase.addEventListener('keydown', event => {
    if (event.key === 'Enter') { event.preventDefault(); element('save').click(); }
  });
  element('verify').addEventListener('click', async () => {
    if (busy) return;
    setBusy(true);
    status.textContent = 'Checking registration and local signing…';
    try { const settings = await request('/verify', {}); status.textContent = settings.message; }
    catch (error) { status.textContent = error.message || 'Unable to verify signing.'; }
    finally { setBusy(false); }
  });
  element('copy').addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(publicKey.value); status.textContent = 'Public key copied.'; }
    catch (_) { publicKey.focus(); publicKey.select(); status.textContent = 'Select and copy the public key above.'; }
  });
  window.addEventListener('commit-signing-open', load);
  window.addEventListener('commit-signing-close', clearSecrets);
  if (!document.getElementById('settings-gate').classList.contains('hidden')) void load();
})();
