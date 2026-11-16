import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';

const [fixturePath, output] = process.argv.slice(2);
const fixture = JSON.parse(await fs.readFile(fixturePath, 'utf8'));
const {chromium} = await import(process.env.TRUNKSYSTEM_PLAYWRIGHT || 'playwright');
const browser = await chromium.launch({headless: true, channel: 'chrome'});
const results = [];
try {
  for (const site of fixture.roles) {
    assert.equal(new URL(site.origin).hostname, '127.0.0.1', 'disposable local Site only');
    const context = await browser.newContext();
    const page = await context.newPage();
    const requests = [];
    page.on('request', request => {
      if (request.method() !== 'GET') requests.push({method: request.method(), path: new URL(request.url()).pathname});
    });
    await page.goto(site.origin + '/login');
    await page.locator('[name=username]').fill(site.username);
    await page.locator('[name=password]').fill(site.password);
    await Promise.all([page.waitForURL(site.origin + '/'), page.locator('button[type=submit]').click()]);
    await page.goto(site.origin + '/control-policy');
    assert.equal(await page.locator('.ts-topbar').count(), 1, 'one administration shell');
    const checkbox = page.locator('input[type=checkbox][name=start_on_service_start]');
    assert.equal(await checkbox.count(), 1);
    assert.ok(await checkbox.isEnabled());
    assert.ok(!(await checkbox.isChecked()));
    assert.ok((await page.locator('.ts-main').innerText()).includes('Start radios when Site Controller starts'));
    for (const width of [1440, 320]) {
      await page.setViewportSize({width, height: 1000});
      for (const theme of ['light', 'dark']) {
        if (await page.locator('body').getAttribute('data-theme') !== theme) await page.locator('[data-admin-theme]').click();
        await page.evaluate(async () => {
          await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
          await Promise.all(document.getAnimations().filter(animation => animation.effect.getComputedTiming().iterations !== Infinity).map(animation => animation.finished.catch(() => {})));
        });
        const size = await page.evaluate(() => ({scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth}));
        assert.ok(size.scroll <= size.client + 1, JSON.stringify({width, theme, ...size}));
        await page.screenshot({path: path.join(output, `${site.instance}-startup-${width}-${theme}.png`), fullPage: true});
      }
    }
    await page.setViewportSize({width: 1440, height: 1000});
    await page.evaluate(() => document.documentElement.style.zoom = '2');
    assert.ok(await checkbox.isVisible(), 'startup checkbox remains visible at 200%');
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth + 1));
    await page.evaluate(() => document.documentElement.style.zoom = '');
    const help = page.locator('[data-field=start_on_service_start] .ts-help');
    await help.focus();
    assert.ok((await help.locator('.ts-helpbody').innerText()).includes('next service start'));
    await page.keyboard.press('Tab');
    await checkbox.focus();
    await page.keyboard.press('Space');
    assert.ok(await checkbox.isChecked(), 'keyboard toggles startup preference');
    await page.getByRole('button', {name: 'Save', exact: true}).click();
    try {
      await page.waitForURL('**/control-policy?saved=1', {timeout: 5000});
    } catch (error) {
      await page.screenshot({path: path.join(output, site.instance+'-save-failure.png'), fullPage: true});
      console.error(JSON.stringify({url: page.url(), requests, invalid: await page.locator('form[data-config-form] :invalid').evaluateAll(nodes => nodes.map(n => ({name:n.name, value:n.value, message:n.validationMessage}))), errors: await page.locator('.ts-error,[role=alert]').allTextContents()}));
      throw error;
    }
    assert.ok(await checkbox.isChecked());
    await page.reload();
    assert.ok(await checkbox.isChecked(), 'saved flag survives reload');
    await checkbox.uncheck();
    await Promise.all([page.waitForNavigation(), page.getByRole('button', {name: 'Save', exact: true}).click()]);
    assert.ok(!(await checkbox.isChecked()));
    assert.deepEqual(requests, [{method: 'POST', path: '/login'}, {method: 'POST', path: '/control-policy'}, {method: 'POST', path: '/control-policy'}], 'only explicit owned-form submissions');
    results.push({site: site.instance, saveAndReload: true, widths: [1440, 320], themes: ['light', 'dark'], zoom: '200%', keyboard: true, automaticRadioActions: 0});
    await context.close();
  }
} finally {
  await browser.close();
}
await fs.writeFile(path.join(output, 'results.json'), JSON.stringify(results, null, 2) + '\n');
console.log(JSON.stringify(results));
