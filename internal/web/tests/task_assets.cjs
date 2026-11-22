// The Go fixture serves real issue-derived run configs through the broker API.
const { after, before, test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const { expect } = require('playwright/test');
const baseURL = process.env.HARNESS_TEST_URL || 'http://127.0.0.1:7777';
let browser;
const screenshotURL = 'https://github.com/user-attachments/assets/screenshot-id';
const pdfURL = 'https://github.com/user-attachments/files/42/spec.pdf';
const unavailableURL = 'https://github.com/user-attachments/assets/unavailable';
const signedImageURL = 'https://private-user-images.githubusercontent.com/42/private.png?jwt=fixture%2Bsignature';
const expiredImageURL = 'https://private-user-images.githubusercontent.com/42/expired.png?jwt=expired-fixture';
const pixelPNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/P2cAAAAASUVORK5CYII=', 'base64');

before(async () => { browser = await chromium.launch({ channel: 'chromium', headless: true, args: ['--no-sandbox'] }); });
after(async () => { await browser?.close(); });

for (const mobile of [false, true]) {
  test(`${mobile ? 'mobile' : 'desktop'}: task assets open in a navigable preview dialog`, async t => {
    const page = await browser.newPage({
      viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 },
      isMobile: mobile, hasTouch: mobile,
    });
    page.setDefaultTimeout(10000);
    t.after(() => page.close());
    const errors = [], writes = [], attachmentRequests = [], signedImageRequests = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => {
      if (message.type() === 'error' && !message.text().startsWith('Failed to load resource:')) errors.push(message.text());
    });
    await page.context().route(/^https?:/, route => {
      const url = new URL(route.request().url());
      if (url.href === signedImageURL || url.href === expiredImageURL) {
        signedImageRequests.push({
          url: url.href,
          type: route.request().resourceType(),
          referer: route.request().headers().referer,
          authorization: route.request().headers().authorization,
        });
        return url.href === signedImageURL
          ? route.fulfill({ contentType: 'image/png', body: pixelPNG })
          : route.fulfill({ status: 403 });
      }
      if (url.origin !== new URL(baseURL).origin) {
        if (url.pathname.includes('/user-attachments/')) errors.push('Browser requested a GitHub attachment');
        return route.abort();
      }
      if (!url.pathname.startsWith('/api/')) return route.continue();
      if (route.request().method() !== 'GET') writes.push(url.pathname);
      if (url.pathname.endsWith('/asset-preview')) {
        attachmentRequests.push([url.pathname + url.search, decodeURIComponent(route.request().headers()['x-task-asset-url'])]);
        return route.continue();
      }
      if (url.pathname === '/api/status') return route.continue();
      if (url.pathname === '/api/stream') return route.fulfill({ contentType: 'text/event-stream', body: '' });
      let json = { ok: true };
      if (url.pathname === '/api/agent-auth') json.auth = { required: false, ready: true, state: 'ready' };
      if (url.pathname === '/api/review-settings') json.settings = { github_issues_enabled: true };
      if (url.pathname === '/api/github/repos') json.repos = [];
      if (url.pathname === '/api/library') json.tasks = [];
      return route.fulfill({ json });
    });

    await page.goto(`${baseURL}/#current-work`);
    const task = page.locator('#task-list .task[data-request-id="req-assets"]');
    const assets = task.locator('.task-assets-toggle');
    await expect(assets).toHaveAccessibleName('View 2 task assets');
    await expect(assets.locator('svg.lucide-images')).toBeVisible();
    await expect(assets).toHaveText('2');
    await expect(page.locator('#task-list .task[data-request-id="req-no-assets"] .task-assets-toggle')).toHaveCount(0);
    assert.deepEqual(attachmentRequests, [], 'previews load only when requested');
    const viewer = page.locator('#task-asset-viewer');
    await expect(viewer).toBeHidden();
    await expect(page.locator('link[href="/static/style.css?v=10"]')).toHaveCount(1);
    if (mobile) {
      await assets.tap();
    } else {
      await assets.scrollIntoViewIfNeeded();
      const originalButton = await assets.elementHandle();
      const box = await assets.boundingBox();
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
      await page.mouse.down();
      // A changed task snapshot replaces the card before the pointer is released.
      await page.evaluate(() => render({
        ...state.snapshot,
        tasks: state.snapshot.tasks.map(task => task.request_id === 'req-assets'
          ? { ...task, issue_title: 'Updated issue title from live snapshot' }
          : task),
      }));
      assert.equal(await originalButton.evaluate(button => button.isConnected), false);
      await page.mouse.up();
    }
    const previous = viewer.getByRole('button', { name: 'Previous asset' });
    const next = viewer.getByRole('button', { name: 'Next asset' });
    const source = viewer.getByRole('link', { name: 'Open on GitHub' });
    const count = viewer.locator('#task-asset-count');
    await expect(viewer).toBeVisible();
    await expect(page.locator('#task-prompt-modal')).toBeHidden();
    await expect(viewer.getByRole('img', { name: 'screenshot-id' })).toBeVisible();
    await expect(count).toHaveText('1 / 2');
    await expect(source).toHaveAttribute('href', screenshotURL);
    assert.deepEqual(attachmentRequests, [['/api/tasks/req-assets/asset-preview', screenshotURL]]);
    const pdfResponse = page.waitForResponse(response =>
      response.url().endsWith('/asset-preview') && decodeURIComponent(response.request().headers()['x-task-asset-url']) === pdfURL);
    await next.click();
    const response = await pdfResponse;
    assert.equal(response.status(), 200);
    assert.equal(response.headers()['content-type'], 'application/pdf');
    assert.equal(response.headers()['cache-control'], 'no-store');
    assert.equal(response.headers()['authorization'], undefined);
    await expect(viewer).toHaveAccessibleName('spec.pdf');
    await expect(viewer.getByTitle('spec.pdf')).toBeVisible();
    await expect(viewer.getByTitle('spec.pdf')).toHaveAttribute('src', /^blob:/);
    await expect(viewer.getByRole('status')).toBeHidden();
    const pdfFrame = () => page.frames().find(frame =>
      frame.url().startsWith('chrome-extension:') && frame.parentFrame()?.url().startsWith('blob:'),
    );
    await expect.poll(() => Boolean(pdfFrame())).toBe(true);
    await expect(pdfFrame().locator('pdf-viewer embed')).toBeVisible();
    await expect(source).toHaveAttribute('href', pdfURL);
    await expect(count).toHaveText('2 / 2');
    await previous.click();
    await expect(viewer.getByRole('img', { name: 'screenshot-id' })).toBeVisible();
    await page.keyboard.press('ArrowLeft');
    await expect(count).toHaveText('2 / 2');
    await expect.poll(() => attachmentRequests.length).toBe(4);
    await page.keyboard.press('Escape');
    await expect(viewer).toBeHidden();
    await expect(assets).toBeFocused();
    assert.deepEqual(attachmentRequests, [screenshotURL, pdfURL, screenshotURL, pdfURL].map(url => ['/api/tasks/req-assets/asset-preview', url]),
      'only the current asset preview is requested');

    const singleAsset = page.locator('#task-list .task[data-request-id="req-single-asset"] .task-assets-toggle');
    await expect(singleAsset).toHaveAccessibleName('View 1 task asset');
    await expect(singleAsset.locator('svg.lucide-image')).toBeVisible();
    await expect(singleAsset).toHaveText('');
    await singleAsset.click();
    await expect(viewer.getByRole('status')).toHaveText('Preview unavailable.');
    await expect(source).toHaveAttribute('href', unavailableURL);
    await expect(previous).toBeHidden();
    await expect(next).toBeHidden();
    await expect(page.locator('#task-asset-token')).toHaveCount(0);
    const authLink = viewer.getByRole('link', { name: 'Preview with GitHub token', exact: false });
    const [auth, authResponse] = await Promise.all([
      page.context().waitForEvent('page'),
      page.context().waitForEvent('response', { predicate: response => new URL(response.url()).pathname === '/task-asset-auth' }),
      authLink.click(),
    ]);
    auth.on('pageerror', error => errors.push(error.message));
    auth.on('console', message => {
      if (message.type() === 'error' && !message.text().startsWith('Failed to load resource:')) errors.push(message.text());
    });
    assert.equal(authResponse.headers()['cross-origin-opener-policy'], 'same-origin');
    assert.match(authResponse.headers()['content-security-policy'], /frame-ancestors 'none'/);
    assert.equal(await auth.evaluate(() => window.opener), null);
    assert.deepEqual(await auth.locator('script').evaluateAll(scripts => scripts.map(script => script.getAttribute('src'))),
      ['/static/task-asset-preview.js?v=3', '/static/task-asset-auth.js?v=2']);
    const token = auth.getByLabel('GitHub token with access to this attachment');
    const retry = auth.getByRole('button', { name: 'Retry preview' });
    await token.fill('denied-fixture');
    await retry.click();
    await expect(auth.getByRole('status')).toHaveText('GitHub denied access to this attachment.');
    await expect(token).toHaveValue('');
    const authClosed = auth.waitForEvent('close');
    await token.fill('viewer-fixture');
    await retry.click();
    await expect(viewer.locator('pre')).toHaveText('Private attachment preview');
    await expect(viewer.getByRole('status')).toBeHidden();
    await authClosed;
    await viewer.getByRole('button', { name: 'Close asset viewer' }).click();
    await expect(viewer).toBeHidden();
    await expect(singleAsset).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(viewer).toBeVisible();
    await expect(viewer.getByRole('status')).toHaveText('Preview unavailable.');
    const staleAuthOpening = page.context().waitForEvent('page');
    await authLink.click();
    const staleAuth = await staleAuthOpening;
    await page.keyboard.press('Escape');
    await expect(viewer).toBeHidden();

    const unsupported = page.locator('#task-list .task[data-request-id="req-unsupported-asset"] .task-assets-toggle');
    await unsupported.click();
    await expect(viewer.getByRole('status')).toHaveText('Preview unavailable.');
    await expect(viewer.locator('#task-asset-auth-link')).toBeHidden();
    await page.keyboard.press('Escape');

    const textAsset = page.locator('#task-list .task[data-request-id="req-text-asset"] .task-assets-toggle');
    await textAsset.focus();
    await page.keyboard.press('Enter');
    const textPreview = viewer.locator('pre');
    await expect(textPreview).toBeVisible();
    assert.equal(await textPreview.textContent(), "<script>throw new Error('attachment executed')</script>\nUTF-8: café");
    await expect(textPreview.locator('*')).toHaveCount(0);
    await expect(viewer.getByRole('status')).toBeHidden();
    const staleToken = staleAuth.getByLabel('GitHub token with access to this attachment');
    await staleToken.fill('viewer-fixture');
    await staleAuth.getByRole('button', { name: 'Retry preview' }).click();
    await expect(staleAuth.locator('pre')).toHaveText('Private attachment preview');
    await expect(staleToken).toBeHidden();
    await expect(staleToken).toHaveValue('');
    assert.equal(await textPreview.textContent(), "<script>throw new Error('attachment executed')</script>\nUTF-8: café",
      'late authorization cannot replace a different preview');
    await staleAuth.close();
    const htmlResponse = page.waitForResponse(response =>
      response.url().endsWith('/asset-preview') && decodeURIComponent(response.request().headers()['x-task-asset-url']).endsWith('/page.html'));
    await next.click();
    assert.equal((await htmlResponse).headers()['content-type'], 'text/plain; charset=utf-8');
    await expect(textPreview).toBeVisible();
    assert.equal(await textPreview.textContent(), '<!DOCTYPE html>\n<strong>Café</strong>');
    await expect(textPreview.locator('*')).toHaveCount(0);
    await page.keyboard.press('Escape');
    await expect(viewer.locator('pre')).toHaveCount(0);

    await page.locator('#task-list .task[data-request-id="req-audio-asset"] .task-assets-toggle').click();
    const audio = viewer.locator('audio');
    await expect(audio).toBeVisible();
    await expect(audio).toHaveJSProperty('controls', true);
    await expect(audio).toHaveJSProperty('duration', 1);
    await expect(viewer.getByRole('status')).toBeHidden();
    await next.click();
    await expect(audio).toHaveAccessibleName('silence.mp3');
    await expect(audio).toBeVisible();
    await audio.evaluate(element => element.play());
    await expect.poll(() => audio.evaluate(element => element.currentTime)).toBeGreaterThan(0);
    await page.keyboard.press('Escape');
    await expect(audio).toHaveCount(0);

    const streamResponse = page.waitForResponse(response => response.request().resourceType() === 'media' &&
      new URL(response.url()).pathname === '/api/tasks/req-video-asset/asset-preview');
    await page.locator('#task-list .task[data-request-id="req-video-asset"] .task-assets-toggle').click();
    const stream = await streamResponse;
    assert.equal(stream.status(), 200);
    assert.equal(stream.headers()['content-type'], 'video/webm');
    assert.equal(stream.headers()['content-encoding'], undefined);
    assert.equal(stream.request().headers().authorization, undefined);
    const video = viewer.locator('video');
    await expect(video).toBeVisible();
    await expect(video).toHaveAccessibleName('Recording');
    await video.evaluate(element => element.play());
    await expect.poll(() => video.evaluate(element => element.currentTime)).toBeGreaterThan(0);
    await page.keyboard.press('Escape');
    await expect(video).toHaveCount(0);

    await page.locator('#task-list .task[data-request-id="req-private-video"] .task-assets-toggle').click();
    await expect(authLink).toBeVisible();
    const privateAuthOpening = page.context().waitForEvent('page');
    await authLink.click();
    const privateAuth = await privateAuthOpening;
    await privateAuth.getByLabel('GitHub token with access to this attachment').fill('viewer-fixture');
    const privateAuthClosed = privateAuth.waitForEvent('close');
    await privateAuth.getByRole('button', { name: 'Retry preview' }).click();
    await expect(video).toBeVisible();
    await video.evaluate(element => element.play());
    await expect.poll(() => video.evaluate(element => element.currentTime)).toBeGreaterThan(0);
    await privateAuthClosed;
    const privateSource = await video.getAttribute('src');
    assert.ok(privateSource.includes('playback='));
    assert.ok(!privateSource.includes('viewer-fixture'));
    // The capability cannot authorize another task or video identity.
    const wrongTask = await page.request.get(new URL(privateSource.replace('req-private-video', 'req-video-asset'), page.url()).href);
    assert.equal(wrongTask.status(), 404);
    const wrongAsset = new URL(privateSource, page.url());
    wrongAsset.searchParams.set('video', '0'.repeat(64));
    assert.equal((await page.request.get(wrongAsset.href)).status(), 404);
    await next.click();
    await expect(viewer.locator('img')).toBeVisible();
    await previous.click();
    await expect(video).toBeVisible();
    await expect(authLink).toBeHidden();
    assert.equal(await video.getAttribute('src'), privateSource);
    await page.keyboard.press('Escape');
    await page.locator('#task-list .task[data-request-id="req-private-video"] .task-assets-toggle').click();
    await expect(authLink).toBeVisible();
    await page.keyboard.press('Escape');

    const proxyRequestsBeforePrivateImages = attachmentRequests.length;
    await page.locator('#task-list .task[data-request-id="req-private-images"] .task-assets-toggle').click();
    const privateImage = viewer.getByRole('img', { name: 'Private screenshot' });
    await expect(privateImage).toBeVisible();
    await expect(privateImage).toHaveJSProperty('naturalWidth', 1);
    await expect(source).toHaveAttribute('href', signedImageURL);
    await next.click();
    await expect(privateImage).toHaveCount(0);
    await expect(viewer.getByRole('status')).toHaveText('Preview unavailable.');
    await expect(source).toHaveAttribute('href', expiredImageURL);
    await expect(authLink).toBeHidden();
    assert.equal(attachmentRequests.length, proxyRequestsBeforePrivateImages);
    assert.deepEqual(signedImageRequests, [signedImageURL, expiredImageURL].map(url =>
      ({ url, type: 'image', referer: undefined, authorization: undefined })));
    await page.keyboard.press('Escape');
    assert.deepEqual(errors, [], 'no browser JavaScript errors');
    assert.deepEqual(writes, [], 'viewing assets must not submit or modify a task');
  });
}
