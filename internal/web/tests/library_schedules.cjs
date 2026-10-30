// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/library_schedules.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
//
// hoursForIndex/indexForHours in static/library-schedules.js are unexported
// closures with no Go-reachable equivalent, so these tests drive the real
// slider and editor in a headless browser to cover the hours -> days -> weeks
// snap table and the reverse mapping used when reopening a schedule.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

async function openPage(t) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.goto(baseURL);
  await page.waitForFunction(() => Boolean(window.LibrarySchedulesUI));
  return page;
}

test('slider progresses from hours to days to weeks and tops out at two weeks', async t => {
  const page = await openPage(t);
  const samples = await page.evaluate(() => {
    const slider = document.getElementById('library-schedule-hours');
    const frequency = document.getElementById('library-schedule-frequency');
    const read = index => {
      slider.value = String(index);
      slider.dispatchEvent(new Event('input'));
      return frequency.textContent;
    };
    return { hours: read(22), day: read(23), days: read(28), week: read(29), max: read(Number(slider.max)) };
  });
  assert.equal(samples.hours, 'every 23 hours');
  assert.equal(samples.day, 'every 1 day');
  assert.equal(samples.days, 'every 6 days');
  assert.equal(samples.week, 'every 1 week');
  assert.equal(samples.max, 'every 2 weeks');
});

test('reopening a schedule snaps an off-grid saved interval to its nearest step', async t => {
  const page = await openPage(t);
  const saved = await page.evaluate(async () => {
    const response = await fetch('/api/library/schedules', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ libraryTaskName: 'unit-test-coverage', repo: 'acme/repo', intervalHours: 10 }),
    });
    return response.json();
  });
  assert.equal(saved.ok, true, `expected schedule PUT to succeed: ${JSON.stringify(saved)}`);

  await page.goto(`${baseURL}/#studio-library`);
  await page.waitForSelector('#library-schedules-open');
  await page.locator('#library-schedules-open').click();
  const editButton = page.locator('button[title^="Edit schedule"]').first();
  await editButton.waitFor();
  await editButton.click();
  await page.waitForSelector('#library-schedule-editor:not([hidden])');

  const frequencyText = await page.locator('#library-schedule-frequency').textContent();
  assert.equal(frequencyText, 'every 10 hours', `expected exact 10-hour step to be selected, got ${frequencyText}`);
});

test('Run Now saves schedule and submits one immediate library run', async t => {
  const page = await openPage(t);
  let submitted = null;
  await page.route('**/api/library/run', async route => {
    submitted = route.request().postDataJSON();
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, request_id: 'run-now-test' }) });
  });
  const saved = await page.evaluate(async () => {
    const response = await fetch('/api/library/schedules', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ libraryTaskName: 'unit-test-coverage', repo: 'acme/run-now', intervalHours: 24 }),
    });
    return response.json();
  });
  assert.equal(saved.ok, true);

  await page.goto(`${baseURL}/#studio-library`);
  await page.waitForFunction(() => Boolean(window.LibrarySchedulesUI));
  await page.locator('#library-schedules-open').click();
  await page.locator('.library-schedule-row').filter({ hasText: 'acme/run-now' }).locator('button[title^="Edit schedule"]').click();
  const thumb = page.locator('#library-schedule-run-now-field .prompt-draft-toggle-thumb');
  const uncheckedTransform = await thumb.evaluate(element => getComputedStyle(element).transform);
  await page.locator('#library-schedule-run-now-field').click();
  assert.equal(await page.locator('#library-schedule-run-now').isChecked(), true);
  await page.waitForFunction(
    previous => getComputedStyle(document.querySelector('#library-schedule-run-now-field .prompt-draft-toggle-thumb')).transform !== previous,
    uncheckedTransform,
  );
  const checkedTransform = await thumb.evaluate(element => getComputedStyle(element).transform);
  assert.notEqual(checkedTransform, uncheckedTransform, 'Run Now toggle should visibly change when selected');
  await page.locator('#library-schedule-save').click();
  await page.waitForFunction(() => document.getElementById('library-schedule-status').textContent === 'Schedule saved and task queued.');

  assert.equal(submitted.libraryTaskName, 'unit-test-coverage');
  assert.deepEqual(submitted.repos, ['https://github.com/acme/run-now.git']);
});
