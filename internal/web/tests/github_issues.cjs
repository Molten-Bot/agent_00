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

// Which space en-US puts before AM/PM is an ICU build detail: ICU 72 switched
// it to U+202F and later Chromium builds went back to U+0020. The assertions
// below fold both to a plain space so they hold on either Chromium.
const plainSpaces = value => String(value ?? '').replace(/[\u202f\u00a0\u2009]/g, ' ');
const assignedIssue = {
  number: 7, title: 'Move assignees and timestamps into the header', state: 'OPEN',
  url: 'https://github.com/example/project/issues/7', author: { login: 'octocat' },
  labels: [{ name: 'ui' }], assignees: [{ login: 'mona-octocat', name: 'Mona Octocat' }, { login: 'hubot' }],
  createdAt, updatedAt,
};
const unassignedIssue = { ...assignedIssue, number: 8, title: 'Nobody owns this yet', assignees: [] };
// A name long enough that an unwrapped tooltip would run past the pane's right
// edge on a phone, which is the case the revealed name has to survive.
const longNameIssue = {
  ...assignedIssue, number: 10,
  assignees: [{ login: 'mariafernanda', name: 'Maria-Fernanda Gonzalez de la Vega Rodriguez Hernandez San Martin' }],
};

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
    if (url.pathname === `/api/github/issues/${issue.number}`) json.issue = { ...issue, body: 'Example description.', comments: [comment] };
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
    const chip = document.querySelector('.github-issue-assignees .github-issue-avatar');
    const box = chip.getBoundingClientRect();
    // The name is a ::after anchored at the chip's left edge, so it has no
    // rect of its own; its width plus that edge is where it actually ends.
    const width = parseFloat(getComputedStyle(chip, '::after').width);
    return { paneLeft: pane.left, paneRight: pane.right, chipLeft: box.left, nameRight: box.left + width };
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
  assert.ok(box.nameRight <= box.paneRight, 'the name fits inside the issue pane');
});

test('a long assignee name wraps inside the pane instead of running off a phone screen', async t => {
  const page = await openIssue(t, longNameIssue, { touch: true });
  await page.locator('.github-issue-assignees .github-issue-avatar').first().tap();
  const box = await tooltipShown(page);
  assert.ok(box.chipLeft >= box.paneLeft, 'the named chip stays inside the issue pane');
  assert.ok(box.nameRight <= box.paneRight,
    `the name ends at ${box.nameRight} but the pane ends at ${box.paneRight}`);
});

test('Escape dismisses the name a focused assignee icon is holding open', async t => {
  const page = await openIssue(t, assignedIssue);
  const chips = page.locator('.github-issue-assignees .github-issue-avatar');
  // Content revealed on hover must be keyboard reachable and dismissable
  // without moving the pointer (WCAG 1.4.13), so the chip is a tab stop.
  await chips.first().focus();
  await tooltipShown(page);
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => {
    const node = document.querySelector('.github-issue-assignees .github-issue-avatar');
    return getComputedStyle(node, '::after').opacity === '0' && document.activeElement !== node;
  });
  // Escape only drops the chip's own focus; it must not close the panel.
  assert.equal(await page.locator('.github-issue-detail-header .github-issue-metadata').count(), 1);
  assert.deepEqual(await chips.evaluateAll(nodes => nodes.map(node => node.tabIndex)), [0, 0]);
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
  assert.deepEqual((await headerTimes.evaluateAll(nodes => nodes.map(node => node.title))).map(plainSpaces),
    ['Mar 4, 2024, 2:06 PM', 'May 6, 2024, 4:08 PM']);

  // The list's "updated" line is the same instant and needs the same treatment.
  const listTime = tokyo.locator('.github-issue-list-meta time').first();
  assert.equal(await listTime.evaluate(node => node.dateTime), '2024-05-06T07:08:09.000Z');
  assert.equal(plainSpaces(await listTime.evaluate(node => node.title)), 'May 6, 2024, 4:08 PM');
  assert.match(await tokyo.locator('.github-issue-list-meta').first().innerText(), /updated May 6, 2024/);

  // Comment times are the same kind of value, so they carry the same treatment.
  const commentTime = tokyo.locator('.github-issue-comment-meta time');
  assert.equal(await commentTime.count(), 1, 'each comment timestamp is a <time>');
  assert.equal(await commentTime.evaluate(node => node.dateTime), '2024-04-05T06:07:08.000Z');
  assert.equal(plainSpaces(await commentTime.evaluate(node => node.title)), 'Apr 5, 2024, 3:07 PM');

  // The byline would otherwise repeat the Created value already in the header.
  assert.equal((await tokyo.locator('.github-issue-author span:not(.github-issue-avatar)').innerText()).trim(), 'opened this issue');
  assert.equal(await tokyo.locator('.github-issue-author time').count(), 0);

  const losAngeles = await openIssue(t, assignedIssue, { timezoneId: 'America/Los_Angeles' });
  assert.deepEqual((await losAngeles.locator('.github-issue-metadata time').evaluateAll(nodes => nodes.map(node => node.title))).map(plainSpaces),
    ['Mar 3, 2024, 9:06 PM', 'May 6, 2024, 12:08 AM']);
});

test('a timestamp the browser cannot parse stays out of <time>, which needs a machine-readable value', async t => {
  const page = await openIssue(t, { ...assignedIssue, number: 9, updatedAt: 'sometime last week' });
  const updated = page.locator('.github-issue-metadata div').last();
  assert.equal((await updated.locator('dt').innerText()).trim(), 'Updated');
  assert.equal((await updated.locator('dd').innerText()).trim(), 'sometime last week');
  assert.equal(await updated.locator('time').count(), 0, 'an empty datetime would be invalid markup');
  assert.equal(await page.locator('.github-issue-metadata time').count(), 1, 'the parsable Created value is still a <time>');
});
