import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  DndContext, KeyboardSensor, PointerSensor, closestCenter, useSensor, useSensors, type DragEndEvent,
} from '@dnd-kit/core'
import { SortableContext, arrayMove, sortableKeyboardCoordinates, useSortable, verticalListSortingStrategy } from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import * as Dialog from '@radix-ui/react-dialog'
import * as Popover from '@radix-ui/react-popover'
import * as Tooltip from '@radix-ui/react-tooltip'
import {
  Activity, AlertTriangle, ArrowDownUp, Blocks, Bot, Check, ChevronRight, CircleHelp,
  Database, ExternalLink, Eye, EyeOff, Gauge, GripVertical, KeyRound, LogIn, Menu, Moon,
  Network, Plus, RefreshCw, Route as RouteIcon, Save, Search, Server, Settings2,
  ShieldCheck, Sparkles, Sun, Trash2, Unplug, X,
} from 'lucide-react'
import { api } from './api'
import type {
  AppState, EffectiveModel, HealthProbeStatus, HealthState, Model, ModelOverride, ProviderStatus, RouteType, RouterConfig, RuntimeStatus,
} from './types'

type Page = 'overview' | 'routes' | 'models' | 'providers' | 'system'

const routeLabels: Record<string, { title: string; description: string }> = {
  chat: { title: '通用对话', description: '标准文本聊天与流式输出' },
  'chat-tools': { title: '工具调用', description: '需要 function/tool call 的任务' },
  'image-understanding': { title: '图片理解', description: '读取图片并输出文本' },
  'image-generation': { title: '图片生成', description: '根据文本或图片生成图片' },
  'video-understanding': { title: '视频理解', description: '读取视频并输出文本' },
  'video-generation': { title: '视频生成', description: '根据文本或图片生成视频' },
  'audio-understanding': { title: '音频理解', description: '理解音频内容并回答' },
  'speech-to-text': { title: '语音转文字', description: '语音转录与翻译' },
  'text-to-speech': { title: '文字转语音', description: '将文本合成为语音' },
  embedding: { title: '向量嵌入', description: '文本向量化与语义检索' },
  rerank: { title: '重排序', description: '检索结果相关性排序' },
  moderation: { title: '内容审核', description: '内容安全分类' },
}

const pageInfo: Record<Page, { title: string; subtitle: string }> = {
  overview: { title: '运行概览', subtitle: '服务、模型目录和路由健康状态' },
  routes: { title: '路由策略', subtitle: '配置固定能力名称的 fallback 优先级' },
  models: { title: '模型目录', subtitle: '浏览缓存模型、能力元数据和健康状态' },
  providers: { title: '免费源', subtitle: '管理凭据、连接状态和模型同步' },
  system: { title: '系统状态', subtitle: '查看守护进程、缓存和本地配置位置' },
}

const routeOrder: RouteType[] = ['chat', 'chat-tools', 'image-understanding', 'image-generation', 'video-understanding', 'video-generation', 'audio-understanding', 'speech-to-text', 'text-to-speech', 'embedding', 'rerank', 'moderation']

const navigation: { id: Page; label: string; icon: typeof Gauge }[] = [
  { id: 'overview', label: '概览', icon: Gauge },
  { id: 'routes', label: '路由', icon: RouteIcon },
  { id: 'models', label: '模型', icon: Database },
  { id: 'providers', label: '免费源', icon: Network },
  { id: 'system', label: '系统', icon: Settings2 },
]

const cloneConfig = (config: RouterConfig): RouterConfig => JSON.parse(JSON.stringify(config))
const cx = (...classes: Array<string | false | null | undefined>) => classes.filter(Boolean).join(' ')

function effectiveModel(model: Model, config: RouterConfig): EffectiveModel {
  const override = config.models?.[model.id] || {}
  const parameters = model.supported_parameters || []
  const toolCall = override.tool_call ?? (model.capabilities.tool_call_known
    ? model.capabilities.tool_call
    : parameters.includes('tools'))
  let functions = [...(override.functions || model.functions || [])]
  if (override.tool_call === false) functions = functions.filter(item => item !== 'chat-tools')
  if (toolCall && functions.includes('chat') && !functions.includes('chat-tools')) functions.push('chat-tools')
  return {
    ...model,
    disabled: Boolean(override.disabled),
    functions: functions as RouteType[],
    route_types: functions as RouteType[],
    capabilities: {
      ...model.capabilities,
      tool_call: toolCall,
      vision: override.vision ?? model.capabilities.vision,
      reasoning: override.reasoning ?? model.capabilities.reasoning,
    },
  }
}

const healthKey = (model: string, capability: string) => `${model}\u0000${capability}`
const modelHealth = (model: EffectiveModel, healthMap: Map<string, HealthState>): HealthState | undefined => {
  const states = model.route_types.map(capability => healthMap.get(healthKey(model.id, capability))).filter(Boolean) as HealthState[]
  return states.find(item => item.status === 'failed') || (states.length && states.every(item => item.status === 'healthy') ? states[0] : undefined)
}

function formatNumber(value?: number) { return Number(value || 0).toLocaleString() }
function formatDate(value?: string) { return value ? new Date(value).toLocaleString() : '尚未更新' }
function formatUptime(seconds = 0) {
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days) return `${days} 天 ${hours} 小时`
  if (hours) return `${hours} 小时 ${minutes} 分钟`
  if (minutes) return `${minutes} 分钟`
  return `${Math.max(0, Math.floor(seconds))} 秒`
}

function IconButton({ label, children, onClick, disabled }: { label: string; children: ReactNode; onClick?: () => void; disabled?: boolean }) {
  return <Tooltip.Root><Tooltip.Trigger asChild><button className="icon-button" aria-label={label} onClick={onClick} disabled={disabled}>{children}</button></Tooltip.Trigger><Tooltip.Portal><Tooltip.Content className="tooltip" sideOffset={7}>{label}<Tooltip.Arrow className="tooltip-arrow" /></Tooltip.Content></Tooltip.Portal></Tooltip.Root>
}

function StatusBadge({ status }: { status: HealthState['status'] | 'configured' | 'missing' }) {
  const labels: Record<string, string> = { healthy: '健康', failed: '故障 · 已隔离', unknown: '未测试', configured: '已配置', missing: '未配置' }
  return <span className={`status-badge ${status}`}><span />{labels[status] || status}</span>
}

function CapabilityPills({ model }: { model: EffectiveModel }) {
  return <div className="pill-row">
    {model.capabilities.tool_call && <span className="capability-pill accent">Tools</span>}
    {model.capabilities.vision && <span className="capability-pill">Vision</span>}
    {model.capabilities.reasoning && <span className="capability-pill">Reasoning</span>}
    {!model.capabilities.tool_call && !model.capabilities.vision && !model.capabilities.reasoning && <span className="capability-pill muted">Standard</span>}
  </div>
}

function App() {
  const queryClient = useQueryClient()
  const [page, setPage] = useState<Page>('overview')
  const [mobileNav, setMobileNav] = useState(false)
  const [dark, setDark] = useState(() => localStorage.getItem('free-router-theme') === 'dark')
  const [draft, setDraft] = useState<RouterConfig | null>(null)
  const [baseline, setBaseline] = useState<RouterConfig | null>(null)
  const [toast, setToast] = useState<{ message: string; error?: boolean } | null>(null)

  const stateQuery = useQuery({ queryKey: ['state'], queryFn: api.state, refetchInterval: 5000 })
  const runtimeQuery = useQuery({ queryKey: ['runtime'], queryFn: api.runtime, refetchInterval: 5000, retry: false })

  useEffect(() => {
    if (stateQuery.data && !draft) {
      setDraft(cloneConfig(stateQuery.data.config))
      setBaseline(cloneConfig(stateQuery.data.config))
    }
  }, [stateQuery.data, draft])

  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
    localStorage.setItem('free-router-theme', dark ? 'dark' : 'light')
  }, [dark])

  useEffect(() => {
    if (!toast) return
    const timer = window.setTimeout(() => setToast(null), 3200)
    return () => window.clearTimeout(timer)
  }, [toast])

  useEffect(() => {
    const params = new URLSearchParams(window.location.search)
    const status = params.get('oauth_status')
    if (!status) return
    setPage('providers')
    setToast(status === 'success'
      ? { message: 'OpenRouter 已登录，API Key 已安全保存并启用' }
      : { message: params.get('oauth_message') || 'OpenRouter 登录失败', error: true })
    window.history.replaceState({}, '', window.location.pathname)
  }, [])

  const dirty = useMemo(() => Boolean(draft && baseline && JSON.stringify(draft) !== JSON.stringify(baseline)), [draft, baseline])

  useEffect(() => {
    const beforeUnload = (event: BeforeUnloadEvent) => { if (dirty) event.preventDefault() }
    window.addEventListener('beforeunload', beforeUnload)
    return () => window.removeEventListener('beforeunload', beforeUnload)
  }, [dirty])

  const saveMutation = useMutation({
    mutationFn: () => api.saveConfig(draft!),
    onSuccess: ({ config }) => {
      setDraft(cloneConfig(config)); setBaseline(cloneConfig(config)); setToast({ message: '路由配置已保存并即时生效' })
      queryClient.invalidateQueries({ queryKey: ['state'] })
    },
    onError: (error: Error) => setToast({ message: error.message, error: true }),
  })
  const refreshMutation = useMutation({
    mutationFn: api.refresh,
    onSuccess: ({ models }) => { setToast({ message: `模型目录已刷新，共 ${models} 个模型` }); queryClient.invalidateQueries({ queryKey: ['state'] }) },
    onError: (error: Error) => setToast({ message: error.message, error: true }),
  })

  if (stateQuery.isError) return <ErrorScreen error={(stateQuery.error as Error).message} retry={() => stateQuery.refetch()} />
  if (stateQuery.isLoading || !stateQuery.data || !draft) return <LoadingScreen />

  const state = stateQuery.data
  const runtime = runtimeQuery.data || state.runtime
  const models = state.models.map(model => effectiveModel(model, draft))
  const healthMap = new Map(state.health.map(item => [healthKey(item.model, item.capability), item]))
  const info = pageInfo[page]

  return <div className="app-shell">
    <aside className={cx('sidebar', mobileNav && 'mobile-open')}>
      <div className="brand"><div className="brand-mark"><RouteIcon size={18} /></div><div><strong>Free Router</strong><small>Model control plane</small></div></div>
      <nav className="main-nav" aria-label="主要导航">
        {navigation.map(item => <button key={item.id} className={page === item.id ? 'active' : ''} onClick={() => { setPage(item.id); setMobileNav(false) }}><item.icon size={18} /><span>{item.label}</span>{item.id === 'routes' && dirty && <i />}</button>)}
      </nav>
      <div className="sidebar-bottom">
        <div className="sidebar-health"><span className={runtimeQuery.isError ? 'offline-dot' : 'online-dot'} /><div><strong>{runtimeQuery.isError ? '服务连接中断' : '服务运行中'}</strong><small>{runtime.service_manager === 'manual' ? '手动启动' : runtime.service_manager}</small></div></div>
        <div className="version-row"><span>free-router</span><code>v{runtime.version}</code></div>
      </div>
    </aside>
    {mobileNav && <button className="nav-backdrop" aria-label="关闭导航" onClick={() => setMobileNav(false)} />}

    <main className="workspace">
      <header className="topbar">
        <div className="page-heading"><button className="mobile-menu" onClick={() => setMobileNav(true)} aria-label="打开导航"><Menu size={20} /></button><div><h1>{info.title}</h1><p>{info.subtitle}</p></div></div>
        <div className="topbar-actions">
          <RuntimePopover runtime={runtime} offline={runtimeQuery.isError} state={state} />
          <IconButton label={dark ? '切换亮色模式' : '切换暗色模式'} onClick={() => setDark(value => !value)}>{dark ? <Sun size={18} /> : <Moon size={18} />}</IconButton>
          <button className="button secondary compact" onClick={() => refreshMutation.mutate()} disabled={refreshMutation.isPending}><RefreshCw size={16} className={refreshMutation.isPending ? 'spin' : ''} /><span className="desktop-label">刷新模型</span></button>
        </div>
      </header>

      <div className="page-content">
        {page === 'overview' && <Overview state={state} runtime={runtime} models={models} healthMap={healthMap} navigate={setPage} />}
        {page === 'routes' && <RoutesPage config={draft} setConfig={setDraft} models={models} healthMap={healthMap} />}
        {page === 'models' && <ModelsPage config={draft} setConfig={setDraft} models={models} healthMap={healthMap} probe={state.health_probe} />}
        {page === 'providers' && <ProvidersPage state={state} onToast={setToast} />}
        {page === 'system' && <SystemPage state={state} runtime={runtime} offline={runtimeQuery.isError} />}
      </div>

      {dirty && <div className="save-bar"><div><span className="unsaved-dot" /><div><strong>有未保存的路由修改</strong><small>保存后立即对新请求生效</small></div></div><div><button className="button ghost" onClick={() => setDraft(cloneConfig(baseline!))}>放弃修改</button><button className="button" onClick={() => saveMutation.mutate()} disabled={saveMutation.isPending}><Save size={16} />{saveMutation.isPending ? '保存中…' : '保存配置'}</button></div></div>}
    </main>
    {toast && <div className={cx('toast', toast.error && 'error')}><span>{toast.error ? <AlertTriangle size={17} /> : <Check size={17} />}</span>{toast.message}</div>}
  </div>
}

function LoadingScreen() {
  return <div className="loading-screen"><div className="loading-brand"><RouteIcon size={22} /><strong>Free Router</strong></div><div className="loading-line" /><p>正在读取本地路由状态…</p></div>
}

function ErrorScreen({ error, retry }: { error: string; retry: () => void }) {
  return <div className="error-screen"><div className="error-icon"><Unplug size={24} /></div><h1>无法连接管理服务</h1><p>{error}</p><button className="button" onClick={retry}><RefreshCw size={16} />重新连接</button></div>
}

function RuntimePopover({ runtime, offline, state }: { runtime: RuntimeStatus; offline: boolean; state: AppState }) {
  return <Popover.Root><Popover.Trigger asChild><button className={cx('runtime-trigger', offline && 'offline')}><span />{offline ? '已断开' : '运行中'}<ChevronRight size={14} /></button></Popover.Trigger><Popover.Portal><Popover.Content className="runtime-popover" align="end" sideOffset={10}>
    <div className="popover-title"><div className={cx('service-icon', offline && 'offline')}><Server size={18} /></div><div><strong>{offline ? '服务连接已断开' : '服务运行正常'}</strong><small>v{runtime.version} · PID {runtime.pid || '—'}</small></div></div>
    <div className="runtime-grid"><div><span>运行方式</span><strong>{runtime.service_manager === 'launchd' ? 'macOS LaunchAgent' : runtime.service_manager === 'systemd' ? 'systemd user' : '手动启动'}</strong></div><div><span>运行时间</span><strong>{offline ? '—' : formatUptime(runtime.uptime_seconds)}</strong></div><div><span>缓存模型</span><strong>{state.models.length}</strong></div><div><span>累计请求</span><strong>{formatNumber(state.summary.requests)}</strong></div></div>
    <div className="command-hint"><code>free-router daemon status</code></div><Popover.Arrow className="popover-arrow" /></Popover.Content></Popover.Portal></Popover.Root>
}

function MetricCard({ icon, label, value, note, tone = 'default' }: { icon: ReactNode; label: string; value: string | number; note: string; tone?: string }) {
  return <article className="metric-card"><div className={`metric-icon ${tone}`}>{icon}</div><div className="metric-label">{label}</div><strong>{value}</strong><p>{note}</p></article>
}

function Overview({ state, runtime, models, healthMap, navigate }: { state: AppState; runtime: RuntimeStatus; models: EffectiveModel[]; healthMap: Map<string, HealthState>; navigate: (page: Page) => void }) {
  const configured = state.providers.filter(provider => provider.configured).length
  const healthy = models.filter(model => modelHealth(model, healthMap)?.status === 'healthy').length
  const requests = state.summary.requests || 0
  const successRate = requests ? Math.round((state.summary.successes / requests) * 100) : null
  const incidents = state.health.filter(item => item.status === 'failed').sort((a, b) => (b.last_used_at || '').localeCompare(a.last_used_at || '')).slice(0, 5)
  return <div className="stack-xl">
    <section className="metrics-grid">
      <MetricCard icon={<Bot size={19} />} label="缓存模型" value={models.length} note={`${healthy} 个已验证健康`} tone="green" />
      <MetricCard icon={<Network size={19} />} label="已配置免费源" value={`${configured} / ${state.providers.length}`} note="凭据保存在本机安全存储" tone="blue" />
      <MetricCard icon={<Activity size={19} />} label="请求成功率" value={successRate === null ? '—' : `${successRate}%`} note={requests ? `${formatNumber(requests)} 次路由请求` : '等待首个推理请求'} tone="violet" />
      <MetricCard icon={<Server size={19} />} label="服务运行时间" value={formatUptime(runtime.uptime_seconds)} note={`${runtime.service_manager} · PID ${runtime.pid}`} tone="amber" />
    </section>

    <section className="overview-grid">
      <article className="panel route-summary-panel"><div className="panel-heading"><div><span className="eyebrow">ACTIVE ROUTES</span><h2>固定能力路由</h2></div><button className="text-button" onClick={() => navigate('routes')}>编辑策略 <ChevronRight size={15} /></button></div>
        <div className="route-summary-list">{routeOrder.filter(alias => state.config.routes[alias]).map(alias => {
          const route = state.config.routes[alias]
          const first = route.models[0]
          const health = first ? healthMap.get(healthKey(first, route.capability)) : undefined
          return <button key={alias} onClick={() => navigate('routes')}><div className="route-glyph"><Blocks size={17} /></div><div><strong>{routeLabels[alias]?.title || alias}</strong><small><code>{alias}</code> · {route.models.length ? `${route.models.length} 个固定模型` : '完全自动选择'}</small></div><div className="route-current"><span>{first || 'AUTO'}</span>{first && <StatusBadge status={health?.status || 'unknown'} />}</div><ChevronRight size={16} /></button>
        })}</div>
      </article>

      <article className="panel incidents-panel"><div className="panel-heading"><div><span className="eyebrow">RECENT HEALTH</span><h2>最近异常</h2></div><button className="text-button" onClick={() => navigate('models')}>查看模型 <ChevronRight size={15} /></button></div>
        {incidents.length ? <div className="incident-list">{incidents.map(item => <div key={healthKey(item.model, item.capability)}><div className={`incident-icon ${item.status}`}><AlertTriangle size={15} /></div><div><strong>{item.model}</strong><p>{item.capability} · {friendlyError(item.last_status, item.last_error)}</p></div><StatusBadge status={item.status} /></div>)}</div> : <div className="healthy-empty"><ShieldCheck size={28} /><strong>没有待处理异常</strong><p>路由健康状态将在请求发生后持续更新。</p></div>}
      </article>
    </section>

    <section className="panel catalog-strip"><div><div className="catalog-icon"><Database size={20} /></div><div><strong>模型目录已缓存到本机</strong><p>仅在启动或手动刷新时访问 Provider 的模型接口。</p></div></div><div><span>最后更新</span><strong>{formatDate(state.catalog.updated_at)}</strong></div><div><span>已隔离故障</span><strong>{state.summary.failed} 个模型功能</strong></div></section>
  </div>
}

function friendlyError(status?: number, error?: string) {
  if (status === 401 || status === 403) return `认证被拒绝 (${status})，请检查 API Key、账户权限或区域限制。`
  if (status === 429) return '上游免费额度或速率限制已触发，模型已退出自动路由。'
  if (status && status >= 500) return `上游服务暂时不可用 (${status})，路由已尝试后续模型。`
  return error || '模型当前处于降级状态。'
}

function RoutesPage({ config, setConfig, models, healthMap }: { config: RouterConfig; setConfig: (config: RouterConfig) => void; models: EffectiveModel[]; healthMap: Map<string, HealthState> }) {
  const [selected, setSelected] = useState(() => config.routes.chat ? 'chat' : Object.keys(config.routes)[0])
  const [search, setSearch] = useState('')
  const [provider, setProvider] = useState('')
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 5 } }), useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }))
  const route = config.routes[selected]
  const modelMap = new Map(models.map(model => [model.id, model]))
  const eligible = models.filter(model => !model.disabled && model.route_types.includes(route.capability))
  const routable = eligible.filter(model => healthMap.get(healthKey(model.id, route.capability))?.status !== 'failed')
  const providers = [...new Set(routable.map(model => model.provider))].sort()
  const candidates = routable.filter(model => !route.models.includes(model.id) && (!provider || model.provider === provider) && (!search || `${model.id} ${model.name || ''}`.toLowerCase().includes(search.toLowerCase())))

  const updateModels = (items: string[]) => {
    const next = cloneConfig(config); next.routes[selected].models = items; setConfig(next)
  }
  const updateStrategy = (strategy: 'ordered' | 'round-robin') => {
    const next = cloneConfig(config); next.routes[selected].strategy = strategy; setConfig(next)
  }
  const onDragEnd = ({ active, over }: DragEndEvent) => {
    if (!over || active.id === over.id) return
    const oldIndex = route.models.indexOf(String(active.id)), newIndex = route.models.indexOf(String(over.id))
    updateModels(arrayMove(route.models, oldIndex, newIndex))
  }

  return <div className="route-workbench">
    <aside className="route-nav panel"><div className="route-nav-heading"><span>固定能力</span><small>{Object.keys(config.routes).length} 条路由</small></div>{routeOrder.filter(alias => config.routes[alias]).map(alias => { const item = config.routes[alias]; return <button key={alias} className={selected === alias ? 'active' : ''} onClick={() => setSelected(alias)}><div className="route-glyph"><Blocks size={16} /></div><div><strong>{routeLabels[alias]?.title || alias}</strong><small>{item.models.length ? `${item.models.length} 个固定模型` : '自动选择'}</small></div><ChevronRight size={15} /></button> })}</aside>

    <section className="route-chain panel"><div className="route-chain-heading"><div><span className="eyebrow">FALLBACK CHAIN</span><h2>{routeLabels[selected]?.title || selected}</h2><p>{routeLabels[selected]?.description}</p></div><span className="model-alias"><code>model: {selected}</code></span></div>
      <div className="route-strategy"><button className={(route.strategy || 'ordered') === 'ordered' ? 'active' : ''} onClick={() => updateStrategy('ordered')}><ArrowDownUp size={16} /><span><strong>按顺序</strong><small>从首个健康项开始</small></span></button><button className={route.strategy === 'round-robin' ? 'active' : ''} onClick={() => updateStrategy('round-robin')}><RefreshCw size={16} /><span><strong>雨露均沾</strong><small>健康模型轮流首选</small></span></button></div>
      <div className="chain-explainer"><ArrowDownUp size={16} /><span>{route.strategy === 'round-robin' ? '每次轮换首选模型；失败后继续尝试环中的下一个。' : '严格从上到下尝试。拖动可排序，也支持键盘操作。'}</span></div>
      <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onDragEnd}><SortableContext items={route.models} strategy={verticalListSortingStrategy}><div className="fallback-chain">
        {route.models.map((id, index) => <SortableModel key={id} id={id} index={index} model={modelMap.get(id)} health={healthMap.get(healthKey(id, route.capability))} remove={() => updateModels(route.models.filter(item => item !== id))} />)}
        {!route.models.length && <div className="empty-chain"><Sparkles size={24} /><strong>当前使用完全自动路由</strong><p>可以从右侧加入固定优先模型，或继续让系统按能力与健康状态选择。</p></div>}
      </div></SortableContext></DndContext>
      <div className="automatic-fallback"><div className="auto-index">∞</div><div><strong>自动安全兜底</strong><p>固定数组全部失败后，从剩余 {Math.max(0, routable.filter(model => !route.models.includes(model.id)).length)} 个同类型健康模型中轮换选择一个。</p></div><span className="capability-pill accent">始终启用</span></div>
    </section>

    <aside className="candidate-panel panel"><div className="candidate-heading"><div><span className="eyebrow">CANDIDATES</span><h2>添加候选模型</h2></div><span>{candidates.length}</span></div><div className="candidate-filters"><label className="search-field"><Search size={15} /><input value={search} onChange={event => setSearch(event.target.value)} placeholder="搜索模型" /></label><select value={provider} onChange={event => setProvider(event.target.value)}><option value="">全部 Provider</option>{providers.map(item => <option key={item}>{item}</option>)}</select></div>
      <div className="candidate-list">{candidates.slice(0, 80).map(model => <div key={model.id} className="candidate-row"><div><strong title={model.id}>{model.id}</strong><span><StatusBadge status={healthMap.get(healthKey(model.id, route.capability))?.status || 'unknown'} /> · {formatNumber(model.context_length)} context</span></div><IconButton label="加入优先级" onClick={() => updateModels([...route.models, model.id])}><Plus size={16} /></IconButton></div>)}{!candidates.length && <div className="small-empty"><Search size={22} /><span>没有匹配的候选模型</span></div>}</div>
    </aside>
  </div>
}

function SortableModel({ id, index, model, health, remove }: { id: string; index: number; model?: EffectiveModel; health?: HealthState; remove: () => void }) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id })
  const unavailable = !model || model.disabled
  return <div ref={setNodeRef} style={{ transform: CSS.Transform.toString(transform), transition }} className={cx('fallback-item', isDragging && 'dragging', unavailable && 'unavailable')}>
    <div className="priority-index">{index + 1}</div><button className="drag-handle" {...attributes} {...listeners} aria-label={`拖动 ${id} 调整优先级`}><GripVertical size={17} /></button><div className="fallback-model"><strong>{id}</strong><div>{model ? <><span>{model.provider}</span><CapabilityPills model={model} /></> : <span className="danger-text">模型不在当前缓存中</span>}</div></div><div className="fallback-health"><StatusBadge status={health?.status || 'unknown'} />{health?.average_latency_ms ? <small>{Math.round(health.average_latency_ms)}ms</small> : null}</div><IconButton label="从优先级移除" onClick={remove}><Trash2 size={15} /></IconButton>
  </div>
}

function ModelsPage({ config, setConfig, models, healthMap, probe }: { config: RouterConfig; setConfig: (config: RouterConfig) => void; models: EffectiveModel[]; healthMap: Map<string, HealthState>; probe: HealthProbeStatus }) {
  const queryClient = useQueryClient()
  const [search, setSearch] = useState('')
  const [type, setType] = useState('')
  const [provider, setProvider] = useState('')
  const [health, setHealth] = useState('')
  const [selected, setSelected] = useState<EffectiveModel | null>(null)
  const resetMutation = useMutation({ mutationFn: api.resetHealth, onSuccess: () => queryClient.invalidateQueries({ queryKey: ['state'] }) })
  const probeMutation = useMutation({ mutationFn: api.probeHealth, onSuccess: () => queryClient.invalidateQueries({ queryKey: ['state'] }) })
  const modelProbeMutation = useMutation({ mutationFn: ({ model, allowExpensive }: { model: string; allowExpensive: boolean }) => api.probeModelHealth(model, allowExpensive), onSuccess: () => queryClient.invalidateQueries({ queryKey: ['state'] }) })
  useEffect(() => { probeMutation.mutate(false) }, [])
  const providers = [...new Set(models.map(model => model.provider))].sort()
  const filtered = models.filter(model => (!search || `${model.id} ${model.name || ''}`.toLowerCase().includes(search.toLowerCase())) && (!type || model.route_types.includes(type as RouteType)) && (!provider || model.provider === provider) && (!health || (modelHealth(model, healthMap)?.status || 'unknown') === health))
  const updateOverride = (id: string, patch: Partial<ModelOverride>) => {
    const next = cloneConfig(config); next.models[id] = { ...(next.models[id] || {}), ...patch }; setConfig(next)
    setSelected(previous => previous?.id === id ? effectiveModel(models.find(model => model.id === id)!, next) : previous)
  }
  const progress = probe.total ? Math.round((probe.completed / probe.total) * 100) : 100
  return <div className="stack-lg">
    <section className={cx('probe-panel panel', probe.status === 'running' && 'running')}><div className="probe-icon"><Activity size={19} /></div><div className="probe-copy"><div><strong>{probe.status === 'running' ? '正在检测模型可用性' : '模型可用性检测'}</strong><span>{probe.status === 'running' ? `${probe.completed} / ${probe.total}` : '全部可探测模型的结果缓存 24 小时'}</span></div><p>{probe.status === 'running' ? `健康 ${probe.healthy} · 故障 ${probe.failed} · 每个 Provider 串行检测` : probe.finished_at ? `上次完成：${formatDate(probe.finished_at)} · 健康 ${probe.healthy} · 故障 ${probe.failed} · 跳过 ${probe.skipped}` : '进入页面时检测缓存过期的全部模型；Image、Video 使用最小任务，可能消耗少量免费额度。'}</p>{probe.status === 'running' && <div className="probe-progress"><span style={{ width: `${progress}%` }} /></div>}</div><button className="button secondary compact" disabled={probe.status === 'running' || probeMutation.isPending} onClick={() => { if (!window.confirm('将忽略 24 小时缓存并检测全部模型；Image、Video 会创建最小真实任务，可能消耗免费额度。确认继续？')) return; probeMutation.mutate(true) }}><RefreshCw size={16} className={probe.status === 'running' ? 'spin' : ''} />{probe.status === 'running' ? '检测中' : '重新检测全部'}</button></section>
    <section className="model-toolbar panel"><label className="search-field large"><Search size={17} /><input value={search} onChange={event => setSearch(event.target.value)} placeholder="搜索模型 ID、名称或组织" /></label><div className="filter-row"><select value={type} onChange={event => setType(event.target.value)}><option value="">全部类型</option>{Object.keys(routeLabels).map(item => <option key={item}>{item}</option>)}</select><select value={provider} onChange={event => setProvider(event.target.value)}><option value="">全部 Provider</option>{providers.map(item => <option key={item}>{item}</option>)}</select><select value={health} onChange={event => setHealth(event.target.value)}><option value="">全部健康状态</option><option value="failed">故障（已隔离）</option><option value="healthy">健康</option><option value="unknown">未测试</option></select></div><div className="result-count"><strong>{filtered.length}</strong><span>个模型</span></div></section>
    <section className="model-table panel"><div className="model-table-head"><span>模型</span><span>功能路由</span><span>能力</span><span>健康</span><span>Context</span><span /></div><div className="model-table-body">{filtered.map(model => { const health = modelHealth(model, healthMap); const expensive = model.route_types.some(item => item === 'image-generation' || item === 'video-generation'); return <button className={cx('model-table-row', model.disabled && 'disabled')} key={model.id} onClick={() => setSelected(model)}><div className="model-identity"><div className="provider-avatar">{model.provider.slice(0, 2).toUpperCase()}</div><div><strong>{model.id}</strong><small>{model.name || model.upstream_id}</small></div></div><div className="pill-row">{model.route_types.map(item => <span key={item} className="route-pill">{item}</span>)}</div><CapabilityPills model={model} /><div className="model-health"><StatusBadge status={health?.status || 'unknown'} />{!health && expensive && <small>待最小任务检测</small>}</div><span className="context-cell">{model.context_length ? formatNumber(model.context_length) : '—'}</span><ChevronRight size={16} /></button> })}</div></section>
    <ModelDrawer model={selected} health={selected ? modelHealth(selected, healthMap) : undefined} healthStates={selected ? selected.route_types.map(item => healthMap.get(healthKey(selected.id, item))).filter(Boolean) as HealthState[] : []} override={selected ? config.models[selected.id] || {} : {}} close={() => setSelected(null)} update={updateOverride} reset={() => selected && resetMutation.mutate(selected.id)} resetting={resetMutation.isPending} probe={() => { if (!selected) return; const expensive = selected.route_types.some(item => item === 'image-generation' || item === 'video-generation'); if (expensive && !window.confirm('该检测会创建真实的图像或视频任务，可能消耗免费额度。确认继续？')) return; modelProbeMutation.mutate({ model: selected.id, allowExpensive: expensive }) }} probing={probe.status === 'running' || modelProbeMutation.isPending} />
  </div>
}

function ModelDrawer({ model, health, healthStates, override, close, update, reset, resetting, probe, probing }: { model: EffectiveModel | null; health?: HealthState; healthStates: HealthState[]; override: ModelOverride; close: () => void; update: (id: string, patch: Partial<ModelOverride>) => void; reset: () => void; resetting: boolean; probe: () => void; probing: boolean }) {
  return <Dialog.Root open={Boolean(model)} onOpenChange={open => !open && close()}><Dialog.Portal><Dialog.Overlay className="dialog-overlay" /><Dialog.Content className="model-drawer">{model && <>
    <div className="drawer-header"><div><span className="eyebrow">MODEL DETAILS</span><Dialog.Title>{model.id}</Dialog.Title><Dialog.Description>{model.description || model.name || model.upstream_id}</Dialog.Description></div><Dialog.Close asChild><button className="icon-button"><X size={19} /></button></Dialog.Close></div>
    <div className="drawer-status"><StatusBadge status={health?.status || 'unknown'} /><span>{model.provider}</span><span>{model.tier || 'free tier'}</span></div>
    {health?.last_error && <div className="drawer-alert"><AlertTriangle size={17} /><div><strong>最近一次错误</strong><p>{friendlyError(health.last_status, health.last_error)}</p></div></div>}
    <div className="detail-section"><h3>能力与上下文</h3><div className="detail-grid"><div><span>功能数量</span><strong>{model.route_types.length}</strong></div><div><span>Context</span><strong>{model.context_length ? formatNumber(model.context_length) : '未知'}</strong></div><div><span>最大输出</span><strong>{model.max_output_tokens ? formatNumber(model.max_output_tokens) : '未知'}</strong></div><div><span>平均延迟</span><strong>{health?.average_latency_ms ? `${Math.round(health.average_latency_ms)}ms` : '尚无数据'}</strong></div></div><div className="function-health-list">{model.route_types.map(item => { const state = healthStates.find(healthState => healthState.capability === item); return <div key={item}><div><strong>{routeLabels[item].title}</strong><small>{item}</small></div><StatusBadge status={state?.status || 'unknown'} /></div> })}</div><CapabilityPills model={model} /></div>
    <div className="detail-section"><h3>人工覆盖</h3><p className="section-help">上游元数据不准确时，可让一个模型参与多个固定功能路由；如需全部移除请直接禁用模型。</p><div className="function-override-head"><span>功能集合</span><button className="text-button" onClick={() => update(model.id, { functions: undefined })}>恢复自动识别</button></div><div className="function-override-grid">{routeOrder.map(item => { const selected = (override.functions || model.route_types).includes(item); return <button key={item} className={selected ? 'active' : ''} onClick={() => { const current = [...(override.functions || model.route_types)]; update(model.id, { functions: selected && current.length > 1 ? current.filter(value => value !== item) : selected ? current : [...current, item] }) }}><Check size={13} />{routeLabels[item].title}</button> })}</div><div className="toggle-list"><OverrideToggle label="Tool call" value={override.tool_call} onChange={value => update(model.id, { tool_call: value })} /><OverrideToggle label="Vision" value={override.vision} onChange={value => update(model.id, { vision: value })} /><OverrideToggle label="Reasoning" value={override.reasoning} onChange={value => update(model.id, { reasoning: value })} /></div></div>
    <div className="drawer-footer"><button className="button secondary full" onClick={probe} disabled={probing || model.disabled}><Activity size={16} />{probing ? '检测任务运行中…' : model.route_types.some(item => item === 'image-generation' || item === 'video-generation') ? '检测全部功能（可能消耗额度）' : '检测此模型的全部功能'}</button>{health?.status === 'failed' && <button className="button secondary full" onClick={reset} disabled={resetting}><RefreshCw size={16} />{resetting ? '恢复中…' : '重置全部功能健康状态'}</button>}<button className={cx('button full', model.disabled ? '' : 'danger-button')} onClick={() => update(model.id, { disabled: !model.disabled })}>{model.disabled ? <><Eye size={16} />重新启用模型</> : <><EyeOff size={16} />禁用这个模型</>}</button></div>
  </>}</Dialog.Content></Dialog.Portal></Dialog.Root>
}

function OverrideToggle({ label, value, onChange }: { label: string; value?: boolean; onChange: (value: boolean | undefined) => void }) {
  return <div><span>{label}</span><div className="segmented"><button className={value === undefined ? 'active' : ''} onClick={() => onChange(undefined)}>自动</button><button className={value === true ? 'active' : ''} onClick={() => onChange(true)}>支持</button><button className={value === false ? 'active' : ''} onClick={() => onChange(false)}>不支持</button></div></div>
}

function ProvidersPage({ state, onToast }: { state: AppState; onToast: (toast: { message: string; error?: boolean }) => void }) {
  const queryClient = useQueryClient()
  const configured = state.providers.filter(provider => provider.configured).length
  const available = state.providers.filter(provider => provider.configured && state.models.some(model => model.provider === provider.id)).length
  const providers = [...state.providers].sort((a, b) => Number(b.configured) - Number(a.configured))
  return <div className="stack-lg"><section className="provider-intro panel"><div className="provider-intro-icon"><KeyRound size={22} /></div><div><h2>连接状态与凭据配置分开显示</h2><p>运行状态由缓存模型和连接测试决定；凭据状态只表示 Key 已存在，不代表上游一定可用。</p></div><div><strong>{available}</strong><span>可用 · {configured} 已配置</span></div></section><section className="provider-grid">{providers.map(provider => <ProviderCard key={provider.id} provider={provider} modelCount={state.models.filter(model => model.provider === provider.id).length} saved={state.credentials.some(item => item.provider === provider.id)} refresh={() => queryClient.invalidateQueries({ queryKey: ['state'] })} toast={onToast} />)}</section></div>
}

function ProviderCard({ provider, modelCount, saved, refresh, toast }: { provider: ProviderStatus; modelCount: number; saved: boolean; refresh: () => void; toast: (toast: { message: string; error?: boolean }) => void }) {
  const [key, setKey] = useState('')
  const [visible, setVisible] = useState(false)
  const [busy, setBusy] = useState('')
  const [probe, setProbe] = useState<{ status: 'healthy' | 'error'; models?: number; latency?: number; message?: string } | null>(null)
  const run = async (action: string, task: () => Promise<unknown>, success: string) => { setBusy(action); try { await task(); setProbe(null); toast({ message: success }); refresh(); setKey('') } catch (error) { toast({ message: (error as Error).message, error: true }) } finally { setBusy('') } }
  const login = async () => { setBusy('oauth'); try { const result = await api.startOpenRouterOAuth(); window.location.assign(result.authorization_url) } catch (error) { toast({ message: (error as Error).message, error: true }); setBusy('') } }
  const testConnection = async () => {
    setBusy('test')
    try {
      const result = await api.testProvider(provider.id)
      setProbe({ status: 'healthy', models: result.models, latency: result.latency_ms })
      toast({ message: `${provider.id} 连接正常，发现 ${result.models} 个模型` })
      refresh()
    } catch (error) {
      const message = (error as Error).message
      setProbe({ status: 'error', message })
      toast({ message, error: true })
    } finally { setBusy('') }
  }
  const missingRequired = provider.missing_required || []
  const source = provider.matched_env || (provider.source === 'saved' ? '安全存储' : '—')
  const placeholder = provider.envs?.length ? provider.envs.join(' / ') : 'API Key'
  const state = !provider.configured ? 'inactive' : probe?.status === 'error' ? 'error' : probe?.status === 'healthy' ? (Number(probe.models) > 0 ? 'available' : 'pending') : modelCount > 0 ? 'available' : 'pending'
  const stateTitle = state === 'available' ? '服务可用' : state === 'pending' ? '等待模型同步' : state === 'error' ? '连接异常' : '尚未接入'
  const stateDetail = probe?.status === 'healthy' ? `${probe.models} 个模型 · ${probe.latency}ms` : probe?.status === 'error' ? '最近一次连接测试失败' : state === 'available' ? '模型目录已缓存，可参与路由' : state === 'pending' ? '凭据已配置，但尚未发现模型' : '配置凭据后才会启动这个 Provider'
  return <article className={cx('provider-card', state)}>
    <div className="provider-card-head"><div className="provider-logo">{provider.id.slice(0, 2).toUpperCase()}</div><div><h3>{provider.id}</h3><p>{provider.tier}</p></div><StatusBadge status={provider.configured ? 'configured' : 'missing'} /></div>
    <div className={cx('provider-runtime', state)}><div className="provider-runtime-icon">{state === 'available' ? <Check size={17} /> : state === 'error' ? <AlertTriangle size={17} /> : state === 'pending' ? <RefreshCw size={17} /> : <Unplug size={17} />}</div><div><span>运行状态</span><strong>{stateTitle}</strong><small title={probe?.message}>{stateDetail}</small></div><div className="provider-model-count"><strong>{modelCount}</strong><span>缓存模型</span></div></div>
    <div className="provider-discovery"><div><span>免费模型策略</span><strong>{provider.discovery_policy || '未声明'}</strong></div><small title={provider.free_basis}>{provider.manifest_generated_at ? `清单 ${formatDate(provider.manifest_generated_at)}` : '未加载模型清单'}</small></div>
    <div className="provider-credential-head"><div><span>凭据配置</span><strong>{provider.configured ? '已设置' : '未设置'}</strong></div><small title={source}>来源：{source}</small></div>
    {provider.manifest_error && <div className="provider-warning"><AlertTriangle size={14} />模型清单错误：{provider.manifest_error}</div>}
    {missingRequired.length > 0 && <div className="provider-warning"><AlertTriangle size={14} />还需要 {missingRequired.join(', ')}</div>}
    {provider.billing_warning && <div className="provider-warning"><AlertTriangle size={14} />{provider.billing_warning}</div>}
    <div className="credential-input"><input type={visible ? 'text' : 'password'} value={key} onChange={event => setKey(event.target.value)} placeholder={provider.configured ? '输入新 Key 可替换当前凭据' : `粘贴 ${placeholder}`} autoComplete="new-password" /><button onClick={() => setVisible(value => !value)} aria-label={visible ? '隐藏密钥' : '显示密钥'}>{visible ? <EyeOff size={16} /> : <Eye size={16} />}</button></div>
    <div className="provider-actions">{provider.oauth && <button className="button small" disabled={Boolean(busy)} onClick={login}><LogIn size={14} />{busy === 'oauth' ? '跳转中…' : provider.configured ? '重新登录' : 'OAuth 登录'}</button>}<button className={cx('button small', provider.oauth && 'secondary')} disabled={!key || Boolean(busy)} onClick={() => run('save', () => api.saveCredential(provider.id, key), `${provider.id} 凭据已保存并热加载`)}>{busy === 'save' ? '保存中…' : provider.configured ? '更新 Key' : '保存凭据'}</button>{provider.configured && <button className="button secondary small" disabled={Boolean(busy)} onClick={testConnection}>{busy === 'test' ? '测试中…' : '测试连接'}</button>}{provider.register_url && <a className="button secondary small provider-register" href={provider.register_url} target="_blank" rel="noreferrer">获取 Key<ExternalLink size={14} /></a>}{saved && <IconButton label="删除保存的凭据" disabled={Boolean(busy)} onClick={() => run('delete', () => api.deleteCredential(provider.id), `${provider.id} 凭据已删除`)}><Trash2 size={15} /></IconButton>}</div>
  </article>
}

function SystemPage({ state, runtime, offline }: { state: AppState; runtime: RuntimeStatus; offline: boolean }) {
  const rows = [
    ['服务状态', offline ? '连接已断开' : '运行中'], ['启动方式', runtime.service_manager], ['版本', runtime.version], ['PID', String(runtime.pid)], ['运行时间', formatUptime(runtime.uptime_seconds)], ['监听地址', 'http://localhost:1314'], ['路由配置', state.config_path], ['模型缓存', state.catalog.cache_path], ['缓存更新时间', formatDate(state.catalog.updated_at)],
  ]
  return <div className="system-layout"><section className="panel system-card"><div className="system-hero"><div className={cx('system-service-icon', offline && 'offline')}><Server size={28} /></div><div><span className="eyebrow">FREE ROUTER DAEMON</span><h2>{offline ? '服务连接已断开' : '本地服务运行正常'}</h2><p>用户级守护进程，无需 root 权限。</p></div><StatusBadge status={offline ? 'failed' : 'healthy'} /></div><div className="system-rows">{rows.map(([label, value]) => <div key={label}><span>{label}</span><code>{value}</code></div>)}</div></section><aside className="stack-md"><section className="panel command-card"><div className="command-card-icon"><Activity size={19} /></div><h3>守护进程管理</h3><p>页面随服务一起运行，停止后需从终端重新启动。</p>{['free-router daemon status', 'free-router daemon restart', 'free-router daemon logs --follow'].map(command => <code key={command}>{command}</code>)}</section><section className="panel command-card"><div className="command-card-icon blue"><CircleHelp size={19} /></div><h3>API 接入</h3><p>所有 OpenAI 兼容客户端只需设置一个本地地址。</p><code>OPENAI_BASE_URL=http://localhost:1314/v1</code></section></aside></div>
}

export default App
