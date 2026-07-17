const state = {config:null, models:[], providers:[], credentials:[], catalog:{}, health:[], summary:{}};
const labels = {chat:'通用对话','chat-tools':'工具调用',embedding:'向量嵌入',audio:'音频',image:'图像',video:'视频',rerank:'重排序',moderation:'内容审核'};
const modelTypes = ['chat','chat-tools','embedding','audio','image','video','rerank','moderation'];
let dirty = false;
const $ = selector => document.querySelector(selector);
const esc = value => String(value ?? '').replace(/[&<>"']/g, character => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[character]));

async function api(path, options={}) {
  const response = await fetch('/admin/api/' + path, {headers:{'Content-Type':'application/json'}, ...options});
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || '请求失败');
  return data;
}

function toast(message, error=false) {
  const node = $('#toast');
  node.textContent = message;
  node.className = error ? 'show error' : 'show';
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => node.className = '', 2600);
}

function markDirty() {
  dirty = true;
  $('#save').textContent = '保存更改';
  $('#save').classList.add('dirty');
}

function clearDirty() {
  dirty = false;
  $('#save').textContent = '已保存';
  $('#save').classList.remove('dirty');
  setTimeout(() => { if (!dirty) $('#save').textContent = '保存路由配置'; }, 1200);
}

async function load() {
  try {
    const data = await api('state');
    Object.assign(state, data);
    state.models = state.models || [];
    state.credentials = state.credentials || [];
    state.health = state.health || [];
    state.summary = state.summary || {};
    state.config.models = state.config.models || {};
    Object.values(state.config.routes).forEach(route => route.models = route.models || []);
    render();
  } catch (error) { toast(error.message, true); }
}

function effectiveModel(model) {
  const override = state.config.models[model.id] || {};
  const capabilities = {...model.capabilities, tool_call:override.tool_call ?? model.capabilities?.tool_call, vision:override.vision ?? model.capabilities?.vision, reasoning:override.reasoning ?? model.capabilities?.reasoning};
  const selectedType = override.type || (model.type === 'normal' ? 'chat' : model.type);
  const internalType = selectedType === 'chat' || selectedType === 'chat-tools' ? 'normal' : selectedType;
  const parameters = model.supported_parameters || [];
  let supportsTools = override.tool_call ?? (selectedType === 'chat-tools' || (model.capabilities?.tool_call_known ? model.capabilities.tool_call : parameters.length === 0 || parameters.includes('tools')));
  const routeTypes = internalType === 'normal' ? ['chat', ...(supportsTools ? ['chat-tools'] : [])] : [selectedType];
  return {...model, disabled:Boolean(override.disabled), type:internalType, route_types:routeTypes, capabilities};
}

function render() {
  const requests = state.summary.requests || 0;
  const rate = requests ? Math.round(((state.summary.successes || 0) / requests) * 100) : null;
  $('#model-count').textContent = state.models.length;
  $('#provider-count').textContent = state.providers.filter(provider => provider.configured).length;
  $('#route-count').textContent = Object.keys(state.config.routes).length;
  $('#success-rate').textContent = rate === null ? '—' : rate + '%';
  $('#request-status').textContent = requests ? `${requests} 次请求 · ${state.summary.cooling || 0} 个模型冷却中` : '尚无推理请求';
  $('#cache-status').textContent = state.catalog.updated_at ? '缓存更新于 ' + new Date(state.catalog.updated_at).toLocaleString() : '尚未生成模型缓存';
  renderRoutes(); renderProviders(); renderTypeFilter(); renderModels();
}

function eligible(route) {
  return state.models.map(effectiveModel).filter(model => !model.disabled && model.route_types.includes(route.type));
}

function renderRoutes() {
  const byID = new Map(state.models.map(model => [model.id, effectiveModel(model)]));
  $('#route-grid').innerHTML = Object.entries(state.config.routes).sort(([a],[b]) => a.localeCompare(b)).map(([alias, route]) => {
    const options = eligible(route).filter(model => !route.models.includes(model.id)).map(model => `<option value="${esc(model.id)}">${esc(model.id)}</option>`).join('');
    const rows = route.models.map((id, index) => {
      const model = byID.get(id);
      const unavailable = !model || model.disabled || !model.route_types.includes(route.type);
      return `<div class="model-row ${unavailable?'unavailable':''}" draggable="true" data-route="${esc(alias)}" data-index="${index}"><span class="grip">⠿</span><span class="model-name" title="${esc(id)}">${index+1}. ${esc(id)}${unavailable?' · 不可用':''}</span><span class="row-actions"><button class="icon-button" data-move="up" data-route="${esc(alias)}" data-index="${index}" aria-label="上移">↑</button><button class="icon-button" data-move="down" data-route="${esc(alias)}" data-index="${index}" aria-label="下移">↓</button><button class="icon-button danger" data-remove="1" data-route="${esc(alias)}" data-index="${index}" aria-label="删除">×</button></span></div>`;
    }).join('');
    return `<article class="route-card"><div class="route-head"><div><h3>${esc(alias)}</h3><p>${esc(labels[alias] || alias)} · 客户端 model 字符串</p></div><span class="type-pill ${route.type==='chat-tools'?'tools':''}">${esc(route.type)}</span></div><div class="fallback-list">${rows || '<div class="auto-route"><b>AUTO</b> 根据能力、禁用状态和健康度动态选择</div>'}</div><div class="add-model"><select aria-label="为 ${esc(alias)} 添加模型"><option value="">选择缓存模型…</option>${options}</select><button data-add="${esc(alias)}">添加</button></div></article>`;
  }).join('');
  bindRouteEvents();
}

function bindRouteEvents() {
  document.querySelectorAll('[data-add]').forEach(button => button.onclick = () => {const select=button.previousElementSibling;if(select.value){state.config.routes[button.dataset.add].models.push(select.value);markDirty();renderRoutes();}});
  document.querySelectorAll('[data-remove]').forEach(button => button.onclick = () => {state.config.routes[button.dataset.route].models.splice(+button.dataset.index,1);markDirty();renderRoutes();});
  document.querySelectorAll('[data-move]').forEach(button => button.onclick = () => {const list=state.config.routes[button.dataset.route].models,index=+button.dataset.index,target=button.dataset.move==='up'?index-1:index+1;if(target>=0&&target<list.length){[list[index],list[target]]=[list[target],list[index]];markDirty();renderRoutes();}});
  let dragged;
  document.querySelectorAll('.model-row').forEach(row => {row.ondragstart=()=>{dragged={route:row.dataset.route,index:+row.dataset.index};row.classList.add('dragging')};row.ondragend=()=>row.classList.remove('dragging');row.ondragover=event=>event.preventDefault();row.ondrop=event=>{event.preventDefault();if(!dragged||dragged.route!==row.dataset.route)return;const list=state.config.routes[dragged.route].models,[item]=list.splice(dragged.index,1);list.splice(+row.dataset.index,0,item);markDirty();renderRoutes();};});
}

function renderProviders() {
  const saved = new Set(state.credentials.map(item => item.provider));
  $('#provider-grid').innerHTML = state.providers.map(provider => `<article class="provider-card"><div class="provider-head"><strong>${esc(provider.id)}</strong><span class="provider-state ${provider.configured?'on':''}">${provider.configured?'● 已配置':'○ 未配置'}</span></div><p>${esc(provider.tier || 'free tier')} · ${esc(provider.env)} · ${provider.source==='environment'?'环境变量':provider.source==='saved'?'安全存储':'无凭据'}</p><div class="credential-row"><input type="password" autocomplete="new-password" placeholder="粘贴 API Key" data-key="${esc(provider.id)}"><button data-save-key="${esc(provider.id)}">保存</button>${saved.has(provider.id)?`<button class="secondary" data-delete-key="${esc(provider.id)}">删除</button>`:''}</div>${provider.configured?`<div class="provider-actions"><button class="secondary" data-test-provider="${esc(provider.id)}">测试连接与模型目录</button></div>`:''}</article>`).join('');
  document.querySelectorAll('[data-save-key]').forEach(button => button.onclick = async () => {const input=document.querySelector(`[data-key="${CSS.escape(button.dataset.saveKey)}"]`);if(!input.value)return toast('请输入 API Key',true);button.disabled=true;try{await api('credentials',{method:'POST',body:JSON.stringify({provider:button.dataset.saveKey,api_key:input.value})});input.value='';toast('凭据已保存，免费源与模型目录已热加载');await load();}catch(error){toast(error.message,true)}finally{button.disabled=false;}});
  document.querySelectorAll('[data-delete-key]').forEach(button => button.onclick = async () => {button.disabled=true;try{await api('credentials/'+encodeURIComponent(button.dataset.deleteKey),{method:'DELETE'});toast('凭据已删除，路由目录已更新');await load();}catch(error){toast(error.message,true)}finally{button.disabled=false;}});
  document.querySelectorAll('[data-test-provider]').forEach(button => button.onclick = async () => {button.disabled=true;const original=button.textContent;button.textContent='测试中…';try{const result=await api('providers/'+encodeURIComponent(button.dataset.testProvider)+'/test',{method:'POST'});toast(`${result.provider} 正常 · ${result.models} 个模型 · ${result.latency_ms}ms`);}catch(error){toast(error.message,true)}finally{button.disabled=false;button.textContent=original;}});
}

function renderTypeFilter() {
  const select=$('#type-filter'), current=select.value, types=[...new Set(state.models.flatMap(model=>effectiveModel(model).route_types))].sort();
  select.innerHTML='<option value="">全部类型</option>'+types.map(type=>`<option value="${esc(type)}">${esc(type)}</option>`).join('');
  select.value=current;
}

function triStateSelect(model, field, value) {
  return `<label>${field}<select data-override="${field}" data-model="${esc(model.id)}"><option value="auto" ${value===undefined?'selected':''}>自动</option><option value="true" ${value===true?'selected':''}>支持</option><option value="false" ${value===false?'selected':''}>不支持</option></select></label>`;
}

function renderModels() {
  const query=$('#search').value.toLowerCase(), type=$('#type-filter').value, healthByID=new Map(state.health.map(item=>[item.model,item]));
  const models=state.models.map(model=>({raw:model,effective:effectiveModel(model)})).filter(({effective:model})=>(!type||model.route_types.includes(type))&&(!query||model.id.toLowerCase().includes(query)||model.provider.toLowerCase().includes(query)));
  $('#model-table').innerHTML=models.map(({raw,effective:model})=>{
    const override=state.config.models[model.id]||{}, caps=[];
    if(model.capabilities?.tool_call)caps.push('<span class="cap yes">tools</span>');
    if(model.capabilities?.vision)caps.push('<span class="cap yes">vision</span>');
    if(model.capabilities?.reasoning)caps.push('<span class="cap">reasoning</span>');
    const health=healthByID.get(model.id)||{status:'unknown'};
    const typeOptions=['',...modelTypes].map(value=>`<option value="${value}" ${(override.type||'')===value?'selected':''}>${value||'自动识别'}</option>`).join('');
    const routeTypePills=model.route_types.map(routeType=>`<span class="type-pill ${routeType==='chat-tools'?'tools':''}">${esc(routeType)}</span>`).join(' ');
    return `<tr class="${model.disabled?'disabled':''}"><td class="table-model"><strong>${esc(model.id)}</strong><small>${esc(model.name||model.upstream_id)}</small></td><td>${routeTypePills}</td><td><div class="caps">${caps.join('')||'<span class="cap">standard</span>'}</div></td><td><span class="health-pill ${esc(health.status)}" title="${esc(health.last_error||'')}">${esc(health.status)}</span></td><td>${model.context_length?Number(model.context_length).toLocaleString():'—'}</td><td>${esc(model.provider)}</td><td><details class="override-details"><summary>设置</summary><div class="override-panel"><label>路由类型<select data-override="type" data-model="${esc(model.id)}">${typeOptions}</select></label>${triStateSelect(raw,'tool_call',override.tool_call)}${triStateSelect(raw,'vision',override.vision)}${triStateSelect(raw,'reasoning',override.reasoning)}<button class="${model.disabled?'':'secondary'}" data-toggle-model="${esc(model.id)}">${model.disabled?'重新启用':'禁用模型'}</button></div></details></td></tr>`;
  }).join('')||'<tr><td colspan="7" class="empty">没有匹配的缓存模型</td></tr>';
  document.querySelectorAll('[data-override]').forEach(select=>select.onchange=()=>{const id=select.dataset.model,field=select.dataset.override,override=state.config.models[id]||{};if(select.value==='auto'||select.value===''){delete override[field]}else if(field==='type'){override[field]=select.value}else{override[field]=select.value==='true'}state.config.models[id]=override;markDirty();renderRoutes();renderModels();});
  document.querySelectorAll('[data-toggle-model]').forEach(button=>button.onclick=()=>{const id=button.dataset.toggleModel,override=state.config.models[id]||{};override.disabled=!override.disabled;state.config.models[id]=override;markDirty();renderRoutes();renderModels();});
}

$('#save').onclick=async()=>{const button=$('#save');button.disabled=true;try{const data=await api('config',{method:'PUT',body:JSON.stringify(state.config)});state.config=data.config;clearDirty();toast('配置已保存并即时生效');render();}catch(error){toast(error.message,true)}finally{button.disabled=false;}};
$('#refresh').onclick=async()=>{const button=$('#refresh'),original=button.textContent;button.disabled=true;button.textContent='正在刷新…';try{const data=await api('refresh',{method:'POST'});toast(`模型目录已刷新，共 ${data.models} 个模型`);await load();}catch(error){toast(error.message,true)}finally{button.disabled=false;button.textContent=original;}};
$('#search').oninput=renderModels;
$('#type-filter').onchange=renderModels;
window.addEventListener('beforeunload',event=>{if(dirty){event.preventDefault();event.returnValue='';}});
load();
