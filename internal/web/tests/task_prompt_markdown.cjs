// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/task_prompt_markdown.cjs
// The task snapshot is stubbed, so no harness run is needed.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

const prompt = [
  '# Heading one',
  '',
  'Body with **bold**, `code`, and a [link](https://example.com/docs). ' + 'x'.repeat(260),
  '',
  '- first',
  '- second',
  '',
  '| Metric | Window | Count |',
  '| :--- | :---: | ---: |',
  '| Freshness | 24h | 3 |',
  '',
  '> quoted note',
  '',
  '```go',
  'func run() error { return nil }',
  '```',
  '',
  '- [x] done item',
  '- [ ] open item',
].join('\n');

test('task prompt modal renders markdown through MarkdownView', async t => {
  const page = await browser.newPage({ viewport: { width: 1280, height: 1000 } });
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  // The bare test server has no backing daemon, so its own endpoint failures are
  // not what this test watches for.
  page.on('console', m => { if (m.type() === 'error' && !/net::ERR_FAILED|status of 50\d/.test(m.text())) errors.push(`console: ${m.text()}`); });
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.route('**/api/status', route => route.fulfill({
    json: {
      ok: true,
      tasks: [{
        request_id: 'req-1',
        status: 'running',
        prompt,
        repo_url: 'https://github.com/acme/repo',
        started_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-01T00:00:00Z',
        logs: [],
      }],
      releases: [],
    },
  }));
  // The live stream would replace the stubbed snapshot with the empty server state.
  await page.route('**/api/stream', route => route.abort());
  await page.goto(baseURL + '#work');
  await page.waitForSelector('.task-prompt-toggle', { state: 'attached', timeout: 15000 });
  await page.locator('.task-prompt-toggle').first().click({ force: true });
  await page.waitForSelector('#task-prompt-modal:not(.hidden)');
  await page.waitForSelector('#task-prompt-modal-body.markdown-body:not(.markdown-body-pending)');

  const body = page.locator('#task-prompt-modal-body');
  assert.equal(await body.locator('h1').innerText(), 'Heading one');
  assert.equal(await body.locator('strong').first().innerText(), 'bold');
  assert.equal(await body.locator('code').first().innerText(), 'code');
  assert.equal(await body.locator('a').getAttribute('rel'), 'noopener noreferrer');
  assert.equal(await body.locator('table th').count(), 3);
  assert.equal(await body.locator('blockquote').innerText(), 'quoted note');
  assert.equal((await body.locator('pre code').innerText()).trim(), 'func run() error { return nil }');
  assert.equal(await body.locator('input[type="checkbox"]').count(), 2);
  assert.equal(await body.locator('input[disabled]').count(), 2);
  const aligns = await page.$$eval('#task-prompt-modal-body th', nodes => nodes.map(n => getComputedStyle(n).textAlign.replace('-webkit-', '')));
  assert.deepEqual(aligns, ['left', 'center', 'right']);

  await page.locator('#task-prompt-modal-plain').click();
  assert.ok((await body.innerText()).includes('**bold**'), 'plain tab shows raw markdown');
  assert.equal(await body.locator('h1').count(), 0);

  await page.locator('#task-prompt-modal-markdown').click();
  await page.waitForSelector('#task-prompt-modal-body.markdown-body:not(.markdown-body-pending)');
  assert.equal(await body.locator('h1').innerText(), 'Heading one');

  assert.deepEqual(errors, []);
});
