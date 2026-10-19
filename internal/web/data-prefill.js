function qaLane(){const lane=document.body?.classList?.contains('outbound-page')?'CPO':'EMSP';return pagePath().startsWith('/dual/')?'DUAL_'+lane:lane}
function qaIsOutbound(){return qaLane().endsWith('CPO')}
function qaConfig(){return qaIsOutbound()?(state?.config||{}):(state?.run?.config||{})}
function qaRows(module){return (qaIsOutbound()?state?.objects?.[module]:state?.[module])||[]}
function qaRecordBody(row){return row?.body||row||{}}
function qaRecordID(row){const b=qaRecordBody(row);return String(row?.id||row?.object_id||b.id||b.uid||'')}
function qaMissing(value){return value===undefined||value===null||value===''}
function qaPlain(value){return value!==null&&typeof value==='object'&&!Array.isArray(value)}
function qaFillEmpty(target,source,prefix='',changes=[]){
  for(const [key,value] of Object.entries(source||{})){
    if(['__proto__','prototype','constructor'].includes(key)||qaMissing(value))continue;
    const path=prefix?`${prefix}.${key}`:key;
    if(qaMissing(target[key])){target[key]=JSON.parse(JSON.stringify(value));changes.push(path)}
    else if(qaPlain(target[key])&&qaPlain(value))qaFillEmpty(target[key],value,path,changes);
  }
  return changes;
}
function mountQAPrefill(prefix,mount,apply){
  mount.innerHTML=`<div class="qa-prefill"><label class="prefill-toggle"><input id="${prefix}Enabled" type="checkbox"> Prefill from available data <small>เติมเฉพาะช่องว่าง ไม่ทับค่าที่แก้เอง</small></label><div id="${prefix}Sources" class="prefill-sources" hidden><label>Location<select id="${prefix}Location"></select></label><label>Token<select id="${prefix}Token"></select></label><label>Session<select id="${prefix}Session"></select></label><button id="${prefix}Apply" type="button" class="ghost">Fill empty fields</button></div><p id="${prefix}Report" class="ir-hint" role="status">Prefill off · ไม่มีการเติมค่าจาก connection หรือ records</p></div>`;
  document.getElementById(`${prefix}Enabled`).addEventListener('change',event=>{document.getElementById(`${prefix}Sources`).hidden=!event.target.checked;if(event.target.checked){refreshQAPrefill(prefix);apply()}else document.getElementById(`${prefix}Report`).textContent='Prefill off · ค่าที่เติมไปแล้วคงอยู่ แก้หรือลบได้เอง ไม่มีการเติมต่ออัตโนมัติ'});
  document.getElementById(`${prefix}Apply`).addEventListener('click',apply);
  refreshQAPrefill(prefix);
}
function qaPrefillEnabled(prefix){return document.getElementById(`${prefix}Enabled`)?.checked===true}
function refreshQAPrefill(prefix){
  for(const [field,module] of [['Location','locations'],['Token','tokens'],['Session','sessions']]){
    const select=document.getElementById(`${prefix}${field}`);if(!select)continue;const previous=select.value;
    select.replaceChildren(new Option(`No ${field.toLowerCase()} selected`,''));
    qaRows(module).forEach(row=>{const body=qaRecordBody(row),id=qaRecordID(row);select.add(new Option(`${id}${body.name?' · '+body.name:''}`,id))});
    select.value=previous;
  }
}
function qaPrefillData(prefix,module){
  const c=qaConfig(),report=['Connection (current lane)'];
  const chosen=field=>document.getElementById(`${prefix}${field}`)?.value||'';
  const selected=(field,collection)=>qaRows(collection).find(row=>qaRecordID(row)===chosen(field));
  const session=qaRecordBody(selected('Session','sessions')),token=qaRecordBody(selected('Token','tokens'));
  let location=qaRecordBody(selected('Location','locations'));
  if(chosen('Session'))report.push(`Session ${chosen('Session')}`);
  if(chosen('Token'))report.push(`Token ${chosen('Token')}`);
  if(chosen('Location'))report.push(`Location ${chosen('Location')}`);
  if(session.location_id&&chosen('Location')&&session.location_id!==location.id)throw new Error('Selected Session belongs to a different Location. Select matching sources.');
  if(session.cdr_token?.uid&&chosen('Token')&&session.cdr_token.uid!==token.uid)throw new Error('Selected Token differs from Session token. Select matching sources.');
  if(session.location_id&&!chosen('Location')){location=qaRecordBody(qaRows('locations').find(row=>qaRecordBody(row).id===session.location_id));if(location.id)report.push(`Location ${location.id} referenced by Session`)}
  const evses=location.evses||[],evse=session.evse_uid?evses.find(e=>e.uid===session.evse_uid):(evses.length===1?evses[0]:null);
  const connectors=evse?.connectors||[],connector=session.connector_id?connectors.find(e=>e.id===session.connector_id):(connectors.length===1?connectors[0]:null);
  const identity={country_code:c.country_code,party_id:c.party_id};
  const body=module==='commands'?{}:{...identity};
  if(module==='locations')for(const key of ['address','city','postal_code','country','coordinates','time_zone'])if(location[key]!==undefined)body[key]=location[key];
  if(module==='tariffs'&&session.currency)body.currency=session.currency;
  if(module==='tokens')Object.assign(body,{issuer:c.business_name,...token});
  if(module==='commands')Object.assign(body,{token:{...identity,issuer:c.business_name,...token},location_id:location.id,evse_uid:evse?.uid,connector_id:connector?.id,session_id:session.id});
  if(['sessions','cdrs'].includes(module)){
    const t=session.cdr_token||(Object.keys(token).length?Object.fromEntries(['country_code','party_id','uid','type','contract_id'].filter(k=>token[k]!==undefined).map(k=>[k,token[k]])):undefined);
    Object.assign(body,{location_id:location.id,evse_uid:evse?.uid,connector_id:connector?.id,cdr_token:t,currency:session.currency});
    if(module==='cdrs'){delete body.location_id;delete body.evse_uid;delete body.connector_id;body.session_id=session.id}
  }
  return {body,report};
}
function qaPrefillReport(prefix,changes,sources){document.getElementById(`${prefix}Report`).textContent=changes.length?`Filled: ${changes.join(', ')} · Sources: ${sources.join('; ')}`:'No matching empty fields. Existing values, false, 0 and arrays are preserved; choose records for more data.'}
