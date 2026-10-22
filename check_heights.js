const { chromium } = require('playwright');
(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage();
  await page.goto('file://' + process.cwd() + '/internal/web/static/index.html');
  await page.evaluate(() => {
    document.getElementById('custom-library-section').hidden = false;
  });
  const result = await page.evaluate(() => {
    const name = document.getElementById('custom-library-name');
    const repo = document.getElementById('custom-library-repo');
    return {
      name: getComputedStyle(name).height,
      repo: getComputedStyle(repo).height,
    };
  });
  console.log(JSON.stringify(result));
  await page.screenshot({ path: 'custom-library-after.png', clip: { x: 0, y: 0, width: 760, height: 700 } });
  await browser.close();
})();
