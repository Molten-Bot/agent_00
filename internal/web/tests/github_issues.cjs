// HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/github_issues.cjs
// Requires Playwright/Chromium. Every GitHub call is a fixture; no tasks are started.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';

// Fixed instants far enough in the past that formatRelativeTime falls back to an
// absolute date, so the assertions never depend on when the suite runs.
const createdAt = '2024-03-04T05:06:07Z';
const commentedAt = '2024-04-05T06:07:08Z';
const updatedAt = '2024-05-06T07:08:09Z';
const comment = { author: { login: 'hubot' }, createdAt: commentedAt, body: 'Looks right to me.' };

const assignedIssue = {
  number: 7, title: 'Move assignees and timestamps into the header', state: 'OPEN',
  url: 'https://github.com/example/project/issues/7', author: { login: 'octocat' },
  labels: [{ name: 'ui' }], assignees: [{ login: 'mona-octocat', name: 'Mona Octocat' }, { login: 'hubot' }],
  createdAt, updatedAt,
};
const unassignedIssue = { ...assignedIssue, number: 8, title: 'Nobody owns this yet', assignees: [] };
// Seven assignees, six of them short logins: the row the revealed names have
// the hardest time with. The long name on the last chip has to stay inside the
// pane, which clips horizontally, and the short ones have to reach back under
// the chips that opened them.
const crowdedIssue = {
  ...assignedIssue, number: 10,
  assignees: [
    { login: 'ana' }, { login: 'bo' }, { login: 'cy' }, { login: 'di' }, { login: 'ed' }, { login: 'fi' },
    { login: 'mariafernanda', name: 'Maria-Fernanda Gonzalez de la Vega Rodriguez Hernandez San Martin' },
  ],
};

// A 1x1 PNG stands in for the avatar github.com serves, so the icon path is
// exercised without the suite reaching the network.
const avatarPixel = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64');

let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

// openIssue loads the Studio issues panel in a known timezone and opens issue,
// so every rendered timestamp is the viewer's local time for a fixed instant.
// Avatars are the one request that leaves the app's origin: serving them is
// the normal case, aborting them is the offline case the initials cover.
async function openIssue(t, issue, { timezoneId = 'Asia/Tokyo', touch = false, avatars = false, onRequest = () => {} } = {}) {
  const viewport = touch ? { width: 430, height: 932 } : { width: 1280, height: 900 };
  const page = await browser.newPage({ viewport, timezoneId, locale: 'en-US', isMobile: touch, hasTouch: touch });
  page.setDefaultTimeout(10000);
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    onRequest(url);
    if (url.hostname === 'github.com' && url.pathname.endsWith('.png')) {
      return avatars ? route.fulfill({ contentType: 'image/png', body: avatarPixel }) : route.abort();
    }
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    let json = { ok: true };
    if (url.pathname === '/api/status') json = { tasks: [], events: [], repositories: [], stats: {}, resources: {} };
    if (url.pathname === '/api/agent-auth') json.auth = { required: false, ready: true, harness: 'codex', state: 'ready' };
    if (url.pathname === '/api/review-settings') json.settings = { review_level: 'off', github_issues_enabled: true, github_issue_link_mode: 'reference' };
    if (url.pathname === '/api/github/repos') json.repos = [{ name: 'project', full_name: 'example/project', private: false, default_branch: 'main' }];
    if (url.pathname === '/api/github/issues') json.issues = [issue];
    if (url.pathname === `/api/github/issues/${issue.number}`) {
      json.issue = {
        ...issue,
        body: issue.body ?? 'Example description.',
        comments: issue.comments ?? [comment],
      };
    }
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    return route.fulfill({ json });
  });
  await page.goto(`${baseURL}/#studio-issues`);
  await page.locator('#github-issues-repo option[value="example/project"]').waitFor({ state: 'attached' });
  await page.selectOption('#github-issues-repo', 'example/project');
  const first = page.locator('#github-issues-list .github-issue-list-item').first();
  if (touch) await first.tap(); else await first.click();
  await page.locator('.github-issue-detail-header .github-issue-metadata').waitFor();
  return page;
}

test('assignees and timestamps lead the issue header instead of trailing the description', async t => {
  const page = await openIssue(t, assignedIssue);
  assert.equal(await page.locator('.github-issue-detail-body .github-issue-metadata').count(), 0,
    'metadata must not stay below the description');
  const order = await page.evaluate(() => {
    const metadata = document.querySelector('.github-issue-metadata');
    const body = document.querySelector('.github-issue-body');
    return metadata.compareDocumentPosition(body) & Node.DOCUMENT_POSITION_FOLLOWING ? 'before' : 'after';
  });
  assert.equal(order, 'before', 'the header metadata renders ahead of the issue description');
  assert.deepEqual(await page.locator('.github-issue-metadata dt').allInnerTexts(), ['Assignees', 'Created', 'Updated']);

  const chips = page.locator('.github-issue-assignees .github-issue-avatar');
  assert.equal(await chips.count(), 2, 'each assignee gets an icon');
  assert.deepEqual(await chips.allInnerTexts(), ['MO', 'H']);
  assert.deepEqual(await chips.evaluateAll(nodes => nodes.map(node => node.getAttribute('aria-label'))),
    ['mona-octocat (Mona Octocat)', 'hubot']);
  assert.equal(await page.locator('.github-issue-assignee-empty').count(), 0);
});

test('issue descriptions and comments render sanitized GitHub-flavored Markdown', async t => {
  let untrustedImageRequests = 0;
  const markdownIssue = {
    ...assignedIssue,
    number: 11,
    body: [
      '## Rendered heading',
      '',
      '- **bold item**',
      '- `inline code`',
      '',
      '| Name | Value |',
      '| --- | --- |',
      '| Marked | works |',
      '',
      '[safe link](https://marked.js.org/)',
      '[unsafe link](javascript:window.__issueXSS=true)',
      '<script>window.__issueXSS = true</script>',
      '<img src="javascript:window.__issueXSS=true" alt="unsafe image">',
      '',
      '![external image](https://attacker.example/tracking.png)',
      '',
      '![GitHub attachment](https://github.com/user-attachments/assets/12345678-1234-1234-1234-123456789abc)',
    ].join('\n'),
    comments: [{ ...comment, body: 'Comment with *emphasis* and ~~deletion~~.' }],
  };
  const page = await openIssue(t, markdownIssue, {
    onRequest: url => { if (url.hostname === 'attacker.example') untrustedImageRequests++; },
  });
  const body = page.locator('.github-issue-body');
  assert.equal(await body.locator('h2').innerText(), 'Rendered heading');
  assert.deepEqual(await body.locator('li').allInnerTexts(), ['bold item', 'inline code']);
  assert.equal(await body.locator('strong').innerText(), 'bold item');
  assert.equal(await body.locator('code').innerText(), 'inline code');
  assert.deepEqual(await body.locator('table tbody td').allInnerTexts(), ['Marked', 'works']);
  assert.equal(await body.getByText('safe link', { exact: true }).getAttribute('href'), 'https://marked.js.org/');
  assert.equal(await body.getByText('safe link', { exact: true }).getAttribute('rel'), 'noopener noreferrer');
  assert.equal(await body.getByText('unsafe link', { exact: true }).getAttribute('href'), null);
  assert.equal(await body.locator('script').count(), 0);
  assert.equal(await body.getByAltText('external image').count(), 0);
  assert.match(await body.innerText(), /external image/);
  assert.equal(untrustedImageRequests, 0, 'untrusted Markdown images must not trigger browser requests');
  assert.equal(await body.getByAltText('GitHub attachment').getAttribute('src'),
    'https://github.com/user-attachments/assets/12345678-1234-1234-1234-123456789abc');
  assert.equal(await page.evaluate(() => window.__issueXSS), undefined);

  const renderedComment = page.locator('.github-issue-comment-body');
  assert.equal(await renderedComment.locator('em').innerText(), 'emphasis');
  assert.equal(await renderedComment.locator('del').innerText(), 'deletion');
});

test('assignee icons are the GitHub avatars, with initials only as the fallback', async t => {
  const page = await openIssue(t, assignedIssue, { avatars: true });
  const images = page.locator('.github-issue-assignees .github-issue-avatar img');
  await images.nth(1).waitFor();
  assert.deepEqual(await images.evaluateAll(nodes => nodes.map(node => node.src)),
    ['https://github.com/mona-octocat.png?size=80', 'https://github.com/hubot.png?size=80']);
  assert.deepEqual(await images.evaluateAll(nodes => nodes.map(node => node.alt)), ['', ''],
    'the chip already names the person, so its picture is decorative');
  // Two logins can share initials, which is why the picture has to be what is
  // actually drawn: it covers the chip rather than sitting beside the letters.
  const covered = await page.locator('.github-issue-assignees .github-issue-avatar').first().evaluate(chip => {
    const chipBox = chip.getBoundingClientRect();
    const imageBox = chip.querySelector('img').getBoundingClientRect();
    return imageBox.width >= chipBox.width && imageBox.height >= chipBox.height
      && Math.abs(imageBox.left - chipBox.left) < 1 && Math.abs(imageBox.top - chipBox.top) < 1;
  });
  assert.equal(covered, true, 'the avatar does not fill its chip, so the initials still show through');

  // The assignee icons are the ask, and they are the only place an icon stands
  // in for a name. The byline and the comments spell the login out beside
  // their initials, so fetching a picture there would tell github.com who is
  // reading the issue and show nothing the reader did not already have.
  assert.equal(await page.locator('.github-issue-author img, .github-issue-comment-meta img').count(), 0,
    'only the assignee chips should reach github.com for a picture');
});

test('an avatar that cannot be loaded falls back to the initials', async t => {
  const page = await openIssue(t, assignedIssue);
  await page.waitForFunction(
    () => document.querySelectorAll('.github-issue-assignees .github-issue-avatar img').length === 0,
    null, { timeout: 10000 });
  assert.deepEqual(await page.locator('.github-issue-assignees .github-issue-avatar').allInnerTexts(), ['MO', 'H']);
});

test('assignee icons are buttons and tapping one reveals its name on touch', async t => {
  const page = await openIssue(t, assignedIssue, { touch: true });
  const chip = page.locator('.github-issue-assignees .github-issue-avatar').first();
  const label = page.locator('.github-issue-assignee-label');
  assert.equal(await chip.evaluate(node => node.tagName), 'BUTTON');
  assert.equal(await chip.getAttribute('type'), 'button');
  assert.equal(await label.isHidden(), true);
  await chip.tap();
  assert.equal(await label.innerText(), 'mona-octocat (Mona Octocat)');
  assert.equal(await chip.getAttribute('aria-expanded'), 'true');
});

test('hovering an assignee icon reveals its name', async t => {
  const page = await openIssue(t, assignedIssue);
  const chip = page.locator('.github-issue-assignees .github-issue-avatar').first();
  const label = page.locator('.github-issue-assignee-label');
  assert.equal(await label.isHidden(), true);
  await chip.hover();
  assert.equal(await label.innerText(), 'mona-octocat (Mona Octocat)');
  assert.equal(await chip.getAttribute('aria-expanded'), 'true');
  await label.hover();
  assert.equal(await label.innerText(), 'mona-octocat (Mona Octocat)',
    'moving from the icon onto its revealed name must keep the name visible');
  assert.equal(await chip.getAttribute('aria-expanded'), 'true');
  await page.locator('.github-issue-detail-title').hover();
  assert.equal(await label.isHidden(), true);
  assert.equal(await chip.getAttribute('aria-expanded'), 'false');
});

test('hovering another assignee does not replace the focused assignee name', async t => {
  const page = await openIssue(t, assignedIssue);
  const chips = page.locator('.github-issue-assignees .github-issue-avatar');
  const label = page.locator('.github-issue-assignee-label');
  await chips.first().focus();
  await chips.nth(1).hover();
  assert.equal(await label.innerText(), 'mona-octocat (Mona Octocat)');
  assert.deepEqual(await chips.evaluateAll(nodes => nodes.map(node => node.getAttribute('aria-expanded'))),
    ['true', 'false']);
});

test('wrapped assignee icons reveal one in-flow name inside a narrow pane', async t => {
  const page = await openIssue(t, crowdedIssue, { touch: true });
  const chips = page.locator('.github-issue-assignees .github-issue-avatar');
  const count = await chips.count();
  assert.equal(count, 7, 'the crowded issue should render every assignee');
  await page.locator('.github-issue-assignees').evaluate(row => { row.style.width = '74px'; });
  const last = count - 1;
  await chips.nth(last).tap();
  const bounds = await page.evaluate(() => {
    const row = document.querySelector('.github-issue-assignees').getBoundingClientRect();
    const icons = [...document.querySelectorAll('.github-issue-assignees .github-issue-avatar')].map(node => node.getBoundingClientRect());
    const label = document.querySelector('.github-issue-assignee-label').getBoundingClientRect();
    return {
      wrapped: new Set(icons.map(box => Math.round(box.top))).size > 1,
      labelLeft: label.left, labelRight: label.right, rowLeft: row.left, rowRight: row.right,
      labelTop: label.top, lastIconBottom: icons.at(-1).bottom,
    };
  });
  assert.equal(bounds.wrapped, true, 'fixture must wrap icons to cover narrow layout');
  assert.ok(bounds.labelLeft >= bounds.rowLeft && bounds.labelRight <= bounds.rowRight,
    'revealed name must remain inside assignee row');
  assert.ok(bounds.labelTop >= bounds.lastIconBottom, 'revealed name must follow wrapped icon rows');
});

test('Escape dismisses the assignee name without losing focus or blocking page handlers', async t => {
  const page = await openIssue(t, assignedIssue);
  const chips = page.locator('.github-issue-assignees .github-issue-avatar');
  await chips.first().focus();
  assert.equal(await page.locator('.github-issue-assignee-label').innerText(), 'mona-octocat (Mona Octocat)');
  await page.evaluate(() => {
    window.escapesSeenByDocument = 0;
    document.addEventListener('keydown', event => {
      if (event.key === 'Escape') window.escapesSeenByDocument++;
    });
  });
  await page.keyboard.press('Escape');
  assert.equal(await page.locator('.github-issue-assignee-label').isHidden(), true);
  assert.equal(await chips.first().evaluate(node => node === document.activeElement), true,
    'Escape must preserve the keyboard tab position');
  assert.equal(await page.evaluate(() => window.escapesSeenByDocument), 1,
    'the chip swallows Escape, so whatever the page has open stays open');
  assert.equal(await page.locator('.github-issue-detail-header .github-issue-metadata').count(), 1);
  assert.deepEqual(await chips.evaluateAll(nodes => nodes.map(node => node.tagName)), ['BUTTON', 'BUTTON']);
});

test('an issue with nobody assigned says so instead of rendering an empty row', async t => {
  const page = await openIssue(t, unassignedIssue);
  assert.equal(await page.locator('.github-issue-assignees .github-issue-avatar').count(), 0);
  assert.equal(await page.locator('.github-issue-assignee-empty').innerText(), 'Unassigned');
});

test('changing an issue filter shows a loading icon while results update', async t => {
  const page = await openIssue(t, assignedIssue);
  let releaseResponse;
  let markRequestStarted;
  const requestStarted = new Promise(resolve => { markRequestStarted = resolve; });
  await page.route('**/api/github/issues?**', async route => {
    markRequestStarted();
    await new Promise(resolve => { releaseResponse = resolve; });
    await route.fulfill({ json: { ok: true, issues: [] } });
  });

  await page.locator('[data-branch-filter="with"]').click();
  await requestStarted;
  const list = page.locator('#github-issues-list');
  const loading = list.locator('.github-issues-loading');
  assert.equal(await list.getAttribute('aria-busy'), 'true');
  assert.equal(await loading.getAttribute('aria-label'), 'Loading issues');
  assert.equal(await loading.locator('.task-action-glyph-loader').count(), 1);
  assert.equal(await page.locator('#github-issues-count').innerText(), '…');

  releaseResponse();
  await page.waitForFunction(() => document.getElementById('github-issues-list').getAttribute('aria-busy') === 'false');
  assert.equal(await loading.count(), 0);
  assert.equal(await page.locator('#github-issues-count').innerText(), '0');
});

test('header, list, and byline times read in the viewer timezone', async t => {
  const tokyo = await openIssue(t, assignedIssue, { timezoneId: 'Asia/Tokyo' });
  const headerTimes = tokyo.locator('.github-issue-metadata time');
  assert.deepEqual(await headerTimes.evaluateAll(nodes => nodes.map(node => node.dateTime)),
    ['2024-03-04T05:06:07.000Z', '2024-05-06T07:08:09.000Z']);
  // The header reads in the same relative form as the rest of the issue view,
  // which is the form this panel already used. Only the viewer's timezone
  // decides the calendar day.
  assert.deepEqual(await headerTimes.allInnerTexts(), ['Mar 4, 2024', 'May 6, 2024']);
  // 05:06 UTC is 14:06 in Tokyo, and the clock time behind the relative one
  // stays in the tooltip rather than taking a line of its own.
  assert.match((await headerTimes.evaluateAll(nodes => nodes.map(node => node.title))).join(' | '),
    /Mar 4, 2024, 2:06\sPM \| May 6, 2024, 4:08\sPM/);

  // The list's "updated" line is the same instant and carries the same
  // treatment.
  const listTime = tokyo.locator('.github-issue-list-meta time').first();
  assert.equal(await listTime.evaluate(node => node.dateTime), '2024-05-06T07:08:09.000Z');
  assert.match(await listTime.evaluate(node => node.title), /May 6, 2024, 4:08\sPM/);
  assert.match(await tokyo.locator('.github-issue-list-meta').first().innerText(), /updated May 6, 2024/);

  // Comment times are the same kind of value, so they carry the same treatment.
  const commentTime = tokyo.locator('.github-issue-comment-meta time');
  assert.equal(await commentTime.count(), 1, 'each comment timestamp is a <time>');
  assert.equal(await commentTime.evaluate(node => node.dateTime), '2024-04-05T06:07:08.000Z');
  assert.match(await commentTime.evaluate(node => node.title), /Apr 5, 2024, 3:07\sPM/);

  // The byline would otherwise repeat the Created value already in the header.
  assert.equal((await tokyo.locator('.github-issue-author > span:not(.github-issue-avatar)').innerText()).trim(), 'opened this issue');
  assert.equal(await tokyo.locator('.github-issue-author time').count(), 0);

  const losAngeles = await openIssue(t, assignedIssue, { timezoneId: 'America/Los_Angeles' });
  assert.deepEqual(await losAngeles.locator('.github-issue-metadata time').allInnerTexts(),
    ['Mar 3, 2024', 'May 6, 2024']);
});

test('a timestamp the browser cannot parse stays out of <time>, which needs a machine-readable value', async t => {
  const page = await openIssue(t, { ...assignedIssue, number: 9, updatedAt: 'sometime last week' });
  const updated = page.locator('.github-issue-metadata div').last();
  assert.equal((await updated.locator('dt').innerText()).trim(), 'Updated');
  assert.equal((await updated.locator('dd').innerText()).trim(), 'sometime last week');
  assert.equal(await updated.locator('time').count(), 0, 'an empty datetime would be invalid markup');
  assert.equal(await page.locator('.github-issue-metadata time').count(), 1, 'the parsable Created value is still a <time>');
});
