// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/github_issue_markdown.cjs
// Uses the real embedded page; the GitHub endpoints are stubbed so no repository is contacted.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

const issueBody = [
  '## Connector contract',
  '',
  'A connector is **one definition** with a `source key` and a [listing function](https://example.com/docs).',
  '',
  '- Fleet overview',
  '- Connection detail',
  '',
  '| Metric | Definition | Window | Count |',
  '| --- | :--- | :---: | ---: |',
  '| Freshness | Time since lastSuccessAt | 24h | 3 |',
  '',
  '```go',
  'func run() error { return nil }',
  '```',
].join('\n');

// Everything a markdown body can smuggle into the page: raw script and frame
// tags, an inline event handler, and a javascript: link.
const hostileBody = [
  '<script>window.__xss = "executed";</script>',
  '',
  '<img src="x" onerror="window.__xss = \'executed\'">',
  '',
  '<iframe src="https://example.com/frame"></iframe>',
  '',
  '[click me](javascript:window.__xss="executed")',
  '',
  'Trailing **text** survives.',
].join('\n');

async function openIssue(t, { body = issueBody, comments = [] } = {}) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.route('**/api/agent-auth**', route => route.fulfill({ json: { ok: true, auth: { required: true, ready: true, harness: 'claude', state: 'ready' } } }));
  await page.route('**/api/review-settings', route => route.fulfill({ json: { ok: true, settings: { merge_method: 'squash', review_level: 'off', github_issues_enabled: true, github_issue_link_mode: 'reference' } } }));
  await page.route('**/api/github/repos', route => route.fulfill({ json: { ok: true, repos: [{ full_name: 'acme/repo', name: 'repo' }] } }));
  await page.route('**/api/github/issues?**', route => route.fulfill({
    json: { ok: true, issues: [{ number: 7, title: 'Ingestion connectors', state: 'open', updatedAt: '2026-01-02T03:04:05Z', labels: [] }] },
  }));
  await page.route('**/api/github/issues/7**', route => route.fulfill({
    json: {
      ok: true,
      issue: {
        number: 7,
        title: 'Ingestion connectors',
        state: 'open',
        url: 'https://example.com/acme/repo/issues/7',
        author: { login: 'octocat' },
        createdAt: '2026-01-01T00:00:00Z',
        updatedAt: '2026-01-02T03:04:05Z',
        assignees: [],
        labels: [],
        body,
        comments,
      },
    },
  }));
  await page.goto(`${baseURL}/#studio-issues`);
  await page.waitForFunction(() => Boolean(document.querySelector('#github-issues-repo option[value="acme/repo"]')));
  await page.selectOption('#github-issues-repo', 'acme/repo');
  await page.locator('#github-issues-list button').first().click();
  await page.waitForSelector('.github-issue-body:not(.markdown-body-pending)');
  return page;
}

test('issue bodies render as markdown instead of raw source', async t => {
  const page = await openIssue(t, {
    comments: [{ author: { login: 'hubot' }, createdAt: '2026-01-02T00:00:00Z', body: 'Try `marked`, see the **docs**.' }],
  });
  const body = page.locator('.github-issue-body');

  assert.equal(await body.locator('h2').innerText(), 'Connector contract');
  assert.equal(await body.locator('strong').first().innerText(), 'one definition');
  assert.equal(await body.locator('code').first().innerText(), 'source key');
  assert.equal(await body.locator('ul > li').count(), 2);
  assert.equal(await body.locator('table th').first().innerText(), 'Metric');
  assert.equal((await body.locator('pre code').innerText()).trim(), 'func run() error { return nil }');
  assert.ok(!(await body.innerText()).includes('**one definition**'), 'markdown markers are consumed, not shown');

  const link = body.locator('a');
  assert.equal(await link.getAttribute('href'), 'https://example.com/docs');
  assert.equal(await link.getAttribute('target'), '_blank');
  assert.equal(await link.getAttribute('rel'), 'noopener noreferrer');

  const comment = page.locator('.github-issue-comment-body');
  await page.waitForSelector('.github-issue-comment-body:not(.markdown-body-pending)');
  assert.equal(await comment.locator('strong').innerText(), 'docs');
});

test('GFM table column alignment survives sanitizing and styling', async t => {
  const page = await openIssue(t);
  const columns = await page.$$eval('.github-issue-body table th', nodes => nodes.map(node => ({
    align: node.getAttribute('align'),
    // Chromium reports the align attribute's keyword with a -webkit- prefix.
    textAlign: getComputedStyle(node).textAlign.replace('-webkit-', ''),
  })));

  assert.deepEqual(columns.map(column => column.align), [null, 'left', 'center', 'right'], 'align attributes are preserved');
  assert.deepEqual(columns.map(column => column.textAlign), ['left', 'left', 'center', 'right'], 'stylesheet defaults to left without overriding the alignment');
});

test('a failed marked load warns, falls back to plain text, and retries next render', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  const warnings = [];
  page.on('console', message => { if (message.type() === 'warning') warnings.push(message.text()); });
  let failMarked = true;
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.route('**/static/marked.umd.js', route => {
    if (!failMarked) return route.continue();
    failMarked = false;
    return route.abort();
  });
  await page.goto(baseURL);
  await page.waitForFunction(() => Boolean(window.MarkdownView));

  const renderOnce = () => page.evaluate(async () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    const rendered = await window.MarkdownView.render(host, 'keep **this**');
    return { rendered, text: host.textContent, html: host.innerHTML, pending: host.classList.contains('markdown-body-pending') };
  });

  const first = await renderOnce();
  assert.equal(first.rendered, false, 'the failed load is reported to the caller');
  assert.equal(first.text, 'keep **this**', 'the body stays readable as plain text');
  assert.ok(first.pending, 'the plain-text fallback keeps its line breaks');
  assert.ok(warnings.some(warning => warning.includes('markdown render failed')), `expected a console warning, got ${JSON.stringify(warnings)}`);

  const second = await renderOnce();
  assert.equal(second.rendered, true, 'a later render retries the script instead of reusing the failure');
  assert.ok(second.html.includes('<strong>this</strong>'), `expected rendered markdown, got ${second.html}`);
  assert.ok(!second.pending);
});

test('markdown HTML cannot execute script or keep javascript: links', async t => {
  const page = await openIssue(t, { body: hostileBody });
  const body = page.locator('.github-issue-body');

  assert.equal(await page.evaluate(() => window.__xss || ''), '');
  assert.equal(await body.locator('script').count(), 0);
  assert.equal(await body.locator('iframe').count(), 0);
  assert.equal(await body.locator('img').count(), 0, 'an image whose only payload is an event handler is dropped with it');
  assert.equal(await body.locator('[onerror]').count(), 0);
  assert.equal(await body.locator('a').getAttribute('href'), null, 'javascript: hrefs are stripped from the link');
  assert.equal(await body.locator('strong').innerText(), 'text');
  assert.equal(await page.evaluate(() => window.__xss || ''), '');
});

test('markdown renderer keeps safe elements and drops the rest', async t => {
  const page = await openIssue(t);
  const result = await page.evaluate(() => {
    const host = document.createElement('div');
    host.appendChild(window.MarkdownView.sanitize(
      '<p>keep <b>unwrapped</b> <a href="https://example.com" title="t">link</a></p>'
      + '<style>body{display:none}</style>'
      + '<form><button onclick="window.__xss=1">go</button></form>'
      + '<img src="https://example.com/a.png" alt="a">'
      + '<img src="data:image/svg+xml,<svg onload=alert(1)>" alt="b">'
      + '<img src="relative.png" alt="relative alt text">'
      + '<img src="relative.png">'
      + '<ul><li><input type="checkbox" checked><input type="text" value="x"> task</li></ul>'
      + '<code class="language-go">go</code><code class="prompt-submit">styled</code>',
    ));
    const link = host.querySelector('a');
    const checkbox = host.querySelector('input');
    return {
      html: host.innerHTML,
      text: host.textContent,
      tags: Array.from(host.querySelectorAll('*')).map(node => node.tagName.toLowerCase()),
      link: { href: link.getAttribute('href'), title: link.getAttribute('title'), target: link.target, rel: link.rel },
      images: Array.from(host.querySelectorAll('img')).map(node => node.getAttribute('src')),
      inputs: Array.from(host.querySelectorAll('input')).length,
      checkbox: { type: checkbox.type, checked: checkbox.checked, disabled: checkbox.disabled },
      codeClasses: Array.from(host.querySelectorAll('code')).map(node => node.getAttribute('class')),
    };
  });

  assert.ok(result.text.includes('keep unwrapped'), 'unknown tags keep their text');
  assert.deepEqual(result.link, { href: 'https://example.com', title: 't', target: '_blank', rel: 'noopener noreferrer' });
  assert.ok(!result.tags.includes('style') && !result.text.includes('display:none'), 'style blocks are dropped with their contents');
  assert.ok(!result.tags.includes('button') && !result.html.includes('onclick'), 'buttons are dropped with their handlers');
  assert.deepEqual(result.images, ['https://example.com/a.png'], 'only absolute http(s) image sources survive');
  assert.ok(result.text.includes('relative alt text'), 'a dropped image leaves its alt text behind');
  assert.equal(result.inputs, 1, 'only the task list checkbox survives');
  assert.deepEqual(result.checkbox, { type: 'checkbox', checked: true, disabled: true }, 'task list checkboxes stay read-only');
  assert.deepEqual(result.codeClasses, ['language-go', null], 'application classes cannot be borrowed by issue text');
});
