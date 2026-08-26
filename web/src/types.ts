export type RouteType = 'chat' | 'chat-tools' | 'image-understanding' | 'image-generation' | 'video-understanding' | 'video-generation' | 'audio-understanding' | 'speech-to-text' | 'text-to-speech' | 'embedding' | 'rerank' | 'moderation'

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
  functions: RouteType[]
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
  functions?: RouteType[]
  tool_call?: boolean
  vision?: boolean
  reasoning?: boolean
}

export interface Route {
  _comment?: string
  capability: RouteType
  strategy?: 'ordered' | 'round-robin'
  require_tool?: boolean
  models: string[]
}

export interface RouterConfig {
  _comment?: string
  _help?: Record<string, string>
  version: number
  revision: number
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
  free_basis?: string
  source_urls?: string[]
  catalog_status?: 'ready' | 'empty' | string
  discovery_status?: 'ready' | 'confirmed-empty' | 'discovery-failed' | 'validation-failed' | 'verification-failed' | 'awaiting-approval' | 'awaiting-discovery' | 'manifest-error' | string
  discovery_message?: string
  formula_model_count?: number
  manifest_generated_at?: string
  manifest_error?: string
  register_url?: string
  register_label?: string
  oauth?: boolean
  connection_status?: 'healthy' | 'error'
  connection_formula_models?: number
  connection_latency_ms?: number
  connection_error?: string
  connection_checked_at?: string
}

export interface FreeModelPricing { prompt?: string; completion?: string }

export interface ProviderFreeModel {
  id: string
  name?: string
  description?: string
  type?: string
  functions?: RouteType[]
  context_length?: number
  max_output_tokens?: number
  free_basis?: string
  source_urls?: string[]
  verified_at?: string
  pricing?: FreeModelPricing
}

export interface ProviderDetails {
  id: string
  tier?: string
  free_kind?: string
  billing_warning?: string
  free_basis?: string
  source_urls?: string[]
  discovery_status?: string
  discovery_message?: string
  manifest_generated_at?: string
  models: ProviderFreeModel[]
}

export interface Credential { provider: string; backend?: string }

export interface HealthState {
  model: string
  capability: RouteType | 'provider'
  status: 'unknown' | 'healthy' | 'degraded' | 'open' | 'half-open' | 'cooling'
  requests: number
  successes: number
  failures: number
  consecutive_failures: number
  average_latency_ms: number
  last_status?: number
  last_error?: string
  last_used_at?: string
  checks: number
  verified: boolean
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
  dry_run?: boolean
  expensive_budget_remaining: number
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
  provider_health?: HealthState[]
  summary: Summary
  health_probe: HealthProbeStatus
  runtime: RuntimeStatus
  eligibility: Array<{ model: string; capability: RouteType; eligible: boolean; reason?: string }>
}

export interface ModelStatistics {
  model: string
  provider: string
  capability: string
  requests: number
  successes: number
  failures: number
  success_rate: number
  input_tokens: number
  output_tokens: number
  total_tokens: number
  usage_reported: number
  usage_missing: number
  average_latency_ms: number
  last_status?: number
  last_used_at: string
}

export interface StatisticsSnapshot {
  updated_at?: string
  models: ModelStatistics[]
}

export interface EffectiveModel extends Model {
  disabled: boolean
  route_types: RouteType[]
}
