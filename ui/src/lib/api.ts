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
  /** Everything seen on the LAN. A different question from wireless_clients. */
  known_devices: number
  active_devices: number
  focused_devices: number
  quiesced_devices: number
  recent_events: EventRow[] | null
  series_count: number
}

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
  overhead: (id: number) => get<Overhead>(`/devices/${id}/overhead`),
  deviceSeries: (id: number) =>
    get<{ series: Record<string, string[]> }>(`/devices/${id}/series`),
  focus: (id: number, seconds = 30) =>
    post<{ focused_for_seconds: number }>(`/devices/${id}/focus?seconds=${seconds}`),
  clients: () =>
    get<{
      clients: Client[]
      /** Counts per scope, from the server, so the rail does not depend on
       *  which rows the page happened to receive. */
      scopes: Record<string, number>
      note: string
      scope_note: string
    }>('/clients'),
  adopt: (req: {
    host: string
    name?: string
    username: string
    password: string
    scheme?: 'http' | 'https'
    port?: number
    role?: string
  }) => post<AdoptResult>('/devices/adopt', req),
  unadopt: (id: number, req?: { username?: string; password?: string; force?: boolean }) =>
    post<UnadoptResult>(`/devices/${id}/unadopt`, req ?? {}),
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
