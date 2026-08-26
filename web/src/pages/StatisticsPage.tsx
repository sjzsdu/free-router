import { Activity, BarChart3, Clock3, Coins } from 'lucide-react'
import type { StatisticsSnapshot } from '../types'

const number = (value = 0) => value.toLocaleString()
const percent = (value = 0) => `${(value * 100).toFixed(value > 0 && value < 0.1 ? 1 : 0)}%`

export function StatisticsPage({ snapshot, loading, error }: { snapshot?: StatisticsSnapshot; loading: boolean; error: Error | null }) {
  if (loading && !snapshot) return <section className="panel statistics-empty"><Activity className="spin" /><strong>正在读取统计数据…</strong></section>
  if (error && !snapshot) return <section className="panel statistics-empty"><BarChart3 /><strong>统计数据暂不可用</strong><p>{error.message}</p></section>

  const rows = snapshot?.models || []
  const totals = rows.reduce((sum, row) => ({
    requests: sum.requests + row.requests,
    successes: sum.successes + row.successes,
    input: sum.input + row.input_tokens,
    output: sum.output + row.output_tokens,
    missing: sum.missing + row.usage_missing,
  }), { requests: 0, successes: 0, input: 0, output: 0, missing: 0 })

  return <div className="stack-xl">
    <section className="metrics-grid">
      <article className="metric-card"><div className="metric-icon green"><Activity size={19} /></div><div className="metric-label">模型调用</div><strong>{number(totals.requests)}</strong><p>包含失败后触发 fallback 的上游尝试</p></article>
      <article className="metric-card"><div className="metric-icon blue"><Coins size={19} /></div><div className="metric-label">输入 Token</div><strong>{number(totals.input)}</strong><p>仅累计上游明确返回的 usage</p></article>
      <article className="metric-card"><div className="metric-icon violet"><Coins size={19} /></div><div className="metric-label">输出 Token</div><strong>{number(totals.output)}</strong><p>{totals.missing ? `${number(totals.missing)} 次成功调用未返回 usage` : '已返回 usage 的成功调用均已统计'}</p></article>
      <article className="metric-card"><div className="metric-icon amber"><BarChart3 size={19} /></div><div className="metric-label">整体成功率</div><strong>{totals.requests ? percent(totals.successes / totals.requests) : '—'}</strong><p>按实际模型尝试计算</p></article>
    </section>

    <section className="panel statistics-panel">
      <div className="panel-heading"><div><span className="eyebrow">MODEL AGGREGATES</span><h2>按模型聚合</h2></div><span className="statistics-updated"><Clock3 size={13} />{snapshot?.updated_at ? new Date(snapshot.updated_at).toLocaleString() : '尚无调用'}</span></div>
      {rows.length ? <div className="statistics-table-wrap"><table className="statistics-table"><thead><tr><th>模型</th><th>能力</th><th>调用</th><th>成功率</th><th>输入 Token</th><th>输出 Token</th><th>Usage 覆盖</th><th>平均延迟</th></tr></thead><tbody>{rows.map(row => <tr key={`${row.model}:${row.capability}`}><td><strong>{row.model}</strong><small>{row.provider}</small></td><td><code>{row.capability}</code></td><td>{number(row.requests)}<small>{row.failures ? `${number(row.failures)} 失败` : '全部成功'}</small></td><td><span className={row.success_rate >= .95 ? 'rate-good' : row.success_rate >= .7 ? 'rate-warn' : 'rate-bad'}>{percent(row.success_rate)}</span></td><td>{number(row.input_tokens)}</td><td>{number(row.output_tokens)}</td><td>{number(row.usage_reported)} / {number(row.successes)}{row.usage_missing > 0 && <small>{number(row.usage_missing)} 缺失</small>}</td><td>{Math.round(row.average_latency_ms).toLocaleString()} ms</td></tr>)}</tbody></table></div> : <div className="statistics-empty"><BarChart3 size={28} /><strong>尚无模型调用统计</strong><p>完成首个 OpenAI 兼容推理请求后，这里会显示实际模型的调用质量与 Token 用量。</p></div>}
    </section>
  </div>
}
