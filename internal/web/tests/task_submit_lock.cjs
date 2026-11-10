// HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/task_submit_lock.cjs
const { after, before, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');

const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;

before(async () => {
  browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
});
after(async () => { await browser?.close(); });

function installControlledTaskStream(page) {
  return page.addInitScript(() => {
    class ControlledEventSource {
      constructor() {
        window.testTaskStream = this;
        queueMicrotask(() => this.onopen?.());
      }
    }
    window.EventSource = ControlledEventSource;
  });
}

test('Studio Run remains available with active tasks and unresolved status', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  const writes = [];
  let status = 'running';
  let releaseInitialStatus;
  const initialStatusGate = new Promise(resolve => { releaseInitialStatus = resolve; });
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await installControlledTaskStream(page);

  await page.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (route.request().method() !== 'GET') writes.push(url.pathname);
    if (url.pathname === '/api/status') {
      await initialStatusGate;
      return route.fulfill({ json: {
        tasks: [{ request_id: 'github-actions-1', source: 'github_actions', status }],
        events: [], repositories: [], stats: {}, resources: {},
      } });
    }
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  const run = page.locator('#local-prompt-submit');
  await expect(run).toBeEnabled();
  releaseInitialStatus();
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');

  const cancelled = await page.evaluate(() => document.getElementById('local-prompt-form')
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })) === false);
  assert.equal(cancelled, true);
  assert.deepEqual(writes, [], 'empty prompt must not submit');

  status = 'completed';
  await page.reload();
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
});

test('Studio Run ignores an initial status older by one nanosecond than the stream snapshot', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  let releaseInitialStatus;
  const initialStatusGate = new Promise(resolve => { releaseInitialStatus = resolve; });
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await installControlledTaskStream(page);

  await page.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (url.pathname === '/api/status') {
      await initialStatusGate;
      return route.fulfill({ json: {
        generated_at: '2026-11-09T20:00:00.123456788Z',
        tasks: [], events: [], repositories: [], stats: {}, resources: {},
      } });
    }
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  await page.evaluate(() => window.testTaskStream.onmessage({ data: JSON.stringify({
    generated_at: '2026-11-09T20:00:00.123456789Z',
    tasks: [{ request_id: 'github-actions-newer', source: 'github_actions', status: 'running' }],
    events: [], repositories: [], stats: {}, resources: {},
  }) }));
  await expect.poll(() => page.evaluate(() => state.snapshot?.tasks?.[0]?.request_id || ''))
    .toBe('github-actions-newer');
  const run = page.locator('#local-prompt-submit');
  await expect(run).toBeEnabled();

  releaseInitialStatus();
  await expect.poll(() => page.evaluate(() => state.snapshot?.tasks?.[0]?.request_id || ''))
    .toBe('github-actions-newer');
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
});

test('Studio Run remains available when initial status fails', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  const writes = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await installControlledTaskStream(page);

  await page.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (route.request().method() !== 'GET') writes.push(url.pathname);
    if (url.pathname === '/api/status') return route.fulfill({ status: 503, json: { error: 'unavailable' } });
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  const run = page.locator('#local-prompt-submit');
  await expect(run).toBeEnabled();
  const cancelled = await page.evaluate(() => document.getElementById('local-prompt-form')
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })) === false);
  assert.equal(cancelled, true);
  assert.deepEqual(writes, [], 'empty prompt must not submit');

  await page.evaluate(() => window.testTaskStream.onmessage({ data: JSON.stringify({
    tasks: [], events: [], repositories: [], stats: {}, resources: {},
  }) }));
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
});

test('Studio Run remains available when the task stream fails before initial status resolves', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  let releaseInitialStatus;
  const initialStatusGate = new Promise(resolve => { releaseInitialStatus = resolve; });
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await installControlledTaskStream(page);

  await page.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (url.pathname === '/api/status') {
      await initialStatusGate;
      return route.fulfill({ json: { tasks: [], events: [], repositories: [], stats: {}, resources: {} } });
    }
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  const run = page.locator('#local-prompt-submit');
  await page.evaluate(() => window.testTaskStream.onerror());
  await expect(run).toBeEnabled();

  releaseInitialStatus();
  await expect.poll(() => page.evaluate(() => state.snapshot !== null)).toBe(true);
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
});

test('Studio Run remains available when the task stream disconnects', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));

  await installControlledTaskStream(page);
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (url.pathname === '/api/status') {
      return route.fulfill({ json: { tasks: [], events: [], repositories: [], stats: {}, resources: {} } });
    }
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  const run = page.locator('#local-prompt-submit');
  await expect(run).toBeEnabled();

  await page.evaluate(() => {
    window.testTaskStream.onmessage({ data: JSON.stringify({
      tasks: [], events: [], repositories: [], stats: {}, resources: {},
    }) });
    window.testTaskStream.onerror();
  });
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
  await page.evaluate(() => render(state.snapshot));
  await expect(run).toBeEnabled();
  await page.waitForTimeout(600);
  await expect(run).toBeEnabled();

  await page.evaluate(() => window.testTaskStream.onmessage({ data: JSON.stringify({
    tasks: [], events: [], repositories: [], stats: {}, resources: {},
  }) }));
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
});

test('Studio Run remains available after a duplicate response', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  let submitCount = 0;
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await installControlledTaskStream(page);

  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (url.pathname === '/api/status') {
      return route.fulfill({ json: { tasks: [], events: [], repositories: [], stats: {}, resources: {} } });
    }
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    if (url.pathname === '/api/local-prompt' && route.request().method() === 'POST') {
      submitCount++;
      return route.fulfill({
        status: 409,
        json: { ok: false, duplicate: true, request_id: 'existing-task', state: 'in_flight' },
      });
    }
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  const run = page.locator('#local-prompt-submit');
  await expect(run).toBeEnabled();
  await page.locator('#builder-repo-input').fill('acme/repo');
  await page.locator('#builder-prompt-input').fill('Do work');
  await run.click();
  await expect(page.locator('#local-prompt-status')).toContainText('Duplicate ignored');
  await expect(run).toBeEnabled();

  await page.evaluate(() => render(
    { tasks: [], events: [], repositories: [], stats: {}, resources: {} },
    { resolveTaskStatus: true },
  ));
  await expect(run).toBeEnabled();
  await page.evaluate(() => render(
    {
      tasks: [{ request_id: 'existing-task', source: 'local_prompt', status: 'running' }],
      events: [], repositories: [], stats: {}, resources: {},
    },
    { resolveTaskStatus: true },
  ));
  await expect(run).toBeEnabled();
  await page.evaluate(() => render(
    {
      tasks: [{ request_id: 'existing-task', source: 'local_prompt', status: 'completed' }],
      events: [], repositories: [], stats: {}, resources: {},
    },
    { resolveTaskStatus: true },
  ));
  await expect(run).toBeEnabled();
  assert.equal(submitCount, 1);
});

test('Studio Run is available when duplicate completion arrives before submit response', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await installControlledTaskStream(page);

  await page.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (url.pathname === '/api/status') {
      return route.fulfill({ json: { tasks: [], events: [], repositories: [], stats: {}, resources: {} } });
    }
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    if (url.pathname === '/api/local-prompt' && route.request().method() === 'POST') {
      await page.evaluate(() => render(
        {
          tasks: [{ request_id: 'existing-fast-task', source: 'local_prompt', status: 'completed' }],
          events: [], repositories: [], stats: {}, resources: {},
        },
        { resolveTaskStatus: true },
      ));
      return route.fulfill({
        status: 409,
        json: { ok: false, duplicate: true, request_id: 'existing-fast-task', state: 'in_flight' },
      });
    }
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  const run = page.locator('#local-prompt-submit');
  await expect(run).toBeEnabled();
  await page.locator('#builder-repo-input').fill('acme/repo');
  await page.locator('#builder-prompt-input').fill('Do work');
  await run.click();
  await expect(page.locator('#local-prompt-status')).toContainText('Duplicate ignored');
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
});

test('Studio Run is available when completion arrives before submit response', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await installControlledTaskStream(page);

  await page.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (url.pathname === '/api/status') {
      return route.fulfill({ json: { tasks: [], events: [], repositories: [], stats: {}, resources: {} } });
    }
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    if (url.pathname === '/api/local-prompt' && route.request().method() === 'POST') {
      await page.evaluate(() => render(
        {
          tasks: [{ request_id: 'fast-task', source: 'local_prompt', status: 'completed' }],
          events: [], repositories: [], stats: {}, resources: {},
        },
        { resolveTaskStatus: true },
      ));
      return route.fulfill({ json: { ok: true, request_id: 'fast-task' } });
    }
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  const run = page.locator('#local-prompt-submit');
  await expect(run).toBeEnabled();
  await page.locator('#builder-repo-input').fill('acme/repo');
  await page.locator('#builder-prompt-input').fill('Do work');
  await run.click();
  await expect(page.locator('#local-prompt-status')).toContainText('Queued request fast-task');
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
});

test('Studio Run is available when a newer snapshot has already pruned the submitted task', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await installControlledTaskStream(page);

  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (url.pathname === '/api/status') {
      return route.fulfill({ json: {
        generated_at: '2026-11-09T20:00:00.123456789Z',
        tasks: [], events: [], repositories: [], stats: {}, resources: {},
      } });
    }
    if (url.pathname === '/api/agent-auth') {
      return route.fulfill({ json: { ok: true, auth: { required: false, ready: true, state: 'ready' } } });
    }
    if (url.pathname === '/api/review-settings') {
      return route.fulfill({ json: { ok: true, settings: { default_library_enabled: true } } });
    }
    if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
    if (url.pathname === '/api/library') return route.fulfill({ json: { ok: true, tasks: [] } });
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    if (url.pathname === '/api/local-prompt' && route.request().method() === 'POST') {
      return route.fulfill({ json: { ok: true, request_id: 'pruned-task' } });
    }
    return route.fulfill({ json: { ok: true } });
  });

  await page.goto(baseURL);
  const run = page.locator('#local-prompt-submit');
  await expect(run).toBeEnabled();
  await page.locator('#builder-repo-input').fill('acme/repo');
  await page.locator('#builder-prompt-input').fill('Do work');
  await run.click();
  await expect(page.locator('#local-prompt-status')).toContainText('Queued request pruned-task');
  await expect(run).toBeEnabled();

  await page.evaluate(() => render(state.snapshot));
  await expect(run).toBeEnabled();

  await page.evaluate(() => render({
    generated_at: '2026-11-09T20:00:00.123456790Z',
    tasks: [], events: [], repositories: [], stats: {}, resources: {},
  }, { resolveTaskStatus: true }));
  await expect(run).toBeEnabled();

  await page.evaluate(() => render({
    generated_at: '2026-11-09T20:00:00.123456791Z',
    tasks: [], events: [], repositories: [], stats: {}, resources: {},
  }, { resolveTaskStatus: true }));
  await expect(run).toBeEnabled();
  await expect(run).toHaveAttribute('title', '');
});

for (const mode of ['prompt', 'library']) {
  test(`Studio queues multiple ${mode} tasks under resource pressure without waiting for snapshots`, async t => {
    const page = await browser.newPage();
    t.after(() => page.close());
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    t.after(() => assert.deepEqual(errors, []));
    await installControlledTaskStream(page);
    const writes = [];
    let releaseSubmission;
    await page.route('**/*', async route => {
      const url = new URL(route.request().url());
      if (url.origin !== new URL(baseURL).origin) return route.abort();
      if (!url.pathname.startsWith('/api/')) return route.continue();
      if (url.pathname === '/api/status') return route.fulfill({ json: {
        tasks: [
          { request_id: 'running', status: 'running', source: 'local_prompt' },
          { request_id: 'waiting', status: 'queued', source: 'local_prompt' },
        ],
        events: [], repositories: [], stats: {},
        resources: { cpu_percent: 99, memory_percent: 99 },
      } });
      if (url.pathname === '/api/agent-auth') return route.fulfill({ json: {
        ok: true, auth: { required: false, ready: true, state: 'ready' },
      } });
      if (url.pathname === '/api/review-settings') return route.fulfill({ json: {
        ok: true, settings: { default_library_enabled: true },
      } });
      if (url.pathname === '/api/library') return route.fulfill({ json: {
        ok: true, tasks: [{ name: 'unit-test-coverage', title: 'Unit test coverage', prompt: 'Add tests', promptContextLabel: 'Additional context' }],
      } });
      if (url.pathname === '/api/github/repos') return route.fulfill({ json: { ok: true, repos: [] } });
      if (route.request().method() === 'POST' &&
          ['/api/local-prompt', '/api/library/run'].includes(url.pathname)) {
        writes.push({ endpoint: url.pathname, body: route.request().postDataJSON() });
        if (writes.length === 1) await new Promise(resolve => { releaseSubmission = resolve; });
        return route.fulfill({ json: { ok: true, request_id: `queued-${writes.length}` } });
      }
      return route.fulfill({ json: { ok: true } });
    });
    await page.goto(baseURL + (mode === 'library' ? '/#studio-library' : ''));
    const run = page.locator('#local-prompt-submit');
    const fill = async text => {
      if (mode === 'library') {
        await page.locator('#library-repo-input').fill('acme/repo');
        await expect(page.locator('.library-task-option-toggle').first()).toHaveAttribute('aria-pressed', 'true');
        await page.locator('#library-prompt-context-input').fill(text);
      } else {
        await page.locator('#builder-repo-input').fill('acme/repo');
        await page.locator('#builder-prompt-input').fill(text);
      }
    };
    await fill('First task');
    await run.click();
    await expect.poll(() => writes.length).toBe(1);
    await expect(run).toBeDisabled();
    await page.evaluate(() => document.getElementById('local-prompt-form')
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })));
    assert.equal(writes.length, 1, 'double submit cannot duplicate the HTTP request');
    releaseSubmission();
    await expect(run).toBeEnabled();
    await fill('Second task');
    await run.click();
    await expect.poll(() => writes.length).toBe(2);
    await expect(run).toBeEnabled();
    assert.equal(writes[1].endpoint, mode === 'library' ? '/api/library/run' : '/api/local-prompt');
  });
}
