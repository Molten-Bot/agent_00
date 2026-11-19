// The Go fixture serves real issue-derived run configs through the broker API.
const { after, before, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;

before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

for (const mobile of [false, true]) {
  test(`${mobile ? 'mobile' : 'desktop'}: issue assets open their source list without starting a task`, async t => {
    const page = await browser.newPage({
      viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 },
      isMobile: mobile, hasTouch: mobile,
    });
    page.setDefaultTimeout(10000);
    t.after(() => page.close());
    const errors = [], writes = [], externalRequests = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.route('**/*', route => {
      const url = new URL(route.request().url());
      if (url.origin !== new URL(baseURL).origin) {
        externalRequests.push(url.href);
        return route.abort();
      }
      if (!url.pathname.startsWith('/api/')) return route.continue();
      if (route.request().method() !== 'GET') writes.push(url.pathname);
      if (url.pathname === '/api/status') return route.continue();
      if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
      let json = { ok: true };
      if (url.pathname === '/api/agent-auth') json.auth = { required: false, ready: true, state: 'ready' };
      if (url.pathname === '/api/review-settings') json.settings = { github_issues_enabled: true };
      if (url.pathname === '/api/github/repos') json.repos = [];
      if (url.pathname === '/api/library') json.tasks = [];
      return route.fulfill({ json });
    });

    await page.goto(`${baseURL}/#current-work`);
    const task = page.locator('#task-list .task[data-request-id="req-assets"]');
    const assets = task.locator('.task-assets-toggle');
    await expect(assets).toHaveText('2 assets');
    await expect(page.locator('#task-list .task[data-request-id="req-no-assets"] .task-assets-toggle')).toHaveCount(0);
    if (mobile) await assets.tap(); else await assets.click();
    await expect(page.locator('#task-prompt-modal')).toBeVisible();
    const body = page.locator('#task-prompt-modal-body');
    await expect(body).toContainText('Task assets (2)');
    await expect(body).toContainText('screenshot-id (Issue description)');
    await expect(body).toContainText('spec.pdf (Comment 1)');
    await expect(body.locator('a[href="https://github.com/user-attachments/files/42/spec.pdf"]').first()).toBeVisible();
    await page.locator('#task-prompt-modal-plain').click();
    await expect(body).toContainText('https://github.com/user-attachments/assets/screenshot-id');
    await page.locator('#task-prompt-modal-close').click();
    await expect(page.locator('#task-prompt-modal')).toBeHidden();
    assert.deepEqual(errors, [], 'no browser JavaScript errors');
    assert.deepEqual(writes, [], 'viewing assets must not submit or modify a task');
    assert.ok(!externalRequests.some(url => url.includes('/user-attachments/')), 'opening the list must not fetch private attachments in the browser');
  });
}
