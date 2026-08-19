import type { EffectiveModel, HealthState } from './types'

export type ModelDisplayStatus = HealthState['status'] | 'configured' | 'missing' | 'manual'

export const healthKey = (model: string, capability: string) => `${model}\u0000${capability}`
export const modelIDKey = (id: string) => id.trim().toLowerCase()

export function uniqueModelIDs(items: string[]): string[] {
  const seen = new Set<string>()
  return items.filter(item => {
    const key = modelIDKey(item)
    if (seen.has(key)) return false
    seen.add(key)
    return true
  })
}

export function isModelAlreadySelected(id: string, selected: string[]): boolean {
  const selectedKeys = new Set(selected.map(modelIDKey))
  return selectedKeys.has(modelIDKey(id))
}

export function modelHealth(model: EffectiveModel, healthMap: Map<string, HealthState>): HealthState | undefined {
  return mostSevereHealth([...healthMap.values()].filter(item => item.model === model.id))
}

export function modelDisplayStatus(model: EffectiveModel, healthMap: Map<string, HealthState>, configuredProviders: Set<string>): ModelDisplayStatus {
  const health = modelHealth(model, healthMap)
  if (health) return health.status
  if (!configuredProviders.has(model.provider)) return 'missing'
  if (model.route_types.some(item => item === 'image-generation' || item === 'video-generation')) return 'manual'
  return 'unknown'
}

function mostSevereHealth(states: HealthState[]): HealthState | undefined {
  const priority: HealthState['status'][] = ['open', 'cooling', 'degraded', 'half-open', 'unknown']
  for (const status of priority) {
    const matched = states.find(item => item.status === status)
    if (matched) return matched
  }
  return states.length && states.every(item => item.status === 'healthy') ? states[0] : undefined
}

export function modelRuntimeHealth(model: EffectiveModel, healthMap: Map<string, HealthState>): HealthState | undefined {
  return mostSevereHealth([...healthMap.values()].filter(item => item.model === model.id))
}

export function routeModelHealth(model: EffectiveModel, capability: string, healthMap: Map<string, HealthState>, providerHealthMap: Map<string, HealthState>): HealthState | undefined {
  const runtimeHealth = modelRuntimeHealth(model, healthMap)
  if (runtimeHealth && runtimeHealth.status !== 'healthy') return runtimeHealth
  const aggregateHealth = modelHealth(model, healthMap)
  const capabilityHealth = healthMap.get(healthKey(model.id, capability))
  const providerHealth = providerHealthMap.get(model.provider)
  return providerHealth && providerHealth.status !== 'healthy' ? providerHealth : (runtimeHealth || aggregateHealth || capabilityHealth)
}

export function isRouteModelHealthy(model: EffectiveModel, capability: string, healthMap: Map<string, HealthState>, providerHealthMap: Map<string, HealthState>): boolean {
  const runtimeHealth = modelRuntimeHealth(model, healthMap)
  const capabilityHealth = healthMap.get(healthKey(model.id, capability))
  const providerHealth = providerHealthMap.get(model.provider)
  return runtimeHealth?.status === 'healthy' && capabilityHealth?.verified === true && capabilityHealth.status === 'healthy' && (!providerHealth || providerHealth.status === 'healthy')
}
