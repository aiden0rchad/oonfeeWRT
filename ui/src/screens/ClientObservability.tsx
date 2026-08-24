import { useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../lib/api'
import { live } from '../lib/live'
import type {
  Client,
  ClientObservability as ClientObservabilityData,
  ClientObservabilityEvent,
  ClientObservabilityMetric,
  ClientObservabilityPathInterval,
} from '../lib/api'
import { Banner, Card } from '../components/ui'

const DAY_MS = 24 * 60 * 60 * 1000
const ROLLUP_MS = 5 * 60 * 1000

export function ClientObservability({ client, onClose }: { client: Client; onClose: () => void }) {
  const closeButton = useRef<HTMLButtonElement>(null)
  const [data, setData] = useState<ClientObservabilityData | null>(null)
  const [error, setError] = useState('')
  const [cursorTs, setCursorTs] = useState(() => Date.now())
  const [selectedEventID, setSelectedEventID] = useState<number | null>(null)
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  useEffect(() => {
    let active = true
    let request = 0
    setData(null)
    setError('')
    setSelectedEventID(null)
    const refresh = async (initial: boolean) => {
      const currentRequest = ++request
      const to = Date.now()
      const nextRange = { from: to - DAY_MS, to }
      try {
        const result = await api.clientObservability(client.mac, nextRange.from, nextRange.to)
        if (!active || currentRequest !== request) return
        setData(result)
        setError('')
        setCursorTs((cursor) => initial
          ? (result.timestamps.at(-1) ?? result.to)
          : Math.max(result.from, Math.min(cursor, result.to)))
      } catch (reason) {
        if (active && currentRequest === request) {
          setError(reason instanceof Error ? reason.message : String(reason))
        }
      }
    }
    void refresh(true)
    const timer = window.setInterval(() => void refresh(false), ROLLUP_MS)
    return () => {
      active = false
      window.clearInterval(timer)
    }
  }, [client.mac])

  // The joined timeline, not the inventory row, is the source of historical AP
  // attribution. Follow its newest unambiguous bucket and let the refcounted
  // live client release/rebind whenever a refresh changes or removes it.
  const watchedAP = data ? newestAttributedAP(data.ap_device_at) : null
  useEffect(() => {
    if (watchedAP == null) return
    return live.watch(watchedAP)
  }, [watchedAP])

  // This is part of the four-pane workspace, not a modal over its filter and
  // client panes. Escape still closes it and focus returns to the opener, but
  // Tab remains free to traverse all four visible panes.
  useEffect(() => {
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        onCloseRef.current()
      }
    }
    window.addEventListener('keydown', onKey)
    requestAnimationFrame(() => closeButton.current?.focus())
    return () => {
      window.removeEventListener('keydown', onKey)
      if (previous?.isConnected) previous.focus()
    }
  }, [])

  const index = data ? bucketIndex(data.timestamps, data.bucket_ms, cursorTs) : -1
  const selectedEvent = data
    ? data.events.find((event) => event.id === selectedEventID) ?? nearestEvent(data.events, cursorTs)
    : undefined
  const selectedPath = data ? pathAt(data.paths, cursorTs) : undefined
  const moveCursor = (ts: number) => {
    setSelectedEventID(null)
    setCursorTs(ts)
  }
  const selectEvent = (event: ClientObservabilityEvent) => {
    setSelectedEventID(event.id)
    setCursorTs(event.ts)
  }
  const grouped = useMemo(() => ({
    client: data?.metrics.filter((metric) => metric.scope === 'client') ?? [],
    ap: data?.metrics.filter((metric) => metric.scope === 'ap') ?? [],
    site: data?.metrics.filter((metric) => metric.scope === 'site') ?? [],
  }), [data])

  return (
    <>
      <header className="client-observability-toolbar">
        <div className="client-observability-heading">
          <div>
            <strong id="client-observability-title" style={{ fontSize: 14 }}>
              Client observability · {client.name || client.mac}
            </strong>
            <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 2 }}>{client.mac}</div>
          </div>
          <button
            ref={closeButton}
            type="button"
            onClick={onClose}
            aria-label="Close client observability"
            style={closeStyle}
          >
            ×
          </button>
        </div>
        {data && (
          <section aria-label="Shared investigation cursor" className="client-observability-cursor">
            <div>
              <label htmlFor="client-observability-cursor" style={{ fontSize: 12, fontWeight: 600 }}>
                Investigation time
              </label>
              <div aria-live="polite" style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 2 }}>
                {new Date(cursorTs).toLocaleString()} · one cursor controls every chart, event and path below
              </div>
            </div>
            <input
              id="client-observability-cursor"
              type="range"
              min={data.from}
              max={data.to}
              step={1000}
              value={cursorTs}
              onChange={(event) => moveCursor(Number(event.target.value))}
            />
          </section>
        )}
      </header>

      <aside className="client-observability-events" aria-label="Event spine">
        {data ? (
          <EventSpine
            events={data.events}
            selected={selectedEvent}
            cursorTs={cursorTs}
            onSelect={selectEvent}
            truncated={data.data_contract.events_truncated}
          />
        ) : (
          <Card title="Event spine">
            <div role="status" style={mutedStyle}>
              {error ? 'Events are unavailable while this investigation cannot load.' : 'Loading client events…'}
            </div>
          </Card>
        )}
      </aside>

      <section className="client-observability-analysis" aria-label="Client analysis">
        {error && <Banner tone="critical">Client observability could not load: {error}</Banner>}
        {!data && !error && <div role="status" style={mutedStyle}>Loading the 24-hour investigation…</div>}
        {data && (
          <>
            {data.gaps.length > 0 && (
              <Banner tone="accent">
                <strong>Known limits:</strong> {data.gaps.join(' · ')}
              </Banner>
            )}
            <PathSummary interval={selectedPath} cursorTs={cursorTs} />
            <MetricGroup
              title="Client health"
              metrics={grouped.client}
              timestamps={data.timestamps}
              bucketMs={data.bucket_ms}
              index={index}
              cursorTs={cursorTs}
              onCursor={moveCursor}
              formula={data.experience_formula}
            />
            <MetricGroup
              title="AP health"
              metrics={grouped.ap}
              timestamps={data.timestamps}
              bucketMs={data.bucket_ms}
              index={index}
              cursorTs={cursorTs}
              onCursor={moveCursor}
              subtitle={index >= 0 && data.ap_device_at[index] != null
                ? `Client AP at cursor: device ${data.ap_device_at[index]}`
                : 'Client AP at cursor is unavailable'}
            />
            <MetricGroup
              title="Site health"
              metrics={grouped.site}
              timestamps={data.timestamps}
              bucketMs={data.bucket_ms}
              index={index}
              cursorTs={cursorTs}
              onCursor={moveCursor}
            />
            <div role="note" style={mutedStyle}>
              Metrics are {data.data_contract.metric_source.replace('_', ' ')} aggregates.
              Lines show bucket averages and shaded bands show stored minima and maxima.
              Raw samples are not persisted, so this surface does not claim raw-data precision.
              Event time resolution is {data.data_contract.event_time_resolution_ms / 1000} second.
            </div>
          </>
        )}
      </section>
    </>
  )
}

function EventSpine({ events, selected, cursorTs, onSelect, truncated }: {
  events: ClientObservabilityEvent[]
  selected?: ClientObservabilityEvent
  cursorTs: number
  onSelect: (event: ClientObservabilityEvent) => void
  truncated: boolean
}) {
  return (
    <Card title="Event spine">
      <div style={{ fontSize: 11, color: 'var(--text-muted)', marginBottom: 8 }}>
        Exact persisted events for this client; selecting one moves the shared cursor.
      </div>
      {events.length === 0 ? (
        <div style={mutedStyle}>
          No sourced client event was returned. Historical router-log coverage for this interval is unavailable.
        </div>
      ) : (
        <ol aria-label="Client events" style={{ listStyle: 'none', margin: 0, padding: 0, display: 'grid', gap: 6 }}>
          {events.map((event) => {
            const active = selected?.id === event.id
            return (
              <li key={event.id}>
                <button
                  type="button"
                  aria-pressed={active}
                  onClick={() => onSelect(event)}
                  style={{
                    width: '100%', padding: '7px 8px', textAlign: 'left', borderRadius: 5,
                    border: `1px solid ${active ? 'var(--accent)' : 'var(--border)'}`,
                    background: active ? 'var(--accent-soft)' : 'transparent', color: 'var(--text-primary)',
                    cursor: 'pointer',
                  }}
                >
                  <span style={{ display: 'block', fontSize: 11, fontWeight: 600 }}>{event.event}</span>
                  <span style={{ display: 'block', fontSize: 10, color: 'var(--text-muted)', marginTop: 2 }}>
                    {new Date(event.ts).toLocaleTimeString()} · {event.source}
                    {event.source_id ? ` #${event.source_id}` : ''}
                  </span>
                </button>
              </li>
            )
          })}
        </ol>
      )}
      {selected && (
        <dl style={{ margin: '10px 0 0', fontSize: 11, display: 'grid', gridTemplateColumns: '64px 1fr', gap: 4 }}>
          <dt style={mutedStyle}>Action</dt><dd style={{ margin: 0 }}>{selected.action || 'not classified'}</dd>
          <dt style={mutedStyle}>Interface</dt><dd style={{ margin: 0 }}>{selected.in_iface || selected.out_iface || 'not reported'}</dd>
          <dt style={mutedStyle}>At cursor</dt><dd style={{ margin: 0 }}>{Math.abs(selected.ts - cursorTs) < 1000 ? 'yes' : 'nearest event'}</dd>
        </dl>
      )}
      {truncated && <div role="note" style={{ ...mutedStyle, marginTop: 8 }}>Event limit reached; this spine is incomplete.</div>}
    </Card>
  )
}

function PathSummary({ interval, cursorTs }: { interval?: ClientObservabilityPathInterval; cursorTs: number }) {
  return (
    <Card title="Path at cursor">
      {!interval ? (
        <div style={mutedStyle}>No persisted topology interval contains {new Date(cursorTs).toLocaleTimeString()}.</div>
      ) : (
        <div style={{ display: 'grid', gap: 7 }}>
          {(interval.paths ?? []).map((path, index) => {
            const nodes = path.node_ids ?? []
            const labels = path.labels ?? []
            const mediums = path.mediums ?? []
            return (
              <div key={`${nodes.join('|')}-${index}`}>
                <div style={{ fontSize: 12, fontWeight: 600 }}>
                  {labels.join(' → ') || 'Unlabelled observed path'}
                </div>
                <div style={mutedStyle}>{mediums.join(' → ') || 'no observed link'} · {path.confidence}</div>
              </div>
            )
          })}
          {(interval.paths ?? []).length === 0 && <div style={mutedStyle}>No client path was observed.</div>}
          <div style={{ fontSize: 11, color: interval.complete ? 'var(--good)' : 'var(--warning)' }}>
            {interval.complete ? 'Complete observed path to Internet' : 'Incomplete or ambiguous path'}
          </div>
          {(interval.gaps ?? []).length > 0 && <div role="note" style={mutedStyle}>{interval.gaps.join(' · ')}</div>}
        </div>
      )}
    </Card>
  )
}

function MetricGroup({ title, subtitle, metrics, timestamps, bucketMs, index, cursorTs, onCursor, formula }: {
  title: string
  subtitle?: string
  metrics: ClientObservabilityMetric[]
  timestamps: number[]
  bucketMs: number
  index: number
  cursorTs: number
  onCursor: (ts: number) => void
  formula?: ClientObservabilityData['experience_formula']
}) {
  return (
    <Card title={title}>
      {subtitle && <div style={{ ...mutedStyle, marginBottom: 8 }}>{subtitle}</div>}
      {formula && (
        <div role="note" style={{ ...mutedStyle, marginBottom: 8 }}>
          {formula.name}: RSSI {formula.weights.rssi * 100}% · retry delta {formula.weights.retry_delta * 100}% ·
          TX failure delta {formula.weights.tx_fail_delta * 100}%. {formula.missing_policy}.
        </div>
      )}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(260px, 1fr))', gap: 10 }}>
        {metrics.map((metric) => (
          <MetricChart
            key={metric.id}
            metric={metric}
            timestamps={timestamps}
            bucketMs={bucketMs}
            index={index}
            cursorTs={cursorTs}
            onCursor={onCursor}
          />
        ))}
      </div>
    </Card>
  )
}

function MetricChart({ metric, timestamps, bucketMs, index, cursorTs, onCursor }: {
  metric: ClientObservabilityMetric
  timestamps: number[]
  bucketMs: number
  index: number
  cursorTs: number
  onCursor: (ts: number) => void
}) {
  const [view, setView] = useState<'chart' | 'table'>('chart')
  const averages = metric.values.filter(isFiniteNumber)
  const envelope = [...(metric.mins ?? []), ...(metric.maxs ?? [])].filter(isFiniteNumber)
  const finite = [...averages, ...envelope]
  const min = finite.length ? Math.min(...finite) : 0
  const max = finite.length ? Math.max(...finite) : 1
  const span = max === min ? 1 : max - min
  const x = (i: number) => timestamps.length <= 1 ? 300 : 5 + (i / (timestamps.length - 1)) * 590
  const y = (value: number) => 75 - ((value - min) / span) * 65
  const segments: string[][] = []
  let segment: string[] = []
  metric.values.forEach((value, i) => {
    if (value == null || !Number.isFinite(value)) {
      if (segment.length) segments.push(segment)
      segment = []
    } else {
      segment.push(`${x(i)},${y(value)}`)
    }
  })
  if (segment.length) segments.push(segment)
  const nominalBandWidth = timestamps.length <= 1 ? 590 : 590 / (timestamps.length - 1)
  const bands = metric.values.flatMap((_value, i) => {
    const low = metric.mins?.[i]
    const high = metric.maxs?.[i]
    if (!isFiniteNumber(low) || !isFiniteNumber(high)) return []
    const left = Math.max(5, x(i) - nominalBandWidth / 2)
    const right = Math.min(595, x(i) + nominalBandWidth / 2)
    const top = y(Math.max(low, high))
    const bottom = y(Math.min(low, high))
    return [{ x: left, y: top, width: right - left, height: Math.max(1, bottom - top) }]
  })
  const selected = index >= 0 ? metric.values[index] : null
  const selectedMin = index >= 0 ? metric.mins?.[index] : null
  const selectedMax = index >= 0 ? metric.maxs?.[index] : null
  const selectedCount = index >= 0 ? metric.counts?.[index] : null
  const selectedX = index >= 0 ? x(index) : 5
  const provenance = [metric.device_name, metric.key].filter(Boolean).join(' · ')
  const tooltipID = `metric-tooltip-${metric.id.replace(/[^a-zA-Z0-9_-]/g, '-')}`
  const windowLabel = index >= 0 && timestamps[index] != null
    ? formatBucketWindow(timestamps[index], bucketMs)
    : 'No completed rollup bucket at this time'
  const hasEnvelope = isFiniteNumber(selectedMin) && isFiniteNumber(selectedMax)

  return (
    <section aria-label={`${metric.label} metric`} style={{ border: '1px solid var(--border)', borderRadius: 6, padding: 9 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8, alignItems: 'baseline' }}>
        <strong style={{ fontSize: 12 }}>{metric.label}</strong>
        <span className="num" style={{ fontSize: 12 }}>Avg {formatMetric(selected, metric.unit)}</span>
      </div>
      {provenance && <div style={mutedStyle}>{provenance}</div>}
      <div role="group" aria-label={`${metric.label} view`} style={{ display: 'flex', gap: 4, margin: '7px 0' }}>
        <button
          type="button"
          aria-pressed={view === 'chart'}
          onClick={() => setView('chart')}
          style={viewButtonStyle(view === 'chart')}
        >
          Chart
        </button>
        <button
          type="button"
          aria-pressed={view === 'table'}
          onClick={() => setView('table')}
          style={viewButtonStyle(view === 'table')}
        >
          Table
        </button>
      </div>
      <div id={tooltipID} role="tooltip" style={tooltipStyle}>
        <strong>{windowLabel}</strong>
        <span>Rollup average: {formatMetric(selected, metric.unit)}</span>
        <span>
          Stored range: {hasEnvelope
            ? `${formatMetric(Math.min(selectedMin, selectedMax), metric.unit)} – ${formatMetric(Math.max(selectedMin, selectedMax), metric.unit)}`
            : 'Unavailable'}
        </span>
        <span>{selectedCount == null ? 'Source sample count: Unavailable' : `Source samples: ${selectedCount}`}</span>
      </div>
      {view === 'chart' && (averages.length === 0 ? (
        <div style={{ ...emptyChartStyle, height: 86 }}>{metric.availability.reason || 'Unavailable'}</div>
      ) : (
        <svg
          viewBox="0 0 600 84"
          width="100%"
          height="86"
          role="img"
          aria-describedby={tooltipID}
          aria-label={`${metric.label} rollup averages with stored minimum and maximum bands at ${new Date(cursorTs).toLocaleString()}`}
          onPointerMove={(event) => {
            const box = event.currentTarget.getBoundingClientRect()
            if (box.width <= 0 || timestamps.length === 0) return
            const ratio = Math.max(0, Math.min(1, (event.clientX - box.left) / box.width))
            onCursor(timestamps[Math.round(ratio * (timestamps.length - 1))])
          }}
        >
          <line x1="5" y1="76" x2="595" y2="76" stroke="var(--border)" />
          {bands.map((band, i) => (
            <rect
              key={`band-${i}`}
              data-rollup-band="true"
              x={band.x}
              y={band.y}
              width={band.width}
              height={band.height}
              fill="var(--series-1)"
              opacity="0.18"
            />
          ))}
          {segments.map((points, i) => (
            <polyline key={i} points={points.join(' ')} fill="none" stroke="var(--series-1)" strokeWidth="2" />
          ))}
          <line x1={selectedX} y1="5" x2={selectedX} y2="78" stroke="var(--accent)" strokeWidth="1" />
        </svg>
      ))}
      {view === 'table' && (
        <div style={{ overflowX: 'auto', maxHeight: 260, overflowY: 'auto' }}>
          <table aria-label={`${metric.label} rollup table`} style={metricTableStyle}>
            <caption style={{ textAlign: 'left', color: 'var(--text-muted)', paddingBottom: 5 }}>
              Completed {metric.availability.source.replace('_', ' ')} buckets; unavailable cells are source gaps.
            </caption>
            <thead>
              <tr>
                <th scope="col">Time window</th>
                <th scope="col">Average</th>
                <th scope="col">Minimum</th>
                <th scope="col">Maximum</th>
                <th scope="col">Samples</th>
              </tr>
            </thead>
            <tbody>
              {timestamps.map((timestamp, i) => (
                <tr key={timestamp}>
                  <th scope="row">{formatBucketWindow(timestamp, bucketMs)}</th>
                  <td>{formatMetric(metric.values[i], metric.unit)}</td>
                  <td>{formatMetric(metric.mins?.[i], metric.unit)}</td>
                  <td>{formatMetric(metric.maxs?.[i], metric.unit)}</td>
                  <td>{metric.counts?.[i] ?? 'Unavailable'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div style={mutedStyle}>
        {metric.availability.state} · {metric.availability.observed_points}/{metric.availability.expected_points} buckets ·
        {' '}{metric.availability.source.replace('_', ' ')}
        {metric.availability.gaps.length > 0 && ` · ${metric.availability.gaps.length} gap${metric.availability.gaps.length === 1 ? '' : 's'}`}
      </div>
    </section>
  )
}

function bucketIndex(timestamps: number[], bucketMs: number, cursorTs: number): number {
  for (let i = 0; i < timestamps.length; i++) {
    if (timestamps[i] <= cursorTs && cursorTs < timestamps[i] + bucketMs) return i
  }
  return -1
}

function nearestEvent(events: ClientObservabilityEvent[], cursorTs: number) {
  return events.reduce<ClientObservabilityEvent | undefined>((best, event) =>
    !best || Math.abs(event.ts - cursorTs) < Math.abs(best.ts - cursorTs) ? event : best, undefined)
}

function pathAt(paths: ClientObservabilityPathInterval[], cursorTs: number) {
  return paths.find((interval) => interval.from <= cursorTs && cursorTs < interval.to)
}

function newestAttributedAP(attribution: (number | null)[]): number | null {
  for (let i = attribution.length - 1; i >= 0; i--) {
    const deviceID = attribution[i]
    if (deviceID != null) return deviceID
  }
  return null
}

function isFiniteNumber(value: number | null | undefined): value is number {
  return value != null && Number.isFinite(value)
}

function formatBucketWindow(timestamp: number, bucketMs: number): string {
  return `${new Date(timestamp).toLocaleString()} – ${new Date(timestamp + bucketMs).toLocaleString()}`
}

function formatMetric(value: number | null | undefined, unit: string): string {
  if (value == null || !Number.isFinite(value)) return 'Unavailable'
  if (unit === 'state') return value >= .5 ? 'Up' : 'Down'
  const rendered = Math.abs(value) >= 100 || Number.isInteger(value) ? value.toFixed(0) : value.toFixed(1)
  return unit === 'score' ? `${rendered}/100` : `${rendered} ${unit}`
}

const closeStyle = {
  width: 30, height: 30, border: 0, borderRadius: 5, background: 'transparent',
  color: 'var(--text-secondary)', cursor: 'pointer', fontSize: 20,
} as const

const mutedStyle = { fontSize: 11, color: 'var(--text-muted)' } as const

const emptyChartStyle = {
  display: 'grid', placeItems: 'center', textAlign: 'center', color: 'var(--text-muted)',
  border: '1px dashed var(--border)', borderRadius: 5, margin: '7px 0', padding: 8, fontSize: 11,
} as const

const tooltipStyle = {
  display: 'grid', gap: 2, padding: '6px 8px', marginBottom: 6, borderRadius: 5,
  background: 'var(--surface-2)', color: 'var(--text-secondary)', fontSize: 10,
} as const

const metricTableStyle = {
  width: '100%', borderCollapse: 'collapse', fontSize: 10, whiteSpace: 'nowrap',
} as const

function viewButtonStyle(active: boolean) {
  return {
    minHeight: 26, padding: '2px 9px', borderRadius: 5, cursor: 'pointer', fontSize: 11,
    border: `1px solid ${active ? 'var(--accent)' : 'var(--border-strong)'}`,
    color: active ? 'var(--accent-text)' : 'var(--text-secondary)',
    background: active ? 'var(--accent-soft)' : 'var(--surface-2)',
  } as const
}
