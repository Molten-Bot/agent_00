// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/agent_auth.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('@playwright/test');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => {
  browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
});
after(async () => { await browser?.close(); });

async function openPage(t, authHandler) {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => {
    if (new URL(route.request().url()).origin !== new URL(baseURL).origin) return route.abort();
    return route.continue();
  });
  if (authHandler) await page.route('**/api/agent-auth**', authHandler);
  await page.goto(baseURL);
  return page;
}
const ready = { harness: 'claude', required: true, ready: true, state: 'ready' };
const needsLogin = { harness: 'claude', required: true, ready: false, state: 'needs_browser_login' };
const pending = {
  ...needsLogin, state: 'pending_browser_login', accepts_browser_code: true,
  auth_url: 'https://claude.com/cai/oauth/authorize?code=true&state=test-state',
};
const reply = (route, auth) => route.fulfill({ json: { ok: true, auth } });

test('first load checks auth before enabling submission and displays the Claude link', async t => {
  let release;
  const waiting = new Promise(resolve => { release = resolve; });
  let requests = 0;
  const page = await openPage(t, async route => {
    requests++;
    await waiting;
    await reply(route, pending);
  });
  await expect(page.locator('#local-prompt-submit')).toBeDisabled();
  await expect(page.locator('#agent-auth-message')).toHaveText('Checking agent authorization…');
  release();
  await expect(page.locator('#agent-auth-title')).toHaveText('Setup Claude');
  await expect(page.locator('#agent-auth-url')).toBeVisible();
  await expect(page.locator('#agent-auth-url')).toHaveText('Open Claude login');
  await expect(page.locator('#agent-auth-browser-code-input')).toBeVisible();
  assert.ok(requests > 0, 'initial auth status was requested');
});

test('GitHub setup and agent selection continue into Claude browser authorization', async t => {
  let auth = { required: true, ready: false, state: 'needs_configure', configure_command: 'gh auth token' };
  const page = await openPage(t, async route => {
    const request = route.request();
    if (request.url().endsWith('/configure')) {
      const input = request.postDataJSON();
      if (auth.state === 'needs_configure') {
        assert.equal(input.github_token, 'github_token_test_only');
        auth = { required: true, ready: false, state: 'needs_harness_selection', configure_options: [
          { value: 'claude', label: 'Claude Code' }, { value: 'codex', label: 'Codex' },
        ] };
      } else if (auth.state === 'needs_harness_selection') {
        assert.equal(input.value, 'claude');
        auth = needsLogin;
      } else {
        assert.equal(input.claude_auth_code, 'browser-code#test-state');
        auth = ready;
      }
    } else if (request.url().endsWith('/verify') && !auth.ready) {
      auth = pending;
    }
    await reply(route, auth);
  });
  await expect(page.locator('#agent-auth-title')).toHaveText('Setup GitHub');
  await page.locator('#agent-auth-configure-secret-input').fill('github_token_test_only');
  await page.locator('#agent-auth-verify').click();
  await expect(page.locator('#agent-auth-title')).toHaveText('Select Agent');
  await page.locator('[data-agent-harness="claude"]').click();
  await expect(page.locator('#agent-auth-verify')).toHaveText('Sign in to Claude');
  await page.locator('#agent-auth-verify').click();
  await expect(page.locator('#agent-auth-url')).toBeVisible();
  await page.locator('#agent-auth-browser-code-input').fill('browser-code#test-state');
  await page.locator('#agent-auth-verify').click();
  await expect(page.locator('#agent-auth-gate')).toBeHidden();
  await expect(page.locator('#local-prompt-submit')).toBeEnabled();
  await page.reload();
  await expect(page.locator('#agent-auth-gate')).toBeHidden();
  await expect(page.locator('#local-prompt-submit')).toBeEnabled();
});

test('auth status errors block submission and polling recovers', async t => {
  let fail = true;
  const page = await openPage(t, route => fail
    ? route.fulfill({ status: 503, json: { error: 'Auth temporarily unavailable' } })
    : reply(route, ready));
  await expect(page.locator('#agent-auth-message')).toContainText('Auth temporarily unavailable');
  await expect(page.locator('#local-prompt-submit')).toBeDisabled();
  fail = false;
  await expect(page.locator('#agent-auth-gate')).toBeHidden();
  await expect(page.locator('#local-prompt-submit')).toBeEnabled();
});

test('already authorized users can start working', async t => {
  const page = await openPage(t, route => reply(route, ready));
  await expect(page.locator('#agent-auth-gate')).toBeHidden();
  await expect(page.locator('#local-prompt-submit')).toBeEnabled();
});

test('submission rejected for expired auth reopens onboarding and keeps work disabled', async t => {
  let auth = ready;
  const page = await openPage(t, route => reply(route, auth));
  await page.route('**/api/local-prompt', route => {
    auth = needsLogin;
    return route.fulfill({ status: 400, json: { error: 'agent auth required: complete agent authorization in the UI' } });
  });
  await expect(page.locator('#local-prompt-submit')).toBeEnabled();
  await page.locator('#builder-repo-input').fill('https://github.com/example/project');
  await page.locator('#builder-prompt-input').fill('Update the documentation');
  await page.locator('#local-prompt-submit').click();
  await expect(page.locator('#agent-auth-gate')).toBeVisible();
  await expect(page.locator('#agent-auth-verify')).toHaveText('Sign in to Claude');
  await expect(page.locator('#local-prompt-submit')).toBeDisabled();
});

test('live container exposes the installed Claude CLI browser challenge', {
  skip: process.env.HARNESS_TEST_LIVE_CLAUDE !== '1',
}, async t => {
  const page = await openPage(t);
  await expect(page.locator('#agent-auth-title')).toHaveText('Setup Claude');
  const signIn = page.getByRole('button', { name: 'Sign in to Claude', exact: true });
  if (await signIn.isVisible()) await signIn.click();
  await expect(page.locator('#agent-auth-url')).toBeVisible({ timeout: 15000 });
  await expect(page.locator('#agent-auth-url')).toHaveAttribute('href', /^https:\/\/claude\.(com|ai)\//);
  await expect(page.locator('#agent-auth-browser-code-input')).toBeVisible();
  await expect(page.locator('#local-prompt-submit')).toBeDisabled();
});
