import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { TopologyEdge, TopologyNode, TopologySnapshot } from '../lib/api'
import { Banner, Button, Card, Stat, Status } from '../components/ui'
import { DeviceDetailPanel } from './Devices'

type Mode = 'current' | 'history'
type HistoryPreset = '1' | '24' | '168' | '744' | 'custom'
type HistoryRange =
  | { kind: 'preset'; hours: number }
  | { kind: 'custom'; from: number; to: number }

interface Position {
  x: number
  y: number
}

type Confidence = TopologyEdge['confidence']
type Medium = TopologyEdge['medium']

const hourMillis = 60 * 60 * 1000
const maxHistoryMillis = 31 * 24 * hourMillis

const edgeVisuals: Record<Confidence, { stroke: string; width: number; dash?: string }> = {
  measured: { stroke: 'var(--accent)', width: 5 },
  inferred: { stroke: 'var(--text-secondary)', width: 4, dash: '14 7' },
  ambiguous: { stroke: 'var(--warning)', width: 5, dash: '2 8' },
}

function ConfidenceLegendItem({ confidence }: { confidence: Confidence }) {
  const visual = edgeVisuals[confidence]
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 5 }}>
      <svg aria-hidden width="28" height="8" viewBox="0 0 28 8">
        <line
          x1="1" y1="4" x2="27" y2="4"
          stroke={visual.stroke}
          strokeWidth={visual.width}
          strokeDasharray={visual.dash}
          strokeLinecap="round"
        />
      </svg>
      {confidence}
    </span>
  )
}

/** Separate simultaneous candidate-parent lines so they cannot hide behind
 * each other or run through an intermediate node. */
export function edgeLaneOffsets(edges: TopologyEdge[]) {
  const byChild = new Map<string, TopologyEdge[]>()
  for (const edge of edges) {
    byChild.set(edge.child_id, [...(byChild.get(edge.child_id) ?? []), edge])
  }
  const offsets = new Map<TopologyEdge['id'], number>()
  for (const candidates of byChild.values()) {
    candidates.sort((a, b) =>
      `${a.parent_id}\u0000${a.parent_port ?? ''}\u0000${a.id}`.localeCompare(
        `${b.parent_id}\u0000${b.parent_port ?? ''}\u0000${b.id}`,
      ),
    )
    candidates.forEach((edge, index) => {
      offsets.set(edge.id, (index - (candidates.length - 1) / 2) * 180)
    })
  }
  return offsets
}

/** A deterministic, dependency-free tidy layout. Disconnected and cyclic
 * nodes remain visible instead of being silently dropped. */
export function layoutTopology(nodes: TopologyNode[], edges: TopologyEdge[]) {
  const ids = new Set(nodes.map((node) => node.id))
  const children = new Set(edges.map((edge) => edge.child_id))
  const roots = nodes.filter((node) => !children.has(node.id)).map((node) => node.id)
  if (roots.length === 0) roots.push(...nodes.map((node) => node.id))
  roots.sort()

  const depth = new Map<string, number>(roots.map((id) => [id, 0]))
  const ordered = [...edges].sort((a, b) =>
    `${a.parent_id}\u0000${a.child_id}\u0000${a.id}`.localeCompare(
      `${b.parent_id}\u0000${b.child_id}\u0000${b.id}`,
    ),
  )
  for (let pass = 0; pass < nodes.length; pass += 1) {
    let changed = false
    for (const edge of ordered) {
      const parentDepth = depth.get(edge.parent_id)
      if (parentDepth == null || !ids.has(edge.child_id)) continue
      const next = parentDepth + 1
      if (depth.get(edge.child_id) == null || depth.get(edge.child_id)! < next) {
        depth.set(edge.child_id, next)
        changed = true
      }
    }
    if (!changed) break
  }
  for (const node of nodes) if (!depth.has(node.id)) depth.set(node.id, 0)

  const levels = new Map<number, TopologyNode[]>()
  for (const node of nodes) {
    const level = Math.min(depth.get(node.id) ?? 0, Math.max(nodes.length - 1, 0))
    levels.set(level, [...(levels.get(level) ?? []), node])
  }
  const positions = new Map<string, Position>()
  const width = 1000
  for (const [level, members] of [...levels.entries()].sort((a, b) => a[0] - b[0])) {
    members.sort((a, b) => a.id.localeCompare(b.id))
    members.forEach((node, index) => {
      positions.set(node.id, {
        x: ((index + 1) * width) / (members.length + 1),
        y: 56 + level * 132,
      })
    })
  }
  const maxDepth = Math.max(0, ...levels.keys())
  return { positions, width, height: Math.max(180, 112 + maxDepth * 132) }
}

function when(ms?: number) {
  if (ms == null) return 'Active'
  return new Date(ms).toLocaleString()
}

function datetimeLocalValue(ms: number) {
  const date = new Date(ms)
  const part = (value: number) => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${part(date.getMonth() + 1)}-${part(date.getDate())}T${part(date.getHours())}:${part(date.getMinutes())}`
}

function nodeLabel(nodes: Map<string, TopologyNode>, id: string) {
  return nodes.get(id)?.name || id
}

function detailValue(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(detailValue).join(', ')}]`
  if (value && typeof value === 'object') {
    return `{${Object.entries(value as Record<string, unknown>)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([key, nested]) => `${key}: ${detailValue(nested)}`)
      .join(', ')}}`
  }
  return value === null ? 'null' : String(value)
}

export function detailText(detail: Record<string, unknown>) {
  return Object.entries(detail)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([key, value]) => `${key}: ${detailValue(value)}`)
    .join(' · ')
}

function edgeVLAN(edge: TopologyEdge) {
  for (const evidence of edge.evidence) {
    const value = evidence.detail.vlan ?? evidence.detail.vlan_id
    if (typeof value === 'number' && Number.isInteger(value) && value >= 0) return String(value)
    if (typeof value === 'string' && /^\d{1,4}$/.test(value)) return value
  }
  return 'unknown'
}

export function topologyEdgesAt(edges: TopologyEdge[], at: number) {
  return edges.filter((edge) => edge.valid_from <= at && (edge.valid_to == null || at < edge.valid_to))
}

const topologyCapabilitySource = /^device:(\d+)\/(?:brctl\.showmacs|brctl\.showstp|ip-[46]-neigh): source call failure: (.+)$/i

function topologyCapabilityDeviceCount(gaps: string[]) {
  const devices = new Set<string>()
  for (const gap of gaps) {
    const match = gap.match(topologyCapabilitySource)
    if (match?.[2].split(', ').includes('access/permission denied')) devices.add(match[1])
  }
  return devices.size
}

export function Topology({ onReviewCapabilities }: { onReviewCapabilities?: () => void } = {}) {
  const [mode, setMode] = useState<Mode>('current')
  const [historyRange, setHistoryRange] = useState<HistoryRange>({ kind: 'preset', hours: 24 })
  const [rangeChoice, setRangeChoice] = useState<HistoryPreset>('24')
  const [customFrom, setCustomFrom] = useState('')
  const [customTo, setCustomTo] = useState('')
  const [customRangeError, setCustomRangeError] = useState('')
  const [loaded, setLoaded] = useState<{ query: string; data: TopologySnapshot; from: number; to: number } | null>(null)
  const [failure, setFailure] = useState<{ query: string; message: string } | null>(null)
  const [loadingQuery, setLoadingQuery] = useState<string | null>(null)
  const [historyAt, setHistoryAt] = useState(0)
  const [confidence, setConfidence] = useState<Confidence | 'all'>('all')
  const [medium, setMedium] = useState<Medium | 'all'>('all')
  const [vlan, setVLAN] = useState('all')
  const [zoom, setZoom] = useState(1)
  const [selectedNodeID, setSelectedNodeID] = useState<string | null>(null)
  const [selectedDeviceID, setSelectedDeviceID] = useState<number | null>(null)
  const generation = useRef(0)
  const rangeKey = historyRange.kind === 'preset'
    ? `preset:${historyRange.hours}`
    : `custom:${historyRange.from}:${historyRange.to}`
  const query = mode === 'current' ? 'current' : `history:${rangeKey}`

  const load = useCallback(async () => {
    const request = ++generation.current
    setLoadingQuery(query)
    try {
      const to = mode === 'history' && historyRange.kind === 'custom'
        ? historyRange.to
        : Date.now()
      const from = mode === 'history'
        ? historyRange.kind === 'custom'
          ? historyRange.from
          : to - historyRange.hours * hourMillis
        : to
      const next = mode === 'current' ? await api.topology() : await api.topologyHistory(from, to)
      if (request !== generation.current) return
      const loadedFrom = mode === 'current' ? next.at : from
      setLoaded({ query, data: next, from: loadedFrom, to })
      if (mode === 'history') setHistoryAt(Math.max(loadedFrom, to - 1))
      setFailure(null)
    } catch (e) {
      if (request !== generation.current) return
      setFailure({ query, message: e instanceof Error ? e.message : String(e) })
    } finally {
      if (request === generation.current) setLoadingQuery(null)
    }
  }, [historyRange, mode, query])

  useEffect(() => {
    void load()
    return () => { generation.current++ }
  }, [load])

  const data = loaded?.query === query ? loaded.data : null
  const bounds = loaded?.query === query ? loaded : null
  const error = failure?.query === query ? failure.message : ''
  const loading = loadingQuery === query

  const selectHistoryRange = (value: HistoryPreset) => {
    setRangeChoice(value)
    setCustomRangeError('')
    if (value !== 'custom') {
      setHistoryRange({ kind: 'preset', hours: Number(value) })
      return
    }
    if (!customFrom || !customTo) {
      const to = bounds?.to ?? Date.now()
      const from = bounds?.from ?? to - 24 * hourMillis
      setCustomFrom(datetimeLocalValue(from))
      setCustomTo(datetimeLocalValue(to))
    }
  }

  const applyCustomRange = () => {
    const from = new Date(customFrom).getTime()
    const to = new Date(customTo).getTime()
    if (!Number.isFinite(from) || !Number.isFinite(to)) {
      setCustomRangeError('Enter both the start and end of the custom range.')
      return
    }
    if (to <= from) {
      setCustomRangeError('Custom range must start before it ends.')
      return
    }
    if (to - from > maxHistoryMillis) {
      setCustomRangeError('Custom topology history cannot exceed 31 days.')
      return
    }
    setCustomRangeError('')
    setHistoryRange({ kind: 'custom', from, to })
  }

  const intervalEdges = useMemo(
    () => mode === 'history' && data ? topologyEdgesAt(data.edges, historyAt) : (data?.edges ?? []),
    [data, historyAt, mode],
  )
  const vlanOptions = useMemo(
    () => [...new Set((data?.edges ?? []).map(edgeVLAN))].sort((a, b) => a.localeCompare(b, undefined, { numeric: true })),
    [data],
  )
  const knownVLANs = vlanOptions.filter((value) => value !== 'unknown')
  const visibleEdges = useMemo(() => intervalEdges.filter((edge) =>
    (confidence === 'all' || edge.confidence === confidence)
      && (medium === 'all' || edge.medium === medium)
      && (vlan === 'all' || edgeVLAN(edge) === vlan),
  ), [confidence, intervalEdges, medium, vlan])
  const visibleNodeIDs = useMemo(() => {
    if (mode === 'current') {
      return new Set((data?.nodes ?? []).map((node) => node.id))
    }
    return new Set(visibleEdges.flatMap((edge) => [edge.child_id, edge.parent_id]))
  }, [confidence, data, medium, mode, visibleEdges, vlan])
  const visibleNodes = useMemo(
    () => (data?.nodes ?? []).filter((node) => visibleNodeIDs.has(node.id)),
    [data, visibleNodeIDs],
  )
  const nodeNameCounts = useMemo(() => {
    const counts = new Map<string, number>()
    for (const node of visibleNodes) counts.set(node.name, (counts.get(node.name) ?? 0) + 1)
    return counts
  }, [visibleNodes])

  const nodeByID = useMemo(
    () => new Map((data?.nodes ?? []).map((node) => [node.id, node])),
    [data],
  )
  const layout = useMemo(
    () => layoutTopology(visibleNodes, visibleEdges),
    [visibleEdges, visibleNodes],
  )
  const edgeOffsets = useMemo(() => edgeLaneOffsets(visibleEdges), [visibleEdges])
  const selectedNode = selectedNodeID ? nodeByID.get(selectedNodeID) : undefined
  const selectedNodeEdges = selectedNodeID
    ? (data?.edges ?? []).filter((edge) => edge.child_id === selectedNodeID || edge.parent_id === selectedNodeID)
    : []
  const capabilityDeviceCount = mode === 'current' && data
    ? topologyCapabilityDeviceCount(data.gaps)
    : 0

  return (
    <div style={{ display: 'grid', gap: 12 }}>
      <header style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12 }}>
        <div>
          <h1 style={{ margin: 0, fontSize: 20 }}>Topology</h1>
          <div style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            Infrastructure links with source provenance and historical intervals.
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', justifyContent: 'flex-end' }}>
          <Button aria-pressed={mode === 'current'} onClick={() => setMode('current')} kind={mode === 'current' ? 'primary' : 'default'}>
            Current
          </Button>
          <Button aria-pressed={mode === 'history'} onClick={() => setMode('history')} kind={mode === 'history' ? 'primary' : 'default'}>
            History
          </Button>
          {mode === 'history' && (
            <label style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
              <span style={{ marginRight: 6 }}>Range</span>
              <select
                aria-label="Topology history range"
                value={rangeChoice}
                onChange={(event) => selectHistoryRange(event.target.value as HistoryPreset)}
                style={{
                  height: 28,
                  borderRadius: 6,
                  border: '1px solid var(--border-strong)',
                  background: 'var(--surface-2)',
                  color: 'var(--text-primary)',
                }}
              >
                <option value={1}>1 hour</option>
                <option value={24}>24 hours</option>
                <option value={168}>7 days</option>
                <option value={744}>31 days</option>
                <option value="custom">Custom…</option>
              </select>
            </label>
          )}
          <Button onClick={() => void load()} disabled={loading}>Refresh</Button>
        </div>
      </header>

      {mode === 'history' && rangeChoice === 'custom' && (
        <fieldset style={{ display: 'flex', alignItems: 'end', gap: 8, flexWrap: 'wrap', margin: 0, padding: 10, border: '1px solid var(--border)', borderRadius: 8 }}>
          <legend style={{ color: 'var(--text-secondary)', fontSize: 12 }}>Custom topology history range (maximum 31 days)</legend>
          <label style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            From
            <input
              aria-label="Custom topology history start"
              type="datetime-local"
              value={customFrom}
              max={customTo || undefined}
              onChange={(event) => {
                setCustomFrom(event.target.value)
                setCustomRangeError('')
              }}
              style={{ display: 'block' }}
            />
          </label>
          <label style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            To
            <input
              aria-label="Custom topology history end"
              type="datetime-local"
              value={customTo}
              min={customFrom || undefined}
              onChange={(event) => {
                setCustomTo(event.target.value)
                setCustomRangeError('')
              }}
              style={{ display: 'block' }}
            />
          </label>
          <Button onClick={applyCustomRange}>Apply custom range</Button>
          {customRangeError && <div role="alert" style={{ color: 'var(--critical)', fontSize: 12 }}>{customRangeError}</div>}
        </fieldset>
      )}

      {error && (
        <Banner tone="critical">
          <div role="alert">
            {error} — {data ? 'showing the last topology that loaded successfully.' : 'no graph is available for this request.'}
          </div>
        </Banner>
      )}
      {capabilityDeviceCount > 0 && (
        <Banner tone="accent">
          <div role="status" style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, flexWrap: 'wrap' }}>
            <div>
              <strong>
                Some topology sources are unavailable on{' '}
                {capabilityDeviceCount} {capabilityDeviceCount === 1 ? 'router' : 'routers'}.
              </strong>{' '}
              Would you like to add this functionality? Optional controller access may restore
              bridge and neighbor evidence when access is the cause, and it never runs automatically.
            </div>
            {onReviewCapabilities && (
              <Button onClick={onReviewCapabilities}>Review optional capability</Button>
            )}
          </div>
        </Banner>
      )}
      {data && !data.complete && (
        <Banner>
          <div role="status">
            <strong>Topology is partial.</strong>{' '}
            {data.gaps.length > 0
              ? `${data.gaps.length} coverage ${data.gaps.length === 1 ? 'issue is' : 'issues are'} recorded. `
              : 'Source coverage is incomplete. '}
            Missing evidence is not treated as an empty network.
          </div>
          {data.gaps.length > 0 && (
            <details style={{ marginTop: 6 }}>
              <summary style={{ cursor: 'pointer', color: 'var(--text-secondary)' }}>
                Show technical details ({data.gaps.length})
              </summary>
              <ul style={{ margin: '6px 0 0', paddingLeft: 20, overflowWrap: 'anywhere' }}>
                {data.gaps.map((gap) => <li key={gap}>{gap}</li>)}
              </ul>
            </details>
          )}
        </Banner>
      )}
      {data?.truncated && (
        <Banner>
          <div role="status">
            Topology history reached its retained or response limit. The graph and interval details are truncated.
          </div>
        </Banner>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, minmax(0, 1fr))', gap: 10 }}>
        <Card><Stat label="Nodes" value={data?.nodes.length ?? '—'} /></Card>
        <Card><Stat label={mode === 'current' ? 'Active links' : 'Link intervals'} value={data?.edges.length ?? '—'} /></Card>
        <Card>
          <Stat
            label="Evidence coverage"
            value={data ? (data.complete ? 'Complete' : 'Partial') : '—'}
            tone={data?.complete ? 'good' : data ? 'warning' : 'muted'}
            sub={data ? `As of ${when(data.at)}` : undefined}
          />
        </Card>
      </div>

      <Card
        title={mode === 'current'
          ? 'Current infrastructure'
          : `Infrastructure at ${historyAt ? when(historyAt) : 'selected time'}`}
        actions={(
          <div aria-label="Topology confidence legend" style={{ display: 'flex', gap: 10, fontSize: 11, color: 'var(--text-secondary)' }}>
            <ConfidenceLegendItem confidence="measured" />
            <ConfidenceLegendItem confidence="inferred" />
            <ConfidenceLegendItem confidence="ambiguous" />
          </div>
        )}
      >
        {data && (
          <div aria-label="Topology filters" style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'end', marginBottom: 10 }}>
            <label style={{ color: 'var(--text-secondary)', fontSize: 11 }}>
              Confidence
              <select aria-label="Filter topology by confidence" value={confidence} onChange={(event) => setConfidence(event.target.value as Confidence | 'all')} style={{ display: 'block' }}>
                <option value="all">All</option>
                <option value="measured">Measured</option>
                <option value="inferred">Inferred</option>
                <option value="ambiguous">Ambiguous</option>
              </select>
            </label>
            <label style={{ color: 'var(--text-secondary)', fontSize: 11 }}>
              Medium
              <select aria-label="Filter topology by medium" value={medium} onChange={(event) => setMedium(event.target.value as Medium | 'all')} style={{ display: 'block' }}>
                <option value="all">All</option>
                {(['wired', 'wireless', 'mesh', 'uplink', 'unknown'] as Medium[]).map((value) => <option key={value} value={value}>{value}</option>)}
              </select>
            </label>
            {knownVLANs.length > 0 ? (
              <div aria-label="VLAN filter" style={{ display: 'flex', gap: 5, alignItems: 'center', flexWrap: 'wrap' }}>
                <span style={{ color: 'var(--text-secondary)', fontSize: 11 }}>VLAN</span>
                <Button aria-pressed={vlan === 'all'} kind={vlan === 'all' ? 'primary' : 'default'} onClick={() => setVLAN('all')}>All</Button>
                {vlanOptions.map((value) => (
                  <Button aria-pressed={vlan === value} key={value} kind={vlan === value ? 'primary' : 'default'} onClick={() => setVLAN(value)}>
                    {value === 'unknown' ? 'Unknown' : value}
                  </Button>
                ))}
              </div>
            ) : (
              <div role="note" style={{ color: 'var(--text-muted)', fontSize: 11 }}>
                VLAN evidence is unavailable; no VLAN path filter is shown.
              </div>
            )}
          </div>
        )}
        {mode === 'history' && data && bounds && (
          <label style={{ display: 'grid', gap: 4, marginBottom: 10, color: 'var(--text-secondary)', fontSize: 11 }}>
            Selected time: <strong style={{ color: 'var(--text-primary)' }}>{when(historyAt)}</strong>
            <input
              aria-label="Selected topology history time"
              type="range"
              min={bounds.from}
              max={Math.max(bounds.from, bounds.to - 1)}
              step={1000}
              value={historyAt}
              aria-valuetext={when(historyAt)}
              onChange={(event) => setHistoryAt(Number(event.target.value))}
            />
          </label>
        )}
        {loading && !data ? (
          <div role="status" style={{ color: 'var(--text-secondary)' }}>Loading topology…</div>
        ) : data && data.nodes.length > 0 ? (
          <div style={{ overflowX: 'auto', position: 'relative' }}>
            <svg
              aria-label={`${visibleNodes.length} topology nodes and ${visibleEdges.length} links`}
              role="group"
              viewBox={`0 0 ${layout.width} ${layout.height}`}
              style={{ width: `${zoom * 100}%`, minWidth: 620, maxHeight: 520 * zoom }}
            >
              {visibleEdges.map((edge) => {
                const parent = layout.positions.get(edge.parent_id)
                const child = layout.positions.get(edge.child_id)
                if (!parent || !child) return null
                const visual = edgeVisuals[edge.confidence]
                const bend = edgeOffsets.get(edge.id) ?? 0
                const midY = (parent.y + child.y) / 2
                const path = `M ${parent.x} ${parent.y + 25} C ${parent.x + bend} ${midY}, ${child.x + bend} ${midY}, ${child.x} ${child.y - 25}`
                return (
                  <g key={edge.id}>
                    <path
                      d={path}
                      fill="none"
                      stroke="var(--surface-1)"
                      strokeWidth={visual.width + 5}
                      strokeLinecap="round"
                    />
                    <path
                      d={path}
                      fill="none"
                      stroke={visual.stroke}
                      strokeWidth={visual.width}
                      strokeDasharray={visual.dash}
                      strokeLinecap="round"
                    />
                    {edge.parent_port && (
                      <text
                        x={(parent.x + child.x) / 2 + bend}
                        y={midY - 6}
                        textAnchor="middle"
                        fill="var(--text-primary)"
                        stroke="var(--surface-1)"
                        strokeWidth="4"
                        paintOrder="stroke"
                        strokeLinejoin="round"
                        fontSize="11"
                        fontWeight="600"
                      >
                        {edge.parent_port}
                      </text>
                    )}
                  </g>
                )
              })}
              {visibleNodes.map((node) => {
                const pos = layout.positions.get(node.id)
                if (!pos) return null
                return (
                  <g
                    key={node.id}
                    transform={`translate(${pos.x},${pos.y})`}
                    role="button"
                    tabIndex={0}
                    aria-label={`Open details for ${node.name}${(nodeNameCounts.get(node.name) ?? 0) > 1 ? ` (${node.id})` : ''}`}
                    onClick={() => node.device_id != null ? setSelectedDeviceID(node.device_id) : setSelectedNodeID(node.id)}
                    onKeyDown={(event) => {
                      if (event.key === 'Enter' || event.key === ' ') {
                        event.preventDefault()
                        if (node.device_id != null) setSelectedDeviceID(node.device_id)
                        else setSelectedNodeID(node.id)
                      }
                    }}
                    style={{ cursor: 'pointer' }}
                  >
                    <rect
                      x="-70" y="-25" width="140" height="50" rx="8"
                      fill={node.synthetic ? 'var(--accent-soft)' : 'var(--surface-2)'}
                      stroke={node.online === false ? 'var(--critical)' : 'var(--border-strong)'}
                      strokeDasharray={node.kind === 'client' ? '4 3' : undefined}
                    />
                    <text textAnchor="middle" y="-2" fill="var(--text-primary)" fontSize="12" fontWeight="600">
                      {node.name.length > 20 ? `${node.name.slice(0, 18)}…` : node.name}
                    </text>
                    <text textAnchor="middle" y="15" fill="var(--text-secondary)" fontSize="10">
                      {node.kind}{node.online === false ? ' · offline' : ''}
                    </text>
                  </g>
                )
              })}
            </svg>
            <div aria-label="Topology zoom controls" style={{ display: 'flex', gap: 5, position: 'sticky', left: 8, bottom: 8, width: 'max-content' }}>
              <Button aria-label="Zoom out topology" onClick={() => setZoom((value) => Math.max(.75, value - .25))}>−</Button>
              <Button aria-label="Reset topology zoom" onClick={() => setZoom(1)}>{Math.round(zoom * 100)}%</Button>
              <Button aria-label="Zoom in topology" onClick={() => setZoom((value) => Math.min(2, value + .25))}>+</Button>
            </div>
          </div>
        ) : data?.complete ? (
          <div>No topology nodes were observed.</div>
        ) : (
          <div>Topology is unknown until at least one source can be observed.</div>
        )}
      </Card>

      {selectedDeviceID != null && (
        <DeviceDetailPanel
          id={selectedDeviceID}
          onClose={() => setSelectedDeviceID(null)}
          onChanged={() => {}}
          onRemoved={() => setSelectedDeviceID(null)}
        />
      )}

      {selectedNode && (
        <Card
          title={selectedNode.name}
          actions={<Button onClick={() => setSelectedNodeID(null)}>Close details</Button>}
        >
          <div role="region" aria-label={`Topology details for ${selectedNode.name}`} style={{ display: 'grid', gap: 6 }}>
            <div>{selectedNode.kind}{selectedNode.online === false ? ' · offline' : selectedNode.online ? ' · online' : ' · status unknown'}</div>
            <div style={{ color: 'var(--text-secondary)' }}>{selectedNode.id}</div>
            <div>{selectedNodeEdges.length} observed link interval{selectedNodeEdges.length === 1 ? '' : 's'}</div>
          </div>
        </Card>
      )}

      <Card title="Accessible topology details" pad={false}>
        <TopologyEdgeTable
          edges={visibleEdges}
          nodes={nodeByID}
          caption={mode === 'current' ? 'Matching active parent-child links' : `Links active at ${when(historyAt)}`}
          empty={data?.edges.length
            ? 'No links match the selected time and filters.'
            : data?.complete ? 'No links in this interval.' : 'Links unknown because required evidence is unavailable.'}
        />
      </Card>

      {mode === 'history' && data && (
        <Card title="Complete interval list for this request" pad={false}>
          <TopologyEdgeTable
            edges={data.edges}
            nodes={nodeByID}
            caption="All link intervals intersecting the selected range"
            empty={data.complete ? 'No links in this range.' : 'Links unknown because required evidence is unavailable.'}
          />
        </Card>
      )}
    </div>
  )
}

function TopologyEdgeTable({ edges, nodes, caption, empty }: {
  edges: TopologyEdge[]
  nodes: Map<string, TopologyNode>
  caption: string
  empty: string
}) {
  return (
    <div style={{ overflowX: 'auto' }}>
      <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
        <caption style={{ textAlign: 'left', padding: '10px 14px', color: 'var(--text-secondary)' }}>
          {caption}
        </caption>
        <thead>
          <tr style={{ textAlign: 'left', borderTop: '1px solid var(--border)', borderBottom: '1px solid var(--border)' }}>
            {['Child', 'Parent', 'Port', 'Medium', 'Confidence', 'Interval', 'Evidence'].map((label) => (
              <th key={label} scope="col" style={{ padding: '8px 10px' }}>{label}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {edges.map((edge) => (
            <tr key={edge.id} style={{ borderBottom: '1px solid var(--border)' }}>
              <td style={{ padding: '8px 10px' }}>{nodeLabel(nodes, edge.child_id)}</td>
              <td style={{ padding: '8px 10px' }}>{nodeLabel(nodes, edge.parent_id)}</td>
              <td style={{ padding: '8px 10px' }}>{edge.parent_port || 'Unknown'}</td>
              <td style={{ padding: '8px 10px' }}>{edge.medium}</td>
              <td style={{ padding: '8px 10px' }}><Status value={edge.confidence} /></td>
              <td className="num" style={{ padding: '8px 10px', textAlign: 'left', whiteSpace: 'nowrap' }}>
                {when(edge.valid_from)} → {when(edge.valid_to)}
              </td>
              <td style={{ padding: '8px 10px', minWidth: 220 }}>
                <details>
                  <summary>{edge.evidence.length} source{edge.evidence.length === 1 ? '' : 's'}{edge.ambiguities.length ? ` · ${edge.ambiguities.length} ${edge.ambiguities.length === 1 ? 'ambiguity' : 'ambiguities'}` : ''}</summary>
                  <ul style={{ margin: '6px 0', paddingLeft: 18 }}>
                    {edge.evidence.map((evidence, index) => (
                      <li key={`${evidence.source}-${evidence.kind}-${index}`}>
                        {evidence.kind} via {evidence.source}
                        {Object.keys(evidence.detail).length > 0 ? ` · ${detailText(evidence.detail)}` : ''}
                      </li>
                    ))}
                    {edge.ambiguities.map((ambiguity) => <li key={ambiguity}>Ambiguous: {ambiguity}</li>)}
                  </ul>
                </details>
              </td>
            </tr>
          ))}
          {edges.length === 0 && (
            <tr><td colSpan={7} style={{ padding: 16, color: 'var(--text-secondary)' }}>{empty}</td></tr>
          )}
        </tbody>
      </table>
    </div>
  )
}
