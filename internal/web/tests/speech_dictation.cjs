// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/speech_dictation.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
//
// speechCaptureSupport/speechCaptureDenial/speechToggleAvailability in
// static/index.html are unexported closures with no Go-reachable equivalent, so
// these tests drive the real mic button in a headless browser. They cover both
// ways a browser refuses microphone capture: an insecure origin, where
// navigator.mediaDevices is absent before any script runs, and iOS Firefox,
// which exposes getUserMedia and only rejects when it is called. Both must keep
// the button usable and hand off to the platform dictation key.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

// openPage stubs /api/speech/status so the whisper sidecar does not have to run.
// stripCapture removes navigator.mediaDevices the way an insecure origin does;
// rejectCapture keeps getUserMedia in place but rejects with the named
// DOMException, which is how iOS browsers other than Safari actually refuse.
async function openPage(t, { speech = { enabled: true, reachable: true }, stripCapture = false, rejectCapture = '', insecure = false, userAgent = '', clock = false } = {}) {
  const page = await browser.newPage(userAgent ? { userAgent } : {});
  if (clock) await page.clock.install();
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
  if (rejectCapture) {
    await page.addInitScript(name => {
      const getUserMedia = () => Promise.reject(new DOMException('Permission denied', name));
      Object.defineProperty(navigator, 'mediaDevices', {
        value: { getUserMedia, enumerateDevices: () => Promise.resolve([]) },
        configurable: true,
      });
    }, rejectCapture);
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
  assert.match(await micButton(page).getAttribute('title'), /HTTPS/);

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  assert.equal(await page.evaluate(() => document.activeElement?.id), 'builder-prompt-input', 'the prompt is focused for keyboard dictation');
});

// iOS Firefox exposes getUserMedia, so nothing is detectable before the tap;
// the refusal arrives as a rejected promise and must still reach the fallback.
test('Firefox on iOS falls back to keyboard dictation when getUserMedia is refused', async t => {
  const iosFirefoxUA = 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/127.0 Mobile/15E148 Safari/605.1.15';
  const page = await openPage(t, { rejectCapture: 'NotAllowedError', userAgent: iosFirefoxUA });
  await micButton(page).waitFor({ state: 'visible' });
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), false, 'capture looks available until getUserMedia is called');

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  const status = await page.textContent('#local-prompt-status');
  assert.match(status, /Safari/, 'the iOS refusal points at Safari for whisper');
  assert.doesNotMatch(status, /Dictation failed/, 'the refusal is not reported as a generic failure');
  assert.equal(await page.evaluate(() => document.activeElement?.id), 'builder-prompt-input');
});

test('a missing microphone falls back to keyboard dictation instead of failing', async t => {
  const page = await openPage(t, { rejectCapture: 'NotFoundError' });
  await micButton(page).waitFor({ state: 'visible' });

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  assert.match(await page.textContent('#local-prompt-status'), /No microphone was available/);
});

// A dropped status poll must not retract the fallback: native keyboard
// dictation needs no server, and the mic button is the only affordance for it.
test('a failed speech status poll keeps the keyboard fallback button', async t => {
  const page = await openPage(t, { stripCapture: true, insecure: true, clock: true });
  await micButton(page).waitFor({ state: 'visible' });
  const whisperIndicator = page.locator('#speech-conn-item');
  await whisperIndicator.waitFor({ state: 'visible' });

  await page.route('**/api/speech/status', route => route.abort());
  await page.clock.runFor(11_000); // one status poll interval
  // The whisper indicator going dark is the first thing the failed poll does,
  // so it marks the point where the fallback decision has been re-made.
  for (let i = 0; i < 100 && await whisperIndicator.isVisible(); i += 1) {
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  assert.equal(await whisperIndicator.isVisible(), false, 'the failed status poll was processed');

  assert.equal(await micButton(page).isVisible(), true, 'the fallback survives a failed status poll');
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), true);
});
