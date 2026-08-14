import { useState } from 'react'
import type { EventRow } from '../lib/api'
import { Card, DataGrid } from '../components/ui'
import type { Column } from '../components/ui'

/**
 * The event log.
 *
 * Filter counts come from the whole result set, never from the visible page —
 * a count computed from what happens to be loaded is a lie that makes a UI feel
 * responsive right up until someone relies on it (UI-SPEC §5).
 */
export function Logs({ events }: { events: EventRow[] }) {
  const [category, setCategory] = useState<string>('')
  const [severity, setSeverity] = useState<string>('')

  const counts = (field: 'Category' | 'Severity') => {
    const m = new Map<string, number>()
    for (const e of events) m.set(e[field], (m.get(e[field]) ?? 0) + 1)
    return [...m.entries()].sort((a, b) => b[1] - a[1])
  }

  const rows = events.filter(
    (e) =>
      (category === '' || e.Category === category) &&
      (severity === '' || e.Severity === severity),
  )

  const columns: Column<EventRow>[] = [
    {
      key: 'ts',
      header: 'Time',
      width: 170,
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
    { key: 'cat', header: 'Category', width: 90, render: (e) => e.Category, sortBy: (e) => e.Category },
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
    <div style={{ display: 'grid', gridTemplateColumns: '190px 1fr', gap: 14, alignItems: 'start' }}>
      <Card title="Filters">
        <FilterGroup
          label="Severity"
          options={counts('Severity')}
          value={severity}
          onChange={setSeverity}
        />
        <div style={{ height: 12 }} />
        <FilterGroup
          label="Category"
          options={counts('Category')}
          value={category}
          onChange={setCategory}
        />
      </Card>
      <Card title={`Events (${rows.length})`} pad={false}>
        <DataGrid rows={rows} columns={columns} rowKey={(e) => `${e.TS}-${e.Event}-${e.Category}`} empty="No events match these filters." />
      </Card>
    </div>
  )
}

function FilterGroup({
  label,
  options,
  value,
  onChange,
}: {
  label: string
  options: [string, number][]
  value: string
  onChange: (v: string) => void
}) {
  return (
    <div>
      <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-secondary)', marginBottom: 6 }}>
        {label}
      </div>
      <div style={{ display: 'grid', gap: 3 }}>
        <Option label="All" count={options.reduce((n, [, c]) => n + c, 0)} active={value === ''} onClick={() => onChange('')} />
        {options.map(([k, n]) => (
          <Option key={k} label={k} count={n} active={value === k} onClick={() => onChange(k)} />
        ))}
      </div>
    </div>
  )
}

function Option({
  label,
  count,
  active,
  onClick,
}: {
  label: string
  count: number
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      onClick={onClick}
      style={{
        display: 'flex',
        justifyContent: 'space-between',
        alignItems: 'center',
        padding: '3px 7px',
        borderRadius: 4,
        border: 'none',
        cursor: 'pointer',
        fontSize: 12,
        background: active ? 'var(--accent-soft)' : 'transparent',
        color: 'var(--text-primary)',
      }}
    >
      <span>{label}</span>
      <span className="num" style={{ color: 'var(--text-secondary)' }}>
        {count}
      </span>
    </button>
  )
}

function severityTone(s: string): string {
  return s === 'error' ? 'var(--critical)' : s === 'warning' ? 'var(--warning)' : 'var(--text-secondary)'
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
