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
// grantCapture hands back a real but silent MediaStream, so the recording path
// can run end to end in a headless browser that has no microphone.
async function openPage(t, { speech = { enabled: true, reachable: true }, blockStatus = false, stripCapture = false, rejectCapture = '', gateCapture = false, grantCapture = false, insecure = false, userAgent = '', clock = false, path = '/', repos = null } = {}) {
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
    await page.route('**/api/github/repos**', route => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ ok: true, repos }),
    }));
    // The repository cards on the main app only render once the dashboard
    // snapshot lands and the chat feature is switched on, neither of which the
    // bare server under test reports.
    await page.route('**/api/status', route => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ ok: true, tasks: [], events: [], repositories: [], stats: {}, resources: {} }),
    }));
    await page.route('**/api/review-settings', route => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ ok: true, settings: { review_level: 'off', chat_enabled: true, default_library_enabled: true } }),
    }));
    await page.route('**/api/stream', route => route.fulfill({ contentType: 'text/event-stream', body: '' }));
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
  if (grantCapture) {
    await page.addInitScript(() => {
      window.__micCalls = 0;
      const getUserMedia = () => {
        window.__micCalls += 1;
        const Ctor = window.AudioContext || window.webkitAudioContext;
        return Promise.resolve(new Ctor().createMediaStreamDestination().stream);
      };
      Object.defineProperty(navigator, 'mediaDevices', {
        value: { getUserMedia, enumerateDevices: () => Promise.resolve([]) },
        configurable: true,
      });
    });
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

// Node-side polling: page.waitForFunction polls inside the page, which a fake
// clock can stall, so tests that install one wait from out here instead.
async function waitUntil(check, message) {
  for (let i = 0; i < 200; i += 1) {
    if (await check()) return;
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  assert.fail(message);
}

test('a reachable whisper sidecar shows the recording mic button', async t => {
  const page = await openPage(t);
  await micButton(page).waitFor({ state: 'visible' });
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), false);
  assert.equal(await micButton(page).getAttribute('aria-label'), 'Dictate prompt');
});

// Capture works here, so hiding the button would remove the only dictation
// affordance on the device over a sidecar the platform dictation key never
// needs. The software keyboard carries that key, so this is a mobile fallback.
test('an unreachable whisper sidecar keeps the mic button as a keyboard fallback', async t => {
  const page = await openPage(t, { speech: { enabled: true, reachable: false }, userAgent: IOS_FIREFOX_UA });
  await micButton(page).waitFor({ state: 'visible' });
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), true);
  assert.equal(await micButton(page).isEnabled(), true, 'the fallback button stays clickable');
  assert.match(await micButton(page).getAttribute('title'), /not reachable/);

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  assert.match(await page.textContent('#local-prompt-status'), /not reachable/);
  assert.equal(await page.evaluate(() => document.activeElement?.id), 'builder-prompt-input', 'the prompt is focused for keyboard dictation');
});

// The whisper host defaults to a Compose service name, so an unreachable
// sidecar is the normal state of every deployment without it. A desktop
// keyboard has no dictation key to hand off to, so the button stays hidden
// there instead of advertising a key that does not exist.
test('an unreachable whisper sidecar hides the mic button on desktop', async t => {
  const page = await openPage(t, { speech: { enabled: true, reachable: false } });
  await micButton(page).waitFor({ state: 'hidden' });
});

// The /chat page carries its own availability copy, so an unreachable sidecar
// has to leave the same fallback there, and hide the button on desktop too.
test('the chat mic stays usable when the whisper sidecar is unreachable', async t => {
  const page = await openPage(t, {
    path: '/chat',
    speech: { enabled: true, reachable: false },
    userAgent: IOS_FIREFOX_UA,
    repos: [{ full_name: 'octo/demo', name: 'demo', private: false, description: 'Demo repository' }],
  });
  const chatMic = page.locator('#chat-speech-toggle');
  await page.locator('.chat-repo-card').first().click();
  await chatMic.waitFor({ state: 'visible' });
  assert.equal(await chatMic.evaluate(node => node.classList.contains('speech-native')), true);

  await chatMic.click();
  // The hint belongs next to the prompt it talks about: the page-top status is
  // the repository counter and the next grid render overwrites it.
  await page.waitForFunction(text => document.querySelector('.chat-repo-submit-status')?.textContent.includes(text), 'not reachable');
  assert.equal(
    await page.evaluate(() => document.activeElement?.classList.contains('chat-repo-prompt')),
    true,
    'the chat prompt is focused for keyboard dictation',
  );
});

test('the chat mic is hidden on desktop when the whisper sidecar is unreachable', async t => {
  const page = await openPage(t, {
    path: '/chat',
    speech: { enabled: true, reachable: false },
    repos: [{ full_name: 'octo/demo', name: 'demo', private: false, description: 'Demo repository' }],
  });
  await page.locator('.chat-repo-card').first().click();
  await page.locator('#chat-speech-toggle').waitFor({ state: 'hidden' });
});

// A repository grid re-render rewrites the page-top status, so a dictation hint
// parked there disappears while the user is still reading it.
test('a chat dictation hint survives a repository grid re-render', async t => {
  const page = await openPage(t, {
    path: '/chat',
    stripCapture: true,
    insecure: true,
    repos: [
      { full_name: 'octo/demo', name: 'demo', private: false, description: 'Demo repository' },
      { full_name: 'octo/other', name: 'other', private: false, description: 'Other repository' },
    ],
  });
  const chatMic = page.locator('#chat-speech-toggle');
  await page.locator('.chat-repo-card').first().click();
  await chatMic.waitFor({ state: 'visible' });

  await chatMic.click();
  const cardStatus = page.locator('.chat-repo-card').first().locator('.chat-repo-submit-status');
  await page.waitForFunction(text => document.querySelector('.chat-repo-submit-status')?.textContent.includes(text), 'dictation key');
  await page.fill('#chat-repo-search', 'demo'); // re-renders the grid and the page-top status
  await page.waitForFunction(() => document.getElementById('chat-status').textContent.includes('repositor'));
  assert.match(await cardStatus.textContent(), /dictation key/, 'the hint outlives the grid render');
});

// The hint promises the prompt is focused. A prompt that cannot take focus
// makes that promise false, so the chat copy has to check, not assume.
test('the chat fallback does not claim a focus it could not take', async t => {
  const page = await openPage(t, {
    path: '/chat',
    stripCapture: true,
    insecure: true,
    repos: [{ full_name: 'octo/demo', name: 'demo', private: false, description: 'Demo repository' }],
  });
  const chatMic = page.locator('#chat-speech-toggle');
  await page.locator('.chat-repo-card').first().click();
  await chatMic.waitFor({ state: 'visible' });
  // A disabled textarea is still the open card's prompt, but focus() on it is a
  // no-op, the same way a detached one is.
  await page.evaluate(() => { document.querySelector('.chat-repo-prompt').disabled = true; });

  await chatMic.click();
  await page.waitForFunction(text => document.querySelector('.chat-repo-submit-status')?.textContent.includes(text), 'dictation key');
  const status = await page.textContent('.chat-repo-submit-status');
  assert.match(status, /Tap the prompt box/);
  assert.doesNotMatch(status, /The prompt is focused/);
});

// A non-Safari iOS browser on a plain-http origin trips both blockers at once.
// The HTTPS advice is the fix, and the Safari note has to survive alongside it.
test('an insecure iOS browser gets both the HTTPS fix and the Safari advice', async t => {
  const page = await openPage(t, { stripCapture: true, insecure: true, userAgent: IOS_FIREFOX_UA });
  await micButton(page).waitFor({ state: 'visible' });
  const title = await micButton(page).getAttribute('title');
  assert.match(title, /HTTPS/, 'the insecure origin is still named as the blocker');
  assert.match(title, /Safari/, 'the iOS browser advice is not shadowed by the insecure-context message');
});

// The fallback hint promises the prompt is focused. When the textarea is gone,
// that promise is false and the message has to ask for a tap instead.
test('the keyboard fallback does not claim a focus it could not take', async t => {
  const page = await openPage(t, { stripCapture: true, insecure: true });
  await micButton(page).waitFor({ state: 'visible' });
  await page.evaluate(() => document.getElementById('builder-prompt-input').remove());

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  const status = await page.textContent('#local-prompt-status');
  assert.match(status, /Tap the prompt box/);
  assert.doesNotMatch(status, /The prompt is focused/);
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

// A refusal advertises keyboard dictation, but it is not the end of the story:
// a dismissed permission prompt rejects the same way and Chrome and Firefox ask
// again on the next gesture, so the next tap has to retry capture.
test('a denied microphone shows keyboard dictation and still retries on the next tap', async t => {
  const page = await openPage(t, { rejectCapture: 'NotAllowedError', userAgent: IOS_FIREFOX_UA });
  await micButton(page).waitFor({ state: 'visible' });

  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), true, 'the refusal flips the button to keyboard dictation');
  assert.equal(await page.evaluate(() => window.__micCalls), 1);

  await micButton(page).click();
  await page.waitForFunction(() => window.__micCalls > 1, null, { timeout: 5000 });
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');
  assert.equal(await page.evaluate(() => window.__micCalls), 2, 'the second tap asks for the microphone again');
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), true, 'a second refusal keeps the keyboard dictation state');
});

// A dismissed prompt is the common case on a first tap: the retry has to reach
// a working microphone, not stay stuck on the remembered refusal.
test('a retry after a dismissed permission prompt starts recording', async t => {
  const page = await openPage(t, { rejectCapture: 'NotAllowedError' });
  await micButton(page).waitFor({ state: 'visible' });
  await micButton(page).click();
  await page.waitForFunction(text => document.getElementById('local-prompt-status').textContent.includes(text), 'dictation key');

  // The user grants the permission the second prompt asks for.
  await page.evaluate(() => {
    navigator.mediaDevices.getUserMedia = () => {
      window.__micCalls += 1;
      const Ctor = window.AudioContext || window.webkitAudioContext;
      return Promise.resolve(new Ctor().createMediaStreamDestination().stream);
    };
  });
  await micButton(page).click();
  await page.waitForFunction(() => document.getElementById('local-prompt-status').textContent.includes('Listening'), null, { timeout: 5000 });
  assert.equal(await micButton(page).evaluate(node => node.classList.contains('speech-native')), false, 'the granted retry leaves the fallback state');
});

// While whisper records, the stop button is the only control that matters. On
// iOS the software keyboard would cover it, so the focus taken inside the tap
// is dropped once capture actually starts.
test('a recording microphone releases the prompt focus that raised the keyboard', async t => {
  const page = await openPage(t, { grantCapture: true, userAgent: IOS_FIREFOX_UA });
  await micButton(page).waitFor({ state: 'visible' });

  await micButton(page).click();
  await page.waitForFunction(() => document.getElementById('local-prompt-status').textContent.includes('Listening'), null, { timeout: 5000 });
  assert.notEqual(
    await page.evaluate(() => document.activeElement?.id || ''),
    'builder-prompt-input',
    'the prompt no longer holds focus while recording',
  );
  assert.equal(await micButton(page).getAttribute('aria-pressed'), 'true', 'the button is in its stop state');
});

// Desktop has no software keyboard to cover the stop button, so recording keeps
// the prompt focused the way it did before the keyboard fallback existed.
test('a recording microphone keeps the prompt focused on desktop', async t => {
  const page = await openPage(t, { grantCapture: true });
  await micButton(page).waitFor({ state: 'visible' });

  await micButton(page).click();
  await page.waitForFunction(() => document.getElementById('local-prompt-status').textContent.includes('Listening'), null, { timeout: 5000 });
  assert.equal(
    await page.evaluate(() => document.activeElement?.id),
    'builder-prompt-input',
    'the prompt still holds focus while recording',
  );
});

// The /chat page carries its own copy of the dictation code, so it needs the
// same coverage: an in-gesture focus, a readable fallback message, and a tap
// that asks again after the microphone is refused.
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
  await page.waitForFunction(text => document.querySelector('.chat-repo-submit-status')?.textContent.includes(text), 'dictation key');
  assert.match(await page.textContent('.chat-repo-submit-status'), /Safari/);
  assert.equal(await chatMic.evaluate(node => node.classList.contains('speech-native')), true);

  await chatMic.click();
  await page.waitForFunction(() => window.__micCalls > 1, null, { timeout: 5000 });
  await page.evaluate(() => window.__releaseMic());
  assert.equal(await page.evaluate(() => window.__micCalls), 2, 'the second tap asks for the microphone again');
});

// A status poll that lands mid-recording must not retract the sidecar: the live
// stop button is the only way to finish a capture that is already running.
test('a status poll during chat recording keeps the live stop button', async t => {
  const page = await openPage(t, {
    path: '/chat',
    grantCapture: true,
    clock: true,
    repos: [{ full_name: 'octo/demo', name: 'demo', private: false, description: 'Demo repository' }],
  });
  const chatMic = page.locator('#chat-speech-toggle');
  await page.locator('.chat-repo-card').first().click();
  await chatMic.waitFor({ state: 'visible' });

  await chatMic.click();
  await waitUntil(
    () => page.evaluate(() => document.getElementById('chat-status').textContent.includes('Listening')),
    'the chat page never started recording',
  );

  await page.route('**/api/speech/status', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ ok: true, speech: { enabled: true, reachable: false } }),
  }));
  // Counted on the response, not the request: the page only applies the new
  // sidecar state once the fetch resolves.
  let polls = 0;
  page.on('response', response => {
    if (response.url().includes('/api/speech/status')) polls += 1;
  });
  await page.clock.runFor(31_000); // one chat status poll interval
  await waitUntil(() => polls > 0, 'the status poll never ran while recording');
  await new Promise(resolve => setTimeout(resolve, 250)); // let the poll apply
  assert.equal(
    await page.evaluate(() => document.getElementById('chat-speech-toggle').getAttribute('aria-pressed')),
    'true',
    'the stop button state was dropped by the status poll',
  );
  assert.equal(await chatMic.isVisible(), true, 'the stop button is still on screen');
  assert.equal(
    await chatMic.evaluate(node => node.classList.contains('speech-native')),
    false,
    'an in-flight recording is not flipped into the keyboard fallback',
  );
});

// Tapping the chat mic with no prompt open is the one path that cannot focus
// anything, so the button state still has to be synced before bailing out.
test('the chat mic keeps its keyboard dictation state with no prompt open', async t => {
  const page = await openPage(t, {
    path: '/chat',
    stripCapture: true,
    insecure: true,
    repos: [{ full_name: 'octo/demo', name: 'demo', private: false, description: 'Demo repository' }],
  });
  const chatMic = page.locator('#chat-speech-toggle');
  await chatMic.waitFor({ state: 'visible' });

  await chatMic.click();
  await page.waitForFunction(() => document.getElementById('chat-status').textContent.includes('Open a repository prompt'));
  assert.equal(await chatMic.evaluate(node => node.classList.contains('speech-native')), true, 'the button still advertises keyboard dictation');
  assert.equal(await chatMic.isEnabled(), true, 'the fallback button stays clickable');
});

// The repository cards share one status line with the submit flow, so only the
// dictation hint is allowed to wrap: everything else stays single-line to keep
// the card action row from growing.
test('only the dictation hint wraps in a repository card status', async t => {
  const page = await openPage(t, {
    path: '/#chat',
    stripCapture: true,
    insecure: true,
    repos: [{ full_name: 'octo/demo', name: 'demo', private: false, description: 'Demo repository' }],
  });
  const card = page.locator('.chat-repo-card').first();
  await card.waitFor({ state: 'visible' });
  await card.click();
  const status = card.locator('.chat-repo-submit-status');
  await status.waitFor({ state: 'visible' });
  assert.equal(await status.evaluate(node => window.getComputedStyle(node).whiteSpace), 'nowrap', 'the default status stays on one line');
  const idleHeight = await status.evaluate(node => node.getBoundingClientRect().height);

  await card.locator('.chat-speech-toggle').first().click();
  await page.waitForFunction(text => document.querySelector('.chat-repo-submit-status')?.textContent.includes(text), 'dictation key');
  const wrapped = await status.evaluate(node => ({
    whiteSpace: window.getComputedStyle(node).whiteSpace,
    overflowing: node.scrollWidth > node.clientWidth + 1,
    height: node.getBoundingClientRect().height,
  }));
  assert.equal(wrapped.whiteSpace, 'normal', 'the dictation hint wraps instead of being ellipsized');
  assert.equal(wrapped.overflowing, false, 'the whole hint fits inside the status element');
  assert.ok(wrapped.height > idleHeight, 'the hint is rendered over several lines');
});

// The fallback message names the key to press, so it must wrap instead of being
// ellipsized away by the single-line status styling.
test('the keyboard dictation message is not clipped to one line', async t => {
  const page = await openPage(t, { stripCapture: true, insecure: true });
  await micButton(page).waitFor({ state: 'visible' });
  // Only the hint wraps: the default status shares the prompt action row and
  // has to keep it one line tall.
  assert.equal(
    await page.evaluate(() => window.getComputedStyle(document.getElementById('local-prompt-status')).whiteSpace),
    'nowrap',
    'the default prompt status stays on one line',
  );
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
