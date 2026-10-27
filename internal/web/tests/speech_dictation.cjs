// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/speech_dictation.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
//
// speechCaptureSupport/speechCaptureDenial/speechToggleAvailability in
// static/index.html, and the second copy of them the /chat page carries in
// server.go, are unexported closures with no Go-reachable equivalent, so these
// tests drive the real mic button in a headless browser. They cover both ways a
// browser refuses microphone capture: an insecure origin, where
// navigator.mediaDevices is absent before any script runs, and iOS Firefox,
// which exposes getUserMedia and only rejects when it is called. Both must keep
// the button usable and hand off to the platform dictation key, and the handoff
// has to focus the prompt inside the tap gesture, because that is the only time
// iOS will raise the keyboard.
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
// gateCapture holds that rejection open until window.__releaseMic() runs, which
// is how a test can inspect the page while the permission prompt is still
// pending. window.__micCalls counts getUserMedia calls so a remembered refusal
// can be shown to stop re-asking.
async function openPage(t, { speech = { enabled: true, reachable: true }, blockStatus = false, stripCapture = false, rejectCapture = '', gateCapture = false, insecure = false, userAgent = '', clock = false, path = '/', repos = null } = {}) {
  const page = await browser.newPage(userAgent ? { userAgent } : {});
  if (clock) await page.clock.install();
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  // Registered last so it wins over the catch-all: Playwright matches routes in
  // reverse registration order. The whisper sidecar does not run under test.
  await page.route('**/api/speech/status', route => blockStatus ? route.abort() : route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ ok: true, speech }),
  }));
  if (repos) {
    await page.route('**/api/github/repos', route => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ ok: true, repos }),
    }));
  }
  if (stripCapture) {
    await page.addInitScript(() => {
      Object.defineProperty(navigator, 'mediaDevices', { value: undefined, configurable: true });
    });
  }
  if (rejectCapture) {
    await page.addInitScript(({ name, gated }) => {
      let release = () => {};
      const gate = new Promise(resolve => { release = resolve; });
      window.__micCalls = 0;
      window.__releaseMic = () => release();
      const refuse = () => { throw new DOMException('Permission denied', name); };
      const getUserMedia = () => {
        window.__micCalls += 1;
        return gated ? gate.then(refuse) : Promise.reject(new DOMException('Permission denied', name));
      };
      Object.defineProperty(navigator, 'mediaDevices', {
        value: { getUserMedia, enumerateDevices: () => Promise.resolve([]) },
        configurable: true,
      });
    }, { name: rejectCapture, gated: Boolean(gateCapture) });
  }
  if (insecure) {
    await page.addInitScript(() => {
      Object.defineProperty(window, 'isSecureContext', { value: false, configurable: true });
    });
  }
  await page.goto(new URL(path, baseURL).toString());
  await page.waitForSelector(path === '/chat' ? '#chat-repo-grid' : '#builder-prompt-input', { state: 'attached' });
  return page;
}

const IOS_FIREFOX_UA = 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/127.0 Mobile/15E148 Safari/605.1.15';

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
  const page = await openPage(t, { rejectCapture: 'NotAllowedError', userAgent: IOS_FIREFOX_UA });
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

// Even the first status poll can fail, leaving no remembered "enabled" flag to
// preserve. A browser that cannot record at all still needs the keyboard
// dictation affordance, so it must not wait for a poll that may never land.
test('a fallback button appears when the first speech status poll never lands', async t => {
  const page = await openPage(t, { blockStatus: true, stripCapture: true, insecure: true });
  await micButton(page).waitFor({ state: 'visible' });
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), true);
  assert.equal(await micButton(page).isEnabled(), true, 'the fallback button stays clickable');
});

// iOS only raises the keyboard for a focus() that runs inside the tap gesture.
// Focusing after the awaited getUserMedia rejection is too late, so the prompt
// must already hold focus while the permission request is still pending.
test('the prompt takes focus inside the tap, before getUserMedia settles', async t => {
  const page = await openPage(t, { rejectCapture: 'NotAllowedError', gateCapture: true, userAgent: IOS_FIREFOX_UA });
  await micButton(page).waitFor({ state: 'visible' });
  await page.evaluate(() => document.getElementById('builder-prompt-input').blur());

  await micButton(page).click();
  await page.waitForFunction(() => window.__micCalls > 0, null, { timeout: 5000 });
  assert.equal(
    await page.evaluate(() => document.activeElement?.id),
    'builder-prompt-input',
    'the prompt is focused while the microphone request is still pending',
  );

  await page.evaluate(() => window.__releaseMic());
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
});

// A denied microphone stays denied until the browser setting changes, so the
// button has to advertise keyboard dictation and stop re-asking.
test('a denied microphone leaves the button in its keyboard dictation state', async t => {
  const page = await openPage(t, { rejectCapture: 'NotAllowedError', userAgent: IOS_FIREFOX_UA });
  await micButton(page).waitFor({ state: 'visible' });

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), true, 'the refusal flips the button to keyboard dictation');
  assert.equal(await page.evaluate(() => window.__micCalls), 1);

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  assert.equal(await page.evaluate(() => window.__micCalls), 1, 'the second tap does not re-trigger the denied getUserMedia');
});

// The /chat page carries its own copy of the dictation code, so it needs the
// same coverage: an in-gesture focus, a readable fallback message, and a button
// that stops re-asking once the microphone is refused.
test('the chat page hands a refused microphone to keyboard dictation', async t => {
  const page = await openPage(t, {
    path: '/chat',
    rejectCapture: 'NotAllowedError',
    gateCapture: true,
    userAgent: IOS_FIREFOX_UA,
    repos: [{ full_name: 'octo/demo', name: 'demo', private: false, description: 'Demo repository' }],
  });
  const chatMic = page.locator('#chat-speech-toggle');
  await page.locator('.chat-repo-card').first().click();
  await chatMic.waitFor({ state: 'visible' });
  await page.evaluate(() => document.activeElement?.blur());

  await chatMic.click();
  await page.waitForFunction(() => window.__micCalls > 0, null, { timeout: 5000 });
  assert.equal(
    await page.evaluate(() => document.activeElement?.classList.contains('chat-repo-prompt')),
    true,
    'the chat prompt is focused while the microphone request is still pending',
  );

  await page.evaluate(() => window.__releaseMic());
  await page.waitForFunction(text => document.getElementById('chat-status').textContent.includes(text), 'dictation key');
  assert.match(await page.textContent('#chat-status'), /Safari/);
  assert.equal(await chatMic.evaluate(node => node.classList.contains('speech-native')), true);

  await chatMic.click();
  assert.equal(await page.evaluate(() => window.__micCalls), 1, 'the second tap does not re-trigger the denied getUserMedia');
});

// The fallback message names the key to press, so it must wrap instead of being
// ellipsized away by the single-line status styling.
test('the keyboard dictation message is not clipped to one line', async t => {
  const page = await openPage(t, { stripCapture: true, insecure: true });
  await micButton(page).waitFor({ state: 'visible' });
  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');

  const clipped = await page.evaluate(() => {
    const node = document.getElementById('local-prompt-status');
    const style = window.getComputedStyle(node);
    return { whiteSpace: style.whiteSpace, overflowing: node.scrollWidth > node.clientWidth + 1 };
  });
  assert.equal(clipped.whiteSpace, 'normal', 'the status wraps instead of staying on one line');
  assert.equal(clipped.overflowing, false, 'the whole message fits inside the status element');
});
