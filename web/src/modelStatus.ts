import type { EffectiveModel, HealthState } from './types'

export type ModelDisplayStatus = HealthState['status'] | 'configured' | 'missing' | 'manual'

export const healthKey = (model: string, capability: string) => `${model}\u0000${capability}`

export function modelHealth(model: EffectiveModel, healthMap: Map<string, HealthState>): HealthState | undefined {
  const states = model.route_types.map(capability => healthMap.get(healthKey(model.id, capability))).filter(Boolean) as HealthState[]
  const priority: HealthState['status'][] = ['open', 'cooling', 'degraded', 'half-open', 'unknown']
  for (const status of priority) {
    const matched = states.find(item => item.status === status)
    if (matched) return matched
  }
  return states.length === model.route_types.length && states.every(item => item.status === 'healthy') ? states[0] : undefined
}

export function modelDisplayStatus(model: EffectiveModel, healthMap: Map<string, HealthState>, configuredProviders: Set<string>): ModelDisplayStatus {
  const health = modelHealth(model, healthMap)
  if (health) return health.status
  if (!configuredProviders.has(model.provider)) return 'missing'
  if (model.route_types.some(item => item === 'image-generation' || item === 'video-generation')) return 'manual'
  return 'unknown'
}
