export type RouteType = 'chat' | 'chat-tools' | 'embedding' | 'audio' | 'image' | 'video' | 'rerank' | 'moderation'

export interface Capabilities {
  tool_call: boolean
  tool_call_known: boolean
  reasoning: boolean
  reasoning_known: boolean
  vision: boolean
  vision_known: boolean
  streaming: boolean
}

export interface Model {
  id: string
  provider: string
  upstream_id: string
  name?: string
  description?: string
  owned_by?: string
  type: string
  free: boolean
  tier?: string
  context_length?: number
  max_output_tokens?: number
  input_modalities?: string[]
  output_modalities?: string[]
  supported_parameters?: string[]
  supported_endpoints?: string[]
  capabilities: Capabilities
}

export interface ModelOverride {
  disabled?: boolean
  type?: string
  tool_call?: boolean
  vision?: boolean
  reasoning?: boolean
}

export interface Route {
  _comment?: string
  type: RouteType
  strategy?: 'ordered' | 'round-robin'
  require_tool?: boolean
  models: string[]
}

export interface RouterConfig {
  _comment?: string
  _help?: Record<string, string>
  version: number
  routes: Record<string, Route>
  models: Record<string, ModelOverride>
  provider_env?: Record<string, string[]>
}

export interface ProviderStatus {
  id: string
  envs: string[]
  matched_env?: string
  requires?: string[]
  missing_required?: string[]
  configured: boolean
  source: 'environment' | 'saved' | 'missing'
  tier: string
  free_kind?: 'credit' | 'trial' | string
  billing_warning?: string
  register_url?: string
  oauth?: boolean
}

export interface Credential { provider: string; backend?: string }

export interface HealthState {
  model: string
  status: 'healthy' | 'failed' | 'unknown'
  requests: number
  successes: number
  failures: number
  consecutive_failures: number
  average_latency_ms: number
  last_status?: number
  last_error?: string
  last_used_at?: string
  checks: number
  last_checked_at?: string
  last_check_latency_ms?: number
}

export interface HealthProbeStatus {
  status: 'idle' | 'running' | 'completed'
  total: number
  completed: number
  healthy: number
  failed: number
  skipped: number
  started_at?: string
  finished_at?: string
}

export interface Summary { requests: number; successes: number; failures: number; failed: number }
export interface CatalogStatus { count: number; updated_at?: string; cache_path: string }
export interface RuntimeStatus {
  status: string
  pid: number
  version: string
  started_at: string
  uptime_seconds: number
  service_manager: 'launchd' | 'systemd' | 'manual'
  models: number
  requests: number
  failed: number
}

export interface AppState {
  config: RouterConfig
  config_path: string
  models: Model[]
  catalog: CatalogStatus
  providers: ProviderStatus[]
  credentials: Credential[]
  health: HealthState[]
  summary: Summary
  health_probe: HealthProbeStatus
  runtime: RuntimeStatus
}

export interface EffectiveModel extends Model {
  disabled: boolean
  route_types: RouteType[]
}
