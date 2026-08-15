import { useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api'
import type { EventPage, EventRow } from '../lib/api'
import { Banner, Card, DataGrid, FilterRail, Pager, useColumnPrefs } from '../components/ui'
import type { Column } from '../components/ui'

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
  const [category, setCategory] = useState('')
  const [severity, setSeverity] = useState('')
  const [limit, setLimit] = useState(100)
  const [offset, setOffset] = useState(0)
  const [page, setPage] = useState<EventPage | null>(null)
  const [err, setErr] = useState('')
  const [colPrefs, setColPrefs] = useColumnPrefs('logs')

  const load = useCallback(async () => {
    try {
      setPage(await api.events({ limit, offset, category, severity }))
      setErr('')
    } catch (e) {
      // Keep the last good page on screen. Blanking it on one dropped request
      // would look like "no events", which is a different claim entirely.
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [limit, offset, category, severity])

  useEffect(() => {
    load()
    const t = setInterval(load, 10_000)
    return () => clearInterval(t)
  }, [load])

  // Changing a filter has to reset the offset. Page 4 of the unfiltered log is
  // not page 4 of the filtered one, and keeping the offset lands on an empty
  // page that reads as "no matches".
  const setFilter = (set: (v: string) => void) => (v: string) => {
    set(v)
    setOffset(0)
  }

  const rows = page?.events ?? []

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
      key: 'detail',
      header: 'Detail',
      render: (e) => (
        <span style={{ color: 'var(--text-secondary)' }}>{summarise(e.Detail)}</span>
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
      <Card title={`Events (${(page?.total ?? 0).toLocaleString()})`} pad={false}>
        {err && (
          <div style={{ padding: 12 }}>
            <Banner tone="critical">{err}</Banner>
          </div>
        )}
        <DataGrid
          rows={rows}
          columns={columns}
          prefs={colPrefs}
          onPrefsChange={setColPrefs}
          rowKey={(e) => `${e.TS}-${e.Category}-${e.Event}`}
          empty={
            category || severity
              ? 'No events match these filters.'
              : 'No events yet.'
          }
        />
        {page && (
          <Pager
            total={page.total}
            limit={limit}
            offset={offset}
            onChange={(l, o) => {
              setLimit(l)
              setOffset(o)
            }}
          />
        )}
      </Card>
    </div>
  )
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
  const parts: string[] = []
  for (const [k, v] of Object.entries(detail as Record<string, unknown>)) {
    if (v === null || v === '' || (Array.isArray(v) && v.length === 0)) continue
    parts.push(`${k}=${typeof v === 'object' ? JSON.stringify(v) : String(v)}`)
    if (parts.length >= 4) break
  }
  return parts.join('  ')
}
