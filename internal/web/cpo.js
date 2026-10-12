let state={objects:{},commands:[],exchanges:[],endpoints:[],config:{}},database={tables:{}};
let activeModule='locations',selectedID='',editorMode='form',pendingPreview=null,objectQuery='';
let activeDBTable='setup';
let partnerDBQuery='',partnerDBSort='latest';
let partnerDBSecretsVisible=false;
let objectDelivery='all',objectSort='latest';
let editorTab='edit';
const editorDrafts=new Map(),newDrafts=new Map();
const meterManualTargets=new Map();
const meterControl={autoMode:false,rate:'7.2',run:null,timer:null,busy:false};
const parkingPreview={sessionID:'',data:null,lastFetch:0,pending:false};
function draftKey(){return `${activeModule}/${selectedID}`}
function captureEditorDraft(){const record=currentRecord();if(!record||['tokens','commands'].includes(activeModule)||(activeModule==='cdrs'&&(record.delivery==='SENT'||record.body.credit===true)))return;editorDrafts.set(draftKey(),document.getElementById('jsonEditor').value);document.getElementById('draftState').hidden=false}
const dbTables=['setup','secrets','endpoints','locations','tariffs','sessions','cdrs','tokens','commands','exchanges'];
const moduleMeta={locations:['Locations','สถานี, EVSE และหัวชาร์จ'],tariffs:['Tariffs','อัตราค่าบริการ'],sessions:['Sessions','สถานะการชาร์จ'],cdrs:['CDRs','ข้อมูลค่าใช้จ่ายหลังจบการชาร์จ'],tokens:['Tokens','Token ที่รับหรือดึงจาก Evolt'],commands:['Commands','คำสั่งที่ Evolt ส่งเข้ามา']};
const moduleActions={locations:['+ New location','Send all locations'],tariffs:['+ New tariff','Send all tariffs'],sessions:['+ New session','Send all sessions'],cdrs:['+ New CDR','Send all CDRs']};
function deliveryText(value){return ({NOT_SENT:'Not sent',SENT:'Sent',FAILED:'Failed',CHANGED_AFTER_SEND:'Edited since send',RECEIVED:'Received',ACCEPTED:'Accepted'})[value]||value}
const formFields={
  locations:[['id','ID',true],['name','Name',false],['address','Address',true],['city','City',true],['postal_code','Postal code',false],['country','Country alpha-3',true],['time_zone','Time zone',true],['last_updated','Last updated UTC',true]],
  tariffs:[['id','ID',true],['currency','Currency',true],['last_updated','Last updated UTC',true]],
  sessions:[['id','ID',true],['location_id','Location ID',true],['evse_uid','EVSE UID',true],['connector_id','Connector ID',true],['kwh','kWh',true],['currency','Currency',true],['status','Status',true],['last_updated','Last updated UTC',true]],
  cdrs:[['id','ID',true],['session_id','Session ID',false],['total_energy','Total energy kWh',true],['total_time','Total time hours',true],['currency','Currency',true],['last_updated','Last updated UTC',true]],
  tokens:[['uid','UID',true],['type','Type',true],['contract_id','Contract ID',true],['issuer','Issuer',true],['valid','Valid',true],['whitelist','Whitelist',true],['last_updated','Last updated UTC',true]]
};

async function api(path,options={}){const response=await fetch((location.pathname.startsWith('/dual/')?'/dual':'')+path,{headers:{'Content-Type':'application/json',...(options.headers||{})},...options});const data=await response.json().catch(()=>({}));if(!response.ok)throw new Error(data.error||`HTTP ${response.status}`);return data}
async function loadState(){try{state=await api('/api/cpo/state');for(const [key,record] of newDrafts){const module=key.split('/')[0];state.objects[module]??=[];if(!state.objects[module].some(item=>item.id===record.id))state.objects[module].push(record)}if(meterControl.run&&!state.objects?.sessions?.some(record=>record.id===meterControl.run.sessionID&&record.body.status==='ACTIVE'&&!stopCommandFor(record.id)?.result?.idle_started_at))stopAutoMeter(false);if(document.querySelector('[data-workspace-panel="data"]')?.hidden===false)await fetchPartnerDB();render();tickAutoMeter();void tickParking();if(typeof syncInboundRequestOptions==='function'&&document.getElementById('inboundRequests')?.hidden===false)syncInboundRequestOptions()}catch(error){toast(`Unable to refresh: ${error.message}`,true)}}
async function fetchPartnerDB(){try{database=await api('/api/cpo/db'+(partnerDBSecretsVisible?'?reveal_secrets=1':''))}catch(error){database={mode:'error',tables:{},error:error.message}}}
async function loadPartnerDB(){await fetchPartnerDB();renderDB()}
const refreshQueue=createRefreshQueue(loadState);
async function refresh(){return refreshQueue.now()}

async function saveConfig(){try{await api('/api/cpo/config',{method:'PUT',body:JSON.stringify({evolt_versions_url:value('versionsURL'),partner_public_url:value('publicURL'),country_code:value('countryCode'),party_id:value('partyID'),business_name:value('businessName'),token_a:value('tokenA'),raw_token_compatibility:document.getElementById('rawCompat').checked})});toast('Connection settings saved');await refresh()}catch(error){toast(error.message,true);throw error}}
async function discover(){try{await saveConfig();await api('/api/cpo/actions/discover',{method:'POST',body:'{}'});toast('Evolt endpoints discovered');await refresh()}catch(error){toast(error.message,true)}}
async function registerPartner(){try{await api('/api/cpo/actions/register',{method:'POST',body:'{}'});toast('Partner CPO registered');await refresh()}catch(error){toast(error.message,true)}}
async function credentialsAction(action){try{await api(`/api/cpo/actions/credentials/${action}`,{method:'POST',body:'{}'});toast(`Credentials ${action} complete`);await refresh()}catch(error){toast(error.message,true)}}
async function generateInitialToken(){try{await saveConfig();const result=await api('/api/cpo/initial-token',{method:'POST',body:'{}'});text('initialTokenValue',result.token);text('initialVersionsURL',result.versions_url);document.getElementById('initialTokenPanel').classList.remove('hidden');toast('Partner Token A generated — copy Token และ Versions URL ไปใส่เอง')}catch(error){toast(error.message,true)}}
async function revealQATokens(){
  const mount=document.getElementById('qaTokenList');
  try{
    const data=await api('/api/cpo/tokens');
    const rows=[['partner_to_evolt','ยิงจาก Partner → Evolt (Postman: token_inbound)'],['evolt_to_partner','Evolt → Partner'],['registration','Token A (handshake)']];
    mount.innerHTML=rows.map(([key,title])=>{
      const entry=data[key]||{};
      if(!entry.present)return `<div class="qa-token"><strong>${esc(title)}</strong><small>ยังไม่มีค่า — ${esc(entry.note||'')}</small></div>`;
      return `<div class="qa-token"><strong>${esc(title)}</strong><small>${esc(entry.note||'')}</small>`+
        `<code>${esc(entry.token)}</code><button class="ghost" onclick="copyQAToken('${attr(entry.token)}')">Copy token</button>`+
        `<code>${esc(entry.authorization)}</code><button class="ghost" onclick="copyQAToken('${attr(entry.authorization)}')">Copy Authorization</button></div>`;
    }).join('');
  }catch(error){toast(error.message,true)}
}
async function copyQAToken(value){try{await navigator.clipboard.writeText(value);toast('Copied')}catch(error){toast(error.message,true)}}
async function copyInitialRegistration(){const token=document.getElementById('initialTokenValue').textContent,versionsURL=document.getElementById('initialVersionsURL').textContent;try{await navigator.clipboard.writeText(`Token A: ${token}\nVersions URL: ${versionsURL}`);toast('Registration setup copied')}catch(error){toast(error.message,true)}}
function openReset(){document.getElementById('resetModal').classList.remove('hidden');document.body.classList.add('modal-open');document.getElementById('resetConfirmation').value='';syncResetButton();document.getElementById('resetConfirmation').focus()}
function closeReset(){document.getElementById('resetModal').classList.add('hidden');document.body.classList.remove('modal-open');document.getElementById('resetConfirmation').value='';syncResetButton()}
function syncResetButton(){document.getElementById('confirmReset').disabled=document.getElementById('resetConfirmation').value!=='RESET'}
async function resetRun(){if(document.getElementById('resetConfirmation').value!=='RESET')return;try{await api('/api/cpo/reset',{method:'POST',body:JSON.stringify({confirmation:'RESET'})});closeReset();selectedID='';newDrafts.clear();editorDrafts.clear();toast('Outbound QA reset · API Workspace unchanged');await refresh()}catch(error){toast(error.message,true);syncResetButton()}}
async function loadSeeds(){try{await api('/api/cpo/seeds',{method:'POST',body:'{}'});toast('Sample dataset loaded');await refresh()}catch(error){toast(error.message,true)}}
async function pullTokens(){try{const result=await api('/api/cpo/actions/pull-tokens',{method:'POST',body:'{}'});toast(`Pulled ${result.pulled} Token(s) from Evolt${result.invalidated?` · invalidated ${result.invalidated} stale`:''}`);await refresh()}catch(error){toast(error.message,true);await refresh()}}

function selectModule(module){activeModule=module;selectedID='';objectQuery='';objectDelivery='all';editorTab=module==='sessions'&&!(state.objects.sessions||[]).length?'draft':'edit';document.getElementById('objectDelivery').value='all';const search=document.getElementById('objectSearch');if(search)search.value='';document.querySelectorAll('.module-tabs button').forEach(button=>{button.classList.toggle('active',button.dataset.module===module);button.setAttribute('aria-pressed',String(button.dataset.module===module))});renderObjects();renderEditor()}
function setObjectQuery(query){objectQuery=query.trim().toLowerCase();renderObjects()}
function setObjectDelivery(value){objectDelivery=value;renderObjects()}
function setObjectSort(value){objectSort=value;renderObjects()}
function selectObject(id){if(activeModule==='sessions'&&meterControl.run?.sessionID!==id)stopAutoMeter();selectedID=id;renderObjects();renderEditor()}
function currentRecord(){if(activeModule==='commands'){const command=(state.commands||[]).find(record=>record.id===selectedID);return command?{id:command.id,body:command,delivery:command.status,findings:[]}:null}return (state.objects?.[activeModule]||[]).find(record=>record.id===selectedID)}

function render(){const config=state.config||{},registered=state.status==='REGISTERED';text('runStatus',state.status||'SETUP');text('connectionState',state.status||'SETUP');text('storageBadge',(state.storage?.mode||'unknown').toUpperCase());setIfIdle('versionsURL',config.evolt_versions_url);setIfIdle('publicURL',config.partner_public_url);setIfIdle('countryCode',config.country_code);setIfIdle('partyID',config.party_id);setIfIdle('businessName',config.business_name);document.getElementById('rawCompat').checked=!!config.raw_token_compatibility;text('metricLocations',(state.objects.locations||[]).length);text('metricTariffs',(state.objects.tariffs||[]).length);text('metricSessions',(state.objects.sessions||[]).length);text('metricCdrs',(state.objects.cdrs||[]).length);text('metricTokens',(state.objects.tokens||[]).length);text('metricCommands',(state.commands||[]).length);const connectionPanel=document.getElementById('connectionPanel');if(!connectionPanel.dataset.initialized){connectionPanel.open=!registered;connectionPanel.dataset.initialized='true'}renderPartnerNext();renderEndpoints();renderObjects();renderEditor();renderTimeline();renderDB();if(typeof renderOutboundFlow==='function')renderOutboundFlow();if(typeof refreshQAPrefill==='function')refreshQAPrefill('op')}
function renderPartnerNext(){const config=state.config||{},hasConnection=config.evolt_versions_url&&config.partner_public_url;let title,hint,target;if(!hasConnection){title='1. ตั้งค่าการเชื่อมต่อ';hint='ใส่ Evolt Versions URL, Token A และ Partner public URL';target='connection'}else if(!(state.endpoints||[]).length){title='2. ตรวจสอบการเชื่อมต่อ';hint='Discover OCPI 2.2.1 endpoints ที่ Evolt เปิดให้ใช้งาน';target='connection'}else if(state.status!=='REGISTERED'){title='3. เชื่อมต่อ Partner';hint='แลก Credentials ให้สำเร็จก่อนส่งหรือรับข้อมูล';target='connection'}else{title='4. เตรียมข้อมูลทดสอบ';hint='เลือก Locations, Tariffs, Sessions หรือ CDRs แล้วส่งไปตรวจใน Evolt';target='objects'}text('partnerNextTitle',title);text('partnerNextHint',hint);document.getElementById('partnerNextButton').dataset.target=target}
function goPartnerNext(){const isObjects=document.getElementById('partnerNextButton').dataset.target==='objects';if(!isObjects&&location.pathname.startsWith('/dual/')){location.href='/dual/';return}selectWorkspaceView(isObjects?'catalogue':'connection');const target=document.getElementById(isObjects?'objectWorkspace':'connectionPanel');if(!isObjects&&target)target.open=true;target?.scrollIntoView({behavior:'smooth',block:'start'})}
function renderEndpoints(){const root=document.getElementById('endpointTable'),endpoints=state.endpoints||[];if(!endpoints.length){root.innerHTML='<div class="empty">Run Discover to see Evolt endpoints</div>'}else root.innerHTML=`<table><thead><tr><th>IDENTIFIER</th><th>ROLE</th><th>URL DISCOVERED AT RUNTIME</th></tr></thead><tbody>${endpoints.map(endpoint=>`<tr><td>${esc(endpoint.identifier)}</td><td>${esc(endpoint.role)}</td><td>${esc(endpoint.url)}</td></tr>`).join('')}</tbody></table>`;const expected=[['credentials',''],['locations','RECEIVER'],['tariffs','RECEIVER'],['sessions','RECEIVER'],['cdrs','RECEIVER'],['tokens','SENDER']];document.getElementById('endpointReadiness').innerHTML=expected.map(([module,role])=>{const exists=endpoints.some(endpoint=>endpoint.identifier===module&&(!role||endpoint.role===role));return `<span class="ready-chip ${exists?'ok':''}">${exists?'✓':'×'} ${module}${role?' '+role:''}</span>`}).join('')}
function renderObjects(){
  const readOnly=activeModule==='tokens'||activeModule==='commands',meta=moduleMeta[activeModule];
  text('moduleTitle',meta[0]);text('moduleHint',meta[1]);
  document.getElementById('datasetActions').classList.toggle('hidden',readOnly);
  const actions=moduleActions[activeModule];if(actions){text('addObjectButton',actions[0]);text('sendAllButton',actions[1])}
  const allRows=activeModule==='commands'?(state.commands||[]).map(command=>({id:command.id,body:command,delivery:command.status})):state.objects?.[activeModule]||[];
  if(allRows.length&&!allRows.some(record=>record.id===selectedID))selectedID=allRows[0].id;
  const stamp=record=>Date.parse(record.updated_at||record.body?.last_updated||record.body?.updated_at||record.body?.created_at||record.created_at)||0;
  const rows=allRows.filter(record=>(objectDelivery==='all'||record.delivery===objectDelivery)&&(!objectQuery||`${record.id} ${JSON.stringify(record.body)}`.toLowerCase().includes(objectQuery))).sort((a,b)=>objectSort==='name'?String(a.body.name||a.id).localeCompare(String(b.body.name||b.id)):objectSort==='oldest'?stamp(a)-stamp(b):stamp(b)-stamp(a)),root=document.getElementById('objectList');
  document.querySelectorAll('[data-module-count]').forEach(el=>el.textContent=el.dataset.moduleCount==='commands'?(state.commands||[]).length:(state.objects?.[el.dataset.moduleCount]||[]).length);
  root.innerHTML=rows.length?rows.map(record=>objectRowHTML(record)).join(''):`<div class="empty">${objectQuery||objectDelivery!=='all'?'ไม่พบรายการที่ตรงกับตัวกรอง':activeModule==='tokens'?'ยังไม่มี Token — ให้ Evolt Push เข้ามา หรือกด Load tokens from Evolt':readOnly?'ยังไม่มีข้อมูลที่ Evolt ส่งเข้ามา':'ยังไม่มีข้อมูล — สร้างรายการใหม่หรือโหลด Sample data'}</div>`;
  root.querySelectorAll('[data-record-id]').forEach(button=>button.addEventListener('click',()=>selectObject(button.dataset.recordId)));
  renderHelpers()
}
function objectRowHTML(record){
  const stage=recordStage(record);
  const row=`<button type="button" class="object-row ${record.id===selectedID?'selected':''} ${stage?'needs-action':''}" data-record-id="${attr(record.id)}" ${stage?`data-stage="${stage.kind}"`:""} aria-pressed="${record.id===selectedID}"><span class="object-row-content"><strong>${esc(record.body.name||record.id)}</strong>${record.body.name?`<small class="record-id">${esc(record.id)}</small>`:''}<small>${summary(record.body)}</small>${recordTimesHTML(record)}${stage?`<small class="session-stage" data-stage="${stage.kind}">${esc(stage.label)}</small>`:''}</span><span class="delivery ${esc(record.delivery)}" title="${esc(record.delivery)}">${esc(deliveryText(record.delivery))}</span></button>`;
  return row;
}
function summary(body){if(activeModule==='locations')return `${esc(body.name||'Unnamed')} · ${(body.evses||[]).length} EVSE`;if(activeModule==='tariffs')return `${esc(body.currency||'')} · ${(body.elements||[]).length} element`;if(activeModule==='sessions')return `${esc(body.status||'')} · ${body.kwh??0} kWh`;if(activeModule==='cdrs')return `${body.credit===true?'Credit CDR · ':''}${body.total_energy??0} kWh · ${body.total_time??0} h${body.session_id?` · Session ${esc(body.session_id)}`:''}${body.credit_reference_id?` · credits ${esc(body.credit_reference_id)}`:''}`;if(activeModule==='tokens')return `${esc(body.uid||'')} · ${esc(body.type||'RFID')} · ${body.valid?'valid':'invalid'}`;const sessionID=body.request?.session_id||body.result?.session_id;return `${esc(body.type||'')} · callback ${esc(body.status||'WAITING')}${sessionID?` · Session ${esc(sessionID)}`:''}`}
function partnerTime(value){const timestamp=Date.parse(value||'');return Number.isFinite(timestamp)?new Date(timestamp).toLocaleString('en-GB',{timeZone:'Asia/Bangkok',day:'2-digit',month:'2-digit',year:'numeric',hour:'2-digit',minute:'2-digit',second:'2-digit',hourCycle:'h23'}):'—'}
function recordTimesHTML(record,module=activeModule){
  const body=record.body||{};
  const stopped=module==='sessions'?stopCommandFor(record.id)?.result?.idle_started_at:null;
  const times=module==='commands'?[['รับคำสั่ง',body.created_at]]:['sessions','cdrs'].includes(module)?[['เริ่ม',body.start_date_time],...(stopped?[['หยุดชาร์จ',stopped]]:[]),['จบ',body.end_date_time],...(module==='cdrs'?[['อัปเดต CDR',body.last_updated]]:[])]:[];
  const valid=times.filter(([,value])=>Number.isFinite(Date.parse(value||'')));
  return valid.length?`<span class="record-times">${valid.map(([label,value])=>`<small>${label} <time datetime="${attr(value)}" title="${attr(value)}">${esc(partnerTime(value))}</time> · UTC+7</small>`).join('')}</span>`:'';
}
function recordStage(record,module=activeModule){
  if(!['sessions','commands','cdrs'].includes(module)||module==='cdrs'&&record.body.credit===true)return null;
  const sessionID=module==='sessions'?record.id:module==='commands'?record.body.request?.session_id||record.body.result?.session_id:record.body.session_id;
  const session=module==='sessions'?record:(state.objects?.sessions||[]).find(row=>row.id===sessionID);
  if(module==='cdrs')return record.delivery!=='SENT'?{kind:'cdr',label:'รอส่ง CDR',hint:'ตรวจยอดและส่ง CDR ไป Evolt'}:null;
  if(!session)return null;
  const stop=stopCommandFor(sessionID);
  const sentCDR=(state.objects?.cdrs||[]).some(row=>row.body.credit!==true&&(row.body.session_id===sessionID||row.id===`CDR-${sessionID}`)&&row.delivery==='SENT');
  if(session.body.status==='COMPLETED'&&!sentCDR)return {kind:'cdr',label:'Session จบแล้ว · รอส่ง CDR',hint:'ถอดปลั๊กแล้ว ตรวจหรือส่งยอด Session/CDR ต่อให้ครบ'};
  if(session.body.status!=='ACTIVE'||!stop)return null;
  if(stop.result?.idle_started_at&&!stop.result?.idle_finished_at)return {kind:'idle',label:'หยุดชาร์จแล้ว · รอถอดปลั๊ก / idle fee',hint:`หยุดเพิ่มพลังงานแล้ว กำลังนับเวลาจอด กดจำลองถอดปลั๊กเพื่อปิด Session${stop.result?.manual_cdr?' แล้วสร้าง CDR draft เอง':' และส่ง CDR'}`};
  return {kind:'stop',label:'ได้รับ STOP_SESSION · รอหยุดชาร์จ',hint:session.bills_parking===false?'กดหยุดชาร์จตาม STOP_SESSION เพื่อปิด Session · Tariff ไม่มีค่าจอดจึงไม่มี idle fee':'กดหยุดชาร์จตาม STOP_SESSION เพื่อเริ่มนับเวลา idle'};
}
function moduleHasDraftTool(){return activeModule==='sessions'}
function setEditorTab(name){editorTab=name;applyEditorTab()}
function applyEditorTab(){
  const tabs=document.getElementById('editorTabs'),draft=document.getElementById('draftPane'),automation=document.getElementById('automationPane'),tab=moduleHasDraftTool()?editorTab:'edit';
  tabs.hidden=!moduleHasDraftTool();
  document.getElementById('draftTabButton').classList.toggle('active',tab==='draft');
  document.getElementById('automationTabButton').classList.toggle('active',tab==='automation');
  document.getElementById('editTabButton').classList.toggle('active',tab==='edit');
  draft.hidden=tab!=='draft';
  automation.hidden=tab!=='automation';
  const record=currentRecord();
  document.getElementById('editorEmpty').classList.toggle('hidden',tab!=='edit'||!!record);
  document.getElementById('editorBody').classList.toggle('hidden',tab!=='edit'||!record);
}
function stopCommandFor(sessionID){return (state.commands||[]).filter(command=>command.type==='STOP_SESSION'&&command.request?.session_id===sessionID).sort((a,b)=>Date.parse(b.created_at||'')-Date.parse(a.created_at||''))[0]}
function sessionLifecycleHTML(record){
  if(!record)return '';
  const stage=recordStage(record,'sessions');
  const notice=stage?`<div class="session-attention" data-stage="${stage.kind}"><strong>${esc(stage.label)}</strong><p>${esc(stage.hint)}</p></div>`:'';
  const stop=stopCommandFor(record.id),idle=!!stop?.result?.idle_started_at;
  const commandAuto=(state.commands||[]).some(command=>command.type==='START_SESSION'&&command.result?.session_id===record.id&&!command.result?.automation_error)&&!!state.config?.auto_command_flow&&record.body.status==='ACTIVE'&&record.delivery==='SENT'&&!idle;
  const running=meterControl.run?.sessionID===record.id;
  const sent=record.delivery==='SENT'||running;
  const active=record.body.status==='ACTIVE';
  const current=Number(record.body.kwh)||0;
  const manual=meterManualTargets.get(record.id)??String(Number((current+1.25).toFixed(6)));
  const reason=record.delivery==='FAILED'?'คำขอก่อนหน้าล้มเหลว ให้ส่ง PUT ใหม่ก่อน':'Session นี้ยังเป็น draft และยังไม่มีใน Evolt';
  const manualMeter=meterControl.autoMode?`<div class="meter-controls"><label>อัตราการไหล (kWh/ชั่วโมง)<input id="meterRate" type="number" min="0.001" step="any" value="${attr(meterControl.rate)}" oninput="meterControl.rate=this.value" ${running?'disabled':''}></label><button type="button" class="${running?'ghost':''}" onclick="${running?'stopAutoMeter()':'startAutoMeter()'}" ${!running&&(!sent||!active||meterControl.busy)?'disabled':''}>${running?'Stop':'Start'}</button></div><p id="meterNextSend" class="meter-hint">${running?'ส่ง PATCH ครั้งถัดไปภายใน 15 วินาที':'กด Start เพื่อเริ่มวิ่ง และส่ง Session PATCH ทุก 15 วินาที'}</p>`:`<div class="meter-controls"><label>ค่า meter สะสมที่จะส่ง (kWh)<input id="meterManualTarget" type="number" min="${current}" step="any" value="${attr(manual)}" oninput="setManualMeterTarget(this.value)" ${!sent||!active?'disabled':''}></label><button type="button" onclick="sendManualMeter()" ${!sent||!active||meterControl.busy?'disabled':''}>ส่ง meter value</button><button type="button" class="ghost" onclick="sessionAction('advance')" ${!sent||!active||meterControl.busy?'disabled':''}>Update meter value (+1.25 kWh)</button></div><p class="meter-hint">กรอกค่าสะสมที่ต้องการส่ง หรือกดเพิ่มทีละ 1.25 kWh</p>`;
  const meterControls=idle?'<p class="meter-hint">Evolt ส่ง STOP_SESSION แล้ว · หยุดเพิ่มพลังงาน หัวยังเสียบอยู่</p>':commandAuto?`<p id="meterNextSend" class="meter-hint">Auto Command Flow กำลังเพิ่ม meter และส่ง PATCH ทุก 15 วินาที</p>`:`<label class="meter-mode"><input type="checkbox" ${meterControl.autoMode?'checked':''} onchange="setAutoMeterMode(this.checked,true)" ${!active?'disabled':''}> Auto meter สำหรับ Session ที่เลือก</label>${manualMeter}`;
  const preview=parkingPreview.sessionID===record.id?parkingPreview.data:null;
  const parking=idle&&active?`<div class="meter-card idle-card" data-stage="idle"><div class="lifecycle-head"><strong>Idle fee · รอถอดปลั๊ก</strong><span>หัวชาร์จยังไม่ว่าง</span></div><div class="meter-readout"><span>เวลาหลังหยุดชาร์จ</span><strong id="parkingElapsed">00:00:00</strong><small>หยุดชาร์จ ${esc(partnerTime(stop.result.idle_started_at))} · UTC+7</small></div><div class="meter-readout"><span>ค่าจอด/ค่าปรับ ณ ตอนนี้</span><strong id="parkingCost">${preview?esc(preview.priced?moneyAmount(preview.parking_cost?.incl_vat,record.body.currency):preview.note||'ไม่พบ Tariff'):'กำลังคำนวณ…'}</strong><small id="parkingTotal">${preview?.priced?`ยอดรวม ${esc(moneyAmount(preview.total_cost?.incl_vat,record.body.currency))}`:'คิดตาม PARKING_TIME ใน Tariff'}</small></div><button onclick="sessionAction('unplug')" ${meterControl.busy?'disabled':''}>จำลองตู้ตรวจพบถอดปลั๊ก · ${stop.result?.manual_cdr?'ปิด Session':'ปิด Session และส่ง CDR'}</button></div>`:'';
  const scheduled=idle&&active&&stop.result.auto_unplug_at?`<p class="idle-schedule" data-stage="idle">ตั้งถอดปลั๊กอัตโนมัติ ${esc(partnerTime(stop.result.auto_unplug_at))} · UTC+7<br><strong id="autoUnplugRemaining">กำลังนับถอยหลัง…</strong> · กดถอดปลั๊กเองก่อนเวลาก็ได้</p>`:'';
  const stopAction=stop&&active&&!idle?`<button onclick="sessionAction('stop')" ${!sent||meterControl.busy?'disabled':''}>หยุดชาร์จตาม STOP_SESSION · ${record.bills_parking===false?'ปิด Session (ไม่มีค่าจอด)':'เริ่มคิด idle fee'}</button>`:'';
  const manualCDR=!!stop?.result?.manual_cdr,pending=manualCDR?record.delivery!=='SENT':!stop?.result?.cdr_id;
  const retry=stop?.result?.idle_finished_at&&!active&&(pending||stop?.result?.evse_status_error)?`<button onclick="sessionAction('unplug')" ${meterControl.busy?'disabled':''}>${pending?manualCDR?'ส่ง Session ต่อหลังการถอดปลั๊ก':'ส่ง Session/CDR ต่อหลังการถอดปลั๊ก':'ส่งสถานะหัวชาร์จ AVAILABLE ซ้ำ'}</button>`:'';
  const cdrSent=(state.objects?.cdrs||[]).some(cdr=>cdr.id===`CDR-${record.id}`&&cdr.delivery==='SENT');
  return `<div class="lifecycle-head"><strong>วงจร Session</strong><span>${esc(record.body.status||'—')}</span></div>${notice}${!sent&&active?`<p class="lifecycle-note">${esc(reason)} — ส่ง initial PUT ด้านบนก่อน</p>`:''}<div class="meter-card"><div class="meter-readout"><span>ค่า meter ปัจจุบัน</span><strong id="meterLiveValue">${esc(meterReading(running?autoMeterValue():current))} kWh</strong><small id="meterSentValue">ส่งล่าสุด ${esc(meterReading(current))} kWh</small></div>${meterControls}</div>${parking}${scheduled}<div class="action-row">${stopAction}${retry}<button onclick="sessionAction('complete')" ${!sent||!active||meterControl.busy||commandAuto||!!stop?'disabled':''}>Complete session</button><button class="ghost" onclick="sessionAction('cdr')" ${!sent||active||meterControl.busy||cdrSent||!!retry?'disabled':''}>Create CDR draft</button></div>`;
}
function automationControlsHTML(config){
  const idle=!!config.test_idle_fee,scheduled=idle&&Number(config.idle_unplug_after_seconds)>0;
  return `<div class="helper-card"><div class="helper-heading"><h3>Auto Command Flow</h3><span>${config.auto_command_flow?'ON':'OFF'}</span></div><label class="meter-mode"><input id="autoCommandFlow" type="checkbox" ${config.auto_command_flow?'checked':''} onchange="setAutoCommandFlow(this.checked)"> รับ START_SESSION แล้วสร้าง/ส่ง Session, ส่งหัว CHARGING และเพิ่ม meter อัตโนมัติ</label><label class="meter-mode"><input id="autoTestIdleFee" type="checkbox" ${idle?'checked':''} onchange="saveIdleSettings()"> ทดสอบ idle fee หลัง STOP_SESSION</label><p class="meter-hint">${idle?'เมื่อ Auto รับ STOP: หยุด meter แต่ Session ยัง ACTIVE และหัวยัง CHARGING จนตรวจพบถอดปลั๊ก · ถ้า Tariff ของ Session ไม่มี PARKING_TIME จะจบ Session และส่ง CDR ทันที':'เมื่อ Auto รับ STOP: จบ Session, ส่งหัว AVAILABLE และส่ง CDR อัตโนมัติเหมือนเดิม'}</p><div class="idle-options"><label class="meter-mode"><input id="autoUnplugEnabled" type="checkbox" ${scheduled?'checked':''} ${!idle?'disabled':''} onchange="saveIdleSettings()"> ตั้งเวลาถอดปลั๊กอัตโนมัติ</label><div class="meter-controls"><label>รอหลังหยุดชาร์จ (วินาที)<input id="autoUnplugSeconds" type="number" min="1" max="86400" step="1" value="${attr(config.idle_unplug_after_seconds||60)}" ${!scheduled?'disabled':''}></label><button type="button" class="ghost" onclick="saveIdleSettings()" ${!scheduled?'disabled':''}>ตั้งเวลา</button></div><p class="meter-hint">ไม่ตั้งเวลา = กดถอดปลั๊กเอง · ตั้งเวลาแล้ว server จะถอดปลั๊กจำลองและส่ง Session/CDR แม้ปิดหน้านี้ · ค่าใหม่ใช้กับ STOP ครั้งถัดไป</p><label class="meter-mode"><input id="autoParkingAtUnplug" type="checkbox" ${config.parking_at_unplug_only?'checked':''} ${!idle?'disabled':''} onchange="saveIdleSettings()"> ไม่ส่งค่าจอดระหว่างรอ ส่งครั้งเดียวตอนถอดปลั๊ก</label><p class="meter-hint">ไม่ติ๊ก = ส่ง PARKING_TIME และยอดค่าจอดให้ Evolt ทุกรอบ meter · ติ๊ก = เงียบจนถอดปลั๊ก (partner บางเจ้าทำแบบนี้ ใช้เทสว่า Evolt รับได้)</p></div><div class="meter-controls"><label>อัตรา meter (kWh/ชั่วโมง)<input id="autoCommandRate" type="number" min="0.001" max="350" step="any" value="${attr(config.auto_meter_rate_kwh||7.2)}" ${config.auto_command_flow?'disabled':''}></label><label>ส่ง Charging Period ทุก (วินาที)<input id="autoPeriodSeconds" type="number" min="15" max="1800" step="1" value="${attr(config.period_interval_seconds||15)}" onchange="saveIdleSettings()"></label></div><p class="meter-hint">ระหว่างรอบ meter ส่งแค่ kwh และ total_cost · 15 = ส่ง period ทุกรอบ · Period ถูกตัดตรงจุดที่ราคาเปลี่ยนเสมอ</p><p class="hint">ตั้งค่าก่อน Evolt ส่ง START/STOP · ปิด Auto Command Flow เพื่อสร้าง Session และกรอก meter ด้วยมือ</p></div>`;
}
function renderHelpers(){
  const root=document.getElementById('helperPanel');
  root.innerHTML='';
  document.getElementById('draftPane').innerHTML='';
  document.getElementById('automationPane').innerHTML='';
  document.getElementById('lifecyclePane').innerHTML='';
  if(activeModule==='locations'){
    const locations=state.objects.locations||[];
    root.innerHTML=`<div class="helper-card"><div class="helper-heading"><h3>Update EVSE status</h3><span>Send PATCH to Evolt</span></div><div class="helper-grid"><select id="statusLocation" aria-label="Location" onchange="fillStatusEVSE()">${locations.map(x=>`<option ${x.id===selectedID?'selected':''}>${esc(x.id)}</option>`).join('')}</select><select id="statusEVSE" aria-label="EVSE"></select><select id="statusValue" aria-label="Status"><option>AVAILABLE</option><option>CHARGING</option><option>OUTOFORDER</option><option>INOPERATIVE</option><option>REMOVED</option></select><button onclick="patchEVSEStatus()">Send status update</button></div></div>`;
    fillStatusEVSE();
    return;
  }
  if(activeModule==='sessions'){
    const automation=state.config||{};
    document.getElementById('automationPane').innerHTML=automationControlsHTML(automation);
    const locations=state.objects.locations||[];
    const startCommands=(state.commands||[]).filter(x=>x.type==='START_SESSION'&&!x.result?.session_id);
    const fromCommand=startCommands.length
      ?`<div class="helper-alt"><span>หรือสร้างจาก START_SESSION ที่ Evolt ส่งมา — ดึง token / location / EVSE จาก command นั้น</span><div class="helper-grid"><select id="helperCommand" aria-label="START_SESSION command" onchange="fillCommandConnectorOptions()">${startCommands.map(x=>`<option value="${esc(x.id)}">${esc(String(x.id).slice(0,8))} · ${esc(partnerTime(x.created_at))} UTC+7 · ${esc(x.request?.location_id||'?')}/${esc(x.request?.evse_uid||'?')}/${esc(x.request?.connector_id||'?')} · ${esc(x.request?.token?.uid||'no token')}</option>`).join('')}</select><select id="helperCommandConnector" aria-label="Connector for this command"></select><button id="createFromCommandButton" onclick="createSessionFromCommand()">${meterControl.autoMode?'Create, send & start':'Create draft from command'}</button></div><p class="hint" id="helperCommandHint"></p></div>`
      :`<div class="helper-alt"><span>ยังไม่มี START_SESSION จาก Evolt — ใช้การเลือกเองด้านบน</span></div>`;
    const createLabel=meterControl.autoMode?'Create, send & start':'Create draft';
    const precreateMeter=`<div class="meter-card"><div class="lifecycle-head"><strong>วงจร Session</strong><span>ก่อนสร้าง Session</span></div><label class="meter-mode"><input type="checkbox" ${meterControl.autoMode?'checked':''} onchange="setAutoMeterMode(this.checked)"> ให้ meter วิ่งอัตโนมัติหลังสร้าง Session</label><div class="meter-controls"><label>อัตราการไหล (kWh/ชั่วโมง)<input id="meterDraftRate" type="number" min="0.001" step="any" value="${attr(meterControl.rate)}" oninput="meterControl.rate=this.value" ${!meterControl.autoMode?'disabled':''}></label></div><p class="meter-hint">เมื่อเปิดโหมด Create จะส่ง initial PUT ไป Evolt แล้วเริ่ม meter; ทุก 15 วินาทีจึงส่ง Session PATCH ขณะเปิดหน้านี้ไว้ ถ้าปิดไว้จะเป็น draft ให้ส่งและกรอกค่าเองเหมือนเดิม</p></div>`;
    document.getElementById('draftPane').innerHTML=`<div class="helper-card"><div class="helper-heading"><h3>Create session draft</h3><span>STEP 1</span></div><div class="helper-grid"><select id="helperLocation" aria-label="Location" onchange="fillConnectorOptions()">${locations.map(x=>`<option>${esc(x.id)}</option>`).join('')}</select><select id="helperEVSE" aria-label="EVSE"></select><select id="helperConnector" aria-label="Connector"></select></div>${precreateMeter}<div class="action-row"><button id="createSessionButton" onclick="createSession()">${createLabel}</button></div>${fromCommand}</div>`;
    document.getElementById('lifecyclePane').innerHTML=sessionLifecycleHTML(currentRecord());
    fillConnectorOptions();
    fillCommandConnectorOptions();
    return;
  }
  if(activeModule==='cdrs'){
    const record=currentRecord(),pane=document.getElementById('lifecyclePane');
    if(!record)return;
    const sent=record.delivery==='SENT',credit=record.body.credit===true;
    const related=(state.objects.cdrs||[]).find(row=>row.body.credit===true&&row.body.credit_reference_id===record.id);
    const actions=[];
    if(credit&&sent)actions.push(`<button data-cdr-action="replacement-draft" data-cdr-source="${attr(record.id)}">Create new CDR draft</button>`);
    else if(!credit&&sent&&!related)actions.push(`<button data-cdr-action="credit-draft" data-cdr-source="${attr(record.id)}">Create Credit CDR draft</button>`);
    if(!credit&&related)actions.push(`<button class="ghost" data-cdr-select="${attr(related.id)}">Open Credit CDR (${esc(deliveryText(related.delivery))})</button>`);
    if(credit)actions.push(`<button class="ghost" data-cdr-select="${attr(record.body.credit_reference_id)}">Open original CDR</button>`);
    const note=credit?sent?'Credit CDR sent. Create a new ordinary CDR with a new ID, edit its values, then send it. OCPI has no direct replacement reference field.':`Credit CDR references ${esc(record.body.credit_reference_id||'—')} and reverses its total_cost. Send this before creating the new CDR.`:sent?'This CDR is sealed. Send a Credit CDR to reverse it before sending corrected values in a new CDR.':'Send this CDR first. Correction is available after Evolt accepts it.';
    pane.innerHTML=`<div class="lifecycle-head"><strong>CDR correction</strong><span>${credit?'Credit CDR':'Ordinary CDR'}</span></div><p class="lifecycle-note">${note}</p>${actions.length?`<div class="action-row">${actions.join('')}</div>`:''}`;
    pane.querySelector('[data-cdr-action]')?.addEventListener('click',event=>createCDRCorrection(event.currentTarget.dataset.cdrAction,event.currentTarget.dataset.cdrSource));
    pane.querySelectorAll('[data-cdr-select]').forEach(button=>button.addEventListener('click',event=>selectObject(event.currentTarget.dataset.cdrSelect)));
    return;
  }
  if(activeModule==='tokens'){
    root.innerHTML=`<div class="helper-card"><div class="helper-heading"><h3>Sync tokens from Evolt</h3><span>GET</span></div><button onclick="pullTokens()">Load tokens</button></div>`;
  }
  else root.innerHTML='';
}
function fillStatusEVSE(){const loc=(state.objects.locations||[]).find(x=>x.id===value('statusLocation'));const select=document.getElementById('statusEVSE');if(select)select.innerHTML=(loc?.body.evses||[]).map(x=>`<option>${esc(x.uid)}</option>`).join('')}
function fillConnectorOptions(){const loc=(state.objects.locations||[]).find(x=>x.id===value('helperLocation'));const evse=document.getElementById('helperEVSE'),connector=document.getElementById('helperConnector');if(!evse||!loc)return;evse.innerHTML=(loc.body.evses||[]).map(x=>`<option>${esc(x.uid)}</option>`).join('');const update=()=>{const selected=(loc.body.evses||[]).find(x=>x.uid===evse.value);connector.innerHTML=(selected?.connectors||[]).map(x=>`<option>${esc(x.id)}</option>`).join('')};evse.onchange=update;update()}
// A START_SESSION carries connector_id only when the EVSE declares START_SESSION_CONNECTOR_REQUIRED
// (OCPI 2.2.1 §13.2), so a multi-connector EVSE needs the tester to say which head to draft against.
function fillCommandConnectorOptions(){
  const select=document.getElementById('helperCommandConnector'),hint=document.getElementById('helperCommandHint');
  if(!select)return;
  const command=(state.commands||[]).find(x=>x.id===value('helperCommand'));
  const loc=(state.objects.locations||[]).find(x=>x.id===command?.request?.location_id);
  const evse=(loc?.body.evses||[]).find(x=>x.uid===command?.request?.evse_uid);
  const sent=command?.request?.connector_id;
  const connectors=evse?.connectors||[];
  select.innerHTML=connectors.map(x=>`<option ${String(x.id)===String(sent)?'selected':''}>${esc(x.id)}</option>`).join('');
  select.disabled=!!sent||connectors.length<2;
  hint.textContent=!loc?`Location ${command?.request?.location_id||'?'} ยังไม่มีใน catalogue ของ partner`
    :!evse?`EVSE ${command?.request?.evse_uid||'?'} ไม่มีใน ${loc.id}`
    :sent?`Evolt ส่ง connector_id ${sent} มาแล้ว`
    :connectors.length===1?`EVSE นี้มีหัวเดียว (${connectors[0].id}) — เลือกให้อัตโนมัติ`
    :`Evolt ไม่ได้ส่ง connector_id มา — เลือกหัวที่จะใช้เอง`;
}

function renderEditor(){
  const record=currentRecord(),empty=document.getElementById('editorEmpty'),body=document.getElementById('editorBody'),editor=document.getElementById('jsonEditor');
  const metadata=document.getElementById('editorRecordMeta');
  if(metadata){const stage=record?recordStage(record):null;metadata.innerHTML=record?recordTimesHTML(record)+(stage?`<span class="session-stage" data-stage="${stage.kind}">${esc(stage.label)}</span>`:''):'';metadata.hidden=!metadata.innerHTML}
  empty.classList.toggle('hidden',!!record);body.classList.toggle('hidden',!record);
  document.getElementById('lifecyclePane').hidden=!['sessions','cdrs'].includes(activeModule)||!record;
  if(!record){text('editorTitle','Select a record');text('deliveryBadge','—');applyEditorTab();return}
  const cdrSealed=activeModule==='cdrs'&&record.delivery==='SENT',cdrCredit=activeModule==='cdrs'&&record.body.credit===true,readOnly=['tokens','commands'].includes(activeModule)||cdrSealed||cdrCredit,key=draftKey(),dirty=editorDrafts.has(key);document.getElementById('objectPrefillMount').hidden=readOnly;
  text('editorTitle',record.body.name||record.id);text('deliveryBadge',deliveryText(record.delivery));
  document.getElementById('deliveryBadge').className='delivery '+record.delivery;
  document.getElementById('draftState').hidden=!dirty;
  if(editor.dataset.record===key&&dirty&&document.getElementById('editorBody').contains(document.activeElement))return;
  editor.dataset.record=key;editor.value=editorDrafts.get(key)??JSON.stringify(record.body,null,2);editor.readOnly=readOnly;
  document.getElementById('editorMode').classList.toggle('hidden',readOnly);
  document.getElementById('quickForm').classList.toggle('hidden',readOnly);
  editor.classList.remove('hidden');
  document.getElementById('editorActions').classList.toggle('hidden',readOnly);
  document.getElementById('sendActions').classList.toggle('hidden',['tokens','commands'].includes(activeModule));
  document.getElementById('sendActionsNote').classList.toggle('hidden',readOnly);
  document.getElementById('sendObjectButton').classList.toggle('hidden',cdrSealed);
  document.getElementById('rawRequestButton').classList.toggle('hidden',readOnly);
  document.getElementById('deleteRemoteButton').classList.toggle('hidden',activeModule!=='tariffs');
  if(!readOnly){renderFindings(record.findings||[]);setEditorMode(editorMode)}
  else renderFindings([{level:'info',rule:cdrSealed?'Sealed CDR':cdrCredit?'Credit CDR':'Received from Evolt',message:cdrSealed?'Sent CDRs cannot be edited or resent':cdrCredit?'The credit copy mirrors the original except for its ID, credit fields, and negated total_cost':'Read-only evidence'}]);
  applyEditorTab();
}
function renderQuickForm(body){document.getElementById('quickForm').innerHTML=(formFields[activeModule]||[]).map(([key,label,required])=>`<label class="${required?'required':''}">${label}<input data-field="${key}" value="${attr(body[key]??'')}" oninput="quickFieldChanged('${key}',this.value)"></label>`).join('')+`<p class="hint">Nested OCPI structures (EVSE, Connector, elements, charging periods) can be edited in Raw JSON.</p>`}
function quickFieldChanged(key,value){try{const body=editorObject();body[key]=['kwh','total_energy','total_time'].includes(key)?Number(value):value;document.getElementById('jsonEditor').value=JSON.stringify(body,null,2);captureEditorDraft()}catch(error){toast(error.message,true)}}
function setEditorMode(mode){if(mode==='form'){try{renderQuickForm(editorObject())}catch{mode='json';toast('Fix invalid JSON before opening Form',true)}}editorMode=mode;document.getElementById('quickForm').classList.toggle('hidden',mode!=='form');document.getElementById('jsonEditor').classList.toggle('hidden',mode!=='json');document.getElementById('formModeButton').classList.toggle('active',mode==='form');document.getElementById('jsonModeButton').classList.toggle('active',mode==='json')}
function editorObject(){try{return JSON.parse(document.getElementById('jsonEditor').value)}catch(error){throw new Error(`Invalid JSON: ${error.message}`)}}
async function validateCurrent(){try{const body=editorObject();const result=await api(`/api/cpo/validate/${activeModule}`,{method:'POST',body:JSON.stringify(body)});renderFindings(result.findings);toast(result.valid?'Object passes implemented spec rules':`${result.findings.length} finding(s)`,!result.valid)}catch(error){toast(error.message,true)}}
async function persistCurrent(){const body=editorObject(),id=String(body.id||selectedID),key=draftKey(),raw=document.getElementById('jsonEditor').value;if(!id)throw new Error('Object id is required');await api(`/api/cpo/objects/${activeModule}/${encodeURIComponent(id)}`,{method:'PUT',body:JSON.stringify(body)});if(editorDrafts.get(key)===raw)editorDrafts.delete(key);newDrafts.delete(key);selectedID=id}
async function saveCurrent(){try{await persistCurrent();toast('Draft saved in Partner PostgreSQL');await refresh()}catch(error){toast(error.message,true);throw error}}
async function deleteCurrent(){if(!confirm(`Delete ${activeModule}/${selectedID} from Partner?`))return;const key=draftKey(),id=selectedID;try{if(!newDrafts.has(key))await api(`/api/cpo/objects/${activeModule}/${encodeURIComponent(id)}`,{method:'DELETE'});newDrafts.delete(key);editorDrafts.delete(key);state.objects[activeModule]=(state.objects[activeModule]||[]).filter(record=>record.id!==id);selectedID='';toast('Partner object deleted');await refresh()}catch(error){toast(error.message,true)}}
function renderFindings(findings){document.getElementById('findings').innerHTML=findings.length?findings.map(f=>`<div class="finding ${esc(f.level)}"><strong>${esc(f.rule)}</strong><br>${esc(f.message)}</div>`).join(''):'<div class="finding info">No findings from implemented rules</div>'}

function defaultMethod(){return activeModule==='cdrs'?'POST':'PUT'}
async function previewSend(method){try{if(method!=='GET'&&!(activeModule==='cdrs'&&currentRecord()?.body.credit===true)){await persistCurrent();await refresh()}const input={module:activeModule,id:selectedID,method};const result=await api('/api/cpo/actions/preview',{method:'POST',body:JSON.stringify(input)});pendingPreview={input,result};text('previewTitle',`${result.method} ${activeModule}/${selectedID}`);text('previewContent',JSON.stringify({method:result.method,url:result.url,headers:result.headers,body:result.body,curl:result.curl},null,2));text('confirmPreviewButton',method==='GET'?'Run GET verification':method==='DELETE'?'Delete from Evolt':'Send to Evolt');const modal=document.getElementById('previewModal');modal.classList.remove('hidden');document.body.classList.add('modal-open');modal.querySelector('.modal-card').focus()}catch(error){toast(error.message,true)}}
async function confirmSend(){if(!pendingPreview)return;const method=pendingPreview.input.method;try{const result=await api('/api/cpo/actions/send',{method:'POST',body:JSON.stringify(pendingPreview.input)});closePreview();toast(result.evse_status_error?`Session sent, but EVSE status PATCH failed: ${result.evse_status_error}`:method==='GET'?'GET verification completed':method==='DELETE'?'Object deleted from Evolt':'Evolt accepted the OCPI request',!!result.evse_status_error);await refresh()}catch(error){toast(error.message,true);await refresh()}}
function closePreview(){document.getElementById('previewModal').classList.add('hidden');document.body.classList.remove('modal-open');pendingPreview=null}
function closePreviewFromBackdrop(event){if(event.target===event.currentTarget)closePreview()}
async function copyPreviewCurl(){if(!pendingPreview)return;navigator.clipboard.writeText(pendingPreview.result.curl);toast('cURL copied with complete headers')}
async function sendAll(){const rows=(state.objects?.[activeModule]||[]).filter(row=>activeModule!=='cdrs'||row.delivery!=='SENT');if(!rows.length)return toast('No unsent objects',true);for(const row of rows){try{await api('/api/cpo/actions/send',{method:'POST',body:JSON.stringify({module:activeModule,id:row.id,method:defaultMethod()})})}catch(error){toast(`${row.id}: ${error.message}`,true);await refresh();return}}toast(`Sent ${rows.length} ${activeModule}`);await refresh()}

async function createCDRCorrection(action,id){try{const result=await api(`/api/cpo/cdrs/${encodeURIComponent(id)}/${action}`,{method:'POST',body:'{}'});activeModule='cdrs';selectedID=result.id;editorTab='edit';toast(action==='credit-draft'?'Credit CDR draft created; review and send it first':'Replacement draft created; edit values before sending');await refresh();activateCurrentTab()}catch(error){toast(error.message,true)}}

async function patchEVSEStatus(){try{await api('/api/cpo/actions/evse-status',{method:'POST',body:JSON.stringify({location_id:value('statusLocation'),evse_uid:value('statusEVSE'),status:value('statusValue')})});toast('EVSE status PATCH accepted');await refresh()}catch(error){toast(error.message,true)}}
async function createSession(){
  await createSessionDraft({location_id:value('helperLocation'),evse_uid:value('helperEVSE'),connector_id:value('helperConnector')});
}
async function createSessionFromCommand(){const commandID=value('helperCommand');if(!commandID)return toast('Select a START_SESSION command first',true);await createSessionDraft({command_id:commandID,connector_id:value('helperCommandConnector')})}
async function createSessionDraft(input){
  const auto=meterControl.autoMode,rate=Number(meterControl.rate);
  if(meterControl.run)return toast('จบหรือหยุด Auto meter ของ Session ปัจจุบันก่อนสร้าง Session ใหม่',true);
  if(auto&&(!Number.isFinite(rate)||rate<0.001))return toast('อัตราการไหลต้องมากกว่าหรือเท่ากับ 0.001 kWh/ชั่วโมง',true);
  let result,sendResult;
  try{result=await api('/api/cpo/actions/session/create',{method:'POST',body:JSON.stringify(input)})}
  catch(error){toast(error.message,true);return}
  activeModule='sessions';selectedID=result.object.id;editorTab='edit';
  if(auto){
    try{sendResult=await api('/api/cpo/actions/send',{method:'POST',body:JSON.stringify({module:'sessions',id:selectedID,method:'PUT'})})}
    catch(error){await refresh();activateCurrentTab();toast(`สร้าง Session draft แล้ว แต่ส่ง initial PUT ไม่สำเร็จ: ${error.message}`,true);return}
  }
  await refresh();activateCurrentTab();
  if(auto){
    if(currentRecord()?.delivery!=='SENT')return toast('สร้าง Session แล้ว แต่ยังไม่ได้รับสถานะส่ง initial PUT สำเร็จ จึงยังไม่เริ่ม meter',true);
    startAutoMeter();
  }
  toast(sendResult?.evse_status_error?`Session sent, but EVSE status PATCH failed: ${sendResult.evse_status_error}`:priceToast(auto?'Session sent; Auto meter started':'Session draft created',result.price),!!sendResult?.evse_status_error);
}
function priceToast(message,price){if(!price)return message;if(!price.priced)return `${message} · ยังไม่มีราคา: ${price.note||'ไม่พบ tariff'}`;return `${message} · tariff ${price.tariff_id}`}
function meterReading(kwh){return Number(kwh).toLocaleString('en-US',{minimumFractionDigits:3,maximumFractionDigits:6})}
function moneyAmount(value,currency){return `${Number(value||0).toLocaleString('th-TH',{minimumFractionDigits:2,maximumFractionDigits:2})} ${currency||'THB'}`}
async function tickParking(){
  if(document.hidden)return;
  const record=activeModule==='sessions'?currentRecord():null,stop=record?stopCommandFor(record.id):null;
  if(!record||record.body.status!=='ACTIVE'||!stop?.result?.idle_started_at)return;
  const elapsed=Math.max(0,Math.floor((Date.now()-Date.parse(stop.result.idle_started_at))/1000));
  const clock=document.getElementById('parkingElapsed');
  const countdown=document.getElementById('autoUnplugRemaining');
  if(countdown&&stop.result.auto_unplug_at){const remaining=Math.max(0,Math.ceil((Date.parse(stop.result.auto_unplug_at)-Date.now())/1000));countdown.textContent=remaining>0?`เหลือ ${remaining} วินาที`:'กำลังปิด Session และส่ง CDR…'}
  if(clock)clock.textContent=`${String(Math.floor(elapsed/3600)).padStart(2,'0')}:${String(Math.floor(elapsed%3600/60)).padStart(2,'0')}:${String(elapsed%60).padStart(2,'0')}`;
  if(parkingPreview.sessionID!==record.id){parkingPreview.data=null;parkingPreview.lastFetch=0}
  if(parkingPreview.pending||parkingPreview.sessionID===record.id&&Date.now()-parkingPreview.lastFetch<3000)return;
  parkingPreview.pending=true;parkingPreview.sessionID=record.id;parkingPreview.lastFetch=Date.now();
  try{const data=await api(`/api/cpo/actions/session/${encodeURIComponent(record.id)}/parking`);parkingPreview.data=data;if(activeModule==='sessions'&&selectedID===record.id){const cost=document.getElementById('parkingCost'),total=document.getElementById('parkingTotal');if(cost)cost.textContent=data.priced?moneyAmount(data.parking_cost?.incl_vat,record.body.currency):data.note||'ไม่พบ Tariff';if(total)total.textContent=data.priced?`ยอดรวม ${moneyAmount(data.total_cost?.incl_vat,record.body.currency)}`:'ยังคำนวณยอดรวมไม่ได้'}}
  catch(error){if(activeModule==='sessions'&&selectedID===record.id){const cost=document.getElementById('parkingCost');if(cost)cost.textContent=error.message}}
  finally{parkingPreview.pending=false}
}
async function setAutoCommandFlow(enabled){
  const rate=Number(document.getElementById('autoCommandRate')?.value||state.config?.auto_meter_rate_kwh||7.2);
  const idle=document.getElementById('autoTestIdleFee')?.checked??!!state.config?.test_idle_fee;
  const scheduled=document.getElementById('autoUnplugEnabled')?.checked??(Number(state.config?.idle_unplug_after_seconds)>0);
  const seconds=idle&&scheduled?Number(document.getElementById('autoUnplugSeconds')?.value??state.config?.idle_unplug_after_seconds??60):0;
  if(idle&&scheduled&&(!Number.isInteger(seconds)||seconds<1||seconds>86400))return toast('ตั้งเวลารอถอดปลั๊กตั้งแต่ 1 ถึง 86400 วินาที',true);
  const parkingAtUnplug=document.getElementById('autoParkingAtUnplug')?.checked??!!state.config?.parking_at_unplug_only;
  const period=Number(document.getElementById('autoPeriodSeconds')?.value||state.config?.period_interval_seconds||15);
  if(!Number.isInteger(period)||period<15||period>1800)return toast('ส่ง Charging Period ได้ทุก 15 ถึง 1800 วินาที',true);
  try{await api('/api/cpo/automation',{method:'PUT',body:JSON.stringify({enabled,rate_kwh_per_hour:rate,test_idle_fee:idle,idle_unplug_after_seconds:seconds,parking_at_unplug_only:parkingAtUnplug,period_interval_seconds:period})});if(enabled)stopAutoMeter(false);await refresh();toast(`บันทึก Auto ${enabled?'ON':'OFF'} · ${idle?scheduled?`ทดสอบ idle ${seconds} วินาที`:'ทดสอบ idle · ถอดปลั๊กเอง':'จบ Session/CDR ทันทีหลัง STOP'}${idle&&parkingAtUnplug?' · ค่าจอดส่งตอนถอดปลั๊ก':''} · period ทุก ${period} วินาที`)}catch(error){toast(error.message,true);await refresh()}
}
async function saveIdleSettings(){return setAutoCommandFlow(document.getElementById('autoCommandFlow')?.checked??!!state.config?.auto_command_flow)}
function setManualMeterTarget(value){meterManualTargets.set(selectedID,value)}
function setAutoMeterMode(enabled,start=false){if(!enabled)stopAutoMeter(false);meterControl.autoMode=enabled;const draftRate=document.getElementById('meterDraftRate');if(draftRate)draftRate.disabled=!enabled;const createButton=document.getElementById('createSessionButton');if(createButton)createButton.textContent=enabled?'Create, send & start':'Create draft';const commandButton=document.getElementById('createFromCommandButton');if(commandButton)commandButton.textContent=enabled?'Create, send & start':'Create draft from command';document.getElementById('lifecyclePane').innerHTML=sessionLifecycleHTML(currentRecord());const record=currentRecord();if(enabled&&start&&record?.delivery==='SENT'&&record.body.status==='ACTIVE'&&!stopCommandFor(record.id)?.result?.idle_started_at&&!meterControl.busy)return startAutoMeter();tickAutoMeter()}
function autoMeterValue(at=Date.now()){const run=meterControl.run;return run?run.baseKwh+run.rate*Math.max(0,at-run.startedAt)/3600000:0}
function stopAutoMeter(render=true){if(meterControl.timer)clearInterval(meterControl.timer);meterControl.timer=null;meterControl.run=null;if(render&&activeModule==='sessions'&&currentRecord())document.getElementById('lifecyclePane').innerHTML=sessionLifecycleHTML(currentRecord())}
function startAutoMeter(){
  const record=currentRecord(),rate=Number(meterControl.rate);
  if(!record||record.delivery!=='SENT'||record.body.status!=='ACTIVE')return toast('ส่ง initial PUT ของ Active Session ก่อน',true);
  if(!Number.isFinite(rate)||rate<0.001)return toast('อัตราการไหลต้องมากกว่าหรือเท่ากับ 0.001 kWh/ชั่วโมง',true);
  if(meterControl.busy)return;
  stopAutoMeter(false);
  const now=Date.now();meterControl.run={sessionID:record.id,baseKwh:Number(record.body.kwh)||0,rate,startedAt:now,nextSendAt:now+15000};
  meterControl.timer=setInterval(tickAutoMeter,250);
  document.getElementById('lifecyclePane').innerHTML=sessionLifecycleHTML(record);
  tickAutoMeter();
}
function tickAutoMeter(){
  const run=meterControl.run;if(!run)return;
  const now=Date.now(),live=document.getElementById('meterLiveValue'),next=document.getElementById('meterNextSend');
  if(activeModule==='sessions'&&selectedID===run.sessionID){if(live)live.textContent=`${meterReading(autoMeterValue(now))} kWh`;if(next)next.textContent=meterControl.busy?'กำลังส่ง Session PATCH…':`ส่งครั้งถัดไปใน ${Math.max(0,Math.ceil((run.nextSendAt-now)/1000))} วินาที`}
  if(now<run.nextSendAt||meterControl.busy)return;
  run.nextSendAt=Math.max(run.nextSendAt+15000,now+15000);
  void sendAutoMeter(run,now);
}
function tickCommandMeter(){
  if(activeModule!=='sessions'||!state.config?.auto_command_flow)return;
  const record=currentRecord(),command=(state.commands||[]).find(item=>item.type==='START_SESSION'&&item.result?.session_id===record?.id);
  if(!record||!command||command.result?.automation_error||stopCommandFor(record.id)?.result?.idle_started_at||record.body.status!=='ACTIVE'||record.delivery!=='SENT')return;
  const live=document.getElementById('meterLiveValue');if(!live)return;
  const elapsed=Math.max(0,Date.now()-Date.parse(record.body.last_updated||''));
  const reading=(Number(record.body.kwh)||0)+(Number(state.config.auto_meter_rate_kwh)||7.2)*elapsed/3600000;
  live.textContent=`${meterReading(reading)} kWh`;
}
async function sendAutoMeter(run,at){
  meterControl.busy=true;
  const target=Math.round(autoMeterValue(at)*1e6)/1e6;
  try{
    await api('/api/cpo/actions/session/advance',{method:'POST',body:JSON.stringify({session_id:run.sessionID,target_kwh:target})});
    if(meterControl.run===run)await refresh();
  }catch(error){if(meterControl.run===run){stopAutoMeter(false);toast(`หยุด Auto meter: ${error.message}`,true);await refresh()}}
  finally{meterControl.busy=false;if(activeModule==='sessions'&&currentRecord())document.getElementById('lifecyclePane').innerHTML=sessionLifecycleHTML(currentRecord());if(meterControl.run===run)tickAutoMeter()}
}
async function sendManualMeter(){
  const record=currentRecord();if(!record||meterControl.busy)return;
  const raw=document.getElementById('meterManualTarget')?.value,target=Number(raw),current=Number(record.body.kwh)||0;
  if(raw===''||!Number.isFinite(target)||target<current)return toast(`กรอกค่า meter สะสมตั้งแต่ ${meterReading(current)} kWh ขึ้นไป`,true);
  meterControl.busy=true;
  try{await api('/api/cpo/actions/session/advance',{method:'POST',body:JSON.stringify({session_id:record.id,target_kwh:target})});meterManualTargets.delete(record.id);toast(`ส่งค่า meter ${meterReading(target)} kWh แล้ว`);await refresh()}
  catch(error){toast(error.message,true);await refresh()}
  finally{meterControl.busy=false;if(activeModule==='sessions'&&currentRecord())document.getElementById('lifecyclePane').innerHTML=sessionLifecycleHTML(currentRecord())}
}
async function sessionAction(action){
  if(!selectedID)return toast('Select a Session first',true);
  if(meterControl.busy)return toast('รอให้ส่ง meter value ครั้งก่อนเสร็จก่อน',true);
  const input={session_id:selectedID};
  if((action==='complete'||action==='stop')&&meterControl.run?.sessionID===selectedID){input.target_kwh=Math.round(autoMeterValue()*1e6)/1e6;stopAutoMeter(false)}
  meterControl.busy=true;
  try{const result=await api(`/api/cpo/actions/session/${action}`,{method:'POST',body:JSON.stringify(input)});if(action==='cdr'){activeModule='cdrs';selectedID=result.object.id}toast(result.evse_status_error?`${result.manual_cdr?'ปิด Session แล้ว':'ส่ง Session/CDR แล้ว'} แต่สถานะหัวชาร์จ PATCH ไม่สำเร็จ: ${result.evse_status_error}`:action==='cdr'?'CDR draft created':action==='advance'?'เพิ่ม meter 1.25 kWh แล้ว':action==='stop'?result.completed?'หยุดชาร์จแล้ว · ปิด Session แล้ว · กด Create CDR draft เพื่อส่ง CDR เอง':'หยุดชาร์จแล้ว · เริ่มคิด idle fee':action==='unplug'?`ถอดปลั๊กแล้ว · ${result.cdr_id?`CDR ${result.cdr_id}`:'ปิด Session แล้ว'} · ยอดรวม ${moneyAmount(result.price?.incl_vat,currentRecord()?.body?.currency)}${result.cdr_id?'':' · กด Create CDR draft เพื่อส่ง CDR เอง'}`:`Session ${action} PATCH accepted`,!!result.evse_status_error);await refresh();activateCurrentTab()}
  catch(error){toast(error.message,true);await refresh()}
  finally{meterControl.busy=false;if(activeModule==='sessions'&&currentRecord())document.getElementById('lifecyclePane').innerHTML=sessionLifecycleHTML(currentRecord())}
}
function activateCurrentTab(){document.querySelectorAll('.module-tabs button').forEach(button=>button.classList.toggle('active',button.dataset.module===activeModule));renderObjects();renderEditor()}

function createObjectDraft(id,prefill=qaPrefillEnabled('op')){
  if(!id)return;
  if((state.objects[activeModule]||[]).some(record=>record.id===id)){toast('This ID already exists. Choose another ID.',true);return false}
  const catalogue=['locations','tariffs'].includes(activeModule);
  const body={country_code:catalogue?state.config?.country_code||'':'',party_id:catalogue?state.config?.party_id||'':'',id,last_updated:catalogue?new Date().toISOString():''};
  if(activeModule==='locations')Object.assign(body,{publish:true,address:'',city:'',country:'',coordinates:{latitude:'',longitude:''},time_zone:'',evses:[]});
  if(activeModule==='tariffs')Object.assign(body,{currency:'',elements:[]});
  if(activeModule==='sessions')Object.assign(body,{start_date_time:'',kwh:0,cdr_token:{country_code:'',party_id:'',uid:'',type:'',contract_id:''},auth_method:'',location_id:'',evse_uid:'',connector_id:'',currency:'',status:'ACTIVE'});
  if(activeModule==='cdrs')Object.assign(body,{session_id:'',start_date_time:'',end_date_time:'',cdr_token:{},auth_method:'',cdr_location:{},currency:'',charging_periods:[],total_cost:{excl_vat:0},total_energy:0,total_time:0});
  state.objects[activeModule]??=[];state.objects[activeModule].push({id,body,delivery:'NOT_SENT',findings:[]});selectedID=id;
  newDrafts.set(draftKey(),state.objects[activeModule].at(-1));editorDrafts.set(draftKey(),JSON.stringify(body,null,2));
  renderObjects();renderEditor();setEditorMode('form');
  document.getElementById('opEnabled').checked=prefill;document.getElementById('opSources').hidden=!prefill;
  if(prefill)applyCataloguePrefill();
  return true;
}

function newObject(){
  let dialog=document.getElementById('newObjectDialog');
  if(!dialog){dialog=document.createElement('dialog');dialog.id='newObjectDialog';dialog.setAttribute('aria-labelledby','newObjectTitle');document.body.append(dialog)}
  const label=moduleMeta[activeModule][0];
  dialog.innerHTML=`<form id="newObjectForm"><div class="dialog-head"><div><span class="eyebrow">Partner test data</span><h2 id="newObjectTitle">New ${esc(label)}</h2></div><button type="button" onclick="document.getElementById('newObjectDialog').close()" aria-label="Close new record">✕</button></div><p class="hint">สร้าง draft แล้วแก้ข้อมูลใน Form หรือ Raw JSON ก่อนบันทึกและส่งไป Evolt</p><label>Record ID<input id="newObjectID" required maxlength="36" placeholder="e.g. QA-001" autocomplete="off"></label><div class="modal-actions"><button type="button" class="ghost" onclick="document.getElementById('newObjectDialog').close()">Cancel</button><button type="submit">Create draft</button></div></form>`;
  const prefillLabel=document.createElement('label');prefillLabel.className='prefill-toggle';prefillLabel.innerHTML='<input id="newObjectPrefill" type="checkbox"> Prefill from current connection / selected records (empty fields only)';dialog.querySelector('.modal-actions').before(prefillLabel);document.getElementById('newObjectPrefill').checked=qaPrefillEnabled('op');
  dialog.querySelector('form').addEventListener('submit',event=>{event.preventDefault();if(createObjectDraft(document.getElementById('newObjectID').value.trim(),document.getElementById('newObjectPrefill').checked))dialog.close()});
  dialog.showModal();document.getElementById('newObjectID').focus();
}
function renderTimeline(){renderRequestHistory('partnerTimeline',state.exchanges||[])}

function setPartnerDBQuery(query){partnerDBQuery=query.trim().toLowerCase();renderDB()}
function setPartnerDBSort(sort){partnerDBSort=sort;renderDB()}
function partnerRecordName(row){return String(row.object_id||row.uid||row.identifier||row.key||row.type||row.id||'')}
function partnerRecordTimestamp(row){const value=row.updated_at||row.created_at||row.body?.last_updated||row.last_updated;const time=Date.parse(value||'');return Number.isFinite(time)?time:0}
function partnerRecordDate(row){const time=partnerRecordTimestamp(row);return time?new Date(time).toLocaleString():'—'}
function partnerSecretBaselineKey(){return `partner-secret-baseline:${database.run_id}`}
function partnerSecretBaselineRecord(){try{const stored=JSON.parse(localStorage.getItem(partnerSecretBaselineKey())||'{}');return stored&&stored.fingerprints?{at:stored.at||'',fingerprints:stored.fingerprints}:{at:'',fingerprints:stored||{}}}catch{return {at:'',fingerprints:{}}}}
function partnerSecretBaseline(){return partnerSecretBaselineRecord().fingerprints||{}}
function partnerSecretComparison(row){const before=partnerSecretBaseline()[row.key];if(!row.fingerprint||!before)return {tone:'neutral',label:'ยังไม่มี baseline'};return before===row.fingerprint?{tone:'same',label:'ยังไม่เปลี่ยน'}:{tone:'changed',label:'เปลี่ยนแล้ว'}}
function partnerSecretDescription(key){return ({partner_issued_token:'Evolt → Partner · token ที่ Partner ออกให้ Evolt',evolt_issued_token:'Partner → Evolt · token ที่ Evolt ออกให้ Partner',partner_initial_token_a:'Token A ที่ Partner ออกให้ · handshake เท่านั้น',evolt_token_a:'Token A ที่ Evolt ออกให้ · handshake เท่านั้น'})[key]||'Encrypted Partner credential'}
async function togglePartnerDBSecrets(){partnerDBSecretsVisible=!partnerDBSecretsVisible;try{database=await api('/api/cpo/db'+(partnerDBSecretsVisible?'?reveal_secrets=1':''));renderDB()}catch(error){partnerDBSecretsVisible=false;toast(error.message,true)}}
function savePartnerSecretBaseline(){const rows=(database.tables||{}).secrets||[],fingerprints={};rows.forEach(row=>{if(row.fingerprint)fingerprints[row.key]=row.fingerprint});if(!Object.keys(fingerprints).length)return toast('กด Show secrets ก่อนบันทึกค่า Before Rotate',true);try{localStorage.setItem(partnerSecretBaselineKey(),JSON.stringify({at:new Date().toISOString(),fingerprints}));renderDB();toast('บันทึก fingerprint ก่อน Rotate แล้ว')}catch(error){toast(error.message,true)}}
function clearPartnerSecretBaseline(){try{localStorage.removeItem(partnerSecretBaselineKey());renderDB();toast('ล้าง baseline แล้ว')}catch(error){toast(error.message,true)}}
function partnerSecretBaselineNote(){const record=partnerSecretBaselineRecord(),keys=Object.keys(record.fingerprints||{});if(!keys.length)return '<span>ยังไม่ได้บันทึก baseline · กด Show secrets แล้วกด “จำค่า Before Rotate” ก่อนไปหมุน key</span>';const at=Date.parse(record.at||'');return `<span>baseline เก็บไว้ ${keys.length} ใบ${Number.isFinite(at)&&at?` · เมื่อ ${esc(new Date(at).toLocaleString())}`:''} <button class="link-button" type="button" onclick="clearPartnerSecretBaseline()">ล้าง baseline</button></span>`}
function renderPartnerSecret(row,index){const comparison=partnerSecretComparison(row),name=partnerRecordName(row)||`row ${index+1}`,copy=row.value?`<div class="partner-secret-copy"><code>${esc(row.value)}</code><button class="ghost" type="button" onclick="copyQAToken('${attr(row.value)}')">Copy secret</button><code>${esc(row.authorization||'')}</code><button class="ghost" type="button" onclick="copyQAToken('${attr(row.authorization||'')}')">Copy Authorization</button></div>`:'';return `<details class="db-card partner-secret-card"><summary class="db-card-head"><span class="db-card-name"><b>${esc(name)}</b><small>${esc(partnerSecretDescription(row.key))}</small></span><time>${esc(row.fingerprint||partnerRecordDate(row))}</time><span class="secret-compare ${comparison.tone}">${esc(comparison.label)}</span></summary>${copy}<pre>${esc(JSON.stringify(row,null,2))}</pre></details>`}
function setPartnerDBTable(name){if(name!==activeDBTable&&partnerDBSecretsVisible)return void togglePartnerDBSecrets().then(()=>{activeDBTable=name;renderDB()});activeDBTable=name;renderDB()}
function renderDB(){const tables=database.tables||{};const tabs=document.getElementById('partnerDBTabs');tabs.innerHTML=dbTables.map(name=>`<button class="${name===activeDBTable?'active':''}" onclick="setPartnerDBTable('${name}')">${name} <small>${(tables[name]||[]).length}</small></button>`).join('');const secretsTab=activeDBTable==='secrets',toggle=document.getElementById('partnerDBSecretToggle'),baseline=document.getElementById('partnerDBSecretBaseline');toggle.hidden=!secretsTab;toggle.textContent=partnerDBSecretsVisible?'Hide secrets':'Show secrets';baseline.hidden=!secretsTab||!partnerDBSecretsVisible;const source=tables[activeDBTable]||[],filtered=partnerDBQuery?source.filter(row=>JSON.stringify(row).toLowerCase().includes(partnerDBQuery)):source,rows=[...filtered].sort((a,b)=>partnerDBSort==='name'?partnerRecordName(a).localeCompare(partnerRecordName(b)):partnerDBSort==='oldest'?partnerRecordTimestamp(a)-partnerRecordTimestamp(b):partnerRecordTimestamp(b)-partnerRecordTimestamp(a)),root=document.getElementById('partnerDBContent');const secretHelp=secretsTab?`<div class="partner-secret-help"><strong>วิธีเช็ค Rotate Key</strong><span>กด Show secrets → จำค่า Before Rotate → ไปยิง Rotate ที่ Evolt Console → กลับมากด Show secrets อีกครั้ง · <code>partner_issued_token</code> และ <code>evolt_issued_token</code> ต้องขึ้น “เปลี่ยนแล้ว” ทั้งคู่</span>${partnerSecretBaselineNote()}</div>`:'';if(!rows.length){root.innerHTML=secretHelp+`<div class="empty">${partnerDBQuery?'ไม่พบข้อมูลที่ค้นหา':'No records in this PostgreSQL table'}</div>`;return}root.innerHTML=secretHelp+rows.map((row,index)=>secretsTab?renderPartnerSecret(row,index):`<details class="db-card"><summary class="db-card-head"><span class="db-card-name">${esc(partnerRecordName(row)||`row ${index+1}`)}</span><time>${esc(partnerRecordDate(row))}</time><span class="${row.encrypted?'encrypted':''}">${esc(row.delivery||row.status||row.role||(row.encrypted?'encrypted':''))}</span></summary><pre>${esc(JSON.stringify(row,null,2))}</pre></details>`).join('')}

function value(id){return document.getElementById(id)?.value?.trim()||''}function text(id,value){document.getElementById(id).textContent=value}function setIfIdle(id,value){const el=document.getElementById(id);if(el&&document.activeElement!==el&&value!==undefined)el.value=value||''}function esc(value){return String(value??'').replace(/[&<>"']/g,char=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]))}function attr(value){return esc(value).replace(/`/g,'&#96;')}function toast(message,error=false){const el=document.getElementById('toast');el.textContent=message;el.className=`toast show ${error?'error':''}`;clearTimeout(el.timer);el.timer=setTimeout(()=>el.className='toast',3500)}
const events=new EventSource((location.pathname.startsWith('/dual/')?'/dual':'')+'/api/cpo/events');events.addEventListener('ready',()=>{text('live','LIVE');refresh()});events.addEventListener('changed',()=>refreshQueue.schedule());events.onerror=()=>text('live','RECONNECTING');setInterval(tickCommandMeter,250);setInterval(()=>{void tickParking()},1000);refresh();
document.addEventListener('keydown',event=>{if(event.key!=='Escape')return;if(!document.getElementById('previewModal').classList.contains('hidden'))closePreview();if(!document.getElementById('resetModal').classList.contains('hidden'))closeReset()});
