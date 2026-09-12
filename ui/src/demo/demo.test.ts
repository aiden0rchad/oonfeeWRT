import controllerAPISource from '../lib/api.ts?raw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError, isDemo } from './api'
import { Live } from './live'
import { registerControllerApp } from './pwa'
import { clients, demoMAC, demoNow, demoSeconds, devices, series } from './fixtures'

const request = vi.fn(() => { throw new Error('Network use is forbidden in the isolated demo') })
beforeEach(() => {
  request.mockClear()
  vi.stubGlobal('fetch', request)
  vi.stubGlobal('WebSocket', request)
  vi.stubGlobal('EventSource', request)
})
afterEach(() => {
  expect(request).not.toHaveBeenCalled()
  vi.unstubAllGlobals()
})

describe('isolated demo', () => {
  it('supplies populated read-only workspaces without network calls or shared mutable responses', async () => {
    expect(isDemo).toBe(true)
    expect(await api.setupState()).toEqual({ needs_setup: false })
    expect((await api.session()).role).toBe('viewer')
    const results = await Promise.all([
      api.dashboard(), api.devices(), api.device(1), api.deviceSeries(1), api.clients(), api.radios(), api.alerts(), api.firmware(),
      api.site(), api.account(), api.accountSessions(), api.events(), api.policies(), api.lastNeighbours(), api.meshHealth(),
      api.restoreSuppression(), api.diagnostics(), api.backups(), api.restores(), api.scanPlan(), api.speedTests(),
      api.adguard(),
    ])
    expect(results[0].devices.total).toBe(4)
    expect(results[1].devices).toHaveLength(4)
    expect(results[4].clients).toHaveLength(8)
    expect(results[5].devices).toHaveLength(2)
    expect(results[6].rules).toHaveLength(3)
    expect(results[7].installation.enabled).toBe(false)
    expect(results.at(-1)).toMatchObject({ configured: false, has_password: false })
    results[1].devices[0].name = 'Changed only in caller memory'
    expect((await api.devices()).devices[0].name).toBe('Studio gateway')
  })

  it('refuses every unsupported production method, including present and future mutations', async () => {
    const source = controllerAPISource.split('export const api = {')[1]
    const methods = [...source.matchAll(/^  (\w+):/gm)].map((match) => match[1])
    const supported = new Set(Object.keys(api))
    const denied = methods.filter((name) => !supported.has(name))
    expect(denied).toEqual(expect.arrayContaining(['setup', 'login', 'logout', 'adopt', 'scan', 'scanRadio', 'startSpeedTest', 'checkFirmware', 'applySite', 'uploadRestore', 'saveAlertRule', 'saveAdguard', 'deleteAdguard', 'checkAdguard', 'checkWireGuard']))
    for (const name of [...denied, 'futureMutation']) {
      const method = (api as unknown as Record<string, (...args: unknown[]) => unknown>)[name]
      if (name === 'backupDownloadURL') expect(() => method('example')).toThrow(ApiError)
      else await expect(method('example')).rejects.toMatchObject({ status: 403, writeState: 'none' })
    }
    expect(methods.length).toBeGreaterThan(90)
  })

  it('never opens a live channel or schedules reconnections', () => {
    vi.useFakeTimers()
    try {
      const live = new Live()
      const state = vi.fn()
      live.onState = state
      live.connect()
      const unwatch = live.watch(1)
      const off = live.on(() => {})
      unwatch(); off(); live.close()
      expect(state).toHaveBeenCalledWith(false)
      expect(vi.getTimerCount()).toBe(0)
    } finally { vi.useRealTimers() }
  })

  it('never registers the controller service worker', () => {
    vi.stubGlobal('navigator', { serviceWorker: { register: request } })
    registerControllerApp()
    expect(request).not.toHaveBeenCalled()
  })

  it('returns deterministic, bounded, source-specific series with explicit gaps', async () => {
    const from = demoSeconds - 7 * 86_400
    const first = series('iface_rx_bps', 1, 'wan', from, demoSeconds)
    expect(first).toEqual(series('iface_rx_bps', 1, 'wan', from, demoSeconds))
    expect(first.points.length).toBeLessThan(2016)
    expect(first.points.length).toBeGreaterThan(1900)
    expect(first.points.every((point) => point.ts >= from && point.ts + 300 <= demoSeconds && point.min <= point.avg && point.max >= point.avg)).toBe(true)
    const longer = series('iface_rx_bps', 1, 'wan', demoSeconds - 31 * 86_400, demoSeconds)
    expect(longer.resolution).toBe('1h')
    expect(longer.points.length).toBeLessThanOrEqual(744)
    await expect(api.stats('iface_rx_bps', 99, 'wan', from, demoSeconds)).rejects.toThrow(/source/)
    await expect(api.stats('iface_rx_bps', 1, 'other', from, demoSeconds)).rejects.toThrow(/key/)
    await expect(api.stats('unknown', 1, '', from, demoSeconds)).rejects.toThrow(/source/)
    for (const [start, end] of [[NaN, demoSeconds], [from, Infinity], [from, from], [demoSeconds - 32 * 86_400, demoSeconds]]) {
      await expect(api.stats('iface_rx_bps', 1, 'wan', start, end)).rejects.toThrow(/ranges/)
    }
  })

  it('filters and pages clients with counts over matching data rather than a single page', async () => {
    const filtered = await api.clients({ presence: 'online', connection: 'wireless', scope: 'local', limit: 2, offset: 2 })
    expect(filtered.total).toBe(5)
    expect(filtered.clients).toHaveLength(2)
    expect(filtered.clients?.every((client) => client.online && client.connection === 'wireless' && client.scope === 'local')).toBe(true)
    expect(filtered.facets.presence).toContainEqual({ value: 'offline', count: 1 })
    expect((await api.clients({ presence: 'offline' })).clients).toHaveLength(1)
    expect((await api.clients({ offset: 100 })).clients).toEqual([])
    await expect(api.clients({ limit: -1 })).rejects.toThrow(/bounds/)
    await expect(api.clients({ offset: Infinity })).rejects.toThrow(/bounds/)
    await expect(api.clients({ scope: 'anything' })).rejects.toThrow(/filter/)
  })

  it('keeps topology references coherent and history bounded without inventing virtual hosts', async () => {
    const snapshot = await api.topology()
    const ids = new Set(snapshot.nodes.map((node) => node.id))
    expect(snapshot.edges.every((edge) => ids.has(edge.parent_id) && ids.has(edge.child_id))).toBe(true)
    expect(snapshot.nodes.filter((node) => node.kind === 'device')).toHaveLength(devices.length)
    expect(snapshot.last_known_edges).toHaveLength(1)
    expect(snapshot.last_known_edges![0].valid_to).toBeLessThan(snapshot.at)
    const historic = await api.topologyHistory(demoNow - 24 * 3600_000, demoNow)
    expect(historic.edges.every((edge) => edge.valid_from === demoNow - 24 * 3600_000)).toBe(true)
    await expect(api.topologyHistory(demoNow - 32 * 86_400_000, demoNow)).rejects.toThrow(/ranges/)
  })

  it('uses only synthetic locally administered identities and documentation IPs', () => {
    for (const item of [...devices, ...clients]) expect(item.mac).toMatch(/^02:00:5e:10:00:[0-9a-f]{2}$/)
    for (const device of devices) expect(device.host).toMatch(/^192\.0\.2\.\d+$/)
    for (const client of clients) expect(client.ipv4).toMatch(/^192\.0\.2\.\d+$/)
    expect(demoMAC(42)).toBe('02:00:5e:10:00:2a')
  })

  it('does not manufacture AP evidence for unassociated or offline clients', async () => {
    const offline = clients[7]
    const detail = await api.clientObservability(offline.mac, demoNow - 6 * 3600_000, demoNow)
    expect(detail.ap_device_at.every((id) => id === null)).toBe(true)
    expect(detail.metrics.every((metric) => metric.values.every((value) => value === null))).toBe(true)
    expect(detail.paths[0].complete).toBe(false)
    const associated = await api.clientObservability(clients[0].mac, demoNow - 6 * 3600_000, demoNow)
    expect(associated.metrics[0].values.every((value) => typeof value === 'number')).toBe(true)
  })

  it('honors abort signals without starting any transport', async () => {
    const controller = new AbortController()
    const pending = api.dashboard(controller.signal)
    controller.abort()
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' })
  })
})
