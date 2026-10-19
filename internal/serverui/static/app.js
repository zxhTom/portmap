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
function view(loggedIn){$('#login').hidden=loggedIn;$('#dashboard').hidden=!loggedIn;$('#logout').hidden=!loggedIn;}
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
  const last=document.createElement('small');last.textContent=`客户端状态：${record.status} · 最近上报 ${new Date(record.last_seen).toLocaleString('zh-CN')}`;status.append(last);
  const actions=cell(tr,'');const button=document.createElement('button');button.className=record.disabled?'secondary':'';button.textContent=record.disabled?'重新启用':'禁用';button.addEventListener('click',async()=>{button.disabled=true;$('#notice').textContent='';try{await request(`/api/tunnels/${record.owner}/${record.disabled?'enable':'disable'}`,'POST');await refresh();}catch(e){$('#notice').textContent=e.message;}finally{button.disabled=false;}});actions.append(button);
  body.append(tr);
 }
}
async function refresh(){try{render(await request('/api/tunnels'));$('#notice').textContent='';}catch(e){$('#notice').textContent=e.message;if(e.message==='请先登录'){session=null;view(false);}}}
$('#login-form').addEventListener('submit',async event=>{event.preventDefault();const form=event.currentTarget;const data=new FormData(form);$('#login-error').textContent='';try{await request('/auth/login','POST',{username:data.get('username'),password:data.get('password')});session=await request('/auth/session');form.reset();view(true);await refresh();}catch(e){$('#login-error').textContent=e.message;}});
$('#refresh').addEventListener('click',refresh);
$('#logout').addEventListener('click',async()=>{try{await request('/auth/logout','POST');}finally{session=null;view(false);}});
(async()=>{try{session=await request('/auth/session');view(true);await refresh();}catch{view(false);}})();
setInterval(()=>{if(session&&!document.hidden)refresh();},15000);
