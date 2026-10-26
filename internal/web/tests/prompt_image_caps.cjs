// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/prompt_image_caps.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
//
// pickedImageFiles/clipboardPngFiles in static/index.html are unexported
// closures with no Go-reachable equivalent, so these tests drive the real
// click-to-pick file input and a simulated clipboard paste in a headless
// browser to cover the MAX_PROMPT_IMAGE_COUNT/MAX_PROMPT_IMAGE_BYTES caps and
// the user-facing "Skipped N screenshot(s)" message for both entry points.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

async function openPage(t) {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.goto(baseURL);
  await page.waitForSelector('#builder-image-file-input', { state: 'attached' });
  return page;
}

function pngFiles(count, { prefix = 'shot', sizeBytes = 16 } = {}) {
  return Array.from({ length: count }, (_, i) => ({
    name: `${prefix}-${i + 1}.png`,
    mimeType: 'image/png',
    buffer: Buffer.alloc(sizeBytes),
  }));
}

async function attachedNames(page) {
  const text = await page.locator('#builder-image-paste-target').textContent();
  return text === 'Paste or click to add screenshots.' ? [] : text.split(' | ');
}

test('click-to-pick caps total attached screenshots at the configured limit', async t => {
  const page = await openPage(t);
  await page.locator('#builder-image-file-input').setInputFiles(pngFiles(14));
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'Skipped 2 screenshot');
  assert.equal((await attachedNames(page)).length, 12, 'only MAX_PROMPT_IMAGE_COUNT files are attached');
});

test('click-to-pick caps remaining room against already-attached screenshots', async t => {
  const page = await openPage(t);
  await page.locator('#builder-image-file-input').setInputFiles(pngFiles(5, { prefix: 'first' }));
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'Attached 5 screenshot');
  assert.equal((await attachedNames(page)).length, 5);

  await page.locator('#builder-image-file-input').setInputFiles(pngFiles(10, { prefix: 'second' }));
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'Skipped 3 screenshot');
  assert.equal((await attachedNames(page)).length, 12, 'room for only 7 more files was available');
});

test('click-to-pick drops oversized files and reports the skip', async t => {
  const page = await openPage(t);
  const files = pngFiles(2).concat(pngFiles(1, { prefix: 'huge', sizeBytes: 8 * 1024 * 1024 + 1 }));
  await page.locator('#builder-image-file-input').setInputFiles(files);
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'Skipped 1 screenshot');
  const names = await attachedNames(page);
  assert.equal(names.length, 2, 'the oversized file is dropped');
  assert.ok(!names.some(name => name.startsWith('huge')), 'oversized file name is not attached');
});

test('pasting screenshots enforces the same count/size caps as the picker', async t => {
  const page = await openPage(t);
  await page.locator('#builder-image-file-input').setInputFiles(pngFiles(11, { prefix: 'first' }));
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'Attached 11 screenshot');

  await page.evaluate(() => {
    const dt = new DataTransfer();
    const small = new File([new Uint8Array(16)], 'pasted-small.png', { type: 'image/png' });
    const huge = new File([new Uint8Array(8 * 1024 * 1024 + 1)], 'pasted-huge.png', { type: 'image/png' });
    dt.items.add(small);
    dt.items.add(huge);
    const target = document.getElementById('builder-image-paste-target');
    target.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  });

  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'Skipped 1 screenshot');
  const names = await attachedNames(page);
  assert.equal(names.length, 12, 'paste respects the remaining room (1 slot) and the per-file size cap');
  assert.ok(names.includes('pasted-small.png'), 'the in-budget pasted file is attached');
  assert.ok(!names.includes('pasted-huge.png'), 'the oversized pasted file is dropped like the picker drops it');
});
