const { test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');

const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';

function pendingDeleteKeys(tab, requestID) {
  return tab.evaluate(id => Object.keys(localStorage).filter(key => id
    ? key === `hubui.pendingTaskDeletes.${id}` || key.startsWith(`hubui.pendingTaskDeletes:[${JSON.stringify(id)},`)
    : key.startsWith('hubui.pendingTaskDeletes')
  ), requestID);
}

test('Close redraws cached history before the first snapshot', async t => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  t.after(() => browser.close());
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, []));
  const now = new Date().toISOString();
  await page.addInitScript(task => {
    localStorage.setItem('hubui.taskStatusFilter', 'completed');
    localStorage.setItem('hubui.taskHistory.v1', JSON.stringify([task, { ...task, request_id: 'other-cached' }]));
  }, {
    request_id: 'cached',
    status: 'completed',
    started_at: now,
    updated_at: now,
    delete_attempt_generations: { cached: 'cached-generation' },
  });
  const origin = new URL(baseURL).origin;
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== origin) return route.abort();
    switch (url.pathname) {
      case '/api/status':
      case '/api/tasks/cached/delete':
        return route.fulfill({ status: 503 });
      case '/api/stream':
        return route.fulfill({ contentType: 'text/event-stream', body: '' });
      case '/api/agent-auth':
        return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
      case '/api/github/repos':
        return route.fulfill({ json: { ok: true, repos: [] } });
      default:
        return route.continue();
    }
  });
  await page.goto(`${baseURL}/#current-work`);
  const card = page.locator('#task-list .task[data-request-id="cached"]');
  const close = card.getByRole('button', { name: 'Delete task' });
  await expect(close).toBeVisible();
  const deletion = page.waitForResponse(response => response.url().endsWith('/api/tasks/cached/delete'));
  await close.click();
  await expect(card).toHaveCount(0);
  assert.equal((await deletion).status(), 503);
  await page.reload();
  await expect(page.locator('#task-list .task[data-request-id="other-cached"]')).toBeVisible();
  await expect(card).toHaveCount(0);
});

test('Close keeps separate scopes and generations across tabs', async t => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  t.after(() => browser.close());
  const context = await browser.newContext();
  const staleTab = await context.newPage();
  const freshTab = await context.newPage();
  const errors = [];
  context.on('weberror', event => errors.push(event.error().message));
  t.after(() => assert.deepEqual(errors, []));

  const requests = [];
  let generation = 'old';
  let siblingCompleted = false;
  let allowDelete = false;
  const now = new Date().toISOString();
  const snapshot = () => ({
    tasks: [{
      request_id: 'same-result',
      status: 'completed',
      started_at: now,
      updated_at: now,
      delete_attempt_generations: {
        'same-result': generation,
        ...(siblingCompleted ? { sibling: 'sibling-generation' } : {}),
      },
    }, {
      request_id: 'still-running',
      status: 'running',
      started_at: now,
    }],
  });
  const origin = new URL(baseURL).origin;
  await context.addInitScript(origin => {
    if (location.origin === origin) localStorage.setItem('hubui.taskStatusFilter', 'completed');
  }, origin);
  await context.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== origin) return route.abort();
    switch (url.pathname) {
      case '/api/status':
        return route.fulfill({ json: snapshot() });
      case '/api/stream':
        return route.fulfill({ contentType: 'text/event-stream', body: '' });
      case '/api/agent-auth':
        return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
      case '/api/github/repos':
        return route.fulfill({ json: { ok: true, repos: [] } });
      case '/api/tasks/same-result/delete':
        requests.push(route.request().postDataJSON().attempt_generations);
        return route.fulfill({ status: allowDelete ? 404 : 503 });
      default:
        return route.continue();
    }
  });

  const card = tab => tab.locator('#task-list .task[data-request-id="same-result"]');
  const close = tab => card(tab).getByRole('button', { name: 'Delete task' });
  for (const tab of [staleTab, freshTab]) {
    await tab.goto(`${baseURL}/#current-work`);
    await expect(close(tab)).toBeVisible();
  }
  generation = 'resumed';
  const narrowTab = await context.newPage();
  await narrowTab.goto(`${baseURL}/#current-work`);
  await expect(close(narrowTab)).toBeVisible();
  siblingCompleted = true;
  // Deliver only the next terminal snapshot, as when the stream coalesces updates.
  await freshTab.evaluate(next => render(next), snapshot());
  await close(freshTab).click();
  const widerScope = { 'same-result': 'resumed', sibling: 'sibling-generation' };
  await expect.poll(() => requests[0]).toEqual(widerScope);
  await expect(card(freshTab)).toHaveCount(0);
  await close(narrowTab).click();
  await expect(card(narrowTab)).toHaveCount(0);
  await close(staleTab).click();
  await expect(card(staleTab)).toHaveCount(0);
  await expect.poll(() => requests).toContainEqual({ 'same-result': 'old' });
  const storedScopes = await freshTab.evaluate(() => Object.entries(localStorage)
    .filter(([key]) => key.startsWith('hubui.pendingTaskDeletes:'))
    .map(([, scope]) => JSON.parse(scope)));
  assert.equal(storedScopes.length, 3);
  assert.deepEqual(storedScopes.find(scope => scope.sibling), widerScope);
  assert.ok(storedScopes.some(scope => scope['same-result'] === 'resumed' && !scope.sibling));
  await staleTab.close();
  await freshTab.close();
  await narrowTab.close();

  requests.length = 0;
  allowDelete = true;
  const reloaded = await context.newPage();
  await reloaded.goto(`${baseURL}/#current-work`);
  await expect.poll(() => [...new Set(requests.map(scope => scope['same-result']))].sort()).toEqual(['old', 'resumed']);
  assert.deepEqual(requests.find(scope => scope['same-result'] === 'resumed'), widerScope);
  await expect.poll(() => pendingDeleteKeys(reloaded)).toEqual([]);
  // Both dismissal markers survive after the pending requests have been cleared.
  for (const dismissedGeneration of ['resumed', 'old']) {
    generation = dismissedGeneration;
    await reloaded.reload();
    await expect(reloaded.locator('#task-list .task[data-request-id="still-running"]')).toBeVisible();
    await expect(card(reloaded)).toHaveCount(0);
  }
});

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
  await expect.poll(() => pendingDeleteKeys(page, 'finished')).toEqual([]);

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
  await expect.poll(() => pendingDeleteKeys(page)).toEqual([]);

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
  await expect.poll(() => pendingDeleteKeys(page, 'resumed')).toEqual([]);

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
  await page.bringToFront();
  await card.locator('.task-id').evaluate(label => {
    window.getSelection().selectAllChildren(label);
    document.getElementById('task-history-toggle').click();
  });
  await expect(filter).toHaveAttribute('data-filter', 'running');
  await expect(card).toBeVisible();
  const alreadyDeleted = page.waitForResponse(response => response.url().endsWith('/api/tasks/finished/delete'));
  await close.click();
  await expect(card).toHaveCount(0);
  assert.equal((await alreadyDeleted).status(), 404);
  await expect.poll(() => pendingDeleteKeys(page, 'finished')).toEqual([]);
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
