const { test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');

const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';

test('Close dismisses immediately and resumes pending deletion after reload', async t => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  t.after(() => browser.close());
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => {
    if (message.type() === 'error' && !message.text().startsWith('Failed to load resource:')) errors.push(message.text());
  });
  t.after(() => assert.deepEqual(errors, []));

  let attempts = 0;
  let allowDelete = false;
  let releaseFirstAttempt;
  const firstAttempt = new Promise(resolve => { releaseFirstAttempt = resolve; });
  await page.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    switch (url.pathname) {
      case '/api/tasks/finished/delete':
        attempts++;
        if (allowDelete) return route.continue();
        if (attempts === 1) {
          await firstAttempt;
          return route.abort('connectionreset');
        }
        return route.fulfill({ status: 503, json: { error: 'temporarily unavailable' } });
      case '/api/agent-auth':
        return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
      case '/api/github/repos':
        return route.fulfill({ json: { ok: true, repos: [] } });
      default:
        return route.continue();
    }
  });

  await page.addInitScript(() => {
    if (!localStorage.getItem('hubui.taskStatusFilter')) {
      localStorage.setItem('hubui.taskStatusFilter', 'completed');
    }
  });
  await page.goto(`${baseURL}/#current-work`);
  const card = page.locator('#task-list .task[data-request-id="finished"]');
  const close = card.getByRole('button', { name: 'Delete task' });
  await expect(close).toBeVisible();
  await card.locator('.task-id').evaluate(label => window.getSelection().selectAllChildren(label));
  try {
    await close.click();
    await expect(card).toHaveCount(0);
    await expect.poll(() => attempts).toBe(1);
  } finally {
    releaseFirstAttempt();
  }
  await expect.poll(() => attempts).toBeGreaterThanOrEqual(2);
  const status = await page.request.get(`${baseURL}/api/status`);
  const snapshot = await status.json();
  assert.ok(snapshot.tasks.some(task => task.request_id === 'finished'));
  await page.evaluate(snapshot => render(snapshot), snapshot);
  await expect(card).toHaveCount(0);
  await expect(page.locator('#local-prompt-status')).not.toContainText('Delete failed');

  allowDelete = true;
  const deleted = page.waitForResponse(response => response.url().endsWith('/api/tasks/finished/delete') && response.ok());
  await page.reload();
  assert.equal((await deleted).status(), 200);
  await expect(card).toHaveCount(0);
  await expect.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem('hubui.pendingTaskDeletes')))).toEqual([]);

  await page.locator('#task-history-toggle').click();
  await expect(card).toBeVisible();
  const alreadyDeleted = page.waitForResponse(response => response.url().endsWith('/api/tasks/finished/delete'));
  await close.click();
  await expect(card).toHaveCount(0);
  assert.equal((await alreadyDeleted).status(), 404);
  await expect.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem('hubui.pendingTaskDeletes')))).toEqual([]);
  await page.reload();
  await expect(page.locator('#task-history-toggle')).toHaveAttribute('data-filter', 'removed');
  await expect(card).toHaveCount(0);
});
