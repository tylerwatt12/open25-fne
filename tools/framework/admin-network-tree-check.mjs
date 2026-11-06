import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';

const [fixtureFile,outputArg]=process.argv.slice(2);
const fixture=JSON.parse(await fs.readFile(fixtureFile,'utf8'));
const output=path.resolve(process.env.OPEN25_ADMIN_UI_EVIDENCE||outputArg);
await fs.mkdir(output,{recursive:true,mode:0o700});
for(const role of fixture.roles)assert.ok(['127.0.0.1','localhost','[::1]'].includes(new URL(role.origin).hostname),'disposable loopback only');
const {chromium}=await import(process.env.TRUNKSYSTEM_PLAYWRIGHT || 'playwright');
const browser=await chromium.launch({headless:true,channel:'chrome'});
const report={scope:'Five disposable Go instances; production renderers, navigation, presentation, intercepted radio-configuration Save, real disposable SMTP saves and planned-alarm rejection. No production endpoint, audio, RF, USB or firmware action.',instances:[],failures:[]};
const retired=['/app.js','/desktop.js','/desktop.css','/radio.js','/radio.css','/relay_icons.js','/sw.js','/manifest.webmanifest'];
const geometry=[];
async function auditGeometry(page,label,width,appearance){
 const data=await page.evaluate(appearance=>{
  const root=document.querySelector('.ts-admin'),scheme=root.style.colorScheme,advanced=[...document.querySelectorAll('details.ts-advanced')].map(n=>[n,n.open]);
  root.style.colorScheme=appearance;for(const [node] of advanced)node.open=true;
  try {
   const visible=n=>n.getBoundingClientRect().height>0,rect=n=>{const r=n.getBoundingClientRect();return {x:r.x,y:r.y,width:r.width,height:r.height,right:r.right,bottom:r.bottom}};
   const grids=[...document.querySelectorAll('.ts-formgrid')].map(grid=>[...grid.children].filter(n=>n.matches('.ts-field')&&visible(n)).map(field=>{
    const control=field.querySelector('input:not([type=hidden]):not([type=checkbox]),select,textarea'),label=field.querySelector('.ts-fieldlabel');
    return {name:field.dataset.field,field:rect(field),label:label&&rect(label),control:control&&rect(control),pair:!!field.querySelector('.ts-numberpair')};
   }));
   const actions=[...document.querySelectorAll('.ts-actions,.ts-relation')].filter(visible).map(group=>[...group.children].filter(n=>n.matches('button,a.ts-button,input')&&visible(n)).map(n=>({label:n.textContent||n.name,box:rect(n)})));
   const arrows=[...document.querySelectorAll('.ts-instance-tree a .ts-external')].filter(visible).map(n=>({label:n.parentElement.textContent,box:rect(n)}));
   const profiles=[...document.querySelectorAll('.ts-gridrow')].filter(visible).map(row=>({columns:getComputedStyle(row).gridTemplateColumns.split(' ').length,inputs:[...row.querySelectorAll('input:not([type=checkbox])')].map(rect),button:rect(row.querySelector('button')),check:row.querySelector('input[type=checkbox]')&&rect(row.querySelector('input[type=checkbox]'))}));
   const pickers=[...document.querySelectorAll('.ts-relation')].filter(visible).map(row=>({columns:getComputedStyle(row).gridTemplateColumns.split(' ').length,button:rect(row.querySelector('button')),input:rect(row.querySelector('input:not([type=hidden])'))}));
   const orders=[...document.querySelectorAll('.ts-orderlist')].filter(visible).map(list=>[...list.querySelectorAll('.ts-orderitem .ts-actions')].map(rect));
   const disclosures=[...document.querySelectorAll('.ts-navbranch>summary,.ts-advanced>summary')].filter(visible).map(n=>({row:rect(n),arrow:n.querySelector('svg')&&rect(n.querySelector('svg'))}));
   const buttons=[...document.querySelectorAll('button:not(.ts-link),a.ts-button')].filter(visible).map(n=>({label:n.textContent||n.getAttribute('aria-label'),box:rect(n)}));
   return {grids,actions,arrows,profiles,pickers,orders,disclosures,buttons,scale:Number(getComputedStyle(document.documentElement).zoom)||1};
  } finally {root.style.colorScheme=scheme;for(const [node,open] of advanced)node.open=open;}
 },appearance);
 geometry.push({label,width,appearance,...data});
 const near=(a,b,detail)=>assert.ok(Math.abs(a-b)<=0.75*data.scale,JSON.stringify({label,width,appearance,detail,a,b}));
 for(const grid of data.grids)for(let i=0;i<grid.length;i++)for(const other of grid.slice(i+1)){
  const field=grid[i];if(Math.abs(field.field.y-other.field.y)<1&&field.control&&other.control)near(field.control.y,other.control.y,'input row '+field.name+'/'+other.name);
 }
 for(const arrow of data.arrows)near(arrow.box.right,data.arrows[0].box.right,'outside-link column');
 for(const row of data.profiles)if(row.columns>1){
  for(const input of row.inputs)near(input.y,row.inputs[0].y,'profile input row');
  near(row.button.y,row.inputs[0].y,'profile delete alignment');
  if(row.check)near(row.check.y+row.check.height/2,row.inputs[0].y+row.inputs[0].height/2,'profile checkbox alignment');
 }
 for(const row of data.pickers)if(row.columns>1){near(row.button.y,row.input.y,'picker alignment');near(row.button.height,row.input.height,'picker height');}
 for(const list of data.orders)for(const item of list)near(item.right,list[0].right,'profile reorder column');
 for(const row of data.disclosures){assert.ok(row.arrow,'shared disclosure arrow');near(row.arrow.y+row.arrow.height/2,row.row.y+row.row.height/2,'disclosure centre');}
 for(const button of data.buttons)near(button.box.height,40*data.scale,'button height '+button.label);
}
async function dimensions(page,label){
 const rows=[];
 for(const width of [1440,1024,736,320]){
  await page.setViewportSize({width,height:1000});
  for(const appearance of ['light','dark'])await auditGeometry(page,label,width,appearance);
  if(label==='profile-dynamic-rows'||label==='/c4fm-test')await page.screenshot({path:path.join(output,label.replaceAll('/','')+'-'+width+'.png'),fullPage:true});
  const size=await page.evaluate(()=>({scroll:document.documentElement.scrollWidth,client:document.documentElement.clientWidth}));
  assert.ok(size.scroll<=size.client+1,JSON.stringify({label,width,...size}));rows.push({width,...size});
 }
 await page.setViewportSize({width:1440,height:1000});
 await page.evaluate(()=>document.documentElement.style.zoom='2');
 try {
  for(const appearance of ['light','dark'])await auditGeometry(page,label+'-zoom-200',1440,appearance);
  if(label==='profile-dynamic-rows'||label==='/c4fm-test')await page.screenshot({path:path.join(output,label.replaceAll('/','')+'-zoom-200.png'),fullPage:true});
  const size=await page.evaluate(()=>({scroll:document.documentElement.scrollWidth,client:document.documentElement.clientWidth}));
  assert.ok(size.scroll<=size.client+1,'200% layout '+label);
 } finally {await page.evaluate(()=>document.documentElement.style.zoom='');}
 return rows;
}
async function fields(page){
 const rows=await page.locator('form[data-config-form] input[name]:not([type=hidden]),form[data-config-form] select[name],form[data-config-form] textarea[name]').evaluateAll(nodes=>nodes.map(n=>({name:n.name,type:n.type,disabled:n.disabled,readonly:n.readOnly||false,restrictions:['min','max','required','pattern','maxlength'].filter(a=>n.hasAttribute(a))})));
 const invalid=rows.filter(f=>f.disabled||f.readonly||(f.restrictions.length&&!(new URL(page.url()).pathname==='/radios/edit'&&f.name==='alias')));
 assert.deepEqual(invalid,[],'no configuration restrictions beyond the authorized OTA alias format');return rows;
}
async function discard(page){
 await page.locator('[data-admin-cancel]').click();
 await page.locator('[data-unsaved-action=stay]').waitFor();await page.locator('[data-unsaved-action=stay]').click();
 assert.equal(await page.locator('[data-unsaved-navigation]').count(),0);
 await page.locator('[data-admin-cancel]').click();await Promise.all([page.waitForURL(u=>!u.pathname.endsWith('/edit')),page.locator('[data-unsaved-action=discard]').click()]);
}
async function compareReference(page,browser){
 const preview=await browser.newPage({viewport:{width:1440,height:1100}});await preview.goto(process.env.OPEN25_REFERENCE_URL);
 await preview.locator('iframe').evaluate(el=>{el.style.setProperty('width','1440px','important');el.style.setProperty('max-width','none','important');});
 const frame=preview.frames().find(f=>f.parentFrame());await frame.waitForFunction(()=>window.__adminRedesign);
 const record=await page.locator('[name=record]').evaluate(el=>JSON.parse(el.value));
 await frame.evaluate(record=>{const v=window.__adminRedesign.views[0];v.dirty=false;v.store.system.talkgroups[0]=record;v.go('talkgroups','system',0);document.querySelector('.ts-admin').style.colorScheme='light';},record);
 if(await page.locator('body').getAttribute('data-theme')!=='light')await page.locator('[data-admin-theme]').click();
 const measure=()=>{
  const main=document.querySelector('.ts-main'),section=document.querySelector('.ts-formsection'),grid=section.querySelector('.ts-formgrid'),field=section.querySelector('[data-field=tgid]'),input=field.querySelector('input[type=number]');
  const rect=el=>{const b=el.getBoundingClientRect();return {width:b.width,height:b.height};};
  return {viewport:innerWidth,sidebar:rect(document.querySelector('.ts-sidebar')).width,editor:rect(document.querySelector('.ts-editor')).width,section:rect(section),sectionPadding:getComputedStyle(section).padding,gridGap:getComputedStyle(grid).gap,field:rect(field),input:rect(input),font:getComputedStyle(input).font,background:getComputedStyle(document.querySelector('.ts-admin')).backgroundColor,mainPadding:getComputedStyle(main).padding};
 };
 const actual=await page.evaluate(measure),reference=await frame.evaluate(measure);await preview.screenshot({path:path.join(output,'reference-talkgroup.png'),fullPage:false});
 await page.screenshot({path:path.join(output,'actual-talkgroup.png'),fullPage:false});
 await preview.close();assert.deepEqual(actual,reference,'same-fixture core editor geometry and tokens match the approved reference');
 return {actual,reference,scope:'Matched at 1440px using the same public talkgroup record. Geometry is relative to the editor; production authentication/theme controls replace preview-only chrome. This is not a claim of full-page pixel identity.'};
}
try{
 for(const role of fixture.roles){
  const item={role:role.role,instance:role.instance,pages:[],forms:[],layouts:[]};report.instances.push(item);
  const context=await browser.newContext({viewport:{width:1440,height:1000}}),page=await context.newPage(),requests=[],errors=[];page.setDefaultTimeout(10000);
  page.on('request',request=>requests.push({method:request.method(),url:request.url(),type:request.resourceType()}));page.on('pageerror',error=>errors.push(error.message));page.on('dialog',dialog=>dialog.accept());
  try{
   await page.goto(role.origin);await page.locator('[name=username]').fill(role.username);await page.locator('[name=password]').fill(role.password);
   await Promise.all([page.waitForURL(role.origin+'/'),page.locator('form:has([name=password]) button').click()]);
   assert.ok((await page.title()).startsWith('Open25 '));
   assert.equal(await page.locator('.ts-sidebar [aria-current=location]').count(),1);
   assert.ok((await page.locator('.ts-sidebar [aria-current=location]').innerText()).length>0);
   assert.equal(await page.locator('.ts-sidebar [aria-current=location] .ts-external').count(),0,'current portal has no outside marker');
   const foreign=page.locator('.ts-instance-tree a:not([aria-current])');
   assert.equal(await foreign.locator('.ts-external').count(),await foreign.count(),'every other portal has an outside-link icon');
   assert.equal(await page.locator('.ts-breadcrumb a:not(:has(.ts-external))').count(),0,'ancestor portal links are marked');
   assert.equal(await page.locator('.ts-network-diagram a:not(:has(.ts-external))').count(),0,'topology portal links are marked');
   assert.equal(await page.locator('.ts-sidebar > nav > a .ts-external').count(),0,'ordinary local navigation has no marker');

   assert.equal(await page.locator('.ts-network-diagram [aria-current=true]').count(),1);
   assert.equal(await page.locator('.ts-page').getAttribute('data-page-group'),'dashboard');
   const crumbs=await page.locator('.ts-breadcrumb').innerText();assert.ok(crumbs.includes('System:'));
   if(role.role==='site'||role.role==='zone')assert.ok(crumbs.includes('Zone:'));
   if(role.role==='site')assert.ok(crumbs.includes('Site:'));
   assert.equal(await page.locator('.ts-instance-tree > li > ul > li > ul > li').count(),role.role==='site'?1:2,'known Sites nested inside their Zone');
   for(const clutter of ['Local administration','Read only · Loaded','Stored observations','Explicit Save'])assert.ok(!(await page.locator('.ts-main,.ts-contextbar').allTextContents()).join(' ').includes(clutter));

   assert.deepEqual(await page.locator('.ts-page h2').allTextContents().then(t=>[t[0],t[1],t.at(-1)]),['Needs attention','Network location','Configuration shortcuts']);
   assert.equal(await page.locator('.ts-main form[method=post]').count(),0);
   item.layouts.push({page:'overview',sizes:await dimensions(page,'overview')});
   for(const appearance of ['light','dark']){
    if(await page.locator('body').getAttribute('data-theme')!==appearance)await page.locator('[data-admin-theme]').click();
    await page.screenshot({path:path.join(output,role.instance+'-overview-'+appearance+'.png'),fullPage:true});
   }
   await page.evaluate(()=>document.documentElement.style.zoom='2');
   const zoom=await page.evaluate(()=>({width:document.documentElement.clientWidth,scroll:document.documentElement.scrollWidth}));assert.ok(zoom.scroll<=zoom.width+1,'200% zoom fits');
   await page.evaluate(()=>document.documentElement.style.zoom='');
   const queue=await page.locator('.ts-sidebar a').evaluateAll(nodes=>nodes.map(n=>n.href)),seen=new Set();queue.push(role.origin+'/installation');
   while(queue.length){
    const address=queue.shift(),url=new URL(address);if(url.origin!==role.origin||seen.has(url.pathname+url.search)||['/dispatch-test','/ptt-test'].includes(url.pathname))continue;seen.add(url.pathname+url.search);
    const start=requests.length,response=await page.goto(address);assert.equal(response.status(),200,'GET '+address);
    assert.equal(await page.locator('.ts-topbar').count(),1,'single current shell');
    assert.equal(await page.locator('.admin-record-editor,.admin-rail,[data-admin-editor],dialog:not(.ts-chooser)').count(),0,'obsolete editor shell removed');
    assert.equal(await page.getByText('View records',{exact:true}).count(),0);
    const group=await page.locator('.ts-page').getAttribute('data-page-group');
    const dates=page.locator('time[datetime]');
    for(const date of await dates.all())assert.ok(await date.getAttribute('title'),'full observation date available on hover');
    if(url.pathname==='/command-results'&&role.role==='site'){
     assert.ok(await page.locator('.ts-status.ts-good').filter({hasText:'Acknowledged'}).count(),'observed acknowledgment stands out');
     assert.ok(await page.locator('.ts-status.ts-bad').filter({hasText:'Timed out'}).count(),'actual timeout stands out');
    }

    if(group==='configuration'||group==='notifications')assert.equal(await page.locator('meta[http-equiv=refresh]').count(),0);
    if(['dashboard','status','reference'].includes(group)){
     assert.equal(await page.locator('.ts-main form[method=post]').count(),0,'read-only page');
     assert.equal(await page.locator('.ts-main [name=config],.ts-main [name=material]').count(),0);
     const text=await page.locator('.ts-main').innerText();for(const secret of ['z'.repeat(64),'s'.repeat(64),'peer_secret','password_hash'])assert.ok(!text.includes(secret),'no credential in '+url.pathname);
    }
    const editable=await fields(page);if(editable.length)item.forms.push({path:url.pathname,fields:editable.map(f=>f.name)});
    assert.ok(requests.slice(start).every(r=>r.method==='GET'),'opening a page performs reads only');
    const links=await page.locator('.ts-records a[href*="/edit"],.ts-heading a[href*="/edit"]').evaluateAll(nodes=>nodes.map(n=>n.href));for(const link of links)if(!seen.has(new URL(link).pathname+new URL(link).search))queue.push(link);
    if(editable.length||(await page.locator('.ts-formgrid').count())||url.pathname==='/command-results'||url.pathname==='/modem-status')item.layouts.push({page:url.pathname+url.search,sizes:await dimensions(page,url.pathname)});
    item.pages.push({path:url.pathname,query:url.search,group});
   }
   const target={system:'/talkgroups/edit?row=0',zone:'/sites/edit?row=0',site:'/modems/edit?row=0',application:'/application-settings'}[role.role];
   await page.goto(role.origin+target);const form=page.locator('form[data-config-form]');assert.equal(await form.count(),1);
   const geometry=await form.evaluate(n=>{const b=n.getBoundingClientRect(),main=n.closest('.ts-main').getBoundingClientRect();return {width:b.width,left:b.left,mainLeft:main.left,font:parseFloat(getComputedStyle(n.querySelector('input:not([type=hidden])')).fontSize),height:n.querySelector('input:not([type=hidden])').getBoundingClientRect().height};});
   assert.ok(geometry.width>=650&&geometry.left-geometry.mainLeft<=40,'useful main-pane editor width');assert.equal(geometry.font,15);assert.ok(geometry.height>=40);item.editor=geometry;
   assert.equal(await form.locator('details.ts-help,details.help').count(),0,'information never uses a click toggle');
   assert.equal(await form.locator('[data-admin-cancel]').getAttribute('class'),'ts-button');
   const info=page.locator('.ts-help').first();
   if(await info.count()){
    for(const width of [1440,320]){
     await page.setViewportSize({width,height:1000});await info.scrollIntoViewIfNeeded();await info.hover();
     const popup=info.locator('.ts-helpbody');await popup.waitFor({state:'visible'});
     const bounds=await popup.boundingBox();assert.ok(bounds.x>=0&&bounds.x+bounds.width<=width+1,'help stays inside viewport');
     await page.screenshot({path:path.join(output,role.instance+'-help-'+width+'.png'),fullPage:false});
     await info.click();await page.mouse.move(1,1);await popup.waitFor({state:'hidden'});
     await info.focus();await page.keyboard.press('Shift+Tab');await page.keyboard.press('Tab');await popup.waitFor({state:'visible'});
     await page.keyboard.press('Tab');await popup.waitFor({state:'hidden'});
    }
    await page.setViewportSize({width:1440,height:1000});
   }
   const advanced=page.locator('details.ts-advanced');
   if(await advanced.count()){
    assert.equal(await advanced.getAttribute('open'),null,'specialist settings start collapsed');
    const before=await form.evaluate(f=>[...new FormData(f).entries()]);
    await advanced.locator('summary').click();assert.deepEqual(await form.evaluate(f=>[...new FormData(f).entries()]),before,'collapsed fields remain in ordinary submission');
    await advanced.locator('summary').click();
   }
   if(role.role==='site'){
    const saved=await page.locator('[name=device]').inputValue();
    await page.locator('[name=device]').fill('/dev/serial/by-id/manual-absent-device');
    const data=page.waitForResponse(r=>new URL(r.url()).pathname==='/chooser');await page.locator('[data-chooser-kind=usb]').click();assert.equal((await data).status(),200);
    await page.locator('.ts-chooser [data-chooser-status]').filter({hasText:''}).waitFor();
    await page.locator('.ts-chooser [data-chooser-close]').last().click();
    assert.equal(await page.locator('[name=device]').inputValue(),'/dev/serial/by-id/manual-absent-device','Cancel preserves manual absent selection');
    await page.locator('[name=device]').fill(saved);
   }
if(role.role==='system'&&process.env.OPEN25_REFERENCE_URL)item.reference=await compareReference(page,browser);await page.screenshot({path:path.join(output,role.instance+'-editor.png'),fullPage:true});
   const wrapped=await page.locator('.ts-fieldlabel label').first().textContent();
   await page.locator('.ts-fieldlabel label').first().evaluate(n=>n.textContent='A deliberately long field label that wraps across multiple lines at intermediate widths');
   await dimensions(page,'wrapped-field-label');
   await page.locator('.ts-fieldlabel label').first().evaluate((n,text)=>n.textContent=text,wrapped);
   const change=form.locator('[name=alias],[name=name]').first();await change.fill('UI check < & >');await page.locator('.ts-heading h1').click();assert.equal(await form.locator('[data-admin-unsaved-label]').innerText(),'Unsaved changes');
   const snapshot=await form.locator('input,select,textarea').evaluateAll(nodes=>nodes.map(n=>[n.name,n.value,n.checked]));
   for(const link of await form.locator('.ts-editor-nav a').all())await link.click();
   assert.deepEqual(await form.locator('input,select,textarea').evaluateAll(nodes=>nodes.map(n=>[n.name,n.value,n.checked])),snapshot,'section navigation never changes data');
   await discard(page);assert.equal(await page.locator('[name=alias]').count(),0);
   await page.goto(role.origin+target);
   const pair=page.locator('.ts-numberpair').first();
   if(await pair.count()){
    await pair.locator('[data-number-hex]').fill('7FFFFFFFFFFFFFFF');assert.equal(await pair.locator('[data-number-decimal]').inputValue(),'9223372036854775807');
    await pair.locator('[data-number-decimal]').fill('16777217');assert.equal(await pair.locator('[data-number-hex]').inputValue(),'1000001');
   }
   await form.locator('[name=alias],[name=name]').first().fill('Save exercise');
   let submitted;
   await page.route('**/*',async route=>{const request=route.request();if(request.method()==='POST'&&new URL(request.url()).pathname===new URL(page.url()).pathname){submitted=new URLSearchParams(request.postData());await route.fulfill({status:303,headers:{location:'/'+new URL(request.url()).pathname.split('/')[1]+'?saved=1'},body:''});}else await route.continue();});
   await Promise.all([page.waitForURL(/saved=1/),form.locator('.ts-primary').click()]);assert.ok(submitted?.get('csrf'),'ordinary form sends CSRF');if(role.role!=='application')assert.ok(submitted.has('config')||submitted.has('record'),'explicit original snapshot retained');await page.unroute('**/*');
   if(role.role==='system'){
    await page.goto(role.origin+'/profiles/edit?row=0');
    assert.equal(await page.locator('[name=receive_0_volume]').inputValue(),'75.3');
    await page.locator('[name=zones_0_talkgroups]').fill('1001, 1002, 999999');
    await page.locator('[data-zone-action=down]').first().click();assert.equal(await page.locator('[name=zones_0_talkgroups]').inputValue(),'1002, 1001, 999999');
    await page.locator('[data-profile-grid=zones] [data-profile-add]').click();assert.equal(await page.locator('[data-profile-grid=zones] [data-profile-row]').count(),2);
    await page.locator('[name=zones_1_name]').fill('Extra zone');await dimensions(page,'profile-dynamic-rows');await page.screenshot({path:path.join(output,role.instance+'-profile.png'),fullPage:true});
    await page.locator('[data-profile-grid=zones] [data-profile-remove]').first().click();
    await page.locator('[data-profile-grid=zones] [data-profile-add]').click();assert.equal(await page.locator('[name=zones_2_name]').count(),1,'row identities stay unique after removal');
    await page.goto(role.origin+'/talkgroups/edit?row=0');
    const reads=requests.length;
    await page.locator('[data-chooser-target=affiliation_radio_ids]').click();
    const option=page.locator('.ts-chooser tbody input').first();await option.waitFor();const chosen=await option.inputValue();await option.check();
    await page.locator('[data-chooser-apply]').click();assert.ok((await page.locator('[name=affiliation_radio_ids]').inputValue()).includes(chosen));
    await page.locator('[name=admission_mode]').selectOption('clear');await page.locator('[name=transmit_mode]').selectOption('secure');assert.equal(await page.locator('[name=admission_mode]').inputValue(),'clear','UI never repairs conflicting settings');
    assert.ok(requests.slice(reads).every(r=>r.method==='GET'&&new URL(r.url).pathname==='/chooser'),'only explicit bounded chooser reads; no preflight');
   }
   if(role.role==='site'){
    await page.goto(role.origin+'/channels/edit?row=0');
    assert.equal(await page.locator('input[name=tx_frequency_hz],input[name=rx_frequency_hz]').count(),0,'frequencies are calculated outputs');
    await page.locator('[name=channel_no]').fill('81');await page.locator('[name=channel_no]').blur();
    assert.match(await page.locator('[data-band-frequency=tx_frequency_hz]').innerText(),/^444\.45/);
    assert.match(await page.locator('[data-band-frequency=rx_frequency_hz]').innerText(),/^449\.45/);
    const calls=requests.length;
    await page.locator('[name=identifier]').fill('99');await page.locator('[name=identifier]').blur();
    await page.locator('[data-band-result]').filter({hasText:'No matching Band Plan'}).waitFor();
    assert.equal(await page.locator('[name=identifier]').inputValue(),'99','reference lookup never repairs input');
    assert.equal(await page.locator('[data-band-frequency=tx_frequency_hz]').innerText(),'Unavailable');
    await page.locator('[data-chooser-kind=bands]').click();await page.locator('.ts-chooser tbody input[value="2"]').waitFor();await page.locator('.ts-chooser tbody input[value="2"]').check();await page.locator('[data-chooser-apply]').click();
    await page.waitForFunction(()=>document.querySelector('[data-band-frequency=tx_frequency_hz]').textContent.startsWith('444.45'));
    assert.ok(requests.slice(calls).every(r=>r.method==='GET'&&new URL(r.url).pathname==='/chooser'),'frequency preview uses only explicit owned reference reads');
    await dimensions(page,'calculated-rf-fields');await page.screenshot({path:path.join(output,role.instance+'-calculated-rf.png'),fullPage:true});
   }
   // SMTP saves are real, scoped to these disposable stores, and never send mail.
   await page.goto(role.origin+'/smtp-settings');assert.equal(await page.locator('[name=smtp_password]').inputValue(),'');
   assert.equal(await page.locator('[name=inherit_smtp]').count(),role.role==='system'?0:1);
   if(role.role!=='system')assert.equal(await page.locator('[name=inherit_smtp]').isChecked(),true);
   const smtpSecret='fixture-only-smtp-'+role.instance;
   await page.locator('[name=enabled]').uncheck();
   await page.locator('[name=host]').fill('mail.example.invalid');await page.locator('[name=port]').fill('2525');
   await page.locator('[name=authentication]').selectOption('password');await page.locator('[name=username]').fill('fixture-only-user');
   await page.locator('[name=sender_address]').fill('fixture@example.invalid');await page.locator('[name=recipients]').fill('recipient@example.invalid');
   await page.locator('[name=smtp_password]').fill(smtpSecret);
   async function saveSMTP(){
    const response=page.waitForResponse(r=>r.request().method()==='POST'&&r.url().endsWith('/smtp-settings'));
    await page.locator('form[data-config-form] .ts-primary').click();assert.equal((await response).status(),200);
    await page.locator('[role=status]').filter({hasText:'SMTP settings saved. No email was sent.'}).waitFor();
    assert.equal(await page.locator('[name=smtp_password]').inputValue(),'');assert.ok(!(await page.content()).includes(smtpSecret));
   }
   await saveSMTP();await page.reload();
   assert.equal(await page.locator('[name=host]').inputValue(),'mail.example.invalid');assert.equal(await page.locator('[name=port]').inputValue(),'2525');
   assert.equal(await page.locator('[name=username]').inputValue(),'fixture-only-user');assert.equal(await page.locator('[name=smtp_password]').inputValue(),'');
   await page.locator('[name=sender_name]').fill('SMTP browser fixture');await saveSMTP();await page.reload();
   assert.equal(await page.locator('[name=sender_name]').inputValue(),'SMTP browser fixture');assert.equal(await page.locator('[name=smtp_password]').inputValue(),'');
   if(role.role!=='system')assert.equal(await page.locator('[name=inherit_smtp]').isChecked(),true);
   item.smtp={saved:true,reloaded:true,blankReplacementSubmitted:true,passwordNotRendered:true,localProfileDisabled:true};
   // Infrastructure alarm acknowledgment is still an explicitly planned operation.
   await page.goto(role.origin+'/alarm-actions');await page.locator('[name=alarm_id]').fill('browser-fixture-alarm');
   const plannedResponse=page.waitForResponse(r=>r.request().method()==='POST'&&r.url().endsWith('/alarm-actions'));
   await page.locator('form[data-config-form] .ts-primary').click();assert.equal((await plannedResponse).status(),501);
   await page.locator('[role=alert]').filter({hasText:'Nothing was saved or sent.'}).waitFor();
   item.plannedAlarmAcknowledgmentStatus=501;
   await page.goto(role.origin+'/find-setting?q=frequency');assert.ok((await page.locator('.ts-main').innerText()).includes('frequency'));
   await page.goto(role.origin+target);const idle=requests.length;await page.waitForTimeout(6200);assert.deepEqual(requests.slice(idle),[],'idle configuration never polls');
   assert.ok(requests.every(r=>new URL(r.url).origin===role.origin),'browser requests only serving service');
   for(const asset of retired)assert.equal((await context.request.get(role.origin+asset)).status(),404,'retired asset '+asset);
   for(const plain of ['/dispatch-test','/ptt-test'])if(role.role==='application'){const response=await context.request.get(role.origin+plain),html=await response.text();assert.equal(response.status(),200);assert.ok(!html.includes('/admin.css')&&!html.includes('class=ts-help'),'plain tests preserved');assert.ok(html.includes(plain==='/dispatch-test'?'Open25 Dispatch':'Open25 PTT'));}
   assert.deepEqual(errors,[],'no browser errors');
   item.request_count=requests.length;
  }catch(error){report.failures.push({instance:role.instance,error:error.stack});await page.screenshot({path:path.join(output,role.instance+'-failure.png'),fullPage:true}).catch(()=>{});}
  await context.close();
 }
}finally{await browser.close();await fs.writeFile(path.join(output,'network-tree-ui-check.json'),JSON.stringify(report,null,2)+'\n');if(process.env.OPEN25_ADMIN_GEOMETRY_AUDIT)await fs.writeFile(path.join(output,'geometry.json'),JSON.stringify(geometry,null,2)+'\n');}
console.log(JSON.stringify({instances:report.instances.map(i=>({id:i.instance,pages:i.pages.length,forms:i.forms.length})),failures:report.failures}));
assert.deepEqual(report.failures,[]);
