"use strict";
// Explicit same-service directory reads only. Selection edits the form; Save owns persistence.
if(document.body.classList.contains("administration")){
 const dialog=document.createElement('dialog');dialog.className='ts-chooser';dialog.setAttribute('aria-labelledby','chooser-title');
 dialog.innerHTML=`<div class=ts-itemline><h2 id=chooser-title>Choose entries</h2><button type=button data-chooser-close aria-label="Close chooser"><span aria-hidden=true>×</span></button></div><form method=dialog data-chooser-search><label class=ts-field>Search by name or identity<input type=search name=q autocomplete=off></label><button type=submit>Search</button></form><p role=status data-chooser-status></p><div class=ts-chooser-table><table><thead><tr><th>Select</th><th>Identity</th><th>Name</th></tr></thead><tbody data-chooser-results></tbody></table></div><div class=ts-toolbar><button type=button data-chooser-prev>Previous</button><span data-chooser-page></span><button type=button data-chooser-next>Next</button></div><div class=ts-actions><button type=button data-chooser-close>Cancel</button><button type=button class=ts-primary data-chooser-apply>Use selection</button></div>`;
 const search=dialog.querySelector('[data-chooser-search]'),results=dialog.querySelector('[data-chooser-results]'),status=dialog.querySelector('[data-chooser-status]');
 const split=(value,kind)=>value.split(['radios','talkgroups'].includes(kind)?/[,\s]+/:/[,\n\r]+/).map(id=>id.trim()).filter(Boolean);
 let state=null,request=null,generation=0;
 function selected(id){return state.added.has(id)||(state.initial.has(id)&&!state.removed.has(id));}
 function selectionCount(){return new Set([...state.original.filter(id=>!state.removed.has(id)),...state.added]).size;}
 function showSelection(){dialog.querySelector('[data-chooser-apply]').textContent=state.multiple?'Use selection ('+selectionCount()+')':'Use selection';}
 dialog.querySelector('[data-chooser-next]').textContent='Next';
 async function load(offset){
  request?.abort();request=new AbortController();const current=++generation;
  const query=new URLSearchParams({kind:state.kind,q:search.elements.q.value,offset:String(offset)});
  status.textContent='Loading…';
  try{
   const response=await fetch('/chooser?'+query,{credentials:'same-origin',signal:request.signal,headers:{Accept:'application/json'}});
   if(!response.ok)throw new Error('Could not load entries ('+response.status+'). Search again to retry.');
   const page=await response.json();if(current!==generation||!dialog.open)return;
   for(const id of Object.keys(state.names))if(!selected(id))delete state.names[id];
   state.offset=page.offset;state.limit=page.limit;state.total=page.total;results.replaceChildren();
   for(const item of page.items){
    if(selected(item.ID))state.names[item.ID]=item.Name;
    const row=document.createElement('tr'),cell=document.createElement('td'),input=document.createElement('input');
    input.type=state.multiple?'checkbox':'radio';input.name='chooser-selection';input.value=item.ID;input.checked=selected(item.ID);input.setAttribute('aria-label',item.Name+' · '+item.ID);
    input.addEventListener('change',()=>{
     if(!state.multiple){state.added.clear();state.removed=new Set(state.original);}
     if(input.checked){state.removed.delete(item.ID);if(!state.initial.has(item.ID)||!state.multiple)state.added.add(item.ID);}
     else{state.added.delete(item.ID);if(state.initial.has(item.ID))state.removed.add(item.ID);}
     if(input.checked)state.names[item.ID]=item.Name;else delete state.names[item.ID];
     showSelection();
    });
    cell.append(input);row.append(cell);
    for(const text of [item.ID,item.Name]){const td=document.createElement('td');td.textContent=text;row.append(td);}
    results.append(row);
   }
   if(!page.items.length){const row=document.createElement('tr'),cell=document.createElement('td');cell.colSpan=3;cell.textContent='No matching entries.';row.append(cell);results.append(row);}
   dialog.querySelector('[data-chooser-prev]').disabled=page.offset===0;
   dialog.querySelector('[data-chooser-next]').disabled=page.offset+page.limit>=page.total;
   dialog.querySelector('[data-chooser-page]').textContent=page.total?(page.offset+1)+'–'+Math.min(page.offset+page.limit,page.total)+' of '+page.total:'0 entries';
   status.textContent=page.notice||'';showSelection();
  }catch(error){if(current===generation&&dialog.open&&error.name!=='AbortError')status.textContent=error.message;}
 }
 document.addEventListener('click',event=>{
  const trigger=event.target.closest('[data-chooser-kind]');if(!trigger)return;
  const form=trigger.closest('form'),input=form?.elements.namedItem(trigger.dataset.chooserTarget)||document.getElementById(trigger.dataset.chooserTarget);if(!input)return;
  if(!dialog.isConnected)document.querySelector('.ts-admin').append(dialog);
  const original=trigger.dataset.chooserKind==='keys'?[form.elements.algorithm_id.value+'/'+input.value]:(trigger.dataset.chooserMultiple==='true'?split(input.value,trigger.dataset.chooserKind):[input.value].filter(Boolean));
  state={trigger,input,navigate:trigger.hasAttribute('data-chooser-navigate'),edit:trigger.dataset.chooserEdit,kind:trigger.dataset.chooserKind,multiple:trigger.dataset.chooserMultiple==='true',original,initial:new Set(original),added:new Set(),removed:new Set(),names:JSON.parse(input.dataset.choiceLabels||'{}'),offset:0};
  dialog.querySelector('#chooser-title').textContent='Choose '+({bands:'Band Plan',usb:'USB modem',users:'login',instances:'administration instance'}[state.kind]||state.kind);
  dialog.querySelector('thead th:nth-child(2)').textContent=state.kind==='keys'?'Algorithm / Key ID':'Identity';
  search.reset();search.querySelector('button').textContent=state.kind==='usb'?'Refresh':'Search';results.replaceChildren();dialog.querySelector('[data-chooser-page]').textContent='';dialog.showModal();search.elements.q.focus();showSelection();load(0);
 });
 search.addEventListener('submit',event=>{event.preventDefault();load(0);});
 dialog.querySelector('[data-chooser-prev]').addEventListener('click',()=>load(Math.max(0,state.offset-state.limit)));
 dialog.querySelector('[data-chooser-next]').addEventListener('click',()=>load(state.offset+state.limit));
 for(const button of dialog.querySelectorAll('[data-chooser-close]'))button.addEventListener('click',()=>dialog.close());
 dialog.addEventListener('close',()=>{request?.abort();generation++;state?.trigger.focus();state=null;});
 dialog.querySelector('[data-chooser-apply]').addEventListener('click',()=>{
  if(state.added.size||state.removed.size){
   const values=[...state.original.filter(id=>!state.removed.has(id)),...state.added].filter((id,i,all)=>state.multiple||i===all.length-1);
   const value=state.multiple?values.join(', '):(values.at(-1)||'');
   if(state.kind==='keys'){const [algorithm,key]=value.split('/');const algorithmInput=state.input.form.elements.algorithm_id;algorithmInput.value=algorithm;state.input.value=key;algorithmInput.dispatchEvent(new Event('input',{bubbles:true}));}else state.input.value=value;
   const names={};for(const id of values)if(state.names[id])names[id]=state.names[id];state.input.dataset.choiceLabels=JSON.stringify(names);
   if(state.navigate||state.edit){const link=document.createElement('a');link.href=state.edit?state.edit+'?'+new URLSearchParams({identity:state.input.value}):state.input.value;document.body.append(link);dialog.close();link.click();link.remove();return;}
   state.input.dispatchEvent(new Event('input',{bubbles:true}));
   state.input.dispatchEvent(new Event('change',{bubbles:true}));
  }
  dialog.close();
 });
}

// Preview only: the Site backend independently calculates the operating frequencies.
const bandOutputs=document.querySelectorAll('[data-band-frequency]');
if(bandOutputs.length){
 const form=bandOutputs[0].closest('form'),identity=form.elements.identifier,channel=form.elements.channel_no,reference=form.querySelector('[data-band-reference]'),result=form.querySelector('[data-band-result]');
 let plan=reference?{ID:reference.dataset.identifier,BaseHz:reference.dataset.base,SpacingHz:reference.dataset.spacing,OffsetHz:reference.dataset.offset}:null,lookup=null,sequence=0;
 function calculate(){
  let values=null;
  try{if(plan&&plan.ID===identity.value&&channel.value!==''){const tx=BigInt(plan.BaseHz)+BigInt(channel.value)*BigInt(plan.SpacingHz);values={tx_frequency_hz:tx,rx_frequency_hz:tx+BigInt(plan.OffsetHz)};}}catch(_){}
  for(const output of bandOutputs){
   if(!values){output.textContent='Unavailable';continue;}
   const n=values[output.dataset.bandFrequency],abs=n<0n?-n:n,raw=abs.toString().padStart(7,'0');
   output.textContent=(n<0n?'-':'')+raw.slice(0,-6)+'.'+raw.slice(-6)+' MHz';
  }
 }
 calculate();
 form.addEventListener('input',event=>{if(event.target.closest('[data-field=channel_no]'))calculate();if(event.target.closest('[data-field=identifier]')){plan=null;calculate();}});
 form.addEventListener('change',async event=>{
  if(!event.target.closest('[data-field=identifier]'))return;
  lookup?.abort();lookup=new AbortController();const current=++sequence,id=identity.value;result.textContent='Reading Band Plan…';
  try{const response=await fetch('/chooser?'+new URLSearchParams({kind:'bands',identity:id}),{credentials:'same-origin',signal:lookup.signal});if(!response.ok)throw new Error('Band Plan could not be read.');const page=await response.json();if(current!==sequence||identity.value!==id)return;plan=page.items[0]||null;result.textContent=plan?'':'No matching Band Plan in the received Zone settings.';calculate();}
  catch(error){if(current===sequence&&error.name!=='AbortError')result.textContent=error.message;}
 });
 // Chooser application is an explicit edit, with the same reference read as manual entry.

}

// Group names are suggestions only; typing a new name never needs permission.
for(const input of document.querySelectorAll('[data-record-combo]')){
 let request=null,timer=null,generation=0;
 async function load(){
  request?.abort();request=new AbortController();const current=++generation,value=input.value;
  try{
   const response=await fetch('/chooser?'+new URLSearchParams({kind:input.dataset.recordCombo,q:value}),{credentials:'same-origin',signal:request.signal});
   if(!response.ok)throw new Error('Could not read group choices.');
   const page=await response.json();if(current!==generation||input.value!==value)return;
   input.list.replaceChildren();for(const item of page.items){const option=document.createElement('option');option.value=item.ID;input.list.append(option);}
  }catch(error){if(error.name!=='AbortError'&&current===generation)input.setAttribute('title',error.message);}
 }
 input.addEventListener('focus',load);
 input.addEventListener('input',()=>{request?.abort();generation++;clearTimeout(timer);timer=setTimeout(load,200);});
}

const bulkSelection=document.querySelector('[data-bulk-selection]');
if(bulkSelection){
 const split=value=>value.split(/[,\s]+/).filter(Boolean),boxes=[...document.querySelectorAll('[data-bulk-record]')],page=document.querySelector('[data-bulk-page]');
 function display(){const chosen=new Set(split(bulkSelection.value));const count=document.querySelector("[data-bulk-count]");if(count)count.textContent=chosen.size+" selected";for(const box of boxes)box.checked=chosen.has(box.value);if(page){page.checked=boxes.length>0&&boxes.every(b=>b.checked);page.indeterminate=boxes.some(b=>b.checked)&&!page.checked;}}
 function update(box,selected){const ids=split(bulkSelection.value);bulkSelection.value=(selected?[...new Set([...ids,box.value])]:ids.filter(id=>id!==box.value)).join(', ');}
 for(const box of boxes)box.addEventListener('change',()=>{update(box,box.checked);display();});
 page?.addEventListener('change',()=>{for(const box of boxes)update(box,page.checked);display();});
 bulkSelection.addEventListener('input',display);display();
}
