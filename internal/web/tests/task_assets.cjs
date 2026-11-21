// The Go fixture serves real issue-derived run configs through the broker API.
const { after, before, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
const screenshotURL = 'https://github.com/user-attachments/assets/screenshot-id';
const pdfURL = 'https://github.com/user-attachments/files/42/spec.pdf';
const unavailableURL = 'https://github.com/user-attachments/assets/unavailable';

before(async () => { browser = await chromium.launch({ channel: 'chromium', headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

for (const mobile of [false, true]) {
  test(`${mobile ? 'mobile' : 'desktop'}: task assets open in a navigable preview dialog`, async t => {
    const page = await browser.newPage({
      viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 },
      isMobile: mobile, hasTouch: mobile,
    });
    page.setDefaultTimeout(10000);
    t.after(() => page.close());
    const errors = [], writes = [], attachmentRequests = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => {
      if (message.type() === 'error' && !message.text().startsWith('Failed to load resource:')) errors.push(message.text());
    });
    await page.route(/^https?:/, route => {
      const url = new URL(route.request().url());
      if (url.origin !== new URL(baseURL).origin) {
        if (url.pathname.includes('/user-attachments/')) errors.push('Browser requested a GitHub attachment');
        return route.abort();
      }
      if (!url.pathname.startsWith('/api/')) return route.continue();
      if (route.request().method() !== 'GET') writes.push(url.pathname);
      if (url.pathname.endsWith('/asset-preview')) {
        attachmentRequests.push(url.pathname + url.search);
        return route.continue();
      }
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
    assert.deepEqual(attachmentRequests, [], 'previews load only when requested');
    if (mobile) await assets.tap(); else await assets.click();
    const viewer = page.locator('#task-asset-viewer');
    const previous = viewer.getByRole('button', { name: 'Previous asset' });
    const next = viewer.getByRole('button', { name: 'Next asset' });
    const source = viewer.getByRole('link', { name: 'Open on GitHub' });
    const count = viewer.locator('#task-asset-count');
    await expect(viewer).toBeVisible();
    await expect(page.locator('#task-prompt-modal')).toBeHidden();
    await expect(viewer.getByRole('img', { name: 'screenshot-id' })).toBeVisible();
    await expect(count).toHaveText('1 / 2');
    await expect(source).toHaveAttribute('href', screenshotURL);
    assert.deepEqual(attachmentRequests, ['/api/tasks/req-assets/asset-preview?index=0']);
    const pdfResponse = page.waitForResponse(response => response.url().endsWith('/asset-preview?index=1'));
    await next.click();
    const response = await pdfResponse;
    assert.equal(response.status(), 200);
    assert.equal(response.headers()['content-type'], 'application/pdf');
    assert.equal(response.headers()['cache-control'], 'no-store');
    assert.equal(response.headers()['authorization'], undefined);
    await expect(viewer).toHaveAccessibleName('spec.pdf');
    await expect(viewer.getByTitle('spec.pdf')).toBeVisible();
    await expect(viewer.getByTitle('spec.pdf')).toHaveAttribute('src', /^blob:/);
    await expect(viewer.getByRole('status')).toBeHidden();
    const pdfFrame = () => page.frames().find(frame =>
      frame.url().startsWith('chrome-extension:') && frame.parentFrame()?.url().startsWith('blob:'),
    );
    await expect.poll(() => Boolean(pdfFrame())).toBe(true);
    await expect(pdfFrame().locator('pdf-viewer embed')).toBeVisible();
    await expect(source).toHaveAttribute('href', pdfURL);
    await expect(count).toHaveText('2 / 2');
    await previous.click();
    await expect(viewer.getByRole('img', { name: 'screenshot-id' })).toBeVisible();
    await page.keyboard.press('ArrowLeft');
    await expect(count).toHaveText('2 / 2');
    await expect.poll(() => attachmentRequests.length).toBe(4);
    await page.keyboard.press('Escape');
    await expect(viewer).toBeHidden();
    assert.deepEqual(attachmentRequests, [0, 1, 0, 1].map(index => `/api/tasks/req-assets/asset-preview?index=${index}`),
      'only the current asset preview is requested');

    const singleAsset = page.locator('#task-list .task[data-request-id="req-single-asset"] .task-assets-toggle');
    await expect(singleAsset).toHaveText('1 asset');
    await singleAsset.click();
    await expect(viewer.getByRole('status')).toHaveText('Preview unavailable.');
    await expect(source).toHaveAttribute('href', unavailableURL);
    await expect(previous).toBeHidden();
    await expect(next).toBeHidden();
    await viewer.getByRole('button', { name: 'Close asset viewer' }).click();
    await expect(viewer).toBeHidden();
    await expect(singleAsset).toBeFocused();
    assert.deepEqual(errors, [], 'no browser JavaScript errors');
    assert.deepEqual(writes, [], 'viewing assets must not submit or modify a task');
  });
}
