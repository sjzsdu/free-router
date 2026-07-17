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
  type: RouteType
  require_tool?: boolean
  models: string[]
}

export interface RouterConfig {
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
}

export interface Credential { provider: string; backend?: string }

export interface HealthState {
  model: string
  status: 'healthy' | 'cooling' | 'degraded' | 'unknown'
  requests: number
  successes: number
  failures: number
  consecutive_failures: number
  average_latency_ms: number
  last_status?: number
  last_error?: string
  last_used_at?: string
  cooldown_until?: string
}

export interface Summary { requests: number; successes: number; failures: number; cooling: number }
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
  cooling: number
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
  runtime: RuntimeStatus
}

export interface EffectiveModel extends Model {
  disabled: boolean
  route_types: RouteType[]
}
