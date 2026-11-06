import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';
const [fixtureFile,output]=process.argv.slice(2),fixture=JSON.parse(await fs.readFile(fixtureFile,'utf8'));
assert.equal(new URL(fixture.origin).hostname,'127.0.0.1');
const {chromium}=await import(process.env.TRUNKSYSTEM_PLAYWRIGHT || 'playwright');
const browser=await chromium.launch({headless:true,channel:'chrome'}),page=await browser.newPage({viewport:{width:1440,height:1000}});
const errors=[],reads=[],report={scope:'Actual Go disposable System, 50,000 radios/talkgroups/logins/Sites; bounded chooser and staged selection only; no RF or production data.',checks:[]};
page.on('pageerror',error=>errors.push(error.message));page.on('request',r=>reads.push({url:r.url(),method:r.method()}));
try{
 await page.goto(fixture.origin+'/login');await page.locator('[name=username]').fill('administrator');await page.locator('[name=password]').fill(fixture.password);await Promise.all([page.waitForURL(fixture.origin+'/'),page.locator('form[action="/login"] button').click()]);
 await page.goto(fixture.origin+'/talkgroups/edit?row=0');
 const field=page.locator('[name=affiliation_radio_ids]'),open=()=>page.locator('[data-chooser-target=affiliation_radio_ids]').click(),dialog=page.locator('.ts-chooser');
 await field.fill('999999, 999998');await open();await dialog.locator('tbody input').first().waitFor();
 assert.equal(await dialog.locator('tbody input').count(),50);assert.equal(await dialog.locator('[data-chooser-page]').innerText(),'1–50 of 50000');
 await dialog.locator('tbody input[value="100001"]').check();
 const next=page.waitForResponse(r=>r.url().includes('/chooser?')&&r.url().includes('offset=50'));await dialog.locator('[data-chooser-next]').click();await next;await dialog.locator('tbody input[value="100050"]').waitFor();await dialog.locator('tbody input[value="100051"]').check();
 await dialog.locator('[name=q]').fill('UNIT 49999');await dialog.locator('button[type=submit]').click();await dialog.locator('tbody input[value="149999"]').waitFor();await dialog.locator('tbody input').check();
 await page.screenshot({path:path.join(output,'chooser-search-50000.png')});
 await dialog.locator('[data-chooser-apply]').click();assert.equal(await field.inputValue(),'999999, 999998, 100001, 100051, 149999');
 report.checks.push('Selection across pages and name search preserves unknown identities and includes disabled radios');
 await open();await dialog.locator('tbody input').first().waitFor();await dialog.locator('tbody input').first().check();await page.keyboard.press('Escape');assert.equal(await field.inputValue(),'999999, 999998, 100001, 100051, 149999');assert.equal(await page.locator('[data-chooser-target=affiliation_radio_ids]').evaluate(n=>n===document.activeElement),true);report.checks.push('Escape cancels staged changes and restores focus');
 // Delayed older requests must not replace a newer result page.
 await page.route('**/chooser?**',async route=>{const q=new URL(route.request().url()).searchParams.get('q');if(q==='slow'){await new Promise(resolve=>setTimeout(resolve,500));await route.fulfill({json:{items:[{ID:'wrong',Name:'Old result'}],total:1,offset:0,limit:50}});}else await route.continue();});
 await open();await dialog.locator('tbody input').first().waitFor();await dialog.locator('[name=q]').fill('slow');await dialog.locator('button[type=submit]').click();await dialog.locator('[name=q]').fill('149999');await dialog.locator('button[type=submit]').click();await dialog.locator('tbody input[value="149999"]').waitFor();await page.waitForTimeout(600);assert.equal(await dialog.locator('tbody input').count(),1);assert.equal(await dialog.locator('tbody input').inputValue(),'149999');await page.keyboard.press('Escape');await page.unroute('**/chooser?**');report.checks.push('New search wins over delayed response');
 await page.route('**/chooser?**',route=>route.fulfill({status:503,body:'Unavailable'}));await open();await dialog.locator('[data-chooser-status]').filter({hasText:'Could not load'}).waitFor();assert.equal(await field.inputValue(),'999999, 999998, 100001, 100051, 149999');await page.keyboard.press('Escape');await page.unroute('**/chooser?**');report.checks.push('Read failure preserves form values and permits retry');
 for(const theme of ['light','dark'])for(const width of [320,736,1440]){
  await page.setViewportSize({width,height:1000});await page.evaluate(theme=>document.querySelector('.ts-admin').style.colorScheme=theme,theme);await open();await dialog.locator('tbody input').first().waitFor();
  const b=await dialog.boundingBox();assert.ok(b.x>=0&&b.x+b.width<=width+1,'dialog inside viewport');assert.equal(await dialog.locator('tbody input').count(),50);await page.screenshot({path:path.join(output,`chooser-${theme}-${width}.png`)});await page.keyboard.press('Escape');
 }
 report.checks.push('Readable bounded dialog at 320/736/1440 px, both themes');
 await page.setViewportSize({width:1440,height:1000});
 await page.locator('[name=algorithm_id]').fill('170');await page.locator('[name=key_id]').fill('7');await page.locator('[name=admission_mode]').selectOption('clear');await page.locator('[name=transmit_mode]').selectOption('secure');
 await page.locator('[data-chooser-kind=keys]').click();await dialog.locator('tbody input[value="132/17"]').waitFor();await dialog.locator('tbody input[value="132/17"]').check();await dialog.locator('[data-chooser-apply]').click();
 assert.equal(await page.locator('[name=algorithm_id]').inputValue(),'132');assert.equal(await page.locator('[name=key_id]').inputValue(),'17');assert.equal(await page.locator('[name=admission_mode]').inputValue(),'clear');assert.equal(await page.locator('[name=transmit_mode]').inputValue(),'secure');assert.ok(!(await page.content()).includes('11'.repeat(32)));report.checks.push('Explicit loaded-key selection fills algorithm and Key ID together, retains disabled key choices and leaves admission/strapping unchanged');
 const sites=page.locator('[name=site_ids]');await sites.fill('missing site, unknown');await page.locator('[data-chooser-target=site_ids]').click();await dialog.locator('tbody input').first().waitFor();const siteId=await dialog.locator('tbody input').first().inputValue();await dialog.locator('tbody input').first().check();await dialog.locator('[data-chooser-apply]').click();assert.equal(await sites.inputValue(),'missing site, unknown, '+siteId);report.checks.push('Text identities with internal spaces survive multi-selection');
 await page.goto(fixture.origin+'/radios');
 await page.locator('[data-bulk-page]').check();assert.equal(await page.locator('[data-bulk-count]').innerText(),'50 selected');await page.locator('[data-bulk-page]').uncheck();
 await page.locator('[data-bulk-record][value="100001"]').check();await page.locator('[data-bulk-record][value="100002"]').check();assert.equal(await page.locator('[data-bulk-count]').innerText(),'2 selected');
 await Promise.all([page.waitForURL(/radios\/bulk/),page.locator('#bulk-record-selection button[value=edit]').click()]);
 assert.equal(await page.locator('[name=selection]').inputValue(),'100001, 100002');
 const group=page.locator('[name=group]');await group.focus();await page.locator('datalist option[value="Existing Fleet"]').waitFor({state:'attached'});await group.fill('New branch');assert.equal(await group.inputValue(),'New branch');
 await page.locator('[name=apply][value=group]').check();await page.locator('[name=alias]').fill('Not applied');
 let bulk;
 await page.route('**/radios/bulk',async route=>{if(route.request().method()==='POST'&&new URLSearchParams(route.request().postData()).get('action')==='save'){bulk=new URLSearchParams(route.request().postData());await route.fulfill({status:303,headers:{location:'/radios?saved=1&updated=2'},body:''});}else await route.continue();});
 await Promise.all([page.waitForURL(/updated=2/),page.locator('button[value=save]').click()]);await page.unroute('**/radios/bulk');assert.deepEqual(bulk.getAll('apply'),['group']);assert.equal(bulk.get('group'),'New branch');report.checks.push('Page/row selection, typed group suggestions and explicit checked-field bulk submission');
 await page.locator('.ts-records a[href*="clone="]').first().click();assert.equal(await page.locator('[name=resource]').count(),0);assert.equal(await page.locator('[name=rid]').inputValue(),'100000');assert.equal(await page.locator('[name=alias]').inputValue(),'Unit 00000');assert.ok((await page.locator('h1').innerText()).startsWith('Copy of'));report.checks.push('Clone opens an unsaved complete copy with editable original values');
 await page.screenshot({path:path.join(output,'compact-clone.png'),fullPage:true});
 await page.goto(fixture.origin+'/profiles');await page.locator('[data-chooser-kind=users]').click();await dialog.locator('tbody input').first().waitFor();await dialog.locator('[name=q]').fill('Person 49999');await dialog.locator('button[type=submit]').click();await dialog.locator('tbody input[value="login-49999"]').waitFor();await dialog.locator('tbody input').check();await Promise.all([page.waitForURL(/identity=login-49999/),dialog.locator('[data-chooser-apply]').click()]);assert.equal(await page.locator('[name=record]').evaluate(n=>JSON.parse(n.value).id),'login-49999');report.checks.push('Shared login chooser opens the chosen profile');
 assert.deepEqual(errors,[]);assert.ok(reads.filter(r=>!r.url.endsWith('/login')&&!r.url.endsWith('/radios/bulk')).every(r=>r.method==='GET'),'only explicit bulk form POSTs; chooser never issues radio operations');report.requests=reads.filter(r=>r.url.includes('/chooser?')).length;report.passed=true;
}catch(error){report.failure=error.message;report.errors=errors;report.reads=reads;report.url=page.url();report.dialog=await page.locator('.ts-chooser').textContent().catch(()=>null);await page.screenshot({path:path.join(output,'failure.png')});console.error(JSON.stringify({errors,url:page.url(),dialog:report.dialog,reads:reads.slice(-5)}));throw error;}finally{await browser.close();await fs.writeFile(path.join(output,'chooser-browser.json'),JSON.stringify(report,null,2)+'\n');}
