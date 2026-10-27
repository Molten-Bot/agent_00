// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/speech_dictation.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
//
// speechCaptureSupport/speechToggleAvailability in static/index.html are
// unexported closures with no Go-reachable equivalent, so these tests drive the
// real mic button in a headless browser. They cover the two ways a browser can
// refuse microphone capture - an insecure origin and an iOS browser other than
// Safari (Firefox on iOS reports FxiOS and gets no getUserMedia) - and confirm
// the button stays visible and hands off to the platform dictation key instead
// of silently disappearing.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

// openPage stubs /api/speech/status so the whisper sidecar does not have to run,
// and optionally strips navigator.mediaDevices the way a non-secure origin or an
// iOS WKWebView browser does before any page script observes it.
async function openPage(t, { speech = { enabled: true, reachable: true }, stripCapture = false, insecure = false, userAgent = '' } = {}) {
  const page = await browser.newPage(userAgent ? { userAgent } : {});
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  // Registered last so it wins over the catch-all: Playwright matches routes in
  // reverse registration order. The whisper sidecar does not run under test.
  await page.route('**/api/speech/status', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ ok: true, speech }),
  }));
  if (stripCapture) {
    await page.addInitScript(() => {
      Object.defineProperty(navigator, 'mediaDevices', { value: undefined, configurable: true });
    });
  }
  if (insecure) {
    await page.addInitScript(() => {
      Object.defineProperty(window, 'isSecureContext', { value: false, configurable: true });
    });
  }
  await page.goto(baseURL);
  await page.waitForSelector('#builder-prompt-input', { state: 'attached' });
  return page;
}

function micButton(page) {
  return page.locator('#prompt-speech-toggle');
}

test('a reachable whisper sidecar shows the recording mic button', async t => {
  const page = await openPage(t);
  await micButton(page).waitFor({ state: 'visible' });
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), false);
  assert.equal(await micButton(page).getAttribute('aria-label'), 'Dictate prompt');
});

test('an unreachable whisper sidecar hides the mic button', async t => {
  const page = await openPage(t, { speech: { enabled: true, reachable: false } });
  await micButton(page).waitFor({ state: 'hidden' });
});

test('disabled dictation hides the mic button even without microphone capture', async t => {
  const page = await openPage(t, { speech: { enabled: false, reachable: false }, stripCapture: true });
  await micButton(page).waitFor({ state: 'hidden' });
});

test('an insecure origin keeps the mic button and explains the HTTPS requirement', async t => {
  const page = await openPage(t, { stripCapture: true, insecure: true });
  await micButton(page).waitFor({ state: 'visible' });
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), true);
  assert.equal(await micButton(page).isEnabled(), true, 'the fallback button stays clickable');
  assert.match(await micButton(page).getAttribute('title'), /HTTPS or localhost/);

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  assert.equal(await page.evaluate(() => document.activeElement?.id), 'builder-prompt-input', 'the prompt is focused for keyboard dictation');
});

test('Firefox on iOS keeps the mic button and points at Safari plus keyboard dictation', async t => {
  const iosFirefoxUA = 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/127.0 Mobile/15E148 Safari/605.1.15';
  const page = await openPage(t, { stripCapture: true, userAgent: iosFirefoxUA });
  await micButton(page).waitFor({ state: 'visible' });
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), true);
  assert.match(await micButton(page).getAttribute('title'), /Safari/);

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  assert.equal(await page.evaluate(() => document.activeElement?.id), 'builder-prompt-input');
});
