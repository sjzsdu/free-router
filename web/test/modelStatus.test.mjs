import assert from 'node:assert/strict'
import test from 'node:test'

import { healthKey, isRouteModelHealthy, modelDisplayStatus, routeModelHealth } from '../src/modelStatus.ts'

const model = (provider = 'configured', routeTypes = ['chat']) => ({
  id: `${provider}/model`,
  provider,
  route_types: routeTypes,
})

const health = (target, capability, status) => ({
  model: target.id,
  capability,
  status,
  verified: true,
})

test('preserves every runtime health state instead of showing untested', () => {
  for (const status of ['healthy', 'degraded', 'open', 'half-open', 'cooling', 'unknown']) {
    const target = model()
    const states = new Map([[healthKey(target.id, 'chat'), health(target, 'chat', status)]])
    assert.equal(modelDisplayStatus(target, states, new Set(['configured'])), status)
  }
})

test('prioritizes an unhealthy capability in a multi-capability model', () => {
  const target = model('configured', ['chat', 'chat-tools'])
  const states = new Map([
    [healthKey(target.id, 'chat'), health(target, 'chat', 'healthy')],
    [healthKey(target.id, 'chat-tools'), health(target, 'chat-tools', 'degraded')],
  ])
  assert.equal(modelDisplayStatus(target, states, new Set(['configured'])), 'degraded')
})

test('route health mirrors model list degradation for configured route models', () => {
  const target = model('configured', ['chat', 'chat-tools'])
  const states = new Map([
    [healthKey(target.id, 'chat'), health(target, 'chat', 'healthy')],
    [healthKey(target.id, 'chat-tools'), health(target, 'chat-tools', 'degraded')],
  ])

  assert.equal(routeModelHealth(target, 'chat', states, new Map()).status, 'degraded')
})

test('route candidates exclude models degraded elsewhere in the model list', () => {
  const target = model('configured', ['chat', 'chat-tools'])
  const states = new Map([
    [healthKey(target.id, 'chat'), health(target, 'chat', 'healthy')],
    [healthKey(target.id, 'chat-tools'), health(target, 'chat-tools', 'degraded')],
  ])

  assert.equal(isRouteModelHealthy(target, 'chat', states, new Map()), false)
})

test('route health follows degraded runtime state even outside route_types', () => {
  const target = model('gemini', ['chat'])
  const states = new Map([
    [healthKey(target.id, 'chat'), health(target, 'chat', 'healthy')],
    [healthKey(target.id, 'chat-tools'), health(target, 'chat-tools', 'degraded')],
  ])

  assert.equal(modelDisplayStatus(target, states, new Set(['gemini'])), 'degraded')
  assert.equal(routeModelHealth(target, 'chat', states, new Map()).status, 'degraded')
  assert.equal(isRouteModelHealthy(target, 'chat', states, new Map()), false)
})

test('labels missing providers and expensive manual probes explicitly', () => {
  assert.equal(modelDisplayStatus(model('missing'), new Map(), new Set()), 'missing')
  assert.equal(modelDisplayStatus(model('configured', ['image-generation']), new Map(), new Set(['configured'])), 'manual')
})
