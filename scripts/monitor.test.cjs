const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../monitor.html'), 'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
const result = (model='gpt-demo') => ({ generated_at_ms: Date.now(), summary:{calls:1,successes:1,failures:0,input_tokens:80,output_tokens:2,cached_tokens:50,total_tokens:82,avg_latency_ms:null}, models:[{model,calls:1,successes:1,total_tokens:82}], events:[{model,failed:false,timestamp_ms:Date.now(),total_tokens:82,latency_ms:null,ttft_ms:null}],truncated:false });

function page(fetch, hash='') {
  const nodes=new Map(), timers=new Map(), events={}, requests=[], historyCalls=[];
  const node=id=>{ if(!nodes.has(id)) nodes.set(id,{ value:({range:'24h',status:'all',limit:'100'})[id]||'',innerHTML:'',textContent:'',hidden:false,className:'',addEventListener(name,fn){this[name]=fn;} }); return nodes.get(id); };
  let timerId=0;
  const location={hash,search:'',pathname:'/gateway/v0/resource/plugins/cpa-carpool/monitor',assign(url){this.assigned=url;}};
  const context=vm.createContext({
    document:{getElementById:node,hidden:false,addEventListener:(name,fn)=>{events[name]=fn;}},
    window:{addEventListener:(name,fn)=>{events[name]=fn;}}, location,
    history:{replaceState(...args){historyCalls.push(args);location.hash='';location.search='';}},
    URLSearchParams,AbortController,
    setTimeout:(fn,delay)=>{const id=++timerId;timers.set(id,{fn,delay});return id;},clearTimeout:id=>timers.delete(id),
    fetch:async(url,options)=>{requests.push({url,options});return fetch(url,options);},
  });
  vm.runInContext(source,context,{filename:'monitor.html'});
  return {node,context,location,timers,events,requests,historyCalls,run:code=>vm.runInContext(code,context)};
}
const response=(d,status=200)=>({ok:status===200,status,json:async()=>d});
const tick=()=>new Promise(resolve=>setImmediate(resolve));

test('monitor uses header credentials and removes share fragment before loading',async()=>{
  const p=page(async()=>response(result()),'#key=sk-demo-only');
  await tick();
  assert.equal(p.historyCalls.length,1);
  assert.equal(p.requests.length,1);
  assert.doesNotMatch(p.requests[0].url,/sk-demo|key=/);
  assert.equal(p.requests[0].options.headers['X-Carpool-Key'],'sk-demo-only');
  assert.equal(p.requests[0].options.cache,'no-store');
  assert.equal(p.requests[0].options.credentials,'omit');
  assert.match(p.node('models').innerHTML,/gpt-demo/);
  assert(p.timers.size===1);
  assert.equal([...p.timers.values()][0].delay,10000);
});

test('late response from a previous key cannot repopulate private data',async()=>{
  let finish;
  const p=page(async(_url,o)=>o.headers['X-Carpool-Key']==='sk-first'?await new Promise(r=>finish=r):response(result('second-model')));
  p.node('keyInput').value='sk-first';p.run('connect()');
  p.node('keyInput').value='sk-second';p.run('connect()');await tick();
  assert.match(p.node('models').innerHTML,/second-model/);
  finish(response(result('private-first-model')));await tick();
  assert.doesNotMatch(p.node('models').innerHTML,/private-first/);
  assert.match(p.node('models').innerHTML,/second-model/);
  assert.equal(p.requests[0].options.signal.aborted,true);
});

test('key edits, logout, and errors clear previously displayed records',async()=>{
  let fail=false;
  const p=page(async()=>response(fail?{error:'PRIVATE-RAW-ERROR'}:result(),fail?503:200));
  p.node('keyInput').value='sk-demo';p.run('connect()');await tick();
  assert.match(p.node('events').innerHTML,/gpt-demo/);
  fail=true;await p.run('refresh()');
  assert.doesNotMatch(p.node('events').innerHTML,/gpt-demo/);
  assert.doesNotMatch(p.node('message').textContent,/PRIVATE/);
  fail=false;await p.run('refresh()');
  p.node('keyInput').input();
  assert.doesNotMatch(p.node('events').innerHTML,/gpt-demo/);
  assert.equal(p.run('currentKey'),'');
  p.run('disconnect()');assert.equal(p.node('keyInput').value,'');assert.equal(p.timers.size,0);
});

test('model values are escaped and missing latency never becomes a false zero',async()=>{
  const p=page(async()=>response(result('<img src=x onerror=alert(1)>')));
  p.node('keyInput').value='sk-demo';p.run('connect()');await tick();
  assert.doesNotMatch(p.node('models').innerHTML,/<img/);
  assert.match(p.node('models').innerHTML,/&lt;img/);
  assert.match(p.node('events').innerHTML,/<td>--<\/td>/);
  assert.doesNotMatch(p.node('metrics').innerHTML,/>0 ms</);
});

test('hidden pages stop polling and status changes clear stale totals',async()=>{
  const p=page(async()=>response(result()));
  p.node('keyInput').value='sk-demo';p.run('connect()');await tick();
  p.run('document.hidden=true');p.events.visibilitychange();assert.equal(p.timers.size,0);
  p.run('document.hidden=false');p.events.visibilitychange();await tick();
  p.node('status').value='failed';p.node('status').onchange();
  assert.doesNotMatch(p.node('events').innerHTML,/gpt-demo/);await tick();
  assert.match(p.requests.at(-1).url,/status=failed/);
  p.events.pagehide();assert.equal(p.run('currentKey'),'');assert.equal(p.timers.size,0);
});

test('failed polling backs off and success restores the normal interval',async()=>{
  let fail=true;
  const p=page(async()=>response(fail?{error:'unavailable'}:result(),fail?503:200));
  p.node('keyInput').value='sk-demo';p.run('connect()');await tick();
  assert.equal([...p.timers.values()][0].delay,20000);
  await p.run('refresh()');assert.equal([...p.timers.values()][0].delay,40000);
  await p.run('refresh()');assert.equal([...p.timers.values()][0].delay,60000);
  fail=false;await p.run('refresh()');assert.equal([...p.timers.values()][0].delay,10000);
  assert.match(p.node('updated').textContent,/汇总/);
});
