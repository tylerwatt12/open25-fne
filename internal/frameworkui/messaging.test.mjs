import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';

const source = await fs.readFile(new URL('./messaging.js', import.meta.url), 'utf8');
const testing = await fs.readFile(new URL('./testing.js', import.meta.url), 'utf8');

// Only local browser/HTTP fixtures are contacted; no radio, microphone or server
// installation participates in these messaging checks.
test('PTT and Dispatch share personal conversations while audio is unavailable without RF sends', {timeout: 60000}, async t => {
  const {chromium} = await import(process.env.TRUNKSYSTEM_PLAYWRIGHT || 'playwright');
  const people = [{id:'alice',username:'Alice <img>'},{id:'bob',username:'Bob'},{id:'charlie',username:'Charlie'}];
  const threads = [{id:'direct-1',kind:'direct',name:'',members:['alice','bob'],created_by:'alice',created_at:'2026-11-04T12:00:00Z'}];
  const messages = Array.from({length:55}, (_, index) => ({id:index+1,conversation_id:'direct-1',client_id:'seed-'+index,sender_id:'bob',sender_name:'Bob',body:index===54?'<img src=x onerror="window.injected=true">':'Earlier message '+(index+1),created_at:'2026-11-04T12:00:00Z'}));
  const reads = new Map(), joined = new Map(), requests = [], streams = new Set();
  let failAfterStore = false, failRead = false, failThreads = false, failMembers = false, failThread = false, failThreadTemporary = false, notify = true;
  const events = users => { if (notify) for (const stream of streams) if (users.includes(stream.user)) stream.response.write('event: inbox\ndata: {}\n\n'); };
  const threadValue = (thread, user) => {
    const after=joined.get(user+':'+thread.id)||0,items = messages.filter(message=>message.conversation_id===thread.id&&message.id>after), through = reads.get(user+':'+thread.id)||0;
    return {...thread,history_after_id:after,last_message:items.at(-1)||null,unread_count:items.filter(message=>message.id>through&&message.sender_id!==user).length,read_through_id:through};
  };
  const server = http.createServer(async (req,res) => {
    const url = new URL(req.url,'http://localhost'), user = /fixture-user=([^;]+)/.exec(req.headers.cookie||'')?.[1]||'alice';
    res.setHeader('Cache-Control','no-store');
    if(url.pathname==='/') { res.setHeader('Content-Type','text/html');res.end('<!doctype html><meta charset="utf-8"><main id="audio-test" data-kind="'+(url.searchParams.get('kind')==='ptt'?'ptt':'dispatch')+'"></main><section id="messages" aria-label="Messages"></section><script src="/messaging.js"></script><script src="/testing.js"></script>');return; }
    if(url.pathname==='/messaging.js'||url.pathname==='/testing.js') { res.setHeader('Content-Type','application/javascript');res.end(url.pathname==='/messaging.js'?source:testing);return; }
    let raw='';for await(const chunk of req)raw+=chunk;
    const body=raw?JSON.parse(raw):null;
    requests.push({user,path:url.pathname,query:url.search,method:req.method,body,csrf:req.headers['x-csrf-token']});
    if(url.pathname==='/api/messages/events') {
      res.writeHead(200,{'Content-Type':'text/event-stream','Connection':'keep-alive'});res.write('retry: 50\n\n: connected\n\n');
      const stream={user,response:res};streams.add(stream);req.on('close',()=>streams.delete(stream));return;
    }
    res.setHeader('Content-Type','application/json');
    const send=(value,status=200)=>{res.statusCode=status;res.end(JSON.stringify(value));};
    if(url.pathname==='/api/state') {send({username:user,csrf:'csrf-'+user,user:user==='admin'?{radio_id:10002}:{id:user,radio_id:10002},is_administrator:user==='admin',profile:{},system:{talkgroups:[]}});return;}
    if(req.method!=='GET'&&req.headers['x-csrf-token']!=='csrf-'+user){send({error:'Missing CSRF'},403);return;}
    if(url.pathname==='/api/messages/people'){send({people});return;}
    if(url.pathname==='/api/messages/threads') {
      if(req.method==='GET') {
        if(failThreads){failThreads=false;send({error:'Inbox unavailable'},503);return;}
        send({threads:threads.filter(thread=>thread.members.includes(user)).map(thread=>threadValue(thread,user))});return;
      }
      let thread=threads.find(thread=>body.kind==='direct'&&thread.kind==='direct'&&thread.members.includes(user)&&body.members.length===1&&thread.members.includes(body.members[0]));
      if(!thread){thread={id:'thread-'+(threads.length+1),kind:body.kind,name:body.name,members:[...new Set([user,...body.members])],created_by:user,created_at:'2026-11-04T12:01:00Z'};threads.push(thread);}
      send({thread:threadValue(thread,user)});events(thread.members);return;
    }
    const match=/^\/api\/messages\/threads\/([^/]+)(?:\/(messages|read|members))?$/.exec(url.pathname);
    if(!match){send({error:'Unexpected endpoint'},404);return;}
    const thread=threads.find(thread=>thread.id===decodeURIComponent(match[1])&&thread.members.includes(user));
    if(!thread){send({error:'Conversation unavailable'},403);return;}
    if(match[2]==='members') {
      if(thread.created_by!==user){send({error:'Only the creator manages members'},403);return;}
      if(failMembers){failMembers=false;send({error:'Member save unavailable'},503);return;}
      if(!body.members.includes(thread.created_by)){send({error:'Creator must remain a member'},400);return;}
      const previous=thread.members;
      for(const id of body.members)if(!previous.includes(id))joined.set(id+':'+thread.id,messages.at(-1)?.id||0);
      thread.members=[...new Set(body.members)];send({thread:threadValue(thread,user)});events([...new Set([...previous,...thread.members])]);return;
    }
    if(!match[2]) {
      if(failThreadTemporary&&user==='bob'){failThreadTemporary=false;send({error:'History temporarily unavailable'},503);return;}
      if(failThread&&user==='bob'){failThread=false;send({error:'Conversation no longer available'},404);return;}
      const before=Number(url.searchParams.get('before_id'))||Infinity, after=joined.get(user+':'+thread.id)||0, all=messages.filter(message=>message.conversation_id===thread.id&&message.id<before&&message.id>after), page=all.slice(-50);
      send({thread:threadValue(thread,user),messages:page,next_before_id:all.length>page.length?page[0].id:0,rf_delivery:'pending'});return;
    }
    if(match[2]==='read') {
      if(failRead){send({error:'Read receipt unavailable'},503);return;}
      if(!messages.some(message=>message.conversation_id===thread.id&&message.id===body.message_id&&message.id>(joined.get(user+':'+thread.id)||0))){send({error:'Message not visible'},404);return;}
      const key=user+':'+thread.id;reads.set(key,Math.max(reads.get(key)||0,body.message_id));send({read_through_id:reads.get(key)});events([user]);return;
    }
    let message=messages.find(message=>message.conversation_id===thread.id&&message.sender_id===user&&message.client_id===body.client_id);
    if(!message){message={id:messages.length+1,conversation_id:thread.id,client_id:body.client_id,sender_id:user,sender_name:people.find(person=>person.id===user)?.username||user,body:body.body,created_at:'2026-11-04T12:02:00Z'};messages.push(message);}
    if(failAfterStore){failAfterStore=false;send({error:'Connection lost after storage'},503);}else send({message,delivery:'stored',rf_delivery:'pending'});
    events(thread.members);
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  const origin='http://127.0.0.1:'+server.address().port;
  const browser=await chromium.launch({headless:true,channel:'chrome'});
  async function messageContext() {
    const context=await browser.newContext();
    await context.addInitScript(() => {window.qaMicrophoneStarts=0; navigator.mediaDevices.getUserMedia=() => {window.qaMicrophoneStarts++;return Promise.reject(new DOMException('Denied in messaging fixture','NotAllowedError'));};});
    return context;
  }
  const context=await messageContext();await context.addCookies([{name:'fixture-user',value:'alice',url:origin}]);
  const bobContext=await messageContext();await bobContext.addCookies([{name:'fixture-user',value:'bob',url:origin}]);
  const ptt=await context.newPage(), dispatch=await context.newPage(), bob=await bobContext.newPage();
  const errors=[];for(const page of [ptt,dispatch,bob])page.on('pageerror',error=>errors.push(error.message));
  const status=page=>page.locator('#messages [role=status]');
  const open=async page=>{await page.locator('#messages button[data-thread="direct-1"]').click();await page.locator('#messages h3').filter({hasText:page===bob?'Alice':'Bob'}).waitFor();};
  try {
    await t.test('both apps open the same inbox while app audio waits for permission',async()=>{
      await Promise.all([ptt.goto(origin+'/?kind=ptt'),dispatch.goto(origin+'/?kind=dispatch'),bob.goto(origin+'/?kind=ptt')]);
      for(const page of [ptt,dispatch,bob]) {await page.locator('#messages button[data-thread="direct-1"]').waitFor();await page.locator('#test-connect').waitFor({state:'visible'});assert.equal(await page.evaluate(()=>window.qaMicrophoneStarts),1);}
      const count=requests.length;await ptt.waitForTimeout(300);
      assert.equal(requests.length,count,'SSE stays quiet without automatic polling');
      assert.equal(requests.filter(request=>request.method!=='GET').length,0,'opening inbox does not send messages or mark unseen history read');
      assert.equal(requests.filter(request=>request.path.startsWith('/api/dispatch/')).length,0,'messages do not connect audio');
      assert.equal(await ptt.locator('#messages [disabled],#messages details.help').count(),0);
      assert.match(await ptt.locator('#messages').innerText(),/Application Server.*RF delivery pending/);
    });
    await t.test('only displayed history is marked read and older pages remain ordered',async()=>{
      await open(ptt);
      await ptt.waitForFunction(()=>document.querySelectorAll('#messages ol li').length===50);
      await ptt.waitForFunction(()=>document.querySelector('#messages strong').textContent==='0 unread messages');
      assert.equal(requests.filter(request=>request.path.endsWith('/read')&&request.user==='alice').at(-1).body.message_id,55);
      assert.equal(await ptt.locator('#messages img').count(),0);assert.equal(await ptt.evaluate(()=>window.injected),undefined);
      await ptt.getByRole('button',{name:'Load older messages',exact:true}).click();
      await ptt.waitForFunction(()=>document.querySelectorAll('#messages ol li').length===55);
      assert.deepEqual(await ptt.locator('#messages ol li').evaluateAll(rows=>rows.map(row=>Number(row.dataset.message))),Array.from({length:55},(_,index)=>index+1));
      await dispatch.waitForFunction(()=>document.querySelector('#messages strong').textContent==='0 unread messages');
      await open(dispatch);await open(bob);
    });
    await t.test('a failed send keeps exact draft and reuses its client ID after lost receipt',async()=>{
      failAfterStore=true;
      const text='  Unseen text\n<img src=x onerror="window.injected=true">  ';
      await ptt.locator('#message-body').fill(text);await ptt.getByRole('button',{name:'Send message',exact:true}).click();
      await status(ptt).filter({hasText:'Your draft is kept'}).waitFor();
      assert.equal(await ptt.locator('#message-body').inputValue(),text);
      const first=requests.filter(request=>request.path.endsWith('/messages')&&request.user==='alice').at(-1);
      await ptt.getByRole('button',{name:'Send message',exact:true}).click();
      await ptt.waitForFunction(()=>document.querySelector('#message-body').value==='');
      const retry=requests.filter(request=>request.path.endsWith('/messages')&&request.user==='alice').at(-1);
      assert.equal(first.body.client_id,retry.body.client_id);assert.equal(retry.body.body,text);
      assert.equal(messages.filter(message=>message.body===text).length,1,'an acknowledged retry does not duplicate the stored message');
      await dispatch.locator('#messages ol li p').filter({hasText:'Unseen text'}).waitFor();
      assert.equal(await dispatch.locator('#messages img').count(),0);
      assert.ok(requests.filter(request=>request.method!=='GET').every(request=>request.csrf==='csrf-'+request.user));
    });
    await t.test('incoming SSE refresh preserves draft, loaded history and reading position',async()=>{
      await ptt.locator('#message-body').fill('Unfinished reply');
      await ptt.locator('#messages ol').evaluate(node=>{node.scrollTop=40;});
      const top=await ptt.locator('#messages ol').evaluate(node=>node.scrollTop);
      await bob.locator('#message-body').fill('Real reply from Bob');await bob.getByRole('button',{name:'Send message',exact:true}).click();
      await ptt.locator('#messages ol li p').filter({hasText:'Real reply from Bob'}).waitFor();
      assert.equal(await ptt.locator('#message-body').inputValue(),'Unfinished reply');
      assert.equal(await ptt.locator('#messages ol li').count(),57,'refresh retains older loaded pages');
      assert.equal(await ptt.locator('#messages ol').evaluate(node=>node.scrollTop),top);
      const posts=requests.filter(request=>request.path.endsWith('/messages')).length;
      await ptt.reload();await ptt.locator('#messages button[data-thread="direct-1"]').waitFor();
      assert.equal(requests.filter(request=>request.path.endsWith('/messages')).length,posts,'page load never replays a draft or failed send');
    });
    await t.test('private groups show actual members and failed read receipts remain retryable',async()=>{
      await ptt.locator('#message-kind').selectOption('group');await ptt.locator('#message-name').fill('Group <script>');
      await ptt.locator('#message-new input[type=checkbox][value=bob]').check();
      await ptt.getByRole('button',{name:'Create conversation',exact:true}).click();
      await ptt.locator('#messages h3').filter({hasText:'Group <script>'}).waitFor();
      assert.deepEqual(await ptt.locator('#messages ul[aria-label="Conversation members"] li').allTextContents(),['Alice <img> (alice)','Bob (bob)']);
      assert.equal(await ptt.locator('#messages script').count(),0);
      const group=threads.at(-1);assert.equal(group.kind,'group');assert.deepEqual(group.members,['alice','bob']);
      failRead=true;reads.set('alice:direct-1',0);await open(ptt);
      await status(ptt).filter({hasText:'Read receipt unavailable'}).waitFor();
      assert.equal(reads.get('alice:direct-1'),0);
      failRead=false;await ptt.getByRole('button',{name:'Mark displayed messages read',exact:true}).click();
      await status(ptt).filter({hasText:'Displayed messages marked read'}).waitFor();
      assert.equal(reads.get('alice:direct-1'),57);
    });
    await t.test('creator membership edits preserve failed choices and isolate new-member history/read state',async()=>{
      const group=threads.find(thread=>thread.kind==='group');
      await ptt.locator('#messages button[data-thread="'+group.id+'"]').click();
      await ptt.locator('#message-members').waitFor();
      await ptt.locator('#message-body').fill('Before Charlie joined');await ptt.getByRole('button',{name:'Send message',exact:true}).click();
      await ptt.locator('#messages ol li p').filter({hasText:'Before Charlie joined'}).waitFor();
      await bob.locator('#messages button[data-thread="'+group.id+'"]').click();await bob.locator('#messages ol li p').filter({hasText:'Before Charlie joined'}).waitFor();
      assert.equal(await bob.locator('#message-members').isVisible(),false,'only the creator has membership controls');
      await ptt.locator('#message-members input[value=charlie]').check();failMembers=true;
      await ptt.getByRole('button',{name:'Save members',exact:true}).click();await status(ptt).filter({hasText:'Member save unavailable'}).waitFor();
      assert.equal(await ptt.locator('#message-members input[value=charlie]').isChecked(),true,'failed edits retain the selected members');
      assert.equal(group.members.includes('charlie'),false);
      await ptt.getByRole('button',{name:'Save members',exact:true}).click();
      await ptt.waitForFunction(()=>document.querySelector('#messages ul[aria-label="Conversation members"]').textContent.includes('Charlie'));
      const change=requests.filter(request=>request.path.endsWith('/members')).at(-1);assert.ok(change.body.members.includes('alice'),'creator is included without a disabled control');
      const charlieContext=await messageContext();await charlieContext.addCookies([{name:'fixture-user',value:'charlie',url:origin}]);const charlie=await charlieContext.newPage();
      try {
        await charlie.goto(origin+'/?kind=dispatch');await charlie.locator('#messages button[data-thread="'+group.id+'"]').click();
        await charlie.locator('#messages h3').filter({hasText:'Group <script>'}).waitFor();
        assert.equal(await charlie.locator('#messages ol li').count(),0,'new membership does not receive the previous message body');
        assert.equal(await charlie.locator('#messages').getByText('Before Charlie joined',{exact:true}).count(),0);
        const before=requests.filter(request=>request.user==='charlie'&&request.path.endsWith('/read')).length;
        await charlie.getByRole('button',{name:'Mark displayed messages read',exact:true}).click();await status(charlie).filter({hasText:'No messages displayed.'}).waitFor();
        assert.equal(requests.filter(request=>request.user==='charlie'&&request.path.endsWith('/read')).length,before,'an empty visible history cannot mark prejoin messages read');
        await ptt.locator('#message-body').fill('After Charlie joined');await ptt.getByRole('button',{name:'Send message',exact:true}).click();
        await charlie.locator('#messages ol li p').filter({hasText:'After Charlie joined'}).waitFor();
        await charlie.waitForFunction(()=>document.querySelector('#messages strong').textContent==='0 unread messages');
        assert.equal(reads.get('charlie:'+group.id),messages.at(-1).id);assert.equal(reads.get('charlie:direct-1'),undefined,'read cursors stay scoped to that person and conversation');
      } finally {await charlieContext.close();}
    });
    await t.test('removal and coalesced remove/rejoin discard cached history, including a later 404',async()=>{
      const group=threads.find(thread=>thread.kind==='group');
      const setBob=async checked=>{
        await ptt.locator('#message-members input[value=bob]').setChecked(checked);
        const response=ptt.waitForResponse(response=>new URL(response.url()).pathname.endsWith('/members')&&response.request().method()==='PUT');
        await ptt.getByRole('button',{name:'Save members',exact:true}).click();assert.equal((await response).status(),200);
        await ptt.waitForFunction(checked=>document.querySelector('#messages ul[aria-label="Conversation members"]').textContent.includes('Bob')===checked,checked);
      };
      const send=async text=>{await ptt.locator('#message-body').fill(text);await ptt.getByRole('button',{name:'Send message',exact:true}).click();await ptt.locator('#messages ol li p').filter({hasText:text}).waitFor();};
      await bob.locator('#message-body').fill('Unsent before removal');await setBob(false);
      await bob.waitForFunction(()=>document.querySelectorAll('#messages ol li').length===0);
      assert.equal(await bob.locator('#messages button[data-thread="'+group.id+'"]').count(),0);
      assert.equal(await bob.locator('#message-body').inputValue(),'','access loss clears the visible draft and history');
      await send('While Bob was absent');await setBob(true);
      await bob.locator('#messages button[data-thread="'+group.id+'"]').click();await bob.locator('#messages h3').filter({hasText:'Group <script>'}).waitFor();
      assert.equal(await bob.locator('#messages ol li').count(),0,'rejoin does not restore earlier membership history');
      await send('Current membership body');await bob.locator('#messages ol li p').filter({hasText:'Current membership body'}).waitFor();
      await bob.locator('#message-body').fill('Draft during rapid changes');notify=false;
      await setBob(false);await send('During rapid removal');await setBob(true);
      failThreadTemporary=true;notify=true;events(['bob']);
      await status(bob).filter({hasText:'History temporarily unavailable'}).waitFor();
      await bob.waitForFunction(()=>document.querySelectorAll('#messages ol li').length===0);
      assert.equal(await bob.locator('#messages').getByText('Current membership body',{exact:true}).count(),0,'membership boundary replaces cached bodies even when removal was coalesced');
      assert.equal(await bob.locator('#message-body').inputValue(),'Draft during rapid changes','draft remains an unsent local value');
      await bob.getByRole('button',{name:'Refresh messages',exact:true}).click();
      await send('Body before unavailable response');await bob.locator('#messages ol li p').filter({hasText:'Body before unavailable response'}).waitFor();
      failThread=true;await bob.getByRole('button',{name:'Refresh messages',exact:true}).click();
      await bob.waitForFunction(()=>document.querySelectorAll('#messages ol li').length===0);
      assert.equal(await bob.locator('#message-body').inputValue(),'','a404 cannot leave the former conversation content on screen');
    });
    await t.test('SSE reconnection refreshes without replay and the message panel fits 320 pixels',async()=>{
      const before=requests.filter(request=>request.path==='/api/messages/threads'&&request.method==='GET'&&request.user==='alice').length;
      const sends=requests.filter(request=>request.path.endsWith('/messages')).length;
      await ptt.locator('#message-body').fill('Local draft at reconnect');
      for(const stream of [...streams])if(stream.user==='alice')stream.response.end();
      await ptt.waitForFunction(()=>document.querySelector('#messages [role=status]').textContent.includes('connected'));
      await ptt.waitForTimeout(200);
      assert.ok(requests.filter(request=>request.path==='/api/messages/threads'&&request.method==='GET'&&request.user==='alice').length>before);
      assert.equal(requests.filter(request=>request.path.endsWith('/messages')).length,sends);
      assert.equal(await ptt.locator('#message-body').inputValue(),'Local draft at reconnect');
      await ptt.setViewportSize({width:320,height:800});await ptt.addStyleTag({content:'input[type=text]{width:26em;max-width:95vw}'});
      assert.ok(await ptt.locator('#messages').evaluate(node=>node.scrollWidth<=node.clientWidth+1),'message controls stay within the narrow panel');
      await ptt.setViewportSize({width:1280,height:720});
    });
    await t.test('known stored result stays distinct from a failed inbox refresh',async()=>{
      notify=false;failThreads=true;await ptt.locator('#message-body').fill('Stored despite refresh failure');
      await ptt.getByRole('button',{name:'Send message',exact:true}).click();
      await status(ptt).filter({hasText:'stored. Refresh failed: Inbox unavailable'}).waitFor();
      assert.equal(await ptt.locator('#message-body').inputValue(),'');
      assert.doesNotMatch(await status(ptt).innerText(),/draft is kept|retry/);
      notify=true;
    });
    await t.test('account change clears the prior personal view and administrators do not open an inbox',async()=>{
      await context.addCookies([{name:'fixture-user',value:'charlie',url:origin}]);
      await ptt.getByRole('button',{name:'Reload account and groups',exact:true}).click();
      await ptt.locator('#messages > p').filter({hasText:'Signed in as Charlie'}).waitFor();
      assert.equal(await ptt.locator('#message-body').inputValue(),'');assert.equal(await ptt.locator('#messages ol li').count(),0);
      assert.equal(await ptt.locator('#messages button[data-thread="direct-1"]').count(),0);
      const admin=await messageContext();await admin.addCookies([{name:'fixture-user',value:'admin',url:origin}]);const page=await admin.newPage();
      const before=requests.length;await page.goto(origin+'/?kind=dispatch');await page.getByText('Sign in with a person account to use messages.',{exact:true}).waitFor();
      assert.equal(requests.slice(before).filter(request=>request.path.startsWith('/api/messages/')).length,0);await admin.close();
    });
    assert.deepEqual(errors,[]);
    for(const page of [ptt,dispatch,bob])assert.equal(await page.evaluate(()=>window.qaMicrophoneStarts),1,'messaging does not restart denied capture');
    assert.equal(requests.filter(request=>request.path.startsWith('/api/operator/')||request.path.startsWith('/api/dispatch/')).length,0,'no RF command or audio session was requested');
  } finally {
    await browser.close();for(const stream of streams)stream.response.end();await new Promise(resolve=>server.close(resolve));
  }
});

test('a delayed initial detail cannot restore bodies after an observed rejoin cutoff', async () => {
  const {chromium}=await import(process.env.TRUNKSYSTEM_PLAYWRIGHT || 'playwright');
  const browser=await chromium.launch({headless:true,channel:'chrome'});
  try {
    const page=await browser.newPage();
    await page.setContent('<section id="messages"></section>');
    await page.addScriptTag({content:source});
    await page.evaluate(() => {
      const initial={id:'group',kind:'group',name:'Private group',members:['alice','bob'],created_by:'alice',history_after_id:0,unread_count:1,read_through_id:0};
      window.qaThread=initial; window.qaOldRendered=false;
      new MutationObserver(()=>{if(document.querySelector('#messages ol')?.textContent.includes('PREJOIN SECRET'))window.qaOldRendered=true;}).observe(document.querySelector('#messages'),{subtree:true,childList:true,characterData:true});
      let details=0;
      window.TrunkMessaging.mount(document.querySelector('#messages'),{userID:'bob',request:async path=>{
        if(path.endsWith('/people'))return {people:[{id:'alice',username:'Alice'},{id:'bob',username:'Bob'}]};
        if(path.endsWith('/threads'))return {threads:[window.qaThread]};
        if(path.includes('/threads/group?')) {
          if(++details===1)return new Promise(resolve=>{window.qaReleaseOld=()=>resolve({thread:initial,messages:[{id:1,sender_id:'alice',body:'PREJOIN SECRET',created_at:'before'}]});});
          throw new Error('Fresh detail unavailable');
        }
        throw new Error('Unexpected request '+path);
      }});
    });
    await page.locator('button[data-thread=group]').click();
    await page.waitForFunction(()=>typeof window.qaReleaseOld==='function');
    await page.evaluate(()=>{window.qaThread={...window.qaThread,history_after_id:2,unread_count:0};});
    await page.getByRole('button',{name:'Refresh messages',exact:true}).click();
    await page.locator('#messages [role=status]').filter({hasText:'Fresh detail unavailable'}).waitFor();
    await page.evaluate(()=>window.qaReleaseOld());
    await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
    assert.equal(await page.locator('#messages ol li').count(),0);
    assert.equal(await page.evaluate(()=>window.qaOldRendered),false,'old bodies never render after the newer list cutoff, even when fresh detail fails');
  } finally {await browser.close();}
});
