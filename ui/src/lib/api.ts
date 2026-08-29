// The API client.
//
// One place that knows about credentials, CSRF and error shapes, so no screen
// has to. Two rules it enforces on every call:
//
//  - Mutations carry the CSRF header. The token comes from a cookie the page is
//    allowed to read; the session cookie itself is HttpOnly and never touched
//    by this code.
//  - A 401 is not an error to display, it is a state change. It clears the
//    session and lets the app fall back to the sign-in screen rather than
//    leaving a logged-out page showing stale data.

export class ApiError extends Error {
  status: number
	  writeState?: 'none' | 'possible'
  /** The decoded response body.
   *
   *  Kept because some non-2xx responses ARE the answer rather than a failure:
   *  un-adopt returns 409 with the full report when phase 2 needs a credential
   *  the controller deliberately does not hold, and the residue list in that
   *  body is the whole point of the reply. */
  body: unknown
  constructor(status: number, message: string, body?: unknown) {
    super(message)
    this.status = status
    this.body = body
    if (body && typeof body === 'object' && 'write_state' in body) {
      const state = (body as { write_state?: unknown }).write_state
      if (state === 'none' || state === 'possible') this.writeState = state
    }
  }
}

/** Fires when the server says we are not signed in. */
export const onUnauthorized = new Set<() => void>()

/** Fires when an API response comes from a different daemon process. */
export const onControllerRestart = new Set<() => void>()
let controllerInstance = ''

function csrfToken(): string {
  const m = document.cookie.match(/(?:^|;\s*)oonfee_csrf=([^;]*)/)
  return m ? decodeURIComponent(m[1]) : ''
}

async function request<T>(
  path: string,
  init: RequestInit = {},
): Promise<T> {
  const method = (init.method ?? 'GET').toUpperCase()
  const headers = new Headers(init.headers)
  if (init.body !== undefined && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  if (method !== 'GET' && method !== 'HEAD') {
    headers.set('X-Oonfee-CSRF', csrfToken())
  }

  const resp = await fetch(`/api/v1${path}`, {
    ...init,
    headers,
    // Cookies are the session. Without this the browser omits them on
    // same-origin fetch in some configurations and every call 401s.
    credentials: 'same-origin',
  })

  const nextInstance = resp.headers.get('X-OonfeeWRT-Instance') ?? ''
  if (nextInstance) {
    if (controllerInstance && controllerInstance !== nextInstance) {
      controllerInstance = nextInstance
      onControllerRestart.forEach((fn) => fn())
      throw new ApiError(409, 'controller restarted')
    }
    controllerInstance = nextInstance
  }

  const credentialCheck = path === '/login' || path === '/session/password' || path === '/session/reauth'
  if (resp.status === 401 && !credentialCheck) {
    onUnauthorized.forEach((fn) => fn())
  }
  const text = await resp.text()
  let body: unknown
  try {
    body = text ? JSON.parse(text) : undefined
  } catch {
    // Reverse proxies and upstream failures commonly return text or HTML. Keep
    // their HTTP status, but do not leak a JSON SyntaxError past this boundary.
    throw new ApiError(
      resp.status,
      resp.ok
        ? `server returned an invalid response (${resp.status})`
        : `request failed (${resp.status})`,
    )
  }
  if (resp.status === 401 && credentialCheck) {
    const code = body && typeof body === 'object' && 'code' in body
      ? (body as { code?: unknown }).code
      : undefined
    const message = body && typeof body === 'object' && 'error' in body
      ? (body as { error?: unknown }).error
      : undefined
    const credentialWasRejected = path === '/login' || code === 'incorrect_password' ||
      message === 'current password is incorrect'
    if (!credentialWasRejected) onUnauthorized.forEach((fn) => fn())
  }
  if (!resp.ok) {
    const message = body && typeof body === 'object' && 'error' in body &&
      typeof (body as { error?: unknown }).error === 'string'
      ? (body as { error: string }).error
      : `request failed (${resp.status})`
    throw new ApiError(resp.status, message, body)
  }
  if (body === undefined) {
    throw new ApiError(resp.status, `server returned an empty response (${resp.status})`)
  }
  return body as T
}

const get = <T>(path: string) => request<T>(path)
const post = <T>(path: string, body?: unknown) =>
  request<T>(path, { method: 'POST', body: body ? JSON.stringify(body) : undefined })
const postRaw = <T>(path: string, body: BodyInit, contentType: string) =>
  request<T>(path, { method: 'POST', body, headers: { 'Content-Type': contentType } })
const patch = <T>(path: string, body: unknown) =>
  request<T>(path, { method: 'PATCH', body: JSON.stringify(body) })
const del = <T>(path: string) => request<T>(path, { method: 'DELETE' })

async function download(
  path: string,
  maxBytes: number,
  expectedBytes: number,
): Promise<{ blob: Blob; filename: string }> {
  if (!Number.isSafeInteger(maxBytes) || maxBytes <= 0 ||
    !Number.isSafeInteger(expectedBytes) || expectedBytes <= 0 || expectedBytes > maxBytes) {
    throw new ApiError(0, 'invalid diagnostic download bounds')
  }
  const resp = await fetch(`/api/v1${path}`, { credentials: 'same-origin' })
  const nextInstance = resp.headers.get('X-OonfeeWRT-Instance') ?? ''
  if (nextInstance) {
    if (controllerInstance && controllerInstance !== nextInstance) {
      controllerInstance = nextInstance
      onControllerRestart.forEach((fn) => fn())
      throw new ApiError(409, 'controller restarted')
    }
    controllerInstance = nextInstance
  }
  if (resp.status === 401) onUnauthorized.forEach((fn) => fn())
  if (!resp.ok) {
    let body: unknown
    try {
      body = JSON.parse(await resp.text())
    } catch {
      body = undefined
    }
    const message = body && typeof body === 'object' && 'error' in body &&
      typeof (body as { error?: unknown }).error === 'string'
      ? (body as { error: string }).error
      : `request failed (${resp.status})`
    throw new ApiError(resp.status, message, body)
  }
  if ((resp.headers.get('Content-Type') ?? '').split(';')[0].trim() !== 'application/zip') {
    throw new ApiError(resp.status, 'server returned a non-ZIP diagnostic download')
  }
  const contentLength = resp.headers.get('Content-Length') ?? ''
  const announcedBytes = /^\d+$/.test(contentLength) ? Number(contentLength) : NaN
  if (!Number.isSafeInteger(announcedBytes) || announcedBytes <= 0 ||
    announcedBytes > maxBytes || announcedBytes !== expectedBytes) {
    throw new ApiError(resp.status, 'server returned an invalid diagnostic download size')
  }
  const disposition = resp.headers.get('Content-Disposition') ?? ''
  const filename = /filename="([^"\r\n]+)"/.exec(disposition)?.[1] ?? 'oonfeewrt-diagnostics.zip'
  const blob = await resp.blob()
  if (blob.size > maxBytes || blob.size !== expectedBytes) {
    throw new ApiError(resp.status, 'server returned an invalid diagnostic download size')
  }
  return { blob, filename }
}

// ---- types, mirroring the Go response structs ----

export interface Device {
  id: number
  mac: string
  name: string
  host: string
  role: string
  /** Independently managed device functions. Older servers and rows may omit
   *  this; `role` remains the compatibility fallback. */
  functions?: DeviceFunction[]
  adopted: boolean
  adopted_at: number | null
  class: string | null
  firmware: string
  /** null means never polled — NOT the epoch. */
  last_seen: number | null
  poll_state: string
  status: 'online' | 'offline' | 'pending' | 'unknown'
  tier?: string
  quiesced?: boolean
}

/** One call the last poll could not use, its failure domain, and what that
 *  costs. This carries both standing ACL/driver limits and current exchange
 *  failures; `cause` and `permanent` tell the UI which is which. */
export interface Degradation {
  call: string
  error: string
  /** Failure domain supplied by the collector; never inferred from `error`. */
  cause: 'permission' | 'unsupported' | 'device' | 'transport' | 'protocol' | 'decode' | 'unknown'
  /** Present only when the device returned a ubus status code. */
  status?: { code: number; name: string }
  /** Whether retrying the same exchange can help. `cause` determines whether
   *  a non-retryable result is a device limit or a controller/protocol fault. */
  permanent: boolean
  costs?: string
}

/** One BSS on the air, whether or not this controller put it there. */
export interface Broadcast {
  ssid: string
  iface: string
  bssid?: string
  /** The wifi-iface section that created it, when the device said. */
  section?: string
  /** Who wrote the section — decided from the SECTION, never the SSID.
   *  "ours": in owned_sections, so un-adopt can put it back.
   *  "foreign": a section this controller did not write.
   *  "unknown": the device did not say, or no poll has read the list. Not
   *  foreign — a check that could not run must not return a verdict. */
  origin: 'ours' | 'foreign' | 'unknown'
  /** Present only for foreign sections: what it would take to bring this SSID
   *  under management, and what it would cost. The controller runs none of it. */
  brief?: TakeoverBrief
}

/** What it would take to manage an SSID oonfeeWRT did not create.
 *
 *  There is no passphrase field, and no field saying whether one exists. The
 *  controller does not read key material it did not set. */
export interface TakeoverBrief {
  section: string
  ssid: string
  iface: string
  mode?: string
  /** False for anything that is not a plain access point, including an
   *  interface whose mode was never read. It may be the device's only path to
   *  the network, and no instructions are offered for that. */
  safe_to_disable: boolean
  refusal?: string
  /** The OTHER devices that would start transmitting this SSID if it were
   *  recreated here — oonfeeWRT has no per-device WLANs. */
  would_start_broadcasting?: string[]
  recipe?: string[]
  cost?: string[]
  note?: string
  decided_by?: string
  decided_at?: number
}

export interface DeviceDetail extends Device {
  capabilities: Registry | null
  interfaces: string[]
  radios: string[]
  stations: string[]
  degraded?: Degradation[]
  /** Every SSID the device is actually putting on the air. */
  broadcasting?: Broadcast[]
  /** Distinguishes "the last poll saw no BSS" from "no poll has looked". An
   *  empty list with this false is not a claim that the radios are silent. */
  broadcast_known: boolean
  /** The UCI sections this controller wrote, which un-adopt would revert.
   *  Named rather than counted, because un-adopt is not rollback-armed. */
  owned_sections?: string[]
  /** False means the ownership ledger could not be read. An absent/empty
   *  owned_sections list is safe to interpret as empty only when this is true.
   *  Optional solely for compatibility with older controller responses. */
  owned_sections_known?: boolean
}

export interface Registry {
  Board?: { Model?: string; Target?: string; Release?: string; Kernel?: string }
  Class?: number
  Features?: Record<string, number>
  Quirks?: { Source: string; Field: string; Reason: string }[]
  Radios?: { Device: string; Channel: number; Hardware: string; NoiseStable?: number }[]
  Notes?: string[]
}

export interface Client {
  mac: string
  name: string
  ipv4?: string
  /** Desired-state grouping used by Object Manager. Polling never rewrites it. */
  group?: string
  /** Desired static lease. Empty/absent means dynamic addressing. */
  fixed_ip?: string
  first_seen: number | null
  last_seen: number | null
  blocked: boolean
  /** "wireless" when a managed AP reports it in a baseline hostapd read;
   *  never "wired" by inference. */
  connection: 'wireless' | 'unknown'
  online: boolean
  /** Absent, not zero, when no managed AP reports this client or RSSI. */
  signal?: number
  tx_retry_pct?: number
  device_id?: number
  /** Current evidence contains competing managed devices or BSSes, so AP/RF
   *  fields are withheld rather than selected by response order. */
  association_ambiguous?: boolean
  /** Which side of the router: a client of this network, a neighbour on its
   *  uplink, or not established. "unknown" is a real answer — a host with no
   *  observed address has not been shown to be either. */
  scope: 'local' | 'upstream' | 'unknown'
}

export interface Point {
  ts: number
  avg: number
  min: number
  max: number
  cnt: number
}

export interface Series {
  device_id: number
  kind: string
  key: string
  resolution: '5m' | '1h'
  points: Point[]
}

export interface EventRow {
  /** Stable database identity used by keyset paging and detail lookup. */
  ID: number
  TS: number
  DeviceID: number | null
  Category: string
  Severity: string
  Event: string
  Detail: unknown
  Source: string
  SourceID: string
  SourceBoot: string
  IngestedAt: number
  ClientMAC: string
  Action: string
  Direction: string
  InIface: string
  OutIface: string
  SrcIP: string
  DstIP: string
  SrcPort: number | null
  DstPort: number | null
  ZoneIn: string
  ZoneOut: string
  PolicyID: number | null
}

export interface EventCursor {
  /** Event time remains Unix seconds; id breaks timestamp ties. */
  ts: number
  id: number
}

/** One filter option and how many rows would match it. */
export interface Facet {
  value: string
  count: number
}

/** A page of the event log, plus counts taken over the whole table.
 *
 *  `facets` is the reason this is not just an array: UI-SPEC §5 requires filter
 *  counts from an aggregate query, and counting the returned page would report
 *  "3 errors" from a page of 100 while the table holds three hundred. */
export interface EventPage {
  events: EventRow[] | null
  total: number
  limit: number
  /** Present only on the legacy offset endpoint. */
  offset?: number
  scope?: 'general' | 'audit'
  next_before?: EventCursor | null
  facets: { category: Facet[]; severity: Facet[] }
  coverage: {
    complete: boolean
    expected_devices: number
    observed_devices: number
    gaps: string[]
  }
}

export interface TopologyNode {
  id: string
  kind: 'device' | 'client' | 'synthetic'
  name: string
  device_id?: number
  mac?: string
  online?: boolean
  synthetic: boolean
}

export interface TopologyEvidence {
  kind: string
  source: string
  device_id?: number
  /** Sanitized fields selected by the server, never a raw router payload. */
  detail: Record<string, unknown>
}

export interface TopologyEdge {
  id: string | number
  child_id: string
  parent_id: string
  parent_device_id?: number
  parent_port?: string
  medium: 'wired' | 'wireless' | 'mesh' | 'uplink' | 'unknown'
  confidence: 'measured' | 'inferred' | 'ambiguous'
  valid_from: number
  valid_to?: number
  last_seen: number
  evidence: TopologyEvidence[]
  ambiguities: string[]
}

export interface TopologySnapshot {
  /** Unix milliseconds represented by this graph. */
  at: number
  complete: boolean
  /** Older intervals were omitted by retention or the bounded API response. */
  truncated: boolean
  nodes: TopologyNode[]
  edges: TopologyEdge[]
  /** Latest closed placement for currently unplaced devices; never current evidence. */
  last_known_edges?: TopologyEdge[]
  /** Sorted sources the controller could not observe. */
  gaps: string[]
}

export type ChannelPlanState = 'in-use' | 'enabled' | 'restricted' | 'unknown'

export interface RadioChannel {
  band?: string
  channel: number
  mhz: number
  state: ChannelPlanState
  availability: Exclude<ChannelPlanState, 'in-use'>
  in_use: boolean
  /** null means iwinfo did not report it. */
  restricted: boolean | null
  /** Always null until a regulatory source proves DFS independently. */
  dfs: boolean | null
  /** Always null until a persisted exclusion model exists. */
  excluded: boolean | null
  flags: string[]
}

export interface RadioScanBSS {
  scan_id: number
  bssid: string
  ssid: string
  mhz: number
  channel: number
  signal?: number
  width?: number
}

export interface RadioScan {
  id: number
  radio: { device_id: number; radio_key: string }
  started_at: number
  finished_at?: number
  status: 'pending' | 'running' | 'completed' | 'failed'
  detail: Record<string, unknown>
}

export interface RadioSuggestion {
  channel: number
  mhz: number
  score: number
  basis: string
  scan_id: number
  observed_at: number
}

export interface RadioView {
  radio_key: string
  up?: boolean
  disabled?: boolean
  pending?: boolean
  band?: string
  configured_channel?: string
  htmode?: string
  current_mhz?: number
  current_channel?: number
  current_ambiguous?: boolean
  inventory_observed_at?: number
  channels_observed_at?: number
  stale: boolean
  interfaces: { name: string; mode?: string }[]
  channels: RadioChannel[]
  channels_known: boolean
  scan_capability: 'present' | 'absent' | 'not-observable' | 'unknown'
  latest_scan?: RadioScan
  latest_observations: RadioScanBSS[]
  suggested?: RadioSuggestion
}

export interface RadioCollectionStatus {
  observed_at?: number
  last_poll_at?: number
  last_poll_ok: boolean
  consecutive_failures: number
  last_source_attempt_at?: number
  last_source_attempt_ok?: boolean
  stale: boolean
}

export interface RadiosResponse {
  generated_at: number
  devices: { device_id: number; name: string; status?: RadioCollectionStatus; radios: RadioView[] }[]
  gaps: string[]
}

export interface ObservabilityGap {
  from: number
  to: number
}

export interface MetricAvailability {
  state: 'available' | 'partial' | 'unavailable'
  /** `rollup_5m`, `rollup_1h`, or the fixed formula derived from one. */
  source: string
  observed_points: number
  expected_points: number
  gaps: ObservabilityGap[]
  reason?: string
}

export interface ClientObservabilityMetric {
  id: string
  scope: 'client' | 'ap' | 'site'
  kind: string
  label: string
  unit: string
  device_id?: number
  device_name?: string
  key?: string
  /** Index-aligned with ClientObservability.timestamps. Null is unavailable. */
  values: (number | null)[]
  /** Stored rollup envelope and source-sample count, aligned with values.
   *  Optional only so the UI remains honest during a rolling controller/UI upgrade. */
  mins?: (number | null)[]
  maxs?: (number | null)[]
  counts?: (number | null)[]
  availability: MetricAvailability
}

export interface ClientObservabilityEvent {
  id: number
  /** Unix milliseconds; data_contract names its source resolution. */
  ts: number
  device_id?: number
  category: string
  severity: string
  event: string
  detail: Record<string, unknown>
  source: string
  source_id?: string
  source_boot?: string
  ingested_at: number
  client_mac: string
  action?: string
  direction?: string
  in_iface?: string
  out_iface?: string
  src_ip?: string
  dst_ip?: string
  src_port?: number
  dst_port?: number
  zone_in?: string
  zone_out?: string
  policy_id?: number
}

export interface ClientObservabilityPath {
  node_ids: string[]
  labels: string[]
  mediums: string[]
  confidence: string
}

export interface ClientObservabilityPathInterval {
  from: number
  to: number
  complete: boolean
  paths: ClientObservabilityPath[]
  gaps: string[]
}

export interface ClientObservability {
  client_mac: string
  from: number
  to: number
  resolution: '5m' | '1h'
  bucket_ms: number
  timestamps: number[]
  /** AP provenance at each timestamp, aligned with timestamps. */
  ap_device_at: (number | null)[]
  metrics: ClientObservabilityMetric[]
  events: ClientObservabilityEvent[]
  paths: ClientObservabilityPathInterval[]
  gaps: string[]
  experience_formula: {
    name: 'wifi-v1'
    weights: { rssi: number; retry_delta: number; tx_fail_delta: number }
    missing_policy: string
  }
  data_contract: {
    metric_source: string
    raw_samples_persisted: false
    event_time_resolution_ms: number
    events_truncated: boolean
    topology_source: string
  }
}

/** What a capability change licenses a reader to conclude.
 *
 *  The two "observable" values are the load-bearing ones: they mean the
 *  controller's view changed, NOT the device. Rendering them the same as
 *  gained/lost would report a narrowed ACL as missing hardware. */
export type CapEffect =
  | 'gained'
  | 'lost'
  | 'now-observable'
  | 'no-longer-observable'
  | 'first-observation'
  | 'changed'

export interface CapChange {
  kind: string
  name: string
  from: string
  to: string
  effect: CapEffect
  detail: string
}

export interface ReprobeResult {
  device_id: number
  name: string
  summary: string
  unchanged: boolean
  changes: CapChange[] | null
  /** How many changes alter what may be rendered or sent. Visibility changes
   *  are excluded — the device is the same device. */
  actionable: number
  capabilities: Registry | null
  /** Where this device's role and its hardware disagree, as the probe just
   *  found it. A device that loses a radio has not only lost a radio, it has
   *  stopped matching the role it was adopted under. */
  role_fit?: string[]
  note: string
}

export interface RefreshACLResult {
  device_id: number
  name: string
  acl_updated: boolean
  controller_verified: boolean
  features: string[]
  unobservable?: string[]
}

export interface LLDPCapabilityResult {
  device_id: number
  name: string
  state: 'not_installed' | 'install_planned' | 'installing' | 'installed' | 'configure_planned' | 'remove_planned' | 'removing' | 'error'
  package_manager?: 'apk' | 'opkg'
  requested_packages: string[]
  added_packages: string[]
  plan?: string
  plan_hash?: string
  diagnostics?: string
  configuration_state?: 'package_default' | 'planned' | 'configured' | 'incomplete'
  configured_interfaces?: string[]
  service_enabled?: boolean
  service_running?: boolean
  detail?: string
}

/** One page of the client grid. Same shape as EventPage and for the same
 *  reason: filters, paging and facet counts are all server-side, so the rail
 *  counts the whole filtered table rather than the rows that arrived. */
export interface ClientPage {
  clients: Client[] | null
  total: number
  limit: number
  offset: number
  facets: { presence: Facet[]; connection: Facet[]; scope: Facet[] }
  note: string
  scope_note: string
}

export interface Dashboard {
  devices: {
    total: number
    online: number
    offline: number
    pending: number
    unknown: number
  }
  /** Online, local Client Devices rows whose current hostapd state or recent
   *  station telemetry identifies them as wireless. This is the same count as
   *  Client Devices with Network=this network, Presence=online and
   *  Connection=wireless. It is safe to present as the full total only when
   *  wireless_clients_complete is true. */
  wireless_clients: number
  /** False when at least one adopted device could not report its current
   *  station set. In that state wireless_clients is still the exact number of
   *  matching rows identified by available evidence, but not a fleet total. */
  wireless_clients_complete: boolean
  wireless_clients_unknown_on?: string[]
  /** Hosts on THIS network — a different question from wireless_clients, and
   *  scoped to `local`: a gateway's neighbour tables also cover its uplink.
   *  upstream_devices and unscoped_devices are the excluded remainder, so the
   *  headline can say what it left out. */
  known_devices: number
  active_devices: number
  upstream_devices: number
  unscoped_devices: number
  /** Current default-route truth for each adopted gateway. `missing` is used
   * only after a fresh successful interface observation on an online device. */
  gateway_uplinks: Array<{
    device_id: number
    name: string
    state: 'up' | 'missing' | 'unknown'
  }>
  focused_devices: number
  quiesced_devices: number
  /** Legacy general activity retained for older Dashboard consumers. */
  recent_events: EventRow[] | null
  /** Newest retained general warning/error rows. Empty means confirmed none;
   * null or absent means unavailable or an older controller. */
  recent_alert_events?: EventRow[] | null
  series_count: number
  /** Server-selected WAN evidence. Interface throughput is only present when
   * the observed default-route interface exactly matches a stored series key. */
  wan: DashboardWAN
}

export type DashboardWANFreshness = 'fresh' | 'last_observed' | 'unavailable'
export type DashboardMetricStatus = 'fresh' | 'last_observed' | 'unavailable'

export interface DashboardMetricPoint {
  ts: number
  value: number | null
}

export interface DashboardMetric {
  kind: string
  unit: string
  meaning: string
  status: DashboardMetricStatus
  value: number | null
  as_of: number | null
  points: DashboardMetricPoint[]
}

export interface DashboardWAN {
  target: string
  probe: 'icmp'
  freshness: DashboardWANFreshness
  as_of: number | null
  gateway: {
    device_id: number
    name: string
    route_interface: string
    series_key: string | null
  } | null
  resolution: '5m'
  bucket_ms: number
  from: number
  to: number
  metrics: {
    download_bps: DashboardMetric
    upload_bps: DashboardMetric
    latency_ms: DashboardMetric
    loss_pct: DashboardMetric
    reachable: DashboardMetric
  }
}

export type SpeedTestState = 'queued' | 'running' | 'cancelling' | 'completed' | 'failed'

export interface SpeedTestJob {
  id: string
  plan_id: string
  state: SpeedTestState
  phase: string
  progress_percent: number
  provider: string
  method: string
  provenance: string
  endpoint: string
  estimated_bytes: number
  created_at: number
  started_at?: number | null
  finished_at?: number | null
  download_mbps?: number | null
  upload_mbps?: number | null
  idle_latency_ms?: number | null
  idle_jitter_ms?: number | null
  loaded_latency_ms?: number | null
  loaded_jitter_ms?: number | null
  bytes_downloaded: number
  bytes_uploaded: number
  error?: string | null
}

export interface SpeedTestCollection {
  jobs: SpeedTestJob[]
  active: SpeedTestJob | null
  test: {
    plan_id: string
    provider: string
    method: string
    provenance: string
    endpoint: string
    download_endpoint: string
    upload_endpoint: string
    estimated_bytes: number
    max_duration_seconds: number
  }
  limits: {
    max_history: number
  }
  disclosure: {
    vantage_point: string
    router_management_calls: boolean
    router_changes: boolean
    saturation_warning: string
    privacy: string
  }
}

/** What a device is for. A closed vocabulary — the API refuses anything else
 *  rather than storing it, because the role decides what gets sent to the
 *  device and a typo used to mean "silently an access point". */
export type DeviceFunction = 'gateway' | 'ap' | 'switch'
export type DeviceRole = DeviceFunction

export interface AdoptResult {
  device_id: number
  mac: string
  name: string
  model: string
  class: string
  firmware: string
  /** The functions accepted at adoption. Absent on a legacy server. */
  functions?: DeviceFunction[]
  cert_fp?: string
  features?: string[]
  /** Checks that were REFUSED, not features the hardware lacks. */
  unobservable?: string[]
  quirks?: string[]
  notes?: string[]
  /** Facts about the DEVICE worth knowing — not controller problems. */
  warnings?: string[]
}

/** Credentialed, read-only adoption preflight. It creates no controller
 *  account and writes no configuration to the device. */
export interface InspectResult {
  mac: string
  model: string
  class: string
  firmware: string
  radio_count: number | null
  /** Board-declared LAN device. A no-switch router may expose only this. */
  lan_device?: string
  /** Independently addressable switch members, not a direct LAN device. */
  lan_ports: string[]
  wan_port?: string
  /** What inspection can safely claim about the switch. DSA support remains
   *  conditional on an already VLAN-aware bridge; observe-only is telemetry. */
  switch_mode: 'dsa-conditional' | 'observe-only' | 'unknown' | 'none'
  functions_supported: DeviceFunction[]
  functions_recommended: DeviceFunction[]
  /** Evidence that could not be read is not the same as absent hardware. */
  functions_unknown?: DeviceFunction[]
  /** Null means the read-only check was unavailable, not that the fact is false. */
  gateway_evidence: {
    active_wan_default_route: boolean | null
    lan_dhcp_enabled: boolean | null
  }
  notes?: string[]
  unobservable?: string[]
  /** Server-built strict allowlist for sharing hardware compatibility evidence.
   *  Absent only when an older server responds or evidence fails safety bounds. */
  compatibility_report?: CompatibilityReport
}

export interface CompatibilityReport {
  format: 'oonfeewrt-compatibility-report'
  format_version: 1
  controller_version: string
  evidence: {
    source: 'read-only-inspection'
    router_changes: false
    persisted: false
  }
  privacy: {
    sanitized: true
    excluded: string[]
  }
  hardware: {
    board: {
      model: string
      board_name: string
      system: string
      kernel: string
      target: string
      release: string
      rootfs_type: string
    }
    class: string
    radio_inventory_state: 'unknown' | 'present' | 'absent' | 'not-observable'
    radio_count: number | null
    radios: {
      band: string
      hardware: string
      hw_modes: string[]
      survey_state: 'unknown' | 'present' | 'absent' | 'not-observable'
      noise_stability: 'unknown' | 'present' | 'absent' | 'not-observable'
    }[]
    ports: {
      lan_device?: string
      lan_ports: string[]
      wan_device?: string
      switch_mode: InspectResult['switch_mode']
    }
  }
  features: {
    name: string
    state: 'unknown' | 'present' | 'absent' | 'not-observable'
  }[]
  functions: {
    supported: DeviceFunction[]
    unknown: DeviceFunction[]
  }
}

export interface UnadoptResult {
  removed_from_inventory: boolean
  reverted_sections: number
  /** True only when every owned UCI section was proved deleted and committed.
   *  Optional for compatibility with older controller responses. */
  config_revert_complete?: boolean
  /** Exact owned sections not proved gone. These survive in the response even
   *  after a forced inventory deletion, when the ledger no longer exists. */
  config_remains?: string[]
  login_removed: boolean
  acl_removed: boolean
  footprint_remains: boolean
  residue?: string[]
  /** Exact stock-OpenWrt commands for residue left on the device. */
  cleanup_commands?: string[]
  errors?: string[]
  needs_operator_credential: boolean
  /** The call's overall failure, when there was one. Present on a non-2xx that
   *  still carries the whole report — a phase-2 failure, or a forced removal
   *  that connected and then could not commit. */
  error?: string
}

/** What a scan would cover, before running one. */
export interface ScanPlan {
  networks: string[]
  /** How many addresses would be probed. Shown before scanning, because a
   *  sweep is unsolicited traffic on the operator's own network. */
  hosts: number
  /** Why a network is NOT in the list. Without this, a controller that
   *  declined to look at the operator's subnet reports "nothing found", which
   *  reads as a fact about their network rather than about itself. */
  skipped?: string[]
}

export interface Discovered {
  host: string
  port: number
  scheme: string
  verdict: 'openwrt' | 'reachable' | 'silent'
  signals: {
    objects: number
    /** Distinct hostapd PHYs with a running BSS — configured radios, not
     *  installed silicon. */
    radios: number
    /** Compatibility name: true means a WAN-named rpcd interface object was
     *  published, not that a default route or forwarding was observed. */
    gateway: boolean
    /** Compatibility name: true means a dnsmasq/dhcp rpcd object was
     *  published, not that an enabled LAN pool was observed. */
    dhcp: boolean
    wireless: boolean
  }
  note?: string
  /** Set when an adopted device currently has this address. Matched on address,
   *  which is a hint: identity is the MAC, and the MAC cannot be read before
   *  authenticating. */
  known_device_id?: number
  known_name?: string
}

export interface ScanResult {
  found: Discovered[]
  /** swept/answered make an empty `found` legible. */
  swept: number
  answered: number
  networks: string[]
  skipped?: string[]
  /** Networks the controller attempted but could not test. `unreachable`
   *  means every dial returned EHOSTUNREACH or ENETUNREACH, so an empty
   *  `found` is not evidence that the subnet has no devices. */
  failures?: Array<{
    network: string
    reason: 'unreachable'
    attempts: number
  }>
  elapsed_ms: number
}

// ---- the site model (Phase 2) ----
//
// Editing any of this changes nothing on any device. It is desired state; it
// reaches hardware only when someone previews and applies.

export interface WLAN {
  id: number
  ssid: string
  network_id: number
  group_id: number
  bands: string[]
  security_mode: 'sae' | 'sae-mixed' | 'psk2' | 'owe' | 'none'
  pmf: '0' | '1' | '2'
  /** Whether a passphrase is set. The passphrase is write-only and is never
   *  returned; omit key on an edit to preserve the existing value. */
  has_key: boolean
  key?: string
  roaming: { ft: boolean; ft_over_ds: boolean; kv: boolean; ft_with_psk2: boolean }
  hidden: boolean
  isolate: boolean
  max_assoc: number
  /** Lets devices join this network as a 4-address bridge rather than as a
   *  client. Off unless asked for: it changes what the access points accept
   *  from the air, which is a security posture and not a convenience. */
  allow_uplink: boolean
  enabled: boolean
}

export interface APGroup {
  id: number
  name: string
  device_ids: number[]
}

export interface SiteNetwork {
  id: number
  name: string
  vlan: number
  cidr: string
  zone: string
  /** Missing only when talking to a controller version from before DHCP became
   * editable; the UI supplies the historical defaults in that case. */
  dhcp?: {
    enabled: boolean
    start: number
    limit: number
    leasetime: string
    /** True only for an upgraded dhcp_json={} row. An explicit save clears it. */
    legacy_default?: boolean
  }
  enabled: boolean
}

/** Effective forwarding policy for one managed routed zone.
 *
 * `explicit: false` is the inherited legacy default, not a missing policy:
 * that zone may initiate to `wan` and nowhere else. An explicit empty list is
 * different and means the zone may initiate nowhere. */
export interface SiteZonePolicy {
  name: string
  forward_to: string[]
  explicit: boolean
}

export type PolicyKind = 'firewall_rule' | 'port_forward' | 'static_route'
export type PolicyOrigin = 'manual' | 'object_manager'

export interface FirewallRule {
  /** This release renders explicit firewall rules as OpenWrt family=ipv4.
   *  There is no selectable family field yet; IPv6 traffic is unaffected. */
  action: 'accept' | 'drop' | 'reject'
  source_zone: string
  /** Empty means traffic to the router itself. */
  destination_zone?: string
  protocols: Array<'all' | 'tcp' | 'udp' | 'icmp'>
  source_cidr?: string
  destination_cidr?: string
  source_port?: string
  destination_port?: string
  source_macs?: string[]
}

export interface PortForward {
  /** Port forwards are IPv4 DNAT in this release. */
  source_zone: 'wan'
  destination_zone: string
  protocols: Array<'tcp' | 'udp'>
  external_port: number
  destination_ip: string
  destination_port: number
  source_cidr?: string
}

export interface StaticRoute {
  /** Static routes are IPv4 routes in this release. */
  /** 0 means the device's WAN; a positive value names a managed network. */
  network_id: number
  target: string
  gateway: string
  metric: number
}

/** One persisted policy record. Drafts returned by Object Manager have id 0. */
export interface Policy {
  id: number
  order: number
  name: string
  kind: PolicyKind
  origin: PolicyOrigin
  enabled: boolean
  firewall?: FirewallRule
  port_forward?: PortForward
  static_route?: StaticRoute
}

export type PolicyRowKind =
  | 'zone_forward'
  | PolicyKind
  | 'client_block'
  | 'fixed_ip'

export interface PolicyRow {
  id: string
  record_id?: number
  origin: 'legacy_default' | 'zone_matrix' | PolicyOrigin | 'client'
  kind: PolicyRowKind
  name: string
  enabled: boolean
  order: number
  order_scope: 'zone_forwarding' | 'firewall' | 'network_route' | 'dhcp' | string
  /** Server-authored scope, including `address_families`. Explicit firewall,
   *  NAT and route rows report only `ipv4`; Client Block reports ipv4+ipv6. */
  effective_scope: Record<string, unknown>
  mutable: boolean
  renderable: boolean
  gated_reason?: string
  rule?: unknown
}

export interface PolicyCapability {
  kind: 'firewall' | 'nat' | 'route' | 'fixed_ip' | 'qos' | 'rate_limit' | 'application' | 'priority'
  available: boolean
  reason?: string
}

export interface PolicyMaster {
  rows: PolicyRow[]
  capabilities: PolicyCapability[]
}

export interface PolicyClient {
  mac: string
  group?: string
  blocked: boolean
  fixed_ip?: string
}

export interface PolicyObjectTarget {
  kind: 'device' | 'group' | 'network'
  /** Device MAC, exact group name, numeric network ID, or `wan`. */
  id: string
}

export interface PolicyObjectOutcome {
  /** `secure` compiles an IPv4-only firewall draft in this release. */
  kind: 'secure' | 'route' | 'qos' | 'application'
  destination_zone?: string
  target?: string
  gateway?: string
  metric?: number
  rate_kbps?: number
}

export interface ObjectCompileResult {
  drafts: Policy[]
  gates: Array<{
    object: PolicyObjectTarget
    outcome: PolicyObjectOutcome['kind']
    reason: string
  }>
  persisted: false
  applied: false
  note: string
}

/** One device's deviation from the site model. */
export interface SiteOverride {
  device_id: number
  wlan_id: number
  key: string
  value: string
  /** The sentence to show. Built server-side so a deviation reads the same
   *  everywhere it appears. */
  describe: string
}

/** An 802.11s mesh backhaul.
 *
 *  Not a WLAN with a flag: a mesh point is a different interface mode. It has a
 *  mesh ID rather than an SSID (it is not beaconed for clients), and exactly ONE
 *  band rather than a list — nodes peer only with nodes on the same band, so
 *  "one mesh" on two bands would be two disjoint backhauls. */
/** One BSS's part in a neighbour-distribution cycle. */
export interface NeighbourBSS {
  iface: string
  ssid: string
  bssid: string
  neighbours: number
  changed?: boolean
  failed?: string
}

export interface NeighbourDevice {
  device_id: number
  name: string
  error?: string
  /** Why this device took no part, when that is a standing property rather
   *  than a failure. Separate from error because an operator responds to the
   *  two differently, and a screen that renders both as red teaches people to
   *  ignore red. */
  skipped?: string
  updated: number
  unchanged: number
  bsses?: NeighbourBSS[]
}

/** What one 802.11k distribution cycle did.
 *
 *  An AP cannot learn its neighbours by itself — it knows its own BSS and
 *  nothing about the AP down the hall. This is the controller telling each one
 *  about the others, which is the whole reason 802.11k is worth enabling. */
export interface NeighbourResult {
  ssids: string[]
  updated: number
  unchanged: number
  devices: NeighbourDevice[]
  /** Explains an empty run. All zeroes with no sentence is indistinguishable
   *  from a broken feature. */
  note?: string
}

/** One 802.11s backhaul on one device.
 *
 *  `state` is a closed vocabulary decided once in the controller. Switch on it;
 *  never re-derive health from the other fields — a screen that decides for
 *  itself what a null peer count means is a second implementation of that
 *  logic, and the two drift. */
/** One device's wireless uplink: how it reaches the network when there is no
 *  cable to it.
 *
 *  Carries no credentials and has none to omit — it references a WLAN, so the
 *  SSID, passphrase and security mode live in one place. Two copies of a
 *  passphrase drift, and a bridge whose key stops matching fails the way a
 *  client with a stale password fails. */
export interface Uplink {
  id: number
  device_id: number
  wlan_id: number
  band: '2g' | '5g' | '6g'
  enabled: boolean
}

export interface MeshLink {
  device_id: number
  device_name: string
  mesh_id: number
  name: string
  iface?: string
  state: string
  tone: 'ok' | 'normal' | 'muted' | 'warning' | 'critical'
  /** Always present. A state with no sentence is a code nobody looks up. */
  reason: string
  /** null when peers were not counted — a real state, not an omission. Never
   *  an empty array: "none" and "not counted" are different answers. */
  peers: MeshPeer[] | null
  established: number | null
  /** Something to go and fix, as opposed to something the controller could not
   *  see. Sent rather than inferred: rendering the second as the first is the
   *  collapse the capability model exists to prevent. */
  actionable: boolean
}

export interface MeshPeer {
  mac: string
  plink?: string
  signal_dbm: number | null
  inactive_ms: number | null
}

export interface MeshHealthResult {
  links: MeshLink[]
  note?: string
}

export interface Mesh {
  id: number
  mesh_id: string
  network_id: number
  group_id: number
  band: '2g' | '5g' | '6g'
  /** The passphrase is write-only and never returned. has_key is what an edit
   *  screen needs: an open mesh is joinable by anyone in radio range. */
  has_key: boolean
  key?: string
  /** Write-only. Explicitly erase the stored key; blank key alone preserves it. */
  clear_key?: boolean
  enabled: boolean
}

export interface Site {
  name: string
  /** Seeds the mobility-domain derivation, so every AP computes the same
   *  802.11r domain. Shown because that is what makes roaming consistent. */
  uuid: string
  wlans: WLAN[]
  meshes: Mesh[]
  uplinks: Uplink[]
  groups: APGroup[]
  networks: SiteNetwork[]
  zones: SiteZonePolicy[]
  /** Unified effective policy rows. Optional only for mixed-version upgrades. */
  policies?: PolicyRow[]
  /** Backend/capability gates shown without implying an unavailable rule shipped. */
  policy_capabilities?: PolicyCapability[]
  problems: string[]
  /** Every per-device deviation, listed. The risk of overrides is not any one
   *  of them; it is a fleet drifting apart until nobody can say what is
   *  deployed. */
  overrides: SiteOverride[]
  /** The settings that may be overridden. Security, SSID and roaming are
   *  deliberately absent. */
  overridable: string[]
  override_note: string
}

export interface Change {
  action: 'create' | 'update' | 'remove'
  config: string
  section: string
  options?: string[]
  /** Set when only this one option is removed, not the whole section. */
  option?: string
  /** The change writes a passphrase. The value is deliberately not here. */
  touches_key?: boolean
}

/** A known flaw in a device's wireless driver that the pending config hits. */
export interface DriverDefect {
  /** The SSID that triggers it, or absent for a defect of the hardware itself
   *  that no configuration causes and none can avoid. */
  wlan?: string
  defect_id: string
  summary: string
  detail: string
  /** How well it is established. Wireless folklore is repeated far more often
   *  than it is verified, so "anecdotal" must never be shown with the same
   *  authority as "documented". */
  confidence: 'documented' | 'measured' | 'reported' | 'anecdotal'
  severity: 'radio-death' | 'silently-ignored' | 'degraded'
  mitigation?: string
  source?: string
}

export interface DevicePreview {
  device_id: number
  name: string
  role: string
  functions: DeviceFunction[]
  changes: Change[]
  /** A human owns something this change would touch. Nothing is applied to a
   *  blocked device — a partial apply around a conflict gives you half a WLAN. */
  blocked: boolean
  conflicts?: string[]
  /** Options this hardware cannot take. Absent, not failed. */
  omitted?: string[]
  /** Rendered and applied, and needing a human decision first — an unencrypted
   *  mesh, a wireless bridge that is a layer-2 loop if the device is also
   *  cabled. These used to sit in `omitted`, under a heading that called them
   *  not an error. */
  cautions?: string[]
  /** What could not be established, including sections left in place because
   *  nothing could be decided about them. The opposite of `omitted`: nothing
   *  was left out, and the reason is a gap in what the controller can see. */
  undetermined?: string[]
  /** Settings this device WILL accept and will not honour, or will break on,
   *  because its wireless driver is known to be broken in that specific way.
   *  Unlike `omitted`, these ARE applied — the controller does not silently
   *  rewrite a user's security settings — so this is the only warning. */
  driver_defects?: DriverDefect[]
  /** A section we own whose value on the device no longer matches what we
   *  applied. Surfaced, never silently corrected. */
  drift?: string[]
  /** Per-device overrides in force here, shown at the moment someone is
   *  deciding what to push. */
  deviations?: string[]
  /** A recent capability change, offered as a PROBABLE cause when this device
   *  omitted or blocked something. The server knows a WLAN was omitted and
   *  knows a radio disappeared; it does not know they are the same fact, so
   *  the UI must not assert the link. Absent when there is nothing to
   *  explain. */
  capability_cause?: { at: number; changes: string[] }
  /** The change edits network or firewall config — the path the controller
   *  reaches this device through. Applying needs an explicit acknowledgment. */
  touches_traversal?: boolean
  /** This device could not be planned. The others are still reported. */
  error?: string
}

export interface PreviewResult {
  site_name: string
  devices: DevicePreview[]
  /** Opaque server binding for the complete desired state and full-fleet plans. */
  preview_token: string
  site_errors?: string[]
}

export interface DeviceApply {
  device_id: number
  name: string
  /** applied | reverted | unknown | error. "unknown" needs a human: the
   *  confirm never landed and what the device did could not be established. */
  outcome: string
  /** Device-side truth, retained when controller bookkeeping fails later. */
  router_outcome?: string
  reason?: string
  changes?: number
}

export interface ApplyResult {
  operation_id?: string
  devices: DeviceApply[]
  aborted: boolean
  aborted_after?: string
}

export type ApplyOperationState =
  | 'queued'
  | 'running'
  | 'completed'
  | 'failed'
  | 'unknown'

export interface ApplyOperation {
  operation_id: string
  state: ApplyOperationState
  created_at: number
  started_at?: number
  finished_at?: number
  result?: ApplyResult
  error?: string
  write_state?: 'none' | 'possible'
  devices: ApplyOperationDevice[]
}

export interface ApplyOperationDevice {
  ordinal: number
  device_id: number
  device_mac: string
  device_name: string
  state: 'queued' | 'applying' | 'completed' | 'failed' | 'unknown' | 'skipped'
  write_state: 'none' | 'possible'
  router_outcome?: string
  outcome?: string
  changes: number
  reason?: string
  started_at?: number
  finished_at?: number
}

/** What the controller costs one device (DEVICE-BUDGET §7). */
export interface Overhead {
  device_id: number
  tier: string
  interval_seconds: number
  requests: number
  polls_per_minute: number
  bytes_out: number
  polls: number
  failed_polls: number
  since: number
  requests_per_minute: number
  /** Requests that were not scheduled polls, including session setup and
   *  explicit actions such as discovery, capability probes, and RF scans. */
  non_poll_requests: number
  quiesced: boolean
  /** Device CPU one poll of the current tier costs. Absent when this device's
   *  class has never been measured — see cpu_basis. */
  cpu_ms_per_poll?: number
  /** That cost at the rate this device is actually polled. Absent likewise. */
  cpu_percent_of_core?: number
  /** Always present: where the figure came from, or why there is none. A
   *  derived number that does not announce itself gets read as a measurement. */
  cpu_basis: string
}

/** The Management Overhead payload (DEVICE-BUDGET §7). */
export interface OverheadReport {
  overhead: Overhead
  /** Packages added by explicitly authorized controller capabilities. */
  packages: string[]
  packages_note: string
  /** 0 means the controller default. */
  poll_interval_s: number
  poll_interval_note: string
}

export type AccountRole = 'owner' | 'admin' | 'operator' | 'viewer'

export interface Account {
  id: number
  username: string
  role: AccountRole
  role_label: string
  enabled: boolean
  created_at: number
  last_login_at: number | null
  active_session_count: number
}

export interface AccountSession {
  id: string
  current: boolean
  created_at: number
  last_seen_at: number
  expires_at: number
  peer_address: string
}

export interface AccountRoleOption {
  value: AccountRole
  label: string
  description: string
}

export interface SessionInfo {
  admin_id: number
  username: string
  role: AccountRole
  role_label: string
  csrf: string
  reauthenticated_until: number | null
}

export interface AccountsResponse {
  accounts: Account[]
  roles: AccountRoleOption[]
}

export interface AccountMutationResponse {
  account: Account
  revoked_sessions?: number
  signed_out?: boolean
}

export interface SessionMutationResponse {
  ok: boolean
  signed_out?: boolean
  revoked?: number
  revoked_sessions?: number
}

export type DiagnosticJobState =
  | 'queued'
  | 'collecting'
  | 'generating'
  | 'completed'
  | 'failed'
  | 'cancelled'

export interface DiagnosticJob {
  id: string
  state: DiagnosticJobState
  phase: string
  progress_percent: number
  created_at: number
  started_at?: number
  finished_at?: number
  expires_at?: number
  size_bytes?: number
  error?: string
}

export interface DiagnosticDescriptor {
  mode: 'stored'
  router_management_calls: false
  router_changes: false
  sections: { id: string; label: string; description: string }[]
  excluded_secret_classes: string[]
  limits: {
    devices: number
    sources: number
    events: number
    controller_log_input_bytes: number
    controller_log_output_bytes: number
    archive_bytes: number
    history: number
    retention_seconds: number
    collection_timeout_seconds: number
  }
  controller_log: { available: boolean; gaps: string[] }
  jobs: DiagnosticJob[]
}

export type BackupJobState =
  | 'queued'
  | 'snapshotting'
  | 'encrypting'
  | 'completed'
  | 'failed'
  | 'cancelled'

export interface BackupJob {
  id: string
  state: BackupJobState
  phase: string
  progress_percent: number
  created_at: number
  started_at?: number
  finished_at?: number
  expires_at?: number
  size_bytes?: number
  sha256?: string
  schema_version?: number
  controller_version?: string
  error?: string
}

export interface BackupDescriptor {
  descriptor: {
    plan_id: string
    format: 'oonfeewrt-portable-backup'
    format_version: 1
    file_extension: '.oowrtbak'
    snapshot: string
    encryption: string
    includes: string[]
    excludes: string[]
  }
  disclosure: {
    router_management_calls: false
    router_changes: false
    automatic_router_apply: false
    separate_export_passphrase: true
    export_passphrase_recoverable: false
    summary: string
  }
  limits: {
    history: number
    retention_seconds: number
    export_timeout_seconds: number
    min_export_passphrase_characters: number
    max_export_passphrase_bytes: number
  }
  jobs: BackupJob[]
}

export type RestorePreviewState = 'queued' | 'running' | 'completed' | 'failed' | 'cancelled'

export interface RestoreUpload {
  id: string
  created_at: number
  expires_at: number
  size_bytes: number
  sha256: string
}

export interface RestoreManifest {
  format: string
  format_version: number
  created_at: string
  controller_version: string
  schema_version: number
  database_size_bytes: number
}

export interface RestoreCounts {
  devices: number
  credentials: number
  owned_sections: number
  wlans: number
  meshes: number
}

export interface RestorePreview {
  id: string
  upload_id: string
  state: RestorePreviewState
  phase: string
  progress_percent: number
  created_at: number
  started_at?: number
  finished_at?: number
  expires_at?: number
  plan_id?: string
  manifest?: RestoreManifest
  source_schema?: number
  target_schema?: number
  counts?: RestoreCounts
  error_code?: string
  error?: string
}

export interface RestoreDescriptor {
  descriptor: {
    format: 'oonfeewrt-portable-backup'
    format_version: 1
    upload_content_type: 'application/vnd.oonfeewrt.backup'
    confirmation_contract: 'controller-restore-confirm-v1'
    typed_confirmation: 'RESTORE CONTROLLER'
    confirmation_requires: string[]
  }
  disclosure: {
    router_management_calls: false
    router_changes: false
    live_controller_changes: false
    automatic_router_apply: false
    summary: string
  }
  limits: {
    max_upload_bytes: number
    max_database_bytes: number
    history: number
    retention_seconds: number
    preview_timeout_seconds: number
    confirmation_timeout_seconds: number
    min_export_passphrase_characters: number
    max_export_passphrase_bytes: number
  }
  uploads: RestoreUpload[]
  previews: RestorePreview[]
}

export interface RestoreConfirmation {
  plan_id: string
  export_passphrase: string
  destination_runtime_passphrase: string
  typed_confirmation: 'RESTORE CONTROLLER'
  acknowledge_restart: true
  acknowledge_session_revocation: true
  acknowledge_router_writes_suppressed: true
  acknowledge_no_automatic_router_apply: true
}

export interface RestoreIntent {
  id: string
  state: 'accepted'
  accepted_at: number
}

export type RestoreSuppression =
  | { active: false }
  | { active: true; restore_id: string; created_at: string; reason: string }

export const api = {
  setupState: () => get<{ needs_setup: boolean }>('/setup'),
  setup: (username: string, password: string) =>
    post<SessionInfo>('/setup', { username, password }),
  login: (username: string, password: string) =>
    post<SessionInfo>('/login', { username, password }),
  logout: () => post<{ ok: boolean }>('/logout'),
  session: () => get<SessionInfo>('/session'),
  changePassword: (current_password: string, new_password: string) =>
    post<{ ok: boolean; message: string }>('/session/password', {
      current_password,
      new_password,
    }),
  account: () => get<{ account: Account }>('/account'),
  reauthenticate: (password: string) =>
    post<{ reauthenticated_until: number }>('/session/reauth', { password }),
  accountSessions: () => get<{ sessions: AccountSession[] }>('/account/sessions'),
  revokeAccountSession: (sessionID: string) =>
    del<SessionMutationResponse>(`/account/sessions/${encodeURIComponent(sessionID)}`),
  accounts: () => get<AccountsResponse>('/accounts'),
  createAccount: (username: string, password: string, role: AccountRole) =>
    post<AccountMutationResponse>('/accounts', { username, password, role }),
  setAccountRole: (id: number, role: AccountRole) =>
    patch<AccountMutationResponse>(`/accounts/${id}/role`, { role }),
  setAccountEnabled: (id: number, enabled: boolean) =>
    patch<AccountMutationResponse>(`/accounts/${id}/enabled`, { enabled }),
  deleteAccount: (id: number) =>
    del<SessionMutationResponse>(`/accounts/${id}`),
  resetAccountPassword: (id: number, password: string) =>
    post<SessionMutationResponse>(`/accounts/${id}/password`, { new_password: password }),
  managedAccountSessions: (id: number) =>
    get<{ sessions: AccountSession[] }>(`/accounts/${id}/sessions`),
  revokeManagedAccountSession: (id: number, sessionID: string) =>
    del<SessionMutationResponse>(`/accounts/${id}/sessions/${encodeURIComponent(sessionID)}`),
  revokeManagedAccountSessions: (id: number) =>
    del<SessionMutationResponse>(`/accounts/${id}/sessions`),
  diagnostics: () => get<DiagnosticDescriptor>('/diagnostics'),
  startDiagnostics: () => post<{ job: DiagnosticJob }>('/diagnostics'),
  diagnostic: (id: string) =>
    get<{ job: DiagnosticJob }>(`/diagnostics/${encodeURIComponent(id)}`),
  cancelDiagnostics: (id: string) =>
    post<{ job: DiagnosticJob }>(`/diagnostics/${encodeURIComponent(id)}/cancel`),
  downloadDiagnostics: (id: string, maxBytes: number, expectedBytes: number) =>
    download(`/diagnostics/${encodeURIComponent(id)}/download`, maxBytes, expectedBytes),
  backups: () => get<BackupDescriptor>('/backups'),
  backup: (id: string) =>
    get<{ job: BackupJob }>(`/backups/${encodeURIComponent(id)}`),
  startBackup: (
    planID: string,
    acknowledgeSensitiveContent: boolean,
    exportPassphrase: string,
    confirmExportPassphrase: string,
  ) => post<{ job: BackupJob }>('/backups', {
    plan_id: planID,
    acknowledge_sensitive_content: acknowledgeSensitiveContent,
    export_passphrase: exportPassphrase,
    confirm_export_passphrase: confirmExportPassphrase,
  }),
  cancelBackup: (id: string) =>
    post<{ job: BackupJob }>(`/backups/${encodeURIComponent(id)}/cancel`),
  backupDownloadURL: (id: string) => `/api/v1/backups/${encodeURIComponent(id)}/download`,
  restores: () => get<RestoreDescriptor>('/restores'),
  uploadRestore: (file: Blob) =>
    postRaw<{ upload: RestoreUpload }>('/restores/uploads', file, 'application/vnd.oonfeewrt.backup'),
  startRestorePreview: (uploadID: string, exportPassphrase: string) =>
    post<{ preview: RestorePreview }>('/restores/previews', {
      upload_id: uploadID,
      export_passphrase: exportPassphrase,
    }),
  restorePreview: (id: string) =>
    get<{ preview: RestorePreview }>(`/restores/previews/${encodeURIComponent(id)}`),
  cancelRestorePreview: (id: string) =>
    post<{ preview: RestorePreview }>(`/restores/previews/${encodeURIComponent(id)}/cancel`),
  confirmRestore: (id: string, confirmation: RestoreConfirmation) =>
    post<{ intent: RestoreIntent }>(`/restores/previews/${encodeURIComponent(id)}/confirm`, confirmation),
  restoreSuppression: () =>
    get<{ suppression: RestoreSuppression }>('/restores/suppression'),
  resumeRouterWrites: (restoreID: string, typedConfirmation: 'RESUME ROUTER WRITES') =>
    post<{ suppression: RestoreSuppression }>('/restores/suppression/resume', {
      restore_id: restoreID,
      typed_confirmation: typedConfirmation,
    }),

  dashboard: () => get<Dashboard>('/dashboard'),
  speedTests: (limit = 3) =>
    get<SpeedTestCollection>(`/speedtests?limit=${encodeURIComponent(limit)}`),
  speedTest: (id: string) => get<SpeedTestJob>(`/speedtests/${encodeURIComponent(id)}`),
  startSpeedTest: (planID: string, acknowledgeDataUse: boolean) =>
    post<SpeedTestJob>('/speedtests', {
      acknowledge_data_use: acknowledgeDataUse,
      plan_id: planID,
    }),
  cancelSpeedTest: (id: string) =>
    post<SpeedTestJob>(`/speedtests/${encodeURIComponent(id)}/cancel`, {}),
  devices: () => get<{ devices: Device[] }>('/devices'),
  /** Records a decision ABOUT a foreign section. Writes nothing to any device.
   *  An empty note clears it. */
  noteForeign: (deviceID: number, section: string, ssid: string, note: string) =>
    post<{ recorded?: boolean; cleared?: boolean }>(
      `/devices/${deviceID}/foreign/${encodeURIComponent(section)}/note`,
      { ssid, note },
    ),
  device: (id: number) => get<DeviceDetail>(`/devices/${id}`),
  overhead: (id: number) => get<OverheadReport>(`/devices/${id}/overhead`),
  setPollInterval: (id: number, seconds: number) =>
    post<{ poll_interval_s: number }>(`/devices/${id}/poll-interval`, { seconds }),
  /** Rename a device. An empty name restores the default the device reports
   *  for itself — its board model — which is what adoption chose. */
  renameDevice: (id: number, name: string) =>
    post<{ name: string }>(`/devices/${id}/name`, { name }),
  deviceSeries: (id: number) =>
    get<{ series: Record<string, string[]> }>(`/devices/${id}/series`),
  reprobe: (id: number) => post<ReprobeResult>(`/devices/${id}/reprobe`, {}),
  refreshACL: (id: number, credential: {
    username: string
    password: string
    private_key?: string
    acknowledge_router_changes: true
  }) => post<RefreshACLResult>(`/devices/${id}/refresh-acl`, credential),
  lldpCapability: (id: number) =>
    get<LLDPCapabilityResult>(`/devices/${id}/capabilities/lldp`),
  changeLLDPCapability: (id: number, request: {
    action: 'diagnose' | 'plan_install' | 'install' | 'plan_configure' | 'configure' | 'plan_remove' | 'remove'
    username: string
    password: string
    private_key?: string
    plan_hash?: string
    acknowledge_package_index_refresh?: boolean
    acknowledge_read_only_diagnostics?: boolean
    acknowledge_router_changes?: boolean
  }) => post<LLDPCapabilityResult>(`/devices/${id}/capabilities/lldp`, request),
  distributeNeighbours: () => post<NeighbourResult>('/roaming/neighbours', {}),
  /** The most recent distribution, WITHOUT running one. `ran: false` means no
   *  cycle has completed since the controller started — not that nothing
   *  needed doing. */
  lastNeighbours: () =>
    get<{
      ran: boolean
      at?: number
      error?: string
      result?: NeighbourResult
      /** Devices whose distribution failed while the CYCLE itself succeeded.
       *  A run can complete with half the fleet unreachable, and the top-level
       *  error is empty in that case. */
      devices_failed?: number
    }>('/roaming/neighbours'),
  meshHealth: () => get<MeshHealthResult>('/site/mesh-health'),
  saveUplink: (u: Partial<Uplink> & { id?: number }) =>
    post<{ uplink: Uplink; note: string }>(
      u.id ? `/site/uplinks/${u.id}` : '/site/uplinks', u),
  deleteUplink: (id: number) =>
    del<{ deleted: number; note: string }>(`/site/uplinks/${id}`),
  focus: (id: number, seconds = 30) =>
    post<{ focused_for_seconds: number }>(`/devices/${id}/focus?seconds=${seconds}`),
  clients: (q: {
    limit?: number
    offset?: number
    presence?: string
    connection?: string
    scope?: string
    all?: boolean
  } = {}) => {
    const p = new URLSearchParams()
    if (q.limit != null) p.set('limit', String(q.limit))
    if (q.offset) p.set('offset', String(q.offset))
    if (q.presence) p.set('presence', q.presence)
    if (q.connection) p.set('connection', q.connection)
    if (q.scope) p.set('scope', q.scope)
    if (q.all) p.set('all', '1')
    const qs = p.toString()
    return get<ClientPage>(`/clients${qs ? `?${qs}` : ''}`)
  },
  clientObservability: (mac: string, from: number, to: number) => {
    const q = new URLSearchParams({
      from: String(Math.trunc(from)),
      to: String(Math.trunc(to)),
    })
    return get<ClientObservability>(
      `/clients/${encodeURIComponent(mac)}/observability?${q}`,
    )
  },
  adopt: (req: {
    host: string
    name?: string
    username: string
    /** Still required for the ubus sign-in even when SSH uses a key. */
    password: string
    /** Optional one-time SSH bootstrap credential; never persisted. */
    private_key?: string
    scheme?: 'http' | 'https'
    port?: number
    /** Additive capability selection. `role` is sent too so a mixed-version
     *  controller still has a deterministic legacy fallback. */
    functions?: DeviceFunction[]
    role?: DeviceRole
    acknowledge_router_changes: true
  }) => post<AdoptResult>('/devices/adopt', req),
  inspectDevice: (req: {
    host: string
    username: string
    password: string
    scheme?: 'http' | 'https'
    port?: number
  }) => post<InspectResult>('/devices/inspect', req),
  unadopt: (
    id: number,
    req?: { username?: string; password?: string; private_key?: string; force?: boolean },
  ) => post<UnadoptResult>(`/devices/${id}/unadopt`, req ?? {}),
  site: () => get<Site>('/site'),
  setSiteName: (name: string) => post<{ name: string }>('/site/name', { name }),
  wlan: (id: number) => get<WLAN>(`/site/wlans/${id}`),
  saveWLAN: (w: Partial<WLAN> & { id?: number }) =>
    post<{ wlan: WLAN; problems: string[] }>(
      w.id ? `/site/wlans/${w.id}` : '/site/wlans', w),
  deleteWLAN: (id: number) => del<{ deleted: number; note: string }>(`/site/wlans/${id}`),
  mesh: (id: number) => get<Mesh>(`/site/meshes/${id}`),
  saveMesh: (m: Partial<Mesh> & { id?: number }) =>
    post<{ mesh: Mesh; problems: string[] }>(
      m.id ? `/site/meshes/${m.id}` : '/site/meshes', m),
  deleteMesh: (id: number) => del<{ deleted: number; note: string }>(`/site/meshes/${id}`),
  saveGroup: (g: Partial<APGroup> & { id?: number }) =>
    post<APGroup>(g.id ? `/site/groups/${g.id}` : '/site/groups', g),
  deleteGroup: (id: number) => del<{ deleted: number }>(`/site/groups/${id}`),
  saveNetwork: (n: Partial<SiteNetwork> & { id?: number }) =>
    post<SiteNetwork>(n.id ? `/site/networks/${n.id}` : '/site/networks', n),
  deleteNetwork: (id: number) => del<{ deleted: number }>(`/site/networks/${id}`),
  saveZonePolicy: (name: string, forwardTo: string[]) =>
    post<SiteZonePolicy>(`/site/zones/${encodeURIComponent(name)}`, {
      forward_to: forwardTo,
    }),
  resetZonePolicy: (name: string) =>
    del<SiteZonePolicy>(`/site/zones/${encodeURIComponent(name)}`),
  policies: () => get<PolicyMaster>('/site/policies'),
  savePolicy: (policy: Omit<Policy, 'id'> & { id?: number }) => {
    const { id, ...body } = policy
    return post<Policy>(id ? `/site/policies/${id}` : '/site/policies', body)
  },
  deletePolicy: (id: number) =>
    del<{ deleted: number; note: string }>(`/site/policies/${id}`),
  saveClientPolicy: (
    mac: string,
    changes: { blocked?: boolean; fixed_ip?: string; group?: string },
  ) => post<{ client: PolicyClient; note: string }>(
    `/clients/${encodeURIComponent(mac)}/policy`, changes,
  ),
  compilePolicyObjects: (
    objects: PolicyObjectTarget[],
    outcomes: PolicyObjectOutcome[],
  ) => post<ObjectCompileResult>('/site/object-manager/compile', { objects, outcomes }),
  setOverride: (deviceID: number, wlan_id: number, key: string, value: string) =>
    post<{ note: string }>(`/site/devices/${deviceID}/override`, { wlan_id, key, value }),
  preview: () => get<PreviewResult>('/site/preview'),
  applySite: (opts: {
    operation_id: string
    preview_token: string
    device_ids?: number[]
    acknowledge_traversal?: boolean
    acknowledge_driver_risk?: boolean
    acknowledge_cautions?: boolean
    acknowledge_partial_fleet?: boolean
  }) =>
    post<ApplyResult>('/site/apply', opts),
  applyOperation: (operationID: string) =>
    get<ApplyOperation>(`/site/apply/${encodeURIComponent(operationID)}`),

  topology: (at?: number) =>
    get<TopologySnapshot>(`/topology${at == null ? '' : `?at=${Math.trunc(at)}`}`),
  topologyHistory: (from: number, to: number) => {
    const q = new URLSearchParams({
      from: String(Math.trunc(from)),
      to: String(Math.trunc(to)),
    })
    return get<TopologySnapshot>(`/topology/history?${q}`)
  },

  radios: () => get<RadiosResponse>('/radios'),
  scanRadio: (deviceID: number, radioKey: string, acknowledgeDisruption: boolean) =>
    post<{ scan: RadioScan; observations: RadioScanBSS[] }>(
      `/devices/${deviceID}/radios/${encodeURIComponent(radioKey)}/scan`,
      { acknowledge_disruption: acknowledgeDisruption },
    ),

  scanPlan: () => get<ScanPlan>('/discovery'),
  scan: (req?: { networks?: string[]; https?: boolean }) =>
    post<ScanResult>('/discovery/scan', req ?? {}),
  events: (opts: {
    limit?: number
    offset?: number
    scope?: 'general' | 'audit'
    before?: EventCursor | null
    category?: string
    severity?: string
  } = {}) => {
    const q = new URLSearchParams()
    q.set('limit', String(opts.limit ?? 100))
    if (opts.offset) q.set('offset', String(opts.offset))
    if (opts.scope) q.set('scope', opts.scope)
    if (opts.before) {
      q.set('before_ts', String(opts.before.ts))
      q.set('before_id', String(opts.before.id))
    }
    if (opts.category) q.set('category', opts.category)
    if (opts.severity) q.set('severity', opts.severity)
    return get<EventPage>(`/events?${q}`)
  },
  eventDetail: (id: number) => get<EventRow>(`/events/${id}`),
  stats: (kind: string, deviceID: number, key: string, from: number, to: number) =>
    get<Series>(
      `/stats/${kind}?device_id=${deviceID}&key=${encodeURIComponent(key)}` +
        `&from=${from}&to=${to}`,
    ),
}
