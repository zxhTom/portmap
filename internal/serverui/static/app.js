'use strict';
const $ = selector => document.querySelector(selector);
let session = null;
async function request(path, method='GET', body) {
 const headers = {};
 if (body !== undefined) headers['Content-Type']='application/json';
 if (method !== 'GET' && session) headers['X-CSRF-Token']=session.csrf_token;
 const response=await fetch(path,{method,headers,credentials:'same-origin',body:body===undefined?undefined:JSON.stringify(body)});
 const data=await response.json().catch(()=>({error:'服务器响应无效'}));
 if(!response.ok) throw new Error(data.error||`请求失败 ${response.status}`);
 return data;
}
function view(loggedIn){
 $('#login').hidden=loggedIn;
 const shell=$('#dashboard-shell');
 if(shell) shell.hidden=!loggedIn;
 $('#dashboard').hidden=!loggedIn;
 $('#logout').hidden=!loggedIn;
}
function cell(row, value){const td=document.createElement('td');td.textContent=value;row.append(td);return td;}
function render(records){
 const body=$('#rows');body.replaceChildren();
 $('#empty').hidden=records.length!==0;
 const online=records.filter(r=>r.online).length, disabled=records.filter(r=>r.disabled).length;
 $('#summary').textContent=`${records.length} 条映射 · ${online} 条在线 · ${disabled} 条已禁用`;
 for(const record of records){
  const tr=document.createElement('tr');
  const title=cell(tr,`${record.client_name||record.client_id} / ${record.group} / ${record.name}`);
  const description=document.createElement('small');description.textContent=record.description||record.type;title.append(description);
  const entry=cell(tr,`${record.listen_host}:${record.listen_port}`);
  for(const domain of record.domains||[]){const a=document.createElement('a');a.href=`https://${domain}`;a.target='_blank';a.rel='noopener noreferrer';a.textContent=domain;const small=document.createElement('small');small.append(a);entry.append(small);}
  cell(tr,record.target);
  const status=cell(tr,'');const badge=document.createElement('span');badge.className='badge '+(record.disabled?'disabled':record.online?'':'offline');badge.textContent=record.disabled?'已禁用':record.online?'在线':'离线';status.append(badge);
  const last=document.createElement('small');last.textContent=`客户端状态：${record.status} · 控制通道${record.client_online===false?'离线':'在线'} · 最近上报 ${new Date(record.last_seen).toLocaleString('zh-CN')}`;status.append(last);
  const actions=cell(tr,'');const button=document.createElement('button');button.className=record.disabled?'secondary':'';button.textContent=record.disabled?'重新启用':'禁用';button.addEventListener('click',async()=>{button.disabled=true;$('#notice').textContent='';try{await request(`/api/tunnels/${record.owner}/${record.disabled?'enable':'disable'}`,'POST');await refresh();}catch(e){$('#notice').textContent=e.message;}finally{button.disabled=false;}});actions.append(button);
  const recover=document.createElement('button');recover.className='secondary';
  const expired=record.expires_at&&Date.parse(record.expires_at)<=Date.now();
  recover.textContent=record.online?'重新连接':'唤醒隧道';recover.disabled=record.disabled||expired;
  recover.title=record.disabled?'请先重新启用此隧道':expired?'请先在客户端续期隧道':record.client_online===false?'保存唤醒请求，等待客户端重新连接':record.invalid?'手动唤醒会清除失败作废状态并重新启动':'请求客户端启动或重建此隧道';
  recover.addEventListener('click',async()=>{recover.disabled=true;try{const result=await request(`/api/tunnels/${record.owner}/recover`,'POST');await refresh();$('#notice').textContent=result.message||'唤醒请求已保存，等待客户端连接并执行。';}catch(e){$('#notice').textContent=e.message;recover.disabled=false;}});actions.append(recover);
  if(record.disabled||expired){const reason=document.createElement('small');reason.textContent=recover.title;actions.append(reason);}

  if(record.recovery_status){const recovery=document.createElement('small');const labels={pending:'等待执行',waiting_client:'等待客户端连接',waiting_upgrade:'等待客户端升级',restarted:'已重建，等待探测',healthy:'恢复成功',failed:'重建失败',skipped:'策略阻止恢复',expired:'指令已过期'};recovery.textContent=`${labels[record.recovery_status]||record.recovery_status} · 尝试 ${record.recovery_attempts} 次 · ${record.recovery_message||''}`;status.append(recovery);}
  if(record.probe_error){const probe=document.createElement('small');probe.textContent=record.probe_error;status.append(probe);}
  body.append(tr);
 }
}
async function refresh(){try{render(await request('/api/tunnels'));$('#notice').textContent='';}catch(e){$('#notice').textContent=e.message;if(e.message==='请先登录'){session=null;view(false);}}}
$('#login-form').addEventListener('submit',async event=>{event.preventDefault();const form=event.currentTarget;const data=new FormData(form);$('#login-error').textContent='';$('#login-error').hidden=true;try{await request('/auth/login','POST',{username:data.get('username'),password:data.get('password'),remember_me:form.querySelector('input[name="remember_me"]')?.checked||false});session=await request('/auth/session');form.reset();view(true);await refresh();await loadZones();}catch(e){$('#login-error').textContent=e.message;$('#login-error').hidden=false;}});
const pwdToggle=$('.password-toggle');
if(pwdToggle){pwdToggle.addEventListener('click',()=>{const input=$('#password');if(input){input.type=input.type==='password'?'text':'password';pwdToggle.setAttribute('aria-label',input.type==='password'?'显示密码':'隐藏密码');}});}
$('#refresh').addEventListener('click',refresh);
$('#logout').addEventListener('click',async()=>{try{await request('/auth/logout','POST');}finally{session=null;view(false);}});
(async()=>{try{session=await request('/auth/session');view(true);await refresh();await loadZones();}catch{view(false);}})();
setInterval(()=>{if(session&&!document.hidden)refresh();},15000);

function serverZoneOptions(zones,primary){const select=$('#server-primary-zone');select.replaceChildren();for(const zone of zones){const option=document.createElement('option');option.value=zone;option.textContent=zone;option.selected=zone===primary;select.append(option);}}
async function loadZones(){try{const data=await request('/api/domain-zones');$('#server-zones').value=data.domain_zones.join('\n');serverZoneOptions(data.domain_zones,data.domain_zone);}catch(e){$('#zones-notice').textContent=e.message;}}
$('#server-zones').addEventListener('input',()=>serverZoneOptions($('#server-zones').value.split(/[,，\s]+/).filter(Boolean),$('#server-primary-zone').value));
$('#zones-form').addEventListener('submit',async e=>{e.preventDefault();const button=e.currentTarget.querySelector('button');button.disabled=true;try{await request('/api/domain-zones','PUT',{domain_zones:$('#server-zones').value.split(/[,，\s]+/).filter(Boolean),domain_zone:$('#server-primary-zone').value});$('#zones-notice').textContent='已保存，已有隧道地址不变。';await loadZones();}catch(error){$('#zones-notice').textContent=error.message;}finally{button.disabled=false;}});
