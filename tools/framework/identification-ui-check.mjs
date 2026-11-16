import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';
const [fixturePath,output]=process.argv.slice(2);
const fixture=JSON.parse(await fs.readFile(fixturePath,'utf8'));
await fs.mkdir(output,{recursive:true});
const {chromium}=await import(process.env.TRUNKSYSTEM_PLAYWRIGHT || 'playwright');
const browser=await chromium.launch({headless:true,channel:'chrome'});
try {
 for(const [index,role] of fixture.roles.entries()){
  const callsign=index===0?'W1TEST':'W2TEST';
  assert.equal(new URL(role.origin).hostname,'127.0.0.1');
  const context=await browser.newContext();const page=await context.newPage();const errors=[];
  page.on('pageerror',e=>errors.push(e.message));
  page.on('dialog',d=>d.accept());
  await page.goto(role.origin+'/login');
  await page.locator('[name=username]').fill(role.username);await page.locator('[name=password]').fill(role.password);
  await Promise.all([page.waitForURL(role.origin+'/'),page.locator('form[action="/login"] button').click()]);
  await page.goto(role.origin+'/identification');
  const form=page.locator('form[data-admin-page="identification"]');
  const values=()=>form.evaluate(f=>Object.fromEntries(new FormData(f)));
  const before=await values();
  for(const name of ['mode','wpm','quiet_seconds'])assert.equal(await form.locator(`[name="${name}"]`).count(),0,'retired CW fields must be absent');
  assert.equal(/CWID|Morse|Ham mode/i.test(await form.innerText()),false);
  const bsi=form.locator('select[name=bsi_profile]');
  assert.equal(await bsi.isEnabled(),true);
  assert.deepEqual(await bsi.locator('option').evaluateAll(options=>options.map(o=>o.value).filter(Boolean)),['motorola']);
  await bsi.selectOption('motorola');
  await form.locator('[name=callsign]').fill(callsign);
  await form.locator('[name=enabled]').uncheck();
  for(const width of [1440,736,320]){
   await page.setViewportSize({width,height:1000});
   assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=document.documentElement.clientWidth+1));
  }
  await page.setViewportSize({width:1440,height:1000});
  for(const appearance of ['light','dark']){
   if(await page.locator('body').getAttribute('data-theme')!==appearance)await page.locator('[data-admin-theme]').click();
   await page.screenshot({path:path.join(output,role.instance+'-identification-'+appearance+'.png'),fullPage:true});
  }
  await page.goto(role.origin+'/identification');
  assert.deepEqual(await values(),before,'unsaved form edits must not change programming');
  assert.deepEqual(await bsi.locator('option').evaluateAll(options=>options.map(o=>o.value).filter(Boolean)),['motorola']);
  await bsi.selectOption('motorola');await form.locator('[name=callsign]').fill(callsign);await form.locator('[name=enabled]').uncheck();
  await Promise.all([page.waitForResponse(r=>r.request().method()==='POST'),form.getByRole('button',{name:'Save',exact:true}).click()]);
  await page.waitForLoadState();
  assert.ok(page.url().includes('saved=1'),await page.locator('.ts-main').innerText());
  assert.equal((await values()).callsign,callsign);assert.equal((await values()).bsi_profile,'motorola');
  assert.equal(await form.locator('[name=enabled]').isChecked(),false);
  for(const name of ['mode','wpm','quiet_seconds'])assert.equal(await form.locator(`[name="${name}"]`).count(),0);
  assert.deepEqual(errors,[]);await context.close();
 }
} finally {await browser.close();}
console.log('Both Sites: digital-only fields, exact Save/reload, unsaved edits, narrow widths and light/dark passed.');
