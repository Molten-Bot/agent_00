// Run against a local harness: HARNESS_TEST_URL=http://127.0.0.1:7777 node --test internal/web/tests/settings_tabs.cjs
// Requires Playwright and Chromium (both are included in the runtime Docker image).
//
// The Go-level CSS assertions in server_additional_test.go only check the
// declarations present in style.css; they can't catch a regression where the
// tabbar's actual rendered layout clips a revealed label or reflows sibling
// tabs. These tests render the real page and measure it.
const { before, after, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
before(async () => { browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

async function openSettings(t, viewport = { width: 1280, height: 900 }) {
  const page = await browser.newPage({ viewport });
  t.after(() => page.close());
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(() => assert.deepEqual(errors, [], 'no browser JavaScript errors'));
  await page.route('**/*', route => new URL(route.request().url()).origin === new URL(baseURL).origin ? route.continue() : route.abort());
  await page.route('**/api/agent-auth**', route => route.fulfill({ json: { ok: true, auth: { required: true, ready: true, harness: 'claude', state: 'ready' } } }));
  await page.route('**/api/review-settings', route => route.fulfill({ json: { ok: true, settings: { merge_method: 'squash', review_level: 'off' } } }));
  await page.route('**/api/commit-signing**', route => route.fulfill({ json: { ok: true, settings: {} } }));
  await page.goto(`${baseURL}/#settings`);
  await page.waitForSelector('.settings-tabbar .settings-tab.active');
  return page;
}

test('hovering an inactive settings tab reveals its label without the tabbar clipping it', async t => {
  const page = await openSettings(t);
  const tabbar = page.locator('.settings-tabbar');
  const label = page.locator('#settings-tab-automation span');

  await page.locator('#settings-tab-automation').hover();
  await page.waitForFunction(() => {
    const span = document.querySelector('#settings-tab-automation span');
    return !!span && getComputedStyle(span).opacity === '1';
  });

  const overflow = await tabbar.evaluate(el => ({ scrollHeight: el.scrollHeight, clientHeight: el.clientHeight }));
  assert.ok(
    overflow.scrollHeight <= overflow.clientHeight + 1,
    `tabbar overflowed vertically while the label was revealed (scrollHeight=${overflow.scrollHeight}, clientHeight=${overflow.clientHeight}); the label is being clipped`
  );

  const labelBox = await label.boundingBox();
  const tabbarBox = await tabbar.boundingBox();
  assert.ok(labelBox && tabbarBox, 'expected both the label and tabbar to be rendered');
  assert.ok(
    labelBox.y >= tabbarBox.y - 1 && labelBox.y + labelBox.height <= tabbarBox.y + tabbarBox.height + 1,
    `revealed label rendered outside the tabbar bounds (label=${JSON.stringify(labelBox)}, tabbar=${JSON.stringify(tabbarBox)})`
  );
});

test('selecting a different settings tab does not shift later sibling tabs', async t => {
  const page = await openSettings(t);
  const signingTab = page.locator('#settings-tab-signing');

  const before = await signingTab.boundingBox();
  await page.locator('#settings-tab-automation').click();
  await page.waitForFunction(() => document.getElementById('settings-tab-automation').classList.contains('active'));
  // The click leaves the pointer resting on the activated tab, which also
  // satisfies :hover - move away so the measurement reflects the settled,
  // non-hovered selection state rather than the transient hover reveal that
  // test 1 already covers.
  await page.mouse.move(0, 0);
  // The label reveal animates max-width/margin-left over 0.2s and opacity
  // over 0.15s - wait for the layout-affecting (max-width/margin-left)
  // transitionend instead of a fixed sleep, so the measurement reflects the
  // settled layout without flaking under CI scheduling jitter. Opacity ends
  // first and doesn't affect layout, so it's ignored here.
  await page.evaluate(() => new Promise(resolve => {
    const span = document.querySelector('#settings-tab-automation span');
    if (!span) { resolve(); return; }
    let done = false;
    const finish = event => {
      if (done || (event && event.propertyName === 'opacity')) return;
      done = true;
      span.removeEventListener('transitionend', finish);
      resolve();
    };
    span.addEventListener('transitionend', finish);
    setTimeout(finish, 1000);
  }));
  const after = await signingTab.boundingBox();

  assert.ok(before && after, 'expected the signing tab to be rendered before and after switching tabs');
  assert.equal(
    Math.round(after.x), Math.round(before.x),
    `activating a different tab shifted a later sibling tab horizontally (before.x=${before.x}, after.x=${after.x})`
  );
});
