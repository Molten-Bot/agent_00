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
async function openIssue(t, issue, { timezoneId = 'Asia/Tokyo', touch = false, avatars = false } = {}) {
  const viewport = touch ? { width: 430, height: 932 } : { width: 1280, height: 900 };
  const page = await browser.newPage({ viewport, timezoneId, locale: 'en-US', isMobile: touch, hasTouch: touch });
  page.setDefaultTimeout(10000);
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
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

// tooltipShown waits for the chip's revealed name, which fades in over a
// transition, and reports where it landed so a clipped tooltip still fails.
async function tooltipShown(page, index = 0) {
  await page.waitForFunction(i => {
    const node = document.querySelectorAll('.github-issue-assignees .github-issue-avatar')[i];
    return getComputedStyle(node, '::after').opacity === '1';
  }, index);
  return page.evaluate(i => {
    const pane = document.querySelector('.github-issue-detail').getBoundingClientRect();
    const chip = document.querySelectorAll('.github-issue-assignees .github-issue-avatar')[i];
    const style = getComputedStyle(chip, '::after');
    const px = value => parseFloat(value) || 0;
    // The name is a ::after with no rect of its own, so measure it the way the
    // browser places it: against whichever ancestor is its containing block,
    // which is the nearest positioned one. Deriving that here instead of
    // assuming it keeps this honest if the anchor ever moves back to the chip.
    let anchor = chip;
    while (anchor && getComputedStyle(anchor).position === 'static') anchor = anchor.parentElement;
    const base = (anchor || document.documentElement).getBoundingClientRect();
    // box-sizing does not reach generated content, so the painted size is the
    // content size plus padding and border.
    const width = px(style.width) + px(style.paddingLeft) + px(style.paddingRight)
      + px(style.borderLeftWidth) + px(style.borderRightWidth);
    const height = px(style.height) + px(style.paddingTop) + px(style.paddingBottom)
      + px(style.borderTopWidth) + px(style.borderBottomWidth);
    const left = base.left + px(style.left);
    const top = base.top + px(style.top);
    return {
      paneLeft: pane.left, paneRight: pane.right,
      chipLeft: chip.getBoundingClientRect().left,
      nameLeft: left, nameRight: left + width,
      nameTop: top, nameBottom: top + height,
    };
  }, index);
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

  // The pane clips horizontally, so a name hung off a later chip would be cut
  // in half. Every chip's name is anchored to the row for that reason.
  const last = page.locator('.github-issue-assignees .github-issue-avatar').last();
  await last.hover();
  const lastBox = await tooltipShown(page, 1);
  assert.ok(lastBox.nameLeft >= lastBox.paneLeft && lastBox.nameRight <= lastBox.paneRight,
    `the second chip's name spans ${lastBox.nameLeft}-${lastBox.nameRight} but the pane spans ${lastBox.paneLeft}-${lastBox.paneRight}`);
});

// Every chip, not just the first: the names share the row's anchor, so a short
// one on a chip further right is the case that can end up with nothing under
// the chip for the pointer to move onto. crowdedIssue carries exactly that,
// six short logins and one long one.
test('the revealed name stays up while the pointer rests on it, for every icon', async t => {
  const page = await openIssue(t, crowdedIssue);
  const chips = page.locator('.github-issue-assignees .github-issue-avatar');
  const count = await chips.count();
  assert.equal(count, 7, 'the crowded issue should render every assignee');
  for (let index = 0; index < count; index++) {
    const chip = chips.nth(index);
    await chip.hover();
    const box = await tooltipShown(page, index);
    // WCAG 1.4.13 hoverable: content a hover reveals must survive the pointer
    // moving onto it, which rules out a gap to cross or pointer-events: none.
    // Straight down out of the chip is the path a pointer actually takes, and
    // the one a name that does not reach the chip fails.
    const chipBox = await chip.boundingBox();
    const x = chipBox.x + chipBox.width / 2;
    assert.ok(x > box.nameLeft && x < box.nameRight,
      `chip ${index} sits at ${x}, outside the ${box.nameLeft}-${box.nameRight} its name occupies`);
    await page.mouse.move(x, (box.nameTop + box.nameBottom) / 2);
    // Longer than the .2s fade, so a name that starts disappearing is caught.
    await page.waitForTimeout(400);
    assert.equal(await chip.evaluate(node => getComputedStyle(node, '::after').opacity), '1',
      `the name for icon ${index} vanishes as soon as the pointer reaches it`);
  }
});

test('hovering one icon while another holds focus draws a single name', async t => {
  const page = await openIssue(t, assignedIssue);
  const chips = page.locator('.github-issue-assignees .github-issue-avatar');
  // Every name is anchored to the row, so two shown at once would land on the
  // same spot and read as one unparsable line.
  await chips.nth(1).focus();
  await tooltipShown(page, 1);
  await chips.first().hover();
  await tooltipShown(page, 0);
  await page.waitForFunction(() => {
    const nodes = document.querySelectorAll('.github-issue-assignees .github-issue-avatar');
    return getComputedStyle(nodes[1], '::after').opacity === '0';
  });
  assert.equal(await chips.nth(1).evaluate(node => node === document.activeElement), true,
    'the focus itself stays put; only the name it was holding open gives way');
});

test('a name a tap left open does not swallow taps on the issue below it', async t => {
  const page = await openIssue(t, assignedIssue, { touch: true });
  const chip = page.locator('.github-issue-assignees .github-issue-avatar').first();
  await chip.tap();
  const box = await tooltipShown(page);
  // A tap focuses the chip and the name stays up until that focus moves, so
  // the strip it paints sits over the header and description. Whatever is
  // underneath has to keep receiving the taps aimed at it.
  const hit = await page.evaluate(point => {
    const node = document.elementFromPoint(point.x, point.y);
    const chip = document.querySelector('.github-issue-assignees .github-issue-avatar');
    return { isChip: node === chip, tag: node?.tagName || 'none' };
  }, { x: (box.nameLeft + box.nameRight) / 2, y: (box.nameTop + box.nameBottom) / 2 });
  assert.equal(hit.isChip, false, `the name intercepts taps meant for the ${hit.tag} under it`);
});

test('a long name on the last chip stays inside the pane on a phone screen', async t => {
  const page = await openIssue(t, crowdedIssue, { touch: true });
  const chips = page.locator('.github-issue-assignees .github-issue-avatar');
  const last = await chips.count() - 1;
  await chips.nth(last).tap();
  const box = await tooltipShown(page, last);
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
  // The page closes its menus and modals from a document-level Escape handler,
  // so the chip must let the key through rather than stop it at itself.
  await page.evaluate(() => {
    window.escapesSeenByDocument = 0;
    document.addEventListener('keydown', event => {
      if (event.key === 'Escape') window.escapesSeenByDocument++;
    });
  });
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => {
    const node = document.querySelector('.github-issue-assignees .github-issue-avatar');
    return getComputedStyle(node, '::after').opacity === '0' && document.activeElement !== node;
  });
  assert.equal(await page.evaluate(() => window.escapesSeenByDocument), 1,
    'the chip swallows Escape, so whatever the page has open stays open');
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
