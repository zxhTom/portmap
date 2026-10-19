// Real-browser acceptance against an isolated daemon; no production mappings are touched.
// PORTMAP_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node tests/web_browser.mjs /path/to/portmap
import { mkdtemp, writeFile, readFile, rm, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { createServer } from 'node:net';
import assert from 'node:assert/strict';
const { chromium }=await import(process.env.PORTMAP_PLAYWRIGHT_MODULE||'playwright');
const freePort=()=>new Promise(resolve=>{const s=createServer();s.listen(0,'127.0.0.1',()=>{const p=s.address().port;s.close(()=>resolve(p));});});
const dir=await mkdtemp(join(tmpdir(),'portmap-web-browser-'));
const shots=process.env.PORTMAP_SCREENSHOTS||join(dir,'screenshots');await mkdir(shots,{recursive:true});
const webPort=await freePort(),apiPort=await freePort(),relayPort=await freePort();
const relay=spawn(process.env.PORTMAP_TEST_RELAY||process.argv[2]+'.test-relay',[`127.0.0.1:${relayPort}`],{stdio:['ignore','pipe','pipe']});
await Promise.race([once(relay.stdout,'data'),once(relay,'error').then(([err])=>Promise.reject(err))]);
const base=`http://127.0.0.1:${webPort}`;
await writeFile(join(dir,'config.json'),JSON.stringify({docker:{host:`unix://${dir}/absent.sock`},remotes:[{name:'test-relay',host:'127.0.0.1',port:relayPort,user:'test',auth:{method:'password',password:'isolated-fixture'}}],groups:[{name:'development',mappings:[{name:'dev-ssh',type:'ssh',remote:'test-relay',listen_port:19022,local_port:22,description:'开发环境 SSH 管理入口'},{name:'tom-ssh',type:'multipass',instance:'test-vm',instance_port:22,listen_port:19023,description:'虚拟机独立测试环境'},{name:'nginx-web',type:'docker',container:'nginx',container_port:80,listen_port:19080,description:'容器 Web 服务',expires_at:'2000-01-01T00:00:00Z'}]}]}),{mode:0o600});
const proc=spawn(process.argv[2],['--data-dir',dir,'--api-addr',`127.0.0.1:${apiPort}`,'serve','--web-addr',`127.0.0.1:${webPort}`],{stdio:['ignore','ignore','pipe'],env:{...process.env,HOME:dir}});
let stderr='';proc.stderr.on('data',b=>stderr+=b.toString());let browser;
try{
 let ready=false;for(let i=0;i<100;i++){try{const r=await fetch(base);if(r.ok){ready=true;break;}}catch{}await new Promise(r=>setTimeout(r,100));}
 assert.ok(ready,'isolated web server failed to start: '+stderr);
 const password=(await readFile(join(dir,'web-initial-password'),'utf8')).trim();
 browser=await chromium.launch({headless:true,executablePath:process.env.PORTMAP_CHROMIUM||undefined,args:['--no-sandbox','--disable-dev-shm-usage']});
 const page=await browser.newPage({viewport:{width:1440,height:1000},deviceScaleFactor:1});
 const errors=[];page.on('pageerror',e=>errors.push(e.message));page.on('console',m=>{if(m.type()==='error'&&!m.text().includes('401 (Unauthorized)')&&!m.text().includes('400 (Bad Request)'))errors.push(m.text());});
 await page.goto(base);await page.locator('#login-page').waitFor({state:'visible'});
 await page.screenshot({path:join(shots,'login-desktop.png'),fullPage:true});
 await page.setViewportSize({width:390,height:844});await page.screenshot({path:join(shots,'login-mobile.png'),fullPage:true});
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'mobile login overflows');
 await page.setViewportSize({width:1440,height:1000});
 await page.locator('#password').fill(password);await page.locator('#login-form button[type=submit]').click();
 await page.locator('#workspace').waitFor({state:'visible'});await page.waitForFunction(()=>document.querySelector('#stat-total').textContent==='3');
 await page.screenshot({path:join(shots,'dashboard-desktop.png'),fullPage:true});
 assert.equal(await page.locator('#stat-attention').textContent(),'1');
 await page.locator('#search').fill('开发环境');assert.equal(await page.locator('#mapping-rows tr').count(),1);await page.locator('#search').fill('');
 await page.locator('[data-type="multipass"]').click();assert.equal(await page.locator('#mapping-rows tr').count(),1);await page.locator('[data-type="all"]').click();
 await page.locator('#new-mapping').click();await page.locator('#new-name').fill('browser-test');await page.locator('#new-target-port').fill('22');await page.locator('#new-description').fill('浏览器验收临时隧道');await page.locator('input[name="start"]').uncheck();await page.locator('#modal button[type=submit]').click();await page.waitForFunction(()=>document.querySelector('#stat-total').textContent==='4');
 const testRow=page.locator('#mapping-rows tr').filter({hasText:'browser-test'});
 const selectedPort=Number((await testRow.locator('.port-num').textContent()).slice(1));assert.ok(selectedPort>=10000&&selectedPort<=65535,'automatic relay port must have five digits');
 // A port owned by the relay SSH listener itself is genuinely unavailable.
 await page.locator('#new-mapping').click();await page.locator('#new-name').fill('occupied-port');await page.locator('#new-port').fill(String(relayPort));await page.locator('#new-target-port').fill('22');await page.locator('input[name="start"]').uncheck();await page.locator('#modal button[type=submit]').click();await page.locator('[data-form-error]').waitFor({state:'visible'});assert.match(await page.locator('[data-form-error]').textContent(),/不可用/);await page.locator('#modal-close').click();assert.equal(await page.locator('#stat-total').textContent(),'4');

 await testRow.getByRole('button',{name:'browser-test',exact:true}).click();await page.locator('[data-detail-action="describe"]').click();await page.locator('#description').fill('已通过真实浏览器验收');await page.locator('#modal button[type=submit]').click();await page.waitForFunction(()=>document.querySelector('#mapping-rows').textContent.includes('已通过真实浏览器验收'));
 await testRow.getByRole('button',{name:'设置 browser-test 的有效期',exact:true}).click();await page.locator('#modal button[type=submit]').click();await page.waitForFunction(()=>document.querySelector('#mapping-rows').textContent.includes('7 天后到期'));
 await testRow.getByRole('button',{name:'browser-test',exact:true}).click();await page.locator('[data-detail-action="renew"]').click();await page.locator('#renew-mode').selectOption('forever');await page.locator('#modal button[type=submit]').click();await page.locator('#modal').waitFor({state:'hidden'});
 await page.locator('[data-view="remotes"]').click();await page.locator('#new-remote').click();await page.locator('#remote-name').fill('browser-relay');await page.locator('#remote-user').fill('test');await page.locator('#remote-host').fill('127.0.0.2');await page.locator('#remote-secret').fill('/tmp/test-only-key');await page.locator('#modal button[type=submit]').click();await page.waitForFunction(()=>document.querySelector('#remote-cards').textContent.includes('browser-relay'));
 const remote=page.locator('.remote-card').filter({hasText:'browser-relay'});await remote.getByRole('button',{name:'删除 browser-relay'}).click();await page.locator('#modal button[type=submit]').click();await page.waitForFunction(()=>!document.querySelector('#remote-cards').textContent.includes('browser-relay'));
 await page.locator('[data-view="activity"]').click();assert.ok(await page.locator('.activity-row').count()>=5);
 await page.locator('[data-view="tunnels"]').click();await testRow.getByRole('button',{name:'browser-test',exact:true}).click();await page.locator('[data-detail-action="delete"]').click();await page.locator('#modal button[type=submit]').click();await page.waitForFunction(()=>document.querySelector('#stat-total').textContent==='3');
 await page.waitForFunction(()=>document.querySelector('#toast-container').childElementCount===0);await page.setViewportSize({width:390,height:844});await page.locator('#top-avatar').click();await page.locator('#modal-close').click();await page.screenshot({path:join(shots,'dashboard-mobile.png'),fullPage:true});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'mobile dashboard overflows');
 await page.setViewportSize({width:1440,height:1000});await page.locator('#account-button').click();await page.locator('#current-password').fill(password);await page.locator('#next-password').fill('browser-new-password-2026');await page.locator('#confirm-password').fill('browser-new-password-2026');await page.locator('#modal button[type=submit]').click();await page.locator('#login-page').waitFor({state:'visible'});
 await page.locator('#password').fill('browser-new-password-2026');await page.locator('#login-form button[type=submit]').click();await page.locator('#workspace').waitFor({state:'visible'});
 await page.locator('#account-button').click();await page.locator('#logout-button').click();await page.locator('#login-page').waitFor({state:'visible'});
 assert.deepEqual(errors,[],'browser errors');console.log('PASS: login, mobile layouts, real mappings, search/filter, create, description, renewal, remotes, deletion, password change, logout');console.log('Screenshots: '+shots);
}finally{if(browser)await browser.close();proc.kill('SIGTERM');await new Promise(r=>proc.once('exit',r));relay.kill('SIGINT');await once(relay,'exit');await rm(dir,{recursive:true,force:true});}
