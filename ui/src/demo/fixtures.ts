import type {
  Client, Dashboard, DashboardMetric, Device, DeviceDetail, EventRow, RadiosResponse,
  Series, Site, SpeedTestCollection, TopologyEdge, TopologySnapshot,
} from '../lib/api'
import type { AlertResponse } from '../lib/alerts'

// Original synthetic fixtures. No captured controller responses or identities.
// Locally administered MAC addresses and RFC 5737 documentation networks only.
export const demoNow = Math.floor(Date.now() / 300_000) * 300_000
export const demoSeconds = demoNow / 1000
const day = 86_400
export const demoMAC = (id: number) => `02:00:5e:10:00:${id.toString(16).padStart(2, '0')}`

export const devices: Device[] = [
  ['Studio gateway', 'gateway', ['gateway', 'switch']],
  ['Workspace access point', 'ap', ['ap']],
  ['Lounge access point', 'ap', ['ap']],
  ['Lab switch', 'switch', ['switch']],
].map(([name, role, functions], index) => ({
  id: index + 1, name: String(name), role: String(role), functions: functions as Device['functions'],
  mac: demoMAC(index + 1), host: `192.0.2.${index + 1}`, management_mode: 'managed', adopted: true,
  adopted_at: demoSeconds - 45 * day, class: index === 0 ? 'A' : 'B', firmware: 'OpenWrt 25.12.0',
  last_seen: demoSeconds, poll_state: 'baseline', status: 'online', tier: 'baseline',
}))

const clientNames = ['Design laptop', 'Desk workstation', 'Prototype lab', 'Guest tablet', 'Meeting display', 'Studio printer', 'Media bridge', 'Archived test phone']
export const clients: Client[] = clientNames.map((name, index) => ({
  mac: demoMAC(index + 32), name, ipv4: `192.0.2.${index + 40}`, first_seen: demoSeconds - (index + 2) * day,
  last_seen: demoSeconds - (index === 7 ? 4 * 3600 : 0), blocked: false,
  connection: index < 5 || index === 7 ? 'wireless' : 'unknown', online: index !== 7,
  ...(index < 5 ? { signal: -45 - index * 6, tx_retry_pct: 1.2 + index * .8, device_id: index < 3 ? 2 : 3 } : {}),
  scope: 'local', group: index === 3 ? 'Guests' : 'Studio',
}))

export function assertRange(from: number, to: number, unit = 1) {
  if (!Number.isSafeInteger(from) || !Number.isSafeInteger(to) || from < 0 || to <= from || to - from > 31 * day * unit) {
    throw new Error('Demo time ranges must be finite, ordered, and no longer than 31 days.')
  }
}

const metricKinds = new Set([
  'iface_rx_bps', 'iface_tx_bps', 'site_wan_latency_ms', 'site_wan_loss_pct', 'site_wan_up',
  'sys_load1', 'sys_mem_pct', 'radio_utilization_pct', 'radio_interference_pct',
  'radio_rx_airtime_pct', 'radio_tx_airtime_pct', 'radio_retry_delta_pct', 'radio_tx_fail_delta_pct', 'radio_signal_avg_dbm',
])

function metricValue(kind: string, ts: number, id: number) {
  const wave = Math.sin(ts / 3600 + id) * .5 + Math.sin(ts / 713 + id * 2) * .25
  const rush = Math.pow(Math.max(0, Math.sin(ts / day * Math.PI * 2 - 1)), 2)
  switch (kind) {
    case 'iface_rx_bps': return 7_000_000 + rush * 14_000_000 + wave * 2_000_000
    case 'iface_tx_bps': return 1_800_000 + rush * 4_000_000 + wave * 500_000
    case 'site_wan_latency_ms': return 15 + rush * 8 + wave * 3
    case 'site_wan_loss_pct': return Math.floor(ts / 300) % 103 === 0 ? .4 : 0
    case 'site_wan_up': return 1
    case 'sys_load1': return .25 + rush * .35 + wave * .12
    case 'sys_mem_pct': return 39 + id * 3 + rush * 7 + wave * 2
    case 'radio_utilization_pct': return 18 + id * 3 + rush * 15 + wave * 4
    case 'radio_interference_pct': return 3.5 + rush * 2 + wave
    case 'radio_retry_delta_pct': return 2.5 + rush * 3 + wave
    case 'radio_rx_airtime_pct': return 12 + rush * 9 + wave * 2
    case 'radio_tx_airtime_pct': return 8 + rush * 6 + wave
    case 'radio_tx_fail_delta_pct': return .3 + rush * .2
    case 'radio_signal_avg_dbm': return -52 + wave * 6
    default: throw new Error('This metric is not part of the isolated demo.')
  }
}

export function series(kind: string, deviceID: number, key: string, from: number, to: number): Series {
  assertRange(from, to)
  if (!devices.some((device) => device.id === deviceID) || !metricKinds.has(kind)) throw new Error('Unknown demo series source.')
  const keys = catalog(deviceID).series[kind]
  if (!keys?.includes(key)) throw new Error('Unknown demo series key.')
  const hourly = to - from > 7 * day
  const step = hourly ? 3600 : 300
  const points: Series['points'] = []
  for (let ts = Math.ceil(from / step) * step; ts + step <= to; ts += step) {
    // A reproducible collection gap, never converted to zero or interpolated.
    if (Math.floor(ts / step) % 211 === 0) continue
    const avg = metricValue(kind, ts, deviceID)
    const spread = kind === 'site_wan_up' || kind === 'site_wan_loss_pct' ? 0 : Math.abs(avg) * .06
    points.push({ ts, avg, min: avg < 0 ? avg - spread : Math.max(0, avg - spread), max: avg + spread, cnt: hourly ? 360 : 30 })
  }
  return { device_id: deviceID, kind, key, resolution: hourly ? '1h' : '5m', points }
}

export function catalog(id: number): { series: Record<string, string[]> } {
  const radio = id === 2 || id === 3
  return { series: {
    iface_rx_bps: id === 1 ? ['wan', 'br-lan'] : ['br-lan'], iface_tx_bps: id === 1 ? ['wan', 'br-lan'] : ['br-lan'],
    sys_load1: [''], sys_mem_pct: [''],
    ...(id === 1 ? { site_wan_latency_ms: [''], site_wan_loss_pct: [''], site_wan_up: [''] } : {}),
    ...(radio ? Object.fromEntries(['radio_utilization_pct', 'radio_interference_pct', 'radio_rx_airtime_pct', 'radio_tx_airtime_pct', 'radio_retry_delta_pct', 'radio_tx_fail_delta_pct', 'radio_signal_avg_dbm'].map((kind) => [kind, ['radio0', 'radio1']])) : {}),
  } }
}

export function deviceDetail(id: number): DeviceDetail {
  const device = devices.find((item) => item.id === id)
  if (!device) throw new Error('Unknown demo device.')
  const radio = device.functions?.includes('ap') ?? false
  return { ...device, capabilities: {
    Board: { Model: `Synthetic ${device.role} reference device`, Target: 'demo/generic', Release: '25.12.0', Kernel: '6.6' },
    Notes: ['Original demonstration fixture; not a hardware compatibility claim.'],
  }, interfaces: id === 1 ? ['wan', 'br-lan'] : ['br-lan'], wan_interface: id === 1 ? 'wan' : null,
  radios: radio ? ['radio0', 'radio1'] : [], stations: clients.filter((client) => client.device_id === id).map((client) => client.mac),
  degraded: [], broadcast_known: true, owned_sections_known: true, owned_sections: radio ? ['oonfeewrt_studio'] : [],
  broadcasting: radio ? [{ ssid: 'Studio demo', iface: 'phy0-ap0', origin: 'ours', section: 'oonfeewrt_studio' }] : [],
  }
}

export const events: EventRow[] = Array.from({ length: 18 }, (_, index) => ({
  ID: 100 - index, TS: demoSeconds - index * 720, DeviceID: index % 4 + 1,
  Category: index % 3 === 0 ? 'client' : 'system', Severity: index === 3 ? 'warning' : 'info',
  Event: index === 3 ? 'demo.wan_latency' : index % 3 === 0 ? 'client.associated' : 'device.observed',
  Detail: { message: index === 3 ? 'Synthetic brief latency increase; recovered after five minutes.' : 'Synthetic observation for the isolated demonstration.' },
  Source: 'demo-fixture', SourceID: `demo-event-${index}`, SourceBoot: 'demo-boot', IngestedAt: demoSeconds - index * 720,
  ClientMAC: index % 3 === 0 ? clients[index % clients.length].mac : '', Action: '', Direction: '', InIface: '', OutIface: '',
  SrcIP: '', DstIP: '', SrcPort: null, DstPort: null, ZoneIn: '', ZoneOut: '', PolicyID: null,
}))

function wanMetric(kind: string, sourceKind: string, unit: string, key: string): DashboardMetric {
  const observations = series(sourceKind, 1, key, demoSeconds - 6 * 3600, demoSeconds)
  const points = observations.points.map((point) => ({ ts: point.ts * 1000, value: point.avg }))
  return { kind, unit, meaning: 'Synthetic demonstration observations; not measured network traffic.',
    status: 'fresh', value: points.at(-1)?.value ?? null, as_of: demoNow, points }
}

export const dashboard: Dashboard = {
  devices: { total: devices.length, online: devices.length, offline: 0, pending: 0, unknown: 0 },
  wireless_clients: clients.filter((client) => client.online && client.connection === 'wireless').length,
  wireless_clients_complete: true, known_devices: clients.length, active_devices: clients.filter((client) => client.online).length,
  upstream_devices: 0, unscoped_devices: 0, gateway_uplinks: [{ device_id: 1, name: devices[0].name, state: 'up' }],
  focused_devices: 0, quiesced_devices: 0, series_count: 42, recent_events: events.slice(0, 6), recent_alert_events: events.filter((event) => event.Severity === 'warning'),
  wan: { target: '203.0.113.1', probe: 'icmp', freshness: 'fresh', as_of: demoNow,
    gateway: { device_id: 1, name: devices[0].name, route_interface: 'wan', series_key: 'wan' },
    resolution: '5m', bucket_ms: 300_000, from: demoNow - 6 * 3600_000, to: demoNow,
    metrics: {
      download_bps: wanMetric('site_wan_download_bps', 'iface_rx_bps', 'B/s', 'wan'),
      upload_bps: wanMetric('site_wan_upload_bps', 'iface_tx_bps', 'B/s', 'wan'),
      latency_ms: wanMetric('site_wan_latency_ms', 'site_wan_latency_ms', 'ms', ''),
      loss_pct: wanMetric('site_wan_loss_pct', 'site_wan_loss_pct', 'percent', ''),
      reachable: wanMetric('site_wan_reachable', 'site_wan_up', 'boolean', ''),
    },
  },
}

export function topology(from = demoNow - day * 1000, to = demoNow): TopologySnapshot {
  const nodes: TopologySnapshot['nodes'] = [
    { id: 'synthetic:internet', name: 'Internet', kind: 'synthetic', synthetic: true },
    ...devices.map((device) => ({ id: `device:${device.id}`, name: device.name, kind: 'device' as const, device_id: device.id, online: true, synthetic: false })),
    ...clients.map((client) => ({ id: `client:${client.mac}`, name: client.name, kind: 'client' as const, mac: client.mac, online: client.online, synthetic: false })),
  ]
  const edge = (id: number, parent: string, child: string, port: string, medium: TopologyEdge['medium'], confidence: TopologyEdge['confidence']): TopologyEdge => ({
    id, parent_id: parent, child_id: child, parent_port: port, medium, confidence,
    valid_from: from, last_seen: to, evidence: [{ kind: 'synthetic-example', source: 'demo-fixture', detail: { note: 'Illustrative evidence only; no router was contacted.' } }], ambiguities: [],
  })
  const edges = [
    edge(1, 'synthetic:internet', 'device:1', 'wan', 'uplink', 'measured'),
    edge(2, 'device:1', 'device:4', 'lan1', 'wired', 'measured'),
    edge(3, 'device:4', 'device:2', 'port2', 'wired', 'measured'),
    edge(4, 'device:4', 'device:3', 'port3', 'wired', 'measured'),
    ...clients.filter((client) => client.online).map((client, index) => edge(index + 10, `device:${client.device_id ?? 4}`, `client:${client.mac}`,
      client.connection === 'wireless' ? 'phy0-ap0' : `port${index + 4}`, client.connection === 'wireless' ? 'wireless' : 'wired', client.connection === 'wireless' ? 'measured' : 'inferred')),
  ]
  return { at: to, complete: true, truncated: false, gaps: [], nodes, edges,
    last_known_edges: [{ ...edge(90, 'device:3', `client:${clients[7].mac}`, 'phy0-ap0', 'wireless', 'measured'), valid_from: from - day * 1000, valid_to: from - 1000 }],
  }
}

export const radios: RadiosResponse = {
  generated_at: demoNow, gaps: [], devices: devices.filter((device) => device.functions?.includes('ap')).map((device) => ({
    device_id: device.id, name: device.name, status: { observed_at: demoNow, last_poll_ok: true, consecutive_failures: 0, stale: false },
    radios: [0, 1].map((index) => {
      const currentChannel = index === 0 ? (device.id === 2 ? 36 : 149) : (device.id === 2 ? 1 : 11)
      return { radio_key: `radio${index}`, up: true, band: index === 0 ? '5g' : '2g', configured_channel: String(currentChannel),
        htmode: index === 0 ? 'VHT80' : 'HT20', current_channel: currentChannel, current_mhz: index === 0 ? 5000 + currentChannel * 5 : 2407 + currentChannel * 5,
        inventory_observed_at: demoNow, channels_observed_at: demoNow, stale: false, interfaces: [{ name: `phy${index}-ap0`, mode: 'ap' }], channels_known: true,
        channels: (index === 0 ? [36, 40, 44, 48, 52, 56, 60, 64, 149, 153, 157, 161] : [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11]).map((channel) => ({
          band: index === 0 ? '5g' : '2g', channel, mhz: index === 0 ? 5000 + channel * 5 : 2407 + channel * 5,
          state: channel === currentChannel ? 'in-use' as const : 'enabled' as const, availability: 'enabled' as const, in_use: channel === currentChannel,
          restricted: false, dfs: index === 0 && channel >= 52 && channel <= 64, excluded: false, flags: [],
        })), scan_capability: 'absent' as const, latest_observations: [],
      }
    }),
  })),
}

export const site: Site = { name: 'Studio demonstration', uuid: '00000000-0000-4000-8000-000000000015',
  wlans: [{ id: 1, ssid: 'Studio demo', bands: ['2g', '5g'], security_mode: 'psk2', pmf: '1', network_id: 1, group_id: 1,
    has_key: true, roaming: { ft: true, ft_over_ds: false, kv: true, ft_with_psk2: true }, hidden: false, isolate: false, max_assoc: 64, allow_uplink: false, enabled: true }],
  meshes: [], uplinks: [], groups: [{ id: 1, name: 'Studio access points', device_ids: [2, 3] }],
  networks: [{ id: 1, name: 'studio', vlan: 1, cidr: '192.0.2.1/24', zone: 'lan', ipv6: { mode: 'preserve', assignment_length: 64 }, enabled: true,
    dhcp: { enabled: true, start: 40, limit: 100, leasetime: '12h' } }], zones: [{ name: 'lan', forward_to: ['wan'], explicit: true }],
  policies: [], policy_sets: [], policy_capabilities: [], problems: [], overrides: [], overridable: [], override_note: 'Isolated demonstration: configuration writes are disabled.',
}

export const alerts: AlertResponse = {
  evaluated_at: demoSeconds,
  rules: [
    { id: 1, name: 'Gateway reachability', condition: 'device_offline', device_id: 1, threshold: 0, hold_seconds: 300, cooldown_seconds: 3600, enabled: true,
      state: 'clear', since: demoSeconds - day, value: 0, observed_at: demoSeconds, reason: 'Synthetic healthy observation.' },
    { id: 2, name: 'Sustained WAN latency', condition: 'wan_latency', device_id: 1, threshold: 80, hold_seconds: 300, cooldown_seconds: 3600, enabled: true,
      state: 'clear', since: demoSeconds - 3600, value: 18.4, observed_at: demoSeconds, reason: 'Below the configured threshold.' },
    { id: 3, name: 'WAN packet loss', condition: 'wan_loss', device_id: 1, threshold: 3, hold_seconds: 300, cooldown_seconds: 3600, enabled: true,
      state: 'clear', since: demoSeconds - day, value: 0, observed_at: demoSeconds, reason: 'No sustained synthetic packet loss.' },
  ],
  incidents: [{ id: 1, rule_id: 2, rule_name: 'Sustained WAN latency', device_id: 1, device_name: devices[0].name, condition: 'wan_latency',
    state: 'resolved', started_at: demoSeconds - 3900, resolved_at: demoSeconds - 3600, value: 86, delivery_state: 'not_configured', delivery_error: '' }],
  delivery: { configured: false, host: '', enabled: false, last_attempt_at: null, last_success_at: null, last_error: '' },
}

export const speedTests: SpeedTestCollection = {
  jobs: [0, 1, 2].map((index) => ({ id: `demo-speed-${index}`, plan_id: 'demo-read-only', state: 'completed', phase: 'complete', progress_percent: 100,
    provider: 'Synthetic example', method: 'illustrative result', provenance: 'controller-host', endpoint: 'speed.example.invalid', estimated_bytes: 0,
    created_at: demoNow - (index + 1) * day * 1000, finished_at: demoNow - (index + 1) * day * 1000 + 30_000,
    download_mbps: [642, 618, 631][index], upload_mbps: [216, 208, 212][index], idle_latency_ms: 16 + index, idle_jitter_ms: 1.8 + index * .2,
    loaded_latency_ms: null, loaded_jitter_ms: null, bytes_downloaded: 0, bytes_uploaded: 0,
  })), active: null,
  test: { plan_id: 'demo-read-only', provider: 'Synthetic example', method: 'illustrative result', provenance: 'controller-host', endpoint: 'speed.example.invalid',
    download_endpoint: '', upload_endpoint: '', estimated_bytes: 0, max_duration_seconds: 0 }, limits: { max_history: 3 },
  disclosure: { vantage_point: 'demo', router_management_calls: false, router_changes: false, saturation_warning: 'Speed tests cannot run in the isolated demo.', privacy: 'All results are generated locally; nothing is sent to a provider.' },
}
