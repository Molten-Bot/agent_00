const { after, before, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');

const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
const snapshot = { tasks: [], events: [], repositories: [], stats: {}, resources: {} };
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

async function openPage(t, submit = {}) {
  const page = await browser.newPage();
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
        return route.fulfill({ json: snapshot });
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
  const page = await openPage(t, { status: 409, json: { duplicate: true, error: 'duplicate fixture' } });
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

test('Task snapshots emit changed statuses only and keep request IDs local', async t => {
  const page = await openPage(t);
  await page.goto(baseURL);
  await page.waitForFunction(() => state.snapshot !== undefined && state.snapshot !== null);
  const statuses = ['pending', 'running', 'paused', 'running', 'completed', 'completed'];
  for (const [index, status] of statuses.entries()) {
    const generatedAt = new Date(index * 1000).toISOString();
    await page.evaluate(({ status, snapshot, generatedAt }) => window.testTaskStream.onmessage({ data: JSON.stringify({
      ...snapshot, generated_at: generatedAt, tasks: [{ request_id: 'private-task-fixture', source: 'prompt', status }],
    }) }), { status, snapshot, generatedAt });
    await page.waitForFunction(generatedAt => state.snapshot.generated_at === generatedAt, generatedAt);
  }
  const observed = await events(page, 'task_status_observed');
  assert.deepEqual(observed.map(event => event.task_status), ['pending', 'running', 'paused', 'running', 'completed']);
  assert.equal(observed[3].previous_status, 'paused');
  assert.doesNotMatch(JSON.stringify(observed), /private-task-fixture/);
});
