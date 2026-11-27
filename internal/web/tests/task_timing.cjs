const { test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');

test('Studio history displays timing from both attempts', async t => {
  const baseURL = process.env.HARNESS_TEST_URL;
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  t.after(() => browser.close());
  const page = await browser.newPage({ locale: 'en-US' });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', msg => {
    if (msg.type() === 'error') errors.push(`${msg.location().url} ${msg.text()}`);
  });
  await page.addInitScript(() => localStorage.setItem('hubui.taskStatusFilter', 'completed'));
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.fulfill({ status: 204 });
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/commit-signing') return route.fulfill({ json: { ok: true, signing: { ready: true } } });
    return route.continue();
  });
  await page.goto(`${baseURL}/#current-work`);
  const task = page.locator('#task-list .task[data-request-id="delivery"]');
  const timing = task.locator('.task-stage-timing');
  const timingSummary = timing.getByText('Stage timing', { exact: true });
  const attemptHistory = task.locator('.task-issue-history details');
  await timingSummary.click({ timeout: 5000 });
  await expect(timing.getByText('Attempt 1')).toBeVisible();
  await expect(timing.getByText('Attempt 2')).toBeVisible();
  await expect(timing.getByText('Implementation · 5,000 ms · Agent 3,000 ms')).toBeVisible();
  await expect(timing.getByText('CI wait · 20,000 ms')).toBeVisible();
  await expect(timing.getByText('Mark ready · Unknown')).toBeVisible();
  await expect(timing.getByText('Workflow total · 60,000 ms')).toBeVisible();
  await expect(timing.locator('time[datetime="2026-11-27T12:01:00Z"]')).toBeVisible();
  await expect(attemptHistory).toHaveJSProperty('open', false);
  await attemptHistory.getByText('Attempt history (2)', { exact: true }).click();
  await expect(timing.getByText('Workflow total · 60,000 ms')).toBeVisible();
  await timingSummary.click();
  await expect(attemptHistory).toHaveJSProperty('open', true);
  await expect(timing).toHaveJSProperty('open', false);
  assert.deepEqual(errors, []);
});
