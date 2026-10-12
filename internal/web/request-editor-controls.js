{
  const root=document.getElementById('inboundRequests'),out=qaIsOutbound();
  root.querySelector('.panel-title p').textContent=out?'Partner CPO → Evolt eMSP · ใช้ editor เดียวกับ Inbound':'Partner eMSP → Evolt CPO · ใช้ editor เดียวกับ Outbound';
  const templates=document.getElementById('irTemplate');
  if(out){templates.replaceChildren();for(const [key,label] of [['custom','Blank / custom request'],['versions','GET Versions'],['locations','PUT Location'],['tariffs','PUT Tariff'],['sessions','PUT Session'],['cdrs','POST CDR'],['tokens','GET Tokens']])templates.add(new Option(label,key))}
  document.getElementById('irAuth').options[0].textContent=`Registered ${qaLane()} credential`;
  const prefill=document.createElement('div');document.getElementById('irEndpoint').parentElement.before(prefill);
  mountQAPrefill('rp',prefill,()=>applyRequestPrefill());
  const body=document.getElementById('irBody'),modes=document.createElement('div');modes.className='editor-mode';
  modes.innerHTML='<button type="button" class="ghost" onclick="showRequestBodyForm(true)">Form</button><button type="button" class="ghost" onclick="showRequestBodyForm(false)">Raw JSON</button>';
  const form=document.createElement('div');form.id='irForm';form.className='quick-form';form.hidden=true;body.before(modes,form);
  const empty=[...root.querySelectorAll('button')].find(button=>button.textContent==='Empty body');if(empty)empty.onclick=()=>{body.value='';showRequestBodyForm(false);markInboundDirty()};
  const result=document.getElementById('irResult').querySelector('.ir-hint');result.textContent='ผลของ request นี้เท่านั้น ไม่ใช่การรับรอง flow ทั้งหมด · เก็บ QA evidence แยกจาก catalogue/walkthrough; ไม่เปลี่ยน delivery status ของ object · incoming callback ดูเพิ่มใน Request history';
}
