// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/settings_tabs.cjs
// Uses the real embedded page; API writes are intercepted and never touch user settings.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });
const defaults = { auto_merge: false, delete_merged_branches: false, merge_method: 'squash', review_level: 'off', default_library_enabled: true, chat_enabled: true, json_enabled: false };
async function openSettings(t, { viewport = { width: 1280, height: 900 }, theme = 'dark', configurePage } = {}) {
  const page = await browser.newPage({ viewport });
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.addInitScript(theme => localStorage.setItem('hubui.theme', theme), theme);
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.route('**/api/agent-auth**', route => route.fulfill({ json: { ok: true, auth: { required: true, ready: true, harness: 'claude', state: 'ready' } } }));
  let settings = { ...defaults };
  const writes = [];
  await page.route('**/api/review-settings', route => {
    if (route.request().method() === 'POST') {
      settings = { ...settings, ...route.request().postDataJSON() };
      writes.push({ ...settings });
    }
    return route.fulfill({ json: { ok: true, settings } });
  });
  await page.route('**/api/commit-signing**', route => route.fulfill({ json: { ok: true, settings: { configured: false, enabled: false } } }));
  await page.route('**/api/library/sources', route => route.fulfill({ json: { ok: true, sources: [] } }));
  if (configurePage) await configurePage(page);
  await page.goto(`${baseURL}/#settings`);
  await page.waitForFunction(() => !document.getElementById('review-settings-close').disabled);
  return { page, writes };
}
async function select(page, name) {
  await page.locator(`#settings-tab-${name}`).click();
  await page.waitForFunction(name => document.getElementById(`settings-tab-${name}`).getAttribute('aria-selected') === 'true', name);
  assert.equal(await page.locator('#review-settings-form [role=tabpanel]:visible').count(), 1);
}
async function toggle(page, id) { await page.locator(`label:has(#${id})`).click(); }
async function close(page, method = 'button') {
  if (method === 'escape') await page.keyboard.press('Escape');
  else if (method === 'backdrop') await page.locator('#settings-gate').click({ position: { x: 2, y: 2 } });
  else await page.locator('#review-settings-close').click();
  await page.waitForFunction(() => document.getElementById('settings-gate').classList.contains('hidden'));
}

test('all settings save on close and survive reopening; no main Save button', async t => {
  const { page, writes } = await openSettings(t);
  assert.equal(await page.locator('#review-settings-save').count(), 0);
  assert.equal(await page.locator('.settings-header-slim #review-settings-close').count(), 1);
  await select(page, 'automation');
  await toggle(page, 'review-settings-auto-merge');
  await toggle(page, 'review-settings-delete-merged-branches');
  await select(page, 'review');
  await page.locator('#review-settings-slider').focus();
  for (const [key, level, text] of [['ArrowRight', 'low', 'Low · 1 review'], ['ArrowRight', 'medium', 'Medium · 3 reviews'], ['ArrowRight', 'high', 'High · 6 reviews']]) {
    await page.keyboard.press(key);
    assert.equal(await page.locator('#review-settings-level').inputValue(), level);
    assert.equal(await page.locator('#review-selected-text').textContent(), text);
    assert.equal(await page.locator('#review-selected-icon').getAttribute('href'), `#review-${level}`);
  }
  await select(page, 'merge');
  await page.locator('[data-review-settings-merge-method=rebase]').click();
  await select(page, 'library');
  await page.locator('#custom-library-name').fill('kept draft');
  await toggle(page, 'review-settings-default-library');
  assert.equal(await page.locator('.custom-library-fields').isVisible(), false);
  await toggle(page, 'review-settings-default-library');
  assert.equal(await page.locator('#custom-library-name').inputValue(), 'kept draft');
  await toggle(page, 'review-settings-default-library');
  await close(page);
  assert.deepEqual(writes.at(-1), { auto_merge: true, delete_merged_branches: true, merge_method: 'rebase', review_level: 'high', default_library_enabled: false, chat_enabled: true, json_enabled: false });
  await page.reload();
  await page.waitForFunction(() => !document.getElementById('review-settings-close').disabled);
  assert.equal(await page.locator('#review-settings-auto-merge').isChecked(), true);
  assert.equal(await page.locator('#review-settings-slider').inputValue(), '3');
  assert.equal(await page.locator('#review-settings-default-library').isChecked(), false);
  assert.equal(await page.locator('#review-settings-merge-method').inputValue(), 'rebase');
});

test('chat toggle marks the experimental chat tab unavailable immediately and persists on reload', async t => {
  const { page, writes } = await openSettings(t, { configurePage: async page => {
    await page.route('**/api/github/repos**', route => route.fulfill({ json: { ok: true, repos: [] } }));
  } });
  await page.waitForFunction(() => document.querySelector('[data-app-display=chat]').getAttribute('aria-disabled') === 'false');
  assert.equal(await page.locator('#review-settings-chat-enabled').isChecked(), true);
  assert.equal(await page.locator('[data-app-display=chat]').getAttribute('aria-disabled'), 'false');
  assert.notEqual(await page.locator('[data-app-display=chat]').getAttribute('title'), 'Chat is turned off in Settings → Automation');
  await select(page, 'automation');
  await toggle(page, 'review-settings-chat-enabled');
  assert.equal(await page.locator('[data-app-display=chat]').getAttribute('aria-disabled'), 'true');
  assert.equal(await page.locator('[data-app-display=chat]').getAttribute('title'), 'Chat is turned off in Settings → Automation');
  await close(page);
  assert.equal(writes.at(-1).chat_enabled, false);
  await page.reload();
  await page.waitForFunction(() => !document.getElementById('review-settings-close').disabled);
  assert.equal(await page.locator('#review-settings-chat-enabled').isChecked(), false);
  assert.equal(await page.locator('[data-app-display=chat]').getAttribute('title'), 'Chat is turned off in Settings → Automation');
  await select(page, 'automation');
  await toggle(page, 'review-settings-chat-enabled');
  assert.equal(await page.locator('[data-app-display=chat]').getAttribute('aria-disabled'), 'false');
  assert.equal(await page.locator('[data-app-display=chat]').getAttribute('href'), '#chat');
  await close(page);
  await page.locator('[data-app-display=chat]').click();
  await page.waitForFunction(() => document.body.dataset.appDisplay === 'chat');
});

test('json toggle marks the experimental JSON tab unavailable immediately and persists on reload', async t => {
  const { page, writes } = await openSettings(t);
  assert.equal(await page.locator('#review-settings-json-enabled').isChecked(), false);
  assert.equal(await page.locator('#prompt-mode-json').getAttribute('aria-disabled'), 'true');
  await select(page, 'automation');
  await toggle(page, 'review-settings-json-enabled');
  assert.equal(await page.locator('#prompt-mode-json').getAttribute('aria-disabled'), 'false');
  assert.equal(await page.locator('#prompt-mode-json').getAttribute('href'), '#studio-json');
  await close(page);
  assert.equal(writes.at(-1).json_enabled, true);
  await page.reload();
  await page.waitForFunction(() => !document.getElementById('review-settings-close').disabled);
  assert.equal(await page.locator('#review-settings-json-enabled').isChecked(), true);
  assert.equal(await page.locator('#prompt-mode-json').getAttribute('aria-disabled'), 'false');
  await close(page);
  await page.locator('#prompt-mode-json').click();
  await page.waitForFunction(() => document.body.dataset.promptMode === 'json');
});

for (const method of ['escape', 'backdrop']) test(`${method} saves settings before closing`, async t => {
  const { page, writes } = await openSettings(t);
  await select(page, 'automation'); await toggle(page, 'review-settings-auto-merge');
  await close(page, method);
  assert.equal(writes.at(-1).auto_merge, true);
});

test('failed save preserves the open dialog and edits; repeated close is single flight', async t => {
  let fail = true, release;
  let posts = 0;
  const { page } = await openSettings(t, { configurePage: async page => {
    await page.route('**/api/review-settings', async route => {
      if (route.request().method() !== 'POST') return route.fulfill({ json: { ok: true, settings: defaults } });
      posts++;
      await new Promise(resolve => { release = resolve; });
      return route.fulfill(fail ? { status: 500, json: { error: 'Storage temporarily unavailable.' } } : { json: { ok: true, settings: route.request().postDataJSON() } });
    });
  }});
  await select(page, 'automation'); await toggle(page, 'review-settings-auto-merge');
  await page.locator('#review-settings-close').click();
  await page.waitForFunction(() => document.getElementById('review-settings-close').disabled);
  await page.keyboard.press('Escape');
  assert.equal(posts, 1); release();
  await page.waitForFunction(() => document.getElementById('review-settings-status').textContent.includes('Storage temporarily'));
  assert.equal(await page.locator('#settings-gate').isVisible(), true);
  assert.equal(await page.locator('#review-settings-auto-merge').isChecked(), true);
  fail = false;
  await page.locator('#review-settings-close').click();
  while (posts < 2) await new Promise(resolve => setTimeout(resolve, 10));
  release();
  await page.waitForFunction(() => document.getElementById('settings-gate').classList.contains('hidden'));
});

test('selected labels remain visible; hover and keyboard navigation reveal reachable tabs', async t => {
  const { page } = await openSettings(t);
  for (const name of ['library', 'automation', 'review', 'merge', 'signing']) {
    await select(page, name); await page.mouse.move(0, 0);
    await page.waitForFunction(name => getComputedStyle(document.querySelector(`#settings-tab-${name} span`)).opacity === '1', name);
    assert.equal(await page.locator(`#settings-tab-${name} span`).evaluate(el => el.scrollWidth <= el.clientWidth + 1), true);
  }
  await page.locator('#settings-tab-signing').focus(); await page.keyboard.press('Home');
  assert.equal(await page.locator('#settings-tab-library').getAttribute('aria-selected'), 'true');
  await page.keyboard.press('ArrowRight');
  assert.equal(await page.locator('#settings-tab-automation').getAttribute('aria-selected'), 'true');
  await page.locator('.settings-tabbar').evaluate(async el => { await Promise.all(el.getAnimations({ subtree: true }).map(animation => animation.finished.catch(() => {}))); });
  await page.locator('#settings-tab-review').hover();
  await page.waitForFunction(() => getComputedStyle(document.querySelector('#settings-tab-review span')).opacity === '1');
  assert.equal(await page.locator('.settings-tabbar').evaluate(el => el.scrollHeight <= el.clientHeight + 1), true);
});

for (const theme of ['light', 'dark', 'night', 'pink']) test(`${theme} theme: selected highlights, icons, desktop and mobile panels`, async t => {
  const { page } = await openSettings(t, { theme });
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 900 });
    for (const name of ['library', 'automation', 'review', 'merge', 'signing']) {
      await select(page, name); await page.mouse.move(0, 0);
      assert.equal(await page.locator('.settings-shell').evaluate(el => el.scrollWidth <= el.clientWidth + 1), true, `${theme}/${width}/${name} fits`);
    }
    await select(page, 'automation');
    if (!(await page.locator('#review-settings-auto-merge').isChecked())) await toggle(page, 'review-settings-auto-merge');
    const colors = await page.locator('label:has(#review-settings-auto-merge)').evaluate(el => {
      const marker = document.createElement('span'); marker.style.color = 'var(--running)'; el.append(marker);
      const highlight = getComputedStyle(marker).color; marker.remove();
      return { icon: getComputedStyle(el.querySelector('svg')).color, highlight, text: getComputedStyle(el).color, surface: getComputedStyle(el).backgroundImage };
    });
    assert.equal(colors.icon, colors.highlight);
    assert.notEqual(colors.surface, 'none');
    await select(page, 'review');
    assert.equal(await page.locator('#review-slider-value svg').evaluate(el => getComputedStyle(el).color), colors.highlight);
    assert.equal(await page.locator('.review-slider-stop[data-current=true] svg').evaluate(el => getComputedStyle(el).color), colors.highlight);
    assert.equal(await page.locator('.review-slider-stop strong').count(), 0);
  }
});

test('changing themes while the dialog is open updates colors and preserves choices', async t => {
  const { page } = await openSettings(t);
  await select(page, 'automation'); await toggle(page, 'review-settings-auto-merge');
  const surfaces=[];
  for(const theme of ['light','dark','night','pink']) {
    // These are the same root classes changed by the app's theme picker.
    await page.evaluate(theme => { document.documentElement.classList.remove('light','dark','night','pink'); document.documentElement.classList.add(theme); document.documentElement.setAttribute('data-theme',theme); },theme);
    surfaces.push(await page.locator('.settings-tabbed-panel').evaluate(el=>getComputedStyle(el).backgroundImage));
    assert.equal(await page.locator('#review-settings-auto-merge').isChecked(),true);
  }
  assert.equal(new Set(surfaces).size,4);
});

test('library sources load on direct open and refresh after adding and removing', async t => {
  let sources = [{ name: 'existing', repo: 'git@example.com:team/library.git', taskCount: 1 }];
  const { page } = await openSettings(t, { configurePage: async page => {
    await page.route('**/api/library/sources**', route => {
      const request = route.request();
      if (request.method() === 'POST') {
        const source = { ...request.postDataJSON(), taskCount: 2 }; sources.push(source);
        return route.fulfill({ json: { ok: true, source } });
      }
      if (request.method() === 'DELETE') sources = sources.filter(source => source.name !== new URL(request.url()).searchParams.get('name'));
      return route.fulfill({ json: { ok: true, sources } });
    });
  }});
  await select(page, 'library');
  await page.waitForSelector('#custom-library-list .library-schedule-row');
  assert.equal(await page.locator('#custom-library-list .library-schedule-row').count(), 1);
  await page.locator('#custom-library-name').fill('new-library');
  await page.locator('#custom-library-repo').fill('git@example.com:team/new.git');
  await page.locator('#custom-library-add').click();
  await page.waitForFunction(() => document.querySelectorAll('#custom-library-list .library-schedule-row').length === 2);
  await page.getByRole('button', { name: 'Remove existing', exact: true }).click();
  await page.waitForFunction(() => document.querySelectorAll('#custom-library-list .library-schedule-row').length === 1);
  assert.equal(await page.locator('#custom-library-list strong').textContent(), 'new-library');
});

test('a failed initial read cannot overwrite saved settings with defaults', async t => {
  let posts = 0;
  const { page } = await openSettings(t, { configurePage: async page => {
    await page.route('**/api/review-settings', route => {
      if (route.request().method() === 'POST') posts++;
      return route.fulfill({ status: 503, json: { error: 'Settings unavailable.' } });
    });
  }});
  await page.waitForFunction(() => document.getElementById('review-settings-status').textContent.includes('Settings unavailable'));
  assert.equal(await page.locator('#review-settings-auto-merge').isDisabled(), true);
  await close(page);
  assert.equal(posts, 0);
});

for (const viewport of [{ width: 1280, height: 1100 }, { width: 430, height: 932 }, { width: 430, height: 500 }]) {
  test(`settings keep their height and header position across tabs at ${viewport.width}×${viewport.height}`, async t => {
    const { page } = await openSettings(t, { viewport });
    const geometry = () => page.locator('.settings-shell').evaluate(node => {
      const shell = node.getBoundingClientRect();
      const header = node.querySelector('.settings-header-slim').getBoundingClientRect();
      const tabs = node.querySelector('.settings-tabbar').getBoundingClientRect();
      return { top: shell.top, height: shell.height, headerTop: header.top, tabsTop: tabs.top, bottom: shell.bottom };
    });
    const initial = await geometry();
    assert.ok(initial.top >= 0 && initial.bottom <= viewport.height, 'dialog fits the screen');
    for (const name of ['signing', 'automation', 'review', 'merge', 'library']) {
      await select(page, name);
      const current = await geometry();
      for (const key of ['top', 'height', 'headerTop', 'tabsTop']) {
        assert.ok(Math.abs(current[key] - initial[key]) < 1, `${name}: ${key} stays fixed`);
      }
      assert.equal(await page.locator('#review-settings-form [role=tabpanel][hidden]').evaluateAll(nodes => nodes.every(node => node.inert)), true);
    }
    await select(page, 'signing');
    await page.locator('#commit-signing-enabled-help').scrollIntoViewIfNeeded();
    const scrolled = await geometry();
    assert.equal(scrolled.headerTop, initial.headerTop, 'scrolling long content keeps Close reachable');
    assert.equal(scrolled.tabsTop, initial.tabsTop, 'scrolling long content keeps tabs reachable');
    assert.ok(await page.locator('#commit-signing-section').evaluate(node => node.scrollTop > 0), 'long panels scroll internally');
    await select(page, 'review');
    assert.equal(await page.locator('#review-settings-slider').isVisible(), true);
    await close(page);
  });
}
