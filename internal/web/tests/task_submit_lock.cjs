// HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/task_submit_lock.cjs
const { after, before, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');

const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;

before(async () => {
  browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
});
after(async () => { await browser?.close(); });

test('Studio Run stays disabled until an in-progress task finishes', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  const writes = [];
  let status = 'running';
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));

  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (route.request().method() !== 'GET') writes.push(url.pathname);
    if (url.pathname === '/api/status') {
      return route.fulfill({ json: {
        tasks: [{ request_id: 'github-actions-1', source: 'github_actions', status }],
        events: [], repositories: [], stats: {}, resources: {},
      } });
    }
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  const run = page.locator('#local-prompt-submit');
  await expect(run).toBeDisabled();
  await expect(run).toHaveAttribute('title', 'Wait for the current task to finish');

  const cancelled = await page.evaluate(() => document.getElementById('local-prompt-form')
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })) === false);
  assert.equal(cancelled, true);
  assert.deepEqual(writes, [], 'active task must block programmatic submission too');

  status = 'completed';
  await page.reload();
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
});
