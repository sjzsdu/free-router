import type { AppState, HealthProbeStatus, ProviderDetails, RouterConfig, RuntimeStatus } from './types'

const base = '/admin/api/'

async function request<T>(path: string, init: RequestInit = {}, timeoutMs = 0): Promise<T> {
  const controller = timeoutMs > 0 && !init.signal ? new AbortController() : null
  const timeout = controller ? window.setTimeout(() => controller.abort(), timeoutMs) : null
  try {
    const response = await fetch(base + path, {
      ...init,
      signal: controller?.signal || init.signal,
      headers: init.body ? { 'Content-Type': 'application/json', ...init.headers } : init.headers,
    })
    const payload = await response.json().catch(() => ({}))
    if (!response.ok) throw new Error(payload.error || `请求失败 (${response.status})`)
    return payload as T
  } catch (error) {
    if (controller?.signal.aborted) throw new Error(`保存请求超时（${Math.round(timeoutMs / 1000)} 秒），请检查服务日志或 macOS 钥匙串状态`)
    throw error
  } finally {
    if (timeout !== null) window.clearTimeout(timeout)
  }
}

export const api = {
  state: () => request<AppState>('state'),
  runtime: () => request<RuntimeStatus>('runtime'),
  saveConfig: (config: RouterConfig) => request<{ saved: boolean; config: RouterConfig }>('config', { method: 'PUT', body: JSON.stringify(config) }),
  refresh: () => request<{ refreshed: boolean; models: number }>('refresh', { method: 'POST' }),
  resetHealth: (model: string) => request<{ reset: boolean; model: string }>('health/reset', { method: 'POST', body: JSON.stringify({ model }) }),
  probeHealth: (force = false, models?: string[]) => request<HealthProbeStatus>('health/probe', { method: 'POST', body: JSON.stringify({ force, ...(models === undefined ? {} : { models }) }) }),
  probeModelHealth: (model: string, allowExpensive = false, dryRun = false) => request<HealthProbeStatus>('health/probe/model', { method: 'POST', body: JSON.stringify({ model, allow_expensive: allowExpensive, dry_run: dryRun }) }),
  testProvider: (provider: string) => request<{ ok: boolean; provider: string; formula_models: number; latency_ms: number }>(`providers/${encodeURIComponent(provider)}/test`, { method: 'POST' }),
  providerDetails: (provider: string) => request<ProviderDetails>(`providers/${encodeURIComponent(provider)}`),
  startOpenRouterOAuth: () => request<{ provider: string; authorization_url: string }>('oauth/openrouter/start', { method: 'POST' }),
  saveCredential: (provider: string, apiKey: string) => request<{ saved: boolean; backend: string; validation: { ok: boolean; provider: string; formula_models?: number; latency_ms: number; error?: string } }>('credentials', { method: 'POST', body: JSON.stringify({ provider, api_key: apiKey }) }, 20_000),
  deleteCredential: (provider: string) => request<{ removed: boolean }>(`credentials/${encodeURIComponent(provider)}`, { method: 'DELETE' }),
}
