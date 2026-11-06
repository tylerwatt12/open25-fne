import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';

// Only loopback fixture traffic is used. PCMU audio is negotiated by real Chrome
// peers; no production endpoint, USB device or radio is contacted.
const source = await fs.readFile(new URL('./testing.js', import.meta.url), 'utf8');
const setup = String.raw`
window.fixture = {requests:[], messages:[], failures:[], micLevel:0, calls:new Map(), microphoneOptions:[], microphones:[], sinkRequests:[], removedDevices:[], audioContexts:[], connections:[],peers:[],serverSessions:[],...window.fixtureOptions};
const AudioContextBase=window.AudioContext;window.AudioContext=class extends AudioContextBase{constructor(...args){super(...args);fixture.audioContexts.push(this);}get state(){return fixture.suspendAudio?'suspended':super.state;}resume(){return fixture.suspendAudio?new Promise(()=>{}):super.resume();}};
const PeerBase=window.RTCPeerConnection;window.RTCPeerConnection=class extends PeerBase{constructor(...args){super(...args);fixture.peers.push(this);}};
const connectNode=AudioNode.prototype.connect;AudioNode.prototype.connect=function(destination,...args){fixture.connections.push({source:this,destination});return connectNode.call(this,destination,...args);};
const getUserMedia=navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
navigator.mediaDevices.getUserMedia=async options=>{fixture.microphoneOptions.push(structuredClone(options));if(fixture.microphoneError)throw new DOMException('Microphone permission denied','NotAllowedError');if(fixture.removedDevices.includes(options.audio?.deviceId?.exact))throw new DOMException('Selected microphone is no longer available','NotFoundError');const microphone=await getUserMedia(options);fixture.microphone=microphone;fixture.microphones.push(microphone);if(fixture.delayMicrophone)await new Promise(resolve=>fixture.releaseMicrophone=resolve);return microphone;};
if(fixture.missingMicrophoneAPI)navigator.mediaDevices.getUserMedia=undefined;
const enumerateDevices=navigator.mediaDevices.enumerateDevices.bind(navigator.mediaDevices);navigator.mediaDevices.enumerateDevices=async()=> (await enumerateDevices()).filter(device=>!fixture.removedDevices.includes(device.deviceId));
const playMedia=HTMLMediaElement.prototype.play;HTMLMediaElement.prototype.play=function(){if(this.dataset.output==='receive'&&fixture.playDenied)return Promise.reject(new DOMException('Playback permission denied','NotAllowedError'));return playMedia.call(this);};
const setSinkId=HTMLMediaElement.prototype.setSinkId;fixture.setSinkId=async function(id){fixture.sinkRequests.push({player:this,id});if(fixture.speakerError)throw new DOMException('Speaker permission denied','NotAllowedError');return setSinkId.call(this,id);};HTMLMediaElement.prototype.setSinkId=fixture.setSinkId;
fixture.outputLevels=async()=>{const audio=new AudioContext(),input=audio.createMediaStreamSource(document.querySelector('audio[data-output=receive]').srcObject),split=audio.createChannelSplitter(2),silent=audio.createGain();silent.gain.value=0;input.connect(split);const meters=[audio.createAnalyser(),audio.createAnalyser()];meters.forEach((meter,index)=>{split.connect(meter,index);meter.connect(silent);});silent.connect(audio.destination);await audio.resume();await new Promise(r=>setTimeout(r,250));const values=meters.map(meter=>{const samples=new Float32Array(meter.fftSize);meter.getFloatTimeDomainData(samples);return Math.sqrt(samples.reduce((sum,x)=>sum+x*x,0)/samples.length);});await audio.close();return values;};
window.WebSocket=class extends EventTarget {
 static OPEN=1;
 constructor(){super();this.readyState=0;this.queue=Promise.resolve();fixture.socket=this;fixture.serverSessions.push(this);setTimeout(()=>{if(this.readyState===3)return;this.readyState=1;this.onopen?.();},0);}
 deliver(value){if(this.readyState===1)this.onmessage?.({data:JSON.stringify(value)});}
 send(raw){const message=JSON.parse(raw);fixture.messages.push(message);this.queue=this.queue.then(async()=>{
  if(fixture.noAnswer)return;
  if(message.type==='offer'){
   const audio=this.audio=fixture.audio=new AudioContext();await audio.resume();const peer=this.peer=fixture.peer=new RTCPeerConnection();
   peer.onicecandidate=e=>{if(e.candidate)this.deliver({type:'ice',candidate:e.candidate.toJSON()});};
   peer.ontrack=e=>{const stream=e.streams[0]||new MediaStream([e.track]);const input=audio.createMediaStreamSource(stream),meter=audio.createAnalyser(),silent=audio.createGain();silent.gain.value=0;input.connect(meter);meter.connect(silent);silent.connect(audio.destination);this.micTimer=fixture.micTimer=setInterval(()=>{const samples=new Float32Array(meter.fftSize);meter.getFloatTimeDomainData(samples);fixture.micLevel=Math.sqrt(samples.reduce((s,x)=>s+x*x,0)/samples.length);},50);};
   await peer.setRemoteDescription({type:'offer',sdp:message.sdp});const transceivers=peer.getTransceivers();transceivers[0].direction='recvonly';
   for(let i=1;i<transceivers.length;i++){
    const output=audio.createMediaStreamDestination(),oscillator=audio.createOscillator(),gain=audio.createGain();oscillator.frequency.value=400+i*200;gain.gain.value=0;oscillator.connect(gain);gain.connect(output);oscillator.start();fixture.calls.set(1000+i,{gain,oscillator});transceivers[i].direction='sendonly';await transceivers[i].sender.replaceTrack(output.stream.getAudioTracks()[0]);transceivers[i].sender.setStreams(output.stream);
   }
   const codecs=RTCRtpSender.getCapabilities('audio').codecs.filter(c=>c.mimeType.toLowerCase()==='audio/pcmu');transceivers.forEach(t=>t.setCodecPreferences(codecs));
   await peer.setLocalDescription(await peer.createAnswer());this.deliver({type:'answer',sdp:peer.localDescription.sdp});
  } else if(message.type==='ice') await this.peer.addIceCandidate(message.candidate);
 }).catch(error=>fixture.failures.push(error.message));}
 close(){this.readyState=3;clearInterval(this.micTimer);this.peer?.close();if(this.audio&&this.audio.state!=='closed')this.audio.close();}
};
fixture.grant=(id,security={secure:false,algorithm_id:0,key_id:0})=>fixture.socket.deliver({type:'state',state:{tx:{state:'transmitting',request_id:id,...security},available:true}});
fixture.call=(id,active,callID='call-'+id,security={})=>{fixture.calls.get(id).gain.gain.value=active?.15:0;fixture.socket.deliver({type:'rx',talkgroup_id:id,source_id:10003,call_id:callID,active,...security});};
fixture.endMetadata=id=>fixture.socket.deliver({type:'rx',talkgroup_id:id,source_id:10003,call_id:'call-'+id,active:false});
fixture.drop=()=>{const socket=fixture.socket;socket.close();socket.onclose?.();};
fixture.finish=async()=>{for(const socket of fixture.serverSessions)socket.close();};
`;

function wave(rate=48000,seconds=30) {
  const samples=rate*seconds, b=Buffer.alloc(44+samples*2);
  b.write('RIFF',0);b.writeUInt32LE(b.length-8,4);b.write('WAVEfmt ',8);b.writeUInt32LE(16,16);b.writeUInt16LE(1,20);b.writeUInt16LE(1,22);b.writeUInt32LE(rate,24);b.writeUInt32LE(rate*2,28);b.writeUInt16LE(2,32);b.writeUInt16LE(16,34);b.write('data',36);b.writeUInt32LE(samples*2,40);
  for(let i=0;i<samples;i++)b.writeInt16LE(Math.round(Math.sin(i*2*Math.PI*750/rate)*9000),44+i*2);
  return b;
}

test('plain Dispatch/PTT pages preserve actual audio and bounded call ownership', {timeout:180000}, async t=>{
  let chromium;
  try { ({chromium}=await import(process.env.TRUNKSYSTEM_PLAYWRIGHT||'playwright')); }
  catch(error){throw new Error('Set TRUNKSYSTEM_PLAYWRIGHT to the installed Playwright module: '+error.message);}
  const directory=await fs.mkdtemp(path.join(os.tmpdir(),'trunksystem-plain-audio-'));
  const microphone=path.join(directory,'microphone.wav');await fs.writeFile(microphone,wave());
  const requests=[];
  let controllerSnapshot={call_delivery:[],emergencies:[],patches:[]}, patchFailure="", profileReceive=[], historyPage={calls:[]}, holdAnnouncements=false;
  let profile={selected_talkgroup:1001,zones:[{name:'Lab',talkgroups:[1001,1002]}]}, profileFailure="";
  const server=http.createServer(async(req,res)=>{
    const url=new URL(req.url,'http://localhost');res.setHeader('Cache-Control','no-store');
    if(url.pathname==='/'){
      res.setHeader('Content-Type','text/html');res.end('<!doctype html><meta charset="utf-8"><main id="audio-test" data-kind="'+(url.searchParams.get('kind')==='ptt'?'ptt':'dispatch')+'"></main><script>'+setup+'</script><script src="/testing.js"></script>');return;
    }
    if(url.pathname==='/testing.js'){res.setHeader('Content-Type','application/javascript');res.end(source);return;}
    let body='';for await(const chunk of req)body+=chunk;
    const record={path:url.pathname,query:url.search,method:req.method,body,csrf:req.headers['x-csrf-token'],contentType:req.headers['content-type']};requests.push(record);
    res.setHeader('Content-Type','application/json');
    if(url.pathname==='/api/state')res.end(JSON.stringify({username:'fixture',csrf:'fixture-csrf',user:{radio_id:10002},profile:{...profile,receive:profileReceive},system:{talkgroups:[{tgid:1001,alias:'Alpha',enabled:true,admission_mode:'both',transmit_mode:'selectable'},{tgid:1002,alias:'Bravo',enabled:true,admission_mode:'clear',transmit_mode:'clear'}]}}));
    else if(url.pathname==='/api/profile'&&req.method==='PUT'){
      if(profileFailure){res.statusCode=503;res.end(JSON.stringify({error:profileFailure}));}
      else{profile=JSON.parse(body);profileReceive=profile.receive;res.end(JSON.stringify({saved:true}));}
    }
    else if(url.pathname==='/api/dispatch/session')res.end(JSON.stringify({ticket:'local-test',socket_path:'/local-test',receive_groups:[1001,1002],transmit_groups:[1001,1002],keys:[{algorithm_id:132,key_id:0},{algorithm_id:129,key_id:9}]}));
    else if(url.pathname==='/api/operator/state')res.end(JSON.stringify(controllerSnapshot));
    else if(url.pathname==='/api/operator/patch'&&patchFailure){res.statusCode=409;res.end(JSON.stringify({error:patchFailure}));}
    else if(url.pathname==='/api/operator/radio-commands')res.end(JSON.stringify({commands:[]}));
    else if(url.pathname==='/api/operator/units')res.end(JSON.stringify({units:[]}));
    else if(url.pathname==='/api/operator/radio-command'&&JSON.parse(body).action==='message'&&JSON.parse(body).code===null){res.statusCode=400;res.end(JSON.stringify({error:'Enter a message code.'}));}
    else if(url.pathname.startsWith('/api/operator/'))res.end(JSON.stringify({received:JSON.parse(body)}));
    else if(url.pathname.startsWith('/api/history/'))res.end(JSON.stringify(historyPage));
    else if(holdAnnouncements&&['/api/dispatch/tts','/api/dispatch/wav'].includes(url.pathname))return;
    else if(url.pathname.startsWith('/api/dispatch/'))res.end(JSON.stringify({completed:true}));
    else{res.statusCode=404;res.end(JSON.stringify({error:'Unexpected endpoint'}));}
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  const origin='http://127.0.0.1:'+server.address().port;
  const browser=await chromium.launch({headless:true,channel:'chrome',args:['--use-fake-ui-for-media-stream','--use-fake-device-for-media-stream','--use-file-for-fake-audio-capture='+microphone,'--autoplay-policy=no-user-gesture-required']});
  try {
    for(const kind of ['dispatch','ptt']) await t.test(kind+' uses one state read and real PCMU receive',async()=>{
      const context=await browser.newContext({permissions:['microphone']});const page=await context.newPage();const errors=[];page.on('pageerror',error=>errors.push(error.message));
      const before=requests.length;
      try {
        await page.goto(origin+'/?kind='+kind);
        await page.waitForFunction(()=>document.getElementById('test-identity').textContent.includes('10002'));
        await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
        assert.equal(requests.slice(before).filter(r=>r.path==='/api/state').length,1);
        assert.deepEqual(requests.slice(before).filter(r=>r.method!=='GET').map(r=>r.path),['/api/dispatch/session'],'opening creates audio only, without issuing a radio operation');
        assert.equal(await page.locator('#test-connect').isVisible(),false,'automatic audio needs no enable button');
        assert.equal(await page.locator('#test-disconnect').count(),0);
        assert.equal(await page.locator('link[rel=stylesheet]').count(),0);
        assert.equal(await page.locator('#test-patch-select').count(),kind==='dispatch'?1:0,'PTT keeps its separate controls');
        assert.equal(await page.locator('#test-key').inputValue(),'0:0');assert.equal(await page.locator('#test-key option').count(),3);
        assert.ok(await page.getByText('Not implemented',{exact:false}).count()>10);
        await page.locator('#test-command [name=radio_id]').fill('10001');
        await page.locator('#test-command [name=action]').selectOption('message');
        await page.locator('#test-command [name=code]').fill('0');
        await page.locator('#test-command button').click();
        await page.waitForFunction(()=>document.getElementById('test-result').textContent.includes('Controller response received'));
        const coded=requests.slice(before).filter(r=>r.path==='/api/operator/radio-command');
        assert.equal(coded.length,1);assert.equal(coded[0].csrf,'fixture-csrf');
        assert.deepEqual(JSON.parse(coded[0].body),{action:'message',radio_id:10001,talkgroup_id:1001,code:0});
        await page.locator('#test-command [name=code]').fill('');
        await page.locator('#test-command button').click();
        await page.waitForFunction(()=>document.getElementById('test-result').textContent==='Enter a message code.');
        assert.equal(JSON.parse(requests.filter(r=>r.path==='/api/operator/radio-command').at(-1).body).code,null,'a blank code never becomes code zero');
        assert.equal(await page.locator('#test-command [name=code]').inputValue(),'');
        await page.locator('#test-command [name=action]').selectOption('radio_check');
        await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
        assert.deepEqual(await page.locator('#test-key option').allTextContents(),["Talkgroup's assigned key",'Algorithm 132, Key 0','Algorithm 129, Key 9']);
        await page.evaluate(()=>fixture.call(1001,true));
        await page.waitForFunction(()=>Number(document.getElementById('test-rx-level').value)>3);
        const received=await page.evaluate(async()=>{let packets=0;for(const r of(await fixture.peer.getStats()).values())if(r.type==='outbound-rtp')packets+=r.packetsSent;return packets;});
        assert.ok(received>0);
        // A control End must not clip audio already admitted on the RTC path.
        await page.evaluate(()=>fixture.endMetadata(1001));await page.waitForTimeout(250);
        assert.ok(await page.locator('#test-rx-level').evaluate(el=>el.value)>3);
        await page.locator('#listen-1001').uncheck();await page.waitForTimeout(250);
        assert.ok(await page.locator('#test-rx-level').evaluate(el=>el.value)<.5);
        await page.locator('#listen-1001').check();
        await page.evaluate(()=>fixture.call(1001,true));
        await page.waitForFunction(()=>Number(document.getElementById('test-rx-level').value)>3);
        assert.ok(await page.evaluate(()=>fixture.micLevel)<.005,'microphone remains silent before a grant');
        // A connected audio session must not turn message navigation or text
        // entry into an RF request through the page-wide Space shortcut.
        await page.evaluate(()=>{
          const section=document.createElement('section');section.id='messages';
          const history=document.createElement('ol');history.id='message-keyboard-history';history.tabIndex=0;
          const item=document.createElement('li');item.textContent='Received message';history.append(item);
          const compose=document.createElement('textarea');compose.id='message-keyboard-compose';
          section.append(history,compose);document.body.append(section);
          fixture.messageKeyboardStart=fixture.messages.length;
        });
        await page.locator('#message-keyboard-history').focus();await page.keyboard.press('Space');
        await page.locator('#message-keyboard-compose').focus();await page.keyboard.press('Space');
        assert.deepEqual(await page.evaluate(()=>fixture.messages.slice(fixture.messageKeyboardStart).filter(message=>message.type==='ptt_start'||message.type==='ptt_stop')),[],'Space in message history and compose does not operate PTT');
        assert.equal(await page.locator('#message-keyboard-compose').inputValue(),' ','compose keeps ordinary Space entry');
        assert.ok(await page.evaluate(()=>fixture.micLevel)<.005,'message keyboard use leaves microphone unadmitted');
        await page.locator('#messages').evaluate(node=>node.remove());
        await page.locator('#test-ptt').hover();await page.mouse.down();
        let first=await page.evaluate(()=>fixture.messages.findLast(m=>m.type==='ptt_start'));
        assert.ok(first.request_id);assert.deepEqual(first.talkgroups,[1001]);assert.equal(first.algorithm_id,0);assert.equal(first.key_id,0);
        await page.waitForTimeout(200);assert.ok(await page.evaluate(()=>fixture.micLevel)<.005,'request alone does not transmit microphone');
        await page.evaluate(id=>fixture.grant(id,{secure:true,algorithm_id:132,key_id:7}),first.request_id);
        await page.waitForFunction(()=>fixture.micLevel>.01);
        assert.equal(await page.locator('#test-transmit').textContent(),'Transmitting encrypted · Algorithm 132, Key 7','display uses actual granted mode rather than clear intent');
        await page.mouse.up();await page.waitForTimeout(200);
        assert.ok(await page.evaluate(()=>fixture.micLevel)<.005,'release stops microphone immediately');
        // A late grant for a released request cannot key the microphone.
        await page.evaluate(id=>fixture.grant(id),first.request_id);await page.waitForTimeout(150);
        assert.ok(await page.evaluate(()=>fixture.micLevel)<.005);
        await page.locator('#test-key').selectOption('132:0');
        assert.equal(await page.locator('#test-secure').isChecked(),false,'selecting a key preserves the clear/encrypted request');
        assert.equal(await page.evaluate(()=>fixture.messages.filter(m=>m.type==='ptt_start').length),1,'key choice alone never starts transmission');
        await page.locator('#test-secure').check();
        await page.locator('#test-ptt').focus();await page.keyboard.down('Space');
        const second=await page.evaluate(()=>fixture.messages.findLast(m=>m.type==='ptt_start'));
        assert.notEqual(second.request_id,first.request_id);assert.equal(second.algorithm_id,132);assert.equal(second.key_id,0);
        await page.evaluate(id=>fixture.socket.deliver({type:'error',action:'ptt_start',request_id:id,message:'old error'}),first.request_id);
        await page.evaluate(id=>fixture.grant(id),second.request_id);await page.waitForFunction(()=>fixture.micLevel>.01);
        assert.equal(await page.locator('#test-transmit').textContent(),'Transmitting clear','display uses actual granted mode rather than encrypted intent');
        await page.evaluate(()=>window.dispatchEvent(new Event('blur')));await page.keyboard.up('Space');await page.waitForTimeout(150);
        assert.ok(await page.evaluate(()=>fixture.micLevel)<.005,'loss of focus releases PTT');
        await page.locator('#test-ptt').focus();await page.keyboard.down('Space');
        const selected=await page.evaluate(()=>fixture.messages.findLast(m=>m.type==='ptt_start'));
        assert.equal(selected.algorithm_id,132);assert.equal(selected.key_id,0);
        await page.evaluate(id=>fixture.grant(id,{secure:true,algorithm_id:132,key_id:0}),selected.request_id);
        await page.waitForFunction(()=>fixture.micLevel>.01);
        assert.equal(await page.locator('#test-transmit').textContent(),'Transmitting encrypted · Algorithm 132, Key 0');
        await page.locator('#test-key').selectOption('129:9');await page.keyboard.up('Space');await page.waitForTimeout(150);
        assert.ok(await page.evaluate(()=>fixture.micLevel)<.005,'changing the transmit key releases PTT');
        assert.equal(await page.evaluate(()=>fixture.messages.findLast(m=>m.type==='ptt_stop').request_id),selected.request_id);
        await page.evaluate(()=>fixture.call(1001,true,'received-encrypted',{encrypted:true,algorithm_id:132,key_id:0}));
        assert.equal(await page.locator('#call-1001').textContent(),'Radio 10003  · Encrypted · Algorithm 132, Key 0','receive indication uses the received key independently of selected transmit key');
        await page.evaluate(()=>fixture.call(1001,true,'received-clear',{encrypted:false,algorithm_id:128,key_id:0}));
        await page.waitForTimeout(250); // Let real playout and its meter observe the clear call.
        await page.waitForFunction(()=>Number(document.getElementById('test-rx-level').value)>3);
        assert.equal(await page.locator('#call-1001').textContent(),'Radio 10003  · Clear','permitted clear calls remain audible during encrypted transmit selection');
        assert.equal(await page.locator('#test-secure').isChecked(),true);
        await page.locator('#test-key').selectOption('132:0');
        const commandResults=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/operator/radio-commands');await page.locator('#test-command-results').click();await commandResults;
        const units=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/operator/units');await page.locator('#test-units').click();await units;
        assert.equal(requests.findLast(r=>r.path==='/api/operator/units').query,'?talkgroup_id=1001');
        await page.locator('#test-read-results').click();
        await page.waitForFunction(()=>document.getElementById('test-controller-results').textContent.includes('call_delivery'));
        assert.equal(requests.slice(before).filter(r=>r.path==='/api/state').length,1,'active audio never polls saved configuration');
        assert.equal(requests.slice(before).filter(r=>r.path.includes('/crypto/')).length,0,'loaded public key choices arrive with the audio session, without extra key queries');
        // Each plain control sends the existing backend operation directly.
        const post=async(id,endpoint)=>{const response=page.waitForResponse(r=>new URL(r.url()).pathname===endpoint&&r.request().method()==='POST');await page.locator(id).click();await response;return requests.findLast(r=>r.path===endpoint);};
        assert.deepEqual(JSON.parse((await post('#test-emergency','/api/operator/emergency')).body),{action:'declare',talkgroup_id:1001});
        await page.locator('#test-incident').fill('fixture-incident');
        assert.deepEqual(JSON.parse((await post('#test-ack','/api/operator/emergency')).body),{action:'acknowledge',id:'fixture-incident'});
        assert.deepEqual(JSON.parse((await post('#test-clear','/api/operator/emergency')).body),{action:'clear',id:'fixture-incident'});
        await page.locator('#test-command [name=radio_id]').fill('10003');
        assert.deepEqual(JSON.parse((await post('#test-command button','/api/operator/radio-command')).body),{action:'radio_check',radio_id:10003,talkgroup_id:1001});
        await page.locator('#test-command [name=action]').selectOption('page');
        assert.equal(JSON.parse((await post('#test-command button','/api/operator/radio-command')).body).action,'page');
        await page.locator('#test-secure').check();await page.locator('#test-speech').fill('Testing one two three');
        assert.deepEqual(JSON.parse((await post('#test-send-speech','/api/dispatch/tts')).body),{text:'Testing one two three',talkgroups:[1001],secure:true,algorithm_id:132,key_id:0});
        await page.locator('#test-wav').setInputFiles({name:'test.wav',mimeType:'audio/wav',buffer:wave(8000,1)});
        const wav=await post('#test-send-wav','/api/dispatch/wav');assert.equal(wav.body.slice(0,4),'RIFF');assert.equal(wav.contentType,'audio/wav');assert.equal(wav.query,'?secure=true&algorithm_id=132&key_id=0&talkgroup_id=1001');
        if(kind==='dispatch'){
          await page.locator('#test-patch [name=name]').fill('Test patch');
          await page.locator('#test-patch [name=talkgroups]').fill('1001,1002');
          await page.locator('#test-patch [name=algorithm_id]').fill('132');await page.locator('#test-patch [name=key_id]').fill('7');
          assert.deepEqual(JSON.parse((await post('#test-patch button','/api/operator/patch')).body),{action:'activate',patch:{name:'Test patch',talkgroups:[1001,1002],algorithm_id:132,key_id:7}});
          await page.locator('#test-patch-id').fill('patch-one');await page.locator('#test-patch-previous').fill('1001,1002');await page.locator('#test-patch-members').fill('1001,1003');
          assert.deepEqual(JSON.parse((await post('#test-patch-edit','/api/operator/patch')).body),{action:'update',id:'patch-one',patch:{name:'Test patch',algorithm_id:132,key_id:7,talkgroups:[1001,1003],previous_talkgroups:[1001,1002]}});
          assert.deepEqual(JSON.parse((await post('#test-patch-end','/api/operator/patch')).body),{action:'end',id:'patch-one'});
        }
        await page.locator('#test-record').click();await page.waitForTimeout(1200);await page.locator('#test-record-stop').click();
        await page.waitForFunction(()=>document.querySelector('#test-recording a[download]'));
        assert.ok(await page.locator('#test-recording a').evaluate(async el=>{const audio=new AudioContext();try{const pcm=await audio.decodeAudioData(await(await fetch(el.href)).arrayBuffer()),samples=pcm.getChannelData(0);return Math.sqrt(samples.reduce((sum,value)=>sum+value*value,0)/samples.length);}finally{await audio.close();}})>.03,'the decoded recording contains received audio');
        await page.locator('#test-record-delete').click();assert.equal(await page.locator('#test-recording audio').count(),0);
        if(kind==='ptt'){
          await page.locator('#test-scan').check();
          await page.evaluate(()=>{fixture.call(1001,true);fixture.call(1002,true);});
          await page.waitForTimeout(200);
          assert.equal(await page.locator('#call-1002').textContent(),'Radio 10003  · Clear');
          await page.evaluate(()=>fixture.call(1001,false));await page.waitForTimeout(200);
          assert.ok(await page.locator('#test-rx-level').evaluate(el=>el.value)>3);
        }
        // Loss of either local capture or the control connection releases ownership.
        await page.locator('#test-ptt').focus();await page.keyboard.down('Space');
        await page.evaluate(()=>fixture.grant(fixture.messages.findLast(m=>m.type==='ptt_start').request_id));
        await page.waitForFunction(()=>fixture.micLevel>.01,{},{timeout:3000});
        if(kind==='dispatch')await page.evaluate(()=>fixture.microphone.getAudioTracks()[0].dispatchEvent(new Event('ended')));
        else await page.evaluate(()=>fixture.socket.onerror(new Event('error')));
        await page.keyboard.up('Space');
        assert.ok(await page.evaluate(()=>fixture.microphone.getTracks().every(track=>track.readyState==='ended')));
        await page.evaluate(()=>window.dispatchEvent(new Event('pagehide')));
        assert.equal(await page.locator('#test-transmit').textContent(),'Idle');
        assert.deepEqual(await page.evaluate(()=>fixture.failures),[]);
        assert.deepEqual(errors,[]);
        assert.ok(requests.slice(before).filter(r=>r.method!=='GET').every(r=>r.csrf==='fixture-csrf'));
      } finally {await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
    });
    for(const kind of ['dispatch','ptt']) {
      await t.test(kind+' reconnects a dropped held session without replaying PTT',async()=>{
        const context=await browser.newContext({permissions:['microphone']}),page=await context.newPage(),errors=[];page.on('pageerror',error=>errors.push(error.message));
        try{
          await page.goto(origin+'/?kind='+kind);await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
          await page.locator('#test-ptt').focus();await page.keyboard.down('Space');
          await page.evaluate(()=>fixture.grant(fixture.messages.findLast(message=>message.type==='ptt_start').request_id));await page.waitForFunction(()=>fixture.micLevel>.01);
          const starts=await page.evaluate(()=>fixture.messages.filter(message=>message.type==='ptt_start').length);
          await page.evaluate(()=>{fixture.oldMicrophone=fixture.microphone;fixture.oldSocket=fixture.socket;fixture.drop();});
          assert.ok(await page.evaluate(()=>fixture.oldMicrophone.getTracks().every(track=>track.readyState==='ended')),'a dropped session releases its old microphone');
          assert.equal(await page.locator('#test-connect').isVisible(),false,'a network failure does not ask for a browser gesture');
          await page.waitForFunction(()=>fixture.socket!==fixture.oldSocket&&document.getElementById('test-connection').textContent==='connected');
          assert.equal(await page.locator('#test-transmit').textContent(),'Idle');
          assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type==='ptt_start').length),starts,'a still-held key never starts a new call after reconnect');
          await page.keyboard.up('Space');await page.waitForTimeout(150);assert.ok(await page.evaluate(()=>fixture.micLevel)<.005);
          await page.evaluate(()=>fixture.call(1001,true));await page.waitForFunction(()=>Number(document.getElementById('test-rx-level').value)>3);
          // RTC failures follow the same recovery path as socket loss.
          await page.evaluate(()=>{fixture.previousSocket=fixture.socket;const client=fixture.peers.find(peer=>peer.connectionState==='connected'&&peer!==fixture.peer);client.close();client.onconnectionstatechange?.();});
          await page.waitForFunction(()=>fixture.socket!==fixture.previousSocket&&document.getElementById('test-connection').textContent==='connected');
          assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type==='ptt_start').length),starts);
          await context.setOffline(true);await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='Waiting for network');
          const offlineCaptures=await page.evaluate(()=>fixture.microphoneOptions.length);await page.waitForTimeout(1200);
          assert.equal(await page.evaluate(()=>fixture.microphoneOptions.length),offlineCaptures,'offline browsers wait for network rather than repeatedly opening microphones');
          assert.equal(await page.locator('#test-connect').isVisible(),false);await context.setOffline(false);
          await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
          assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type==='ptt_start').length),starts);
          const captures=await page.evaluate(()=>fixture.microphoneOptions.length);
          await page.evaluate(()=>{fixture.drop();window.dispatchEvent(new Event('pagehide'));});await page.waitForTimeout(1400);
          assert.equal(await page.evaluate(()=>fixture.microphoneOptions.length),captures,'leaving the page cancels a scheduled reconnect');
          assert.ok(await page.evaluate(()=>fixture.microphones.every(stream=>stream.getTracks().every(track=>track.readyState==='ended'))));
          assert.deepEqual(errors,[]);assert.deepEqual(await page.evaluate(()=>fixture.failures),[]);
        }finally{await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
      });
      for(const blocked of ['microphoneError','suspendAudio','playDenied'])await t.test(kind+' shows Enable audio only when '+blocked+' requires browser interaction',async()=>{
        const context=await browser.newContext({permissions:['microphone']});await context.addInitScript(flag=>window.fixtureOptions={[flag]:true},blocked);
        const page=await context.newPage(),errors=[];page.on('pageerror',error=>errors.push(error.message));
        try{
          await page.goto(origin+'/?kind='+kind);await page.locator('#test-connect').waitFor({state:'visible'});
          assert.equal(await page.locator('#test-connect').textContent(),'Enable audio');assert.equal(await page.locator('#test-transmit').textContent(),'Idle');
          const captures=await page.evaluate(()=>fixture.microphoneOptions.length);await page.waitForTimeout(1400);
          assert.equal(await page.evaluate(()=>fixture.microphoneOptions.length),captures,'a blocked browser request is not repeatedly prompted');
          await page.evaluate(flag=>fixture[flag]=false,blocked);await page.locator('#test-connect').click();
          await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
          assert.equal(await page.locator('#test-connect').isVisible(),false,'successful activation removes the fallback button');
          assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type.startsWith('ptt_')).length),0);
          await page.evaluate(()=>fixture.call(1001,true));await page.waitForFunction(()=>Number(document.getElementById('test-rx-level').value)>3);
          assert.deepEqual(errors,[]);assert.deepEqual(await page.evaluate(()=>fixture.failures),[]);
        }finally{await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
      });
      await t.test(kind+' keeps explicit announcements independent of failed automatic audio setup',async()=>{
        const context=await browser.newContext({permissions:['microphone']}),page=await context.newPage(),errors=[],aborted=[];page.on('pageerror',error=>errors.push(error.message));
        page.on('requestfailed',request=>{if(['/api/dispatch/tts','/api/dispatch/wav'].includes(new URL(request.url()).pathname))aborted.push(request.url());});
        let attempts=0;await page.route('**/api/dispatch/session',async route=>{attempts++;await route.fulfill({status:503,contentType:'application/json',body:JSON.stringify({error:'Audio service temporarily unavailable'})});});
        const before=requests.length;holdAnnouncements=true;
        try{
          await page.goto(origin+'/?kind='+kind);await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='Reconnecting in 1s');
          await page.locator('#test-speech').fill('An explicitly requested announcement');
          const ttsStarted=page.waitForRequest(request=>new URL(request.url()).pathname==='/api/dispatch/tts');await page.locator('#test-send-speech').click();await ttsStarted;
          await page.waitForTimeout(1500);assert.ok(attempts>=2,'audio setup retried while the explicit announcement was pending');assert.equal(aborted.length,0,'automatic audio failure must not cancel a requested announcement');
          assert.equal(requests.slice(before).filter(request=>request.path==='/api/dispatch/tts').length,1,'audio retries do not replay the announcement');
          const ttsCancelled=page.waitForEvent('requestfailed',{predicate:request=>new URL(request.url()).pathname==='/api/dispatch/tts'});await page.locator('#test-cancel-file').click();await ttsCancelled;
          await page.waitForFunction(()=>document.getElementById('test-result').textContent==='Announcement cancelled.');
          await page.locator('#test-wav').setInputFiles({name:'test.wav',mimeType:'audio/wav',buffer:wave(8000,1)});
          const wavStarted=page.waitForRequest(request=>new URL(request.url()).pathname==='/api/dispatch/wav');await page.locator('#test-send-wav').click();await wavStarted;
          const wavCancelled=page.waitForEvent('requestfailed',{predicate:request=>new URL(request.url()).pathname==='/api/dispatch/wav'});await page.evaluate(()=>window.dispatchEvent(new Event('pagehide')));await wavCancelled;
          assert.equal(aborted.length,2,'Cancel and departure each abort only their explicitly requested announcement');
          assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type.startsWith('ptt_')).length),0);assert.deepEqual(errors,[]);
        }finally{holdAnnouncements=false;await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
      });
      await t.test(kind+' still loads account and groups when browser capture is unsupported',async()=>{
        const context=await browser.newContext();await context.addInitScript(()=>window.fixtureOptions={missingMicrophoneAPI:true});
        const page=await context.newPage(),errors=[];page.on('pageerror',error=>errors.push(error.message));const before=requests.length;
        try{
          await page.goto(origin+'/?kind='+kind);await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='Audio unavailable');
          assert.ok((await page.locator('#test-identity').textContent()).includes('10002'));assert.equal(await page.locator('#test-group').inputValue(),'1001');assert.equal(await page.locator('#test-receive tr').count(),2);
          assert.equal(await page.locator('#test-connect').isVisible(),false,'unsupported capture cannot be fixed by an enable click');
          await page.waitForTimeout(1200);assert.equal(requests.slice(before).filter(request=>request.path==='/api/state').length,1);assert.equal(requests.slice(before).filter(request=>request.path==='/api/dispatch/session').length,0);
          assert.equal(await page.evaluate(()=>fixture.microphoneOptions.length),0);assert.deepEqual(errors,[]);
        }finally{await context.close();}
      });
      await t.test(kind+' releases a microphone that resolves after page departure',async()=>{
        const context=await browser.newContext({permissions:['microphone']});await context.addInitScript(()=>window.fixtureOptions={delayMicrophone:true});
        const page=await context.newPage(),errors=[];page.on('pageerror',error=>errors.push(error.message));const before=requests.length;
        try{
          await page.goto(origin+'/?kind='+kind);await page.waitForFunction(()=>!!fixture.releaseMicrophone);
          await page.evaluate(()=>{window.dispatchEvent(new Event('pagehide'));fixture.releaseMicrophone();});
          await page.waitForFunction(()=>fixture.microphone.getTracks().every(track=>track.readyState==='ended'));await page.waitForTimeout(1200);
          assert.equal(requests.slice(before).filter(request=>request.path==='/api/dispatch/session').length,0,'canceled capture cannot start a late audio session');
          assert.equal(await page.locator('audio').count(),0);assert.equal(await page.locator('#test-connect').isVisible(),false);assert.deepEqual(errors,[]);
        }finally{await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
      });
    }
    await t.test('temporary server failures retry with backoff without reloading account programming',async()=>{
      const context=await browser.newContext({permissions:['microphone']}),page=await context.newPage(),errors=[],attempts=[];page.on('pageerror',error=>errors.push(error.message));
      await page.route('**/api/dispatch/session',async route=>{attempts.push(Date.now());if(attempts.length<3)await route.fulfill({status:503,contentType:'application/json',body:JSON.stringify({error:'Application Server temporarily unavailable'})});else await route.continue();});
      const before=requests.length;
      try{
        await page.goto(origin+'/?kind=dispatch');await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
        assert.equal(attempts.length,3);assert.ok(attempts[1]-attempts[0]>=800,'first server retry waits rather than looping');assert.ok(attempts[2]-attempts[1]>=1800,'continued failure increases the retry interval');
        assert.equal(requests.slice(before).filter(request=>request.path==='/api/state').length,1,'audio recovery does not poll configuration');
        assert.ok(await page.evaluate(()=>fixture.microphones.slice(0,-1).every(stream=>stream.getTracks().every(track=>track.readyState==='ended'))));
        assert.equal(await page.locator('#test-connect').isVisible(),false);assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type.startsWith('ptt_')).length),0);
        assert.deepEqual(errors,[]);assert.deepEqual(await page.evaluate(()=>fixture.failures),[]);
      }finally{await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
    });
    await t.test('an unanswered setup releases its resources and automatically retries',async()=>{
      const context=await browser.newContext({permissions:['microphone']});await context.addInitScript(()=>window.fixtureOptions={noAnswer:true});
      const page=await context.newPage(),errors=[];page.on('pageerror',error=>errors.push(error.message));
      try{
        await page.goto(origin+'/?kind=ptt');await page.waitForFunction(()=>fixture.messages.some(message=>message.type==='offer'));
        await page.evaluate(()=>fixture.firstMicrophone=fixture.microphone);
        await page.waitForFunction(()=>fixture.firstMicrophone.getTracks().every(track=>track.readyState==='ended'),null,{timeout:12000});
        assert.equal(await page.locator('#test-connect').isVisible(),false,'a negotiation timeout needs retry rather than a browser gesture');
        await page.evaluate(()=>fixture.noAnswer=false);await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
        assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type==='offer').length),2);
        assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type.startsWith('ptt_')).length),0);
        assert.deepEqual(errors,[]);assert.deepEqual(await page.evaluate(()=>fixture.failures),[]);
      }finally{await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
    });
    await t.test('PTT explicitly saves preferences and restores them without replaying actions',async()=>{
      const oldProfile=profile,oldReceive=profileReceive,oldFailure=profileFailure;
      const hidden=[{talkgroup_id:9009,volume:1.125,pan:2,muted:true},{talkgroup_id:9009,volume:-.125,pan:-2,muted:false}];
      profile={name:'Personal groups',preferred_position_id:'desk-1',zones:[{name:'Work',talkgroups:[1001,1002,9009]}],selected_talkgroup:1001,transmit_talkgroups:[1001,9009]};
      profileReceive=[{talkgroup_id:1001,volume:.375,pan:-1,muted:false},{talkgroup_id:1002,volume:.625,pan:0,muted:true},...hidden];
      const baseline={...structuredClone(profile),receive:structuredClone(profileReceive)};
      const context=await browser.newContext({permissions:['microphone']}),page=await context.newPage(),errors=[];page.on('pageerror',error=>errors.push(error.message));
      const before=requests.length;
      const idle=async()=>{
        await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');assert.equal(await page.locator('#test-transmit').textContent(),'Idle');
        assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type.startsWith('ptt_')).length),0,'Save/load never replays PTT');
        assert.equal(await page.locator('#test-connect').isVisible(),false);
      };
      try{
        await page.goto(origin+'/?kind=ptt');await page.waitForSelector('#listen-1001');await idle();
        assert.equal(await page.locator('#volume-1001').inputValue(),'37.5','load keeps non-five-percent-aligned volume');
        await page.locator('#test-group').fill('1002');await page.locator('#listen-1001').uncheck();await page.locator('#listen-1002').check();
        await page.locator('#volume-1001').evaluate(el=>{el.value='37.25';el.dispatchEvent(new Event('input',{bubbles:true}));});
        await page.locator('#pan-1001').selectOption('1');await page.locator('#pan-1002').selectOption('-1');
        await page.locator('#test-scan').check();await page.locator('#test-secure').check();
        assert.deepEqual(requests.slice(before).filter(value=>value.path.startsWith('/api/')&&value.path!=='/api/dispatch/session').map(value=>[value.path,value.method]),[['/api/state','GET']],'edits never autosave or preflight');
        await page.locator('#test-save-preferences').click();await page.waitForFunction(()=>document.getElementById('test-result').textContent==='Preferences saved.');
        const save=requests.slice(before).find(value=>value.path==='/api/profile');
        assert.equal(save.method,'PUT');assert.equal(save.csrf,'fixture-csrf');assert.equal(save.contentType,'application/json');
        const submitted=JSON.parse(save.body),{receive,...programming}=submitted,{receive:initialReceive,...initialProgramming}=baseline;
        assert.deepEqual(programming,{...initialProgramming,selected_talkgroup:1002},'preserve existing fields and add no scan, encryption, key or device fields');
        assert.equal(receive.length,initialReceive.length);
        assert.deepEqual(receive.find(value=>value.talkgroup_id===1001),{talkgroup_id:1001,volume:.3725,pan:1,muted:true});
        assert.deepEqual(receive.find(value=>value.talkgroup_id===1002),{talkgroup_id:1002,volume:.625,pan:-1,muted:false});
        assert.deepEqual(receive.filter(value=>value.talkgroup_id===9009),hidden,'unrendered rows retain order, duplicates and entered values');
        assert.deepEqual(profile,submitted);await idle();const lastSaved=structuredClone(profile);
        await page.reload();await page.waitForSelector('#listen-1001');
        assert.equal(await page.locator('#test-group').inputValue(),'1002');
        assert.equal(await page.locator('#volume-1001').inputValue(),'37.25');assert.equal(await page.locator('#volume-1002').inputValue(),'62.5');
        assert.equal(await page.locator('#listen-1001').isChecked(),false);assert.equal(await page.locator('#listen-1002').isChecked(),true);
        assert.equal(await page.locator('#pan-1001').inputValue(),'1');assert.equal(await page.locator('#pan-1002').inputValue(),'-1');
        assert.equal(await page.locator('#test-scan').isChecked(),false);assert.equal(await page.locator('#test-secure').isChecked(),false);await idle();
        profileFailure='System Administration is unavailable; preferences were not saved.';
        await page.locator('#test-group').fill('1001');await page.locator('#listen-1002').uncheck();
        await page.locator('#volume-1001').evaluate(el=>{el.value='12.125';el.dispatchEvent(new Event('input',{bubbles:true}));});await page.locator('#pan-1001').selectOption('0');
        await page.locator('#test-save-preferences').click();await page.waitForFunction(message=>document.getElementById('test-result').textContent===message,profileFailure);
        assert.equal(await page.locator('#test-group').inputValue(),'1001');assert.equal(await page.locator('#volume-1001').inputValue(),'12.125');
        assert.equal(await page.locator('#listen-1002').isChecked(),false);assert.equal(await page.locator('#pan-1001').inputValue(),'0');
        assert.deepEqual(profile,lastSaved,'failure leaves the retained profile unchanged');await idle();
        await page.reload();await page.waitForSelector('#listen-1001');
        assert.equal(await page.locator('#test-group').inputValue(),'1002');assert.equal(await page.locator('#volume-1001').inputValue(),'37.25');await idle();
        await page.goto(origin+'/?kind=dispatch');await page.waitForSelector('#listen-1001');
        assert.equal(await page.locator('#test-save-preferences').count(),0,'Dispatch has no new Save control');await idle();
        assert.deepEqual(requests.slice(before).filter(value=>value.path.startsWith('/api/')&&value.path!=='/api/dispatch/session').map(value=>[value.path,value.method]),[['/api/state','GET'],['/api/profile','PUT'],['/api/state','GET'],['/api/profile','PUT'],['/api/state','GET'],['/api/state','GET']],'only explicit Saves write; load never polls or preflights profiles');
        assert.deepEqual(errors,[]);
      }finally{await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();profile=oldProfile;profileReceive=oldReceive;profileFailure=oldFailure;}
    });
    await t.test('Testing pages show human units and percent controls without changing API values',async()=>{
      const oldSnapshot=controllerSnapshot,oldHistory=historyPage,oldReceive=profileReceive;
      controllerSnapshot={call_delivery:[],emergencies:[],patches:[],measurements:{id:'443450001',radio_id:10001,talkgroup_id:1001,channel_number:81,attempts:2,tx_frequency_hz:443450001,rx_frequency_hz:448450000,base_frequency_hz:443437500.25,uplink_offset_hz:5000000,channel_spacing_hz:12500,bandwidth_hz:12500,rx_offset_hz:.25,tx_offset_hz:-.125,lifetime_ms:1501,allocated_ms:1200,speech_ms:null,latency_ms:17.250004,jitter_ms:.0004,message:'Stored report: duration 1501 ms, RF 443450001 Hz'}};
      historyPage={calls:[{id:'call-1',radio_id:10001,lifetime_ms:1501,speech_ms:null,destinations:[{site_id:'site-1',talkgroup_id:1001,allocated_ms:1200,speech_ms:null}]}]};
      profileReceive=[{talkgroup_id:1001,volume:.25,muted:false,pan:0}];
      try{
        for(const kind of ['dispatch','ptt']){
          const context=await browser.newContext({permissions:['microphone']}),page=await context.newPage(),errors=[];page.on('pageerror',error=>errors.push(error.message));
          const before=requests.length;
          try{
            await page.goto(origin+'/?kind='+kind);await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
            assert.equal(await page.locator('#volume-1001').inputValue(),'25');assert.equal(await page.locator('#volume-1001').getAttribute('max'),'100');assert.equal(await page.locator('#volume-1001').getAttribute('step'),'any');assert.equal(await page.locator('output[for=volume-1001]').textContent(),'25%');
            for(const id of ['test-mic-level','test-rx-level']){assert.equal(await page.locator('#'+id).getAttribute('max'),'100');assert.ok((await page.locator('#'+id).locator('..').textContent()).includes('% full scale'));}
            await page.locator('#test-read-results').click();await page.waitForFunction(()=>document.getElementById('test-controller-results').textContent.includes('Transmit frequency (MHz)'));
            const shown=JSON.parse(await page.locator('#test-controller-results').textContent()).measurements;
            assert.deepEqual(shown,{id:'443450001',radio_id:10001,talkgroup_id:1001,channel_number:81,attempts:2,'Transmit frequency (MHz)':443.450001,'Receive frequency (MHz)':448.45,'Base frequency (MHz)':443.43750025,'Uplink offset (MHz)':5,'Channel spacing (kHz)':12.5,'Bandwidth (kHz)':12.5,'Receive calibration (Hz)':.25,'Transmit calibration (Hz)':-.125,'Call lifetime (seconds)':1.501,'Allocated time (seconds)':1.2,'Speech time (seconds)':null,'Latency (ms)':17.250004,'Jitter (ms)':.0004,message:'Stored report: duration 1501 ms, RF 443450001 Hz'});
            if(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE){await fs.mkdir(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE,{recursive:true});await page.locator('#test-controller-results').screenshot({path:path.join(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE,kind+'-measurements.png')});}
            await page.locator('#test-history').click();await page.waitForFunction(()=>document.getElementById('test-controller-results').textContent.includes('call-1'));
            const history=JSON.parse(await page.locator('#test-controller-results').textContent()).calls[0];assert.equal(history['Call lifetime (seconds)'],1.501);assert.equal(history['Speech time (seconds)'],null);assert.equal(history.destinations[0]['Allocated time (seconds)'],1.2);
            assert.equal(controllerSnapshot.measurements.tx_frequency_hz,443450001);assert.equal(historyPage.calls[0].lifetime_ms,1501);assert.equal(profileReceive[0].volume,.25,'presentation does not mutate received API/profile values');
            await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
            await page.evaluate(()=>fixture.call(1001,true));await page.waitForFunction(()=>Number(document.getElementById('test-rx-level').value)>1);
            const quarter=await page.evaluate(()=>fixture.outputLevels());assert.ok(quarter.every(value=>value>.01&&value<.04),'25 percent UI retains normalized quarter gain on actual audio');
            await page.locator('#volume-1001').evaluate(el=>{el.value='50';el.dispatchEvent(new Event('input',{bubbles:true}));});
            assert.equal(await page.locator('output[for=volume-1001]').textContent(),'50%');
            const half=await page.evaluate(()=>fixture.outputLevels());assert.ok(half.every((value,index)=>value/quarter[index]>1.8&&value/quarter[index]<2.2),'50 percent changes audio gain to one half');
            await page.locator('#test-refresh').click();await page.waitForFunction(()=>document.getElementById('volume-1001').value==='50');assert.equal(await page.locator('output[for=volume-1001]').textContent(),'50%');assert.equal(profileReceive[0].volume,.25,'explicit account reload keeps server profile gain unchanged');
            await page.evaluate(()=>{fixture.realGetStats=RTCPeerConnection.prototype.getStats;RTCPeerConnection.prototype.getStats=async()=>new Map([['in',{id:'unit-audio',type:'inbound-rtp',ssrc:443450000,packetsReceived:81,packetsLost:-1,bytesReceived:900,jitter:.000000125,jitterBufferDelay:2.5,jitterBufferTargetDelay:3.5,jitterBufferMinimumDelay:1.5,jitterBufferEmittedCount:1000}],['out',{id:'unit-mic',type:'outbound-rtp',ssrc:10002,packetsSent:0,bytesSent:0}],['remote',{id:'not-selected',type:'remote-inbound-rtp',roundTripTime:.05}]]);});
            await page.locator('#test-stats').click();await page.waitForFunction(()=>document.getElementById('test-controller-results').textContent.includes('unit-audio'));
            const stats=JSON.parse(await page.locator('#test-controller-results').textContent());assert.deepEqual(stats,[{id:'unit-audio',type:'inbound-rtp',ssrc:443450000,packetsReceived:81,packetsLost:-1,bytesReceived:900,'Jitter (ms)':.000125,'Cumulative jitter-buffer delay (ms)':2500,'Cumulative target jitter-buffer delay (ms)':3500,'Cumulative minimum jitter-buffer delay (ms)':1500,jitterBufferEmittedCount:1000},{id:'unit-mic',type:'outbound-rtp',ssrc:10002,packetsSent:0,bytesSent:0}]);
            assert.ok(!JSON.stringify(stats).includes('Round-trip'),'no absent round-trip measurement is invented');assert.ok(!JSON.stringify(stats).includes('bitrate'),'byte counts do not invent a bitrate');
            await page.evaluate(()=>RTCPeerConnection.prototype.getStats=fixture.realGetStats);
            const api=requests.slice(before).filter(value=>value.path.startsWith('/api/'));
            assert.deepEqual(api.map(value=>[value.path,value.method]),[['/api/state','GET'],['/api/dispatch/session','POST'],['/api/operator/state','GET'],['/api/history/calls','GET'],['/api/state','GET']],'units create no extra queries or mutation');
            assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type==='ptt_start').length),0,'display/volume/statistics never replay PTT');assert.deepEqual(errors,[]);assert.deepEqual(await page.evaluate(()=>fixture.failures),[]);
            if(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE)await page.screenshot({path:path.join(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE,kind+'-stats-percent.png'),fullPage:true});
          }finally{await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
        }
      }finally{controllerSnapshot=oldSnapshot;historyPage=oldHistory;profileReceive=oldReceive;}
    });
    await t.test('Dispatch loads patch fields only from explicitly refreshed controller state',async()=>{
      controllerSnapshot={call_delivery:[],emergencies:[],patches:[
        {id:'patch-one',name:'Existing <&> patch',talkgroups:[1002,1001],algorithm_id:132,key_id:0,scope_id:'zone-one',active:true,can_edit:false},
        {id:'patch-ended',name:'Ended patch',talkgroups:[1001,1003],algorithm_id:0,key_id:0,scope_id:'other-site',active:false,state:'ended',can_edit:false}
      ]};
      const context=await browser.newContext({permissions:['microphone']});const page=await context.newPage();const errors=[];page.on('pageerror',error=>errors.push(error.message));
      const before=requests.length, reads=()=>requests.slice(before).filter(r=>r.path==='/api/operator/state').length;
      const fields=()=>page.evaluate(()=>{const f=document.getElementById('test-patch').elements;return {id:document.getElementById('test-patch-id').value,name:f.name.value,algorithm:f.algorithm_id.value,key:f.key_id.value,members:f.talkgroups.value,previous:document.getElementById('test-patch-previous').value,next:document.getElementById('test-patch-members').value};});
      const refresh=async count=>{await page.locator('#test-read-results').click();await page.waitForFunction(count=>document.querySelectorAll('#test-patch-select option').length===count,count);};
      try{
        await page.goto(origin+'/?kind=dispatch');await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
        assert.equal(reads(),0,'opening the page does not query patch state');assert.equal(await page.locator('#test-patch-select option').count(),1);
        await refresh(3);assert.equal(reads(),1);
        assert.deepEqual(await page.locator('#test-patch-select option').allTextContents(),['Choose a patch','Existing <&> patch (patch-one)','Ended patch (patch-ended)'],'all returned patches remain selectable without browser capability or owner filtering');
        await page.locator('#test-patch-select').selectOption('patch-ended');
        assert.deepEqual(await fields(),{id:'patch-ended',name:'Ended patch',algorithm:'0',key:'0',members:'1001,1003',previous:'1001,1003',next:'1001,1003'});
        await page.locator('#test-patch-select').selectOption('patch-one');
        assert.deepEqual(await fields(),{id:'patch-one',name:'Existing <&> patch',algorithm:'132',key:'0',members:'1002,1001',previous:'1002,1001',next:'1002,1001'});
        assert.equal(reads(),1,'selection uses the same returned snapshot');assert.equal(await page.locator('#test-patch-edit').isEnabled(),true);
        await page.locator('#test-patch-members').fill('1001,1003');
        controllerSnapshot.patches[0]={...controllerSnapshot.patches[0],talkgroups:[1001,1004]};
        patchFailure='this patch changed; refresh it before editing';
        const response=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/operator/patch');await page.locator('#test-patch-edit').click();assert.equal((await response).status(),409);
        await page.waitForFunction(()=>document.getElementById('test-result').textContent==='this patch changed; refresh it before editing');
        const post=requests.findLast(r=>r.path==='/api/operator/patch');
        assert.deepEqual(JSON.parse(post.body),{action:'update',id:'patch-one',patch:{name:'Existing <&> patch',algorithm_id:132,key_id:0,talkgroups:[1001,1003],previous_talkgroups:[1002,1001]}});
        assert.equal(post.csrf,'fixture-csrf');assert.equal(reads(),1,'submission has no state preflight or automatic retry');
        assert.equal((await fields()).next,'1001,1003','an actual controller error preserves the entered members');
        await refresh(3);await page.waitForFunction(()=>document.getElementById('test-controller-results').textContent.includes('1004'));
        assert.equal(reads(),2);assert.equal((await fields()).next,'1001,1003','refreshing the list does not replace entered fields');
        await page.locator('#test-patch-select').selectOption('patch-one');
        assert.deepEqual(await fields(),{id:'patch-one',name:'Existing <&> patch',algorithm:'132',key:'0',members:'1001,1004',previous:'1001,1004',next:'1001,1004'});
        if(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE){await fs.mkdir(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE,{recursive:true});await page.screenshot({path:path.join(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE,'dispatch-patch-membership.png'),fullPage:true});}
        controllerSnapshot={call_delivery:[],emergencies:[],patches:[]};await refresh(1);assert.equal(reads(),3);assert.equal((await fields()).previous,'1001,1004');
        await page.waitForTimeout(300);assert.equal(reads(),3,'there is no patch-state polling');
        assert.equal(requests.slice(before).filter(r=>r.method==='POST'&&r.path!=='/api/dispatch/session').length,1,'refresh and selection never issue operations');
        assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type.startsWith('ptt_')).length),0);assert.equal(requests.slice(before).filter(r=>r.path.startsWith('/api/dispatch/')&&r.path!=='/api/dispatch/session').length,0);
        assert.equal(await page.locator('link[rel=stylesheet]').count(),0);assert.deepEqual(errors,[]);
      }finally{patchFailure='';await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
    });
    for(const kind of ['dispatch','ptt']) await t.test(kind+' applies local audio devices without replaying PTT',async()=>{
      const context=await browser.newContext({permissions:['microphone']});const page=await context.newPage();const errors=[];page.on('pageerror',error=>errors.push(error.message));
      try {
        await page.goto(origin+'/?kind='+kind);await page.waitForFunction(()=>document.getElementById('test-identity').textContent.includes('10002'));
        await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');assert.equal(await page.evaluate(()=>fixture.microphoneOptions.length),1,'opening the app acquires its microphone once');
        await page.locator('#test-devices').click();await page.waitForFunction(()=>document.getElementById('test-devices-result').textContent.includes('Device list loaded'));
        assert.equal(await page.evaluate(()=>fixture.microphoneOptions.length),1,'listing devices reuses the connected microphone');
        const devices=await page.evaluate(async()=> (await navigator.mediaDevices.enumerateDevices()).map(d=>({kind:d.kind,id:d.deviceId,label:d.label})));
        const inputs=devices.filter(d=>d.kind==='audioinput'&&d.id!=='default'),outputs=devices.filter(d=>d.kind==='audiooutput'&&d.id!=='default');assert.equal(inputs.length,2);assert.equal(outputs.length,2);
        await page.locator('#test-microphone').selectOption(inputs[0].id);await page.locator('#test-speaker').selectOption(outputs[0].id);
        assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type.startsWith('ptt_')).length),0,'device choices do not operate the radio');
        await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');
        await page.waitForFunction(id=>fixture.microphone.getAudioTracks()[0].getSettings().deviceId===id,inputs[0].id);assert.equal(await page.evaluate(()=>fixture.microphone.getAudioTracks()[0].getSettings().deviceId),inputs[0].id,'the actual capture uses the selected microphone');
        assert.equal(await page.locator('audio[data-output=receive]').evaluate(el=>el.sinkId),outputs[0].id,'the actual output element uses the selected native sink');
        await page.evaluate(()=>fixture.call(1001,true));await page.waitForFunction(()=>Number(document.getElementById('test-rx-level').value)>3);
        const both=await page.evaluate(()=>fixture.outputLevels());assert.ok(both.every(v=>v>.02),'actual receive mix reaches both output channels');
        await page.locator('#pan-1001').selectOption('-1');const left=await page.evaluate(()=>fixture.outputLevels());assert.ok(left[0]>.02&&left[1]<.005,'stereo placement survives selected output routing');
        assert.equal(await page.evaluate(()=>[...document.querySelectorAll('audio')].filter(el=>!el.muted&&!el.paused).length),1,'one audible path for live receive');
        assert.equal(await page.evaluate(()=>fixture.connections.filter(c=>c.source.context===fixture.audioContexts[0]&&c.destination===fixture.audioContexts[0].destination).length),0,'receive mix has no parallel default-speaker path');
        await page.locator('#test-ptt').hover();await page.mouse.down();const first=await page.evaluate(()=>fixture.messages.findLast(m=>m.type==='ptt_start'));await page.evaluate(id=>fixture.grant(id),first.request_id);await page.waitForFunction(()=>fixture.micLevel>.01);
        await page.locator('#test-microphone').selectOption(inputs[1].id);await page.mouse.up();await page.waitForFunction(id=>fixture.microphone.getAudioTracks()[0].getSettings().deviceId===id,inputs[1].id);
        await page.waitForFunction(()=>fixture.micLevel<.005);assert.equal(await page.locator('#test-transmit').textContent(),'Idle');
        await page.evaluate(id=>fixture.grant(id),first.request_id);await page.waitForTimeout(100);assert.ok(await page.evaluate(()=>fixture.micLevel)<.005,'late grant cannot replay PTT after changing microphone');
        const starts=await page.evaluate(()=>fixture.messages.filter(m=>m.type==='ptt_start').length);
        await page.locator('#test-record').click();await page.waitForTimeout(1100);await page.locator('#test-record-stop').click();await page.waitForFunction(id=>document.querySelector('#test-recording audio')?.sinkId===id,outputs[0].id);
        assert.equal(await page.evaluate(()=>[...document.querySelectorAll('audio')].filter(el=>!el.muted&&!el.paused).length),1,'recording does not create another audible receive path');
        await page.evaluate(()=>fixture.playDenied=true);await page.locator('#test-speaker-resume').click();await page.waitForFunction(()=>document.getElementById('test-devices-result').textContent.includes('Playback permission denied'));assert.ok(await page.locator('audio[data-output=receive]').evaluate(el=>el.muted&&el.paused),'blocked playback stays silent');await page.evaluate(()=>fixture.playDenied=false);await page.locator('#test-speaker-resume').click();await page.waitForFunction(()=>!document.querySelector('audio[data-output=receive]').muted);
        await page.evaluate(()=>fixture.speakerError=true);await page.locator('#test-speaker').selectOption(outputs[1].id);await page.waitForFunction(()=>document.getElementById('test-devices-result').textContent.includes('Speaker unavailable'));
        await page.locator('#test-connect').waitFor({state:'visible'});assert.equal(await page.locator('audio[data-output=receive]').count(),0,'denied output ends the live audio session');assert.equal(await page.locator('#test-recording audio').getAttribute('src'),null,'denied output disconnects recording playback');
        assert.equal(await page.locator('#test-speaker').inputValue(),outputs[1].id,'failure preserves the selected device');
        await page.evaluate(()=>fixture.speakerError=false);await page.locator('#test-connect').click();await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');await page.waitForFunction(id=>document.querySelector('audio[data-output=receive]').sinkId===id&&!document.querySelector('audio[data-output=receive]').muted,outputs[1].id);
        await page.evaluate(id=>{fixture.removedDevices=[id];navigator.mediaDevices.dispatchEvent(new Event('devicechange'));},outputs[1].id);await page.waitForFunction(()=>document.getElementById('test-devices-result').textContent.includes('speaker is no longer available'));
        assert.ok(await page.locator('audio[data-output=receive]').evaluate(el=>el.muted&&el.paused));assert.equal(await page.locator('#test-speaker').inputValue(),outputs[1].id,'device removal never selects another speaker');
        await page.locator('#test-speaker').selectOption('');await page.waitForFunction(()=>document.querySelector('audio[data-output=receive]').sinkId===''&&!document.querySelector('audio[data-output=receive]').muted);
        await page.evaluate(()=>{fixture.removedDevices=[];HTMLMediaElement.prototype.setSinkId=undefined;});await page.locator('#test-speaker').selectOption(outputs[0].id);await page.waitForFunction(()=>document.getElementById('test-devices-result').textContent.includes('browser cannot select a speaker'));
        assert.equal(await page.locator('#test-speaker option').count(),4,'unsupported API does not hide declared device choices');
        await page.locator('#test-speaker').selectOption('');await page.waitForFunction(()=>!document.querySelector('audio[data-output=receive]').muted);
        await page.evaluate(()=>navigator.mediaDevices.selectAudioOutput=undefined);await page.locator('#test-speaker-choose').click();assert.ok((await page.locator('#test-devices-result').textContent()).includes('no speaker chooser'));
        await page.evaluate(()=>HTMLMediaElement.prototype.setSinkId=fixture.setSinkId);await page.locator('#test-speaker').selectOption(outputs[0].id);await page.waitForFunction(id=>document.querySelector('audio[data-output=receive]').sinkId===id,outputs[0].id);
        assert.equal(await page.evaluate(()=>fixture.messages.filter(m=>m.type==='ptt_start').length),starts,'speaker/error/recovery controls never start PTT');
        // A real recorded blob keeps its selected output and device-removal handling.
        await page.waitForFunction(id=>{const el=document.querySelector('#test-recording audio');return el.sinkId===id&&!el.muted&&!!el.getAttribute('src');},outputs[0].id);
        const sessions=requests.filter(r=>r.path==='/api/dispatch/session').length;
        const captures=await page.evaluate(()=>fixture.microphoneOptions.length);
        await page.locator('#test-recording audio').evaluate(async el=>{el.loop=true;await el.play();});
        assert.ok(await page.locator('#test-recording audio').evaluate(el=>!el.paused&&!el.muted&&el.currentSrc.startsWith('blob:')));
        await page.evaluate(id=>{fixture.removedDevices=[id];navigator.mediaDevices.dispatchEvent(new Event('devicechange'));},outputs[0].id);
        await page.waitForFunction(()=>document.getElementById('test-devices-result').textContent.includes('speaker is no longer available'),null,{timeout:2500}).catch(async error=>{t.diagnostic('recording after removal: '+JSON.stringify(await page.locator('#test-recording audio').evaluate(el=>({paused:el.paused,muted:el.muted,hasSource:!!el.getAttribute('src'),message:document.getElementById('test-devices-result').textContent}))));throw error;});
        assert.ok(await page.locator('#test-recording audio').evaluate(el=>el.paused&&el.muted));
        assert.equal(await page.locator('#test-recording audio').getAttribute('src'),null,'recording is detached on output removal');
        assert.equal(await page.locator('#test-speaker').inputValue(),outputs[0].id,'recorded playback never falls back to another speaker');
        assert.equal(requests.filter(r=>r.path==='/api/dispatch/session').length,sessions);assert.equal(await page.evaluate(()=>fixture.microphoneOptions.length),captures);assert.equal(await page.evaluate(()=>fixture.messages.filter(m=>m.type==='ptt_start').length),starts);
        await page.locator('#test-speaker').selectOption('');await page.locator('#test-speaker-resume').click();
        await page.waitForFunction(()=>{const el=document.querySelector('#test-recording audio');return el.sinkId===''&&!el.muted&&!!el.getAttribute('src');});
        assert.ok(await page.locator('#test-recording audio').evaluate(el=>el.paused),'output recovery does not replay a recording');
        await page.locator('#test-recording audio').evaluate(el=>el.play());
        assert.ok(await page.locator('#test-recording audio').evaluate(el=>!el.paused&&!el.muted));
        assert.equal(requests.filter(r=>r.path==='/api/dispatch/session').length,sessions);assert.equal(await page.evaluate(()=>fixture.messages.filter(m=>m.type==='ptt_start').length),starts);
        await page.locator('#test-recording audio').evaluate(el=>el.pause());await page.evaluate(()=>fixture.removedDevices=[]);await page.locator('#test-speaker').selectOption(outputs[0].id);
        await page.evaluate(()=>fixture.finish());
        await page.reload();await page.waitForFunction(()=>document.getElementById('test-identity').textContent.includes('10002'));
        assert.equal(await page.locator('#test-microphone').inputValue(),inputs[1].id);assert.equal(await page.locator('#test-speaker').inputValue(),outputs[0].id);
        await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');assert.equal(await page.evaluate(()=>fixture.microphoneOptions.length),1,'opening reconnects using the saved device preferences');assert.equal(await page.evaluate(()=>fixture.messages.filter(message=>message.type.startsWith('ptt_')).length),0,'restored preferences never replay PTT');
        await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');assert.equal(await page.evaluate(()=>fixture.microphone.getAudioTracks()[0].getSettings().deviceId),inputs[1].id);assert.equal(await page.locator('audio[data-output=receive]').evaluate(el=>el.sinkId),outputs[0].id);assert.equal(await page.locator('#test-transmit').textContent(),'Idle');
        await page.evaluate(id=>{fixture.removedDevices=[id];navigator.mediaDevices.dispatchEvent(new Event('devicechange'));},inputs[1].id);await page.waitForFunction(()=>document.getElementById('test-devices-result').textContent.toLowerCase().includes('microphone is no longer available'));assert.ok(await page.evaluate(()=>fixture.microphone.getTracks().every(track=>track.readyState==='ended')));assert.equal(await page.locator('#test-microphone').inputValue(),inputs[1].id);await page.evaluate(()=>fixture.removedDevices=[]);
        await page.evaluate(()=>fixture.microphoneError=true);await page.locator('#test-microphone').selectOption(inputs[0].id);await page.locator('#test-connect').waitFor({state:'visible'});assert.ok((await page.locator('#test-devices-result').textContent()).includes('Microphone permission denied'));
        await page.locator('#test-ptt').hover();await page.mouse.down();await page.mouse.up();assert.equal(await page.evaluate(()=>fixture.messages.filter(m=>m.type==='ptt_start').length),0,'failed capture cannot transmit the previous microphone');
        await page.evaluate(()=>fixture.microphoneError=false);await page.locator('#test-microphone').selectOption('');await page.locator('#test-connect').click();await page.waitForFunction(()=>document.getElementById('test-connection').textContent==='connected');await page.waitForFunction(()=>fixture.microphone.getAudioTracks()[0].readyState==='live');assert.equal(await page.evaluate(()=>Object.hasOwn(fixture.microphoneOptions.at(-1).audio,'deviceId')),false,'Browser default requests the browser-selected input');
        if(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE){await fs.mkdir(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE,{recursive:true});await page.screenshot({path:path.join(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE,kind+'-audio-devices.png')});await fs.writeFile(path.join(process.env.TRUNKSYSTEM_AUDIO_EVIDENCE,kind+'-browser.json'),JSON.stringify({version:browser.version(),devices,requests:requests.filter(r=>r.path==='/api/dispatch/session').length,failures:await page.evaluate(()=>fixture.failures)},null,2)+'\n');}
        await page.evaluate(()=>window.dispatchEvent(new Event('pagehide')));assert.ok(await page.evaluate(()=>fixture.microphones.every(stream=>stream.getTracks().every(track=>track.readyState==='ended'))));assert.deepEqual(errors,[]);assert.deepEqual(await page.evaluate(()=>fixture.failures),[]);
      }finally{await page.evaluate(()=>fixture.finish()).catch(()=>{});await context.close();}
    });
  } finally {await browser.close();await new Promise(resolve=>server.close(resolve));await fs.rm(directory,{recursive:true,force:true});}
});
