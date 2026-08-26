import type { EffectiveModel } from './types'

export interface ModelFilters {
  search: string
  type: string
  provider: string
  health: string
}

export function hasActiveModelFilters(filters: ModelFilters) {
  return Boolean(filters.search.trim() || filters.type || filters.provider || filters.health)
}

export function bulkProbeLabel(filtersActive: boolean) {
  return filtersActive ? '手动检测筛选' : '手动检测全部'
}

export function bulkProbeModelIDs(filtered: EffectiveModel[], filtersActive: boolean) {
  return filtersActive ? [...new Set(filtered.map(model => model.id))] : undefined
}
