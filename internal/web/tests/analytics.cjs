const { after, before, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');

const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
const snapshot = { tasks: [], events: [], repositories: [], stats: {}, resources: {} };
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

async function openPage(t, { submit = {}, context, initialSnapshot = snapshot } = {}) {
  const page = context ? await context.newPage() : await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(async () => {
    await page.close();
    assert.deepEqual(errors, []);
  });
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.hostname === 'www.googletagmanager.com') {
      return route.fulfill({ contentType: 'application/javascript', body: '' });
    }
    if (url.origin !== baseURL) return route.abort();
    switch (url.pathname) {
      case '/api/status':
        return route.fulfill({ json: initialSnapshot });
      case '/api/agent-auth':
        return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
      case '/api/review-settings':
        return route.fulfill({ json: { ok: true, settings: { chat_enabled: true } } });
      case '/api/commit-signing':
        return route.fulfill({ json: { ok: true, settings: {} } });
      case '/api/github/repos':
        return route.fulfill({ json: { ok: true, repos: [{ full_name: 'example/private-fixture', private: true }] } });
      case '/api/local-prompt':
        return route.fulfill(submit);
      default:
        return url.pathname.startsWith('/api/') ? route.fulfill({ json: { ok: true } }) : route.continue();
    }
  });
  await page.addInitScript(() => {
    window.EventSource = class {
      constructor() { window.testTaskStream = this; }
    };
  });
  return page;
}

function events(page, name) {
  return page.evaluate(name => window.dataLayer
    .filter(args => args[0] === 'event' && args[1] === name)
    .map(args => args[2]), name);
}

async function assertTag(page) {
  assert.equal(await page.locator('head script[src*="googletagmanager.com/gtag/js"]').count(), 1);
  assert.deepEqual(await page.evaluate(() => window.dataLayer.filter(args => args[0] === 'config').map(args => Array.from(args))), [
    ['config', 'G-BY33RFG2WB', { send_page_view: false }],
  ]);
}

test('Studio reports each rendered route once with safe location and referrer', async t => {
  const page = await openPage(t);
  await page.goto(`${baseURL}/?code=private-query#private-fragment`, {
    referer: 'https://referrer.example/private-path?code=private-referrer',
  });
  await assertTag(page);

  await page.locator('#prompt-mode-library').click();
  await expect.poll(async () => (await events(page, 'page_view')).length).toBe(2);
  const views = await events(page, 'page_view');
  assert.deepEqual(views.map(event => event.page_location), [
    `${baseURL}/#studio-builder`,
    `${baseURL}/#studio-library`,
  ]);
  assert.equal(views[0].page_referrer, 'https://referrer.example/');
  assert.equal(views[1].page_referrer, views[0].page_location);

  await page.locator('#prompt-mode-builder').click();
  await expect.poll(async () => (await events(page, 'page_view')).length).toBe(3);
  await page.goBack();
  await expect.poll(async () => (await events(page, 'page_view')).at(-1).prompt_mode).toBe('library');
  await page.goForward();
  await expect.poll(async () => (await events(page, 'page_view')).length).toBe(5);
  assert.equal((await events(page, 'page_view')).at(-1).prompt_mode, 'builder');
  await page.evaluate(() => new Promise(resolve => {
    window.addEventListener('hashchange', () => resolve(), { once: true });
    location.hash = 'other-private-fragment';
  }));
  assert.equal((await events(page, 'page_view')).length, 5);
  const commands = await page.evaluate(() => JSON.stringify(window.dataLayer));
  assert.doesNotMatch(commands, /private-query|private-fragment|private-path|private-referrer/);

  await page.locator('#settings-open').click();
  await page.locator('#settings-tab-automation').click();
  assert.equal((await events(page, 'settings_tab_selected'))[0].tab, 'automation');

  await page.evaluate(() => { window.gtag = () => { throw new Error('Analytics unavailable'); }; });
  await page.locator('#settings-tab-review').click();
  await expect(page.locator('#settings-panel-review')).toBeVisible();
});

test('Chat records validation and duplicate prompts without repository or prompt contents', async t => {
  const page = await openPage(t, { submit: { status: 409, json: { duplicate: true, error: 'duplicate fixture' } } });
  await page.goto(`${baseURL}/chat?code=private-query`);
  await assertTag(page);
  assert.deepEqual((await events(page, 'page_view')).map(event => event.page_location), [`${baseURL}/chat`]);

  await page.locator('.chat-repo-card').click();
  const prompt = page.locator('.chat-repo-prompt');
  await prompt.press('Enter');
  await expect.poll(async () => (await events(page, 'chat_repo_prompt_validation_failed')).length).toBe(1);
  await prompt.fill('private prompt fixture');
  await prompt.press('Enter');
  await expect.poll(async () => (await events(page, 'chat_repo_prompt_duplicate')).length).toBe(1);
  assert.equal((await events(page, 'chat_repo_prompt_started')).length, 1);
  assert.equal((await events(page, 'chat_repo_prompt_failed')).length, 0);
  const [duplicate] = await events(page, 'chat_repo_prompt_duplicate');
  assert.equal(duplicate.http_status, 409);
  assert.equal(duplicate.source, 'chat_page');
  assert.doesNotMatch(await page.evaluate(() => JSON.stringify(window.dataLayer)),
    /private-query|private-fixture|private prompt fixture|duplicate fixture/);
});

test('GA opt-out suppresses sends and credential popup stays isolated', async t => {
  const page = await openPage(t);
  await page.addInitScript(() => { window['ga-disable-G-BY33RFG2WB'] = true; });
  await page.goto(baseURL);
  await page.locator('#prompt-mode-library').click();
  assert.deepEqual(await page.evaluate(() => window.dataLayer), []);

  await page.goto(`${baseURL}/task-asset-auth`);
  assert.equal(await page.locator('script[src*="analytics"], script[src*="googletagmanager"]').count(), 0);
});

test('Dashboard deep links report only the resolved screen', async t => {
  for (const [totalTasks, display] of [[1, 'dashboard'], [0, 'studio-builder']]) {
    const page = await openPage(t, { initialSnapshot: { ...snapshot, stats: { total_tasks: totalTasks } } });
    await page.goto(`${baseURL}/#dashboard`);
    await page.waitForFunction(() => state.snapshot !== null);
    await expect.poll(async () => (await events(page, 'page_view')).map(event => event.page_location))
      .toEqual([`${baseURL}/#${display}`]);
  }
});

test('Current Work, Chat, and JSON deep links wait for route availability', async t => {
  const activeSnapshot = { ...snapshot, tasks: [{ request_id: 'work-fixture', status: 'running' }] };
  const cases = [
    ['current-work', '**/api/status', { json: activeSnapshot }, 'current-work'],
    ['work', '**/api/status', { json: snapshot }, 'studio-builder'],
    ['chat', '**/api/github/repos?*', { json: { ok: true, repos: [] } }, 'chat'],
    ['chat', '**/api/github/repos?*', { status: 503 }, 'studio-builder'],
    ['studio-json', '**/api/review-settings', { json: { settings: { json_enabled: true } } }, 'studio-json'],
    ['studio-json', '**/api/review-settings', { json: { settings: { json_enabled: false } } }, 'studio-builder'],
  ];
  for (const [hash, endpoint, response, display] of cases) {
    const page = await openPage(t);
    let release;
    const pending = new Promise(resolve => { release = resolve; });
    await page.route(endpoint, async route => {
      await pending;
      await route.fulfill(response);
    });
    try {
      await page.goto(`${baseURL}/#${hash}`, { waitUntil: 'domcontentloaded' });
      assert.deepEqual(await events(page, 'page_view'), [], hash);
    } finally {
      release();
    }
    await expect.poll(async () => (await events(page, 'page_view')).map(event => event.page_location))
      .toEqual([`${baseURL}/#${display}`]);
  }
});

test('Task transitions are reported once across windows and keep request IDs local', async t => {
  const context = await browser.newContext();
  t.after(() => context.close());
  const pages = [await openPage(t, { context }), await openPage(t, { context })];
  await Promise.all(pages.map(async page => {
    await page.goto(baseURL);
    await page.waitForFunction(() => state.snapshot !== null);
    await page.evaluate(() => Object.defineProperty(document, 'hidden', { value: false }));
  }));
  const observedStatuses = async () => (await Promise.all(pages.map(page => events(page, 'task_status_observed')))).flat();
  const statuses = ['pending', 'running', 'paused', 'running', 'completed', 'completed'];
  for (const [index, status] of statuses.entries()) {
    const generatedAt = new Date(index * 1000).toISOString();
    const packet = { ...snapshot, generated_at: generatedAt, tasks: [{
      request_id: 'private-task-fixture', source: 'prompt', status,
      started_at: new Date(0).toISOString(), updated_at: generatedAt, status_revision: Math.min(index, 4),
    }] };
    await Promise.all(pages.map(async (page, client) => {
      const task = { ...packet.tasks[0], updated_at: new Date(index * 1000 + client).toISOString() };
      await page.evaluate(packet => window.testTaskStream.onmessage({ data: JSON.stringify(packet) }), { ...packet, tasks: [task] });
      await page.waitForFunction(generatedAt => state.snapshot.generated_at === generatedAt, generatedAt);
    }));
    await expect.poll(async () => (await observedStatuses()).filter(event =>
      event.task_status === status && event.previous_status === statuses[index - 1]).length).toBe(index === 5 ? 0 : 1);
  }
  const observed = await observedStatuses();
  assert.equal(observed.length, 5);
  assert.doesNotMatch(JSON.stringify(observed), /private-task-fixture/);
});

test('Every delivered task transition is observed even when stream renders are coalesced', async t => {
  const page = await openPage(t);
  await page.goto(baseURL);
  await page.waitForFunction(() => state.snapshot !== null);
  const sequence = ['pending', 'running', 'paused', 'running', 'completed'];
  await page.evaluate(({ snapshot, sequence }) => {
    Object.defineProperty(document, 'hidden', { configurable: true, value: true });
    for (const [index, status] of sequence.entries()) {
      const packet = { ...snapshot, generated_at: new Date(index * 1000).toISOString(), tasks: [{
        request_id: 'burst-task-fixture', source: 'prompt', status, status_revision: index,
      }] };
      window.testTaskStream.onmessage({ data: JSON.stringify(packet) });
    }
    window.testTaskStream.onmessage({ data: '{malformed' });
    window.testTaskStream.onmessage({ data: JSON.stringify({
      ...snapshot, generated_at: new Date(0).toISOString(),
      tasks: [{ request_id: 'burst-task-fixture', status: 'failed', status_revision: 0 }],
    }) });
  }, { snapshot, sequence });
  await expect.poll(async () => (await events(page, 'task_status_observed'))
    .map(event => [event.previous_status, event.task_status]))
    .toEqual(sequence.map((status, index) => [sequence[index - 1], status]));
  assert.deepEqual(await page.evaluate(() => state.snapshot.tasks), []);

  await page.evaluate(() => {
    Object.defineProperty(document, 'hidden', { configurable: true, value: false });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await page.waitForFunction(() => state.snapshot.tasks[0]?.status === 'completed');
  assert.equal((await events(page, 'task_status_observed')).length, sequence.length);
});

test('An image decode error reports a sanitized preview failure', async t => {
  const page = await openPage(t, { initialSnapshot: { ...snapshot, tasks: [{
    request_id: 'media-task-fixture', status: 'running',
    assets: [{ name: 'private-image-fixture', url: 'https://github.com/user-attachments/assets/private-asset-fixture' }],
  }] } });
  await page.route('**/api/tasks/*/asset-preview', route =>
    route.fulfill({ contentType: 'image/png', body: 'invalid image bytes' }));
  await page.goto(`${baseURL}/#current-work`);
  await page.locator('#task-list .task-assets-toggle').click();
  await expect(page.locator('#task-asset-message')).toHaveText('Preview unavailable.');
  await expect.poll(async () => await events(page, 'task_asset_preview_failed')).toEqual([
    { send_to: 'G-BY33RFG2WB', media_type: 'image' },
  ]);
});

test('Automation scan reports acceptance for HTTP 202', async t => {
  const page = await openPage(t);
  await page.route('**/api/review-settings', route => route.fulfill({ json: { ok: true, settings: { github_issues_enabled: true } } }));
  await page.route('**/api/github/automation/scan', route => route.continue());
  await page.goto(baseURL);
  await page.locator('#settings-open').click();
  await page.locator('#settings-tab-github-issues').click();
  const response = page.waitForResponse(response => new URL(response.url()).pathname === '/api/github/automation/scan');
  await page.locator('#github-automation-scan').click();
  assert.equal((await response).status(), 202);
  await expect.poll(async () => await events(page, 'github_automation_scan_accepted')).toHaveLength(1);
  assert.equal((await events(page, 'github_automation_scan_queued')).length, 0);
});
