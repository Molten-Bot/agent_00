// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/settings_tabs.cjs
// Uses the real embedded page; API writes are intercepted and never touch user settings.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });
const defaults = { auto_merge: false, delete_merged_branches: false, merge_method: 'squash', review_level: 'off', default_library_enabled: true, chat_enabled: true, json_enabled: false, github_issues_enabled: false, github_issue_link_mode: 'reference', github_issue_auto_start_assigned: false };
async function openSettings(t, { viewport = { width: 1280, height: 900 }, theme = 'dark', configurePage, initialSettings = defaults, path = '#settings' } = {}) {
  const page = await browser.newPage({ viewport });
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.addInitScript(theme => localStorage.setItem('hubui.theme', theme), theme);
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.route('**/api/agent-auth**', route => route.fulfill({ json: { ok: true, auth: { required: true, ready: true, harness: 'claude', state: 'ready' } } }));
  let settings = { ...initialSettings };
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
  await page.goto(path.startsWith('/') ? `${baseURL}${path}` : `${baseURL}/${path}`);
  if (path === '#settings') await page.waitForFunction(() => !document.getElementById('review-settings-close').disabled);
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

for (const harness of ['claude', 'codex']) test(`settings reopen ${harness} sign-in and recover after completion`, async t => {
  let auth = { harness, required: true, ready: true, state: 'ready' };
  let starts = 0;
  const { page, writes } = await openSettings(t, { configurePage: async page => {
    await page.route('**/api/agent-auth**', route => {
      const path = new URL(route.request().url()).pathname;
      if (path.endsWith('/reauthenticate')) {
        assert.equal(route.request().method(), 'POST');
        starts++;
        auth = { harness, required: true, ready: false,
          state: harness === 'claude' ? 'pending_browser_login' : 'pending_device_auth',
          auth_url: harness === 'claude' ? 'https://claude.com/cai/oauth/authorize?code=true&state=test' : 'https://auth.openai.com/device',
          accepts_browser_code: harness === 'claude', device_code: harness === 'codex' ? 'ABCD-EFGH' : '',
        };
      } else if (path.endsWith('/configure') || path.endsWith('/verify')) {
        auth = { harness, required: true, ready: true, state: 'ready' };
      }
      return route.fulfill({ json: { ok: true, auth } });
    });
  }});
  const link = page.locator('#settings-agent-reauthenticate');
  assert.equal(await link.textContent(), `Sign in to ${harness === 'claude' ? 'Claude Code' : 'Codex'} again`);
  await select(page, 'automation');
  await toggle(page, 'review-settings-auto-merge');
  await link.click();
  await page.waitForFunction(harness => {
    if (!document.getElementById('settings-gate').classList.contains('hidden')) return false;
    const selector = harness === 'claude' ? '#agent-auth-browser-code-input' : '#agent-auth-device-code-row';
    return !document.querySelector(selector).classList.contains('hidden');
  }, harness);
  assert.equal(starts, 1);
  assert.equal(writes.at(-1).auto_merge, true, 'pending settings saved before sign-in');
  assert.equal(await page.locator('#local-prompt-submit').isDisabled(), true);
  if (harness === 'claude') await page.locator('#agent-auth-browser-code-input').fill('browser-code#test-state');
  else await page.locator('#agent-auth-verify').click();
  await page.waitForFunction(() => document.getElementById('agent-auth-gate').classList.contains('hidden'));
  assert.equal(await page.locator('#local-prompt-submit').isEnabled(), true);
  await page.reload();
  await page.waitForFunction(() => document.getElementById('agent-auth-gate').classList.contains('hidden'));
  assert.equal(starts, 1, 'reload uses refreshed auth without starting another login');
});

test('reauthentication preserves settings when saving fails', async t => {
  let starts = 0;
  const { page } = await openSettings(t, { configurePage: async page => {
    await page.route('**/api/agent-auth/reauthenticate', route => { starts++; return route.abort(); });
    await page.route('**/api/review-settings', route => route.fulfill(route.request().method() === 'POST'
      ? { status: 500, json: { error: 'Unable to save settings' } }
      : { json: { ok: true, settings: defaults } }));
  }});
  await page.locator('#settings-agent-reauthenticate').click();
  await page.waitForFunction(() => document.getElementById('review-settings-status').textContent.includes('Unable to save settings'));
  assert.equal(await page.locator('#settings-gate').isVisible(), true);
  assert.equal(starts, 0);
});

test('a stale ready status cannot dismiss a newly opened sign-in dialog', async t => {
  const ready = { harness: 'claude', required: true, ready: true, state: 'ready' };
  let auth = ready;
  let statusReads = 0;
  let releaseStatus;
  const { page } = await openSettings(t, { configurePage: async page => {
    await page.route('**/api/agent-auth**', async route => {
      if (route.request().url().endsWith('/reauthenticate')) {
        auth = { harness: 'claude', required: true, ready: false, state: 'needs_browser_login' };
      } else if (++statusReads === 2) {
        await new Promise(resolve => { releaseStatus = resolve; });
        return route.fulfill({ json: { ok: true, auth: ready } });
      }
      return route.fulfill({ json: { ok: true, auth } });
    });
  }});
  // Wait for the normal background poll, keeping its old ready response in flight.
  const deadline = Date.now() + 10000;
  while (!releaseStatus && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 20));
  assert.ok(releaseStatus, 'background auth poll started');
  await page.locator('#settings-agent-reauthenticate').click();
  await page.waitForFunction(() => document.getElementById('agent-auth-verify').textContent === 'Sign in to Claude');
  const staleResponse = page.waitForResponse(response => new URL(response.url()).pathname === '/api/agent-auth');
  releaseStatus();
  await (await staleResponse).finished();
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.locator('#agent-auth-gate').isVisible(), true);
  assert.equal(await page.locator('#local-prompt-submit').isDisabled(), true);
});

test('reauthentication start errors are visible and later status recovers', async t => {
  let auth = { harness: 'claude', required: true, ready: true, state: 'ready' };
  const { page } = await openSettings(t, { configurePage: async page => {
    await page.route('**/api/agent-auth**', route => {
      if (route.request().url().endsWith('/reauthenticate')) {
        auth = { harness: 'claude', required: true, ready: false, state: 'needs_browser_login', message: 'Login unavailable' };
        return route.fulfill({ status: 400, json: { error: 'Login unavailable', auth } });
      }
      return route.fulfill({ json: { ok: true, auth } });
    });
  }});
  await page.locator('#settings-agent-reauthenticate').click();
  await page.waitForFunction(() => document.getElementById('agent-auth-message').textContent.includes('Login unavailable'));
  assert.equal(await page.locator('#agent-auth-gate').isVisible(), true);
  assert.equal(await page.locator('#local-prompt-submit').isDisabled(), true);
  auth = { harness: 'claude', required: true, ready: true, state: 'ready' };
  await page.waitForFunction(() => document.getElementById('agent-auth-gate').classList.contains('hidden'));
});

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
  assert.deepEqual(writes.at(-1), { auto_merge: true, delete_merged_branches: true, merge_method: 'rebase', review_level: 'high', default_library_enabled: false, chat_enabled: true, json_enabled: false, github_issues_enabled: false, github_issue_link_mode: 'reference', github_issue_auto_start_assigned: false });
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

test('GitHub Issues toggle exposes and hides the dock link and persists on reload', async t => {
  const { page, writes } = await openSettings(t);
  assert.equal(await page.locator('#review-settings-github-issues-enabled').isChecked(), false);
  assert.equal(await page.locator('#github-issues-dock-link').getAttribute('aria-disabled'), 'true');
  await select(page, 'github-issues');
  await toggle(page, 'review-settings-github-issues-enabled');
  assert.equal(await page.locator('#review-settings-github-issues-auto-start').isVisible(), true);
  await toggle(page, 'review-settings-github-issues-auto-start-assigned');
  assert.equal(await page.locator('#review-settings-github-issues-link-mode').isVisible(), true);
  assert.equal(await page.locator('#settings-tab-github-issues [data-lucide="folder-kanban"]').count(), 1);
  assert.equal(await page.locator('#review-settings-github-issues-enabled').locator('xpath=..').locator('[data-lucide="folder-kanban"]').count(), 1);
  assert.equal(await page.locator('#review-settings-github-issues-link-reference').isChecked(), true);
  await page.locator('#review-settings-github-issues-link-close').check();
  assert.equal(await page.locator('#github-issues-dock-link').getAttribute('aria-disabled'), 'false');
  assert.equal(await page.locator('#github-issues-dock-link').getAttribute('href'), '#studio-issues');
  await close(page);
  assert.equal(writes.at(-1).github_issues_enabled, true);
  assert.equal(writes.at(-1).github_issue_link_mode, 'close');
  assert.equal(writes.at(-1).github_issue_auto_start_assigned, true);
  assert.equal(await page.locator('#github-issues-dock-link').getAttribute('aria-disabled'), 'false');
  assert.equal(await page.locator('#github-issues-dock-link').getAttribute('href'), '#studio-issues');
  await page.reload();
  await page.waitForFunction(() => !document.getElementById('review-settings-close').disabled);
  await select(page, 'github-issues');
  assert.equal(await page.locator('#review-settings-github-issues-enabled').isChecked(), true);
  assert.equal(await page.locator('#review-settings-github-issues-link-close').isChecked(), true);
  assert.equal(await page.locator('#review-settings-github-issues-auto-start-assigned').isChecked(), true);
  await toggle(page, 'review-settings-github-issues-enabled');
  assert.equal(await page.locator('#review-settings-github-issues-link-mode').isVisible(), false);
  assert.equal(await page.locator('#review-settings-github-issues-auto-start').isVisible(), false);
  assert.equal(await page.locator('#github-issues-dock-link').getAttribute('aria-disabled'), 'true');
  assert.equal(await page.locator('#github-issues-dock-link').getAttribute('href'), null);
  assert.equal(await page.locator('#github-issues-dock-link').evaluate(link => link.classList.contains('hidden')), true);
  await close(page);
  assert.equal(writes.at(-1).github_issues_enabled, false);
  assert.equal(writes.at(-1).github_issue_link_mode, 'close');
  assert.equal(writes.at(-1).github_issue_auto_start_assigned, true);
  await page.reload();
  await page.waitForFunction(() => !document.getElementById('review-settings-close').disabled);
  await select(page, 'github-issues');
  assert.equal(await page.locator('#review-settings-github-issues-enabled').isChecked(), false);
  assert.equal(await page.locator('#github-issues-dock-link').getAttribute('aria-disabled'), 'true');
});

test('GitHub Issues settings initialize even when Studio opens in another prompt mode', async t => {
  const { page } = await openSettings(t, {
    path: '/',
    initialSettings: { ...defaults, github_issues_enabled: true },
    configurePage: async page => {
      await page.route('**/api/github/repos**', route => route.fulfill({ json: { ok: true, repos: [] } }));
    },
  });

  assert.notEqual(await page.locator('body').getAttribute('data-prompt-mode'), 'issues');
  assert.equal(await page.locator('script[src^="/static/github-issues.js"]').getAttribute('src'), '/static/github-issues.js?v=10');
  await page.waitForFunction(() => document.querySelector('.github-issue-empty-state h2')?.textContent === 'No repositories available');
  assert.equal(await page.locator('.github-issue-empty-state h2').textContent(), 'No repositories available');
  await page.locator('#github-issues-dock-link').click();
  assert.equal(await page.locator('#github-issues-status').textContent(), '');
});

test('GitHub Issues remembers its repository and gives a useful empty state', async t => {
  let lastSearch = '';
  const { page } = await openSettings(t, {
    path: '/',
    initialSettings: { ...defaults, github_issues_enabled: true },
    configurePage: async page => {
      await page.addInitScript(() => localStorage.setItem('hubui.githubIssuesRepository.v1', 'Molten-Bot/agent_00'));
      await page.route('**/api/github/repos**', route => route.fulfill({ json: { ok: true, repos: [
        { full_name: 'Molten-Bot/agent_00', default_branch: 'main' },
      ] } }));
      await page.route('**/api/github/issues**', route => {
        lastSearch = new URL(route.request().url()).searchParams.get('search') || '';
        return route.fulfill({ json: { ok: true, issues: [] } });
      });
    },
  });

  await page.waitForFunction(() => document.getElementById('github-issues-repo').value === 'Molten-Bot/agent_00');
  await page.waitForFunction(() => document.getElementById('github-issues-count').textContent === '0 open issues assigned to you');
  assert.equal(await page.locator('.github-issues-empty').textContent(), 'No open issues are assigned to you. Use the filter button to show every open issue.');
  assert.equal(await page.locator('#github-issues-search').getAttribute('placeholder'), 'Search issues assigned to you');
  assert.equal(await page.locator('.github-issue-empty-state h2').textContent(), 'No assigned issues', 'an empty list must not invite choosing an issue');
  assert.equal(await page.locator('.github-issue-empty-state p').textContent(), 'No open issues are assigned to you. Use the filter button to show every open issue.');
  await page.locator('#github-issues-dock-link').click();
  await page.setViewportSize({ width: 390, height: 844 });
  assert.equal(await page.evaluate(() => document.body.scrollWidth), 390, 'mobile Issues view should not scroll horizontally');
  assert.equal(await page.locator('#github-issues-filters button').count(), 0, 'issue search should not need a Search button');
  const automaticSearch = page.waitForResponse(response => new URL(response.url()).searchParams.get('search') === 'retry');
  await page.locator('#github-issues-search').fill('retry');
  await automaticSearch;
  assert.equal(lastSearch, 'retry', 'typing should automatically search issues');
  await page.waitForFunction(() => document.querySelector('.github-issues-empty')?.textContent === 'No open issues assigned to you match this search. Use the filter button to search every open issue.');
  assert.equal(await page.locator('.github-issue-empty-state h2').textContent(), 'No matching issues');
});

test('GitHub Issues honours a stored assignee filter choice before the browser is shown', async t => {
  let issueAssigneeFilter;
  const { page } = await openSettings(t, {
    path: '/#studio-issues',
    initialSettings: { ...defaults, github_issues_enabled: true },
    configurePage: async page => {
      await page.addInitScript(() => {
        localStorage.setItem('hubui.githubIssuesRepository.v1', 'Molten-Bot/agent_00');
        // Only seed the preference on the first load so the reload below reads what the click stored.
        if (localStorage.getItem('hubui.githubIssuesAssignee.v1') === null) localStorage.setItem('hubui.githubIssuesAssignee.v1', 'false');
      });
      await page.route('**/api/github/repos**', route => route.fulfill({ json: { ok: true, repos: [
        { full_name: 'Molten-Bot/agent_00', default_branch: 'main' },
      ] } }));
      await page.route('**/api/github/issues**', route => {
        issueAssigneeFilter = new URL(route.request().url()).searchParams.get('assignee') || '';
        return route.fulfill({ json: { ok: true, issues: [] } });
      });
    },
  });

  const assigneeFilter = page.getByRole('button', { name: 'Show only issues assigned to me' });
  await page.waitForFunction(() => document.getElementById('github-issues-browser').hidden === false);
  assert.equal(await assigneeFilter.getAttribute('aria-pressed'), 'false', 'a stored filter-off choice must win over the server-rendered default');
  assert.equal(await page.locator('#github-issues-search').getAttribute('placeholder'), 'Search this repository');
  await page.waitForFunction(() => document.querySelector('.github-issues-empty')?.textContent === 'This repository has no open issues.');
  assert.equal(await page.locator('#github-issues-count').textContent(), '0 open issues');
  assert.equal(await page.locator('.github-issue-empty-state h2').textContent(), 'No open issues');
  assert.equal(issueAssigneeFilter, '');
  const assignedIssuesResponse = page.waitForResponse(response => new URL(response.url()).searchParams.get('assignee') === 'me');
  await assigneeFilter.click();
  await assignedIssuesResponse;
  assert.equal(await page.evaluate(() => localStorage.getItem('hubui.githubIssuesAssignee.v1')), 'true');
  await page.reload();
  await page.waitForFunction(() => document.querySelector('.github-issues-empty')?.textContent === 'No open issues are assigned to you. Use the filter button to show every open issue.');
  assert.equal(await assigneeFilter.getAttribute('aria-pressed'), 'true');
  assert.equal(await page.locator('#github-issues-search').getAttribute('placeholder'), 'Search issues assigned to you');
  assert.equal(issueAssigneeFilter, 'me');
});

test('GitHub Issues renders inside Studio and uses the configured link mode', async t => {
  let submitted;
  let issueQueryState;
  let issueAssigneeFilter;
  const issueURL = 'https://github.com/Molten-Bot/agent_00/issues/17';
  const { page } = await openSettings(t, {
    path: '/#studio-issues',
    initialSettings: { ...defaults, github_issues_enabled: true, github_issue_link_mode: 'close' },
    configurePage: async page => {
      await page.route('**/api/github/repos**', route => route.fulfill({ json: { ok: true, repos: [
        { full_name: 'Molten-Bot/agent_00', html_url: 'https://github.com/Molten-Bot/agent_00', default_branch: 'main' },
      ] } }));
      await page.route('**/api/github/issues**', route => {
        const issueRequest = new URL(route.request().url());
        const path = issueRequest.pathname;
        if (path === '/api/github/issues') {
          issueQueryState = issueRequest.searchParams.get('state');
          issueAssigneeFilter = issueRequest.searchParams.get('assignee') || '';
        }
        const issue = { number: 17, title: 'Add issue browser', state: 'OPEN', url: issueURL,
          author: { login: 'octocat' }, labels: [{ name: 'feature' }], assignees: [{ login: 'hubot' }],
          createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-02T00:00:00Z' };
        return route.fulfill({ json: path.endsWith('/17')
          ? { ok: true, issue: { ...issue, body: 'Implement this issue.', comments: [{ author: { login: 'hubot' }, body: 'Please include retry coverage.', createdAt: '2026-01-03T00:00:00Z' }] } }
          : { ok: true, issues: [issue] } });
      });
      await page.route('**/api/local-prompt', route => {
        submitted = route.request().postDataJSON();
        return route.fulfill({ json: { ok: true, request_id: 'issue-task-17' } });
      });
    },
  });

  assert.equal(await page.locator('body').getAttribute('data-prompt-mode'), 'issues');
  assert.equal(await page.locator('#github-issues-panel').isVisible(), true);
  assert.equal(
    await page.locator('#github-issues-panel').evaluate(panel => getComputedStyle(panel).padding),
    await page.locator('#local-prompt-form').evaluate(form => getComputedStyle(form).padding),
    'GitHub Issues should use the prompt form padding',
  );
  assert.equal(
    await page.locator('#github-issues-filters').evaluate(filters => getComputedStyle(filters).gap),
    await page.locator('#prompt-builder-panel .prompt-grid').evaluate(grid => getComputedStyle(grid).gap),
    'issue filters should use the shared prompt grid spacing',
  );
  assert.equal(
    Math.round((await page.locator('#github-issues-filters').boundingBox()).x - (await page.locator('#github-issues-panel').boundingBox()).x),
    Number.parseFloat(await page.locator('#github-issues-panel').evaluate(panel => getComputedStyle(panel).paddingLeft)),
    'issue filters should use the prompt form inset',
  );
  assert.equal(await page.locator('#local-prompt-form').evaluate(form => form.classList.contains('hidden')), true);
  assert.equal(await page.locator('.prompt-wrap').isVisible(), true);
  assert.equal(await page.locator('.prompt-title-mode[data-prompt-title-mode="issues"]').isVisible(), true);
  assert.match(await page.locator('.prompt-title-mode[data-prompt-title-mode="issues"]').textContent(), /GitHub Issues/);
  assert.equal(
    await page.locator('#prompt-panel-title').evaluate(title => getComputedStyle(title).fontSize),
    await page.locator('.prompt-titlebar').evaluate(header => getComputedStyle(header).fontSize),
  );
  assert.equal(await page.locator('#github-issues-title').count(), 0);
  assert.equal(await page.locator('#github-issues-repo').evaluate(select => select.closest('#github-issues-filters') !== null), true);
  assert.equal(await page.locator('#github-issues-repo').evaluate(select => select.classList.contains('prompt-control')), true);
  assert.equal(await page.locator('#github-issues-repo').evaluate(select => select.parentElement.classList.contains('prompt-select-action-wrap')), true);
  assert.equal(
    await page.locator('#github-issues-repo').evaluate(select => getComputedStyle(select).borderRadius),
    await page.locator('#builder-repo-select').evaluate(select => getComputedStyle(select).borderRadius),
  );
  assert.equal(
    await page.locator('#github-issues-search').evaluate(input => getComputedStyle(input).minHeight),
    await page.locator('#builder-base-branch').evaluate(input => getComputedStyle(input).minHeight),
  );
  assert.equal(await page.locator('#github-issues-state').count(), 0);
  assert.equal(await page.locator('#github-issues-dock-link [data-lucide="folder-kanban"]').count(), 1);
  assert.equal(await page.locator('#github-issues-dock-link').isVisible(), true);
  assert.deepEqual(await page.evaluate(() => {
    const root = document.querySelector('.prompt-mode-tabs-dock');
    const ids = ['prompt-mode-library', 'github-issues-dock-link', 'task-view-dock-link'];
    return ids.map(id => Array.from(root.children).findIndex(node => node.id === id));
  }), [2, 3, 5]);
  assert.equal(await page.locator('#github-issues-dock-link + .prompt-mode-divider').count(), 1);
  assert.equal(await page.locator('#prompt-visibility-toggle').count(), 0);
  const title = page.locator('#prompt-panel-title .prompt-title-mode.active');
  const titleWidth = await title.evaluate(node => node.getBoundingClientRect().width);
  assert.equal(await title.evaluate(node => getComputedStyle(node).whiteSpace), 'nowrap');
  assert.equal(await page.locator('#prompt-panel-title').evaluate(node => getComputedStyle(node).flexShrink), '0');
  await page.setViewportSize({ width: 360, height: 900 });
  assert.ok(await title.evaluate((node, width) => node.getBoundingClientRect().width >= width - 1, titleWidth), 'shared title should keep its width when the viewport narrows');
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.locator('#github-issues-repo').selectOption('Molten-Bot/agent_00');
  await page.locator('.github-issue-list-item').waitFor();
  await page.waitForFunction(() => document.getElementById('github-issues-status').textContent === '');
  assert.equal(issueQueryState, 'open');
  assert.equal(issueAssigneeFilter, 'me', 'issues assigned to the signed-in account load by default');
  const assigneeFilter = page.getByRole('button', { name: 'Show only issues assigned to me' });
  assert.equal(await assigneeFilter.innerText(), '', 'assignee filter should be icon-only');
  assert.equal(await assigneeFilter.locator('svg.lucide-user-round-arrow-left').count(), 1);
  assert.equal(await assigneeFilter.getAttribute('aria-pressed'), 'true', 'the assignee filter starts pressed');
  assert.equal(await page.locator('#github-issues-search').getAttribute('placeholder'), 'Search issues assigned to you');
  await page.waitForFunction(() => document.getElementById('github-issues-count').textContent === '1 open issue assigned to you');
  const allIssuesResponse = page.waitForResponse(response => new URL(response.url()).pathname === '/api/github/issues' && !new URL(response.url()).searchParams.has('assignee'));
  await assigneeFilter.click();
  await allIssuesResponse;
  assert.equal(issueAssigneeFilter, '');
  assert.equal(await assigneeFilter.getAttribute('aria-pressed'), 'false');
  assert.equal(await assigneeFilter.getAttribute('aria-label'), 'Show only issues assigned to me', 'the toggle keeps one name so it never contradicts aria-pressed');
  assert.equal(await page.locator('#github-issues-search').getAttribute('placeholder'), 'Search this repository');
  const assignedIssuesResponse = page.waitForResponse(response => new URL(response.url()).searchParams.get('assignee') === 'me');
  await assigneeFilter.click();
  await assignedIssuesResponse;
  assert.equal(issueAssigneeFilter, 'me');
  assert.equal(await assigneeFilter.getAttribute('aria-pressed'), 'true');
  await page.waitForFunction(() => document.getElementById('github-issues-count').textContent === '1 open issue assigned to you');
  assert.equal(await page.locator('#github-issues-status').textContent(), '');
  assert.match(await page.locator('.github-issue-list-title').textContent(), /Add issue browser/);
  const resizeHandle = page.locator('#github-issues-resize-handle');
  await resizeHandle.focus();
  const initialWidth = Number(await resizeHandle.getAttribute('aria-valuenow'));
  await page.keyboard.press('ArrowRight');
  assert.equal(Number(await resizeHandle.getAttribute('aria-valuenow')), initialWidth + 24);
  const divider = await resizeHandle.boundingBox();
  await page.mouse.move(divider.x + divider.width / 2, divider.y + divider.height / 2);
  await page.mouse.down();
  await page.mouse.move(divider.x + divider.width / 2 + 48, divider.y + divider.height / 2);
  await page.mouse.up();
  assert.equal(Number(await resizeHandle.getAttribute('aria-valuenow')), initialWidth + 72);
  assert.match(await page.evaluate(() => localStorage.getItem('hubui.githubIssuesLayout.v1')), /"width":/);
  assert.equal(await page.locator('#github-issues-list-toggle').count(), 0);
  const columnsBox = await page.locator('.github-issues-columns').boundingBox();
  const listBox = await page.locator('#github-issues-list-panel').boundingBox();
  const detailBox = await page.locator('#github-issue-detail').boundingBox();
  assert.equal(Math.round(listBox.y), Math.round(columnsBox.y));
  assert.equal(Math.round(detailBox.y), Math.round(columnsBox.y));
  assert.equal(Math.round(listBox.height), Math.round(columnsBox.height));
  assert.equal(Math.round(detailBox.height), Math.round(columnsBox.height));
  await page.locator('.github-issue-list-item').click();
  await page.getByRole('heading', { name: 'Add issue browser' }).waitFor();
  assert.match(await page.locator('.github-issue-body').textContent(), /Implement this issue/);
  assert.match(await page.locator('.github-issue-comment-body').textContent(), /retry coverage/);
  assert.equal(await page.locator('.github-issue-task .github-issue-link-options').count(), 0);
  assert.match(await page.locator('.github-issue-task-heading').textContent(), /close #17 on merge/);
  await page.evaluate(() => window.dispatchEvent(new CustomEvent('github-issue-link-mode-updated', { detail: { mode: 'reference' } })));
  assert.match(await page.locator('.github-issue-task-heading').textContent(), /reference #17/);

  const issueEditButton = page.getByRole('button', { name: 'Edit prompt first' });
  const issueStartButton = page.getByRole('button', { name: 'Start task' });
  assert.equal(await issueEditButton.evaluate(button => button.classList.contains('prompt-action-button')), true);
  assert.equal(await issueStartButton.evaluate(button => button.classList.contains('prompt-submit')), true);
  assert.equal(await issueStartButton.locator('.prompt-action-icon').count(), 1);
  const readPromptButtonStyle = button => button.evaluate(node => {
    const style = getComputedStyle(node);
    return { minHeight: style.minHeight, borderRadius: style.borderRadius, fontSize: style.fontSize, backgroundImage: style.backgroundImage };
  });
  assert.deepEqual(await readPromptButtonStyle(issueStartButton), await readPromptButtonStyle(page.locator('#local-prompt-submit')));

  await issueStartButton.click();
  await page.waitForFunction(() => document.querySelector('.github-issue-task-status').textContent.includes('Task queued'));
  assert.equal(submitted.githubIssue.repository, 'Molten-Bot/agent_00');
  assert.equal(submitted.githubIssue.number, 17);
  assert.equal(submitted.githubIssue.linkMode, 'reference');
  assert.match(submitted.prompt, /Please include retry coverage/);
});

test('selecting an issue highlights its row and breathes a loader while details load', async t => {
  let releaseDetails;
  const detailsPending = new Promise(resolve => { releaseDetails = resolve; });
  const issue = { number: 17, title: 'Add issue browser', state: 'OPEN', url: 'https://github.com/Molten-Bot/agent_00/issues/17', author: { login: 'octocat' }, labels: [], assignees: [] };
  const { page } = await openSettings(t, {
    path: '/#studio-issues',
    initialSettings: { ...defaults, github_issues_enabled: true },
    configurePage: async page => {
      await page.route('**/api/github/repos**', route => route.fulfill({ json: { ok: true, repos: [
        { full_name: 'Molten-Bot/agent_00', html_url: 'https://github.com/Molten-Bot/agent_00', default_branch: 'main' },
      ] } }));
      await page.route('**/api/github/issues**', async route => {
        if (new URL(route.request().url()).pathname.endsWith('/17')) {
          await detailsPending;
          return route.fulfill({ json: { ok: true, issue: { ...issue, body: 'Description', comments: [] } } });
        }
        return route.fulfill({ json: { ok: true, issues: [issue] } });
      });
    },
  });
  await page.locator('#github-issues-repo').selectOption('Molten-Bot/agent_00');
  const row = page.locator('.github-issue-list-item[data-issue-number="17"]');
  await row.waitFor();
  await row.click();
  assert.equal(await row.getAttribute('aria-current'), 'true');
  assert.equal(await row.getAttribute('aria-busy'), 'true');
  assert.equal(await row.locator('.github-issue-loading').getAttribute('aria-label'), 'Loading issue details');
  assert.equal(await row.locator('.github-issue-loading svg').evaluate(icon => getComputedStyle(icon).animationName), 'taskIconBreathe');
  releaseDetails();
  await page.waitForFunction(() => document.querySelector('.github-issue-list-item[data-issue-number="17"]')?.getAttribute('aria-busy') === 'false');
  assert.equal(await row.locator('.github-issue-loading').count(), 0);
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

test('dock links fail safe to hidden when the initial settings read fails, then recover once a later read succeeds', async t => {
  let fail = true;
  const { page } = await openSettings(t, { path: '', configurePage: async page => {
    await page.route('**/api/review-settings', route => {
      if (fail) return route.fulfill({ status: 503, json: { error: 'Settings unavailable.' } });
      return route.fulfill({ json: { ok: true, settings: { ...defaults, chat_enabled: true, json_enabled: true } } });
    });
  }});
  await page.waitForFunction(() => document.getElementById('prompt-mode-json')?.getAttribute('aria-disabled') === 'true');
  assert.equal(await page.locator('[data-app-display="chat"]').evaluate(el => el.classList.contains('hidden')), true);
  assert.equal(await page.locator('#prompt-mode-json').evaluate(el => el.classList.contains('hidden')), true);

  fail = false;
  await page.locator('#settings-open').click();
  await page.waitForFunction(() => !document.getElementById('review-settings-close').disabled);
  await page.waitForFunction(() => document.getElementById('prompt-mode-json')?.getAttribute('aria-disabled') === 'false');
  assert.equal(await page.locator('[data-app-display="chat"]').evaluate(el => el.classList.contains('hidden')), false);
  assert.equal(await page.locator('#prompt-mode-json').evaluate(el => el.classList.contains('hidden')), false);
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
