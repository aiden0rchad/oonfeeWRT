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
  }
}

/** Fires when the server says we are not signed in. */
export const onUnauthorized = new Set<() => void>()

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
  if (init.body !== undefined) headers.set('Content-Type', 'application/json')
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

  if (resp.status === 401) {
    onUnauthorized.forEach((fn) => fn())
    throw new ApiError(401, 'not signed in')
  }
  const text = await resp.text()
  const body = text ? JSON.parse(text) : {}
  if (!resp.ok) {
    throw new ApiError(resp.status,
      body.error ?? `request failed (${resp.status})`, body)
  }
  return body as T
}

const get = <T>(path: string) => request<T>(path)
const post = <T>(path: string, body?: unknown) =>
  request<T>(path, { method: 'POST', body: body ? JSON.stringify(body) : undefined })
const del = <T>(path: string) => request<T>(path, { method: 'DELETE' })

// ---- types, mirroring the Go response structs ----

export interface Device {
  id: number
  mac: string
  name: string
  host: string
  role: string
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

export interface DeviceDetail extends Device {
  capabilities: Registry | null
  interfaces: string[]
  radios: string[]
  stations: string[]
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
  first_seen: number | null
  last_seen: number | null
  blocked: boolean
  /** "wireless" only when a focused poll saw it; never "wired" by inference. */
  connection: 'wireless' | 'unknown'
  online: boolean
  /** Absent, not zero, when no focused poll has covered this client. */
  signal?: number
  tx_retry_pct?: number
  device_id?: number
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
  TS: number
  DeviceID: number | null
  Category: string
  Severity: string
  Event: string
  Detail: unknown
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
  offset: number
  facets: { category: Facet[]; severity: Facet[] }
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
  /** Stations associated to the radios. null when any AP's count could not be
   *  read — see wireless_clients_unknown_on. */
  wireless_clients: number | null
  wireless_clients_unknown_on?: string[]
  /** Hosts on THIS network — a different question from wireless_clients, and
   *  scoped to `local`: a gateway's neighbour tables also cover its uplink.
   *  upstream_devices and unscoped_devices are the excluded remainder, so the
   *  headline can say what it left out. */
  known_devices: number
  active_devices: number
  upstream_devices: number
  unscoped_devices: number
  focused_devices: number
  quiesced_devices: number
  recent_events: EventRow[] | null
  series_count: number
}

/** What a device is for. A closed vocabulary — the API refuses anything else
 *  rather than storing it, because the role decides what gets sent to the
 *  device and a typo used to mean "silently an access point". */
export type DeviceRole = 'gateway' | 'ap' | 'switch'

export interface AdoptResult {
  device_id: number
  mac: string
  name: string
  model: string
  class: string
  firmware: string
  cert_fp?: string
  features?: string[]
  /** Checks that were REFUSED, not features the hardware lacks. */
  unobservable?: string[]
  quirks?: string[]
  notes?: string[]
  /** Facts about the DEVICE worth knowing — not controller problems. */
  warnings?: string[]
}

export interface UnadoptResult {
  removed_from_inventory: boolean
  reverted_sections: number
  login_removed: boolean
  acl_removed: boolean
  footprint_remains: boolean
  residue?: string[]
  errors?: string[]
  needs_operator_credential: boolean
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
    gateway: boolean
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
  /** Whether a passphrase is set. The passphrase itself never rides along in a
   *  list — fetch one WLAN with reveal to see it. */
  has_key: boolean
  key?: string
  roaming: { ft: boolean; ft_over_ds: boolean; kv: boolean; ft_with_psk2: boolean }
  hidden: boolean
  isolate: boolean
  max_assoc: number
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
  enabled: boolean
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

export interface Site {
  name: string
  /** Seeds the mobility-domain derivation, so every AP computes the same
   *  802.11r domain. Shown because that is what makes roaming consistent. */
  uuid: string
  wlans: WLAN[]
  groups: APGroup[]
  networks: SiteNetwork[]
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
  /** The change writes a passphrase. The value is deliberately not here. */
  touches_key?: boolean
}

export interface DevicePreview {
  device_id: number
  name: string
  role: string
  changes: Change[]
  /** A human owns something this change would touch. Nothing is applied to a
   *  blocked device — a partial apply around a conflict gives you half a WLAN. */
  blocked: boolean
  conflicts?: string[]
  /** Options this hardware cannot take. Absent, not failed. */
  omitted?: string[]
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
  site_errors?: string[]
}

export interface DeviceApply {
  device_id: number
  name: string
  /** applied | reverted | unknown | error. "unknown" needs a human: the
   *  confirm never landed and what the device did could not be established. */
  outcome: string
  reason?: string
  changes?: number
}

export interface ApplyResult {
  devices: DeviceApply[]
  aborted: boolean
  aborted_after?: string
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
  /** Requests that were not polls: session logins, and anything that escaped
   *  the batch. The second is a defect, not a rate. */
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
  /** Packages the controller installed. Always empty today, and reported
   *  rather than omitted so the "we install nothing" claim is checkable. */
  packages: string[]
  packages_note: string
  /** 0 means the controller default. */
  poll_interval_s: number
  poll_interval_note: string
}

export interface SessionInfo {
  username: string
  csrf: string
}

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

  dashboard: () => get<Dashboard>('/dashboard'),
  devices: () => get<{ devices: Device[] }>('/devices'),
  device: (id: number) => get<DeviceDetail>(`/devices/${id}`),
  overhead: (id: number) => get<OverheadReport>(`/devices/${id}/overhead`),
  setPollInterval: (id: number, seconds: number) =>
    post<{ poll_interval_s: number }>(`/devices/${id}/poll-interval`, { seconds }),
  deviceSeries: (id: number) =>
    get<{ series: Record<string, string[]> }>(`/devices/${id}/series`),
  reprobe: (id: number) => post<ReprobeResult>(`/devices/${id}/reprobe`, {}),
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
  adopt: (req: {
    host: string
    name?: string
    username: string
    password: string
    scheme?: 'http' | 'https'
    port?: number
    role?: DeviceRole
  }) => post<AdoptResult>('/devices/adopt', req),
  unadopt: (id: number, req?: { username?: string; password?: string; force?: boolean }) =>
    post<UnadoptResult>(`/devices/${id}/unadopt`, req ?? {}),
  site: () => get<Site>('/site'),
  setSiteName: (name: string) => post<{ name: string }>('/site/name', { name }),
  wlan: (id: number, reveal = false) =>
    get<WLAN>(`/site/wlans/${id}${reveal ? '?reveal=1' : ''}`),
  saveWLAN: (w: Partial<WLAN> & { id?: number }) =>
    post<{ wlan: WLAN; problems: string[] }>(
      w.id ? `/site/wlans/${w.id}` : '/site/wlans', w),
  deleteWLAN: (id: number) => del<{ deleted: number; note: string }>(`/site/wlans/${id}`),
  saveGroup: (g: Partial<APGroup> & { id?: number }) =>
    post<APGroup>(g.id ? `/site/groups/${g.id}` : '/site/groups', g),
  deleteGroup: (id: number) => del<{ deleted: number }>(`/site/groups/${id}`),
  saveNetwork: (n: Partial<SiteNetwork> & { id?: number }) =>
    post<SiteNetwork>(n.id ? `/site/networks/${n.id}` : '/site/networks', n),
  deleteNetwork: (id: number) => del<{ deleted: number }>(`/site/networks/${id}`),
  setOverride: (deviceID: number, wlan_id: number, key: string, value: string) =>
    post<{ note: string }>(`/site/devices/${deviceID}/override`, { wlan_id, key, value }),
  preview: () => get<PreviewResult>('/site/preview'),
  applySite: (opts: { device_ids?: number[]; acknowledge_traversal?: boolean } = {}) =>
    post<ApplyResult>('/site/apply', opts),

  scanPlan: () => get<ScanPlan>('/discovery'),
  scan: (req?: { networks?: string[]; https?: boolean }) =>
    post<ScanResult>('/discovery/scan', req ?? {}),
  events: (opts: {
    limit?: number
    offset?: number
    category?: string
    severity?: string
  } = {}) => {
    const q = new URLSearchParams()
    q.set('limit', String(opts.limit ?? 100))
    if (opts.offset) q.set('offset', String(opts.offset))
    if (opts.category) q.set('category', opts.category)
    if (opts.severity) q.set('severity', opts.severity)
    return get<EventPage>(`/events?${q}`)
  },
  stats: (kind: string, deviceID: number, key: string, from: number, to: number) =>
    get<Series>(
      `/stats/${kind}?device_id=${deviceID}&key=${encodeURIComponent(key)}` +
        `&from=${from}&to=${to}`,
    ),
}
