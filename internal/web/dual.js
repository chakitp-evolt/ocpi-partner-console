let dualFormDirty=false;
let dualBusy=false;
let dualReady=false;
let dualTokensOpen=false;
let dualToastTimer;
const dualModules=['locations','tariffs','sessions','cdrs','tokens','commands'];

function dualText(id,value){document.getElementById(id).textContent=String(value??'')}
function dualValue(id){return document.getElementById(id).value.trim()}
function dualFeedback(message,error=false){const element=document.getElementById('dualFeedback');element.textContent=message;element.classList.toggle('error',error)}
function dualToast(message,error=false){const element=document.getElementById('dualToast');element.textContent=message;element.className=`toast show ${error?'error':''}`;clearTimeout(dualToastTimer);dualToastTimer=setTimeout(()=>element.className='toast',4000)}

async function dualAPI(path,options={}){
  const response=await fetch('/dual'+path,{headers:{'Content-Type':'application/json'},cache:'no-store',...options});
  const data=await response.json().catch(()=>({}));
  if(!response.ok)throw new Error(data.detail||data.message||data.error||`HTTP ${response.status}`);
  return data;
}

async function dualRun(label,work,requireReady=true){
  if(dualBusy)return;
  if(requireReady&&!dualReady){dualFeedback('โหลด connection ไม่สำเร็จ กรุณากด Refresh อีกครั้ง',true);dualToast('โหลด connection ไม่สำเร็จ กรุณากด Refresh อีกครั้ง',true);return}
  dualBusy=true;
  document.querySelectorAll('.dual-connection button,.dual-header-actions button').forEach(button=>button.disabled=true);
  dualFeedback(`${label}…`);
  try{await work();dualFeedback(`${label} สำเร็จ`);dualToast(`${label} สำเร็จ`)}
  catch(error){dualFeedback(error.message,true);dualToast(`${label} ไม่สำเร็จ: ${error.message}`,true)}
  finally{dualBusy=false;document.querySelectorAll('.dual-connection button,.dual-header-actions button').forEach(button=>button.disabled=false)}
}

function dualPopulateForm(run){
  if(dualFormDirty)return;
  const config=run.config||{};
  for(const [id,value] of Object.entries({
    dualRunName:run.name,dualVersionsInput:config.evolt_versions_url,
    dualPublicInput:config.partner_public_url,dualCountryCode:config.country_code,
    dualPartyID:config.party_id,dualBusinessName:config.business_name
  }))document.getElementById(id).value=value||'';
  document.getElementById('dualStrict').checked=!!config.strict;
  document.getElementById('dualRawCompat').checked=!!config.raw_token_compatibility;
}

function dualRenderInterfaces(id,interfaces,roles){
  const root=document.getElementById(id);
  root.replaceChildren();
  for(const module of dualModules){
    const role=roles[module],ready=interfaces.some(endpoint=>endpoint.identifier===module&&endpoint.role===role);
    const chip=document.createElement('span');
    chip.className=ready?'ready':'';
    chip.textContent=`${ready?'✓':'×'} ${module} · ${role}`;
    root.append(chip);
  }
}

function dualRenderEndpoints(interfaces){
  const root=document.getElementById('dualEndpointTable');
  root.replaceChildren();
  if(!interfaces.length){const empty=document.createElement('div');empty.className='empty';empty.textContent='Check connection เพื่อดู endpoints';root.append(empty);return}
  const table=document.createElement('table'),head=document.createElement('thead'),body=document.createElement('tbody');
  table.className='dual-endpoints-table';
  const header=document.createElement('tr');
  for(const label of ['MODULE','ROLE','URL']){const cell=document.createElement('th');cell.textContent=label;header.append(cell)}
  head.append(header);
  for(const endpoint of interfaces){
    const row=document.createElement('tr');
    const module=document.createElement('td');module.textContent=String(endpoint.identifier||'');
    const role=document.createElement('td'),badge=document.createElement('span');
    badge.className=`dual-endpoint-role ${String(endpoint.role||'').toLowerCase()}`;badge.textContent=String(endpoint.role||'—');role.append(badge);
    const target=document.createElement('td'),url=document.createElement('code');url.textContent=String(endpoint.url||'');target.append(url);
    row.append(module,role,target);
    body.append(row);
  }
  table.append(head,body);root.append(table);
}

async function refreshDual(){
  try{
    const [inbound,outbound]=await Promise.all([
      dualAPI('/api/emsp/state'),dualAPI('/api/cpo/state')
    ]);
    const run=inbound.run,config=run.config||{},interfaces=outbound.endpoints||[];
    const status=run.status||'SETUP',remoteRoles=(config.remote_roles||[]).map(role=>role.role);
    dualText('dualStatus',status);dualText('dualConnectionStatus',status);
    document.getElementById('dualStatus').className=status==='REGISTERED'?'pill registered':'pill neutral';
    dualText('dualIdentity',`${config.country_code||'—'}-${config.party_id||'—'}`);
    dualText('dualEndpointCount',interfaces.length);
    dualText('dualRemoteRoles',remoteRoles.join(' · ')||'ยังไม่เชื่อมต่อ');
    const emspRoles={locations:'SENDER',tariffs:'SENDER',sessions:'SENDER',cdrs:'SENDER',tokens:'RECEIVER',commands:'RECEIVER'};
    const cpoRoles={locations:'RECEIVER',tariffs:'RECEIVER',sessions:'RECEIVER',cdrs:'RECEIVER',tokens:'SENDER',commands:'SENDER'};
    const summary=(role,roles)=>`${remoteRoles.includes(role)?'Evolt ประกาศ '+role:'Evolt ยังไม่ประกาศ '+role} · มี interface ${dualModules.filter(module=>interfaces.some(endpoint=>endpoint.identifier===module&&endpoint.role===roles[module])).length}/${dualModules.length}`;
    dualText('dualEMSPReadiness',summary('CPO',emspRoles));
    dualText('dualCPOReadiness',summary('EMSP',cpoRoles));
    dualRenderInterfaces('dualEMSPInterfaces',interfaces,emspRoles);
    dualRenderInterfaces('dualCPOInterfaces',interfaces,cpoRoles);
    dualRenderEndpoints(interfaces);
    dualText('dualVersionsURL',`${String(config.partner_public_url||'').replace(/\/$/,'')}/ocpi/versions`);
    if(status==='REGISTERED'){document.getElementById('dualInitialTokenPanel').hidden=true;dualText('dualInitialToken','');dualText('dualInitialVersions','')}
    dualPopulateForm(run);
    dualReady=true;
    if(dualTokensOpen)await dualReloadTokens();
  }catch(error){
    dualReady=false;
    dualText('dualStatus','ERROR');dualText('dualConnectionStatus','โหลดข้อมูลไม่ได้');
    document.getElementById('dualStatus').className='pill neutral';
    dualFeedback(error.message,true);
    throw error;
  }
}

function dualManualRefresh(){return dualRun('รีเฟรชข้อมูล',refreshDual,false)}

async function dualPersist(){
  await dualAPI('/api/emsp/config',{method:'PUT',body:JSON.stringify({
    name:dualValue('dualRunName'),evolt_versions_url:dualValue('dualVersionsInput'),
    token_a:dualValue('dualTokenA'),partner_public_url:dualValue('dualPublicInput'),
    country_code:dualValue('dualCountryCode'),party_id:dualValue('dualPartyID'),
    business_name:dualValue('dualBusinessName'),
    strict:document.getElementById('dualStrict').checked,
    raw_token_compatibility:document.getElementById('dualRawCompat').checked
  })});
  document.getElementById('dualTokenA').value='';
  dualFormDirty=false;
}

function saveDualConnection(){
  return dualRun('บันทึก connection',async()=>{await dualPersist();await refreshDual()});
}

function dualAction(action){
  const labels={discover:'ตรวจ connection',register:'ลงทะเบียน partner','check-credentials':'ตรวจ credentials','rotate-credentials':'หมุน credentials'};
  return dualRun(labels[action],async()=>{
    if(dualFormDirty&&['discover','register'].includes(action))await dualPersist();
    await dualAPI(`/api/emsp/actions/${action}`,{method:'POST',body:'{}'});
    await refreshDual();
  });
}

function dualDisconnect(){
  if(!confirm('ยกเลิก Credentials ของ Dual-role Partner กับ Evolt?'))return;
  return dualRun('ยกเลิก connection',async()=>{
    await dualAPI('/api/cpo/actions/credentials/unregister',{method:'POST',body:'{}'});
    await refreshDual();
  });
}

function dualGenerateToken(){
  return dualRun('สร้าง Token A',async()=>{
    if(dualFormDirty)await dualPersist();
    const result=await dualAPI('/api/emsp/initial-token',{method:'POST',body:'{}'});
    dualText('dualInitialToken',result.token);
    dualText('dualInitialVersions',result.versions_url);
    document.getElementById('dualInitialTokenPanel').hidden=false;
    await refreshDual();
  });
}

async function dualCopy(value){
  try{await navigator.clipboard.writeText(value);dualFeedback('คัดลอกแล้ว');dualToast('คัดลอกแล้ว')}
  catch(error){dualFeedback(error.message,true);dualToast(`คัดลอกไม่สำเร็จ: ${error.message}`,true)}
}
function dualCopyInitial(){return dualCopy(`Token A: ${document.getElementById('dualInitialToken').textContent}\nVersions URL: ${document.getElementById('dualInitialVersions').textContent}`)}

function dualOpenReset(){if(dualBusy)return;const modal=document.getElementById('dualResetModal');document.getElementById('dualResetConfirmation').value='';dualSyncResetButton();modal.classList.remove('hidden');document.getElementById('dualResetConfirmation').focus()}
function dualCloseReset(){if(dualBusy)return;document.getElementById('dualResetModal').classList.add('hidden')}
function dualSyncResetButton(){document.getElementById('dualConfirmReset').disabled=dualValue('dualResetConfirmation')!=='RESET'}
function dualReset(){
  if(dualValue('dualResetConfirmation')!=='RESET')return;
  return dualRun('ล้างข้อมูล Dual',async()=>{
    await dualAPI('/api/emsp/reset',{method:'POST',body:JSON.stringify({confirmation:'RESET'})});
    dualTokensOpen=false;
    document.getElementById('dualTokenList').replaceChildren();
    document.getElementById('dualInitialTokenPanel').hidden=true;
    document.getElementById('dualTokenA').value='';
    dualFormDirty=false;
    await refreshDual();
    document.getElementById('dualResetModal').classList.add('hidden');
  },false);
}

async function dualLoadTokens(){
    const data=await dualAPI('/api/cpo/tokens');
    const root=document.getElementById('dualTokenList');
    root.replaceChildren();
    for(const [key,title] of [
      ['partner_to_evolt','Partner → Evolt'],['evolt_to_partner','Evolt → Partner'],['registration','Token A · registration']
    ]){
      const entry=data[key]||{},card=document.createElement('div');
      card.className='dual-token-card';
      const heading=document.createElement('strong'),note=document.createElement('small');
      heading.textContent=title;note.textContent=entry.note||'ยังไม่มี token';
      card.append(heading,note);
      if(entry.present){
        for(const [label,value] of [['Token',entry.token],['Authorization',entry.authorization]]){
          const caption=document.createElement('small'),code=document.createElement('code'),button=document.createElement('button');
          caption.textContent=label;code.textContent=value;button.type='button';button.className='ghost';button.textContent=`Copy ${label}`;
          button.addEventListener('click',()=>dualCopy(value));
          card.append(caption,code,button);
        }
      }
      root.append(card);
    }
}
async function dualReloadTokens(){
  try{await dualLoadTokens()}
  catch(error){const root=document.getElementById('dualTokenList'),note=document.createElement('small');note.className='error';note.textContent=`โหลด QA tokens ไม่สำเร็จ: ${error.message} · กด Show QA tokens อีกครั้ง`;root.replaceChildren(note);dualTokensOpen=false}
}
function dualShowTokens(){
  return dualRun('โหลด QA tokens',async()=>{
    try{await dualLoadTokens();dualTokensOpen=true}
    catch(error){document.getElementById('dualTokenList').replaceChildren();dualTokensOpen=false;throw error}
  });
}

document.getElementById('dualConnectionForm').addEventListener('input',()=>{dualFormDirty=true});
document.addEventListener('keydown',event=>{if(event.key==='Escape')dualCloseReset()});
refreshDual().catch(()=>{});
