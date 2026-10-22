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

async function openSettings(t, handler, viewport = { width: 1280, height: 900 }) {
  const page = await browser.newPage({ viewport });
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.route('**/api/agent-auth**', route => route.fulfill({ json: { ok: true, auth: { required: true, ready: true, harness: 'claude', state: 'ready' } } }));
  await page.route('**/api/review-settings', route => route.fulfill({ json: { ok: true, settings: { merge_method: 'squash', review_level: 'off' } } }));
  await page.route('**/api/commit-signing**', handler);
  await page.goto(`${baseURL}/#settings`);
  await page.locator('#commit-signing-section summary').click();
  await page.waitForFunction(() => !document.getElementById('commit-signing-save').disabled);
  return page;
}

const empty = { enabled: false, configured: false, registration: 'not_configured' };
const saved = { ...empty, configured: true, public_key: 'ssh-ed25519 TEST_PUBLIC_KEY', fingerprint: 'SHA256:test', account: 'test-bot', registration: 'not_registered' };

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
  await page.locator('#commit-signing-private-key').fill('test-private-key');
  await page.locator('#commit-signing-passphrase').fill('test-passphrase');
  await page.locator('#commit-signing-save').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-status').textContent.startsWith('Key saved.'));
  assert.equal(await page.locator('#commit-signing-private-key').inputValue(), '');
  assert.equal(await page.locator('#commit-signing-passphrase').inputValue(), '');
  assert.equal(await page.locator('#commit-signing-public-key').inputValue(), saved.public_key);
  assert.match(await page.locator('#commit-signing-account').textContent(), /test-bot/);
  await page.locator('#commit-signing-verify').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-registration').textContent.startsWith('Registered'));
  await page.locator('#commit-signing-enabled').check();
  await page.locator('#commit-signing-save').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-status').textContent === 'Commit signing settings saved.');
  assert.equal(settings.enabled, true);
  await page.reload();
  await page.locator('#commit-signing-section summary').click();
  await page.waitForFunction(() => document.getElementById('commit-signing-enabled').checked);
  assert.equal(await page.locator('#commit-signing-private-key').inputValue(), '');
  await page.locator('#commit-signing-enabled').uncheck();
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
  assert.equal(await page.locator('#commit-signing-private-key').inputValue(), '');
});
