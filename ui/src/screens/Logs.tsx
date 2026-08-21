import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { Device, EventCursor, EventPage, EventRow } from '../lib/api'
import { Banner, Button, Card, DataGrid, FilterRail, useColumnPrefs } from '../components/ui'
import type { Column } from '../components/ui'

type EventScope = 'general' | 'audit'

/**
 * The event log.
 *
 * Fetches its own page rather than being handed one. Filtering and paging are
 * server-side, so a parent that pre-fetched a fixed window could only ever hand
 * this screen the wrong rows: filtering that window client-side selects from
 * the newest N events overall instead of the newest N matching, which shows an
 * empty "errors" view on a controller that has plenty of them.
 *
 * The filter counts come from an aggregate over the whole table (UI-SPEC §5).
 * This screen previously computed them from the array it was given and carried
 * a comment claiming they covered "the whole result set, never the visible
 * page" — while the endpoint returned at most 300 of however many rows exist.
 * The comment asserted precisely the property it did not have.
 */
export function Logs() {
  const [scope, setScope] = useState<EventScope>('general')
  const [category, setCategory] = useState('')
  const [severity, setSeverity] = useState('')
  const [limit, setLimit] = useState(100)
  const [before, setBefore] = useState<EventCursor | null>(null)
  const [history, setHistory] = useState<(EventCursor | null)[]>([])
  const [loaded, setLoaded] = useState<{
    query: string
    page: EventPage
  } | null>(null)
  const [failure, setFailure] = useState<{
    query: string
    message: string
  } | null>(null)
  const requestGeneration = useRef(0)
  const inFlight = useRef<{ query: string; generation: number } | null>(null)
  const detailGeneration = useRef(0)
  const detailTrigger = useRef<HTMLElement | null>(null)
  const [detail, setDetail] = useState<EventRow | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detailError, setDetailError] = useState('')
  const [colPrefs, setColPrefs] = useColumnPrefs('logs')
  // Only to turn a device id into a name. Fetched once: the log reloads every
  // 10s and the roster does not change on that timescale.
  const [devices, setDevices] = useState<Device[]>([])

  // Bind every response to the exact query that started it. A periodic refresh
  // can still be in flight when an operator changes a filter; without both the
  // generation check and the query tag, that older response can replace the
  // filtered page after the newer response has rendered.
  const query = JSON.stringify([scope, limit, before?.ts, before?.id, category, severity])
  const load = useCallback(async () => {
    // Do not start another refresh for the same query while one is unresolved.
    // Otherwise every 10s tick invalidates the previous generation, and an API
    // that consistently takes longer than 10s can never publish a response.
    if (inFlight.current?.query === query) return
    const generation = ++requestGeneration.current
    inFlight.current = { query, generation }
    try {
      const page = await api.events({ scope, limit, before, category, severity })
      if (generation !== requestGeneration.current) return
      setLoaded({ query, page })
      setFailure(null)
    } catch (e) {
      if (generation !== requestGeneration.current) return
      // A same-query refresh keeps its last good page. A different query's
      // cached page is hidden below: showing it would claim those rows match
      // filters they were never fetched with.
      setFailure({
        query,
        message: e instanceof Error ? e.message : String(e),
      })
    } finally {
      if (inFlight.current?.generation === generation) inFlight.current = null
    }
  }, [scope, limit, before, category, severity, query])

  useEffect(() => {
    load()
    const t = setInterval(load, 10_000)
    return () => {
      clearInterval(t)
      // Invalidate a request from the previous filter even in the small window
      // before the next effect starts its replacement request.
      requestGeneration.current++
      // The request itself may still finish, but it is now obsolete. Clearing
      // this marker lets the next query start immediately and also keeps React
      // StrictMode's setup-cleanup-setup cycle from waiting for the old call.
      inFlight.current = null
    }
  }, [load])

  useEffect(() => {
    // A failure here costs the Device column its names, not its answers — the
    // row falls back to the id — so it is not surfaced as an error over the log.
    api
      .devices()
      .then((r) => setDevices(r.devices))
      .catch(() => {})
  }, [])

  const resetPage = () => {
    setBefore(null)
    setHistory([])
    detailGeneration.current++
    setDetail(null)
    setDetailError('')
  }

  // A keyset cursor belongs to one exact filter. Reusing it after a filter
  // change would skip the newer matching rows that preceded that cursor.
  const setFilter = (set: (v: string) => void) => (v: string) => {
    set(v)
    resetPage()
  }

  const selectScope = (next: EventScope) => {
    setScope(next)
    setCategory('')
    setSeverity('')
    resetPage()
  }

  // Never label rows from one query with another query's active filters. The
  // last page remains cached for a same-query refresh failure, but is not a
  // valid answer while a different filter/page is loading.
  const page = loaded?.query === query ? loaded.page : null
  const coverage = page?.coverage ?? {
    complete: false, expected_devices: 0, observed_devices: 0,
    gaps: ['router log coverage was not reported by this controller response'],
  }
  const rows = page?.events ?? []
  const err = failure?.query === query ? failure.message : ''
  const loading = page === null && err === ''

  const openDetail = async (row: EventRow) => {
    const generation = ++detailGeneration.current
    setDetail(null)
    setDetailError('')
    setDetailLoading(true)
    try {
      const event = await api.eventDetail(row.ID)
      if (generation === detailGeneration.current) setDetail(event)
    } catch (e) {
      if (generation === detailGeneration.current) {
        setDetailError(e instanceof Error ? e.message : String(e))
      }
    } finally {
      if (generation === detailGeneration.current) setDetailLoading(false)
    }
  }

  const columns: Column<EventRow>[] = [
    {
      key: 'ts',
      header: 'Time',
      width: 170,
      required: true,
      render: (e) => new Date(e.TS * 1000).toLocaleString(),
      sortBy: (e) => e.TS,
    },
    {
      key: 'sev',
      header: 'Severity',
      width: 90,
      render: (e) => (
        <span style={{ color: severityTone(e.Severity) }}>{e.Severity}</span>
      ),
      sortBy: (e) => e.Severity,
    },
    {
      key: 'cat',
      header: 'Category',
      width: 90,
      render: (e) => e.Category,
      sortBy: (e) => e.Category,
    },
    { key: 'event', header: 'Event', render: (e) => e.Event, sortBy: (e) => e.Event },
    {
      key: 'device',
      header: 'Device',
      width: 150,
      // Every device event carries a device_id, the API has always returned it,
      // and this screen had no column for it — not hidden, absent. So
      // "device.unreachable" never said which device. On a two-device lab you
      // can guess; on a fleet it makes the row useless, and the whole point of
      // an event log is answering "what happened to what".
      render: (e) =>
        e.DeviceID == null ? (
          <span style={{ color: 'var(--text-muted)' }}>—</span>
        ) : (
          (devices.find((d) => d.id === e.DeviceID)?.name ?? `device ${e.DeviceID}`)
        ),
      sortBy: (e) =>
        e.DeviceID == null
          ? ''
          : (devices.find((d) => d.id === e.DeviceID)?.name ?? String(e.DeviceID)),
    },
    {
      key: 'detail',
      header: 'Detail',
      render: (e) => (
        <span style={{ color: 'var(--text-secondary)' }}>{summarise(e.Detail)}</span>
      ),
    },
    {
      key: 'view',
      header: 'Action',
      width: 76,
      required: true,
      render: (e) => (
        <Button aria-label={`View event ${e.ID}: ${e.Event}`} onClick={(event) => {
          detailTrigger.current = event.currentTarget
          void openDetail(e)
        }}>View</Button>
      ),
    },
  ]

  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: '190px 1fr',
        gap: 14,
        alignItems: 'start',
      }}
    >
      <h1 style={{ margin: 0, fontSize: 20, gridColumn: '1 / -1' }}>Logs</h1>
      <FilterRail
        counted="all"
        groups={[
          {
            label: 'Severity',
            options: page?.facets.severity ?? [],
            selected: severity,
            onChange: setFilter(setSeverity),
          },
          {
            label: 'Category',
            options: page?.facets.category ?? [],
            selected: category,
            onChange: setFilter(setCategory),
          },
        ]}
      />
      <div style={{ display: 'grid', gap: 12 }}>
        <Card
          title={`Events (${page == null ? '…' : page.total.toLocaleString()})`}
          actions={(
            <div role="group" aria-label="Event view" style={{ display: 'flex', gap: 6 }}>
              {(['general', 'audit'] as const).map((value) => (
                <button
                  key={value}
                  type="button"
                  aria-pressed={scope === value}
                  onClick={() => selectScope(value)}
                  style={{
                    height: 28,
                    padding: '0 12px',
                    borderRadius: 6,
                    border: '1px solid var(--border-strong)',
                    color: scope === value ? '#fff' : 'var(--text-primary)',
                    background: scope === value ? 'var(--accent)' : 'var(--surface-2)',
                    cursor: 'pointer',
                  }}
                >
                  {value === 'general' ? 'General' : 'Audit'}
                </button>
              ))}
            </div>
          )}
          pad={false}
        >
          {err && (
            <div role="alert" style={{ padding: 12 }}>
              <Banner tone="critical">{err}</Banner>
            </div>
          )}
          {scope === 'general' && (
            <div style={{ padding: '12px 12px 0' }}>
              <Banner>
                General includes controller events plus router syslog and hostapd association events.
                Packet-flow/NFLOG, GeoIP, and application identity are not collected in this phase;
                blank enrichment fields mean unavailable, not no traffic.
              </Banner>
            </div>
          )}
          {scope === 'general' && page && !coverage.complete && (
            <div style={{ padding: '12px 12px 0' }} role="status">
              <Banner tone="warning">
                Router log coverage is incomplete. {coverage.gaps.join(' · ')}
              </Banner>
            </div>
          )}
          <DataGrid
            tableLabel={`${scope === 'audit' ? 'Audit' : 'General'} events`}
            totalRows={page?.total}
            rowOffset={history.length * limit}
            rows={rows}
            columns={columns}
            prefs={colPrefs}
            onPrefsChange={setColPrefs}
            rowKey={(e) => `event-${e.ID}`}
            empty={
              loading
                ? 'Loading events…'
                : err
                  ? 'Events for these filters could not be loaded.'
                  : category || severity
                    ? 'No events match these filters.'
                    : scope === 'audit'
                      ? 'No audit events yet.'
                      : coverage.complete
                        ? 'No general events were observed.'
                        : 'General event coverage is incomplete; an empty result is not proven.'
            }
          />
          {page && (
            <div
              style={{
                display: 'flex', alignItems: 'center', gap: 10, padding: '6px 12px',
                borderTop: '1px solid var(--border)', fontSize: 11,
                color: 'var(--text-secondary)',
              }}
            >
              <span className="num">
                Page {history.length + 1} · {rows.length.toLocaleString()} rows · {page.total.toLocaleString()} matching
              </span>
              <div style={{ flex: 1 }} />
              <label style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                Rows
                <select
                  value={limit}
                  onChange={(e) => {
                    setLimit(Number(e.target.value))
                    resetPage()
                  }}
                  style={{
                    background: 'var(--surface-0)', color: 'var(--text-primary)',
                    border: '1px solid var(--border-strong)', borderRadius: 4,
                    fontSize: 11, padding: '1px 4px',
                  }}
                >
                  {[50, 100, 250, 500, 1000].map((n) => <option key={n} value={n}>{n}</option>)}
                </select>
              </label>
              <Button
                disabled={history.length === 0}
                onClick={() => {
                  setBefore(history[history.length - 1] ?? null)
                  setHistory((current) => current.slice(0, -1))
                }}
              >
                Previous
              </Button>
              <Button
                disabled={!page.next_before}
                onClick={() => {
                  if (!page.next_before) return
                  setHistory((current) => [...current, before])
                  setBefore(page.next_before)
                }}
              >
                Next
              </Button>
            </div>
          )}
        </Card>

        {(detailLoading || detailError || detail) && (
          <EventDetail
            event={detail}
            loading={detailLoading}
            error={detailError}
            returnFocus={detailTrigger.current}
            deviceName={detail?.DeviceID == null
              ? ''
              : (devices.find((d) => d.id === detail.DeviceID)?.name ?? `device ${detail.DeviceID}`)}
            onClose={() => {
              detailGeneration.current++
              setDetail(null)
              setDetailError('')
              setDetailLoading(false)
            }}
          />
        )}
      </div>
    </div>
  )
}

function EventDetail({
  event, loading, error, deviceName, returnFocus, onClose,
}: {
  event: EventRow | null
  loading: boolean
  error: string
  deviceName: string
  returnFocus: HTMLElement | null
  onClose: () => void
}) {
  const panel = useRef<HTMLElement>(null)
  const closeRef = useRef(onClose)
  closeRef.current = onClose
  useEffect(() => {
    const previous = returnFocus ?? (document.activeElement instanceof HTMLElement ? document.activeElement : null)
    const focusable = () => Array.from(panel.current?.querySelectorAll<HTMLElement>(
      'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])',
    ) ?? [])
    const onKey = (keyboard: KeyboardEvent) => {
      if (keyboard.key === 'Escape') {
        keyboard.preventDefault()
        closeRef.current()
        return
      }
      if (keyboard.key !== 'Tab') return
      const items = focusable()
      if (items.length === 0) {
        keyboard.preventDefault()
        panel.current?.focus()
        return
      }
      const first = items[0]
      const last = items.at(-1)!
      if (keyboard.shiftKey && document.activeElement === first) {
        keyboard.preventDefault()
        last.focus()
      } else if (!keyboard.shiftKey && document.activeElement === last) {
        keyboard.preventDefault()
        first.focus()
      }
    }
    window.addEventListener('keydown', onKey)
    requestAnimationFrame(() => (focusable()[0] ?? panel.current)?.focus())
    return () => {
      window.removeEventListener('keydown', onKey)
      if (previous?.isConnected) previous.focus()
    }
  }, [])

  const facts = event == null ? [] : [
    ['Time', new Date(event.TS * 1000).toLocaleString()],
    ['Ingested', new Date(event.IngestedAt).toLocaleString()],
    ['Device', deviceName],
    ['Source', event.Source],
    ['Source identity', [event.SourceBoot, event.SourceID].filter(Boolean).join(' · ')],
    ['Client', event.ClientMAC],
    ['Action', event.Action],
    ['Direction', event.Direction],
    ['Interfaces', [event.InIface, event.OutIface].filter(Boolean).join(' → ')],
    ['Endpoints', endpointSummary(event)],
    ['Zones', [event.ZoneIn, event.ZoneOut].filter(Boolean).join(' → ')],
    ['Policy', event.PolicyID == null ? '' : String(event.PolicyID)],
  ].filter(([, value]) => value !== '')

  return (
    <>
      <div
        aria-hidden="true"
        onMouseDown={onClose}
        style={{ position: 'fixed', inset: 0, zIndex: 39, background: 'rgb(0 0 0 / 44%)' }}
      />
      <aside
        ref={panel}
        role="dialog"
        aria-modal="true"
        aria-labelledby="event-detail-title"
        tabIndex={-1}
        style={{
          position: 'fixed', zIndex: 40, top: 48, right: 8, bottom: 8,
          width: 'min(720px, calc(100vw - 76px))', overflow: 'auto',
          background: 'var(--surface-1)', border: '1px solid var(--border)', borderRadius: 8,
          boxShadow: '0 18px 64px rgb(0 0 0 / 45%)', padding: 14,
        }}
      >
        <header style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 12 }}>
          <h2 id="event-detail-title" style={{ margin: 0, fontSize: 15, flex: 1 }}>
            {event ? `${event.Event} · event ${event.ID}` : 'Event detail'}
          </h2>
          <Button onClick={onClose}>Close</Button>
        </header>
        {loading && <div role="status" aria-live="polite" style={{ color: 'var(--text-secondary)' }}>Loading event detail…</div>}
        {error && <div role="alert"><Banner tone="critical">{error}</Banner></div>}
        {event && (
          <div style={{ display: 'grid', gap: 12 }}>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: 8 }}>
            {facts.map(([label, value]) => (
              <div key={label}>
                <div style={{ color: 'var(--text-muted)', fontSize: 11 }}>{label}</div>
                <div style={{ fontSize: 12, overflowWrap: 'anywhere' }}>{value}</div>
              </div>
            ))}
          </div>
          <pre style={{
            margin: 0, padding: 10, maxHeight: 320, overflow: 'auto', borderRadius: 6,
            background: 'var(--surface-0)', border: '1px solid var(--border)',
            whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', fontSize: 11,
          }}>
            {JSON.stringify(event.Detail ?? {}, null, 2)}
          </pre>
          </div>
        )}
      </aside>
    </>
  )
}

function endpointSummary(event: EventRow): string {
  const endpoint = (ip: string, port: number | null) => ip && port != null ? `${ip}:${port}` : ip
  return [endpoint(event.SrcIP, event.SrcPort), endpoint(event.DstIP, event.DstPort)]
    .filter(Boolean)
    .join(' → ')
}

function severityTone(s: string): string {
  return s === 'error'
    ? 'var(--critical)'
    : s === 'warning'
      ? 'var(--warning)'
      : 'var(--text-secondary)'
}

function summarise(detail: unknown): string {
  if (!detail || typeof detail !== 'object') return ''
  const shown: string[] = []
  let dropped = 0
  for (const [k, v] of Object.entries(detail as Record<string, unknown>)) {
    if (v === null || v === '' || (Array.isArray(v) && v.length === 0)) continue
    if (shown.length >= 4) {
      dropped++
      continue
    }
    shown.push(`${k}=${brief(v)}`)
  }
  // Say when the cell is not the whole detail. It stopped at four keys and
  // joined what it had, so a row with more looked complete and was not — an
  // apply event carrying device, ssid, changes, omissions and outcome showed
  // the first four and silently dropped the outcome.
  const more = dropped > 0 ? `  (+${dropped} more)` : ''
  return shown.join('  ') + more
}

/**
 * One value, short enough to sit in a table cell.
 *
 * This used to be JSON.stringify for anything object-shaped, which put a whole
 * serialised array of apply omissions — each with a full sentence of prose —
 * into a single cell. It ran off the side of the screen, forced the table into
 * a horizontal scrollbar, and was unreadable even after scrolling to it.
 *
 * Counting is the honest summary for a list: "3 items" tells a reader there is
 * something to look at without pretending the cell can hold it. It must never
 * silently drop the fact that more exists, which is why the count is the
 * summary rather than the first element.
 */
function brief(v: unknown): string {
  if (Array.isArray(v)) {
    return `${v.length} item${v.length === 1 ? '' : 's'}`
  }
  if (typeof v === 'object' && v !== null) {
    const keys = Object.keys(v as Record<string, unknown>)
    return `{${keys.slice(0, 3).join(', ')}${keys.length > 3 ? ', …' : ''}}`
  }
  const s = String(v)
  return s.length > 80 ? `${s.slice(0, 79)}…` : s
}
