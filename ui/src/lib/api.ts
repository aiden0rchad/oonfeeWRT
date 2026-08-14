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
  constructor(status: number, message: string) {
    super(message)
    this.status = status
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
    throw new ApiError(resp.status, body.error ?? `request failed (${resp.status})`)
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
  deviceSeries: (id: number) =>
    get<{ series: Record<string, string[]> }>(`/devices/${id}/series`),
  focus: (id: number, seconds = 30) =>
    post<{ focused_for_seconds: number }>(`/devices/${id}/focus?seconds=${seconds}`),
  clients: () => get<{ clients: Client[]; note: string }>('/clients'),
  events: (limit = 200) => get<{ events: EventRow[] | null }>(`/events?limit=${limit}`),
  stats: (kind: string, deviceID: number, key: string, from: number, to: number) =>
    get<Series>(
      `/stats/${kind}?device_id=${deviceID}&key=${encodeURIComponent(key)}` +
        `&from=${from}&to=${to}`,
    ),
}
