const http = require('node:http');
const path = require('node:path');
const fs = require('node:fs');
const root = path.resolve(__dirname, '..');
fs.mkdirSync(path.join(root,'out/review'),{recursive:true});
const assert = require('node:assert/strict');
const { chromium } = require(process.env.CARPOOL_PLAYWRIGHT_PATH || 'playwright');
const host = process.env.CARPOOL_TEST_HOST;
if (process.env.CARPOOL_INTEGRATION_TEST !== '1' || !host) throw new Error('Use an isolated synthetic CPA fixture and set CARPOOL_INTEGRATION_TEST=1 and CARPOOL_TEST_HOST');
const key = 'review-v060-management-demo';
const prefix = '/v0/resource/plugins/cpa-carpool';
const sleep = ms => new Promise(r => setTimeout(r,ms));
async function managed(path,method='GET',body) {
 const r=await fetch(host+path,{method,headers:{Authorization:'Bearer '+key,'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});
 assert(r.ok,`${path} HTTP ${r.status}: ${await r.clone().text()}`);return r.json();
}
(async()=>{
 let browser,server;
 try {
  let keys=await managed('/v0/management/carpool/api-keys');assert(keys.includes('sk-demo-owner')&&keys.every(k=>k.startsWith('sk-demo-')),'refuse to modify a non-test gateway');
  const initial=await managed('/v0/management/carpool/config');
  if(initial.config.users.length){await managed('/v0/management/carpool/save','POST',{request_id:'reset-'+Date.now(),expected_revision:initial.revision,config:{...initial.config,users:[]}});for(let i=0;i<120;i++){const status=(await managed('/v0/management/carpool/save')).job;if(status.state==='succeeded')break;assert.notEqual(status.state,'failed',status.message);await sleep(250);}}
  keys=await managed('/v0/management/carpool/api-keys');assert.deepEqual(keys,['sk-demo-owner']);
  const requests=[];
  server=http.createServer(async(req,res)=>{
   if(req.url==='/'){res.setHeader('Content-Type','text/html');res.end('<!doctype html><title>CPAMP embedding fixture</title><iframe id="admin" src="/gateway'+prefix+'/admin" style="width:100%;height:1100px;border:0"></iframe>');return;}
   if(!req.url.startsWith('/gateway/v0/')&&!req.url.startsWith('/gateway/v1/')){res.writeHead(404);res.end('not exposed');return;}
   requests.push(req.url);
   const upstream=http.request(host+req.url.slice('/gateway'.length),{method:req.method,headers:{...req.headers,host:new URL(host).host}},reply=>{res.writeHead(reply.statusCode,reply.headers);reply.pipe(res);});
   upstream.on('error',()=>{res.writeHead(502);res.end();});req.pipe(upstream);
  });
  await new Promise(resolve=>server.listen(28419,'127.0.0.1',resolve));
  browser=await chromium.launch({headless:true});const page=await browser.newPage({viewport:{width:1600,height:1200}}),errors=[];
  page.setDefaultTimeout(15000);page.on('pageerror',e=>errors.push(e.message));page.on('dialog',d=>d.accept());
  const origin='http://127.0.0.1:28419';await page.goto(origin+'/');const frame=page.frames().find(f=>f.url().includes('/admin'));assert(frame,'same-origin admin iframe must load');
  await frame.locator('#mgmtKey').fill(key);await frame.locator('#loginForm button').click();await frame.getByRole('heading',{name:'席位（0）'}).waitFor();
  async function save(){await frame.locator('#saveBtn').click();await frame.waitForFunction(()=>S.saveJob&&S.saveJob.state!=='running'&&!S.saving,null,{timeout:60000});assert.equal(await frame.evaluate(()=>S.saveJob.state),'succeeded',await frame.evaluate(()=>S.saveJob.message));assert.equal(await frame.evaluate(()=>S.saveError),'');}
  async function add(name,key){await frame.locator('[data-act="add"]').click();await frame.locator('[data-ed="name"]').fill(name);await frame.locator('[data-ed="key"]').fill(key);await frame.locator('#authPick').selectOption('claude-demo.json');await frame.locator('[data-ed-act="pick-auth"]').click();await frame.locator('[data-ed-act="ok"]').click();}
  await add('Demo Active','sk-demo-v060-buyer');await save();
  assert.equal((await fetch(host+'/v1/models',{headers:{Authorization:'Bearer sk-demo-v060-buyer'}})).status,200);
  await frame.locator('[data-act="pause"]').click();await save();
  for(const path of ['/api/status','/api/monitor'])assert.equal((await fetch(host+prefix+path,{headers:{'X-Carpool-Key':'sk-demo-v060-buyer'}})).status,403);
  const rejected=await fetch(host+'/v1/chat/completions',{method:'POST',headers:{Authorization:'Bearer sk-demo-v060-buyer','Content-Type':'application/json'},body:JSON.stringify({model:'synthetic-test',messages:[{role:'user',content:'demo'}]})});assert.equal(rejected.status,403);assert.match(await rejected.text(),/seat_paused/);
  assert((await managed('/v0/management/carpool/api-keys')).includes('sk-demo-v060-buyer'),'pausing retains authentication keys');
  await frame.locator('[data-act="pause"]').click();await save();assert.equal((await fetch(host+prefix+'/api/status',{headers:{'X-Carpool-Key':'sk-demo-v060-buyer'}})).status,200);
  await frame.locator('#renewDays').fill('4');await frame.locator('[data-act="renew"]').click();await save();
  await frame.locator('#seatFilter').selectOption('soon');assert.equal(await frame.locator('[data-act="edit"]').count(),1);await frame.locator('#seatFilter').selectOption('all');
  await add('Demo Second','sk-demo-v060-second');await save();
  await frame.locator('[data-select-seat="0"]').check();await frame.locator('[data-select-seat="1"]').check();await frame.locator('#renewDays').fill('14');await frame.locator('[data-act="renew-selected"]').click();await save();
  const cfg=await managed('/v0/management/carpool/config');assert(cfg.config.users.every(s=>Date.parse(s['expire-at'])>Date.now()+13*86400000));
  await frame.locator('#diagnosticsBtn').click();await frame.locator('#diagnosticsPanel').getByText(/席位配置已生效/).waitFor();
  assert.equal((await fetch(origin+'/gateway/v8/management/config')).status,404);assert(requests.every(p=>!p.includes('/v8/')));
  // A configuration 404 must lock the editor without changing persisted seats.
  await page.route('**/v0/management/carpool/config',route=>route.fulfill({status:404,contentType:'application/json',body:'{}'}));
  await frame.locator('#refreshBtn').click();await frame.getByText('无法读取配置接口，请检查网络或反代',{exact:true}).waitFor();assert.equal(await frame.locator('#saveBtn').isDisabled(),true);
  assert.deepEqual((await managed('/v0/management/carpool/config')).config,cfg.config);await page.unroute('**/v0/management/carpool/config');await frame.locator('#refreshBtn').click();await frame.locator('[data-act="edit"]').first().waitFor();
  // Abort the response after CPA has accepted the save, then close the page.
  await page.route('**/v0/management/carpool/save',async route=>{if(route.request().method()==='POST'){await route.fetch();await route.abort();}else await route.continue();});
  await frame.locator('[data-act="pause"]').first().click();await frame.locator('#saveBtn').click();await frame.getByText(/保存提交状态尚未确认/).waitFor();await page.close();
  const reopened=await browser.newPage({viewport:{width:1600,height:1100}});reopened.on('dialog',d=>d.accept());reopened.on('pageerror',e=>errors.push(e.message));
  await reopened.goto(origin+'/gateway'+prefix+'/admin');await reopened.locator('#mgmtKey').fill(key);await reopened.locator('#loginForm button').click();
  await reopened.waitForFunction(()=>S.configLoaded&&S.saveJob&&S.saveJob.state==='succeeded'&&!S.saving,null,{timeout:60000});
  assert.equal(await reopened.evaluate(()=>S.original.users[0].paused),true,'backend must complete after page closes');
  await reopened.screenshot({path:path.join(root,'docs/images/admin.png'),fullPage:true});
  await reopened.setViewportSize({width:390,height:844});assert.equal(await reopened.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'mobile page overflows');
  await reopened.screenshot({path:path.join(root,'out/review/admin-v060-mobile.png'),fullPage:true});
  assert.deepEqual(errors,[]);console.log('PASS: real CPA 8.0.30, plugin 0.6.0, same-origin iframe, prefixed reverse proxy without /v8/, backend save, pause/resume, renewal and bulk renewal, diagnostics, read-failure lock, lost response and page-closure recovery, desktop and mobile.');
 } finally {if(browser)await browser.close();if(server){server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}}
})().catch(error=>{console.error(error);process.exit(1);});
