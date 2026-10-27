// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/prompt_enter_submit.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
//
// usesSoftwareKeyboard() in static/index.html, and the copy the /chat page
// carries in server.go, decide whether Enter runs the prompt or types a
// newline. Nothing in Go can observe that decision, because it reads the live
// user agent, UA-CH and the pointer media queries, so these tests press the
// real key in a headless browser. Three devices matter: a desktop, where Enter
// has to run the prompt; a phone, where Enter is the only newline a software
// keyboard can type and a Run button takes over; and a touch-only desktop,
// which matches every pointer test a phone does while a hardware keyboard is
// still plugged in and therefore has to keep Enter-to-submit.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';

const ANDROID_UA = 'Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/130.0 Mobile Safari/537.36';
const REPOS = [{ full_name: 'octo/demo', name: 'demo', private: false, description: 'Example repository.' }];

let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

// openPage fixtures every API the prompt surfaces read and records the run
// requests, so a test can tell "Enter submitted" from "Enter typed a newline".
// coarsePointer forces the touch-only screen a desktop kiosk reports without
// touching the user agent, which is the case the pointer media queries alone
// would misread as a phone.
async function openPage(t, { path = '/', userAgent = '', coarsePointer = false } = {}) {
  const mobile = Boolean(userAgent);
  const page = await browser.newPage({
    viewport: mobile ? { width: 430, height: 932 } : { width: 1280, height: 900 },
    ...(mobile ? { userAgent, isMobile: true, hasTouch: true } : { hasTouch: coarsePointer }),
  });
  page.setDefaultTimeout(10000);
  t.after(() => page.close());
  const errors = [], runs = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  if (coarsePointer) {
    await page.addInitScript(() => {
      const real = window.matchMedia.bind(window);
      const forced = query => ({
        matches: true, media: query, onchange: null,
        addEventListener() {}, removeEventListener() {},
        addListener() {}, removeListener() {}, dispatchEvent() { return false; },
      });
      window.matchMedia = query =>
        query === '(any-pointer: coarse)' || query === '(any-hover: none)' ? forced(query) : real(query);
    });
  }
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (url.pathname === '/api/local-prompt' && route.request().method() === 'POST') {
      runs.push(route.request().postData() || '');
      return route.fulfill({ json: { ok: true, request_id: 'req-enter-1' } });
    }
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    let json = { ok: true };
    if (url.pathname === '/api/status') json = { ok: true, tasks: [], events: [], repositories: [], stats: {}, resources: {} };
    if (url.pathname === '/api/agent-auth') json.auth = { required: false, ready: true, harness: 'codex', state: 'ready' };
    if (url.pathname === '/api/review-settings') json.settings = { review_level: 'off', chat_enabled: true, default_library_enabled: true };
    if (url.pathname === '/api/github/repos') json.repos = REPOS;
    if (url.pathname === '/api/speech/status') json.speech = { enabled: false, reachable: false };
    return route.fulfill({ json });
  });
  await page.goto(new URL(path, baseURL).toString());
  await page.waitForSelector(path.startsWith('/chat') ? '#chat-repo-grid' : '#builder-prompt-input', { state: 'attached' });
  return { page, runs };
}

// The run requests are recorded in Node, so waiting on them needs a Node-side
// poll rather than page.waitForFunction.
async function waitForRuns(runs, count) {
  const deadline = Date.now() + 10000;
  while (runs.length < count && Date.now() < deadline) {
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  assert.equal(runs.length, count, `expected ${count} run request(s), saw ${runs.length}`);
}

// Nothing may be submitted: give the page the same budget a real submit would
// have taken before declaring the prompt stayed put.
async function assertNoRun(page, runs) {
  await page.waitForTimeout(250);
  assert.deepEqual(runs, [], 'a software keyboard must keep Enter as a newline');
}

async function openRepoCard(page) {
  await page.locator('.chat-repo-card').first().waitFor();
  await page.locator('.chat-repo-card').first().click();
  const prompt = page.locator('.chat-repo-prompt:visible').first();
  await prompt.waitFor();
  return prompt;
}

test('desktop: Enter runs the builder prompt instead of typing a newline', async t => {
  const { page, runs } = await openPage(t);
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('Ship the fix');
  await page.locator('#builder-prompt-input').press('Enter');
  await waitForRuns(runs, 1);
  assert.match(runs[0], /Ship the fix/);
  assert.doesNotMatch(await page.locator('#builder-prompt-input').inputValue(), /\n/, 'Enter must not also type a newline');
});

test('phone: Enter types a newline in the builder prompt and never runs it', async t => {
  const { page, runs } = await openPage(t, { userAgent: ANDROID_UA });
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('First line');
  await page.locator('#builder-prompt-input').press('Enter');
  await page.keyboard.type('Second line');
  assert.equal(await page.locator('#builder-prompt-input').inputValue(), 'First line\nSecond line');
  await assertNoRun(page, runs);
  const status = page.locator('#local-prompt-status');
  assert.equal(await status.textContent(), await status.getAttribute('data-default-text'), 'nothing ran, so the status stays at its default');
});

// A kiosk or touch laptop reports a coarse pointer that cannot hover, exactly
// like a phone, but still has a real keyboard attached.
test('touch-only desktop: Enter still runs the builder prompt', async t => {
  const { page, runs } = await openPage(t, { coarsePointer: true });
  assert.equal(
    await page.evaluate(() => window.matchMedia('(any-pointer: coarse)').matches && window.matchMedia('(any-hover: none)').matches),
    true,
    'the pointer queries alone would read as a phone',
  );
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('Run from the kiosk');
  await page.locator('#builder-prompt-input').press('Enter');
  await waitForRuns(runs, 1);
});

for (const path of ['/#chat', '/chat']) {
  test(`${path}: a desktop runs a repository prompt on Enter and needs no Run button`, async t => {
    const { page, runs } = await openPage(t, { path });
    const prompt = await openRepoCard(page);
    assert.equal(await page.locator('.chat-repo-submit').first().isVisible(), false, 'a hardware keyboard needs no Run button');
    assert.match(await page.locator('.chat-repo-submit-status').first().textContent(), /Press Enter to run/);
    await prompt.fill('Review the default branch');
    await prompt.press('Enter');
    await waitForRuns(runs, 1);
    assert.match(runs[0], /Review the default branch/);
  });

  test(`${path}: a phone keeps Enter as a newline and runs from the Run button`, async t => {
    const { page, runs } = await openPage(t, { path, userAgent: ANDROID_UA });
    const prompt = await openRepoCard(page);
    const run = page.locator('.chat-repo-submit:visible').first();
    await run.waitFor();
    assert.match(await page.locator('.chat-repo-submit-status').first().textContent(), /Tap Run to start/);
    await prompt.fill('First line');
    await prompt.press('Enter');
    await page.keyboard.type('Second line');
    assert.equal(await prompt.inputValue(), 'First line\nSecond line');
    await assertNoRun(page, runs);
    await run.click();
    await waitForRuns(runs, 1);
    assert.match(runs[0], /First line\\nSecond line/);
  });
}
