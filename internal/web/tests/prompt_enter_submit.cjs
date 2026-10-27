// Run through its Go wrapper, which boots the UI and points this suite at it:
// go test ./internal/web -run TestPromptEnterSubmitBrowserSuite
// Or against an already running harness:
// HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/prompt_enter_submit.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
//
// usesSoftwareKeyboard() in static/index.html, and the copy the /chat page
// carries in server.go, decide whether Enter runs the prompt or types a
// newline. Nothing in Go can observe that decision, because it reads the live
// user agent, UA-CH and the pointer media queries, so these tests press the
// real key in a headless browser. The devices that matter: a desktop, where
// Enter has to run the prompt; a phone, where Enter is the only newline a
// software keyboard can type and a Run button takes over; a touch-only
// desktop, which matches every pointer test a phone does while a hardware
// keyboard is still plugged in and therefore has to keep Enter-to-submit; and
// the Windows and ChromeOS tablets, which report the same token their laptops
// do on the user agent string branch and mobile:false on the UA-CH one, so
// both branches are exercised for them.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';

const ANDROID_UA = 'Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/130.0 Mobile Safari/537.36';
// Names no desktop platform and no mobile one either, so the pointer media
// queries are the only branch of usesSoftwareKeyboard() left to decide it.
const TABLET_UA = 'Mozilla/5.0 (Tablet; rv:130.0) Gecko/20100101 Firefox/130.0';
// Windows ships keyboard-less tablets and laptops under one token, and so does
// ChromeOS; a browser without UA-CH gives the page nothing else to go on.
const WINDOWS_UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:130.0) Gecko/20100101 Firefox/130.0';
// The browsers those two platforms actually ship with answer UA-CH, so these
// pair a Chromium user agent with the platform the client hint reports.
const WINDOWS_CHROME_UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36';
const CROS_CHROME_UA = 'Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36';
const REPOS = [{ full_name: 'octo/demo', name: 'demo', private: false, description: 'Example repository.' }];

let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

// openPage fixtures every API the prompt surfaces read and records the run
// requests, so a test can tell "Enter submitted" from "Enter typed a newline".
// coarsePointer forces the touch-only screen a desktop kiosk reports without
// touching the user agent, which is the case the pointer media queries alone
// would misread as a phone. mobileViewport overrides the phone-sized screen
// that a user agent otherwise implies, so a desktop-shaped device can still
// carry a custom user agent. uaClientHints: false takes navigator.userAgentData
// away, which is the only way to reach the user agent string branch of
// reportsDesktopPlatform() in Chromium: it answers UA-CH itself and always
// reports a desktop there, so Firefox and Safari are simulated this way.
// clientHintsPlatform keeps UA-CH present and answers it the way Chrome and
// Edge actually do on a named device: a Windows or ChromeOS tablet reports the
// platform its laptops report, alongside mobile:false.
// runDelayMs holds the run response open so a second tap lands while the first
// request is still in flight.
async function openPage(t, {
  path = '/', userAgent = '', mobileViewport, coarsePointer = false, uaClientHints = true,
  clientHintsPlatform = '', runDelayMs = 0,
} = {}) {
  const mobile = mobileViewport === undefined ? Boolean(userAgent) : mobileViewport;
  const page = await browser.newPage({
    viewport: mobile ? { width: 430, height: 932 } : { width: 1280, height: 900 },
    ...(userAgent ? { userAgent } : {}),
    ...(mobile ? { isMobile: true, hasTouch: true } : { hasTouch: coarsePointer }),
  });
  page.setDefaultTimeout(10000);
  t.after(() => page.close());
  const errors = [], runs = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  if (!uaClientHints) {
    await page.addInitScript(() => {
      Object.defineProperty(navigator, 'userAgentData', { value: undefined, configurable: true });
    });
  } else if (clientHintsPlatform) {
    await page.addInitScript(platform => {
      Object.defineProperty(navigator, 'userAgentData', {
        value: {
          brands: [], mobile: false, platform,
          getHighEntropyValues: () => Promise.resolve({ platform }),
          toJSON: () => ({ brands: [], mobile: false, platform }),
        },
        configurable: true,
      });
    }, clientHintsPlatform);
  }
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
      const fulfill = () => route.fulfill({ json: { ok: true, request_id: 'req-enter-1' } });
      if (!runDelayMs) return fulfill();
      return new Promise(resolve => setTimeout(() => resolve(fulfill()), runDelayMs));
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
async function assertNoRun(page, runs, message = 'a software keyboard must keep Enter as a newline') {
  await page.waitForTimeout(250);
  assert.deepEqual(runs, [], message);
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

// Enter no longer starts the run on a phone, so the idle status has to name the
// control that does, the way the chat card's "Tap Run to start" does.
test('builder: the idle status names the key or button that starts the run', async t => {
  const desktop = await openPage(t);
  assert.match(await desktop.page.locator('#local-prompt-status').textContent(), /Press Enter to run\./);
  const phone = await openPage(t, { userAgent: ANDROID_UA });
  assert.match(await phone.page.locator('#local-prompt-status').textContent(), /Tap Run to start\./);
});

// A phone keeps Enter as a newline, so the Run button is the only way left to
// start a builder run: press it on the multi-line prompt Enter just typed.
test('phone: the builder Run button starts the prompt Enter would not', async t => {
  const { page, runs } = await openPage(t, { userAgent: ANDROID_UA });
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('First line');
  await page.locator('#builder-prompt-input').press('Enter');
  await page.keyboard.type('Second line');
  await assertNoRun(page, runs);
  await page.locator('#local-prompt-submit').click();
  await waitForRuns(runs, 1);
  assert.match(runs[0], /First line\\nSecond line/);
});

// The prompt textarea is not the only field in the run form: a form with a
// submit button also runs from Enter in any of its single-line inputs, which a
// phone keyboard reaches with the same key it uses for a newline.
test('phone: Enter in the repository field never runs the builder prompt', async t => {
  const { page, runs } = await openPage(t, { userAgent: ANDROID_UA });
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('Ship the fix');
  await page.locator('#builder-repo-input').press('Enter');
  await assertNoRun(page, runs, 'a software keyboard must not submit the form from a single-line field');
});

test('desktop: Enter in the repository field still runs the builder prompt', async t => {
  const { page, runs } = await openPage(t);
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('Ship the fix');
  await page.locator('#builder-repo-input').press('Enter');
  await waitForRuns(runs, 1);
  assert.match(runs[0], /Ship the fix/);
});

// Whether Enter in a select reaches implicit submission is the browser's call
// and not the page's: Blink submits from one on Windows, Gecko and WebKit
// never do, and Blink on macOS hands the key to the popup instead. So the
// select pair pins the only half this page owns - the guard cancels Enter on a
// software keyboard and keeps its hands off it on a hardware one - by watching
// the key event rather than a run request that only some engines would send.
// The listener sits on window so it runs after the form guard has had its say.
async function pressEnterAndReportBlocked(page, selector) {
  await page.evaluate(() => {
    window.__enterBlocked = null;
    window.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && window.__enterBlocked === null) {
        window.__enterBlocked = event.defaultPrevented;
      }
    });
  });
  await page.locator(selector).press('Enter');
  return page.evaluate(() => window.__enterBlocked);
}

test('phone: Enter in the reviewer select never runs the builder prompt', async t => {
  const { page, runs } = await openPage(t, { userAgent: ANDROID_UA });
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('Ship the fix');
  assert.equal(
    await pressEnterAndReportBlocked(page, '#builder-reviewer-select'),
    true,
    'a software keyboard must have Enter cancelled before a select can submit the form',
  );
  await assertNoRun(page, runs, 'a software keyboard must not submit the form from a select');
});

test('desktop: Enter in the reviewer select is left to the browser', async t => {
  const { page } = await openPage(t);
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('Ship the fix');
  assert.equal(
    await pressEnterAndReportBlocked(page, '#builder-reviewer-select'),
    false,
    'a hardware keyboard must keep Enter in a select, whatever the engine then does with it',
  );
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

// The kiosk above is settled by UA-CH, so the pointer queries are still the
// last word for anything UA-CH cannot answer: a tablet on a browser without it
// whose user agent names no platform at all reaches them and nothing else.
test('tablet without UA-CH: a coarse pointer that cannot hover keeps Enter as a newline', async t => {
  const { page, runs } = await openPage(t, { userAgent: TABLET_UA, coarsePointer: true, uaClientHints: false });
  assert.equal(
    await page.evaluate(() => navigator.userAgentData === undefined),
    true,
    'the user agent string branch only runs when UA-CH is absent',
  );
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('First line');
  await page.locator('#builder-prompt-input').press('Enter');
  await page.keyboard.type('Second line');
  assert.equal(await page.locator('#builder-prompt-input').inputValue(), 'First line\nSecond line');
  await assertNoRun(page, runs);
});

// A Windows or ChromeOS tablet reports the token its laptops report, so the
// token cannot decide alone: with no hardware pointer attached it is a tablet,
// and Enter is the only newline it can type.
test('Windows tablet without UA-CH: Enter stays a newline in the builder prompt', async t => {
  const { page, runs } = await openPage(t, {
    userAgent: WINDOWS_UA, mobileViewport: false, coarsePointer: true, uaClientHints: false,
  });
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('First line');
  await page.locator('#builder-prompt-input').press('Enter');
  await page.keyboard.type('Second line');
  assert.equal(await page.locator('#builder-prompt-input').inputValue(), 'First line\nSecond line');
  await assertNoRun(page, runs, 'a keyboard-less Windows tablet must keep Enter as a newline');
});

test('Windows laptop without UA-CH: Enter still runs the builder prompt', async t => {
  const { page, runs } = await openPage(t, { userAgent: WINDOWS_UA, mobileViewport: false, uaClientHints: false });
  await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
  await page.locator('#builder-prompt-input').fill('Ship the fix');
  await page.locator('#builder-prompt-input').press('Enter');
  await waitForRuns(runs, 1);
  assert.match(runs[0], /Ship the fix/);
});

// Chrome and Edge are the browsers these tablets actually ship with, and they
// answer UA-CH rather than leaving it absent: the platform is the laptop one
// and mobile is false, so taking UA-CH at its word would submit on Enter with
// no keyboard to type Shift+Enter on. The pointer has to break the tie here
// exactly as it does on the user agent string branch above.
for (const [device, platform, deviceUA] of [
  ['Windows', 'Windows', WINDOWS_CHROME_UA],
  ['ChromeOS', 'Chrome OS', CROS_CHROME_UA],
]) {
  test(`${device} tablet with UA-CH: Enter stays a newline in the builder prompt`, async t => {
    const { page, runs } = await openPage(t, {
      userAgent: deviceUA, mobileViewport: false, coarsePointer: true, clientHintsPlatform: platform,
    });
    assert.deepEqual(
      await page.evaluate(() => [navigator.userAgentData.platform, navigator.userAgentData.mobile]),
      [platform, false],
      'the tablet has to reach the page as a non-mobile laptop platform',
    );
    await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
    await page.locator('#builder-prompt-input').fill('First line');
    await page.locator('#builder-prompt-input').press('Enter');
    await page.keyboard.type('Second line');
    assert.equal(await page.locator('#builder-prompt-input').inputValue(), 'First line\nSecond line');
    await assertNoRun(page, runs, `a keyboard-less ${device} tablet must keep Enter as a newline`);
  });

  test(`${device} laptop with UA-CH: Enter still runs the builder prompt`, async t => {
    const { page, runs } = await openPage(t, {
      userAgent: deviceUA, mobileViewport: false, clientHintsPlatform: platform,
    });
    await page.locator('#builder-repo-input').fill('git@github.com:octo/demo.git');
    await page.locator('#builder-prompt-input').fill('Ship the fix');
    await page.locator('#builder-prompt-input').press('Enter');
    await waitForRuns(runs, 1);
    assert.match(runs[0], /Ship the fix/);
  });
}

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
    // The phone stylesheet stretches labelled prompt actions across the row; an
    // icon-only Run button that joined in would eat the status text beside it.
    assert.equal(
      await run.evaluate(node => getComputedStyle(node).flexGrow),
      '0',
      'the icon-only Run button must keep its square footprint',
    );
    await prompt.fill('First line');
    await prompt.press('Enter');
    await page.keyboard.type('Second line');
    assert.equal(await prompt.inputValue(), 'First line\nSecond line');
    await assertNoRun(page, runs);
    await run.click();
    await waitForRuns(runs, 1);
    assert.match(runs[0], /First line\\nSecond line/);
  });

  // A tap on a phone double-fires easily, and the run request stays open long
  // enough here that the second tap lands while the first is still in flight.
  test(`${path}: a phone double tap on Run queues a single request`, async t => {
    const { page, runs } = await openPage(t, { path, userAgent: ANDROID_UA, runDelayMs: 750 });
    const prompt = await openRepoCard(page);
    const run = page.locator('.chat-repo-submit:visible').first();
    await run.waitFor();
    await prompt.fill('Run this once');
    await run.dblclick();
    await waitForRuns(runs, 1);
    assert.equal(await run.isDisabled(), true, 'the run in flight must disable the Run button');
    await page.waitForTimeout(250);
    assert.equal(runs.length, 1, 'the run already in flight must swallow the second tap');
  });
}
