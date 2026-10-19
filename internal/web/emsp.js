let state = null;
let activeTable = 'setup';
let dbQuery = '';
let secretsVisible = false;
let dbSort = 'latest';
let suggestedStep = '';
let selectedTokenKey = null;
let tokenChoiceRun = null;
let tokenEditDirty = false;
const tables = ['setup','secrets','endpoints','tokens','locations','tariffs','sessions','cdrs','commands','exchanges'];

document.querySelectorAll('.step-head').forEach(head => head.addEventListener('click', () => openFlowStep(head.parentElement.dataset.step)));
document.addEventListener('keydown', event => { if(event.key==='Escape') closeReset(); });

async function request(path, options={}) {
  const busyButton=options.method&&options.method!=='GET'&&document.activeElement?.tagName==='BUTTON'?document.activeElement:null;
  if(busyButton){busyButton.disabled=true;busyButton.setAttribute('aria-busy','true')}
  try {
    const response = await fetch(appPath((pagePath().startsWith('/dual/')?'/dual':'')+path), {headers:{'Content-Type':'application/json'}, ...options});
    const data = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(data.detail || data.message || data.error || `HTTP ${response.status}`);
    return data;
  } finally { if(busyButton){busyButton.disabled=false;busyButton.removeAttribute('aria-busy')} }
}

async function loadState() {
  try { state = await request('/api/emsp/state' + (secretsVisible ? '?reveal_secrets=1' : '')); render(); if(typeof syncInboundRequestOptions==='function'&&document.getElementById('inboundRequests')?.hidden===false)syncInboundRequestOptions(); }
  catch (error) { toast(error.message, true); }
}
const refreshQueue=createRefreshQueue(loadState);
async function refresh(){return refreshQueue.now()}

async function saveConfig() {
  try {
    await request('/api/emsp/config', {method:'PUT', body:JSON.stringify({
      name:value('runName'), evolt_versions_url:value('versionsURL'), token_a:value('tokenA'), partner_public_url:value('publicURL'),
      country_code:value('countryCode'), party_id:value('partyID'), business_name:value('businessName'), strict:checked('strict'), raw_token_compatibility:checked('rawCompat')
    })});
    document.getElementById('tokenA').value=''; toast('Setup saved'); await refresh();
  } catch(error) { toast(error.message,true); }
}

function openReset(){document.getElementById('resetModal').classList.remove('hidden');document.getElementById('resetConfirmation').value='';syncResetButton();document.getElementById('resetConfirmation').focus()}
function closeReset(){document.getElementById('resetModal').classList.add('hidden');document.getElementById('resetConfirmation').value='';syncResetButton()}
function syncResetButton(){document.getElementById('confirmReset').disabled=document.getElementById('resetConfirmation').value!=='RESET'}
async function resetRun(){
  if(document.getElementById('resetConfirmation').value!=='RESET')return;
  try{await request('/api/emsp/reset',{method:'POST',body:JSON.stringify({confirmation:'RESET'})});closeReset();activeTable='setup';toast('Partner run reset — เริ่ม flow ใหม่ได้แล้ว');await refresh()}
  catch(error){toast(error.message,true);syncResetButton()}
}

async function runAction(action) { try { const data=await request(`/api/emsp/actions/${action}`,{method:'POST',body:'{}'}); toast(`${action} complete`); showResult(data); await refresh(); } catch(error){ toast(error.message,true); await refresh(); } }
async function pull(module) { try { const data=await request('/api/emsp/actions/pull',{method:'POST',body:JSON.stringify({module})}); toast(`${module}: ${data.stored}/${data.received} stored`); await refresh(); } catch(error){ toast(error.message,true); await refresh(); } }

function isDualInbound(){return pagePath()==='/dual/emsp'}
function tokenKey(token){return JSON.stringify([String(token.uid||''),String(token.type||'')])}
function savedTokens(){return (state?.tokens||[]).map(row=>row.body||row).filter(token=>token.uid&&token.type)}
function tokenChoiceStorageKey(){return `dual-token-choice:${state.run.id}`}
function newTokenTemplate(){
  const config=state?.run?.config||{},suffix=crypto.randomUUID().replaceAll('-','').slice(0,12).toUpperCase();
  const country=String(config.country_code||'TH').toUpperCase(),party=String(config.party_id||'TST').toUpperCase();
  return {uid:`MOCK-RFID-${suffix}`,type:'RFID',contract_id:`${country}-${party}-C${suffix}`,whitelist:'ALLOWED',valid:true};
}
function fillTokenForm(token){
  const source=token||newTokenTemplate();
  for(const [id,field] of [['tokenUID','uid'],['tokenType','type'],['contractID','contract_id'],['whitelist','whitelist']])document.getElementById(id).value=source[field];
  document.getElementById('tokenValid').value=String(source.valid);
  tokenEditDirty=false;
}
function chooseSavedToken(key){
  const token=key==='new'?null:savedTokens().find(item=>tokenKey(item)===key);
  if(key!=='new'&&!token)return toast('ไม่พบ Token นี้ใน Partner DB',true);
  selectedTokenKey=key;
  try{sessionStorage.setItem(tokenChoiceStorageKey(),key)}catch{}
  fillTokenForm(token);
  renderSavedTokenPickers();
}
function renderSavedTokenPickers(){
  if(!isDualInbound()||!state)return;
  const tokens=savedTokens(),runID=state.run.id;
  const firstLoad=tokenChoiceRun!==runID;
  if(firstLoad){
    tokenChoiceRun=runID;
    try{selectedTokenKey=sessionStorage.getItem(tokenChoiceStorageKey())}catch{selectedTokenKey=null}
  }
  if(selectedTokenKey!=='new'&&!tokens.some(token=>tokenKey(token)===selectedTokenKey)){
    selectedTokenKey=tokens.length?tokenKey(tokens[0]):'new';
    fillTokenForm(tokens[0]||null);
  }else if(firstLoad){
    fillTokenForm(tokens.find(token=>tokenKey(token)===selectedTokenKey)||null);
  }
  for(const id of ['savedTokenSelect','startTokenSelect']){
    const select=document.getElementById(id);
    select.replaceChildren(new Option('+ สร้าง Token ใหม่','new'),...tokens.map(token=>new Option(`${token.uid} · ${token.type} · ${token.contract_id||'ไม่มี Contract ID'}`,tokenKey(token))));
    select.value=selectedTokenKey;
  }
  document.getElementById('savedTokenNote').textContent=selectedTokenKey==='new'?'เติมค่าเริ่มต้นให้แล้ว · ปรับได้ก่อนกด Save test token':tokenEditDirty?'มีค่าที่แก้แต่ยังไม่ได้บันทึก · กด Save test token ก่อนส่ง':'เลือก Token เพื่อเติมค่าที่บันทึกไว้ แล้วแก้และบันทึกได้';
}

function receivedLocations(){
  return (state?.locations||[]).map(row=>{
    const body=row.body||{},id=String(body.id||row.object_id||'');
    const name=String(body.name||''),owner=[body.country_code||row.country_code,body.party_id||row.party_id].filter(Boolean).join('-');
    return {id,label:[name,id,owner].filter(Boolean).join(' · '),evses:Array.isArray(body.evses)?body.evses:[]};
  }).filter(location=>location.id).sort((a,b)=>a.label.localeCompare(b.label));
}
function syncReceivedSelect(id,placeholder,options,current){
  const select=document.getElementById(id),signature=JSON.stringify([placeholder,options]);
  if(select.dataset.signature!==signature){
    select.replaceChildren(new Option(placeholder,''),...options.map(option=>new Option(option.label,option.value)));
    select.dataset.signature=signature;
  }
  select.disabled=options.length===0;
  select.value=options.some(option=>option.value===current)?current:'';
}
function renderReceivedLocationOptions(){
  if(!isDualInbound()||!state)return;
  const locations=receivedLocations(),locationID=value('locationID');
  syncReceivedSelect('receivedLocationSelect','เลือก Location หรือกรอก ID ด้านล่าง',locations.map(location=>({value:location.id,label:location.label})),locationID);
  const location=locations.find(item=>item.id===locationID);
  const evses=(location?.evses||[]).filter(evse=>evse?.uid).map(evse=>({value:String(evse.uid),label:[evse.uid,evse.evse_id,evse.status].filter(Boolean).join(' · '),connectors:Array.isArray(evse.connectors)?evse.connectors:[]}));
  const evseUID=value('evseUID');
  syncReceivedSelect('receivedEVSESelect',location?'เลือก EVSE หรือกรอก UID ด้านล่าง':'เลือก Location ก่อน หรือกรอก UID',evses.map(({value,label})=>({value,label})),evseUID);
  const evse=evses.find(item=>item.value===evseUID);
  const connectors=(evse?.connectors||[]).filter(connector=>connector?.id).map(connector=>({value:String(connector.id),label:[connector.id,connector.standard,connector.power_type].filter(Boolean).join(' · ')}));
  syncReceivedSelect('receivedConnectorSelect',evse?'เลือก Connector หรือกรอก ID ด้านล่าง':'เลือก EVSE ก่อน หรือกรอก ID',connectors,value('connectorID'));
  document.getElementById('receivedLocationNote').textContent=locations.length?`มี ${locations.length} Location ที่ได้รับจาก Evolt · เลือกตามลำดับ Location → EVSE → Connector หรือแก้ช่อง ID ด้านล่างเอง`:'ยังไม่มี Location ที่บันทึกไว้ · กด Load stations ในขั้นก่อน หรือกรอก ID ทั้งสามช่องเอง';
}
function chooseReceivedLocation(id){
  const input=document.getElementById('locationID');
  if(!id)return input.focus();
  if(input.value.trim()!==id){document.getElementById('evseUID').value='';document.getElementById('connectorID').value=''}
  input.value=id;
  renderReceivedLocationOptions();
}
function chooseReceivedEVSE(uid){
  const input=document.getElementById('evseUID');
  if(!uid)return input.focus();
  if(input.value.trim()!==uid)document.getElementById('connectorID').value='';
  input.value=uid;
  renderReceivedLocationOptions();
}
function chooseReceivedConnector(id){
  const input=document.getElementById('connectorID');
  if(!id)return input.focus();
  input.value=id;
  renderReceivedLocationOptions();
}

async function createToken() {
  const previous=isDualInbound()&&selectedTokenKey!=='new'?savedTokens().find(item=>tokenKey(item)===selectedTokenKey):null;
  const token={...previous,uid:value('tokenUID'),type:value('tokenType'),contract_id:value('contractID'),whitelist:value('whitelist'),valid:value('tokenValid')==='true'};
  try {
    const saved=await request('/api/emsp/tokens',{method:'POST',body:JSON.stringify(token)});
    if(isDualInbound()){selectedTokenKey=tokenKey(saved);tokenEditDirty=false;try{sessionStorage.setItem(tokenChoiceStorageKey(),selectedTokenKey)}catch{}}
    toast('Token saved in PostgreSQL');await refresh();
  }catch(error){toast(error.message,true)}
}

function chosenSavedToken(){
  if(!isDualInbound())return {uid:value('tokenUID'),type:value('tokenType')};
  if(tokenEditDirty){toast('บันทึก Token ที่แก้ก่อนส่งหรือเริ่ม Session',true);return null}
  const token=savedTokens().find(item=>tokenKey(item)===selectedTokenKey);
  if(!token){toast('เลือก Token ที่บันทึกไว้ หรือสร้างและบันทึก Token ใหม่ก่อน',true);return null}
  return token;
}

async function pushToken() {const token=chosenSavedToken();if(!token)return;try { await request('/api/emsp/actions/push-token',{method:'POST',body:JSON.stringify({uid:token.uid,type:token.type})}); toast('Token pushed'); await refresh(); } catch(error){ toast(error.message,true); await refresh(); } }

async function startSession() {
  const token=chosenSavedToken();if(!token)return;
  const payload={token_uid:token.uid,token_type:token.type,location_id:value('locationID'),authorization_reference:crypto.randomUUID()};
  if(value('evseUID')) payload.evse_uid=value('evseUID'); if(value('connectorID')) payload.connector_id=value('connectorID');
  try { const data=await request('/api/emsp/actions/command',{method:'POST',body:JSON.stringify({type:'START_SESSION',payload})}); toast(`START_SESSION ${data.direct_response.result}`); await refresh(); } catch(error){ toast(error.message,true); await refresh(); }
}

async function stopSession() {
  if(!value('sessionID')) return toast('Choose a Session first',true);
  try { const data=await request('/api/emsp/actions/command',{method:'POST',body:JSON.stringify({type:'STOP_SESSION',payload:{session_id:value('sessionID')}})}); toast(`STOP_SESSION ${data.direct_response.result}`); await refresh(); } catch(error){ toast(error.message,true); await refresh(); }
}

function render() {
  const config=state.run.config||{};
  document.getElementById('runStatus').textContent=state.run.status;
  setIfIdle('runName',state.run.name); setIfIdle('versionsURL',config.evolt_versions_url); setIfIdle('publicURL',config.partner_public_url); setIfIdle('countryCode',config.country_code); setIfIdle('partyID',config.party_id); setIfIdle('businessName',config.business_name);
  document.getElementById('strict').checked=!!config.strict; document.getElementById('rawCompat').checked=!!config.raw_token_compatibility;
  const findings=(state.exchanges||[]).reduce((n,x)=>n+(x.findings||[]).filter(f=>f.level==='error').length,0);
  text('metricExchanges',(state.exchanges||[]).length); text('metricFindings',findings); text('metricSessions',(state.sessions||[]).length); text('metricCdrs',(state.cdrs||[]).length);
  const notice=document.getElementById('notice'); if(state.security?.development_encryption_key){notice.classList.remove('hidden');notice.textContent='Development encryption key is active. Set MOCK_ENCRYPTION_KEY to a strong random secret before storing real OCPI tokens or testing through a tunnel.'}else notice.classList.add('hidden');
  renderSavedTokenPickers();renderReceivedLocationOptions();renderSteps(); renderTimeline(); renderDB(); renderSessionOptions();
}

function renderSteps() {
  const status=state.run.status, endpoints=state.endpoints||[], tokens=state.tokens||[], commands=state.commands||[], sessions=state.sessions||[], cdrs=state.cdrs||[];
  if(pagePath()==='/dual/emsp'){mark(1,status==='REGISTERED');mark(2,true);mark(3,true)}
  else{mark(1,true);mark(2,['DISCOVERED','REGISTERED'].includes(status)||endpoints.length>0);mark(3,status==='REGISTERED')}
  mark(4,(state.locations||[]).length>0||(state.tariffs||[]).length>0); mark(5,tokens.length>0); mark(6,commands.some(c=>c.type==='START_SESSION')); mark(7,sessions.length>0); mark(8,cdrs.length>0);
  const next=document.querySelector('.step:not(.done)'),nextStep=next?.dataset.step||'complete';
  if(!suggestedStep&&next){openFlowStep(nextStep);suggestedStep=nextStep}
  renderNextAction();
}
function mark(step,done){const el=document.querySelector(`[data-step="${step}"]`);el.classList.toggle('done',done);el.querySelector('.step-state').textContent=done?'DONE':'WAIT'}

const inboundSteps={
  1:['ตั้งค่าการเชื่อมต่อ','ใส่ Versions URL, Token A และ Partner public URL แล้วบันทึก'],
  2:['ตรวจสอบการเชื่อมต่อ','ตรวจ Versions และ endpoints ที่ Evolt ประกาศ'],
  3:['เชื่อมต่อ Partner','แลก Credentials เพื่อให้ทั้งสองฝั่งเรียกหากันได้'],
  4:['โหลดสถานีและราคา','ดึง Locations และ Tariffs จาก Evolt มาเก็บเป็นหลักฐาน'],
  5:['ส่ง Token ทดสอบ','สร้าง Token สำหรับทดสอบแล้วส่งให้ Evolt'],
  6:['ขอเริ่มการชาร์จ','เลือกสถานีและหัวชาร์จ แล้วส่ง START_SESSION'],
  7:['ตรวจ Session และหยุดการชาร์จ','รอ Session เกิดจริง จากนั้นส่ง STOP_SESSION'],
  8:['ตรวจรายการค่าใช้จ่าย','รอหรือดึง CDR แล้วตรวจผลลัพธ์สุดท้าย']
};
function renderNextAction(){const next=document.querySelector('.step:not(.done)');if(!next){text('nextActionTitle','ครบทุกขั้นตอนแล้ว — ตรวจหลักฐาน');text('nextActionHint','ตรวจ Request history, Partner test data และ Export report เพื่อเก็บผลทดสอบ');text('nextActionButton','ดูผลทดสอบ');return}const step=Number(next.dataset.step),copy=inboundSteps[step],dual=pagePath()==='/dual/emsp';text('nextActionTitle',`${dual&&step>=4?step-2:step}. ${dual&&step===1?'Shared connection':copy[0]}`);text('nextActionHint',dual&&step===1?'ตั้งค่า Discover และ Credentials ที่หน้า Connection กลาง':copy[1]);text('nextActionButton','ไปขั้นตอนนี้')}
function goToNextStep(){const next=document.querySelector('.step:not(.done)');if(!next){selectWorkspaceView('activity');document.querySelector('[data-workspace-panel="activity"]').scrollIntoView({behavior:'smooth',block:'start'});return}selectWorkspaceView('flow');document.querySelectorAll('.step').forEach(step=>step.classList.toggle('open',step===next));next.scrollIntoView({behavior:'smooth',block:'center'})}

function renderTimeline(){renderRequestHistory('timeline',state.exchanges||[])}

function shellQuote(value){return "'"+String(value).replace(/'/g,"'\\''")+"'"}
async function copyCurl(index){
  const x=(state.exchanges||[])[index]; if(!x)return;
  const lines=[`curl -X ${x.method} ${shellQuote(x.url)}`];
  Object.entries(x.request_headers||{}).forEach(([key,header])=>lines.push(`  -H ${shellQuote(`${key}: ${Array.isArray(header)?header.join(', '):header}`)}`));
  if(x.request_body!==null&&x.request_body!==undefined)lines.push(`  --data ${shellQuote(JSON.stringify(x.request_body))}`);
  try{await navigator.clipboard.writeText(lines.join(" \\\n"));toast('cURL copied with complete headers')}catch(error){toast(`Copy failed: ${error.message}`,true)}
}

function setDBQuery(query){dbQuery=query.trim().toLowerCase();renderDB()}
function setDBSort(sort){dbSort=sort;renderDB()}
function recordName(row){return String(row.object_id||row.uid||row.identifier||row.key||row.name||row.id||'')}
function recordTimestamp(row){const value=row.updated_at||row.created_at||row.body?.last_updated||row.last_updated;const time=Date.parse(value||'');return Number.isFinite(time)?time:0}
function recordDate(row){const time=recordTimestamp(row);return time?new Date(time).toLocaleString():'—'}
function sortRecords(rows){return [...rows].sort((a,b)=>dbSort==='name'?recordName(a).localeCompare(recordName(b)):dbSort==='oldest'?recordTimestamp(a)-recordTimestamp(b):recordTimestamp(b)-recordTimestamp(a))}
function secretBaselineKey(){return `inbound-secret-baseline:${state?.run?.id}`}
function secretBaselineRecord(){try{const stored=JSON.parse(localStorage.getItem(secretBaselineKey())||'{}');return stored&&stored.fingerprints?{at:stored.at||'',fingerprints:stored.fingerprints}:{at:'',fingerprints:stored||{}}}catch{return {at:'',fingerprints:{}}}}
function secretComparison(row){const before=(secretBaselineRecord().fingerprints||{})[row.key];if(!row.fingerprint||!before)return {tone:'neutral',label:'ยังไม่มี baseline'};return before===row.fingerprint?{tone:'same',label:'ยังไม่เปลี่ยน'}:{tone:'changed',label:'เปลี่ยนแล้ว'}}
function secretDescription(key){return ({inbound_token_b:'Evolt → Partner · token ที่ Partner ออกให้ Evolt',remote_token_c:'Partner → Evolt · token ที่ Evolt ออกให้ Partner',remote_token_a:'Token A ที่ Evolt ออกให้ · handshake เท่านั้น',mock_initial_token:'Token A ที่ Partner ออกให้ · handshake เท่านั้น'})[key]||'Encrypted Partner credential'}
async function toggleSecrets(){secretsVisible=!secretsVisible;try{state=await request('/api/emsp/state'+(secretsVisible?'?reveal_secrets=1':''));render()}catch(error){secretsVisible=false;toast(error.message,true)}}
function saveSecretBaseline(){const rows=state?.secrets||[],fingerprints={};rows.forEach(row=>{if(row.fingerprint)fingerprints[row.key]=row.fingerprint});if(!Object.keys(fingerprints).length)return toast('กด Show secrets ก่อนบันทึกค่า Before Rotate',true);try{localStorage.setItem(secretBaselineKey(),JSON.stringify({at:new Date().toISOString(),fingerprints}));renderDB();toast('บันทึก fingerprint ก่อน Rotate แล้ว')}catch(error){toast(error.message,true)}}
function clearSecretBaseline(){try{localStorage.removeItem(secretBaselineKey());renderDB();toast('ล้าง baseline แล้ว')}catch(error){toast(error.message,true)}}
function secretBaselineNote(){const record=secretBaselineRecord(),keys=Object.keys(record.fingerprints||{});if(!keys.length)return '<span>ยังไม่ได้บันทึก baseline · กด Show secrets แล้วกด “จำค่า Before Rotate” ก่อนไปหมุน key</span>';const at=Date.parse(record.at||'');return `<span>baseline เก็บไว้ ${keys.length} ใบ${Number.isFinite(at)&&at?` · เมื่อ ${esc(new Date(at).toLocaleString())}`:''} <button class="link-button" type="button" onclick="clearSecretBaseline()">ล้าง baseline</button></span>`}
async function copySecret(value){try{await navigator.clipboard.writeText(value);toast('Copied')}catch(error){toast(error.message,true)}}
function attrValue(value){return esc(value).replace(/`/g,'&#96;')}
function renderSecretCard(row,index){const comparison=secretComparison(row),name=recordName(row)||`row ${index+1}`,copy=row.value?`<div class="partner-secret-copy"><code>${esc(row.value)}</code><button class="ghost" type="button" onclick="copySecret('${attrValue(row.value)}')">Copy secret</button><code>${esc(row.authorization||'')}</code><button class="ghost" type="button" onclick="copySecret('${attrValue(row.authorization||'')}')">Copy Authorization</button></div>`:'';return `<details class="db-card partner-secret-card"><summary class="db-card-head"><span class="db-card-name"><b>${esc(name)}</b><small>${esc(secretDescription(row.key))}</small></span><time>${esc(row.fingerprint||recordDate(row))}</time><span class="secret-compare ${comparison.tone}">${esc(comparison.label)}</span></summary>${copy}<pre>${esc(JSON.stringify(row,null,2))}</pre></details>`}
function setDBTable(name){if(name!==activeTable&&secretsVisible)return void toggleSecrets().then(()=>{activeTable=name;renderDB()});activeTable=name;renderDB()}
function renderDB() {
  const tabs=document.getElementById('dbTabs'); tabs.innerHTML=tables.map(t=>`<button class="${t===activeTable?'active':''}" onclick="setDBTable('${t}')">${t} <small>${(state[t]||[]).length}</small></button>`).join('');
  const secretsTab=activeTable==='secrets',toggle=document.getElementById('dbSecretToggle'),baseline=document.getElementById('dbSecretBaseline');
  if(toggle){toggle.hidden=!secretsTab;toggle.textContent=secretsVisible?'Hide secrets':'Show secrets'}
  if(baseline)baseline.hidden=!secretsTab||!secretsVisible;
  const secretHelp=secretsTab?`<div class="partner-secret-help"><strong>วิธีเช็ค Rotate Key ขา Inbound</strong><span>กด Show secrets → จำค่า Before Rotate → หมุน credentials → กลับมากด Show secrets อีกครั้ง · <code>inbound_token_b</code> และ <code>remote_token_c</code> ต้องขึ้น “เปลี่ยนแล้ว” ทั้งคู่</span>${secretBaselineNote()}</div>`:'';
  const source=state[activeTable]||[],rows=sortRecords(dbQuery?source.filter(row=>JSON.stringify(row).toLowerCase().includes(dbQuery)):source),root=document.getElementById('dbContent'); if(!rows.length){root.innerHTML=secretHelp+`<div class="empty">${dbQuery?'ไม่พบข้อมูลที่ค้นหา':'No records in this table'}</div>`;return}
  root.innerHTML=secretHelp+rows.map((row,i)=>secretsTab?renderSecretCard(row,i):`<details class="db-card"><summary class="db-card-head"><span class="db-card-name">${esc(recordName(row)||`row ${i+1}`)}</span><time>${esc(recordDate(row))}</time><span class="${row.encrypted?'encrypted':''}">${esc(row.status||(row.encrypted?'encrypted':null)||row.role||row.type||'')}</span></summary><pre>${esc(JSON.stringify(row,null,2))}</pre></details>`).join('');
}

function renderSessionOptions(){const select=document.getElementById('sessionID'),old=select.value;select.innerHTML='<option value="">Choose Session</option>'+(state.sessions||[]).map(s=>`<option value="${esc(s.object_id)}">${esc(s.object_id)} · ${esc(s.body?.status||'')}</option>`).join('');select.value=old}

function showResult(data){console.info('OCPI action result',data)}
function toast(message,error=false){const el=document.getElementById('toast');el.textContent=message;el.className=`toast show ${error?'error':''}`;clearTimeout(el.timer);el.timer=setTimeout(()=>el.className='toast',3500)}
function value(id){return document.getElementById(id).value.trim()} function checked(id){return document.getElementById(id).checked} function text(id,v){document.getElementById(id).textContent=v}
function setIfIdle(id,v){const el=document.getElementById(id);if(document.activeElement!==el&&v!==undefined&&v!==null)el.value=v}
function esc(v){return String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))} function time(v){try{return new Date(v).toLocaleTimeString()}catch{return v}}

if(isDualInbound())for(const id of ['tokenUID','tokenType','contractID','whitelist','tokenValid'])for(const event of ['input','change'])document.getElementById(id).addEventListener(event,()=>{tokenEditDirty=true;renderSavedTokenPickers()});
if(isDualInbound())for(const id of ['locationID','evseUID','connectorID'])document.getElementById(id).addEventListener('input',renderReceivedLocationOptions);

const stream=new EventSource(appPath((pagePath().startsWith('/dual/')?'/dual':'')+'/api/emsp/events'));stream.addEventListener('ready',()=>{text('live','LIVE');document.getElementById('live').className='pill';refresh()});stream.addEventListener('changed',()=>refreshQueue.schedule());stream.onerror=()=>{text('live','RECONNECTING');document.getElementById('live').className='pill neutral'};
refresh();
