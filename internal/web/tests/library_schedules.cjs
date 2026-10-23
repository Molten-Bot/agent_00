// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/library_schedules.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
//
// hoursForIndex/indexForHours in static/library-schedules.js are unexported
// closures with no Go-reachable equivalent, so these tests drive the real
// slider and editor in a headless browser to cover the single-digit -> 4h/6h/12h+
// widening snap table and the reverse mapping used when reopening a schedule.
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

test('slider snaps to single-digit hours, then widening 4h/6h/12h+ steps as it moves right', async t => {
  const page = await openPage(t);
  const samples = await page.evaluate(() => {
    const slider = document.getElementById('library-schedule-hours');
    const frequency = document.getElementById('library-schedule-frequency');
    const read = index => {
      slider.value = String(index);
      slider.dispatchEvent(new Event('input'));
      return frequency.textContent;
    };
    return { singleDigit: read(2), fourHour: read(3), twelveHour: read(9), max: read(Number(slider.max)) };
  });
  assert.equal(samples.singleDigit, 'every 3 hours');
  assert.equal(samples.fourHour, 'every 4 hours');
  assert.equal(samples.twelveHour, 'every 12 hours');
  assert.equal(samples.max, 'every 168 hours');
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
  assert.equal(frequencyText, 'every 9 hours', `expected the closest step below 10 (9) to be selected, got ${frequencyText}`);
});
