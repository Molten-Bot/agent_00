(() => {
  const element = name => document.getElementById(`commit-signing-${name}`);
  if (!element('section')) return;
  const enabled = element('enabled');
  const privateKey = element('private-key');
  const passphrase = element('passphrase');
  const publicKey = element('public-key');
  const status = element('status');
  const setupRequired = element('setup-required');
  let configured = false;
  let busy = false;
  let modalBusy = false;
  let savedEnabled = false;
  let loadPromise = null;
  function track(name, params = {}) {
    if (typeof window.trackAnalyticsEvent === 'function') window.trackAnalyticsEvent(name, params);
  }
  function clearSecrets() { privateKey.value = ''; passphrase.value = ''; }
  function setBusy(value) {
    busy = value;
    const blocked = busy || modalBusy;
    for (const name of ['save', 'private-key', 'passphrase']) element(name).disabled = blocked;
    // A missing key must not prevent turning off a saved signing requirement.
    enabled.disabled = blocked || (!configured && !savedEnabled);
    element('enabled-help').textContent = value
      ? 'Please wait while signing settings are being processed.'
      : configured
        ? 'After registering the public key on GitHub, select this option and close settings to save.'
        : savedEnabled
          ? 'The saved signing key is missing. Save a replacement or turn signing off.'
          : 'Save an SSH private key above to enable commit signing.';
    element('verify').disabled = blocked || !configured;
    element('copy').disabled = blocked || !publicKey.value;
  }
  function render(settings) {
    if (!settings) return;
    configured = Boolean(settings.configured);
    savedEnabled = Boolean(settings.enabled);
    enabled.checked = savedEnabled;
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
    applySetupRequirement(settings);
    setBusy(busy);
  }
  // Signing can be required before a key exists, which blocks every task. Say so
  // here, next to the key field, and tell the app so it can hold submissions.
  function applySetupRequirement(settings) {
    const required = Boolean(settings.setup_required);
    const message = required
      ? settings.message || 'Commit signing is required, but no signing key is saved yet.'
      : '';
    // role="alert" announces text that changes while the element is rendered,
    // so reveal the banner first, let the app open the settings dialog it sits
    // in, and only then fill it in. Setting the text while either is hidden
    // leaves the hold silent for assistive technology.
    setupRequired.hidden = !required;
    window.dispatchEvent(new CustomEvent('commit-signing-required', {
      detail: { required, message },
    }));
    setupRequired.textContent = message;
  }
  async function request(path, payload, preserveDraftOnError = false) {
    const response = await fetch(`/api/commit-signing${path}`, {
      method: payload === undefined ? 'GET' : 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      cache: 'no-store',
      ...(payload === undefined ? {} : { body: JSON.stringify(payload) }),
    });
    const body = await response.json();
    if (response.ok || !preserveDraftOnError) render(body.settings);
    if (!response.ok) throw new Error(body.error || 'Unable to update commit signing.');
    return body.settings;
  }
  function load() {
    if (loadPromise) return loadPromise;
    if (busy) return Promise.resolve();
    clearSecrets();
    setBusy(true);
    status.textContent = '';
    loadPromise = (async () => {
      try { await request(''); }
      catch (error) {
        status.textContent = error.message || 'Unable to load commit signing.';
        track('commit_signing_load_failed');
      }
      finally { setBusy(false); loadPromise = null; }
    })();
    return loadPromise;
  }
  // The modal saves the enable/disable choice on close. Importing a key remains
  // an explicit setup action; private material is never submitted by closing.
  window.CommitSigningSettings = {
    setModalBusy(value) { modalBusy = value; setBusy(busy); },
    async saveOnClose() {
      if (loadPromise) await loadPromise;
      if (busy) throw new Error('Wait for commit signing setup to finish before closing.');
      if (enabled.checked === savedEnabled) return;
      const nextEnabled = enabled.checked;
      setBusy(true);
      track('commit_signing_toggle_started', { enabled: nextEnabled });
      try {
        await request('', { enabled: nextEnabled }, true);
        status.textContent = 'Commit signing settings saved.';
        track('commit_signing_toggle_succeeded', { enabled: nextEnabled });
      } catch (error) {
        status.textContent = error.message || 'Unable to save commit signing.';
        track('commit_signing_toggle_failed', { enabled: nextEnabled });
        throw error;
      } finally { setBusy(false); }
    },
  };
  element('save').addEventListener('click', async () => {
    if (busy || modalBusy) return;
    const nextEnabled = enabled.checked;
    setBusy(true);
    status.textContent = 'Saving signing settings…';
    track('commit_signing_save_started', { enabled: nextEnabled, has_passphrase: Boolean(passphrase.value) });
    try {
      const pending = request('', { enabled: nextEnabled, private_key: privateKey.value, passphrase: passphrase.value });
      clearSecrets();
      const settings = await pending;
      status.textContent = settings.message || 'Signing settings saved.';
      track('commit_signing_save_succeeded', { enabled: Boolean(settings.enabled) });
    } catch (error) {
      status.textContent = error.message || 'Unable to save signing settings.';
      track('commit_signing_save_failed', { enabled: nextEnabled });
    }
    finally { clearSecrets(); setBusy(false); }
  });
  passphrase.addEventListener('keydown', event => {
    if (event.key === 'Enter') { event.preventDefault(); element('save').click(); }
  });
  element('verify').addEventListener('click', async () => {
    if (busy || modalBusy) return;
    setBusy(true);
    status.textContent = 'Checking registration and local signing…';
    track('commit_signing_verify_started');
    try {
      const settings = await request('/verify', {});
      status.textContent = settings.message;
      track('commit_signing_verify_succeeded', { registration: settings.registration || 'unknown' });
    }
    catch (error) {
      status.textContent = error.message || 'Unable to verify signing.';
      track('commit_signing_verify_failed');
    }
    finally { setBusy(false); }
  });
  element('copy').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(publicKey.value);
      status.textContent = 'Public key copied.';
      track('commit_signing_public_key_copied', { result: 'copied' });
    }
    catch (_) {
      publicKey.focus(); publicKey.select(); status.textContent = 'Select and copy the public key above.';
      track('commit_signing_public_key_copy_failed', { fallback: 'manual_selection' });
    }
  });
  window.addEventListener('commit-signing-open', load);
  window.addEventListener('commit-signing-close', clearSecrets);
  // Load on boot even when settings are closed: a required key that is missing
  // has to surface before the operator queues work that cannot run.
  void load();
})();
