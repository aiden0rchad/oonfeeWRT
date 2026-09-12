import type { api as ControllerAPI, Client, ClientObservability, EventPage, SessionInfo } from '../lib/api'
import { alerts, assertRange, catalog, clients, dashboard, demoNow, demoSeconds, deviceDetail, devices, events, radios, series, site, speedTests, topology } from './fixtures'

export type * from '../lib/api'
export const isDemo = true
export const onUnauthorized = new Set<() => void>()
export const onControllerRestart = new Set<() => void>()

export class ApiError extends Error {
  readonly writeState = 'none'
  constructor(public status: number, message: string, public body?: unknown) { super(message) }
}

const session: SessionInfo = { admin_id: 1, username: 'demo-viewer', role: 'viewer', role_label: 'Read-only', csrf: '', reauthenticated_until: null }
const account = { id: 1, username: session.username, role: session.role, role_label: session.role_label, enabled: true,
  created_at: demoSeconds - 86_400, last_login_at: demoSeconds, active_session_count: 0 }

function reply<T>(value: T, signal?: AbortSignal): Promise<T> {
  return Promise.resolve().then(() => {
    if (signal?.aborted) throw new DOMException('Request aborted', 'AbortError')
    return structuredClone(value)
  })
}

function boundedInteger(value: number | undefined, fallback: number, minimum: number, maximum: number) {
  if (value == null) return fallback
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) throw new ApiError(400, 'Invalid demo pagination bounds.')
  return value
}

function facets<T>(rows: T[], select: (row: T) => string) {
  const result = new Map<string, number>()
  rows.forEach((row) => result.set(select(row), (result.get(select(row)) ?? 0) + 1))
  return [...result].sort(([a], [b]) => a.localeCompare(b)).map(([value, count]) => ({ value, count }))
}

function clientPage(query: Parameters<typeof ControllerAPI.clients>[0] = {}) {
  const q = query ?? {}
  const options = { presence: ['all', 'online', 'offline'], connection: ['all', 'wireless', 'unknown'], scope: ['all', 'local', 'upstream', 'unknown'] }
  for (const name of ['presence', 'connection', 'scope'] as const) {
    if (q[name] && !options[name].includes(q[name]!)) throw new ApiError(400, 'Unknown demo client filter.')
  }
  const value = (client: Client, name: keyof typeof options) => name === 'presence' ? client.online ? 'online' : 'offline' : client[name]
  const matches = (client: Client, excluding?: keyof typeof options) => (Object.keys(options) as Array<keyof typeof options>)
    .every((name) => name === excluding || !q[name] || q[name] === 'all' || value(client, name) === q[name])
  const matching = clients.filter((client) => matches(client))
  const limit = boundedInteger(q.limit, 500, 1, 5000)
  const offset = boundedInteger(q.offset, 0, 0, 100_000)
  return { clients: matching.slice(offset, offset + limit), total: matching.length, limit, offset,
    facets: {
      presence: facets(clients.filter((client) => matches(client, 'presence')), (client) => value(client, 'presence')),
      connection: facets(clients.filter((client) => matches(client, 'connection')), (client) => client.connection),
      scope: facets(clients.filter((client) => matches(client, 'scope')), (client) => client.scope),
    }, note: 'Synthetic demonstration clients; no devices were discovered or contacted.', scope_note: 'Documentation addresses and locally administered MAC addresses only.',
  }
}

function eventPage(query: Parameters<typeof ControllerAPI.events>[0] = {}): EventPage {
  const q = query ?? {}
  const scope = q.scope ?? 'general'
  if (!['general', 'audit'].includes(scope)) throw new ApiError(400, 'Unknown demo event scope.')
  const rows = scope === 'audit' ? events.slice(0, 6).map((event) => ({ ...event, Category: 'audit', Event: 'demo.readonly_visit', ClientMAC: '' })) : events
  const matching = rows.filter((event) => (!q.category || event.Category === q.category) && (!q.severity || event.Severity === q.severity))
  if (q.before && (!Number.isSafeInteger(q.before.ts) || !Number.isSafeInteger(q.before.id))) throw new ApiError(400, 'Invalid demo event cursor.')
  const after = matching.filter((event) => !q.before || event.TS < q.before.ts || (event.TS === q.before.ts && event.ID < q.before.id))
  const limit = boundedInteger(q.limit, 100, 1, 500)
  const offset = boundedInteger(q.offset, 0, 0, 100_000)
  const page = after.slice(offset, offset + limit)
  const last = page.at(-1)
  return { events: page, total: matching.length, scope, limit, offset,
    next_before: after.length > offset + limit && last ? { ts: last.TS, id: last.ID } : null,
    facets: { category: facets(rows, (event) => event.Category), severity: facets(rows, (event) => event.Severity) },
    conditions: [], router_clocks: [], generated_at: demoSeconds,
    coverage: { complete: true, expected_devices: devices.length, observed_devices: devices.length, gaps: [] },
  }
}

function clientObservability(mac: string, from: number, to: number): ClientObservability {
  assertRange(from, to, 1000)
  const client = clients.find((item) => item.mac === mac)
  if (!client) throw new ApiError(404, 'Unknown demonstration client.')
  const bucket = to - from > 7 * 86_400_000 ? 3_600_000 : 300_000
  const timestamps = []
  for (let ts = Math.ceil(from / bucket) * bucket; ts + bucket <= to; ts += bucket) timestamps.push(ts)
  const available = client.connection === 'wireless' && client.online
  const ap = devices.find((device) => device.id === client.device_id)
  return { client_mac: mac, from, to, resolution: bucket === 300_000 ? '5m' : '1h', bucket_ms: bucket, timestamps,
    ap_device_at: timestamps.map(() => available ? client.device_id ?? null : null),
    metrics: [{ id: 'signal', scope: 'client', kind: 'station_signal_dbm', label: 'Signal', unit: 'dBm',
      device_id: client.device_id, device_name: ap?.name, key: mac,
      values: timestamps.map((ts) => available ? (client.signal ?? -60) + Math.sin(ts / 900_000) * 3 : null),
      availability: { state: available ? 'available' : 'unavailable', source: 'rollup_5m', observed_points: available ? timestamps.length : 0,
        expected_points: timestamps.length, gaps: available ? [] : [{ from, to }], reason: available ? undefined : 'The synthetic client has no current AP association.' },
    }], events: [],
    paths: [{ from, to, complete: available, paths: available ? [{ node_ids: [`client:${mac}`, `device:${client.device_id}`, 'device:4', 'device:1', 'synthetic:internet'],
      labels: [client.name, ap?.name ?? 'Access point', devices[3].name, devices[0].name, 'Internet'], mediums: ['wireless', 'wired', 'wired', 'uplink'], confidence: 'measured' }] : [], gaps: available ? [] : ['No synthetic current association.'] }],
    gaps: [], experience_formula: { name: 'wifi-v1', weights: { rssi: .45, retry_delta: .35, tx_fail_delta: .2 }, missing_policy: 'Missing input remains unavailable.' },
    data_contract: { metric_source: 'synthetic_demo_rollups', raw_samples_persisted: false, event_time_resolution_ms: 1000, events_truncated: false, topology_source: 'synthetic demonstration intervals' },
  }
}

const readers = {
  setupState: () => reply({ needs_setup: false }), session: () => reply(session), account: () => reply({ account }), accountSessions: () => reply({ sessions: [] }),
  dashboard: (signal?: AbortSignal) => reply(dashboard, signal), devices: (signal?: AbortSignal) => reply({ devices }, signal),
  device: (id: number) => reply(deviceDetail(id)), deviceSeries: (id: number) => reply(catalog(id)),
  stats: (kind: string, id: number, key: string, from: number, to: number) => reply(series(kind, id, key, from, to)),
  clients: (q?: Parameters<typeof ControllerAPI.clients>[0]) => reply(clientPage(q)),
  clientObservability: (mac: string, from: number, to: number) => reply(clientObservability(mac, from, to)),
  topology: (at?: number, signal?: AbortSignal) => reply(topology(undefined, at ?? demoNow), signal),
  topologyHistory: (from: number, to: number, signal?: AbortSignal) => { assertRange(from, to, 1000); return reply(topology(from, to), signal) },
  radios: () => reply(radios), site: () => reply(site), wlan: (id: number) => {
    const wlan = site.wlans.find((item) => item.id === id)
    if (!wlan) throw new ApiError(404, 'Unknown demonstration WLAN.')
    return reply(wlan)
  },
  events: (q?: Parameters<typeof ControllerAPI.events>[0]) => reply(eventPage(q)), eventDetail: (id: number) => {
    const event = events.find((item) => item.ID === id)
    if (!event) throw new ApiError(404, 'Unknown demonstration event.')
    return reply(event)
  },
  alerts: (signal?: AbortSignal) => reply(alerts, signal),
  adguard: () => reply({ configured: false, url: '', username: '', has_password: false, tls_fingerprint: '' }),
  speedTests: (limit = 3) => reply({ ...speedTests, jobs: speedTests.jobs.slice(0, boundedInteger(limit, 3, 1, 3)) }),
  firmware: () => reply({ devices: devices.map((device) => ({ device_id: device.id, name: device.name, status: device.status, management_mode: 'managed',
    identity: { board_name: 'demo-reference', target: 'demo/generic', rootfs_type: 'squashfs', release: '25.12.0' }, identity_source: 'stored_capability_probe' as const })),
    checking: { source_url: '', scope: 'Synthetic fixture only', note: 'Release checks and downloads cannot run in this demonstration.' },
    agent: { available: false, installed_state: 'Not installed', package_path: '', note: 'No controller or router is connected.' },
    installation: { enabled: false, required_checks: ['Real hardware verification is required outside the demo.'] },
  }),
  scanPlan: () => reply({ networks: [], hosts: 0, skipped: ['Discovery is disabled in the isolated demonstration.'] }),
  policies: () => reply({ rows: [], capabilities: [] }), lastNeighbours: () => reply({ ran: false }), meshHealth: () => reply({ links: [] }),
  restoreSuppression: () => reply({ suppression: { active: false as const } }),
  diagnostics: () => reply({ mode: 'stored' as const, router_management_calls: false as const, router_changes: false as const,
    sections: [{ id: 'demo', label: 'Synthetic example', description: 'No support bundle is generated in the isolated demo.' }], excluded_secret_classes: ['All real credentials and device identities'],
    limits: { devices: 100, sources: 100, events: 1000, controller_log_input_bytes: 100_000, controller_log_output_bytes: 50_000, archive_bytes: 1_000_000, history: 5, retention_seconds: 86400, collection_timeout_seconds: 30 },
    controller_log: { available: false, gaps: [] }, jobs: [],
  }),
  backups: () => reply({ descriptor: { plan_id: 'demo-disabled', format: 'oonfeewrt-portable-backup' as const, format_version: 1 as const, file_extension: '.oowrtbak' as const,
    snapshot: 'Not available in the demo', encryption: 'No data is exported', includes: [], excludes: ['All real controller state'] },
    disclosure: { router_management_calls: false as const, router_changes: false as const, automatic_router_apply: false as const, separate_export_passphrase: true as const, export_passphrase_recoverable: false as const,
      summary: 'Exports are disabled in this isolated demonstration.' },
    limits: { history: 5, retention_seconds: 86400, export_timeout_seconds: 60, min_export_passphrase_characters: 12, max_export_passphrase_bytes: 1024 }, jobs: [],
  }),
  restores: () => reply({ descriptor: { format: 'oonfeewrt-portable-backup' as const, format_version: 1 as const, upload_content_type: 'application/vnd.oonfeewrt.backup' as const,
    confirmation_contract: 'controller-restore-confirm-v1' as const, typed_confirmation: 'RESTORE CONTROLLER' as const, confirmation_requires: [] },
    disclosure: { router_management_calls: false as const, router_changes: false as const, live_controller_changes: false as const, automatic_router_apply: false as const, summary: 'Uploads and restores are disabled in the isolated demonstration.' },
    limits: { max_upload_bytes: 1_000_000, max_database_bytes: 1_000_000, history: 5, retention_seconds: 86400, preview_timeout_seconds: 30, confirmation_timeout_seconds: 30, min_export_passphrase_characters: 12, max_export_passphrase_bytes: 1024 }, uploads: [], previews: [],
  }),
} satisfies Partial<typeof ControllerAPI>

/** Fail closed: missing methods never fall back to the real adapter, even when
 * new production capabilities are added. All writes are refused before I/O. */
export const api = new Proxy(readers, {
  get(target, name) {
    if (typeof name !== 'string' || name === 'then') return undefined
    if (Object.hasOwn(target, name)) return (...args: unknown[]) => Promise.resolve().then(() => Reflect.apply(Reflect.get(target, name), target, args))
    const refuse = () => { throw new ApiError(403, 'This action is disabled in the read-only demo. No controller or router was contacted.') }
    return name === 'backupDownloadURL' ? refuse : () => Promise.resolve().then(refuse)
  },
}) as unknown as typeof ControllerAPI
