// Run against a local harness after building it:
// HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/commit_signing.cjs
// Requires Playwright and Chromium. API mutations are intercepted; no keys are saved.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

// newStubbedPage blocks external requests and stubs the settings APIs, so a
// suite drives the real UI without touching saved settings or keys.
async function newStubbedPage(t, handler, viewport = { width: 1280, height: 900 }) {
  const page = await browser.newPage({ viewport });
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.route('**/api/agent-auth**', route => route.fulfill({ json: { ok: true, auth: { required: true, ready: true, harness: 'claude', state: 'ready' } } }));
  await page.route('**/api/review-settings', route => route.fulfill({ json: { ok: true, settings: { merge_method: 'squash', review_level: 'off' } } }));
  await page.route('**/api/commit-signing**', handler);
  return page;
}

async function openSettings(t, handler, viewport = { width: 1280, height: 900 }) {
  const page = await newStubbedPage(t, handler, viewport);
  await page.goto(`${baseURL}/#settings`);
  await page.locator('#settings-tab-signing').click();
  await page.waitForFunction(() => !document.getElementById('commit-signing-save').disabled);
  return page;
}

const empty = { enabled: false, configured: false, registration: 'not_configured' };
const saved = { ...empty, configured: true, public_key: 'ssh-ed25519 TEST_PUBLIC_KEY', fingerprint: 'SHA256:test', account: 'test-bot', registration: 'not_registered' };
// What the server sends when signing is required but cannot sign yet.
const held = {
  ...empty,
  enabled: true,
  setup_required: true,
  message: 'Commit signing is enabled. Save a signing key in Settings → Commit signing before running tasks.',
};

test('import, register, enable, reload and disable without returning private material', async t => {
  let settings = { ...empty };
  const requests = [];
  const page = await openSettings(t, route => {
    const req = route.request();
    if (req.method() === 'POST') {
      const payload = req.postDataJSON();
      requests.push(payload);
      if (req.url().endsWith('/verify')) settings = { ...settings, registered: true, registration: 'registered', message: 'Key is registered and local signing works.' };
      else if (payload.private_key) {
        assert.equal(payload.private_key, 'test-private-key');
        assert.equal(payload.passphrase, 'test-passphrase');
        settings = { ...saved, message: 'Key saved. Add the public key to GitHub.' };
      } else settings = { ...settings, enabled: payload.enabled, message: 'Commit signing settings saved.' };
    }
    return route.fulfill({ json: { ok: true, settings } });
  });
  assert.equal(await page.locator('#commit-signing-enabled').isDisabled(), true);
  assert.match(await page.locator('#commit-signing-enabled-help').textContent(), /Save an SSH private key above to enable/);
  assert.equal(await page.locator('#commit-signing-public-key').isEditable(), false);
  assert.match(await page.locator('#commit-signing-public-key-help').textContent(), /Generated from your saved private key/);
  await page.locator('#commit-signing-private-key').fill('test-private-key');
  await page.locator('#commit-signing-passphrase').fill('test-passphrase');
  await page.locator('#commit-signing-save').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-status').textContent.startsWith('Key saved.'));
  assert.equal(await page.locator('#commit-signing-private-key').inputValue(), '');
  assert.equal(await page.locator('#commit-signing-passphrase').inputValue(), '');
  assert.equal(await page.locator('#commit-signing-public-key').inputValue(), saved.public_key);
  assert.equal(await page.locator('#commit-signing-enabled').isEnabled(), true);
  assert.match(await page.locator('#commit-signing-enabled-help').textContent(), /After registering the public key on GitHub/);
  assert.match(await page.locator('#commit-signing-account').textContent(), /test-bot/);
  await page.locator('#commit-signing-verify').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-registration').textContent.startsWith('Registered'));
  await page.locator('label:has(#commit-signing-enabled)').click();
  await page.locator('#commit-signing-save').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-status').textContent === 'Commit signing settings saved.');
  assert.equal(settings.enabled, true);
  await page.reload();
  await page.locator('#settings-tab-signing').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-enabled').checked);
  assert.equal(await page.locator('#commit-signing-private-key').inputValue(), '');
  await page.locator('label:has(#commit-signing-enabled)').click();
  await page.locator('#commit-signing-save').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-status').textContent === 'Commit signing settings saved.');
  assert.equal(settings.enabled, false);
  assert.equal(settings.configured, true);
  assert.ok(requests.slice(1).every(req => !req.private_key && !req.passphrase));
});

test('failed imports clear secrets, preserve saved state, and fit a narrow viewport', async t => {
  const page = await openSettings(t, route => route.request().method() === 'POST'
    ? route.fulfill({ status: 400, json: { ok: false, settings: saved, error: 'Invalid private key or incorrect passphrase.' } })
    : route.fulfill({ json: { ok: true, settings: saved } }), { width: 390, height: 844 });
  await page.locator('#commit-signing-private-key').fill('invalid-private-key');
  await page.locator('#commit-signing-passphrase').fill('wrong-passphrase');
  await page.locator('#commit-signing-save').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-status').textContent.includes('incorrect passphrase'));
  assert.equal(await page.locator('#commit-signing-private-key').inputValue(), '');
  assert.equal(await page.locator('#commit-signing-passphrase').inputValue(), '');
  assert.equal(await page.locator('#commit-signing-public-key').inputValue(), saved.public_key);
  assert.equal(await page.locator('#commit-signing-enabled').isChecked(), false);
  const overflow = await page.locator('.settings-shell').evaluate(el => el.scrollWidth > el.clientWidth + 1);
  assert.equal(overflow, false, 'settings do not overflow horizontally');
  await page.locator('#commit-signing-private-key').fill('unsaved-secret');
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => document.getElementById('settings-gate').classList.contains('hidden'));
  assert.equal(await page.locator('#commit-signing-private-key').inputValue(), '');
});

test('closing saves the signing toggle without submitting a draft key', async t => {
  let settings = { ...saved };
  const writes = [];
  const page = await openSettings(t, route => {
    if (route.request().method() === 'POST') {
      const payload = route.request().postDataJSON(); writes.push(payload);
      settings = { ...settings, enabled: payload.enabled };
    }
    return route.fulfill({ json: { ok: true, settings } });
  });
  await page.locator('label:has(#commit-signing-enabled)').click();
  await page.locator('#commit-signing-private-key').fill('unsaved-test-key');
  await page.locator('#review-settings-close').click();
  await page.waitForFunction(() => document.getElementById('settings-gate').classList.contains('hidden'));
  assert.deepEqual(writes, [{ enabled: true }]);
  assert.equal(await page.locator('#commit-signing-private-key').inputValue(), '');
  await page.reload(); await page.locator('#settings-tab-signing').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-enabled').checked);
});

test('failed close-time signing save keeps the selected option and allows retry', async t => {
  let fail = true;
  const page = await openSettings(t, route => route.request().method() === 'POST'
    ? route.fulfill(fail ? { status: 500, json: { error: 'Signing save failed.', settings: saved } } : { json: { ok: true, settings: { ...saved, enabled: true } } })
    : route.fulfill({ json: { ok: true, settings: saved } }));
  await page.locator('label:has(#commit-signing-enabled)').click();
  await page.locator('#review-settings-close').click();
  await page.waitForFunction(() => document.getElementById('review-settings-status').textContent.includes('Signing save failed'));
  assert.equal(await page.locator('#settings-gate').isVisible(), true);
  assert.equal(await page.locator('#commit-signing-enabled').isChecked(), true);
  fail = false;
  await page.locator('#review-settings-close').click();
  await page.waitForFunction(() => document.getElementById('settings-gate').classList.contains('hidden'));
});

test('saved signing can be disabled in Settings even when its key is missing', async t => {
  let settings = { ...held };
  const writes = [];
  const page = await openSettings(t, route => {
    if (route.request().method() === 'POST') {
      const payload = route.request().postDataJSON();
      writes.push(payload);
      settings = { ...empty };
    }
    return route.fulfill({ json: { ok: true, settings } });
  });
  assert.equal(await page.locator('#commit-signing-enabled').isEnabled(), true);
  await page.locator('label:has(#commit-signing-enabled)').click();
  await page.locator('#review-settings-close').click();
  await page.waitForFunction(() => document.getElementById('settings-gate').classList.contains('hidden'));
  assert.deepEqual(writes, [{ enabled: false }]);
  await page.goto(baseURL);
  await page.waitForFunction(() => !document.getElementById('commit-signing-save').disabled);
  assert.equal(await page.locator('#settings-gate').isVisible(), false);
  assert.equal(await page.locator('#commit-signing-enabled').isChecked(), false);
});

test('a required signing hold opens the signing tab, shows the banner, and blocks submissions', async t => {
  const submissions = [];
  const page = await newStubbedPage(t, route => route.fulfill({ json: { ok: true, settings: held } }));
  await page.route('**/api/local-prompt', route => {
    submissions.push(route.request().postDataJSON());
    return route.fulfill({ json: { ok: true, request_id: 'must-not-queue' } });
  });

  // No #settings hash: the hold alone has to put the operator on the signing tab.
  await page.goto(baseURL);
  await page.waitForFunction(() => !document.getElementById('settings-gate').classList.contains('hidden'));
  assert.equal(await page.locator('#settings-tab-signing').getAttribute('aria-selected'), 'true');
  const banner = page.locator('#commit-signing-setup-required');
  await banner.waitFor({ state: 'visible' });
  assert.match(await banner.textContent(), /Save a signing key/);

  // The capture-phase listener has to beat submitLocalPrompt, which would set
  // "Submitting..." and POST before anything later could stop it. A dispatched
  // submit reaches both listeners without depending on form validation.
  const cancelled = await page.evaluate(() => document.getElementById('local-prompt-form')
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })) === false);
  assert.equal(cancelled, true, 'the held submit was not cancelled');
  await page.waitForFunction(() => document.getElementById('local-prompt-status').textContent.includes('Save a signing key'));
  // submitLocalPrompt awaits only microtasks before its fetch, so anything that
  // slipped past the hold has posted by now.
  await page.waitForTimeout(250);
  assert.deepEqual(submissions, [], 'a held task reached the server');
});
