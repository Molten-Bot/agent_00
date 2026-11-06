// HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/github_issues.cjs
// Requires Playwright/Chromium. Every GitHub call is a fixture; no tasks are started.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';

// Fixed instants far enough in the past that formatRelativeTime falls back to an
// absolute date, so the assertions never depend on when the suite runs.
const createdAt = '2024-03-04T05:06:07Z';
const updatedAt = '2024-05-06T07:08:09Z';
const assignedIssue = {
  number: 7, title: 'Move assignees and timestamps into the header', state: 'OPEN',
  url: 'https://github.com/example/project/issues/7', author: { login: 'octocat' },
  labels: [{ name: 'ui' }], assignees: [{ login: 'mona-octocat', name: 'Mona Octocat' }, { login: 'hubot' }],
  createdAt, updatedAt,
};
const unassignedIssue = { ...assignedIssue, number: 8, title: 'Nobody owns this yet', assignees: [] };

let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

// openIssue loads the Studio issues panel in a known timezone and opens issue,
// so every rendered timestamp is the viewer's local time for a fixed instant.
async function openIssue(t, issue, { timezoneId = 'Asia/Tokyo', touch = false } = {}) {
  const viewport = touch ? { width: 430, height: 932 } : { width: 1280, height: 900 };
  const page = await browser.newPage({ viewport, timezoneId, locale: 'en-US', isMobile: touch, hasTouch: touch });
  page.setDefaultTimeout(10000);
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    let json = { ok: true };
    if (url.pathname === '/api/status') json = { tasks: [], events: [], repositories: [], stats: {}, resources: {} };
    if (url.pathname === '/api/agent-auth') json.auth = { required: false, ready: true, harness: 'codex', state: 'ready' };
    if (url.pathname === '/api/review-settings') json.settings = { review_level: 'off', github_issues_enabled: true, github_issue_link_mode: 'reference' };
    if (url.pathname === '/api/github/repos') json.repos = [{ name: 'project', full_name: 'example/project', private: false, default_branch: 'main' }];
    if (url.pathname === '/api/github/issues') json.issues = [issue];
    if (url.pathname === `/api/github/issues/${issue.number}`) json.issue = { ...issue, body: 'Example description.', comments: [] };
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

// tooltipShown waits for the chip's revealed name, which fades in over a
// transition, and reports where it landed so a clipped tooltip still fails.
async function tooltipShown(page) {
  await page.waitForFunction(() => {
    const node = document.querySelector('.github-issue-assignees .github-issue-avatar');
    return getComputedStyle(node, '::after').opacity === '1';
  });
  return page.evaluate(() => {
    const pane = document.querySelector('.github-issue-detail').getBoundingClientRect();
    const chip = document.querySelector('.github-issue-assignees .github-issue-avatar').getBoundingClientRect();
    return { paneLeft: pane.left, paneRight: pane.right, chipLeft: chip.left };
  });
}

test('tapping an assignee icon names it, which a title tooltip never does on touch', async t => {
  const page = await openIssue(t, assignedIssue, { touch: true });
  const chip = page.locator('.github-issue-assignees .github-issue-avatar').first();
  assert.equal(await chip.getAttribute('data-label'), 'mona-octocat (Mona Octocat)');
  assert.equal(await chip.getAttribute('title'), null, 'a title tooltip is invisible to touch users');
  assert.equal(await chip.evaluate(node => getComputedStyle(node, '::after').opacity), '0');
  // tap() sends touch events only, so nothing here can be satisfied by :hover.
  await chip.tap();
  const box = await tooltipShown(page);
  assert.ok(box.chipLeft >= box.paneLeft, 'the named chip stays inside the issue pane');
});

test('hovering an assignee icon names it without clipping against the pane edge', async t => {
  const page = await openIssue(t, assignedIssue);
  const chip = page.locator('.github-issue-assignees .github-issue-avatar').first();
  await chip.hover();
  const box = await tooltipShown(page);
  const width = await chip.evaluate(node => parseFloat(getComputedStyle(node, '::after').width));
  assert.ok(box.chipLeft + width <= box.paneRight, 'the name fits inside the issue pane');
});

test('an issue with nobody assigned says so instead of rendering an empty row', async t => {
  const page = await openIssue(t, unassignedIssue);
  assert.equal(await page.locator('.github-issue-assignees .github-issue-avatar').count(), 0);
  assert.equal(await page.locator('.github-issue-assignee-empty').innerText(), 'Unassigned');
});

test('header, list, and byline times read in the viewer timezone', async t => {
  const tokyo = await openIssue(t, assignedIssue, { timezoneId: 'Asia/Tokyo' });
  const headerTimes = tokyo.locator('.github-issue-metadata time');
  assert.deepEqual(await headerTimes.evaluateAll(nodes => nodes.map(node => node.dateTime)),
    ['2024-03-04T05:06:07.000Z', '2024-05-06T07:08:09.000Z']);
  assert.deepEqual(await headerTimes.evaluateAll(nodes => nodes.map(node => node.title)),
    ['Mar 4, 2024, 2:06 PM', 'May 6, 2024, 4:08 PM']);

  // The list's "updated" line is the same instant and needs the same treatment.
  const listTime = tokyo.locator('.github-issue-list-meta time').first();
  assert.equal(await listTime.evaluate(node => node.dateTime), '2024-05-06T07:08:09.000Z');
  assert.equal(await listTime.evaluate(node => node.title), 'May 6, 2024, 4:08 PM');
  assert.match(await tokyo.locator('.github-issue-list-meta').first().innerText(), /updated May 6, 2024/);

  // The byline would otherwise repeat the Created value already in the header.
  assert.equal((await tokyo.locator('.github-issue-author span:not(.github-issue-avatar)').innerText()).trim(), 'opened this issue');
  assert.equal(await tokyo.locator('.github-issue-author time').count(), 0);

  const losAngeles = await openIssue(t, assignedIssue, { timezoneId: 'America/Los_Angeles' });
  assert.deepEqual(await losAngeles.locator('.github-issue-metadata time').evaluateAll(nodes => nodes.map(node => node.title)),
    ['Mar 3, 2024, 9:06 PM', 'May 6, 2024, 12:08 AM']);
});
