const { test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');

const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';

test('Close survives overlapping tab retries and a deferred Removed redraw', async t => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  t.after(() => browser.close());
  const context = await browser.newContext();
  const page = await context.newPage();
  const otherTab = await context.newPage();
  const errors = [];
  context.on('weberror', event => errors.push(event.error().message));
  context.on('console', message => {
    if (message.type() === 'error' && !message.text().startsWith('Failed to load resource:')) errors.push(message.text());
  });
  t.after(() => assert.deepEqual(errors, []));

  let attempts = 0;
  let otherAttempts = 0;
  let allowDelete = false;
  let allowOtherDelete = false;
  let allowResumedDelete = false;
  let releaseFirstAttempt;
  const firstAttempt = new Promise(resolve => { releaseFirstAttempt = resolve; });
  await context.route('**/*', async route => {
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
      case '/api/tasks/other/delete':
        otherAttempts++;
        return allowOtherDelete
          ? route.continue()
          : route.fulfill({ status: 503, json: { error: 'temporarily unavailable' } });
      case '/api/tasks/resumed/delete':
        return allowResumedDelete
          ? route.continue()
          : route.fulfill({ status: 503, json: { error: 'temporarily unavailable' } });
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
  await otherTab.goto(`${baseURL}/#current-work`);
  const cardSelector = '#task-list .task[data-request-id="finished"]';
  const card = page.locator(cardSelector);
  const close = card.getByRole('button', { name: 'Delete task' });
  const otherCardSelector = '#task-list .task[data-request-id="other"]';
  const otherCard = otherTab.locator(otherCardSelector);
  const otherClose = otherCard.getByRole('button', { name: 'Delete task' });
  await expect(close).toBeVisible();
  await expect(otherClose).toBeVisible();
  await card.locator('.task-id').evaluate(label => window.getSelection().selectAllChildren(label));
  try {
    await close.click();
    await expect(card).toHaveCount(0);
    await expect.poll(() => attempts).toBe(1);
  } finally {
    releaseFirstAttempt();
  }
  await otherClose.click();
  await expect(otherCard).toHaveCount(0);
  await expect.poll(() => otherAttempts).toBeGreaterThanOrEqual(1);
  await expect.poll(() => attempts).toBeGreaterThanOrEqual(2);
  const status = await page.request.get(`${baseURL}/api/status`);
  const snapshot = await status.json();
  assert.ok(snapshot.tasks.some(task => task.request_id === 'finished'));
  await page.evaluate(snapshot => render(snapshot), snapshot);
  await expect(card).toHaveCount(0);
  await expect(page.locator('#local-prompt-status')).not.toContainText('Delete failed');

  assert.equal((await otherTab.request.post(`${baseURL}/test/rerun`)).status(), 204);
  allowDelete = true;
  const deleted = page.waitForResponse(response => response.url().endsWith('/api/tasks/finished/delete') && response.ok());
  await page.reload();
  assert.equal((await deleted).status(), 200);
  await expect(card).toHaveCount(0);
  await expect(page.locator(otherCardSelector)).toHaveCount(0);
  await expect(page.locator('#task-list .task[data-request-id="later"]')).toBeVisible();
  await expect.poll(() => page.evaluate(() => localStorage.getItem('hubui.pendingTaskDeletes.finished'))).toBeNull();

  await otherTab.close();
  allowOtherDelete = true;
  await page.evaluate(() => localStorage.setItem('hubui.pendingTaskDeletes', JSON.stringify(['legacy'])));
  const otherDeleted = page.waitForResponse(response => response.url().endsWith('/api/tasks/other/delete') && response.ok());
  const legacyDeleted = page.waitForResponse(response => response.url().endsWith('/api/tasks/legacy/delete'));
  await page.reload();
  const [otherResponse, legacyResponse] = await Promise.all([otherDeleted, legacyDeleted]);
  assert.equal(otherResponse.status(), 200);
  assert.equal(legacyResponse.status(), 404);
  assert.deepEqual(legacyResponse.request().postDataJSON(), { attempt_generations: {} });
  await expect.poll(() => page.evaluate(() => Object.keys(localStorage).filter(key =>
    key.startsWith('hubui.pendingTaskDeletes')
  ))).toEqual([]);

  const resumedCard = page.locator('#task-list .task[data-request-id="resumed"]');
  const resumedClose = resumedCard.getByRole('button', { name: 'Delete task' });
  await expect(resumedClose).toBeVisible();
  const resumedDeleteURL = '/api/tasks/resumed/delete';
  const failed = page.waitForResponse(response => response.url().endsWith(resumedDeleteURL));
  await resumedClose.click();
  await expect(resumedCard).toHaveCount(0);
  const firstDelete = await failed;
  assert.equal(firstDelete.status(), 503);
  const oldScope = firstDelete.request().postDataJSON();
  assert.ok(oldScope.attempt_generations.resumed);

  assert.equal((await page.request.post(`${baseURL}/test/resume`)).status(), 204);
  allowResumedDelete = true;
  const settled = page.waitForResponse(response => response.url().endsWith(resumedDeleteURL));
  await page.reload();
  const staleDelete = await settled;
  assert.equal(staleDelete.status(), 404);
  assert.deepEqual(staleDelete.request().postDataJSON(), oldScope);
  await expect(resumedClose).toBeVisible();
  await expect.poll(() => page.evaluate(() => localStorage.getItem('hubui.pendingTaskDeletes.resumed'))).toBeNull();

  const freshDelete = page.waitForResponse(response => response.url().endsWith(resumedDeleteURL));
  await resumedClose.click();
  await expect(resumedCard).toHaveCount(0);
  const freshResponse = await freshDelete;
  assert.equal(freshResponse.status(), 200);
  assert.notEqual(freshResponse.request().postDataJSON().attempt_generations.resumed, oldScope.attempt_generations.resumed);

  const filter = page.locator('#task-history-toggle');
  await filter.click();
  await expect(card).toBeVisible();
  const removedTab = await context.newPage();
  await removedTab.goto(`${baseURL}/#current-work`);
  await expect(removedTab.locator(cardSelector)).toBeVisible();
  const otherRemovedCard = removedTab.locator(otherCardSelector);
  await expect(otherRemovedCard).toBeVisible();
  await card.locator('.task-id').evaluate(label => window.getSelection().selectAllChildren(label));
  await filter.dispatchEvent('click');
  await expect(filter).toHaveAttribute('data-filter', 'running');
  await expect(card).toBeVisible();
  const alreadyDeleted = page.waitForResponse(response => response.url().endsWith('/api/tasks/finished/delete'));
  await close.click();
  await expect(card).toHaveCount(0);
  assert.equal((await alreadyDeleted).status(), 404);
  await expect.poll(() => page.evaluate(() => localStorage.getItem('hubui.pendingTaskDeletes.finished'))).toBeNull();
  await filter.click();
  await filter.click();
  await page.reload();
  await expect(filter).toHaveAttribute('data-filter', 'removed');
  await expect(page.locator(otherCardSelector)).toBeVisible();
  await expect(card).toHaveCount(0);

  await otherRemovedCard.getByRole('button', { name: 'Delete task' }).click();
  await expect(otherRemovedCard).toHaveCount(0);
  await page.reload();
  await expect(filter).toHaveAttribute('data-filter', 'removed');
  await expect(page.locator(otherCardSelector)).toHaveCount(0);
  await expect(card).toHaveCount(0);
});
