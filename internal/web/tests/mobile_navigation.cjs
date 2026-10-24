// HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/mobile_navigation.cjs
// Requires Playwright/Chromium. All APIs use fixtures; no tasks or settings are changed.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

async function openPage(t, hash, viewport = { width: 430, height: 932 }, mobile = true) {
  const page = await browser.newPage({ viewport, isMobile: mobile, hasTouch: mobile });
  page.setDefaultTimeout(10000);
  t.after(() => page.close());
  const errors = [], writes = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  const now = new Date().toISOString();
  const tasks = Array.from({ length: 24 }, (_, i) => ({
    request_id: `mobile-${i}`, prompt: `Task ${i + 1}: Improve repository navigation and support long descriptions on mobile devices`,
    prompt_is_user_input: true, repo: 'example/mobile-project', status: i < 16 ? 'running' : 'completed',
    stage: 'agent', source: 'prompt', agent_harness: 'codex',
    started_at: now, updated_at: now,
    controls: { pause: true, stop: true }, logs: [{ text: 'Example terminal output', stream: 'stdout' }],
  }));
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (route.request().method() !== 'GET') writes.push(url.pathname);
    let json = { ok: true };
    if (url.pathname === '/api/status') json = { tasks, events: [], repositories: [], stats: {}, resources: {} };
    if (url.pathname === '/api/agent-auth') json.auth = { required: false, ready: true, harness: 'codex', state: 'ready' };
    if (url.pathname === '/api/review-settings') json.settings = { review_level: 'off', default_library_enabled: true };
    if (url.pathname === '/api/github/repos') json.repos = Array.from({ length: 40 }, (_, i) => ({
      name: `project-${i + 1}`, full_name: `example/project-${i + 1}`, description: 'An example repository for mobile navigation.', private: false,
    }));
    if (url.pathname === '/api/library') json.tasks = Array.from({ length: 24 }, (_, i) => ({
      name: `task-${i}`, displayName: `Library task ${i + 1}`, type: 'quality',
      description: 'Review and improve the repository with focused changes.', prompt: 'Example prompt',
    }));
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    return route.fulfill({ json });
  });
  await page.goto(`${baseURL}/#${hash}`);
  await page.waitForFunction(() => document.querySelector('.page-bottom-dock')?.dataset.bottomDockReady === 'true');
  return { page, writes };
}

async function assertFits(page) {
  const geometry = await page.evaluate(() => {
    const dock = document.querySelector('.page-bottom-dock').getBoundingClientRect();
    return { width: innerWidth, client: document.documentElement.clientWidth, scroll: document.documentElement.scrollWidth,
      dockLeft: dock.left, dockRight: dock.right, dockBottom: dock.bottom, height: innerHeight };
  });
  assert.equal(geometry.width, geometry.client, 'layout viewport must not expand beyond the phone');
  assert.equal(geometry.scroll, geometry.client, 'no horizontal page overflow');
  assert.ok(geometry.dockLeft >= 0 && geometry.dockRight <= geometry.client + 1, 'dock fits horizontally');
  assert.ok(geometry.dockBottom <= geometry.height + 1, 'navigation remains onscreen');
}

for (const viewport of [{ width: 390, height: 844 }, { width: 430, height: 932 }, { width: 440, height: 956 }, { width: 932, height: 430 }, { width: 430, height: 500 }]) {
  test(`phone ${viewport.width}×${viewport.height}: navigate, scroll to the last library entry, and keep Run accessible`, async t => {
    const { page } = await openPage(t, 'studio-library', viewport);
    await page.locator('.library-task-option').last().waitFor();
    await assertFits(page);
    const last = page.locator('.library-task-option-toggle').last();
    await last.scrollIntoViewIfNeeded();
    await last.tap();
    assert.equal(await page.locator('.library-task-option-toggle').last().getAttribute('aria-pressed'), 'true');
    assert.ok(await page.evaluate(() => scrollY > 0), 'the document scrolls through entries');
    const submit = await page.locator('#local-prompt-submit').boundingBox();
    const dock = await page.locator('.page-bottom-dock').boundingBox();
    assert.ok(submit.y >= 0 && submit.y + submit.height <= dock.y + 1, 'Run is above the dock');
    await page.locator('#mobile-dock-more').tap();
    await page.locator('#prompt-mode-json').tap();
    await page.waitForFunction(() => document.body.dataset.promptMode === 'json');
    assert.equal(await page.locator('#mobile-dock-more').getAttribute('aria-expanded'), 'false');
    await assertFits(page);
    await page.locator('#task-view-dock-link').tap();
    await page.locator('#task-list .task').last().waitFor();
    await page.locator('#task-list .task').last().scrollIntoViewIfNeeded();
    assert.ok(await page.locator('#task-panel .task-scroll').evaluate(node => node.scrollTop > 0));
    await assertFits(page);
    await page.locator('#task-history-toggle').tap();
    await page.waitForFunction(() => document.querySelectorAll('#task-list .task').length === 24);
    await page.locator('#task-list .task').last().scrollIntoViewIfNeeded();
  });
}

test('swiping from a task action scrolls without firing it; tapping still opens output', async t => {
  const { page, writes } = await openPage(t, 'current-work');
  const output = page.locator('#task-list .task-output-toggle').first();
  await output.waitFor();
  const box = await output.boundingBox();
  const client = await page.context().newCDPSession(page);
  const x = box.x + box.width / 2, y = box.y + box.height / 2;
  await client.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
  assert.equal(await page.locator('#task-fullscreen').getAttribute('aria-hidden'), 'true', 'touch-down must not activate output');
  for (let step = 1; step <= 8; step++) {
    await client.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: y - step * 20 }] });
  }
  await client.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  assert.equal(await page.locator('#task-fullscreen').getAttribute('aria-hidden'), 'true');
  assert.deepEqual(writes, [], 'scrolling never sends task controls');
  await page.waitForFunction(() => document.querySelector('#task-panel .task-scroll').scrollTop > 0);
  await output.tap();
  await page.waitForFunction(() => document.querySelector('#task-fullscreen').getAttribute('aria-hidden') === 'false');
  await page.locator('#task-fullscreen-close').tap();
  await page.waitForFunction(() => document.querySelector('#task-fullscreen').getAttribute('aria-hidden') === 'true');
});

test('More supports Escape, outside dismissal, settings and resizing back to desktop', async t => {
  const { page } = await openPage(t, 'current-work');
  const more = page.locator('#mobile-dock-more');
  await more.tap();
  await page.keyboard.press('Escape');
  assert.equal(await more.getAttribute('aria-expanded'), 'false');
  assert.equal(await more.evaluate(node => document.activeElement === node), true);
  await more.tap();
  await page.locator('#task-panel-title').tap();
  assert.equal(await more.getAttribute('aria-expanded'), 'false');
  await more.tap();
  await page.locator('#settings-open').tap();
  await page.locator('#settings-gate:not(.hidden)').waitFor();
  await page.locator('#review-settings-close').tap();
  await page.setViewportSize({ width: 1280, height: 900 });
  assert.equal(await more.isVisible(), false);
  assert.equal(await page.locator('#mobile-dock-secondary').evaluate(node => node.inert), false);
  assert.equal(await page.locator('#prompt-mode-json').isVisible(), true);
  await page.locator('#prompt-mode-json').click();
  await page.waitForFunction(() => document.body.dataset.promptMode === 'json');
});

for (const viewport of [{ width: 430, height: 932 }, { width: 932, height: 430 }]) {
  test(`repository navigation ${viewport.width}: scroll, paginate, search and open an entry`, async t => {
    const { page, writes } = await openPage(t, 'chat', viewport);
    const cards = page.locator('#chat-repo-grid .chat-repo-card');
    await cards.last().waitFor();
    await assertFits(page);
    const overlaps = await cards.evaluateAll(nodes => nodes.slice(1).some((node, index) => {
      const previous = nodes[index].getBoundingClientRect(), current = node.getBoundingClientRect();
      return current.top < previous.bottom - 1 && current.left < previous.right - 1;
    }));
    assert.equal(overlaps, false, 'repository cards must not overlap the open chat');
    await cards.last().scrollIntoViewIfNeeded();
    assert.ok(await page.locator('#chat-repo-grid').evaluate(node => node.scrollTop > 0));
    const next = page.getByRole('button', { name: 'Next', exact: true });
    const nextBox = await next.boundingBox(), dock = await page.locator('.page-bottom-dock').boundingBox();
    assert.ok(nextBox.y >= 0 && nextBox.y + nextBox.height <= dock.y, 'pagination stays above navigation');
    await next.tap();
    assert.match(await page.locator('.chat-repo-page-label').textContent(), /Page 2 of/);
    await page.locator('#chat-repo-search').fill('project-40');
    await page.waitForFunction(() => document.querySelectorAll('#chat-repo-grid .chat-repo-card').length === 1);
    const result = cards.first();
    assert.match(await result.textContent(), /project-40/);
    await result.tap();
    await page.waitForFunction(() => document.querySelector('#chat-repo-grid .chat-repo-card')?.getAttribute('aria-expanded') === 'true');
    await page.locator('.chat-repo-prompt:visible').fill('A draft typed on a phone');
    assert.deepEqual(writes, [], 'browsing and drafting never starts a run');
  });
}
