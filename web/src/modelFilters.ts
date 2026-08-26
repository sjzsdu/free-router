import { modelDisplayStatus } from './modelStatus'
import type { ModelFilters } from './modelProbeScope'
import type { EffectiveModel, HealthState, ProviderStatus, RouteType } from './types'

export function filterModels(models: EffectiveModel[], filters: ModelFilters, healthMap: Map<string, HealthState>, providerStatuses: ProviderStatus[]) {
  const search = filters.search.trim().toLowerCase()
  const configuredProviders = new Set(providerStatuses.filter(item => item.configured).map(item => item.id))
  return models.filter(model => (
    (!search || `${model.id} ${model.name || ''}`.toLowerCase().includes(search))
    && (!filters.type || model.route_types.includes(filters.type as RouteType))
    && (!filters.provider || model.provider === filters.provider)
    && (!filters.health || modelDisplayStatus(model, healthMap, configuredProviders) === filters.health)
  ))
}
