// Sample bodies for the request editor. A template gives an empty skeleton on purpose; this gives a
// complete spec-shaped object so QA edits a real payload instead of hunting one down elsewhere.
// Everything here is made up apart from the identity taken from Connection — IDs and timestamps
// still have to be changed to match the run before sending.
function qaSampleStamp(){return new Date().toISOString().replace(/\.\d+Z$/,'Z')}
function qaSampleOwner(){const config=qaConfig();return {country_code:config.country_code||'TH',party_id:config.party_id||'TST'}}
function qaSampleCallback(){const base=(qaConfig().partner_public_url||'').replace(/\/$/,'');return base?`${base}/ocpi/emsp/2.2.1/commands/callback/${crypto.randomUUID()}`:''}
function qaSampleToken(){
  return {...qaSampleOwner(),uid:'QA-RFID-0001',type:'RFID',contract_id:'THTSTCQA00001',visual_number:'QA-0001',issuer:'QA Console',valid:true,whitelist:'ALLOWED',language:'th',last_updated:qaSampleStamp()};
}
function qaSampleCdrToken(){const owner=qaSampleOwner();return {country_code:owner.country_code,party_id:owner.party_id,uid:'QA-RFID-0001',type:'RFID',contract_id:'THTSTCQA00001'}}
function qaSampleLocation(){
  const stamp=qaSampleStamp();
  return {...qaSampleOwner(),id:'QA-SAMPLE-1',publish:true,name:'QA Sample Station',address:'1 Si Lom Road',city:'Bang Rak',postal_code:'10500',country:'THA',
    coordinates:{latitude:'13.728600',longitude:'100.534000'},parking_type:'ON_STREET',time_zone:'Asia/Bangkok',opening_times:{twentyfourseven:true},charging_when_closed:true,
    evses:[{uid:'QA-EVSE-1',evse_id:'TH*TST*EQA001',status:'AVAILABLE',capabilities:['REMOTE_START_STOP_CAPABLE','RFID_READER'],physical_reference:'QA-01',
      connectors:[
        {id:'1',standard:'IEC_62196_T2',format:'SOCKET',power_type:'AC_3_PHASE',max_voltage:400,max_amperage:32,max_electric_power:22000,tariff_ids:['QA-SAMPLE-TARIFF'],last_updated:stamp},
        {id:'2',standard:'IEC_62196_T2_COMBO',format:'CABLE',power_type:'DC',max_voltage:920,max_amperage:200,max_electric_power:150000,tariff_ids:['QA-SAMPLE-TARIFF'],last_updated:stamp},
      ],last_updated:stamp}],
    last_updated:stamp};
}
function qaSampleTariff(){
  return {...qaSampleOwner(),id:'QA-SAMPLE-TARIFF',currency:'THB',
    tariff_alt_text:[{language:'en',text:'QA sample tariff'},{language:'th',text:'อัตราตัวอย่างสำหรับ QA'}],
    elements:[
      {price_components:[{type:'FLAT',price:10.0,vat:7.0,step_size:1},{type:'ENERGY',price:8.0,vat:7.0,step_size:1}]},
      {price_components:[{type:'PARKING_TIME',price:30.0,vat:7.0,step_size:900}],restrictions:{start_time:'18:00',end_time:'23:59',day_of_week:['MONDAY','TUESDAY','WEDNESDAY','THURSDAY','FRIDAY']}},
    ],
    last_updated:qaSampleStamp()};
}
function qaSampleSession(){
  const stamp=qaSampleStamp();
  return {...qaSampleOwner(),id:'QA-SAMPLE-SESSION-1',start_date_time:stamp,kwh:0,cdr_token:qaSampleCdrToken(),auth_method:'COMMAND',
    location_id:'QA-SAMPLE-1',evse_uid:'QA-EVSE-1',connector_id:'1',currency:'THB',status:'ACTIVE',last_updated:stamp};
}
function qaSampleCdr(){
  const stamp=qaSampleStamp(),owner=qaSampleOwner();
  return {...owner,id:'QA-SAMPLE-CDR-1',start_date_time:stamp,end_date_time:stamp,session_id:'QA-SAMPLE-SESSION-1',cdr_token:qaSampleCdrToken(),auth_method:'COMMAND',
    cdr_location:{id:'QA-SAMPLE-1',address:'1 Si Lom Road',city:'Bang Rak',postal_code:'10500',country:'THA',coordinates:{latitude:'13.728600',longitude:'100.534000'},
      evse_uid:'QA-EVSE-1',evse_id:'TH*TST*EQA001',connector_id:'1',connector_standard:'IEC_62196_T2',connector_format:'SOCKET',connector_power_type:'AC_3_PHASE'},
    currency:'THB',
    charging_periods:[{start_date_time:stamp,dimensions:[{type:'ENERGY',volume:10.0},{type:'TIME',volume:1.0}]}],
    total_cost:{excl_vat:80.0,incl_vat:85.6},total_energy:10.0,total_time:1.0,last_updated:stamp};
}
// null means the template has no body to fill — a GET, or a blank custom request
function requestSampleBody(kind){
  const outbound=qaIsOutbound();
  if(!outbound&&kind==='tokens')return qaSampleToken();
  if(!outbound&&kind==='token-patch')return {valid:false,last_updated:qaSampleStamp()};
  if(kind==='start')return {response_url:qaSampleCallback(),token:qaSampleToken(),location_id:'QA-SAMPLE-1',evse_uid:'QA-EVSE-1',connector_id:'1',authorization_reference:''};
  if(kind==='stop')return {response_url:qaSampleCallback(),session_id:'QA-SAMPLE-SESSION-1'};
  if(outbound&&kind==='locations')return qaSampleLocation();
  if(outbound&&kind==='tariffs')return qaSampleTariff();
  if(outbound&&kind==='sessions')return qaSampleSession();
  if(outbound&&kind==='cdrs')return qaSampleCdr();
  return null;
}
