const inboundEditor={id:'',dirty:false,busy:false,fixtures:[],exchanges:[],preview:null,initialized:false};
const ir=id=>document.getElementById(`ir${id}`);
const qaRequest=(...args)=>qaIsOutbound()?api(...args):request(...args);
function markInboundDirty(){inboundEditor.dirty=true;ir('Dirty').textContent='Unsaved changes';inboundEditor.preview=null}
function inboundMessage(message,error=false){ir('Message').textContent=message;ir('Message').style.color=error?'var(--red)':'var(--muted)'}
function inboundEndpoint(module,role){return (state?.endpoints||[]).find(e=>(qaIsOutbound()||e.platform==='remote')&&e.identifier===module&&(!role||e.role===role))?.url||''}
function syncInboundRequestOptions(){
  const select=ir('Endpoint');
  if(select&&document.activeElement!==select){
    const previous=select.value;
    select.replaceChildren(new Option('Select to fill URL, or type your own below',''));
    (state?.endpoints||[]).filter(e=>qaIsOutbound()||e.platform==='remote').forEach(e=>select.add(new Option(`${e.identifier} · ${e.role} · ${e.url}`,e.url)));
    select.value=previous;
  }
  if(!['Location','Token','Session'].some(field=>document.activeElement===document.getElementById(`rp${field}`)))refreshQAPrefill('rp');
}
async function openInboundRequests(template){
  selectWorkspaceView('requests');
  syncInboundRequestOptions();
  if(!inboundEditor.initialized){inboundEditor.initialized=true;ir('Name').value=`${qaLane()} custom request`}
  if(template){ir('Template').value=template;loadInboundTemplate()}
  try{const data=await qaRequest('/api/requests/state');inboundEditor.fixtures=(data.fixtures||[]).filter(f=>f.lane===qaLane());inboundEditor.exchanges=data.fixture_exchanges||[];const saved=ir('Saved'),chosen=saved.value;saved.replaceChildren(new Option('Select a saved request',''));inboundEditor.fixtures.forEach(f=>saved.add(new Option(`${f.name} · ${f.method}`,f.id)));saved.value=chosen}catch(error){inboundMessage(error.message,true)}
}
function chooseInboundEndpoint(){if(ir('Endpoint').value){ir('URL').value=ir('Endpoint').value;markInboundDirty()}}
function loadInboundTemplate(){
  if(inboundEditor.busy)return;
  if(inboundEditor.dirty&&!confirm('Replace the current unsaved draft with this template?'))return;
  const kind=ir('Template').value,out=qaIsOutbound();
  const token={country_code:'',party_id:'',uid:'',type:'',contract_id:'',issuer:'',valid:null,whitelist:'',last_updated:''};
  let module=kind,method='GET',body='';
  if(!out&&['tokens','token-patch'].includes(kind)){module='tokens';method=kind==='tokens'?'PUT':'PATCH';body=kind==='tokens'?token:{valid:false,last_updated:''}}
  else if(kind==='start'||kind==='stop'){module='commands';method='POST';body=kind==='start'?{response_url:'',token,location_id:'',evse_uid:'',connector_id:'',authorization_reference:''}:{response_url:'',session_id:''}}
  else if(out&&['locations','tariffs','sessions','cdrs'].includes(kind)){
    method=kind==='cdrs'?'POST':'PUT';body={country_code:'',party_id:'',id:'',last_updated:''};
    if(kind==='locations')Object.assign(body,{name:'',publish:true,address:'',city:'',country:'',coordinates:{latitude:'',longitude:''},time_zone:'',evses:[]});
    if(kind==='tariffs')Object.assign(body,{currency:'',elements:[]});
    if(kind==='sessions')Object.assign(body,{start_date_time:'',kwh:0,cdr_token:{country_code:'',party_id:'',uid:'',type:'',contract_id:''},auth_method:'',location_id:'',evse_uid:'',connector_id:'',currency:'',status:'ACTIVE'});
    if(kind==='cdrs')Object.assign(body,{session_id:'',start_date_time:'',end_date_time:'',cdr_token:{},auth_method:'',cdr_location:{},currency:'',charging_periods:[],total_cost:{excl_vat:0},total_energy:0,total_time:0});
  }
  else if(kind==='custom')module='custom';
  inboundEditor.id='';ir('Name').value=`${method} ${kind}`;ir('Module').value=module;ir('Method').value=method;ir('URL').value='';ir('Headers').value='{}';ir('Body').value=body===''?'':JSON.stringify(body,null,2);ir('Auth').value='registered';ir('HTTP').value='';ir('OCPI').value='';ir('Result').hidden=true;markInboundDirty();
  if(qaPrefillEnabled('rp'))applyRequestPrefill();
  if(ir('Form'))ir('Form').hidden=true;
  ir('Body').hidden=false;
  inboundMessage('Template loaded. Prefill is optional; no data is sent until you confirm. Empty fields need review.');
}

function requestTarget(body){
  const module=ir('Module').value,method=ir('Method').value,c=qaConfig(),out=qaIsOutbound();
  if(module==='versions')return c.evolt_versions_url||'';
  const role=out?(module==='tokens'?'SENDER':'RECEIVER'):(['tokens','commands'].includes(module)?'RECEIVER':'SENDER');
  const base=inboundEndpoint(module,role);if(!base)return '';
  if(module==='commands'){const kind=ir('Template').value;return ['start','stop'].includes(kind)?`${base.replace(/\/$/,'')}/${kind==='start'?'START_SESSION':'STOP_SESSION'}`:base}
  if(method==='GET'&&((out&&module==='tokens')||(!out&&!['tokens','commands'].includes(module)))){const url=new URL(base);url.searchParams.set('limit','10');return url.href}
  if(method==='POST'&&module==='cdrs')return base;
  const id=module==='tokens'?body.uid:body.id;
  if(!id||!body.country_code||!body.party_id)return '';
  const target=`${base.replace(/\/$/,'')}/${[body.country_code,body.party_id,id].map(encodeURIComponent).join('/')}`;
  return module==='tokens'&&body.type?`${target}?type=${encodeURIComponent(body.type)}`:target;
}
function applyRequestPrefill(){
  if(!qaPrefillEnabled('rp')||inboundEditor.busy)return;
  try{
    const raw=ir('Body').value,body=raw.trim()?JSON.parse(raw):null,headers=JSON.parse(ir('Headers').value);
    if(body!==null&&!qaPlain(body))throw new Error('Prefill needs a JSON object body, or an empty GET body. Raw request is unchanged.');
    if(!qaPlain(headers)||Object.values(headers).some(v=>typeof v!=='string'))throw new Error('Fix headers JSON before using prefill.');
    const module=ir('Module').value,c=qaConfig(),remote=c.remote_identity||{},data=qaPrefillData('rp',module),changes=[];
    const values={...data.body};
    if(module==='commands'){
      for(const key of Object.keys(values))if(!body||!Object.hasOwn(body,key))delete values[key];
      if(body&&Object.hasOwn(body,'response_url')&&c.partner_public_url){values.response_url=`${c.partner_public_url.replace(/\/$/,'')}/ocpi/emsp/2.2.1/commands/callback/${crypto.randomUUID()}`;data.report.push('Connection public URL + new callback UUID')}
      if(body&&Object.hasOwn(body,'authorization_reference')){values.authorization_reference=crypto.randomUUID();data.report.push('New authorization UUID')}
    }
    if(body&&Object.hasOwn(body,'last_updated')){values.last_updated=new Date().toISOString();data.report.push('Current UTC clock')}
    if(body)qaFillEmpty(body,values,'body',changes);
    const defaults={'OCPI-from-country-code':c.country_code,'OCPI-from-party-id':c.party_id,'OCPI-to-country-code':remote.country_code,'OCPI-to-party-id':remote.party_id,'X-Request-ID':crypto.randomUUID(),'X-Correlation-ID':crypto.randomUUID()};
    if(raw.trim())defaults['Content-Type']='application/json';
    for(const [key,val] of Object.entries(defaults)){const existing=Object.keys(headers).find(k=>k.toLowerCase()===key.toLowerCase());if(!existing&&!qaMissing(val)){headers[key]=val;changes.push(`headers.${key}`)}}
    if(qaMissing(ir('URL').value)){const url=requestTarget(body||{});if(url){ir('URL').value=url;changes.push('URL');data.report.push('Discovered endpoint / Connection')}}
    if(body)ir('Body').value=JSON.stringify(body,null,2);ir('Headers').value=JSON.stringify(headers,null,2);
    if(changes.length)markInboundDirty();data.report.push('New request/correlation UUIDs (only if absent)');qaPrefillReport('rp',changes,data.report);
    if(ir('Form')&&!ir('Form').hidden)showRequestBodyForm(true);
  }catch(error){document.getElementById('rpReport').textContent=`Prefill not applied: ${error.message}`}
}
function showRequestBodyForm(asForm){
  const panel=ir('Form');if(!asForm){panel.hidden=true;ir('Body').hidden=false;return}
  try{const body=JSON.parse(ir('Body').value);if(!qaPlain(body))throw new Error('Body must be a JSON object');panel.replaceChildren();
    for(const [key,value] of Object.entries(body)){
      const label=document.createElement('label');label.textContent=key;let input;
      if(typeof value==='boolean'){input=document.createElement('select');input.add(new Option('true','true'));input.add(new Option('false','false'));input.value=String(value)}
      else{input=document.createElement(typeof value==='object'?'textarea':'input');input.value=typeof value==='object'?JSON.stringify(value,null,2):String(value??'');if(input.tagName==='TEXTAREA')input.rows=5}
      input.addEventListener('input',()=>{try{const current=JSON.parse(ir('Body').value);current[key]=typeof value==='object'?JSON.parse(input.value):typeof value==='boolean'?input.value==='true':typeof value==='number'&&input.value!==''&&Number.isFinite(Number(input.value))?Number(input.value):input.value;ir('Body').value=JSON.stringify(current,null,2);input.setCustomValidity('');markInboundDirty()}catch{input.setCustomValidity('Invalid JSON. Use Raw JSON to send malformed values.');input.reportValidity()}});label.append(input);panel.append(label)
    }
    panel.hidden=false;ir('Body').hidden=true;
  }catch(error){inboundMessage(`${error.message} — use Raw JSON for negative cases.`,true);panel.hidden=true;ir('Body').hidden=false}
}
function readInboundDraft(){
  if(ir('Form')&&!ir('Form').hidden&&ir('Form').querySelector?.(':invalid'))throw new Error('Form contains invalid nested JSON. Fix it or switch to Raw JSON.');
  let headers;try{headers=JSON.parse(ir('Headers').value)}catch{throw new Error('Headers must be a JSON object of string values. Body may be invalid JSON.')}
  if(!headers||Array.isArray(headers)||typeof headers!=='object'||Object.values(headers).some(v=>typeof v!=='string'))throw new Error('Headers must be a JSON object of string values');
  if(Object.keys(headers).some(k=>/^(authorization|cookie)$/i.test(k)))throw new Error('Do not store Authorization or Cookie in a request. Use the credential selector.');
  const draft={lane:qaLane(),name:ir('Name').value.trim(),module:ir('Module').value.trim(),method:ir('Method').value,target_url:ir('URL').value.trim(),headers,use_auth:ir('Auth').value==='registered',content:ir('Body').value};
  if(!draft.name||!draft.module)throw new Error('Enter a case name and module');
  for(const [field,key] of [['HTTP','expected_http'],['OCPI','expected_ocpi']]){const value=ir(field).value;if(value!==''){const number=Number(value);if(!Number.isInteger(number)||number<0)throw new Error('Expected codes must be non-negative integers');draft[key]=number}}
  return draft;
}
function formatInboundBody(){try{ir('Body').value=JSON.stringify(JSON.parse(ir('Body').value),null,2);showRequestBodyForm(false);markInboundDirty();inboundMessage('Formatted JSON')}catch{inboundMessage('Invalid JSON — unchanged. You can still send it as a negative case.',true)}}
function loadInboundSample(){
  if(inboundEditor.busy)return;
  const sample=requestSampleBody(ir('Template').value);
  if(!sample){inboundMessage('Template นี้ไม่มี body ตัวอย่าง — GET และ Versions ส่ง body ว่าง เลือก template อื่นหรือพิมพ์เอง',true);return}
  if(ir('Body').value.trim()&&!confirm('Replace the current request body with the sample?'))return;
  ir('Body').value=JSON.stringify(sample,null,2);showRequestBodyForm(false);markInboundDirty();
  inboundMessage('Sample body loaded. ข้อมูลสมมติทั้งหมด แก้ ID และเวลาให้ตรง run ก่อนส่ง · Prefill ยังเติมเฉพาะช่องว่างตามเดิม');
}
function duplicateInboundRequest(){if(inboundEditor.busy)return;inboundEditor.id='';ir('Name').value=`${ir('Name').value} (copy)`;markInboundDirty();inboundMessage('New independent draft. Save or send to create a separate case.')}
function loadInboundSaved(){
  if(inboundEditor.dirty&&!confirm('Discard unsaved edits and load this request?'))return;
  const f=inboundEditor.fixtures.find(f=>f.id===ir('Saved').value);if(!f)return;
  inboundEditor.id=f.id;
  if(ir('Form'))ir('Form').hidden=true;
  ir('Body').hidden=false;
  for(const [field,key] of [['Name','name'],['Module','module'],['Method','method'],['URL','target_url'],['Body','content'],['HTTP','expected_http'],['OCPI','expected_ocpi']])ir(field).value=f[key]??'';
  ir('Headers').value=JSON.stringify(f.headers||{},null,2);ir('Auth').value=f.use_auth?'registered':'none';inboundEditor.dirty=false;inboundEditor.preview=null;ir('Dirty').textContent='Saved';ir('Result').hidden=true;
  const last=inboundEditor.exchanges.find(x=>x.fixture_id===f.id);if(last)showInboundResult(last);
  inboundMessage('Loaded saved request. Edits stay here when switching tabs; save before reloading the page.');
}
async function persistInboundDraft(draft){
  const id=inboundEditor.id||`${qaLane().toLowerCase()}-${crypto.randomUUID()}`;
  await qaRequest(`/api/requests/fixtures/${encodeURIComponent(id)}`,{method:'PUT',body:JSON.stringify(draft)});
  inboundEditor.id=id;
  if(JSON.stringify(readInboundDraft())===JSON.stringify(draft)){inboundEditor.dirty=false;ir('Dirty').textContent='Saved'}
  return id;
}
async function saveInboundRequest(){if(inboundEditor.busy)return;inboundEditor.busy=true;try{await persistInboundDraft(readInboundDraft());inboundMessage('Saved in Partner QA PostgreSQL. Nothing sent to Evolt.');await openInboundRequests()}catch(error){inboundMessage(error.message,true)}finally{inboundEditor.busy=false}}
function previewInboundRequest(){
  try{const draft=readInboundDraft(),url=new URL(draft.target_url);if(!['http:','https:'].includes(url.protocol)||url.username||url.password)throw new Error('Enter a full HTTP(S) URL without embedded credentials');
    let dialog=document.getElementById('irPreview');if(!dialog){dialog=document.createElement('dialog');dialog.id='irPreview';dialog.innerHTML='<div class="dialog-head"><h2>Review before sending</h2><button type="button" onclick="document.getElementById(\'irPreview\').close()">Close</button></div><p>This sends a real request and may change Evolt data. No automatic retries.</p><pre id="irPreviewBody"></pre><div class="modal-actions"><button class="ghost" onclick="document.getElementById(\'irPreview\').close()">Back to edit</button><button id="irConfirm" onclick="sendInboundRequest()">Confirm &amp; send once</button></div>';document.body.append(dialog)}
    inboundEditor.preview=draft;document.getElementById('irPreviewBody').textContent=JSON.stringify({lane:qaLane(),method:draft.method,url:draft.target_url,headers:draft.headers,authorization:draft.use_auth?`Registered ${qaLane()} credential (resolved when sent)`:'NONE',auto_headers:false,body_raw:draft.content,expected_http:draft.expected_http,expected_ocpi:draft.expected_ocpi},null,2);dialog.showModal();
  }catch(error){inboundMessage(error.message,true)}
}
async function sendInboundRequest(){
  if(inboundEditor.busy||!inboundEditor.preview)return;
  const draft=inboundEditor.preview;inboundEditor.busy=true;document.getElementById('irConfirm').disabled=true;
  try{const id=await persistInboundDraft(draft);inboundMessage('Sending…');const result=await qaRequest(`/api/requests/fixtures/${encodeURIComponent(id)}/send`,{method:'POST',body:JSON.stringify({exact_headers:true})});showInboundResult(result);inboundMessage('Response recorded. No automatic retry.');document.getElementById('irPreview').close();await openInboundRequests()}
  catch(error){inboundMessage(`Request not completed: ${error.message}. Check evidence before retrying.`,true);document.getElementById('irPreview').close()}
  finally{inboundEditor.busy=false;inboundEditor.preview=null;document.getElementById('irConfirm').disabled=false}
}
function showInboundResult(result){const status=result.response?.http_status;ir('Result').hidden=false;ir('ResultTitle').textContent=`HTTP ${status||'no response'} · OCPI ${result.response?.ocpi_status_code||'—'} · ${result.status} · ${result.duration_ms??0} ms`;ir('ResultTitle').dataset.http=status>=200&&status<300?'ok':'bad';ir('Sent').textContent=JSON.stringify(result.request,null,2);ir('Response').textContent=JSON.stringify(result.response,null,2)}
document.getElementById('inboundRequests').addEventListener('input',event=>{if(!['irSaved','irTemplate'].includes(event.target.id))markInboundDirty()});
document.getElementById('inboundRequests').addEventListener('change',event=>{if(!['irSaved','irTemplate','irEndpoint'].includes(event.target.id))markInboundDirty()});
window.addEventListener('beforeunload',event=>{if(inboundEditor.dirty){event.preventDefault();event.returnValue=''}});
document.querySelectorAll('.step-body').forEach(body=>{const step=body.closest('.step').dataset.step,template={2:'versions',4:'locations',5:'tokens',6:'start',7:'stop',8:'cdrs'}[step];if(!template)return;const button=document.createElement('button');button.type='button';button.className='ghost ir-step-edit';button.textContent='Edit request before sending';button.addEventListener('click',()=>openInboundRequests(template));body.prepend(button)});
