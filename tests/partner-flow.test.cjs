const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

const source=name=>fs.readFileSync(path.join(__dirname,'../internal/web',name),'utf8');
function section(code,from,to){
  const start=code.indexOf(from),end=code.indexOf(to,start);
  assert.ok(start>=0&&end>start,`missing section ${from}`);
  return code.slice(start,end);
}

for(const page of ['/emsp','/dual/emsp','/dual/']){
  test(`connection action buttons use the registered routes on ${page}`,async()=>{
    const calls=[];
    const context=vm.createContext({
      document:{activeElement:null},location:{pathname:page},
      fetch:async(url,options)=>{calls.push({url,options});return {ok:true,json:async()=>({})}},
      toast:()=>{},showResult:()=>{},refresh:async()=>{},refreshDual:async()=>{},
      dualRun:async(_,work)=>work(),
    });
    if(page==='/dual/'){
      const code=source('dual.js');
      vm.runInContext("let dualFormDirty=false;"+section(code,'async function dualAPI(','async function dualRun(')+section(code,'function dualAction(','function dualDisconnect('),context);
    }else{
      const code=source('emsp.js');
      vm.runInContext(section(code,'async function request(','async function loadState(')+section(code,'async function runAction(','async function pull('),context);
    }
    for(const action of ['discover','register','check-credentials','rotate-credentials']){
      await vm.runInContext(`${page==='/dual/'?'dualAction':'runAction'}('${action}')`,context);
    }
    assert.deepEqual(calls.map(call=>call.url),['discover','register','check-credentials','rotate-credentials'].map(action=>`${page.startsWith('/dual/')?'/dual':''}/api/emsp/actions/${action}`));
    assert.ok(calls.every(call=>call.options.method==='POST'));
  });
}

function tokenPicker(){
  const calls=[];
  const esc=value=>String(value).replace(/[&<>"']/g,char=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
  const context=vm.createContext({
    state:{run_id:'cpo-1',objects:{tokens:[
      {id:'TH/EVT/SAME/RFID',body:{country_code:'TH',party_id:'EVT',uid:'SAME',type:'RFID',contract_id:'TH-EVT-C1',valid:true}},
      {id:'DE/OTH/SAME/APP_USER',body:{country_code:'DE',party_id:'OTH',uid:'SAME',type:'APP_USER',contract_id:'DE-OTH-C1',valid:false}},
    ]}},esc,attr:esc,
    value:id=>({helperToken:vm.runInContext('sessionDraftTokenID',context),helperLocation:'LOC',helperEVSE:'EVSE',helperConnector:'1'})[id],
    createSessionDraft:async input=>calls.push(JSON.parse(JSON.stringify(input))),
  });
  const code=source('cpo.js');
  vm.runInContext("let sessionDraftTokenID='',sessionDraftTokenRunID='';"+section(code,'function sessionDraftTokenPickerHTML(','function renderHelpers(')+section(code,'async function createSession(){','async function createSessionFromCommand('),context);
  return {calls,context,run:code=>vm.runInContext(code,context)};
}

test('manual token selection survives refresh and sends the full record key',async()=>{
  const s=tokenPicker();
  assert.match(s.run('sessionDraftTokenPickerHTML()'),/TH-EVT.*SAME.*RFID/);
  s.run("sessionDraftTokenID='DE/OTH/SAME/APP_USER'");
  const html=s.run('sessionDraftTokenPickerHTML()');
  assert.match(html,/value="DE\/OTH\/SAME\/APP_USER" selected/);
  assert.match(html,/valid=false/);
  await s.run('createSession()');
  assert.deepEqual(s.calls[0],{location_id:'LOC',evse_uid:'EVSE',connector_id:'1',token_id:'DE/OTH/SAME/APP_USER'});
});

test('the manual Session draft panel renders the catalogue token selector',()=>{
  const s=tokenPicker(),elements=new Map();
  s.context.document={getElementById:id=>{
    if(!elements.has(id))elements.set(id,{innerHTML:''});
    return elements.get(id);
  }};
  Object.assign(s.context,{activeModule:'sessions',meterControl:{autoMode:false,rate:'7.2'},automationControlsHTML:()=>'',sessionLifecycleHTML:()=>'',currentRecord:()=>null,fillConnectorOptions:()=>{},fillCommandConnectorOptions:()=>{}});
  s.context.state.config={};
  s.context.state.commands=[];
  s.context.state.objects.locations=[];
  s.run(section(source('cpo.js'),'function renderHelpers(){','function fillStatusEVSE('));
  s.run('renderHelpers()');
  assert.match(elements.get('draftPane').innerHTML,/id="helperToken"/);
  assert.match(elements.get('draftPane').innerHTML,/value="DE\/OTH\/SAME\/APP_USER"/);
});

test('a removed token stays an explicit missing selection instead of becoming demo',async()=>{
  const s=tokenPicker();
  s.run('sessionDraftTokenPickerHTML()');
  s.run("sessionDraftTokenID='TH/EVT/SAME/RFID';state.objects.tokens=[]");
  assert.match(s.run('sessionDraftTokenPickerHTML()'),/value="TH\/EVT\/SAME\/RFID" selected disabled/);
  await s.run('createSession()');
  assert.equal(s.calls[0].token_id,'TH/EVT/SAME/RFID');
});

test('reset clears token choice and demo remains available with an empty catalogue',async()=>{
  const s=tokenPicker();
  s.run('sessionDraftTokenPickerHTML()');
  s.run("sessionDraftTokenID='TH/EVT/SAME/RFID';state.run_id='cpo-2';state.objects.tokens=[]");
  assert.match(s.run('sessionDraftTokenPickerHTML()'),/value="" selected/);
  assert.equal(s.run('sessionDraftTokenID'),'');
  await s.run('createSession()');
  assert.equal(s.calls[0].token_id,undefined);
});
