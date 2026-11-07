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

// GitHub resolves these against the repository the issue lives in, and the
// collapsible section is raw HTML that markdown passes straight through.
const repositoryBody = [
  '<details><summary>Build log</summary>',
  '',
  'Collapsed **detail** text.',
  '',
  '</details>',
  '',
  'See [the guide](docs/guide.md), [the pull request](/acme/repo/pull/3), and [the anchor](#contract).',
  '',
  '![architecture](docs/architecture.png)',
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
  await page.route('**/api/github/repos', route => route.fulfill({
    json: { ok: true, repos: [{ full_name: 'acme/repo', name: 'repo', html_url: 'https://github.com/acme/repo', default_branch: 'main' }] },
  }));
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

test('a script that loads without defining marked is not kept as a usable load', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  let serveEmpty = true;
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.route('**/static/marked.umd.js', route => {
    if (!serveEmpty) return route.continue();
    serveEmpty = false;
    // A 200 that never defines marked: a proxy or an error page served in place
    // of the bundle fires load, not error.
    return route.fulfill({ contentType: 'text/javascript', body: '/* not marked */' });
  });
  await page.goto(baseURL);
  await page.waitForFunction(() => Boolean(window.MarkdownView));

  const renderOnce = () => page.evaluate(async () => {
    const host = document.createElement('div');
    document.body.appendChild(host);
    const rendered = await Promise.race([
      window.MarkdownView.render(host, 'keep **this**'),
      new Promise(resolve => { setTimeout(() => resolve('pending'), 5000); }),
    ]);
    const result = { rendered, html: host.innerHTML, scripts: document.querySelectorAll('script[data-marked-script="true"]').length };
    host.remove();
    return result;
  });

  const first = await renderOnce();
  assert.equal(first.rendered, false, 'a bundle without marked is reported as a failed render');
  assert.equal(first.scripts, 0, 'the unusable script element is dropped instead of being left in the document');

  const second = await renderOnce();
  assert.equal(second.rendered, true, 'the next render fetches again instead of failing against the stale element');
  assert.ok(second.html.includes('<strong>this</strong>'), `expected rendered markdown, got ${second.html}`);
});

test('relative links resolve against the repository and collapsible sections stay collapsible', async t => {
  const page = await openIssue(t, { body: repositoryBody });
  const body = page.locator('.github-issue-body');

  const details = body.locator('details');
  assert.equal(await details.count(), 1, 'the collapsible section survives sanitizing');
  assert.equal(await details.locator('summary').innerText(), 'Build log');
  assert.equal(await details.evaluate(node => node.open), false, 'a closed section starts closed');
  await details.locator('summary').click();
  assert.equal(await details.evaluate(node => node.open), true, 'the section opens on click');
  assert.equal(await details.locator('strong').innerText(), 'detail', 'the collapsed body is rendered markdown');

  assert.deepEqual(await body.locator('a').evaluateAll(nodes => nodes.map(node => node.getAttribute('href'))), [
    'https://github.com/acme/repo/docs/guide.md',
    'https://github.com/acme/repo/pull/3',
    'https://github.com/acme/repo/#contract',
  ], 'relative hrefs point at GitHub, and so does a fragment opened on its own');
  assert.deepEqual(await body.locator('a').evaluateAll(nodes => nodes.map(node => node.target)), ['_blank', '_blank', ''],
    'only the links that leave the page open a new tab');
  assert.equal(await body.locator('img').getAttribute('src'),
    'https://raw.githubusercontent.com/acme/repo/main/docs/architecture.png',
    'a relative image uses repository default branch raw bytes');
});

test('a heading fragment scrolls inside the body while other fragments remain normal links', async t => {
  const page = await openIssue(t);
  const result = await page.evaluate(async () => {
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.appendChild(window.MarkdownView.sanitize(
      '<h2>Connector contract</h2><a href="#connector-contract">jump</a><a href="#missing">nowhere</a>',
      null, { baseURL: 'https://github.com/acme/repo/' },
    ));
    document.body.appendChild(host);
    const before = location.hash;
    const scrolled = [];
    host.querySelector('h2').scrollIntoView = () => scrolled.push('connector-contract');
    // Whether the body's own handler cancelled the click is read after it ran;
    // the guard then keeps any click the handler left alone from navigating the
    // test page away.
    const cancelled = [];
    const guard = (event) => { cancelled.push(event.defaultPrevented); event.preventDefault(); };
    document.addEventListener('click', guard);
    const anchors = Array.from(host.querySelectorAll('a'));
    anchors.forEach(node => node.click());
    const plainScrolled = scrolled.slice();
    // A modifier click asks for a new tab, which this body cannot be, so the
    // browser is left to follow the href instead.
    anchors[0].dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, ctrlKey: true }));
    document.removeEventListener('click', guard);
    const result = {
      hash: location.hash,
      unchanged: location.hash === before,
      scrolled: plainScrolled,
      scrolledAfterModifier: scrolled.length,
      cancelled,
      hrefs: anchors.map(node => node.getAttribute('href')),
      fragments: anchors.map(node => node.getAttribute('data-markdown-fragment')),
      rels: anchors.map(node => node.rel),
    };
    host.remove();
    return result;
  });

  assert.ok(result.unchanged, `clicking a fragment link must not rewrite the app hash, got ${result.hash}`);
  assert.deepEqual(result.scrolled, ['connector-contract'], 'the heading the fragment names is scrolled to, a missing one is a no-op');
  assert.deepEqual(result.cancelled, [true, false, false], 'only a matching heading click is handled here; other and modifier clicks are left to the browser');
  assert.equal(result.scrolledAfterModifier, 1, 'a modifier click does not also scroll this body');
  assert.deepEqual(result.hrefs, ['https://github.com/acme/repo/#connector-contract', 'https://github.com/acme/repo/#missing'],
    'a new-tab click lands on the page the markdown came from, never the app hash route');
  assert.deepEqual(result.fragments, ['connector-contract', 'missing'], 'the anchor the author named is kept for scrolling');
  assert.deepEqual(result.rels, ['noopener noreferrer', 'noopener noreferrer'], 'a new tab gets no handle on this page');
});

test('non-Latin heading fragments scroll to their matching heading', async t => {
  const page = await openIssue(t);
  const result = await page.evaluate(() => {
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.appendChild(window.MarkdownView.sanitize(
      '<h2>部署 概要</h2><a href="#%E9%83%A8%E7%BD%B2-%E6%A6%82%E8%A6%81">jump</a>',
      null, { baseURL: 'https://github.com/acme/repo/' },
    ));
    document.body.appendChild(host);
    const scrolled = [];
    host.querySelector('h2').scrollIntoView = () => scrolled.push('部署-概要');
    const event = new MouseEvent('click', { bubbles: true, cancelable: true });
    host.querySelector('a').dispatchEvent(event);
    host.remove();
    return { cancelled: event.defaultPrevented, scrolled };
  });

  assert.ok(result.cancelled, 'a matching Unicode heading is handled inside the body');
  assert.deepEqual(result.scrolled, ['部署-概要']);
});

test('a fragment link without a base page carries no href to follow', async t => {
  const page = await openIssue(t);
  const result = await page.evaluate(() => {
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.appendChild(window.MarkdownView.sanitize('<h2>Scope</h2><a href="#scope">jump</a>'));
    document.body.appendChild(host);
    const anchor = host.querySelector('a');
    const scrolled = [];
    host.querySelector('h2').scrollIntoView = () => scrolled.push('scope');
    const before = location.hash;
    anchor.click();
    const result = {
      href: anchor.getAttribute('href'),
      fragment: anchor.getAttribute('data-markdown-fragment'),
      unchanged: location.hash === before,
      scrolled,
      underlined: getComputedStyle(anchor).textDecorationLine,
    };
    host.remove();
    return result;
  });

  assert.equal(result.href, null, 'with no base page there is no safe target, and never the app hash route');
  assert.equal(result.fragment, 'scope');
  assert.ok(result.unchanged, 'the app hash is left alone');
  assert.deepEqual(result.scrolled, ['scope'], 'the fragment still scrolls inside the body');
  assert.equal(result.underlined, 'underline', 'the link still reads as a link without an href');
});

test('a fragment link without a base page is reachable and activated by keyboard', async t => {
  const page = await openIssue(t);
  const result = await page.evaluate(async () => {
    const host = document.createElement('div');
    host.className = 'markdown-body';
    host.appendChild(window.MarkdownView.sanitize('<h2>Scope</h2><a href="#scope">jump</a>'));
    document.body.appendChild(host);
    const anchor = host.querySelector('a');
    const scrolled = [];
    host.querySelector('h2').scrollIntoView = () => scrolled.push('scope');
    anchor.focus();
    const focused = document.activeElement === anchor;
    const press = (key) => {
      const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
      anchor.dispatchEvent(event);
      return event.defaultPrevented;
    };
    const result = {
      // The tabIndex property reads 0 on an anchor the browser will not focus,
      // so the attribute is what says the anchor joined the focus order.
      tabIndex: anchor.getAttribute('tabindex'),
      role: anchor.getAttribute('role'),
      focused,
      enter: press('Enter'),
      space: press(' '),
      ignored: press('a'),
      scrolled,
    };
    host.remove();
    return result;
  });

  assert.ok(result.focused, 'an anchor with no href only takes focus through an explicit tabindex');
  assert.equal(result.tabIndex, '0', 'the anchor joins the normal focus order');
  assert.equal(result.role, 'link', 'assistive technology is told it is still a link');
  assert.deepEqual([result.enter, result.space], [true, true], 'Enter and Space activate the link instead of scrolling the pane');
  assert.equal(result.ignored, false, 'other keys are left alone');
  assert.deepEqual(result.scrolled, ['scope', 'scope'], 'each activation scrolls to the heading the fragment names');
});

test('an image inside a link keeps the link the author wrote', async t => {
  const page = await openIssue(t, {
    body: [
      '[![badge](https://tracker.example.com/pixel.png)](https://example.com/build)',
      '',
      '[![](https://tracker.example.com/bare.png)](https://example.com/bare)',
      '',
      '[![shot](https://user-images.githubusercontent.com/1/shot.png)](https://example.com/shot)',
    ].join('\n'),
  });
  const body = page.locator('.github-issue-body');

  assert.equal(await body.locator('a a').count(), 0, 'a nested anchor would steal the click from the author\'s link');
  assert.deepEqual(await body.locator('a').evaluateAll(nodes => nodes.map(node => ({
    href: node.getAttribute('href'), text: node.textContent, images: node.querySelectorAll('img').length,
  }))), [
    { href: 'https://example.com/build', text: 'badge', images: 0 },
    { href: 'https://example.com/bare', text: 'https://tracker.example.com/bare.png', images: 0 },
    { href: 'https://example.com/shot', text: '', images: 1 },
  ], 'an unfetched image becomes the link text, and a GitHub-hosted one stays an image inside the link');
  assert.deepEqual(await page.evaluate(() => performance.getEntriesByType('resource')
    .map(entry => entry.name).filter(name => name.includes('tracker.example.com'))), [],
  'nothing is requested from the third-party host');
});

test('interactive container tags are unwrapped so no author text is lost', async t => {
  const page = await openIssue(t, {
    body: [
      '<form><label>Ship date</label><select><option>Friday</option></select>',
      '<textarea>draft note</textarea><button onclick="window.__xss = 1">go</button></form>',
      '',
      '<noscript>Enable JavaScript to see the chart.</noscript>',
    ].join('\n'),
  });
  const body = page.locator('.github-issue-body');
  const text = await body.innerText();

  for (const kept of ['Ship date', 'Friday', 'draft note', 'go', 'Enable JavaScript to see the chart.']) {
    assert.ok(text.includes(kept), `expected the body to keep ${JSON.stringify(kept)}, got ${JSON.stringify(text)}`);
  }
  assert.equal(await body.locator('form, select, textarea, button, noscript').count(), 0, 'the markup itself is still dropped');
  assert.equal(await body.locator('[onclick]').count(), 0, 'no handler survives the unwrapping');
  assert.equal(await page.evaluate(() => window.__xss || ''), '');
});

test('images load only from GitHub hosts and others stay links', async t => {
  const page = await openIssue(t, {
    body: [
      '![screenshot](https://user-images.githubusercontent.com/1/shot.png)',
      '',
      '![badge](https://tracker.example.com/pixel.png)',
      '',
      '![](https://tracker.example.com/bare.png)',
    ].join('\n'),
  });
  const body = page.locator('.github-issue-body');

  assert.deepEqual(await body.locator('img').evaluateAll(nodes => nodes.map(node => node.getAttribute('src'))),
    ['https://user-images.githubusercontent.com/1/shot.png'], 'only GitHub-hosted images are fetched');
  assert.deepEqual(await body.locator('a.markdown-remote-image').evaluateAll(nodes => nodes.map(node => ({
    href: node.getAttribute('href'), text: node.textContent, target: node.target, rel: node.rel,
  }))), [
    { href: 'https://tracker.example.com/pixel.png', text: 'badge', target: '_blank', rel: 'noopener noreferrer' },
    { href: 'https://tracker.example.com/bare.png', text: 'https://tracker.example.com/bare.png', target: '_blank', rel: 'noopener noreferrer' },
  ], 'a third-party image becomes a link the reader opens deliberately');
  // Page routing aborts every off-origin request, so a leaked fetch would show
  // up here as a request that was attempted at all.
  assert.deepEqual(await page.evaluate(() => performance.getEntriesByType('resource')
    .map(entry => entry.name).filter(name => name.includes('tracker.example.com'))), [],
  'nothing is requested from the third-party host');
});

test('relative links and images use their distinct repository bases', async t => {
  const page = await openIssue(t);
  const result = await page.evaluate(() => {
    const render = (options) => {
      const host = document.createElement('div');
      host.appendChild(window.MarkdownView.sanitize(
        '<a href="docs/guide.md">guide</a><img src="assets/shot.png" alt="shot">', null, options,
      ));
      return {
        href: host.querySelector('a').getAttribute('href'),
        src: host.querySelector('img')?.getAttribute('src') || null,
        text: host.textContent,
      };
    };
    return {
      none: render(undefined),
      relativeBase: render({ baseURL: 'docs/' }),
      unsafeBase: render({ baseURL: 'javascript:alert(1)' }),
      repository: render({ baseURL: 'https://github.com/acme/repo/' }),
      repositoryWithImages: render({
        baseURL: 'https://github.com/acme/repo/',
        imageBaseURL: 'https://raw.githubusercontent.com/acme/repo/main/',
      }),
    };
  });

  assert.deepEqual(result.none, { href: null, src: null, text: 'guideshot' }, 'without a base a relative URL has no safe target');
  assert.deepEqual(result.relativeBase, { href: null, src: null, text: 'guideshot' }, 'a relative base is not a base');
  assert.deepEqual(result.unsafeBase, { href: null, src: null, text: 'guideshot' }, 'an unsafe base is rejected before resolving');
  assert.deepEqual(result.repository, { href: 'https://github.com/acme/repo/docs/guide.md', src: null, text: 'guideshot' },
    'a repository page base resolves links but does not guess an image branch');
  assert.deepEqual(result.repositoryWithImages, {
    href: 'https://github.com/acme/repo/docs/guide.md',
    src: 'https://raw.githubusercontent.com/acme/repo/main/assets/shot.png',
    text: 'guide',
  }, 'a raw default-branch base resolves relative repository images');
});

test('a marked script element that already loaded does not leave rendering pending', async t => {
  const page = await browser.newPage();
  t.after(() => page.close());
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.goto(baseURL);
  await page.waitForFunction(() => Boolean(window.MarkdownView));

  const result = await page.evaluate(async () => {
    // A marked script element that finished loading before any body asked for
    // it: its load event has already fired and will never fire again.
    await new Promise((resolve, reject) => {
      const node = document.createElement('script');
      node.src = URL.createObjectURL(new Blob(['/* no marked here */'], { type: 'text/javascript' }));
      node.setAttribute('data-marked-script', 'true');
      node.addEventListener('load', resolve, { once: true });
      node.addEventListener('error', reject, { once: true });
      document.head.appendChild(node);
    });
    const host = document.createElement('div');
    document.body.appendChild(host);
    const rendered = await Promise.race([
      window.MarkdownView.render(host, 'keep **this**'),
      new Promise(resolve => { setTimeout(() => resolve('pending'), 5000); }),
    ]);
    return { rendered, html: host.innerHTML, pending: host.classList.contains('markdown-body-pending') };
  });

  assert.notEqual(result.rendered, 'pending', 'rendering settles instead of waiting on a load event that already fired');
  assert.equal(result.rendered, true);
  assert.ok(result.html.includes('<strong>this</strong>'), `expected rendered markdown, got ${result.html}`);
  assert.ok(!result.pending);
});

test('markdown HTML cannot execute script or keep javascript: links', async t => {
  const page = await openIssue(t, { body: hostileBody });
  const body = page.locator('.github-issue-body');

  assert.equal(await page.evaluate(() => window.__xss || ''), '');
  assert.equal(await body.locator('script').count(), 0);
  assert.equal(await body.locator('iframe').count(), 0);
  assert.deepEqual(await body.locator('img').evaluateAll(nodes => nodes.map(node => node.getAttribute('src'))),
    ['https://raw.githubusercontent.com/acme/repo/main/x'],
    'a relative image keeps only its safe raw-repository URL');
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
      '<p>keep <b>bold</b> <i>italic</i> <s>strike</s> <u>underline</u> <span>span</span> '
      + '<a href="https://example.com" title="t">link</a></p>'
      + '<table><caption>caption</caption><tfoot><tr><td>footer</td></tr></tfoot></table>'
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
      remoteImages: Array.from(host.querySelectorAll('a.markdown-remote-image')).map(node => node.getAttribute('href')),
      inputs: Array.from(host.querySelectorAll('input')).length,
      checkbox: { type: checkbox.type, checked: checkbox.checked, disabled: checkbox.disabled },
      codeClasses: Array.from(host.querySelectorAll('code')).map(node => node.getAttribute('class')),
    };
  });

  for (const tag of ['b', 'i', 's', 'u', 'span', 'caption', 'tfoot']) {
    assert.ok(result.tags.includes(tag), `safe raw-HTML tag ${tag} survives sanitizing`);
  }
  assert.deepEqual(result.link, { href: 'https://example.com', title: 't', target: '_blank', rel: 'noopener noreferrer' });
  assert.ok(!result.tags.includes('style') && !result.text.includes('display:none'), 'style blocks are dropped with their contents');
  assert.ok(!result.tags.includes('button') && !result.html.includes('onclick'), 'buttons are dropped with their handlers');
  assert.ok(result.text.includes('go'), 'the words inside a dropped container are still the author\'s');
  assert.deepEqual(result.images, [], 'no image is fetched from a host outside GitHub');
  assert.deepEqual(result.remoteImages, ['https://example.com/a.png'], 'an absolute http(s) image is offered as a link');
  assert.ok(result.text.includes('relative alt text'), 'a dropped image leaves its alt text behind');
  assert.equal(result.inputs, 1, 'only the task list checkbox survives');
  assert.deepEqual(result.checkbox, { type: 'checkbox', checked: true, disabled: true }, 'task list checkboxes stay read-only');
  assert.deepEqual(result.codeClasses, ['language-go', null], 'application classes cannot be borrowed by issue text');
});
