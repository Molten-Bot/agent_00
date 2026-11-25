// HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/mobile_navigation.cjs
// Requires Playwright/Chromium. All APIs use fixtures; no tasks or settings are changed.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

async function openPage(t, hash, viewport = { width: 430, height: 932 }, mobile = true, fixtureTasks, stats = {}) {
  const page = await browser.newPage({ viewport, isMobile: mobile, hasTouch: mobile });
  page.setDefaultTimeout(10000);
  t.after(() => page.close());
  const errors = [], writes = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  const now = new Date().toISOString();
  const tasks = fixtureTasks ?? Array.from({ length: 24 }, (_, i) => ({
    request_id: `mobile-${i}`, prompt: `Task ${i + 1}: Improve repository navigation and support long descriptions on mobile devices`,
    prompt_is_user_input: true, repo: 'example/mobile-project', status: i < 16 ? 'running' : 'completed',
    stage: 'agent', source: 'prompt', agent_harness: 'codex',
    started_at: now, updated_at: now,
    controls: { pause: true, stop: true }, logs: [{ text: 'Example terminal output', stream: 'stdout' }],
  }));
  await page.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/d3.min.js') && stats.source_mix) {
      const d3Script = path.resolve(path.dirname(require.resolve('d3')), '../dist/d3.min.js');
      return route.fulfill({ path: d3Script, contentType: 'application/javascript' });
    }
    if (url.origin !== new URL(baseURL).origin) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    if (route.request().method() !== 'GET') writes.push(url.pathname);
    let json = { ok: true };
    if (url.pathname === '/api/status') json = {
      events: [], repositories: [], stats, resources: {},
      ...(typeof tasks === 'function' ? tasks() : { tasks }),
    };
    if (url.pathname === '/api/agent-auth') json.auth = { required: false, ready: true, harness: 'codex', state: 'ready' };
    if (url.pathname === '/api/review-settings') json.settings = {
      review_level: 'off', default_library_enabled: true, json_enabled: true, chat_enabled: true, github_issues_enabled: true,
    };
    if (url.pathname === '/api/github/repos') json.repos = Array.from({ length: 40 }, (_, i) => ({
      name: `project-${i + 1}`, full_name: `example/project-${i + 1}`, description: 'An example repository for mobile navigation.', private: false,
    }));
    if (url.pathname === '/api/library') json.tasks = Array.from({ length: 24 }, (_, i) => ({
      name: `task-${i}`, displayName: `Library task ${i + 1}`, type: 'quality',
      description: 'Review and improve the repository with focused changes.', prompt: 'Example prompt',
    }));
    if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
    return route.fulfill({ json });
  });
  await page.goto(`${baseURL}/#${hash}`);
  await page.waitForFunction(() => document.querySelector('.page-bottom-dock')?.dataset.bottomDockReady === 'true');
  return { page, writes };
}

function hasD3(t) {
  try {
    require.resolve('d3');
    return true;
  } catch (error) {
    if (error.code !== 'MODULE_NOT_FOUND') throw error;
    t.skip('d3 module not available');
    return false;
  }
}

test('320px: legends fit inside both charts and leave room for the focused share', async t => {
  if (!hasD3(t)) return;
  const sourceMix = ['Chat', 'Prompt', 'Library', 'GitHub Issues', 'JSON']
    .map(name => ({ name, tasks: name === 'GitHub Issues' ? 3 : 1 }));
  const { page } = await openPage(t, 'dashboard', { width: 320, height: 844 }, true, [], {
    source_mix: sourceMix, total_tasks: 7, running_tasks: 2, paused_tasks: 1,
    pending_tasks: 1, stopped_tasks: 1, completed_tasks: 1, failed_tasks: 1,
  });
  await assertFits(page);
  for (const [id, count] of [['dashboard-source-chart', 5], ['dashboard-task-chart', 6]]) {
    const legend = page.locator(`#${id} .dashboard-d3-legend-item`);
    await legend.nth(count - 1).waitFor();
    const overflowingLabels = await legend.evaluateAll(items => {
      const svg = items[0].ownerSVGElement;
      const bounds = svg.getBoundingClientRect();
      const card = svg.parentElement.getBoundingClientRect();
      const ring = svg.querySelector('.dashboard-d3-root').getBoundingClientRect();
      const labels = items.map(item => item.getBoundingClientRect());
      return items.filter((_item, index) => {
        const label = labels[index];
        return label.left < bounds.left || label.right > bounds.right ||
          label.top < ring.bottom || label.bottom > Math.min(bounds.bottom, card.bottom) ||
          labels.slice(0, index).some(previous =>
            label.top < previous.bottom && label.bottom > previous.top &&
            label.left < previous.right && label.right > previous.left);
      }).map(item => item.textContent);
    });
    assert.deepEqual(overflowingLabels, [], `${id}: legend entries fit below the ring without overlap`);
  }
  const chart = page.locator('#dashboard-source-chart');
  const slices = chart.locator('.dashboard-d3-slice');
  await slices.nth(3).focus();
  assert.equal(await chart.locator('.dashboard-d3-legend-item svg').count(), 5);
  assert.equal(await chart.locator('.dashboard-d3-center-value').textContent(), '42.9%');
  const overlapsRing = await chart.evaluate(svg => {
    const slices = [...svg.querySelectorAll('.dashboard-d3-slice')];
    return [...svg.querySelectorAll('.dashboard-d3-center-value, .dashboard-d3-center-label')].some(text => {
      const { x, y, width, height } = text.getBBox();
      return [x, x + width / 2, x + width].some(px =>
        [y, y + height / 2, y + height].some(py =>
          slices.some(slice => slice.isPointInFill(new DOMPoint(px, py)))));
    });
  });
  assert.equal(overlapsRing, false, 'focused percentage and label fit inside the ring');

  const viewBox = await chart.getAttribute('viewBox');
  const taskChart = page.locator('#dashboard-task-chart');
  const taskViewBox = await taskChart.getAttribute('viewBox');
  await page.setViewportSize({ width: 932, height: 430 });
  await page.waitForFunction(previous => document.querySelector('#dashboard-source-chart').getAttribute('viewBox') !== previous, viewBox);
  assert.notEqual(await taskChart.getAttribute('viewBox'), taskViewBox);
  assert.equal(await slices.nth(3).evaluate(node => node === document.activeElement), true);
  assert.equal(await chart.locator('.dashboard-d3-center-value').textContent(), '42.9%');
  assert.equal(await chart.locator('.dashboard-d3-center-label').textContent(), 'GitHub Issues');

  await chart.focus();
  assert.equal(await chart.locator('.dashboard-d3-center-value').textContent(), '7');
  assert.equal(await chart.locator('.dashboard-d3-center-label').textContent(), 'prompts');
});

test('resizing the source chart keeps the share for a stationary pointer over a slice', async t => {
  if (!hasD3(t)) return;
  const { page } = await openPage(t, 'dashboard', { width: 1000, height: 900 }, false, [], {
    source_mix: [{ name: 'Chat', tasks: 1 }, { name: 'GitHub Issues', tasks: 3 }],
    total_tasks: 4,
  });
  const chart = page.locator('#dashboard-source-chart');
  const slice = chart.getByLabel(/^GitHub Issues:/);
  const value = chart.locator('.dashboard-d3-center-value');
  const label = chart.locator('.dashboard-d3-center-label');
  await slice.scrollIntoViewIfNeeded();
  const box = await slice.boundingBox();
  const pointer = { x: box.x + box.width * 0.1, y: box.y + box.height * 0.5 };
  await page.mouse.move(pointer.x, pointer.y);
  assert.equal(await value.textContent(), '75%');

  const viewBox = await chart.getAttribute('viewBox');
  await page.setViewportSize({ width: 1010, height: 900 });
  await page.waitForFunction(previous => document.querySelector('#dashboard-source-chart').getAttribute('viewBox') !== previous, viewBox);
  assert.equal(await slice.evaluate((node, { x, y }) => document.elementFromPoint(x, y) === node, pointer), true);
  assert.equal(await chart.locator('.dashboard-d3-slice.is-active').count(), 1);
  assert.equal(await value.textContent(), '75%');
  assert.equal(await label.textContent(), 'GitHub Issues');

  const chatSlice = chart.getByLabel(/^Chat:/);
  await chatSlice.focus();
  const focusedViewBox = await chart.getAttribute('viewBox');
  await page.setViewportSize({ width: 1020, height: 900 });
  await page.waitForFunction(previous => document.querySelector('#dashboard-source-chart').getAttribute('viewBox') !== previous, focusedViewBox);
  assert.equal(await chatSlice.evaluate(node => node === document.activeElement), true);
  assert.equal(await slice.evaluate(node => node.matches(':hover')), true);
  assert.equal(await value.textContent(), '25%');
  assert.equal(await label.textContent(), 'Chat');

  await chart.focus();
  await page.mouse.move(0, 0);
  assert.equal(await chart.locator('.dashboard-d3-slice.is-active').count(), 0);
  assert.equal(await value.textContent(), '4');
  assert.equal(await label.textContent(), 'prompts');
});

async function assertFits(page) {
  const geometry = await page.evaluate(() => {
    const dock = document.querySelector('.page-bottom-dock').getBoundingClientRect();
    return { width: innerWidth, client: document.documentElement.clientWidth, scroll: document.documentElement.scrollWidth,
      dockLeft: dock.left, dockRight: dock.right, dockBottom: dock.bottom, height: innerHeight };
  });
  assert.equal(geometry.width, geometry.client, 'layout viewport must not expand beyond the phone');
  assert.equal(geometry.scroll, geometry.client, 'no horizontal page overflow');
  assert.ok(geometry.dockLeft >= 0 && geometry.dockRight <= geometry.client + 1, 'dock fits horizontally');
  assert.ok(geometry.dockBottom <= geometry.height + 1, 'navigation remains onscreen');
}

for (const viewport of [{ width: 390, height: 844 }, { width: 430, height: 932 }, { width: 440, height: 956 }, { width: 932, height: 430 }, { width: 430, height: 500 }]) {
  test(`phone ${viewport.width}×${viewport.height}: navigate, scroll to the last library entry, and keep Run accessible`, async t => {
    const { page } = await openPage(t, 'studio-library', viewport);
    await page.locator('.library-task-option').last().waitFor();
    assert.equal(await page.locator('#github-issues-dock-link').isVisible(), true, 'GitHub Issues is a primary mobile destination');
    assert.equal(await page.locator('#github-issues-dock-link').evaluate(node => node.parentElement.id), '');
    await assertFits(page);
    const last = page.locator('.library-task-option-toggle').last();
    await last.scrollIntoViewIfNeeded();
    await last.tap();
    assert.equal(await page.locator('.library-task-option-toggle').last().getAttribute('aria-pressed'), 'true');
    assert.ok(await page.evaluate(() => scrollY > 0), 'the document scrolls through entries');
    const submit = await page.locator('#local-prompt-submit').boundingBox();
    const dock = await page.locator('.page-bottom-dock').boundingBox();
    assert.ok(submit.y >= 0 && submit.y + submit.height <= dock.y + 1, 'Run is above the dock');
    await page.locator('#mobile-dock-more').tap();
    await page.locator('#prompt-mode-json').tap();
    await page.waitForFunction(() => document.body.dataset.promptMode === 'json');
    assert.equal(await page.locator('#mobile-dock-more').getAttribute('aria-expanded'), 'false');
    await assertFits(page);
    await page.locator('#task-view-dock-link').tap();
    await page.locator('#task-list .task').last().waitFor();
    await page.locator('#task-list .task').last().scrollIntoViewIfNeeded();
    assert.ok(await page.locator('#task-panel .task-scroll').evaluate(node => node.scrollTop > 0));
    await assertFits(page);
    await page.locator('#task-history-toggle').tap();
    await page.waitForFunction(() => document.querySelectorAll('#task-list .task').length === 24);
    await page.locator('#task-list .task').last().scrollIntoViewIfNeeded();
  });
}

test('swiping from a task action scrolls without firing it; tapping still opens output', async t => {
  const { page, writes } = await openPage(t, 'current-work');
  const output = page.locator('#task-list .task-output-toggle').first();
  await output.waitFor();
  const box = await output.boundingBox();
  const client = await page.context().newCDPSession(page);
  const x = box.x + box.width / 2, y = box.y + box.height / 2;
  await client.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
  assert.equal(await page.locator('#task-fullscreen').getAttribute('aria-hidden'), 'true', 'touch-down must not activate output');
  for (let step = 1; step <= 8; step++) {
    await client.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: y - step * 20 }] });
  }
  await client.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  assert.equal(await page.locator('#task-fullscreen').getAttribute('aria-hidden'), 'true');
  assert.deepEqual(writes, [], 'scrolling never sends task controls');
  await page.waitForFunction(() => document.querySelector('#task-panel .task-scroll').scrollTop > 0);
  await output.tap();
  await page.waitForFunction(() => document.querySelector('#task-fullscreen').getAttribute('aria-hidden') === 'false');
  await page.locator('#task-fullscreen-close').tap();
  await page.waitForFunction(() => document.querySelector('#task-fullscreen').getAttribute('aria-hidden') === 'true');
});

test('running tasks omit the status icon and always expose their original prompt', async t => {
  const { page } = await openPage(t, 'current-work', { width: 1280, height: 900 }, false);
  const task = page.locator('#task-list .task').first();
  await task.waitFor();
  assert.equal(await task.locator('.badge').count(), 0, 'running task has no status icon');

  const prompt = task.locator('.task-prompt-toggle');
  const originalPrompt = await task.locator('.task-id').getAttribute('title');
  await prompt.waitFor();
  await prompt.click();
  await page.locator('#task-prompt-modal:not(.hidden)').waitFor();
  assert.equal(await page.locator('#task-prompt-modal-body').textContent(), originalPrompt);
});

test('task card distinguishes the titled pull request from the merged pull request', async t => {
  const trackedURL = 'https://github.com/example/project/pull/10';
  const titledURL = 'https://github.com/example/other-project/pull/20';
  const untitledURL = 'https://github.com/example/project/pull/30';
  const now = new Date().toISOString();
  const { page } = await openPage(t, 'current-work', { width: 1280, height: 900 }, false, [{
    request_id: 'multi-repo', prompt: 'Original prompt', repo: 'example/project',
    status: 'merged', started_at: now, updated_at: now,
    pr_url: trackedURL, pr_title_url: titledURL,
    pr_title: 'Improve repository navigation',
  }, {
    request_id: 'untitled-pr', prompt: 'Untitled PR prompt', repo: 'example/project',
    status: 'completed', started_at: now, updated_at: now, pr_url: untitledURL,
  }, {
    request_id: 'running-pr', prompt: 'Running PR prompt', repo: 'example/project',
    status: 'running', started_at: now, updated_at: now, pr_title_url: titledURL,
    issue_title: 'Original issue title', pr_title: 'Improve repository navigation',
  }, {
    request_id: 'queued-issue', prompt: 'Full issue prompt', repo: 'example/project',
    status: 'pending', started_at: now, updated_at: now, issue_title: 'Original issue title',
  }]);

  const queuedIssue = page.locator('#task-list .task[data-request-id="queued-issue"]');
  assert.equal(await queuedIssue.locator('.task-id').textContent(), 'Original issue title');
  const runningTask = page.locator('#task-list .task[data-request-id="running-pr"]');
  assert.equal(await runningTask.locator('.task-id').textContent(), 'Improve repository navigation');
  assert.equal(await runningTask.locator('a.task-progress-step[title^="GitHub:"]').getAttribute('href'), titledURL);
  await page.locator('#task-history-toggle').click();
  const task = page.locator('#task-list .task[data-request-id="multi-repo"]');
  await task.locator('.task-github-link').waitFor();
  assert.match(await task.locator('.task-id').textContent(), /Improve repository navigation/);
  assert.equal(await task.locator('.task-github-link').getAttribute('href'), titledURL);
  const mergedBadge = task.getByRole('link', { name: `Merged pull request: ${trackedURL}` });
  assert.equal(await mergedBadge.getAttribute('href', { timeout: 2000 }), trackedURL);
  assert.equal(await mergedBadge.getAttribute('title'), `Merged pull request: ${trackedURL}`);
  await page.evaluate(() => { window.open = url => { window.openedPR = url; }; });
  await mergedBadge.click();
  assert.equal(await page.evaluate(() => window.openedPR), trackedURL);
  const untitledTask = page.locator('#task-list .task[data-request-id="untitled-pr"]');
  assert.equal(await untitledTask.locator('.task-github-link').getAttribute('href'), untitledURL);

  await task.locator('.task-output-toggle').click();
  const result = page.locator('#task-fullscreen-result');
  await result.locator('.task-result-link').waitFor();
  assert.equal(await result.locator('.task-result-title').textContent(), 'PR Merged');
  assert.equal(await result.locator('.task-result-link').getAttribute('href'), trackedURL);
});

test('workflow-triggered tasks start with the Workflow icon and keep their task steps', async t => {
  const now = new Date().toISOString();
  const issueURL = 'https://github.com/example/project/issues/42';
  const issue = {
    repo: 'example/project', source: 'github_issues', github_issue_url: issueURL,
    started_at: now, updated_at: now, status: 'running', stage: 'agent',
  };
  const { page } = await openPage(t, 'current-work', { width: 1280, height: 900 }, false, [
    { ...issue, request_id: 'workflow-issue', workflow_triggered: true },
    { ...issue, request_id: 'direct-issue', status: 'pending', stage: 'task' },
    { ...issue, request_id: 'workflow-review', source: 'prompt', github_issue_url: '',
      workflow_triggered: true, workflow: 'code-review', stage: 'review' },
  ]);

  const stepsFor = id => page.locator(`#task-list .task[data-request-id="${id}"] .task-progress-step`);
  const current = page.locator('.task-progress-step.current');
  const workflowSteps = stepsFor('workflow-issue');
  await workflowSteps.first().locator('svg.lucide-diagram-flow-vertical').waitFor();
  assert.match(await workflowSteps.first().getAttribute('title'), /^Workflow: done\./);
  assert.equal(await workflowSteps.nth(1).getAttribute('href'), issueURL);
  assert.equal(await workflowSteps.nth(1).getAttribute('aria-label'), 'Open issue on GitHub.');
  assert.match(await workflowSteps.and(current).getAttribute('title'), /^Agent Run: current\./);

  const directSteps = stepsFor('direct-issue');
  assert.equal(await directSteps.first().getAttribute('href'), issueURL);
  assert.match(await directSteps.first().getAttribute('title'), /^GitHub issue: done\./);
  assert.match(await directSteps.and(current).getAttribute('title'), /^Prepare: current\./);

  const reviewSteps = stepsFor('workflow-review');
  await reviewSteps.first().locator('svg.lucide-diagram-flow-vertical').waitFor();
  assert.match(await reviewSteps.nth(1).getAttribute('title'), /^Review: done\./);
  await reviewSteps.nth(1).locator('svg.lucide-git-pull-request').waitFor();
  assert.match(await reviewSteps.and(current).getAttribute('title'), /^Code Review: current\./);
});

test('issue task adds review and fix markers as each pass starts', async t => {
  const now = new Date().toISOString();
  const task = {
    request_id: 'issue-review-cycle', source: 'github_issues', agent_harness: 'claude',
    prompt: 'Update navigation', repo: 'example/project', started_at: now, updated_at: now,
    status: 'running', stage: 'checks', pr_url: 'https://github.com/example/project/pull/10',
    logs: [{ stream: 'meta', text: 'task stage=config status=ok review_total=2' }],
  };
  let serverTasks = [task];
  const { page } = await openPage(t, 'current-work', { width: 320, height: 844 }, true, () => ({ tasks: serverTasks }));
  const card = page.locator('#task-list .task[data-request-id="issue-review-cycle"]');
  const current = card.locator('.task-progress-step.current');
  const reviewMarkers = card.locator('.task-progress-step[title^="Review"], .task-progress-step[title^="Fix Findings"]');
  const publish = () => page.evaluate(task => {
    render({ ...state.snapshot, tasks: [task] });
  }, task);
  const stage = async (name, log, label) => {
    task.stage = name;
    task.logs.push({ stream: 'meta', text: `task stage=${name} ${log}` });
    await publish();
    await card.locator(`.task-progress-step.current[title^="${label}:"]`).waitFor();
  };

  await card.locator('.task-progress-step[title^="Review: upcoming"]').waitFor();
  assert.equal(await reviewMarkers.count(), 1);
  await stage('review', 'status=start action=pass pass=1 repo_dir=.', 'Review (pass 1)');
  await current.locator('svg.lucide-clipboard-check').waitFor();
  assert.equal(await reviewMarkers.count(), 1);

  await stage('review_fix', 'status=start action=agent pass=1 repo_dir=.', 'Fix Findings (pass 1)');
  assert.equal(await current.locator('img').getAttribute('src'), '/static/logos/claude-code.svg');
  assert.equal(await reviewMarkers.count(), 2);
  await stage('review', 'status=warn action=check_pr_state pass=1 repo_dir=.', 'Fix Findings (pass 1)');
  await stage('review_checks', 'status=start action=verify pass=1 repo_dir=.', 'Review Checks (pass 1)');
  await stage('checks', 'status=running', 'Review Checks (pass 1)');
  await stage('checks', 'status=failed attempt=1 repo_dir=.', 'Review Checks (pass 1)');
  await stage('claude', 'status=start mode=remediation attempt=1 repo_dir=.', 'Fix Failed Checks (attempt 1)');
  assert.equal(await current.locator('img').getAttribute('src'), '/static/logos/claude-code.svg');
  await stage('checks', 'status=start attempt=2 repo_dir=.', 'Review Checks (pass 1)');
  await current.locator('svg.lucide-shield-check').waitFor();
  await card.locator('.task-progress-step.done[title^="Fix Failed Checks (attempt 1):"]').waitFor();

  await stage('review', 'status=start action=pass pass=2 repo_dir=.', 'Review (pass 2)');
  await stage('review_fix', 'status=start action=agent pass=2 repo_dir=.', 'Fix Findings (pass 2)');
  await stage('claude', 'status=running mode=review_fix attempt=2 repo_dir=.', 'Fix Findings (pass 2)');
  await stage('review_checks', 'status=start action=verify pass=2 repo_dir=.', 'Review Checks (pass 2)');
  await stage('claude', 'status=start mode=remediation attempt=1 repo_dir=.', 'Fix Failed Checks (attempt 1)');
  await stage('checks', 'status=start attempt=2 repo_dir=.', 'Review Checks (pass 2)');
  await stage('claude', 'status=start mode=remediation attempt=2 repo_dir=.', 'Fix Failed Checks (attempt 2)');
  await stage('checks', 'status=start attempt=3 repo_dir=.', 'Review Checks (pass 2)');
  const progress = card.locator('.task-progress');
  const scrollLeft = await progress.evaluate(node => {
    node.scrollLeft = 12;
    return node.scrollLeft;
  });
  assert.equal(scrollLeft, 12, 'user can scroll back to earlier steps');
  task.logs.push({ stream: 'stdout', text: 'More review output' });
  await publish();
  assert.equal(await progress.evaluate(node => node.scrollLeft), scrollLeft, 'live output preserves horizontal scroll');

  await stage('review', 'status=start action=pass pass=3 repo_dir=.', 'Review (pass 3)');
  assert.deepEqual(await reviewMarkers.evaluateAll(nodes => nodes.map(node => node.title.split(':')[0])), [
    'Review (pass 1)', 'Fix Findings (pass 1)', 'Review Checks (pass 1)',
    'Review (pass 2)', 'Fix Findings (pass 2)', 'Review Checks (pass 2)', 'Review (pass 3)',
  ]);
  await current.locator('svg.lucide-clipboard-check').waitFor();
  assert.equal(await current.evaluate(node => {
    const marker = node.getBoundingClientRect();
    const track = node.closest('.task-progress').getBoundingClientRect();
    return marker.left >= track.left && marker.right <= track.right;
  }), true, 'a newly active step scrolls into view on a narrow screen');

  await stage('review', 'status=ok action=pass pass=3 findings=0 repo_dir=.', 'Review (pass 3)');
  await stage('git', 'status=start action=check_mergeability', 'Review (pass 3)');
  await stage('claude', 'status=start mode=base_sync_conflict attempt=1 repo_dir=.', 'Resolve Merge Conflicts');
  assert.equal(await current.locator('img').getAttribute('src'), '/static/logos/claude-code.svg');
  assert.match(await reviewMarkers.last().getAttribute('title'), /^Review \(pass 3\): done/);
  await stage('git', 'status=start action=sync_base_commit repo_dir=.', 'Resolve Merge Conflicts');
  await stage('claude', 'status=start mode=remote_branch_sync_conflict attempt=1 repo_dir=.', 'Resolve Merge Conflicts');
  assert.equal(await card.locator('.task-progress-step[title^="Resolve Merge Conflicts:"]').count(), 1);

  task.status = 'completed';
  task.logs.push(...Array.from({ length: 510 }, () => ({ stream: 'stdout', text: 'Agent output' })));
  await publish();
  await page.locator('#task-history-toggle').click();
  await card.locator('.task-github-link').first().waitFor();
  serverTasks = [];
  await page.reload();
  await card.locator('.task-github-link').first().waitFor();
  const restored = await page.evaluate(id => {
    const savedTask = state.taskHistoryByID.get(id);
    const { steps, index } = taskProgressModel(savedTask);
    const progress = renderTaskProgress(savedTask);
    return {
      active: steps[index].id,
      reviews: steps.filter(step => step.label.includes('(pass ')).length,
      conflictLogo: progress.querySelector('.task-progress-step.done[title^="Resolve Merge Conflicts:"] img')?.getAttribute('src'),
      remediations: progress.querySelectorAll('.task-progress-step.done[title^="Fix Failed Checks"]').length,
    };
  }, task.request_id);
  assert.deepEqual(restored, {
    active: 'finalize', reviews: 7, conflictLogo: '/static/logos/claude-code.svg', remediations: 3,
  });
});

test('multi-repository task highlights initial checks for the current repository', async t => {
  const now = new Date().toISOString();
  const task = {
    request_id: 'multi-repo-review', prompt: 'Update navigation', repo: 'example/project, example/other',
    agent_harness: 'codex',
    started_at: now, updated_at: now, status: 'running', stage: 'review_checks',
    pr_url: 'https://github.com/example/project/pull/10',
    logs: [
      { stream: 'meta', text: 'task stage=review status=start action=pass pass=1 repo_dir=first' },
      { stream: 'meta', text: 'task stage=review_fix status=start action=agent pass=1 repo_dir=first' },
      { stream: 'meta', text: 'task stage=review_checks status=start action=verify pass=1 repo_dir=first' },
    ],
  };
  const { page } = await openPage(t, 'current-work', { width: 1280, height: 900 }, false, [task]);
  const card = page.locator('#task-list .task[data-request-id="multi-repo-review"]');
  const current = card.locator('.task-progress-step.current');
  const reviews = card.locator('.task-progress-step[title^="Review (pass 1):"]');
  const doneReviews = card.locator('.task-progress-step.done[title*="(pass 1):"]');
  await current.waitFor();
  assert.match(await current.getAttribute('title'), /^Review Checks \(pass 1\): current/);

  const publish = () => page.evaluate(task => render({ ...state.snapshot, tasks: [task] }), task);
  task.stage = 'codex';
  task.logs.push({ stream: 'meta', text: 'task stage=codex status=start mode=remediation attempt=1 repo_dir=first' });
  await publish();
  assert.match(await current.getAttribute('title'), /^Fix Failed Checks \(attempt 1\): current/);
  assert.equal(await current.locator('img').getAttribute('src'), '/static/logos/codex-cli.svg');

  task.stage = 'checks';
  task.pr_url = 'https://github.com/example/other/pull/10';
  task.logs.push({ stream: 'meta', text: 'task stage=checks status=start repo_dir=second' });
  await publish();
  assert.match(await current.getAttribute('title'), /^Checks: current/);
  assert.equal(await reviews.count(), 1);
  assert.equal(await doneReviews.count(), 3);

  task.logs.push({ stream: 'stdout', text: 'Waiting for checks' });
  await page.reload();
  assert.match(await current.getAttribute('title'), /^Checks: current/);
  assert.equal(await doneReviews.count(), 3);

  task.stage = 'review';
  task.logs.push({ stream: 'meta', text: 'task stage=review status=start action=pass pass=1 repo_dir=second' });
  await publish();
  assert.equal(await reviews.count(), 2);
  assert.match(await reviews.first().getAttribute('title'), /^Review \(pass 1\): done/);
  assert.match(await reviews.last().getAttribute('title'), /^Review \(pass 1\): current/);
  assert.equal(await doneReviews.count(), 3);
});

test('current work lists running tasks by oldest start time first', async t => {
  const now = Date.now();
  const tasks = [
    ['middle', 2, 0],
    ['newest', 1, 1],
    ['oldest', 3, 2],
  ].map(([id, startMinutesAgo, updateMinutesAgo]) => ({
    request_id: id, prompt: `Task ${id}`, repo: 'example/project', status: 'running',
    started_at: new Date(now - startMinutesAgo * 60_000).toISOString(),
    updated_at: new Date(now - updateMinutesAgo * 60_000).toISOString(),
  }));
  const { page } = await openPage(t, 'current-work', { width: 1280, height: 900 }, false, tasks);
  const cards = page.locator('#task-list .task[data-request-id]');
  await cards.last().waitFor();
  assert.deepEqual(await cards.evaluateAll(nodes => nodes.map(node => node.dataset.requestId)), [
    'oldest', 'middle', 'newest',
  ]);
});

test('legacy issue prompts restore task actions and workflow links', async t => {
  const now = new Date().toISOString();
  const issueTask = (id, number, status) => ({
    request_id: id, repo: 'example/project', status, started_at: now, updated_at: now,
    prompt: `Complete GitHub issue example/project#${number}: Update navigation\n\nIssue URL: https://github.com/example/project/issues/${number}\nState: OPEN`,
  });
  const running = issueTask('legacy-running', 17, 'running');
  const saved = issueTask('local-legacy-history', 18, 'completed');
  saved.source = 'github_issues';
  saved.prompt = '#18: Update navigation\n\nIssue description\n\nComments:\n(No comments.)';
  const unrelated = issueTask('ordinary-history', 19, 'completed');
  unrelated.prompt = `Review this sample prompt:\n\n${unrelated.prompt}`;
  const lookalike = issueTask('lookalike-history', 20, 'completed');
  lookalike.prompt = lookalike.prompt.replace('github.com/', 'github.com.example/');
  const chat = { ...issueTask('local-chat-issue-prompt', 21, 'running'), source: 'chat' };
  const { page } = await openPage(t, 'current-work', { width: 1280, height: 900 }, false, [running, chat]);
  await page.evaluate(tasks => localStorage.setItem('hubui.taskHistory.v1', JSON.stringify(tasks)), [saved, unrelated, lookalike]);
  await page.reload();

  const runningCard = page.locator('#task-list .task[data-request-id="legacy-running"]');
  const workflowLink = runningCard.locator('.task-progress-step').first();
  const actionLink = runningCard.locator('.task-footer').getByRole('link', { name: 'Open issue on GitHub.' });
  for (const link of [workflowLink, actionLink]) {
    assert.equal(await link.getAttribute('href'), 'https://github.com/example/project/issues/17');
  }

  const chatCard = page.locator('#task-list .task[data-request-id="local-chat-issue-prompt"]');
  await chatCard.locator('.task-progress-step').first().waitFor();
  assert.equal(await chatCard.locator('.task-progress-step').first().locator('svg.lucide-message-circle').isVisible(), true);
  assert.equal(await chatCard.getByRole('link', { name: 'Open issue on GitHub.' }).count(), 0);

  await page.locator('#task-history-toggle').click();
  const savedCard = page.locator('#task-list .task[data-request-id="local-legacy-history"]');
  const savedLink = savedCard.getByRole('link', { name: 'Open issue on GitHub.' });
  assert.equal(await savedLink.getAttribute('href'), 'https://github.com/example/project/issues/18');
  for (const id of ['ordinary-history', 'lookalike-history']) {
    const card = page.locator(`#task-list .task[data-request-id="${id}"]`);
    await card.waitFor();
    assert.equal(await card.getByRole('link', { name: 'Open issue on GitHub.' }).count(), 0);
  }
});

test('task history picks up a verified manual PR after the task expires', async t => {
  const requestID = 'expired-issue-task';
  const prURL = 'https://github.com/example/project/pull/42';
  const completedAt = new Date(Date.now() - 15 * 60_000).toISOString();
  const verifiedPR = { url: prURL, title: 'Manually opened PR', verified_at: new Date().toISOString() };
  let snapshot = { tasks: [{
    request_id: requestID, prompt: 'Original prompt', repo: 'example/project',
    status: 'error', started_at: completedAt, updated_at: completedAt, issue_title: 'Original issue title',
  }] };
  const { page } = await openPage(t, 'current-work', { width: 1280, height: 900 }, false, () => snapshot);
  await page.waitForFunction(id => JSON.parse(localStorage.getItem('hubui.taskHistory.v1') || '[]')
    .some(task => task.request_id === id), requestID);

  snapshot = { tasks: [], verified_task_prs: { [requestID]: verifiedPR, 'unknown-task': verifiedPR } };
  await page.reload();
  await page.locator('#task-history-toggle').click();
  const task = page.locator(`#task-list .task[data-request-id="${requestID}"]`);
  await task.getByText(verifiedPR.title, { exact: true }).waitFor();
  assert.equal(await task.locator('.task-github-link').getAttribute('href'), prURL);
  assert.equal(await page.locator('#task-list .task[data-request-id="unknown-task"]').count(), 0);
  const saved = await page.evaluate(id => JSON.parse(localStorage.getItem('hubui.taskHistory.v1'))
    .find(task => task.request_id === id), requestID);
  assert.equal(saved.history_completed_at, completedAt);
  assert.equal(saved.prompt, 'Original prompt');
  assert.equal(saved.pr_title_updated_at, verifiedPR.verified_at);
});

test('task history keeps a renamed PR title when the live task expires', async t => {
  const requestID = 'renamed-pr-task';
  const prURL = 'https://github.com/example/project/pull/42';
  const now = Date.now();
  const completedAt = new Date(now - 15 * 60_000).toISOString();
  const renamedAt = new Date(now - 60_000).toISOString();
  const verifiedPR = { url: prURL, title: 'Original PR title', verified_at: new Date(now - 120_000).toISOString() };
  let snapshot = { tasks: [{
    request_id: requestID, prompt: 'Original prompt', repo: 'example/project',
    status: 'completed', started_at: completedAt, updated_at: completedAt,
    pr_url: prURL, pr_title_url: prURL, pr_title: 'Renamed PR title', pr_title_updated_at: renamedAt,
  }] };
  const { page } = await openPage(t, 'current-work', { width: 1280, height: 900 }, false, () => snapshot);
  await page.waitForFunction(id => JSON.parse(localStorage.getItem('hubui.taskHistory.v1') || '[]')
    .some(task => task.request_id === id && task.pr_title === 'Renamed PR title'), requestID);

  snapshot = { tasks: [], verified_task_prs: { [requestID]: verifiedPR } };
  await page.reload();
  await page.locator('#task-history-toggle').click();
  const task = page.locator(`#task-list .task[data-request-id="${requestID}"]`);
  assert.equal(await task.locator('.task-id').textContent(), 'Renamed PR title');
  assert.equal(await task.locator('.task-github-link').getAttribute('href'), prURL);
});

for (const { name, trackedPR, titledPR, expectedTitle } of [
  { name: 'different PR', trackedPR: 42, titledPR: 42, expectedTitle: 'Archived PR title' },
  { name: 'tracked PR with a different card PR', trackedPR: 43, titledPR: 42, expectedTitle: 'Archived PR title' },
  { name: 'card PR with a different tracked PR', trackedPR: 42, titledPR: 43, expectedTitle: 'Archived PR title' },
  { name: 'same PR', trackedPR: 43, titledPR: 43, expectedTitle: 'Verified PR title' },
]) {
  test(`task history after recovery verifies ${name}`, async t => {
    const requestID = 'archived-issue-task';
    const prURL = number => `https://github.com/example/project/pull/${number}`;
    const completedAt = new Date(Date.now() - 15 * 60_000).toISOString();
    const verifiedPR = { url: prURL(43), title: 'Verified PR title', verified_at: new Date().toISOString() };
    let snapshot = { tasks: [{
      request_id: requestID, prompt: 'Original prompt', repo: 'example/project',
      status: 'merged', started_at: completedAt, updated_at: completedAt,
      pr_url: prURL(trackedPR), pr_title_url: prURL(titledPR),
      pr_title: 'Archived PR title', pr_title_updated_at: completedAt,
    }] };
    const { page } = await openPage(t, 'current-work', { width: 1280, height: 900 }, false, () => snapshot);
    await page.waitForFunction(id => JSON.parse(localStorage.getItem('hubui.taskHistory.v1') || '[]')
      .some(task => task.request_id === id), requestID);

    snapshot = { tasks: [], verified_task_prs: { [requestID]: verifiedPR } };
    await page.reload();
    await page.locator('#task-history-toggle').click();
    const task = page.locator(`#task-list .task[data-request-id="${requestID}"]`);
    assert.equal(await task.locator('.task-id').textContent(), expectedTitle);
    assert.equal(await task.locator('.task-github-link').getAttribute('href'), prURL(titledPR));
    const savedPRURL = await page.evaluate(id => JSON.parse(localStorage.getItem('hubui.taskHistory.v1'))
      .find(task => task.request_id === id).pr_url, requestID);
    assert.equal(savedPRURL, prURL(trackedPR));
  });
}

for (const viewport of [{ width: 430, height: 932 }, { width: 932, height: 430 }]) {
  test(`Enter adds a newline to the Studio prompt without submitting on mobile ${viewport.width}×${viewport.height}`, async t => {
    const { page, writes } = await openPage(t, 'studio', viewport);
    const prompt = page.locator('#builder-prompt-input');
    await prompt.fill('First line');
    await prompt.press('Enter');
    await prompt.type('Second line');
    assert.equal(await prompt.inputValue(), 'First line\nSecond line');
    assert.deepEqual(writes, [], 'mobile Enter must not start a run');
  });
}

test('More supports Escape, outside dismissal, settings and resizing back to desktop', async t => {
  const { page } = await openPage(t, 'current-work');
  const more = page.locator('#mobile-dock-more');
  await more.tap();
  await page.keyboard.press('Escape');
  assert.equal(await more.getAttribute('aria-expanded'), 'false');
  assert.equal(await more.evaluate(node => document.activeElement === node), true);
  await more.tap();
  await page.locator('#task-panel-title').tap();
  assert.equal(await more.getAttribute('aria-expanded'), 'false');
  await more.tap();
  await page.locator('#settings-open').tap();
  await page.locator('#settings-gate:not(.hidden)').waitFor();
  await page.locator('#review-settings-close').tap();
  await page.setViewportSize({ width: 1280, height: 900 });
  assert.equal(await more.isVisible(), false);
  assert.equal(await page.locator('#mobile-dock-secondary').evaluate(node => node.inert), false);
  assert.equal(await page.locator('#prompt-mode-json').isVisible(), true);
  await page.locator('#prompt-mode-json').click();
  await page.waitForFunction(() => document.body.dataset.promptMode === 'json');
});

for (const viewport of [{ width: 430, height: 932 }, { width: 932, height: 430 }]) {
  test(`repository navigation ${viewport.width}: scroll, paginate, search and open an entry`, async t => {
    const { page, writes } = await openPage(t, 'chat', viewport);
    const cards = page.locator('#chat-repo-grid .chat-repo-card');
    await cards.last().waitFor();
    await assertFits(page);
    const overlaps = await cards.evaluateAll(nodes => nodes.slice(1).some((node, index) => {
      const previous = nodes[index].getBoundingClientRect(), current = node.getBoundingClientRect();
      return current.top < previous.bottom - 1 && current.left < previous.right - 1;
    }));
    assert.equal(overlaps, false, 'repository cards must not overlap the open chat');
    await cards.last().scrollIntoViewIfNeeded();
    assert.ok(await page.locator('#chat-repo-grid').evaluate(node => node.scrollTop > 0));
    const next = page.getByRole('button', { name: 'Next', exact: true });
    const nextBox = await next.boundingBox(), dock = await page.locator('.page-bottom-dock').boundingBox();
    assert.ok(nextBox.y >= 0 && nextBox.y + nextBox.height <= dock.y, 'pagination stays above navigation');
    await next.tap();
    assert.match(await page.locator('.chat-repo-page-label').textContent(), /Page 2 of/);
    await page.locator('#chat-repo-search').fill('project-40');
    await page.waitForFunction(() => document.querySelectorAll('#chat-repo-grid .chat-repo-card').length === 1);
    const result = cards.first();
    assert.match(await result.textContent(), /project-40/);
    await result.tap();
    await page.waitForFunction(() => document.querySelector('#chat-repo-grid .chat-repo-card')?.getAttribute('aria-expanded') === 'true');
    await page.locator('.chat-repo-prompt:visible').fill('A draft typed on a phone');
    assert.deepEqual(writes, [], 'browsing and drafting never starts a run');
  });
}

for (const viewport of [{width:1440,height:1000},{width:1024,height:768}]) {
 test(`Studio prompt fills available space with padded edges at ${viewport.width}×${viewport.height}`, async t => {
  const {page}=await openPage(t,'studio',viewport,false);
  const prompt=page.locator('#builder-prompt-input');
  const geometry=async()=>page.evaluate(()=>{
   const input=document.getElementById('builder-prompt-input'), box=input.getBoundingClientRect();
   const panel=document.getElementById('prompt-builder-panel').getBoundingClientRect();
   const actions=document.querySelector('.prompt-actions').getBoundingClientRect();
   const form=document.getElementById('local-prompt-form').getBoundingClientRect();
   const style=getComputedStyle(input);
   return {height:box.height,gap:actions.top-box.bottom,left:box.left-form.left,right:form.right-box.right,panelGap:panel.bottom-box.bottom,padding:parseFloat(style.paddingTop),scrolls:input.scrollHeight>input.clientHeight,actionsBottom:actions.bottom,viewport:innerHeight};
  });
  const before=await geometry();
  assert.ok(before.height>250,JSON.stringify(before));
  assert.ok(Math.abs(before.panelGap)<2,JSON.stringify(before));
  assert.ok(before.gap>=6&&before.gap<=20,JSON.stringify(before));
  assert.ok(before.left>=10&&before.left<=16&&before.right>=10&&before.right<=16,JSON.stringify(before));
  assert.ok(before.padding>=10);assert.ok(before.actionsBottom<before.viewport);
  await prompt.fill(Array.from({length:150},(_,i)=>`Prompt line ${i}`).join('\n'));
  const after=await geometry();assert.ok(Math.abs(after.height-before.height)<2,JSON.stringify({before,after}));assert.equal(after.scrolls,true);
  await page.setViewportSize({width:viewport.width,height:viewport.height+160});
  await page.waitForFunction(h=>document.getElementById('builder-prompt-input').getBoundingClientRect().height>h+140,before.height);
  if(viewport.width===1440)await page.screenshot({path:'/tmp/agent00-prompt-fill.png',fullPage:true});
 });
}

for (const viewport of [{width:1440,height:1000},{width:1024,height:768}]) {
 test(`Library tasks occupy remaining Studio space without a spacer at ${viewport.width}×${viewport.height}`, async t => {
  const {page}=await openPage(t,'studio-library',viewport,false);
  await page.locator('#library-task-list > *').first().waitFor();
  const geometry=async()=>page.evaluate(()=>{
   const panel=document.getElementById('prompt-library-panel');
   const copy=panel.querySelector('.prompt-field-copy').getBoundingClientRect();
   const tasks=panel.querySelector('.prompt-field-task-list').getBoundingClientRect();
   const list=document.getElementById('library-task-list').getBoundingClientRect();
   const actions=document.querySelector('.prompt-actions').getBoundingClientRect();
   return {gap:tasks.top-copy.bottom,height:list.height,bottomGap:actions.top-list.bottom,scrollable:document.getElementById('library-task-list').scrollHeight>list.height};
  });
  const before=await geometry();
  assert.ok(before.gap>=6&&before.gap<=16,JSON.stringify(before));
  assert.ok(before.height>180,JSON.stringify(before));
  assert.ok(before.bottomGap>=6&&before.bottomGap<=32,JSON.stringify(before));
  assert.equal(before.scrollable,true);
  await page.setViewportSize({width:viewport.width,height:viewport.height+160});
  await page.waitForFunction(h=>document.getElementById('library-task-list').getBoundingClientRect().height>h+140,before.height);
  assert.ok((await geometry()).gap<=16);
  if(viewport.width===1440)await page.screenshot({path:'/tmp/agent00-library-layout-fixed.png',fullPage:true});
 });
}
