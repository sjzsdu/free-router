import assert from 'node:assert/strict'
import test from 'node:test'

import { bulkProbeLabel, bulkProbeModelIDs, hasActiveModelFilters } from '../src/modelProbeScope.ts'
import { listenForPageNavigation, pageFromSearch, pageURL, writePageHistory } from '../src/pageNavigation.ts'

test('restores valid tab deep links and safely falls back for invalid values', () => {
  assert.equal(pageFromSearch('?tab=models'), 'models')
  assert.equal(pageFromSearch('?tab=providers&other=value'), 'providers')
  assert.equal(pageFromSearch('?tab=unknown'), 'overview')
  assert.equal(pageFromSearch(''), 'overview')
})

test('writes copyable tab URLs while preserving unrelated query state', () => {
  const location = { href: 'http://localhost/admin/?token=local&oauth_status=success#section', search: '?token=local&oauth_status=success' }
  assert.equal(pageURL('routes', location), '/admin/?token=local&tab=routes#section')
})

test('syncs page state from browser back and forward navigation', () => {
  let current = new URL('http://localhost/admin/?tab=overview')
  const events = new EventTarget()
  const target = {
    get location() { return { href: current.href, search: current.search } },
    history: {
      pushState(_state, _unused, url) { current = new URL(String(url), current) },
      replaceState(_state, _unused, url) { current = new URL(String(url), current) },
    },
    addEventListener: events.addEventListener.bind(events),
    removeEventListener: events.removeEventListener.bind(events),
  }
  const pages = []
  const stop = listenForPageNavigation(target, page => pages.push(page))

  writePageHistory(target, 'models')
  assert.equal(current.search, '?tab=models')
  current = new URL('http://localhost/admin/?tab=providers')
  events.dispatchEvent(new Event('popstate'))
  current = new URL('http://localhost/admin/?tab=not-valid')
  events.dispatchEvent(new Event('popstate'))
  stop()

  assert.deepEqual(pages, ['providers', 'overview'])
})

test('changes bulk probe label and request scope only for active filters', () => {
  const models = [{ id: 'one' }, { id: 'two' }, { id: 'two' }]
  const empty = { search: '   ', type: '', provider: '', health: '' }
  const filtered = { ...empty, provider: 'configured' }

  assert.equal(hasActiveModelFilters(empty), false)
  assert.equal(bulkProbeLabel(false), '手动检测全部')
  assert.equal(bulkProbeModelIDs(models, false), undefined)
  assert.equal(hasActiveModelFilters(filtered), true)
  assert.equal(bulkProbeLabel(true), '手动检测筛选')
  assert.deepEqual(bulkProbeModelIDs(models.slice(0, 1), true), ['one'])
  assert.deepEqual(bulkProbeModelIDs([], true), [])
})
