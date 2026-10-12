let outboundFlowStep=1;
const outboundSteps=[
  {title:'Connection',hint:'ตั้งค่า identity และ Versions/public URL ของ Partner CPO',module:'connection',detail:'บันทึก connection ให้ตรง environment ตรวจว่า Evolt เข้าถึง public URL ได้ และ Partner เข้าถึง Evolt URL ได้'},
  {title:'Discovery & registration',hint:'Discover endpoints แล้วเชื่อมต่อด้วย Credentials',module:'connection',detail:'เปิด Connection เพื่อเริ่มจาก Partner หรือ Generate Token A ให้ Evolt เป็นฝ่ายเริ่ม รอ REGISTERED ก่อนใช้ registered credential ใน Requests'},
  {title:'Tariffs',hint:'เตรียมราคา แล้วส่งหรือให้ Evolt ดึง',module:'tariffs',detail:'สร้างหรือแก้ Tariff ใน Partner catalogue แล้ว Review & send หรือให้ Evolt GET จาก Sender endpoint ตรวจ response และตรวจราคาใน Evolt แยกด้วย'},
  {title:'Locations & EVSEs',hint:'เตรียมสถานี EVSE/Connector แล้วตรวจการ sync',module:'locations',detail:'กำหนด EVSE/Connector และ tariff_ids ให้ตรง Tariff ที่เตรียมไว้ เพิ่ม/แก้สถานะแล้วส่ง ตรวจผลใน Evolt และ Request history'},
  {title:'Tokens & commands',hint:'Evolt ส่ง Token/Command หรือ Partner ดึง Tokens',module:'tokens',detail:'เปิด Tokens เพื่อดูข้อมูลที่รับหรือกด Load tokens; ให้ Evolt ส่ง START_SESSION แล้วดู Commands และ callback ใน history คำตอบ ACCEPTED ยังไม่ยืนยันว่ามี Session'},
  {title:'Session lifecycle',hint:'สร้าง Session → ส่ง initial PUT → อัปเดต/จบ',module:'sessions',detail:'สร้าง Session draft และเลือก Location/Token ให้ตรงข้อมูลจริงของการทดสอบ ส่ง initial PUT ก่อน Update meter / Complete session; callback และ Session เป็นคนละหลักฐาน'},
  {title:'Final CDR',hint:'เตรียม CDR แล้วส่งไป Evolt',module:'cdrs',detail:'สร้าง CDR draft จาก Session ที่ทดสอบ หรือกรอกเอง ตรวจเวลา energy/cost และข้อมูลอ้างอิงก่อนส่ง POST ตรวจทั้ง response และผลที่ Evolt'},
  {title:'Review evidence',hint:'ตรวจ request/response และผลใน Evolt',module:'activity',detail:'ดู Request history และ QA request results แล้วตรวจ Evolt UI/DB ที่ได้รับสิทธิ์แยกต่างหาก สถานะ Observed ใน flow หมายถึงมีหลักฐานบางส่วน ไม่ใช่ test case ผ่านทั้งหมด'}
];
function activeOutboundSteps(){
  if(location.pathname!=='/dual/cpo')return outboundSteps;
  return [{title:'Shared connection',hint:'Credentials ชุดเดียวสำหรับทั้ง CPO และ eMSP',module:'connection',detail:'ตั้งค่า Discover และ Register ที่หน้า Connection กลาง แล้วกลับมาทดสอบขา CPO ที่นี่'},...outboundSteps.slice(2)];
}
function outboundObserved(){
  const accepted=module=>(state.exchanges||[]).some(x=>x.module===module&&x.http_status>=200&&x.http_status<300&&Number(x.ocpi_status_code)===1000);
  return [!!(state.config?.evolt_versions_url&&state.config?.partner_public_url),state.status==='REGISTERED',accepted('tariffs'),accepted('locations'),(state.objects?.tokens||[]).length>0||(state.commands||[]).length>0,accepted('sessions'),accepted('cdrs'),false];
}
function renderOutboundFlow(){
  const nav=document.getElementById('outboundFlowSteps');if(!nav)return;
  const steps=activeOutboundSteps(),observed=outboundObserved(),evidence=steps===outboundSteps?observed:[observed[1],...observed.slice(2)];
  nav.replaceChildren();steps.forEach((step,i)=>{const button=document.createElement('button');button.className=i+1===outboundFlowStep?'active':'';button.setAttribute('aria-current',i+1===outboundFlowStep?'step':'false');button.innerHTML=`<span>${String(i+1).padStart(2,'0')}</span><div><strong>${step.title}</strong><small>${evidence[i]?'Observed · still review result':'To test / review'}</small></div>`;button.addEventListener('click',()=>openOutboundFlow(i+1));nav.append(button)});
  const step=steps[outboundFlowStep-1],body=document.getElementById('outboundFlowBody');
  body.innerHTML=`<span class="eyebrow">Step ${outboundFlowStep} of ${steps.length}</span><h2>${step.title}</h2><p>${step.detail}</p><div class="action-row" id="outboundStepActions"></div><p class="ir-hint">เปิดส่วนนี้และ Previous / Next เป็นการเปลี่ยนหน้าที่ดูเท่านั้น ไม่ส่ง request ไม่แก้ข้อมูล และไม่ทำเครื่องหมายว่า test ผ่าน</p><div class="outbound-step-nav"><button class="ghost" id="outPrev" ${outboundFlowStep===1?'disabled':''}>← Previous step</button><button class="ghost" id="outNext" ${outboundFlowStep===steps.length?'disabled':''}>Next step →</button></div>`;
  const actions=document.getElementById('outboundStepActions'),add=(label,fn)=>{const b=document.createElement('button');b.textContent=label;b.addEventListener('click',fn);actions.append(b)};
  add(step.module==='connection'?'Open connection':step.module==='activity'?'Open request history':`Open ${step.module}`,()=>{if(step.module==='connection'){if(location.pathname.startsWith('/dual/')){location.href='/dual/';return}selectWorkspaceView('connection');document.getElementById('connectionPanel').open=true}else if(step.module==='activity')selectWorkspaceView('activity');else openOutboundCatalogue(step.module)});
  if(['locations','tariffs','sessions','cdrs','tokens'].includes(step.module))add('Edit QA request',()=>openInboundRequests(step.module));
  if(step.module==='tokens')add('View received commands',()=>openOutboundCatalogue('commands'));
  document.getElementById('outPrev').onclick=()=>openOutboundFlow(outboundFlowStep-1);document.getElementById('outNext').onclick=()=>openOutboundFlow(outboundFlowStep+1);
}
function openOutboundFlow(step){if(step)outboundFlowStep=Math.max(1,Math.min(activeOutboundSteps().length,step));selectWorkspaceView('flow');renderOutboundFlow()}
function openOutboundCatalogue(module){selectWorkspaceView('catalogue');selectModule(module)}
function applyCataloguePrefill(){
  if(!qaPrefillEnabled('op')||!currentRecord()||['tokens','commands'].includes(activeModule))return;
  try{const body=editorObject(),data=qaPrefillData('op',activeModule),changes=[];
    if(Object.hasOwn(body,'last_updated')){data.body.last_updated=new Date().toISOString();data.report.push('Current UTC clock')}
    qaFillEmpty(body,data.body,'body',changes);
    if(changes.length){document.getElementById('jsonEditor').value=JSON.stringify(body,null,2);captureEditorDraft();if(editorMode==='form')renderQuickForm(body)}
    qaPrefillReport('op',changes,data.report);
  }catch(error){document.getElementById('opReport').textContent=`Prefill not applied: ${error.message}`}
}
async function openCatalogueRequest(){
  if(!currentRecord()||['tokens','commands'].includes(activeModule))return;
  if(inboundEditor.dirty&&!confirm('Replace the unsaved QA request with this catalogue draft?'))return;
  const module=activeModule,raw=document.getElementById('jsonEditor').value,record=currentRecord();
  await openInboundRequests();inboundEditor.id='';ir('Template').value=module;ir('Name').value=`${module} ${record.id}`;ir('Module').value=module;ir('Method').value=module==='cdrs'?'POST':'PUT';ir('Body').value=raw;ir('Headers').value='{}';ir('HTTP').value='';ir('OCPI').value='';ir('Auth').value='registered';ir('Result').hidden=true;
  let body;try{body=JSON.parse(raw)}catch{body=record.body}
  ir('URL').value=requestTarget(body||{});ir('Form').hidden=true;ir('Body').hidden=false;markInboundDirty();
  inboundMessage('Copied catalogue draft as-is. Choose Prefill to add headers if needed. Raw requests store QA evidence, not catalogue delivery status.');
}
mountQAPrefill('op',document.getElementById('objectPrefillMount'),applyCataloguePrefill);
renderOutboundFlow();
