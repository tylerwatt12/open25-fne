import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';

const [fixturePath, output] = process.argv.slice(2);
const portals = JSON.parse(await fs.readFile(fixturePath, 'utf8'));
for (const portal of portals) {
  assert.equal(new URL(portal.origin).hostname, '127.0.0.1', 'disposable loopback fixture only');
}
const {chromium} = await import(process.env.TRUNKSYSTEM_PLAYWRIGHT || 'playwright');
const browser = await chromium.launch({headless: true, channel: 'chrome'});
const results = [];
try {
  for (const [index, portal] of portals.entries()) {
    const context = await browser.newContext();
    const page = await context.newPage();
    const writes = [];
    const errors = [];
    page.on('pageerror', error => errors.push(String(error)));
    page.on('request', request => {
      if (request.method() !== 'GET') writes.push(new URL(request.url()).pathname);
    });
    await page.goto(portal.origin + '/login');
    await page.locator('[name=username]').fill('administrator');
    await page.locator('[name=password]').fill(portal.password);
    await Promise.all([page.waitForURL(portal.origin + '/'), page.locator('button[type=submit]').click()]);
    await page.goto(portal.origin + '/neighbor-announcements');
    assert.equal(await page.locator('.ts-topbar').count(), 1);
    const mode = page.locator('[name=neighbor_mode]');
    const neighbors = page.locator('[name=neighbor_site_ids]');
    assert.equal(await mode.inputValue(), 'all');
    await mode.selectOption('selected');
    await page.locator('[data-chooser-target=neighbor_site_ids]').click();
    await page.locator(`.ts-chooser tbody input[value="${portal.neighborId}"]`).check();
    await page.locator('[data-chooser-apply]').click();
    await Promise.all([page.waitForNavigation(), page.getByRole('button', {name: 'Save', exact: true}).click()]);
    await page.reload();
    assert.equal(await mode.inputValue(), 'selected');
    assert.equal(await neighbors.inputValue(), portal.neighborId);
    for (const width of [1440, 320]) {
      await page.setViewportSize({width, height: 1000});
      for (const theme of ['light', 'dark']) {
        if (await page.locator('body').getAttribute('data-theme') !== theme) await page.locator('[data-admin-theme]').click();
        const fits = await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth + 1);
        assert.ok(fits, `${width}/${theme} horizontal overflow`);
        await page.screenshot({path: path.join(output, `site-${index}-neighbors-${width}-${theme}.png`), fullPage: true});
      }
    }
    assert.deepEqual(writes, ['/login', '/neighbor-announcements'], 'only explicit owned-form submissions');
    assert.deepEqual(errors, []);
    results.push({site: index, savedSelection: true, widths: [1440, 320], themes: ['light', 'dark'], automaticRadioActions: 0});
    await context.close();
  }
} finally {
  await browser.close();
}
await fs.writeFile(path.join(output, 'results.json'), JSON.stringify(results, null, 2) + '\n');
console.log(JSON.stringify(results));
