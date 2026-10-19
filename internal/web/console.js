const consoleEnvs=window.CONSOLE_ENVS||[{id:'dev',prefix:''}];
const currentEnv=consoleEnvs.find(env=>env.prefix&&(location.pathname===env.prefix||location.pathname.startsWith(env.prefix+'/')))||consoleEnvs.find(env=>!env.prefix)||{id:'dev',prefix:''};
function pagePath(){return location.pathname.slice(currentEnv.prefix.length)||'/'}
function appPath(path){return currentEnv.prefix+path}
function scopeLinks(root=document){if(!currentEnv.prefix)return;root.querySelectorAll('a[href^="/"]:not([data-env-scoped])').forEach(link=>{const href=link.getAttribute('href');if(/^\/(emsp|cpo|dual|api)(\/|$)/.test(href)){link.setAttribute('href',appPath(href));link.dataset.envScoped='1'}})}
function renderEnvSwitch(){if(consoleEnvs.length<2)return'';return `<nav class="env-switch" aria-label="Evolt environment">${consoleEnvs.map(env=>`<a href="${env.prefix+pagePath()}${location.search}" class="${env.id===currentEnv.id?'active':''}" ${env.id===currentEnv.id?'aria-current="page"':''}>${uiEscape(env.id.toUpperCase())}</a>`).join('')}</nav>`}
const initialTheme=localStorage.getItem('ocpi-test-lab-theme')||(matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light');
document.documentElement.dataset.theme=initialTheme;

const iconPaths={
  inbound:'<path d="M15 4v7H8"/><path d="m15 11-7-7"/><path d="M5 20h14"/>',
  outbound:'<path d="M9 20v-7h7"/><path d="m9 13 7 7"/><path d="M5 4h14"/>',
  database:'<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v6c0 1.7 3.6 3 8 3s8-1.3 8-3V5"/><path d="M4 11v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/>',
  tests:'<path d="M9 11l2 2 4-4"/><path d="M5 4h14v16H5z"/><path d="M9 4V2h6v2"/>',
  help:'<circle cx="12" cy="12" r="9"/><path d="M9.8 9a2.3 2.3 0 1 1 3.3 2.1c-.8.4-1.1.9-1.1 1.9"/><path d="M12 17h.01"/>',
  exchange:'<path d="m7 7-3 3 3 3"/><path d="M4 10h12"/><path d="m17 17 3-3-3-3"/><path d="M20 14H8"/>',
  finding:'<path d="M12 3 2.8 19h18.4z"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
  session:'<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  receipt:'<path d="M6 3h12v18l-3-2-3 2-3-2-3 2z"/><path d="M9 8h6M9 12h6M9 16h3"/>',
  station:'<path d="M7 21V4h8v17"/><path d="M5 21h12M9 8h4M9 12h4"/><path d="M15 8h2l2 2v6"/>',
  price:'<circle cx="12" cy="12" r="9"/><path d="M15 9.5c0-1-1.3-1.8-3-1.8s-3 .8-3 1.8 1.3 1.7 3 1.7 3 .8 3 1.8-1.3 1.8-3 1.8-3-.8-3-1.8M12 6v12"/>',
  token:'<circle cx="8" cy="12" r="4"/><path d="M12 12h9M17 12v3M20 12v2"/>',
  command:'<path d="m13 2-8 12h7l-1 8 8-12h-7z"/>',
  download:'<path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M5 20h14"/>',
  reset:'<path d="M4 12a8 8 0 1 0 2-5.3"/><path d="M4 4v5h5"/>',
  layout:'<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M9 4v16M12 8h6M12 12h6M12 16h4"/>',
  sun:'<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  moon:'<path d="M20 15.5A8 8 0 0 1 8.5 4 8.5 8.5 0 1 0 20 15.5z"/>'
};
function icon(name,className=''){return `<svg class="${className}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${iconPaths[name]||''}</svg>`}
function applyTheme(theme){document.documentElement.dataset.theme=theme;localStorage.setItem('ocpi-test-lab-theme',theme);const button=document.getElementById('themeToggle');if(!button)return;button.innerHTML=icon(theme==='dark'?'sun':'moon');button.setAttribute('aria-label',theme==='dark'?'Switch to light mode':'Switch to dark mode');button.title=theme==='dark'?'Light mode':'Dark mode'}
function toggleTheme(){applyTheme(document.documentElement.dataset.theme==='dark'?'light':'dark')}
function consoleLink([path,title,description,image]){const active=path==='/dual/'?pagePath().startsWith('/dual'):pagePath()===path;return `<a href="${appPath(path)}" data-env-scoped="1" class="${active?'active':''}" ${active?'aria-current="page"':''}>${icon(image,'nav-icon')}<strong>${title}</strong><small>${description}</small></a>`}
function renderConsoleNavigation(){
  const nav=document.querySelector('.lab-nav');if(!nav)return;
  const links=[['/emsp','eMSP Partner','จำลอง Partner eMSP','inbound'],['/cpo','CPO Partner','จำลอง Partner CPO','outbound'],['/dual/','Dual-role Partner','CPO + eMSP connection เดียว','exchange']];
  nav.innerHTML=`<span class="nav-section">PARTNER CONSOLE</span>${links.map(consoleLink).join('')}`;
  document.body.dataset.area='qa';
  if(pagePath().startsWith('/dual')){
    document.body.classList.add('dual-mode');
    const dualLeg=pagePath()==='/dual/emsp'?'inbound':pagePath()==='/dual/cpo'?'outbound':'';
    if(!document.querySelector('.dual-role-nav')){
      const selected=pagePath()==='/dual/emsp'?'inbound':pagePath()==='/dual/cpo'?'outbound':'connection';
      const items=[['connection','/dual/','Connection'],['inbound','/dual/emsp','eMSP · รับจาก Evolt'],['outbound','/dual/cpo','CPO · ส่งให้ Evolt']];
      document.querySelector('main')?.insertAdjacentHTML('afterbegin',`<nav class="dual-role-nav" aria-label="Dual-role Partner workspaces">${items.map(([id,path,label])=>`<a href="${appPath(path)}" data-env-scoped="1" class="${selected===id?'active':''}" ${selected===id?'aria-current="page"':''}>${label}</a>`).join('')}</nav>`);
    }
    if(pagePath()==='/dual/emsp'){
      document.querySelector('.step[data-step="1"] .step-head h3').textContent='Shared connection';
      document.querySelector('#journeyPanel .panel-title h2').textContent='eMSP test flow · Evolt CPO';
      document.querySelectorAll('.step[data-step]').forEach(step=>{const number=Number(step.dataset.step);if(number>=4)step.querySelector('.number').textContent=String(number-2).padStart(2,'0')});
      document.querySelectorAll('.step[data-step="1"],.step[data-step="2"],.step[data-step="3"]').forEach(step=>{
        const body=step.querySelector('.step-body');
        if(!body.querySelector('.dual-connection-link'))body.insertAdjacentHTML('afterbegin',`<div class="dual-connection-link"><p>ตั้งค่า Discover และ Credentials ของทั้ง CPO + eMSP ที่หน้า Connection เดียว</p><a class="button" href="${appPath('/dual/')}" data-env-scoped="1">เปิด Connection →</a></div>`);
      });
    }
    if(pagePath()==='/dual/cpo'){
      document.querySelector('#outboundFlow .panel-title h2').textContent='CPO test flow · Evolt eMSP';
    }
    if(dualLeg){
      const labels={flow:'Flow',catalogue:'Catalogue',requests:'Requests',activity:'History',data:'Data'};
      document.querySelectorAll('.workspace-nav [data-workspace-view]').forEach(button=>{if(labels[button.dataset.workspaceView])button.textContent=labels[button.dataset.workspaceView]});
      const badge=document.getElementById('dualLegStatus'),source=document.getElementById('runStatus');
      const sync=()=>{const status=source?.textContent.trim()||'SETUP';badge.textContent=status;badge.classList.toggle('registered',status==='REGISTERED')};
      sync();
      if(source)new MutationObserver(sync).observe(source,{childList:true,characterData:true,subtree:true});
      const reset=document.getElementById('resetModal');
      reset.querySelector('.eyebrow').textContent='RESET DUAL TEST DATA';
      reset.querySelector('#resetTitle').textContent='ล้างข้อมูลทดสอบ Dual ทั้งสองขา';
      reset.querySelector('.modal-card > p').textContent='ล้าง connection และข้อมูลทดสอบของ Dual eMSP และ CPO เพื่อเริ่มใหม่';
      reset.querySelector('#confirmReset').textContent='ล้าง Dual ทั้งสองขา';
      if(dualLeg==='inbound'){
        reset.querySelector('.hint.warning').textContent='eMSP Partner และ CPO Partner เดิมไม่ถูกล้าง · ควร Export ข้อมูล Dual ก่อน เพราะข้อมูลที่ล้างกู้คืนจาก UI ไม่ได้';
      }else{
        const scope=reset.querySelectorAll('.reset-scope section span');
        scope[0].textContent='Credentials, catalogue, sessions, CDRs, commands และ HTTP traces ของ Dual';
        scope[1].textContent='ข้อมูลของ eMSP Partner และ CPO Partner เดิม';
      }
    }
    document.querySelectorAll('a[href="/api/emsp/export"],a[href="/api/cpo/export"]').forEach(link=>link.href='/dual'+link.getAttribute('href'));
  }
  scopeLinks();
}
function selectWorkspaceView(view){
  document.querySelectorAll('[data-workspace-view]').forEach(button=>{button.classList.toggle('active',button.dataset.workspaceView===view);button.setAttribute('aria-current',button.dataset.workspaceView===view?'page':'false')});
  document.querySelectorAll('[data-workspace-panel]').forEach(panel=>panel.hidden=panel.dataset.workspacePanel!==view);
}
function createRefreshQueue(load,{delay=1500}={}){
  let timer=null,pending=false,running=null,again=false;
  const now=()=>{
    if(timer){clearTimeout(timer);timer=null}
    pending=false;
    if(running){again=true;return running}
    running=(async()=>{try{do{again=false;try{await load()}catch{}}while(again)}finally{running=null}})();
    return running;
  };
  const schedule=()=>{
    if(document.hidden){pending=true;return}
    if(running){again=true;return}
    if(!timer)timer=setTimeout(now,delay);
  };
  document.addEventListener('visibilitychange',()=>{if(!document.hidden&&pending)now()});
  return {schedule,now};
}
function openFlowStep(number){
  selectWorkspaceView('flow');
  document.querySelectorAll('.step').forEach(step=>{const active=Number(step.dataset.step)===Number(number);step.classList.toggle('open',active);step.querySelector('.step-head').setAttribute('aria-expanded',String(active))});
}
const requestHistory={items:[],query:'',status:'all',module:'all',page:1,size:10,selected:null};
function uiEscape(value){return String(value??'').replace(/[&<>"']/g,char=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]))}
function historyPass(item){return item.http_status>=200&&item.http_status<300}
function requestTime(value){return new Date(value).toLocaleString(undefined,{month:'short',day:'numeric',hour:'2-digit',minute:'2-digit',second:'2-digit'})}
function renderRequestHistory(rootId,items){
  const root=document.getElementById(rootId);if(!root)return;
  requestHistory.items=[...items].sort((a,b)=>Date.parse(b.created_at)-Date.parse(a.created_at));
  if(!root.dataset.initialized){
    root.innerHTML=`<div class="history-toolbar"><input id="historySearch" type="search" placeholder="Search requests, URL or status…" aria-label="Search request history"><div class="filter-buttons" aria-label="HTTP result filter"><button data-history-status="all" class="active">All</button><button data-history-status="ok">HTTP success</button><button data-history-status="bad">HTTP error</button></div><select id="historyModule" aria-label="Filter requests by module"><option value="all">All modules</option></select></div><div id="historyTableBody"></div>`;
    root.querySelector('#historySearch').addEventListener('input',event=>{requestHistory.query=event.target.value.toLowerCase().trim();requestHistory.page=1;drawRequestHistory()});
    root.querySelector('#historyModule').addEventListener('change',event=>{requestHistory.module=event.target.value;requestHistory.page=1;drawRequestHistory()});
    root.querySelectorAll('[data-history-status]').forEach(button=>button.addEventListener('click',()=>{requestHistory.status=button.dataset.historyStatus;requestHistory.page=1;drawRequestHistory()}));
    root.dataset.initialized='true';
  }
  const select=root.querySelector('#historyModule'),modules=[...new Set(items.map(item=>item.module).filter(Boolean))].sort();
  select.innerHTML='<option value="all">All modules</option>'+modules.map(module=>`<option value="${uiEscape(module)}">${uiEscape(module)}</option>`).join('');
  if(!modules.includes(requestHistory.module))requestHistory.module='all';select.value=requestHistory.module;
  drawRequestHistory();renderLatestRequest();
}
function drawRequestHistory(){
  const h=requestHistory,rows=h.items.filter(item=>(h.status==='all'||(h.status==='ok')===historyPass(item))&&(h.module==='all'||h.module===item.module)&&(!h.query||[item.method,item.module,item.url,item.http_status,item.ocpi_status_code,item.response_body?.status_message].join(' ').toLowerCase().includes(h.query)));
  const pages=Math.max(1,Math.ceil(rows.length/h.size));h.page=Math.min(h.page,pages);
  document.querySelectorAll('[data-history-status]').forEach(button=>{const active=button.dataset.historyStatus===h.status;button.classList.toggle('active',active);button.setAttribute('aria-pressed',String(active))});
  const from=(h.page-1)*h.size,visible=rows.slice(from,from+h.size);
  document.getElementById('historyTableBody').innerHTML=(visible.length?`<div class="history-table-wrap"><table class="history-table"><thead><tr><th>HTTP result</th><th>Request</th><th>Direction</th><th>OCPI</th><th>Time</th><th>Duration</th></tr></thead><tbody>${visible.map(item=>{const index=h.items.indexOf(item),ok=historyPass(item);return `<tr class="${ok?'ok':'bad'}"><td><span class="http-dot"></span>${item.http_status||'No response'}</td><td><button onclick="openRequestDetail(${index})" aria-label="View ${uiEscape(item.method)} ${uiEscape(item.module)} request at ${uiEscape(requestTime(item.created_at))}"><span class="method">${uiEscape(item.method)}</span> ${uiEscape(item.module||'Request')} ↗</button></td><td>${item.direction==='INBOUND'?'Evolt → Partner':'Partner → Evolt'}</td><td>${uiEscape(item.ocpi_status_code||'—')}</td><td>${uiEscape(requestTime(item.created_at))}</td><td>${Number(item.duration_ms)||0} ms</td></tr>`}).join('')}</tbody></table></div>`:'<div class="empty">'+(h.items.length?'No matching requests. Try another filter.':'No requests yet. Run a test to see what was sent and received.')+'</div>')+`<div class="pagination"><span>${rows.length?from+1:0}–${Math.min(from+h.size,rows.length)} of ${rows.length} requests</span><div><button onclick="changeHistoryPage(-1)" ${h.page===1?'disabled':''} aria-label="Previous request page">←</button><span>${h.page} / ${pages}</span><button onclick="changeHistoryPage(1)" ${h.page===pages?'disabled':''} aria-label="Next request page">→</button></div></div>`;
}
function changeHistoryPage(delta){requestHistory.page=Math.max(1,requestHistory.page+delta);drawRequestHistory()}
function renderLatestRequest(){
  let root=document.getElementById('latestRequest');
  if(!root){root=document.createElement('section');root.id='latestRequest';root.dataset.workspacePanel='flow';root.className='latest-result';document.querySelector('.workspace-nav').after(root)}
  const item=requestHistory.items[0];root.hidden=!item||!document.querySelector('[data-workspace-view="flow"]')?.classList.contains('active');
  if(!item)return;
  root.className=`latest-result ${historyPass(item)?'ok':'bad'}`;
  root.innerHTML=`<span class="result-mark">${historyPass(item)?'✓':'!'}</span><div><small>Latest request · ${uiEscape(requestTime(item.created_at))}</small><strong>${uiEscape(item.method)} ${uiEscape(item.module)} · HTTP ${item.http_status||'no response'} · OCPI ${uiEscape(item.ocpi_status_code||'—')}</strong></div><button onclick="openRequestDetail(0)">View result</button>`;
}
function openRequestDetail(index){
  const item=requestHistory.items[index];if(!item)return;requestHistory.selected=item;
  let dialog=document.getElementById('requestDetailDialog');
  if(!dialog){dialog=document.createElement('dialog');dialog.id='requestDetailDialog';dialog.setAttribute('aria-labelledby','requestDetailTitle');document.body.append(dialog);dialog.addEventListener('click',event=>{if(event.target===dialog){const bounds=dialog.getBoundingClientRect();if(event.clientX<bounds.left||event.clientX>bounds.right||event.clientY<bounds.top||event.clientY>bounds.bottom)dialog.close()}})}
  const findings=item.findings||[],transport=findings.find(finding=>finding.rule==='transport'),message=item.response_body?.status_message||transport?.message||(historyPass(item)?'HTTP request completed. Inspect the OCPI response and payload below.':'The request did not receive a successful HTTP response.');
  const payload=data=>uiEscape(JSON.stringify(data??null,null,2));
  dialog.innerHTML=`<div class="dialog-head"><div><span class="eyebrow">Request details</span><h2 id="requestDetailTitle">${uiEscape(item.method)} ${uiEscape(item.module)}</h2></div><button onclick="document.getElementById('requestDetailDialog').close()" aria-label="Close request details">✕</button></div><div class="request-url">${uiEscape(item.url)}</div><div class="request-facts"><span class="status ${historyPass(item)?'ok':'bad'}">HTTP ${item.http_status||'no response'}</span><span class="status ${item.ocpi_status_code>=1000&&item.ocpi_status_code<2000?'ok':'bad'}">OCPI ${uiEscape(item.ocpi_status_code||'—')}</span><span class="status">${Number(item.duration_ms)||0} ms</span><span class="status">${uiEscape(requestTime(item.created_at))}</span></div><div class="request-result">${uiEscape(message)}</div><div class="request-payloads"><section><h3>Sent · Request body</h3><pre>${payload(item.request_body)}</pre></section><section><h3>Received · Response body</h3><pre>${payload(item.response_body)}</pre></section></div><details><summary>Headers</summary><pre>${payload({request:item.request_headers,response:item.response_headers})}</pre></details><details><summary>Validation findings (${findings.length})</summary><pre>${payload(findings)}</pre></details><div class="modal-actions"><button class="ghost" onclick="copyRequestEvidence()">Copy request & response</button><button onclick="copyRequestCommand()">Copy cURL</button></div>`;
  if(!dialog.open)dialog.showModal();
}
async function copyRequestEvidence(){try{await navigator.clipboard.writeText(JSON.stringify(requestHistory.selected,null,2));toast('Request and response copied')}catch{toast('Unable to copy to clipboard',true)}}
async function copyRequestCommand(){
  const item=requestHistory.selected;if(!item)return;const quote=value=>"'"+String(value).replace(/'/g,"'\\''")+"'",lines=[`curl -X ${item.method} ${quote(item.url)}`];
  Object.entries(item.request_headers||{}).forEach(([key,value])=>lines.push(`  -H ${quote(key+': '+(Array.isArray(value)?value.join(', '):value))}`));
  if(item.request_body!==undefined&&item.request_body!==null)lines.push(`  --data ${quote(JSON.stringify(item.request_body))}`);
  try{await navigator.clipboard.writeText(lines.join(' \\\n'));toast('cURL copied with complete headers')}catch{toast('Unable to copy to clipboard',true)}
}
document.addEventListener('DOMContentLoaded',()=>{
  const nav=document.querySelector('.lab-nav');
  if(nav){
    renderConsoleNavigation();
    const brand=document.querySelector('.brand strong');if(brand)brand.textContent='OCPI Partner Console';
    const subtitle=document.querySelector('.brand small');if(subtitle)subtitle.textContent='Evolt · OCPI 2.2.1 partner mock';
  }
  scopeLinks();
  const topbar=document.querySelector('.lab-topbar');if(topbar){document.body.dataset.env=currentEnv.id;if(currentEnv.prefix){document.title=`[${currentEnv.id.toUpperCase()}] ${document.title}`;document.body.insertAdjacentHTML('afterbegin',`<div class="env-banner" role="status"><strong>${uiEscape(currentEnv.id.toUpperCase())}</strong><span>Evolt ${uiEscape(currentEnv.id)} · connection, token และข้อมูลทดสอบแยกจาก DEV</span><a href="${pagePath()}${location.search}">เปิดหน้านี้บน DEV →</a></div>`)}topbar.insertAdjacentHTML('beforeend',`<div class="sidebar-display-controls">${renderEnvSwitch()}<button id="themeToggle" class="theme-toggle" type="button" onclick="toggleTheme()"></button></div>`);applyTheme(document.documentElement.dataset.theme)}
  const metricIcons={EXCHANGES:'exchange',FINDINGS:'finding',SESSIONS:'session',CDRS:'receipt',LOCATIONS:'station',TARIFFS:'price','RECEIVED TOKENS':'token',COMMANDS:'command'};
  document.querySelectorAll('.metrics>div').forEach(item=>{
    const label=item.querySelector('small')?.textContent.trim().toUpperCase();
    item.insertAdjacentHTML('afterbegin',icon(metricIcons[label]||'database','metric-icon'));
  });
  document.querySelectorAll('[data-icon]').forEach(element=>element.insertAdjacentHTML('afterbegin',icon(element.dataset.icon,'button-icon')));
  const modules=document.querySelector('.module-tabs'),workspace=document.getElementById('objectWorkspace');
  if(modules&&workspace){workspace.prepend(modules);modules.querySelectorAll('button').forEach(button=>{const icons={locations:'station',tariffs:'price',sessions:'session',cdrs:'receipt',tokens:'token',commands:'command'};button.insertAdjacentHTML('afterbegin',icon(icons[button.dataset.module]));button.insertAdjacentHTML('beforeend',`<span class="module-count" data-module-count="${button.dataset.module}"></span>`)});document.getElementById('editorPanel').append(document.getElementById('helperPanel'))}
  document.querySelectorAll('.step').forEach(step=>{
    const head=step.querySelector('.step-head'),number=Number(step.dataset.step);head.setAttribute('role','button');head.tabIndex=0;head.setAttribute('aria-expanded',String(step.classList.contains('open')));head.addEventListener('keydown',event=>{if(event.key==='Enter'||event.key===' '){event.preventDefault();openFlowStep(number)}});
    const body=step.querySelector('.step-body'),title=head.querySelector('h3').textContent;
    const dualInbound=pagePath()==='/dual/emsp',displayNumber=dualInbound&&number>=4?number-2:number,total=dualInbound?6:8;
    const previous=dualInbound&&number===4?1:number-1,next=dualInbound&&number===1?4:number+1;
    body.insertAdjacentHTML('afterbegin',`<div class="step-intro"><span class="eyebrow">Step ${displayNumber} of ${total}</span><h2>${uiEscape(title)}</h2></div>`);
    body.insertAdjacentHTML('beforeend',`<div class="step-footer"><button class="ghost" onclick="openFlowStep(${previous})" ${number===1?'disabled':''}>← Previous step</button>${number<8?`<button class="ghost" onclick="openFlowStep(${next})">Next step →</button>`:'<button class="ghost" onclick="selectWorkspaceView(\'activity\')">Review requests →</button>'}</div>`);
  });
  if(document.querySelector('[data-workspace-view]'))selectWorkspaceView('flow');
});
