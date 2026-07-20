import type { AppState, HealthProbeStatus, RouterConfig, RuntimeStatus } from './types'

const base = '/admin/api/'

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await fetch(base + path, {
    ...init,
    headers: init.body ? { 'Content-Type': 'application/json', ...init.headers } : init.headers,
  })
  const payload = await response.json().catch(() => ({}))
  if (!response.ok) throw new Error(payload.error || `请求失败 (${response.status})`)
  return payload as T
}

export const api = {
  state: () => request<AppState>('state'),
  runtime: () => request<RuntimeStatus>('runtime'),
  saveConfig: (config: RouterConfig) => request<{ saved: boolean; config: RouterConfig }>('config', { method: 'PUT', body: JSON.stringify(config) }),
  refresh: () => request<{ refreshed: boolean; models: number }>('refresh', { method: 'POST' }),
  resetHealth: (model: string) => request<{ reset: boolean; model: string }>('health/reset', { method: 'POST', body: JSON.stringify({ model }) }),
  probeHealth: (force = false) => request<HealthProbeStatus>('health/probe', { method: 'POST', body: JSON.stringify({ force }) }),
  probeModelHealth: (model: string, allowExpensive = false) => request<HealthProbeStatus>('health/probe/model', { method: 'POST', body: JSON.stringify({ model, allow_expensive: allowExpensive }) }),
  testProvider: (provider: string) => request<{ ok: boolean; provider: string; models: number; latency_ms: number }>(`providers/${encodeURIComponent(provider)}/test`, { method: 'POST' }),
  startOpenRouterOAuth: () => request<{ provider: string; authorization_url: string }>('oauth/openrouter/start', { method: 'POST' }),
  saveCredential: (provider: string, apiKey: string) => request<{ saved: boolean; backend: string }>('credentials', { method: 'POST', body: JSON.stringify({ provider, api_key: apiKey }) }),
  deleteCredential: (provider: string) => request<{ removed: boolean }>(`credentials/${encodeURIComponent(provider)}`, { method: 'DELETE' }),
}
