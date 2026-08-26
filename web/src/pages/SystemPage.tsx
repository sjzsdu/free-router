import { Activity, CircleHelp, Server } from 'lucide-react'
import type { AppState, RuntimeStatus } from '../types'

const cx = (...classes: Array<string | false | null | undefined>) => classes.filter(Boolean).join(' ')
const formatDate = (value?: string) => value ? new Date(value).toLocaleString() : '尚未更新'
function formatUptime(seconds = 0) {
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days) return `${days} 天 ${hours} 小时`
  if (hours) return `${hours} 小时 ${minutes} 分钟`
  if (minutes) return `${minutes} 分钟`
  return `${Math.max(0, Math.floor(seconds))} 秒`
}

export function SystemPage({ state, runtime, offline }: { state: AppState; runtime: RuntimeStatus; offline: boolean }) {
  const rows = [
    ['服务状态', offline ? '连接已断开' : '运行中'], ['启动方式', runtime.service_manager], ['版本', runtime.version], ['PID', String(runtime.pid)], ['运行时间', formatUptime(runtime.uptime_seconds)], ['监听地址', 'http://localhost:1314'], ['路由配置', state.config_path], ['模型缓存', state.catalog.cache_path], ['缓存更新时间', formatDate(state.catalog.updated_at)],
  ]
  return <div className="system-layout"><section className="panel system-card"><div className="system-hero"><div className={cx('system-service-icon', offline && 'offline')}><Server size={28} /></div><div><span className="eyebrow">FREE ROUTER DAEMON</span><h2>{offline ? '服务连接已断开' : '本地服务运行正常'}</h2><p>用户级守护进程，无需 root 权限。</p></div><span className={`status-badge ${offline ? 'failed' : 'healthy'}`}><span />{offline ? '故障' : '健康'}</span></div><div className="system-rows">{rows.map(([label, value]) => <div key={label}><span>{label}</span><code>{value}</code></div>)}</div></section><aside className="stack-md"><section className="panel command-card"><div className="command-card-icon"><Activity size={19} /></div><h3>守护进程管理</h3><p>页面随服务一起运行，停止后需从终端重新启动。</p>{['free-router daemon status', 'free-router daemon restart', 'free-router daemon logs --follow'].map(command => <code key={command}>{command}</code>)}</section><section className="panel command-card"><div className="command-card-icon blue"><CircleHelp size={19} /></div><h3>API 接入</h3><p>所有 OpenAI 兼容客户端只需设置一个本地地址。</p><code>OPENAI_BASE_URL=http://localhost:1314/v1</code></section></aside></div>
}
