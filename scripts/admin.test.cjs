const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');
const html = fs.readFileSync(path.join(__dirname, '../admin.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const clone = (v) => JSON.parse(JSON.stringify(v));
const seat = (key = 'sk-old') => ({ name: 'Alice', 'api-keys': [key], primary: ['alice.json'] });
const config = (key = 'sk-old') => ({ enabled: true, priority: 100, users: [seat(key)] });
const revision = 'a'.repeat(64);
function page(options = {}) {
  const elements = new Map(), listeners = new Map(), calls = [];
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, {id, style: {}, hidden: false, disabled: false, value: '', innerHTML: '', textContent: '', open: false,
      focus() {}, contains() {return false;}, showModal() {this.open=true;}, close() {this.open=false;},
      addEventListener(event, fn) {listeners.set(`${id}:${event}`, fn);}});
    return elements.get(id);
  };
  let stored = clone(options.config || config()), keys = clone(options.keys || ['sk-owner', 'sk-old']);
  let job = clone(options.initialJob || null), target = clone(options.target || config('sk-new')), polls = 0;
  const reply = (status, data) => ({status, ok: status >= 200 && status < 300, json: async () => clone(data)});
  const context = vm.createContext({
    document: {getElementById: element, querySelectorAll: () => [], addEventListener() {}, activeElement: null},
    window: {location: {pathname:'/gateway/v0/resource/plugins/cpa-carpool/admin', origin:'https://example.test'}, addEventListener() {}},
    sessionStorage: {getItem: () => '', setItem() {}, removeItem() {}}, crypto: webcrypto, Uint8Array, AbortSignal,
    navigator: {}, confirm: () => options.confirm !== false, queueMicrotask,
    setTimeout: (fn, ms) => {if (ms === 1000) queueMicrotask(fn); return 0;}, clearTimeout() {}, setInterval() {},
    fetch: async (url, init = {}) => {
      const method = init.method || 'GET', route = url.replace(/^\/gateway/, ''), body = init.body === undefined ? undefined : JSON.parse(init.body);
      calls.push({method, route, body});
      if (options.onFetch) await options.onFetch({method, route, body, context});
      if (options.unauthorized) return reply(401,{error:'unauthorized'});
      if (route.endsWith('/overview')) return reply(200,{enabled:true,seats:[]});
      if (route.endsWith('/auths')) return reply(200,{auths:[]});
      if (route.endsWith('/diagnostics')) return reply(200,{checks:[{name:'配置接口',status:'ok',message:'已连接'}, {name:'历史用量',status:'warning',message:'数据库不可用'}]});
      if (route.endsWith('/config')) {
        assert.equal(method,'GET','browser must not write raw configuration');
        if (options.configRead) {const r=options.configRead(); return reply(r.status,r.data);}
        return reply(200,{config:stored,exists:options.exists !== false,revision});
      }
      if (route.endsWith('/api-keys')) {
        assert.equal(method,'GET','browser must not edit authentication keys');
        if (options.keysRead) {const r=options.keysRead(); return reply(r.status,r.data);}
        return reply(200,keys);
      }
      if (route.endsWith('/save/retry')) {
        assert.equal(body.request_id,job.id); job={...job,state:'running',message:'正在重试'}; options.failJob=false; polls=0; return reply(202,{job});
      }
      if (route.includes('/save')) {
        if (method === 'POST') {
          if (options.submitStatus) return reply(options.submitStatus,{error:'配置校验失败，请检查席位设置'});
          if (options.submitError) throw new Error('network down');
          target=clone(body.config); job={id:body.request_id,state:'running',message:'正在保存',retryable:false}; polls=0;
          return reply(202,{job});
        }
        if (job && job.state === 'running' && ++polls > (options.jobPolls || 1)) {
          if (options.failJob) job={...job,state:'failed',message:'新 key 未能加入认证列表',retryable:true,error_code:'authorize_failed'};
          else {job={...job,state:'succeeded',message:'已保存',retryable:false}; stored=clone(target); options.exists=true; keys=['sk-owner',...stored.users.flatMap(s=>s['api-keys'])];}
        }
        return reply(200,{job});
      }
      throw new Error(`Unexpected request ${method} ${route}`);
    }
  });
  vm.runInContext(script, context, {filename:'admin.html'});
  context.fixture={key:'management-test-key',original:config(),draft:config('sk-new'),apiKeys:clone(keys),configExists:true,configLoaded:true,
    persistedRevision:revision,overview:{seats:[]},...options.state};
  vm.runInContext('Object.assign(S,fixture)',context);
  return {context,calls,elements,listeners,run:(js)=>vm.runInContext(js,context),state:()=>JSON.parse(vm.runInContext('JSON.stringify(S)',context))};
}
test('one invalid login consumes one authentication attempt',async()=>{
  const p=page({unauthorized:true});await p.run('loadAll()');assert.equal(p.calls.length,1);assert.equal(p.state().key,'');
});
test('save submits one snapshot to a backend job and only polls progress',async()=>{
  const p=page({jobPolls:3});await p.run('save()');
  const submits=p.calls.filter(c=>c.method==='POST');assert.equal(submits.length,1);assert.equal(submits[0].route,'/v0/management/carpool/save');
  assert.deepEqual(submits[0].body.config,config('sk-new'));assert.equal(submits[0].body.expected_revision,revision);
  assert(p.calls.every(c=>c.route.startsWith('/v0/management/carpool/')));assert.doesNotMatch(script,/\/v8\/management\//);
  assert.equal(p.state().saveJob.state,'succeeded');assert.equal(p.state().saving,false);assert.equal(p.state().saveError,'');
});
test('editor is frozen during submission and duplicate clicks do not start another job',async()=>{
  let release;const gate=new Promise(resolve=>{release=resolve;});
  const p=page({onFetch:async({method,route})=>{if(method==='POST'&&route.endsWith('/save'))await gate;}});
  const saving=p.run('save()');await new Promise(resolve=>setImmediate(resolve));
  assert.equal(p.state().saving,true);assert.equal(p.elements.get('refreshBtn').disabled,true);
  p.run('onSettingInput({target:{dataset:{setting:"rpm"},type:"number",value:"100"}})');assert.equal(p.state().draft.rpm,undefined);
  await p.run('save()');assert.equal(p.calls.filter(c=>c.method==='POST').length,1);release();await saving;
});
test('reconnecting discovers an existing backend job without submitting it again',async()=>{
  const p=page({initialJob:{id:'existing',state:'running',message:'后台继续保存'},jobPolls:3});
  await p.run('loadAll()');await new Promise(resolve=>setImmediate(resolve));
  assert.equal(p.calls.filter(c=>c.method==='POST').length,0);assert.equal(p.state().saveJob.state,'succeeded');assert.deepEqual(p.state().original,config('sk-new'));
});
test('failed backend jobs display an explicit retry and reuse the original job ID',async()=>{
  const p=page({failJob:true});await p.run('save()');assert.equal(p.state().saveJob.state,'failed');
  assert.match(p.elements.get('jobPanel').innerHTML,/重试上次任务/);const id=p.state().saveJob.id;
  await p.run('retrySaveJob()');assert.deepEqual(p.calls.find(c=>c.route.endsWith('/save/retry')).body,{request_id:id});assert.equal(p.state().saveJob.state,'succeeded');
});
test('a lost submit response locks editing until an explicit reload discovers the task',async()=>{
  const p=page({submitError:true});await p.run('save()');assert.equal(p.state().configLoaded,false);assert.match(p.elements.get('app').innerHTML,/提交状态尚未确认/);
  await p.run('save()');assert.equal(p.calls.filter(c=>c.method==='POST').length,1);
});
test('cancelling the change preview sends no requests',async()=>{const p=page({confirm:false});await p.run('save()');assert.deepEqual(p.calls,[]);});
test('an explicitly rejected configuration keeps the draft editable and displays the reason',async()=>{
  const p=page({submitStatus:400});await p.run('save()');assert.equal(p.state().configLoaded,true);assert.deepEqual(p.state().draft,config('sk-new'));assert.match(p.state().saveError,/配置校验失败/);
});
test('invalid default limits never become unlimited',async()=>{
  for(const value of ['-1','1.5']){const p=page();p.context.value=value;p.run('onSettingInput({target:{dataset:{setting:"rpm"},type:"number",value}})');await p.run('save()');assert.equal(p.calls.length,0);assert.match(p.state().saveError,/非负整数/);}
});
test('first setup is allowed only through an explicit successful absent-config response',async()=>{
  const p=page({config:{enabled:true,users:[]},exists:false});await p.run('loadAll()');assert.equal(p.state().configExists,false);
  p.context.newSeat=seat('sk-new');p.run('S.draft.users.push(newSeat)');await p.run('save()');assert.equal(p.state().saveError,'');assert.equal(p.state().configExists,true);
});
const badReads=[
  ['404',()=>({status:404,data:{error:'not_found'}})],['503',()=>({status:503,data:{}})],
  ['HTML',()=>({status:200,data:null})],['missing envelope',()=>({status:200,data:{}})],
  ['invalid config',()=>({status:200,data:{exists:true,revision,config:[]}})],
  ['invalid users',()=>({status:200,data:{exists:true,revision,config:{users:{}}}})],
  ['invalid seat',()=>({status:200,data:{exists:true,revision,config:{users:[null]}}})],
  ['network',()=>{throw new Error('network down');}],['timeout',()=>{throw new DOMException('timeout','TimeoutError');}]
];
for(const [name,configRead]of badReads){test(`config ${name} locks editing after an earlier successful read`,async()=>{
  const options={},p=page(options);await p.run('loadAll()');assert.equal(p.state().configLoaded,true);options.configRead=configRead;
  await p.run('loadAll()');assert.equal(p.state().configLoaded,false);assert.equal(p.state().original,null);assert.equal(p.state().draft,null);
  assert.match(p.elements.get('app').innerHTML,/alert-err.*无法读取配置接口，请检查网络或反代/);assert.equal(p.elements.get('saveBtn').disabled,true);
  assert.equal(p.elements.get('discardBtn').disabled,true);assert.equal(p.elements.get('refreshBtn').disabled,false);
  const count=p.calls.length;await p.run('save()');await p.run('refreshOverview()');p.run('onSettingInput({target:{dataset:{setting:"rpm"},type:"number",value:"100"}})');assert.equal(p.calls.length,count);
});}
for(const result of [{status:404,data:[]},{status:200,data:null},{status:200,data:[42]}]){test(`bad key list ${JSON.stringify(result)} never becomes empty authentication`,async()=>{
  const p=page({keysRead:()=>result});await p.run('loadAll()');await p.run('save()');assert.equal(p.state().configLoaded,false);assert.equal(p.calls.filter(c=>c.method==='POST').length,0);
});}
test('successful explicit reload unlocks the editor',async()=>{
  const options={configRead:()=>({status:404,data:{}})},p=page(options);await p.run('loadAll()');delete options.configRead;await p.run('loadAll()');assert.equal(p.state().configLoaded,true);assert.deepEqual(p.state().draft,config());
});
test('an uninitialized editor cannot submit a draft',async()=>{const p=page({state:{configLoaded:false}});await p.run('save()');assert.deepEqual(p.calls,[]);});
test('seat states cover active, soon, expired, and paused without losing paused bindings',()=>{
  const p=page();const now=Date.now();p.context.now=now;
  assert.equal(p.run('seatState({paused:true,"expire-at":"2020-01-01"},now)'),'paused');
  assert.equal(p.run('seatState({"expire-at":new Date(now-1).toISOString()},now)'),'expired');
  assert.equal(p.run('seatState({"expire-at":new Date(now+86400000).toISOString()},now)'),'soon');assert.equal(p.run('seatState({},now)'),'active');
  p.run('S.draft.users[0].paused=true; renderMain()');assert.deepEqual(p.state().draft.users[0]['api-keys'],['sk-new']);assert.match(p.elements.get('app').innerHTML,/已暂停/);
});
test('renewal extends an active lease and starts expired leases from now, preserving keys and pause',()=>{
  const p=page();const now=Date.now();p.context.future=new Date(now+86400000*10).toISOString();
  p.run('S.draft.users[0]["expire-at"]=future;S.draft.users[0].paused=true;renewSeats([0],30)');
  assert(Math.abs(Date.parse(p.state().draft.users[0]['expire-at'])-Date.parse(p.context.future)-30*86400000)<1000);assert.equal(p.state().draft.users[0].paused,true);
  p.run('S.draft.users[0]["expire-at"]="2020-01-01T00:00:00Z";renewSeats([0],7)');assert(Math.abs(Date.parse(p.state().draft.users[0]['expire-at'])-now-7*86400000)<1000);
  assert.deepEqual(p.state().draft.users[0]['api-keys'],['sk-new']);
});
test('bulk renewal only changes selected seats and rejects invalid durations',()=>{
  const p=page();p.run('S.draft.users.push({name:"Bob","api-keys":["sk-bob"],primary:["bob.json"]});renewSeats([1],30)');assert.equal(p.state().draft.users[0]['expire-at'],undefined);assert(p.state().draft.users[1]['expire-at']);
  const previous=p.state().draft;p.run('renewSeats([0],-1)');assert.deepEqual(p.state().draft,previous);
});
test('diagnostics reports checks and escapes returned messages',async()=>{
  const p=page();await p.run('diagnostics()');assert.match(p.elements.get('diagnosticsPanel').innerHTML,/配置接口/);assert.match(p.elements.get('diagnosticsPanel').innerHTML,/alert-warn/);
});
test('unavailable usage is not displayed as a zero or stale count',()=>{
  const p=page({state:{draft:config(),overview:{usage_database:true,usage_available:false,seats:[{name:'Alice',usage:{local_calls:5,total_tokens:88}}]}}});
  p.run('renderMain()');assert.match(p.elements.get('app').innerHTML,/历史用量暂不可用/);assert.doesNotMatch(p.elements.get('app').innerHTML,/5 次/);assert.equal(p.run('formatTokensM(88)'),'88');
});
