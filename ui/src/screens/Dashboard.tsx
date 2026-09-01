import { useCallback, useEffect, useId, useRef, useState } from 'react'
import { api } from '../lib/api'
import type {
  Dashboard as DashboardData,
  DashboardMetric,
  DashboardMetricPoint,
  DashboardWAN,
  SpeedTestCollection,
  SpeedTestJob,
  TopologySnapshot,
} from '../lib/api'
import { eventLabel, ipv6RACondition } from '../lib/eventCondition'
import { Banner, Button, Card, Notice, Stat, Status, Unknown } from '../components/ui'
import { ago } from '../components/Chart'

function formatRate(value: number, unit?: string) {
  const bitsPerSecond = unit === 'B/s' ? value * 8 : value
  if (bitsPerSecond >= 1_000_000_000) return `${(bitsPerSecond / 1_000_000_000).toFixed(1)} Gbps`
  if (bitsPerSecond >= 1_000_000) return `${(bitsPerSecond / 1_000_000).toFixed(1)} Mbps`
  if (bitsPerSecond >= 1_000) return `${(bitsPerSecond / 1_000).toFixed(0)} Kbps`
  return `${bitsPerSecond.toFixed(0)} bps`
}

function formatBytes(value: number) {
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)} GB`
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(0)} MB`
  if (value >= 1_000) return `${(value / 1_000).toFixed(0)} KB`
  return `${value} B`
}

function validTestValue(value: number | null | undefined): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
}

function formatTestRate(value: number | null | undefined) {
  return validTestValue(value) ? `${value.toFixed(1)} Mbps` : '—'
}

function formatMilliseconds(value: number | null | undefined) {
  return validTestValue(value) ? `${value.toFixed(1)} ms` : '—'
}

export function speedTestScale(jobs: SpeedTestJob[]) {
  const values = jobs.flatMap((job) => [job.download_mbps, job.upload_mbps])
    .filter(validTestValue)
  if (values.length === 0) return 1
  const peak = Math.max(...values)
  if (peak === 0) return 1
  const target = peak * 1.1
  const magnitude = 10 ** Math.floor(Math.log10(target))
  const normalized = target / magnitude
  const factor = [1, 1.25, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10]
    .find((candidate) => candidate >= normalized) ?? 10
  return factor * magnitude
}

function errorText(error: unknown) {
  return error instanceof Error ? error.message : String(error)
}

function agoMilliseconds(timestamp: number | null | undefined) {
  return timestamp ? ago(Math.floor(timestamp / 1_000)) : 'never'
}

function speedTestProvenance(value: string | null | undefined) {
  return value === 'controller-host'
    ? 'Controller host/container (controller-host)'
    : value || 'Controller host/container'
}

type WANTrendTone = 'download' | 'upload' | 'latency' | 'loss'

function validWANValue(kind: string, value: number | null | undefined): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 &&
    (!kind.endsWith('loss_pct') || value <= 100)
}

export function wanTrendCeiling(kind: string, values: number[]) {
  if (kind.endsWith('loss_pct')) return 100
  const peak = Math.max(0, ...values)
  if (peak === 0) return kind.endsWith('latency_ms') ? 10 : 1
  const target = peak * 1.1
  const magnitude = 10 ** Math.floor(Math.log10(target))
  const normalized = target / magnitude
  const factor = [1, 1.25, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10]
    .find((candidate) => candidate >= normalized) ?? 10
  return factor * magnitude
}

function missingRuns(points: DashboardMetricPoint[], kind: string) {
  const runs: Array<{ from: number; to: number }> = []
  let from = -1
  points.forEach((point, index) => {
    if (!validWANValue(kind, point.value)) {
      if (from < 0) from = index
    } else if (from >= 0) {
      runs.push({ from, to: index - 1 })
      from = -1
    }
  })
  if (from >= 0) runs.push({ from, to: points.length - 1 })
  return runs
}

export function Trend({
  label,
  points,
  kind = 'download_bps',
  unit = 'bps',
  tone = 'download',
  format = formatRate,
}: {
  label: string
  points: DashboardMetricPoint[]
  kind?: string
  unit?: string
  tone?: WANTrendTone
  format?: (value: number, unit?: string) => string
}) {
  const observed = points.flatMap((point) => validWANValue(kind, point.value) ? [point.value] : [])
  const missing = points.length - observed.length
  if (observed.length === 0) {
    return (
      <div className="dashboard-trend-empty" role="img" aria-label={`${label}: no samples available in the past six hours`}>
        No samples in the past 6 hours
      </div>
    )
  }

  const width = 320
  const plotTop = 10
  const plotBottom = 76
  const railY = 84
  const ceiling = wanTrendCeiling(kind, observed)
  const slot = width / Math.max(1, points.length)
  const barWidth = Math.max(1, slot * 0.68)
  const gaps = missingRuns(points, kind)

  return (
    <figure className="dashboard-trend-figure" data-tone={tone}>
      <div className="dashboard-trend-scale" aria-hidden>
        <span>{format(ceiling, unit)}</span>
        <span>0</span>
      </div>
      <svg
        className="dashboard-trend"
        viewBox={`0 0 ${width} 90`}
        role="img"
        aria-label={`${label}, past six hours in five-minute averages: ${observed.length} available and ${missing} unavailable samples; zero-based scale to ${format(ceiling, unit)}`}
        preserveAspectRatio="none"
      >
        <line className="dashboard-trend-guide" x1="0" x2={width} y1={plotTop} y2={plotTop} />
        <line className="dashboard-trend-guide" x1="0" x2={width} y1={(plotTop + plotBottom) / 2} y2={(plotTop + plotBottom) / 2} />
        <line className="dashboard-trend-baseline" x1="0" x2={width} y1={plotBottom} y2={plotBottom} />
        {points.map((point, index) => {
          if (!validWANValue(kind, point.value)) return null
          const scaled = point.value / ceiling
          const height = point.value === 0 ? 2 : Math.max(2, scaled * (plotBottom - plotTop))
          return (
            <rect
              key={`${point.ts}-${index}`}
              className="dashboard-trend-bar"
              data-zero={point.value === 0 ? 'true' : undefined}
              x={index * slot + (slot - barWidth) / 2}
              y={plotBottom - height}
              width={barWidth}
              height={height}
            />
          )
        })}
        <rect className="dashboard-trend-coverage" x="0" y={railY} width={width} height="4" />
        {gaps.map(({ from, to }) => (
          <rect
            key={`${from}-${to}`}
            className="dashboard-trend-gap"
            x={from * slot}
            y={railY}
            width={(to - from + 1) * slot}
            height="4"
          />
        ))}
      </svg>
      <figcaption className="dashboard-trend-caption">
        <span>6h ago</span>
        <span>{observed.length}/{points.length} samples</span>
        <span>Now</span>
      </figcaption>
    </figure>
  )
}

export function WANMetric({
  label,
  metric,
  format,
  tone,
}: {
  label: string
  metric?: DashboardMetric
  format: (value: number, unit?: string) => string
  tone: WANTrendTone
}) {
  const available = validWANValue(metric?.kind ?? '', metric?.value) && metric?.status !== 'unavailable'
  const note = [
    metric?.status === 'fresh' && metric.as_of ? `Updated ${agoMilliseconds(metric.as_of)}` : '',
    metric?.status === 'last_observed' && metric.as_of ? `Last observed ${agoMilliseconds(metric.as_of)}` : '',
    !metric || metric.status === 'unavailable' ? 'Current value unavailable' : '',
  ].filter(Boolean).join(' · ')
  return (
    <div className="dashboard-metric" data-tone={tone}>
      <div className="dashboard-metric-heading">
        <div className="dashboard-metric-label">{label}</div>
        <span>Latest 5m average</span>
      </div>
      <div className="dashboard-metric-value num">
        {available
          ? format(metric.value!, metric.unit)
          : <Unknown why={metric?.meaning || 'the controller has no matching WAN telemetry'} />}
      </div>
      {metric && (
        <Trend
          label={label}
          points={metric.points}
          kind={metric.kind}
          unit={metric.unit}
          tone={tone}
          format={format}
        />
      )}
      <div className="dashboard-metric-note">{note}</div>
    </div>
  )
}

function mergeSpeedTest(current: SpeedTestCollection | null, job: SpeedTestJob) {
  if (!current) return null
  const jobs = [job, ...current.jobs.filter((item) => item.id !== job.id)]
  return { ...current, jobs, active: job.state === 'completed' || job.state === 'failed' ? null : job }
}

function SpeedTestRateBar({
  job,
  label,
  series,
  value,
  scale,
}: {
  job: SpeedTestJob
  label: 'Download' | 'Upload'
  series: 'download' | 'upload'
  value: number | null | undefined
  scale: number
}) {
  const available = validTestValue(value)
  const tooltipID = `speedtest-${job.id}-${series}`
  const time = new Date(job.finished_at ?? job.created_at).toLocaleString()
  return (
    <div className="speedtest-bar-row">
      <span className="speedtest-bar-label">{label}</span>
      <span className="speedtest-bar-wrap">
        {available ? (
          <span
            className="speedtest-bar-track"
            role="meter"
            tabIndex={0}
            aria-label={`${label} throughput`}
            aria-valuemin={0}
            aria-valuemax={scale}
            aria-valuenow={value}
            aria-valuetext={`${value.toFixed(1)} Mbps`}
            aria-describedby={tooltipID}
          >
            <span
              className="speedtest-bar-fill"
              data-series={series}
              style={{ width: `${(value / scale) * 100}%` }}
            />
          </span>
        ) : (
          <span className="speedtest-bar-track" data-unavailable="true" aria-label={`${label} throughput unavailable`} />
        )}
        {available && (
          <span className="speedtest-bar-tooltip" id={tooltipID} role="tooltip">
            {label} {value.toFixed(1)} Mbps · {time} · {job.provider} · {speedTestProvenance(job.provenance)}
          </span>
        )}
      </span>
      <span className="speedtest-bar-value num">{formatTestRate(value)}</span>
    </div>
  )
}

function SpeedTestResponsiveness({ job }: { job: SpeedTestJob }) {
  const idleAvailable = validTestValue(job.idle_latency_ms) || validTestValue(job.idle_jitter_ms)
  const loadedAvailable = validTestValue(job.loaded_latency_ms) || validTestValue(job.loaded_jitter_ms)
  return (
    <div className="speedtest-responsiveness">
      <span>
        Idle {idleAvailable
          ? `${formatMilliseconds(job.idle_latency_ms)} latency · ${formatMilliseconds(job.idle_jitter_ms)} jitter`
          : 'unavailable'}
      </span>
      <span>
        Loaded {loadedAvailable
          ? `${formatMilliseconds(job.loaded_latency_ms)} latency · ${formatMilliseconds(job.loaded_jitter_ms)} jitter`
          : 'unavailable'}
      </span>
    </div>
  )
}

export function SpeedTestHistory({ jobs }: { jobs: SpeedTestJob[] }) {
  const [view, setView] = useState<'chart' | 'table'>('chart')
  const charted = jobs.filter((job) => job.state === 'completed' && (
    validTestValue(job.download_mbps) || validTestValue(job.upload_mbps)
  ))
  const uncharted = jobs.filter((job) => !charted.includes(job))
  const scale = speedTestScale(charted)

  return (
    <div className="speedtest-history">
      <div className="speedtest-history-toolbar">
        <div>
          <div className="speedtest-history-heading">Recent performance</div>
          <div className="dashboard-metric-note">Up to three recent attempts · controller host/container</div>
        </div>
        <div className="speedtest-view-toggle" role="group" aria-label="Speed test history view">
          <Button aria-pressed={view === 'chart'} kind={view === 'chart' ? 'primary' : 'default'} onClick={() => setView('chart')}>
            Chart
          </Button>
          <Button aria-pressed={view === 'table'} kind={view === 'table' ? 'primary' : 'default'} onClick={() => setView('table')}>
            Table
          </Button>
        </div>
      </div>

      {view === 'chart' ? (
        charted.length === 0 ? (
          <div className="speedtest-chart-empty" role="img" aria-label="No completed speed tests have chartable throughput results">
            No completed throughput results to chart.
          </div>
        ) : (
          <figure
            className="speedtest-chart"
            aria-label={`Throughput history for ${charted.length} completed test${charted.length === 1 ? '' : 's'}, shared scale zero to ${scale} megabits per second`}
          >
            <div className="speedtest-chart-legend" aria-label="Throughput series legend">
              <span><i data-series="download" aria-hidden />Download</span>
              <span><i data-series="upload" aria-hidden />Upload</span>
            </div>
            <div className="speedtest-chart-scale" aria-hidden>
              <span>0</span>
              <span>{scale.toFixed(scale < 10 ? 1 : 0)} Mbps</span>
            </div>
            <div className="speedtest-chart-runs">
              {charted.map((job) => (
                <section className="speedtest-chart-run" key={job.id} aria-label={`Completed test from ${new Date(job.finished_at ?? job.created_at).toLocaleString()}`}>
                  <header>
                    <strong>{agoMilliseconds(job.finished_at ?? job.created_at)}</strong>
                    <span>{job.provider} · {job.method}</span>
                  </header>
                  <SpeedTestRateBar job={job} label="Download" series="download" value={job.download_mbps} scale={scale} />
                  <SpeedTestRateBar job={job} label="Upload" series="upload" value={job.upload_mbps} scale={scale} />
                  <SpeedTestResponsiveness job={job} />
                </section>
              ))}
            </div>
          </figure>
        )
      ) : (
        <div className="speedtest-table-wrap" role="region" aria-label="Speed test result table" tabIndex={0}>
          <table className="speedtest-table">
            <thead>
              <tr>
                <th>Test</th>
                <th>Status</th>
                <th>Download</th>
                <th>Upload</th>
                <th>Idle latency</th>
                <th>Idle jitter</th>
                <th>Loaded latency</th>
                <th>Loaded jitter</th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((job) => (
                <tr key={job.id}>
                  <td>
                    <time dateTime={new Date(job.finished_at ?? job.created_at).toISOString()}>
                      {new Date(job.finished_at ?? job.created_at).toLocaleString()}
                    </time>
                    <small>{job.provider} · {job.method} · {speedTestProvenance(job.provenance)}</small>
                  </td>
                  <td>{job.state}{job.error ? <small>{job.error}</small> : null}</td>
                  <td className="num">{formatTestRate(job.download_mbps)}</td>
                  <td className="num">{formatTestRate(job.upload_mbps)}</td>
                  <td className="num">{formatMilliseconds(job.idle_latency_ms)}</td>
                  <td className="num">{formatMilliseconds(job.idle_jitter_ms)}</td>
                  <td className="num">{formatMilliseconds(job.loaded_latency_ms)}</td>
                  <td className="num">{formatMilliseconds(job.loaded_jitter_ms)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {view === 'chart' && uncharted.map((job) => (
        <div className="speedtest-uncharted" key={job.id}>
          <Status value={job.state} />
          <span>{job.error || 'Throughput result unavailable'} · {agoMilliseconds(job.finished_at ?? job.created_at)}</span>
        </div>
      ))}
    </div>
  )
}

function SpeedTestImpact({
  plan,
  disclosure,
}: {
  plan: SpeedTestCollection['test'] | undefined
  disclosure: SpeedTestCollection['disclosure'] | undefined
}) {
  const [open, setOpen] = useState(false)
  const regionRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const panelRef = useRef<HTMLDivElement>(null)
  const closeRef = useRef<HTMLButtonElement>(null)
  const openMode = useRef<'mouse' | 'persistent' | null>(null)
  const pointerType = useRef('')
  const previousPlanID = useRef(plan?.plan_id)
  const panelID = useId()
  const titleID = useId()
  const supportsPopover = typeof HTMLElement !== 'undefined' &&
    typeof HTMLElement.prototype.showPopover === 'function'

  const close = useCallback((restoreFocus = false) => {
    setOpen(false)
    openMode.current = null
    if (restoreFocus) window.requestAnimationFrame(() => triggerRef.current?.focus())
  }, [])

  useEffect(() => {
    try {
      if (open) panelRef.current?.showPopover?.()
      else panelRef.current?.hidePopover?.()
    } catch {
      // The panel is still rendered as a fixed-position fallback.
    }
    if (open && openMode.current === 'persistent' && pointerType.current === '') {
      window.requestAnimationFrame(() => closeRef.current?.focus())
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: PointerEvent) => {
      if (!regionRef.current?.contains(event.target as Node)) close()
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      event.preventDefault()
      close(true)
    }
    document.addEventListener('pointerdown', onPointerDown, true)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('pointerdown', onPointerDown, true)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [close, open])

  useEffect(() => {
    if (previousPlanID.current === plan?.plan_id) return
    previousPlanID.current = plan?.plan_id
    if (open) close()
  }, [close, open, plan?.plan_id])

  const data = plan?.estimated_bytes && plan.estimated_bytes > 0
    ? `About ${formatBytes(plan.estimated_bytes)} plus protocol overhead`
    : 'Data estimate unavailable'
  const duration = plan?.max_duration_seconds && plan.max_duration_seconds > 0
    ? `Up to ${plan.max_duration_seconds} seconds`
    : 'Duration unavailable'

  return (
    <div
      ref={regionRef}
      className="speedtest-impact"
      onPointerLeave={(event) => {
        if (event.pointerType === 'mouse' && openMode.current !== 'persistent') close()
      }}
    >
      <button
        ref={triggerRef}
        type="button"
        className="speedtest-impact-trigger"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={panelID}
        onPointerDown={(event) => { pointerType.current = event.pointerType }}
        onClick={(event) => {
          if (open) {
            close()
            return
          }
          const persistent = event.detail === 0 || pointerType.current !== 'mouse'
          openMode.current = persistent ? 'persistent' : 'mouse'
          if (event.detail === 0) pointerType.current = ''
          setOpen(true)
        }}
      >
        Impact &amp; consent
      </button>
      <div
        ref={panelRef}
        id={panelID}
        className="speedtest-impact-popover"
        role="dialog"
        aria-modal="false"
        aria-labelledby={titleID}
        popover={supportsPopover ? 'manual' : undefined}
        hidden={!supportsPopover && !open}
        onFocusCapture={() => { openMode.current = 'persistent' }}
      >
        <div className="speedtest-impact-heading">
          <strong id={titleID}>Speed test impact &amp; consent</strong>
          <button
            ref={closeRef}
            type="button"
            className="speedtest-impact-close"
            aria-label="Close speed test impact and consent"
            onClick={() => close(true)}
          >
            ×
          </button>
        </div>
        <dl className="speedtest-plan">
          <div><dt>Vantage point</dt><dd>{disclosure?.vantage_point ? speedTestProvenance(disclosure.vantage_point) : 'Unavailable'}</dd></div>
          <div><dt>Provider</dt><dd>{plan?.provider || 'Unavailable'}</dd></div>
          <div><dt>Provider origin</dt><dd>{plan?.endpoint || 'Unavailable'}</dd></div>
          <div><dt>Method</dt><dd>{plan?.method || 'Unavailable'}</dd></div>
          <div><dt>Data</dt><dd>{data}</dd></div>
          <div><dt>Duration</dt><dd>{duration}</dd></div>
          <div><dt>Download endpoint</dt><dd>{plan?.download_endpoint || 'Unavailable'}</dd></div>
          <div><dt>Upload endpoint</dt><dd>{plan?.upload_endpoint || 'Unavailable'}</dd></div>
          <div><dt>Router management calls</dt><dd><code>{typeof disclosure?.router_management_calls === 'boolean' ? String(disclosure.router_management_calls) : 'Unavailable'}</code></dd></div>
          <div><dt>Router changes</dt><dd><code>{typeof disclosure?.router_changes === 'boolean' ? String(disclosure.router_changes) : 'Unavailable'}</code></dd></div>
        </dl>
        <p>{disclosure?.saturation_warning || 'Connection-impact disclosure unavailable.'}</p>
        <p>{disclosure?.privacy || 'Public-IP disclosure unavailable.'}</p>
        <p className="dashboard-metric-note">
          Selecting Run speed test acknowledges this data use and possible temporary saturation.
        </p>
      </div>
    </div>
  )
}

function SpeedTestCard() {
  const [collection, setCollection] = useState<SpeedTestCollection | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const refreshGeneration = useRef(0)
  const refreshInFlight = useRef<Promise<SpeedTestCollection | null> | null>(null)
  const startInFlight = useRef(false)
  const planID = collection?.test?.plan_id ?? ''
  const consequenceID = useId()

  const refresh = useCallback(async () => {
    if (refreshInFlight.current) return refreshInFlight.current
    const generation = ++refreshGeneration.current
    const request = api.speedTests(3).then((next) => {
      if (generation === refreshGeneration.current) {
        setCollection(next)
        setError('')
      }
      return next
    }).catch((cause) => {
      if (generation === refreshGeneration.current) setError(errorText(cause))
      return null
    }).finally(() => {
      if (refreshInFlight.current === request) refreshInFlight.current = null
    })
    refreshInFlight.current = request
    return request
  }, [])

  useEffect(() => {
    void refresh()
    return () => {
      refreshGeneration.current++
      refreshInFlight.current = null
    }
  }, [refresh])

  useEffect(() => {
    if (!collection?.active) return
    const timer = window.setInterval(refresh, 1_000)
    return () => window.clearInterval(timer)
  }, [collection?.active?.id, refresh])

  const start = async () => {
    if (startInFlight.current || busy || !planReady || !planID || collection?.active) return
    startInFlight.current = true
    refreshGeneration.current++
    refreshInFlight.current = null
    setBusy(true)
    setError('')
    try {
      const job = await api.startSpeedTest(planID, true)
      refreshGeneration.current++
      refreshInFlight.current = null
      setCollection((current) => mergeSpeedTest(current, job))
      void refresh()
    } catch (cause) {
      setError(errorText(cause))
      void refresh()
    } finally {
      startInFlight.current = false
      setBusy(false)
    }
  }

  const cancel = async () => {
    const active = collection?.active
    if (!active || active.state === 'cancelling') return
    refreshGeneration.current++
    refreshInFlight.current = null
    setBusy(true)
    setError('')
    try {
      const job = await api.cancelSpeedTest(active.id)
      refreshGeneration.current++
      refreshInFlight.current = null
      setCollection((current) => mergeSpeedTest(current, job))
      void refresh()
    } catch (cause) {
      setError(errorText(cause))
      void refresh()
    } finally {
      setBusy(false)
    }
  }

  const active = collection?.active
  const activeProgress = active ? Math.max(0, Math.min(100, active.progress_percent)) : 0
  const plan = collection?.test
  const disclosure = collection?.disclosure
  const planReady = Boolean(
    plan?.plan_id && plan.provider && plan.method && plan.provenance === 'controller-host' &&
    plan.endpoint && plan.download_endpoint && plan.upload_endpoint &&
    plan.estimated_bytes > 0 && plan.max_duration_seconds > 0 &&
    disclosure?.vantage_point === 'controller-host' &&
    disclosure.router_management_calls === false && disclosure.router_changes === false &&
    disclosure.privacy?.trim() && disclosure.saturation_warning?.trim(),
  )
  const maxDuration = plan?.max_duration_seconds
  const estimatedBytes = plan?.estimated_bytes
  const history = (collection?.jobs ?? []).filter((job) => !active || job.id !== active.id).slice(0, 3)
  const provider = plan?.provider || 'Provider unavailable'
  const providerRoute = plan?.provider && plan.endpoint
    ? `${plan.provider} via ${plan.endpoint}`
    : 'provider or endpoint unavailable'
  const vantage = disclosure?.vantage_point === 'controller-host'
    ? 'controller host'
    : 'vantage point unavailable'
  const data = estimatedBytes && estimatedBytes > 0
    ? `~${formatBytes(estimatedBytes)} + overhead`
    : 'data estimate unavailable'
  const duration = maxDuration && maxDuration > 0
    ? `up to ${maxDuration} seconds`
    : 'duration unavailable'
  const saturation = disclosure?.saturation_warning?.trim()
    ? 'may temporarily saturate WAN'
    : 'saturation disclosure unavailable'
  const privacy = disclosure?.privacy?.trim()
    ? `public IP visible to ${provider}`
    : 'public-IP disclosure unavailable'
  const routerImpact = disclosure?.router_management_calls === false && disclosure.router_changes === false
    ? 'no router calls or changes'
    : 'router safety unavailable'

  return (
    <Card
      title="Internet speed test"
      actions={<span className="dashboard-provenance">Controller host/container</span>}
    >
      {active ? (
        <div className="speedtest-active" aria-live="polite">
          <div className="speedtest-active-heading">
            <div>
              <strong>{active.state === 'cancelling' ? 'Cancelling test…' : 'Test in progress'}</strong>
              <div className="dashboard-metric-note">
                {active.provider} · {active.method} · {speedTestProvenance(active.provenance)}
                {' '}· {active.phase || active.state}
                {active.endpoint ? ` · ${active.endpoint}` : ''}
              </div>
            </div>
            <Button disabled={busy || active.state === 'cancelling'} onClick={cancel}>
              {active.state === 'cancelling' ? 'Cancelling…' : 'Cancel'}
            </Button>
          </div>
          <div
            className="speedtest-progress"
            role="progressbar"
            aria-label="Controller speed test progress"
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={activeProgress}
            aria-valuetext={`${active.phase || active.state}, ${activeProgress}%`}
            data-determinate={activeProgress > 0}
          >
            <span style={activeProgress > 0 ? { width: `${activeProgress}%` } : undefined} />
          </div>
          <div className="speedtest-live-metrics">
            <Stat label="Download" value={formatTestRate(active.download_mbps)} />
            <Stat label="Upload" value={formatTestRate(active.upload_mbps)} />
            <Stat label="Idle latency" value={formatMilliseconds(active.idle_latency_ms)} />
            <Stat label="Idle jitter" value={formatMilliseconds(active.idle_jitter_ms)} />
            <Stat label="Loaded latency" value={formatMilliseconds(active.loaded_latency_ms)} />
            <Stat label="Loaded jitter" value={formatMilliseconds(active.loaded_jitter_ms)} />
          </div>
          <div className="dashboard-metric-note">
            {formatBytes(active.bytes_downloaded + active.bytes_uploaded)} transferred
          </div>
        </div>
      ) : (
        <div className="speedtest-launch" role="group" aria-label="Controller speed test">
          <div id={consequenceID} className="speedtest-launch-consequence">
            {providerRoute} · {vantage} · {data} · {duration} · {routerImpact} · {saturation} · {privacy}
          </div>
          {!planReady && (
            <div className="speedtest-plan-unavailable" role="status">
              Exact provider, endpoints, method, provenance, limits, and safety disclosures are unavailable or incomplete; Run remains disabled.
            </div>
          )}
          <div className="speedtest-launch-actions">
            <SpeedTestImpact plan={plan} disclosure={disclosure} />
            <Button
              kind="primary"
              disabled={busy || !planReady}
              aria-describedby={consequenceID}
              onClick={start}
            >
              {busy ? 'Starting…' : 'Run speed test'}
            </Button>
          </div>
        </div>
      )}

      {error && (
        <div className="speedtest-error" role="alert">
          <span>Speed test unavailable: {error}</span>
          <Button disabled={busy} onClick={() => void refresh()}>Retry</Button>
        </div>
      )}

      {history.length === 0 ? (
        <div className="speedtest-history">
          <div className="speedtest-history-heading">Recent performance</div>
          <div className="dashboard-metric-note">
            {collection == null && !error ? 'Loading history…' : 'No tests yet.'}
          </div>
        </div>
      ) : <SpeedTestHistory jobs={history} />}
    </Card>
  )
}

function WANHistoryTable({ wan }: { wan: DashboardWAN }) {
  const metrics = [
    wan.metrics.download_bps,
    wan.metrics.upload_bps,
    wan.metrics.latency_ms,
    wan.metrics.loss_pct,
  ]
  const timestamps = [...new Set(metrics.flatMap((metric) => metric.points.map((point) => point.ts)))]
    .sort((left, right) => left - right)
  const values = metrics.map((metric) => new Map(metric.points.map((point) => [point.ts, point.value])))
  const cell = (metric: DashboardMetric, value: number | null | undefined, format: (value: number, unit?: string) => string) =>
    validWANValue(metric.kind, value) ? format(value, metric.unit) : 'Unavailable'

  return (
    <div className="dashboard-wan-table-wrap" role="region" aria-label="Internet health history table" tabIndex={0}>
      <table className="speedtest-table dashboard-wan-table">
        <thead>
          <tr>
            <th>Time</th>
            <th>Download</th>
            <th>Upload</th>
            <th>Latency</th>
            <th>Loss</th>
          </tr>
        </thead>
        <tbody>
          {timestamps.map((timestamp) => (
            <tr key={timestamp}>
              <td>
                <time dateTime={new Date(timestamp).toISOString()}>{new Date(timestamp).toLocaleString()}</time>
              </td>
              <td className="num">{cell(wan.metrics.download_bps, values[0].get(timestamp), formatRate)}</td>
              <td className="num">{cell(wan.metrics.upload_bps, values[1].get(timestamp), formatRate)}</td>
              <td className="num">{cell(wan.metrics.latency_ms, values[2].get(timestamp), (value) => `${value.toFixed(1)} ms`)}</td>
              <td className="num">{cell(wan.metrics.loss_pct, values[3].get(timestamp), (value) => `${value.toFixed(1)}%`)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function InternetHealth({ data }: { data: DashboardData }) {
  const [view, setView] = useState<'chart' | 'table'>('chart')
  const wan = data.wan
  const missing = (data.gateway_uplinks ?? []).filter((gateway) => gateway.state === 'missing')
  const up = (data.gateway_uplinks ?? []).filter((gateway) => gateway.state === 'up')
  const routeState = wan?.gateway || up.length > 0 ? 'up' : missing.length > 0 ? 'missing' : 'unknown'
  const reachable = wan?.metrics.reachable
  const reachableValue = reachable?.value

  return (
    <Card
      title="Internet health"
      actions={(
        <span className="dashboard-health-state" data-state={routeState}>
          {routeState === 'up' ? 'Route active' : routeState === 'missing' ? 'No route' : 'Route unknown'}
        </span>
      )}
    >
      <div className="dashboard-internet-health" role="region" aria-label="Internet health details">
        <div className="dashboard-wan-path" role="group" aria-label="Observed gateway path">
          <div className="dashboard-wan-path-node">
            <span>Gateway</span>
            <strong>{wan?.gateway?.name ?? up[0]?.name ?? missing[0]?.name ?? 'Unavailable'}</strong>
          </div>
          <span className="dashboard-wan-path-link" aria-hidden>→</span>
          <div className="dashboard-wan-path-node">
            <span>Default route</span>
            <strong>{wan?.gateway?.route_interface ?? 'Unavailable'}</strong>
          </div>
          <span className="dashboard-wan-path-link" aria-hidden>→</span>
          <div className="dashboard-wan-path-node" data-state={reachableValue == null ? 'unknown' : reachableValue > 0 ? 'up' : 'missing'}>
            <span>External ICMP target · from gateway</span>
            <strong>
              {wan?.target ?? '1.1.1.1'} ·{' '}
              {reachableValue == null || reachable?.status === 'unavailable'
                ? <Unknown why={reachable?.meaning || 'fixed-target ICMP evidence is unavailable'} />
                : reachableValue > 0 ? 'Reachable' : 'No reply'}
            </strong>
          </div>
        </div>

        <div className="dashboard-wan-toolbar">
          <div>
            <div className="speedtest-history-heading">Recent network activity</div>
            <div className="dashboard-metric-note">Past 6 hours · five-minute averages</div>
          </div>
          <div className="dashboard-wan-toolbar-actions">
            <div className="dashboard-wan-coverage-key" aria-label="Sample coverage legend">
              <span data-state="observed">Observed</span>
              <span data-state="unavailable">Unavailable</span>
            </div>
            <div className="speedtest-view-toggle" role="group" aria-label="Internet health history view">
              <Button aria-pressed={view === 'chart'} kind={view === 'chart' ? 'primary' : 'default'} onClick={() => setView('chart')}>
                Chart
              </Button>
              <Button aria-pressed={view === 'table'} kind={view === 'table' ? 'primary' : 'default'} onClick={() => setView('table')}>
                Table
              </Button>
            </div>
          </div>
        </div>

        {view === 'chart' ? (
          <div className="dashboard-wan-metrics">
            <WANMetric label="Download traffic" metric={wan?.metrics.download_bps} format={formatRate} tone="download" />
            <WANMetric label="Upload traffic" metric={wan?.metrics.upload_bps} format={formatRate} tone="upload" />
            <WANMetric label="ICMP latency" metric={wan?.metrics.latency_ms} format={(value) => `${value.toFixed(1)} ms`} tone="latency" />
            <WANMetric label="ICMP loss" metric={wan?.metrics.loss_pct} format={(value) => `${value.toFixed(1)}%`} tone="loss" />
          </div>
        ) : wan ? <WANHistoryTable wan={wan} /> : (
          <div className="dashboard-trend-empty">Internet health history is unavailable.</div>
        )}

        <div className="dashboard-wan-footnote">
          Traffic uses the active default-route interface. ICMP measures target reachability from the gateway, not gateway or ISP uptime.
          {wan?.as_of ? ` Evidence ${wan.freshness === 'fresh' ? 'updated' : 'last observed'} ${agoMilliseconds(wan.as_of)}.` : ''}
        </div>
      </div>
    </Card>
  )
}

function TopologySummary({ onOpenTopology }: { onOpenTopology?: () => void }) {
  const [snapshot, setSnapshot] = useState<TopologySnapshot | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const generation = useRef(0)
  const activeRequest = useRef<AbortController | null>(null)

  const load = useCallback(async () => {
    if (activeRequest.current) return
    const controller = new AbortController()
    activeRequest.current = controller
    const request = ++generation.current
    setLoading(true)
    try {
      const next = await api.topology(undefined, controller.signal)
      if (request === generation.current) {
        setSnapshot(next)
        setError('')
      }
    } catch (cause) {
      if (request === generation.current) setError(errorText(cause))
    } finally {
      if (activeRequest.current === controller) activeRequest.current = null
      if (request === generation.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
    const timer = window.setInterval(load, 30_000)
    return () => {
      generation.current++
      activeRequest.current?.abort()
      activeRequest.current = null
      window.clearInterval(timer)
    }
  }, [load])

  const nodes = snapshot?.nodes ?? []
  const edges = snapshot?.edges ?? []
  const nodeByID = new Map(nodes.map((node) => [node.id, node]))
  const managedDevices = nodes.filter((node) => node.kind === 'device' && node.device_id != null).length
  const placedClients = nodes.filter((node) => node.kind === 'client').length
  const ambiguousLinks = edges.filter((edge) => edge.confidence === 'ambiguous').length
  const lastKnown = snapshot?.last_known_edges?.length ?? 0
  const infrastructureRelations = edges.flatMap((edge) => {
    const parent = nodeByID.get(edge.parent_id)
    const child = nodeByID.get(edge.child_id)
    if (!parent || !child || child.kind !== 'device' ||
        (parent.kind !== 'device' && parent.id !== 'synthetic:internet')) return []
    return [{ edge, parent, child }]
  }).sort((a, b) =>
    a.parent.name.localeCompare(b.parent.name) ||
    a.child.name.localeCompare(b.child.name) ||
    String(a.edge.id).localeCompare(String(b.edge.id), undefined, { numeric: true }),
  )
  const relations = infrastructureRelations.slice(0, 3)

  return (
    <Card
      title={<span id="dashboard-topology-heading">Network topology</span>}
      actions={onOpenTopology && <Button onClick={onOpenTopology}>Open topology</Button>}
    >
      <div
        className="dashboard-topology-summary"
        role="region"
        aria-labelledby="dashboard-topology-heading"
        aria-busy={loading}
      >
        {!snapshot && !error && <div role="status">Loading topology summary…</div>}
        {error && (
          <Notice
            tone="warning"
            component="Topology summary"
            summary={(
              <div role={snapshot ? 'status' : 'alert'}>
                {snapshot
                  ? 'Topology refresh failed; the last successful snapshot remains visible.'
                  : 'Topology summary is unavailable; no graph evidence is shown.'}
              </div>
            )}
            details={error}
            actions={<Button disabled={loading} onClick={() => void load()}>Retry</Button>}
          />
        )}
        {snapshot && (
          <>
            <div className="dashboard-topology-header">
              <span
                className="dashboard-topology-coverage"
                data-complete={snapshot.complete}
              >
                {snapshot.complete
                  ? 'Complete coverage'
                  : snapshot.gaps.length > 0
                    ? `Partial · ${snapshot.gaps.length} coverage issue${snapshot.gaps.length === 1 ? '' : 's'}`
                    : 'Partial coverage'}
              </span>
              <span>Snapshot {agoMilliseconds(snapshot.at)}</span>
            </div>
            <div className="dashboard-topology-stats">
              <div><span>Managed devices</span><strong className="num">{managedDevices}</strong></div>
              <div><span>Active links</span><strong className="num">{edges.length}</strong></div>
              <div><span>Placed clients</span><strong className="num">{placedClients}</strong></div>
            </div>
            {relations.length > 0 ? (
              <ul className="dashboard-topology-links" aria-label="Active infrastructure links">
                {relations.map(({ edge, parent, child }) => {
                  const evidence = [edge.medium, edge.parent_port, edge.confidence].filter(Boolean)
                  return (
                    <li
                      key={edge.id}
                      aria-label={`${parent.name} to ${child.name}, ${evidence.join(', ')}`}
                    >
                      <span>{parent.name}</span>
                      <span aria-hidden="true">→</span>
                      <strong>{child.name}</strong>
                      <small>{evidence.join(' · ')}</small>
                    </li>
                  )
                })}
              </ul>
            ) : (
              <div className="dashboard-topology-empty">
                No infrastructure links are currently observed. Managed devices remain counted without inventing placement.
              </div>
            )}
            {infrastructureRelations.length > relations.length && (
              <div className="dashboard-topology-note">
                Showing {relations.length} of {infrastructureRelations.length} infrastructure links; open Topology for the complete graph.
              </div>
            )}
            {(ambiguousLinks > 0 || lastKnown > 0) && (
              <div className="dashboard-topology-note">
                {ambiguousLinks > 0 && `${ambiguousLinks} active link${ambiguousLinks === 1 ? '' : 's'} ${ambiguousLinks === 1 ? 'has' : 'have'} ambiguous evidence.`}
                {ambiguousLinks > 0 && lastKnown > 0 && ' '}
                {lastKnown > 0 && `${lastKnown} last-known placement${lastKnown === 1 ? ' is' : 's are'} excluded from active links.`}
              </div>
            )}
          </>
        )}
      </div>
    </Card>
  )
}

/**
 * The fleet summary.
 *
 * "Wireless clients" is the same server-side online/local/wireless filter as
 * Client Devices. It is intentionally not a sum of per-radio counters: those
 * counters have no client address and therefore cannot apply network scope.
 */
export function Dashboard({
  data,
  onOpenTopology,
}: {
  data: DashboardData
  onOpenTopology?: () => void
}) {
  const d = data.devices
  const alertPayload = data.recent_alert_events
  const alerts = (alertPayload ?? []).filter(
    (event) => event.Severity === 'warning' || event.Severity === 'error',
  )
  const invalidAlerts = (alertPayload ?? []).length - alerts.length
  const wirelessUnknownOn = data.wireless_clients_unknown_on ?? []
  const missingWAN = (data.gateway_uplinks ?? []).filter((gateway) => gateway.state === 'missing')

  // What "Devices on the LAN" leaves out, named under the number itself.
  //
  // The count is scoped to this network, so on a gateway it excludes the
  // neighbours on the uplink — 11 of 14 on the reference device. Without this
  // line the headline is simply smaller than the previous build's and the
  // operator has no way to tell a correct rescoping from lost devices.
  const elsewhere: string[] = []
  if (data.upstream_devices > 0) elsewhere.push(`${data.upstream_devices} upstream`)
  if (data.unscoped_devices > 0) elsewhere.push(`${data.unscoped_devices} unplaced`)

  return (
    <div className="dashboard-page">
      <div className="dashboard-page-heading">
        <div>
          <h1>Dashboard</h1>
          <div>Internet health, fleet status and recent controller activity.</div>
        </div>
        <span className="dashboard-freshness">Live controller view</span>
      </div>
      {missingWAN.length > 0 && (
        <div role="alert">
          <Banner tone="critical">
            No active WAN/default route was observed on{' '}
            <strong>{missingWAN.map((gateway) => gateway.name).join(', ')}</strong>.
            {' '}The gateway is reachable, but clients may not have Internet access.
            Check its WAN cable, upstream modem, and OpenWrt interface status.
          </Banner>
        </div>
      )}

      <div className="dashboard-operations-grid">
        <InternetHealth data={data} />
        <SpeedTestCard />
      </div>

      <section aria-labelledby="fleet-overview-heading" className="dashboard-section">
        <div className="dashboard-section-heading">
          <h2 id="fleet-overview-heading">Fleet overview</h2>
          <span>Current scoped evidence</span>
        </div>
        <div className="dashboard-stat-grid">
        <Card>
          <Stat label="Devices online" value={`${d.online}/${d.total}`}
            tone={d.online === d.total && d.total > 0 ? 'good' : d.offline > 0 ? 'critical' : undefined} />
        </Card>
        <Card>
          <Stat
            label="Wireless clients"
            value={
              data.wireless_clients_complete ? (
                data.wireless_clients
              ) : (
                <Unknown why="one or more devices did not report their current station set" />
              )
            }
            tone={data.wireless_clients_complete ? undefined : 'muted'}
            sub={
              data.wireless_clients_complete
                ? undefined
                : `${data.wireless_clients} matching row${data.wireless_clients === 1 ? '' : 's'} identified; full total unavailable`
            }
          />
        </Card>
        <Card>
          <Stat
            label="Devices on the LAN"
            value={data.active_devices}
            sub={elsewhere.length > 0 ? `${elsewhere.join(', ')} not counted` : undefined}
          />
        </Card>
        <Card>
          {/* Labelled for what it counts. It said "Focused polls" over
              focused_devices — a count of DEVICES under a label promising a
              count of polls, on a dashboard whose own code comment two files
              away says that showing one number under another's label is how a
              dashboard gets quietly distrusted.

              It also reads 0 almost always, and that is correct rather than
              broken: focus is held by an open device panel, and anyone reading
              this screen does not have one open. Said in the note below, so a
              permanent zero is not mistaken for a stuck counter. */}
          <Stat
            label="Devices in focus"
            value={data.focused_devices}
            sub={data.focused_devices === 0 ? 'no panel is open' : undefined}
          />
        </Card>
        <Card>
          <Stat label="Series collected" value={data.series_count} />
        </Card>
        </div>
      </section>

      <Notice
        tone="accent"
        popoverDetails
        compact
        component="Dashboard metrics"
        summary="Current scoped evidence; definitions and exclusions are available."
        closedLabel="How these counts are calculated"
        openLabel="Hide count definitions"
        details={(
          <>
            “Wireless clients” is the same count as Client Devices with this network,
            online presence and wireless connection selected. It uses current
            hostapd associations plus recent station telemetry, so private MACs and
            clients on another managed VLAN count when their client row is local;
            uplink-side and unplaced rows do not. If any device cannot report its
            station set, the matching-row count remains available but is not shown
            as a complete fleet total. “Devices on the LAN” counts every online row on{' '}
            <em>this</em> network, wired included. “Devices in focus” counts the ones
            being polled every few seconds instead of every minute, which happens
            only while somebody has a device panel open — so from this screen it is
            normally zero, and that is the honest answer rather than a stuck counter.
          </>
        )}
      />

      {!data.wireless_clients_complete && (
        <Banner>
          The wireless client total is unavailable because{' '}
          <strong>
            {wirelessUnknownOn.length > 0
              ? wirelessUnknownOn.join(', ')
              : 'one or more managed devices'}
          </strong>{' '}
          did not report a current station set. Client Devices still identifies{' '}
          {data.wireless_clients} matching row
          {data.wireless_clients === 1 ? '' : 's'}, but presenting that partial
          evidence as the fleet total would show a false zero or dip.
        </Banner>
      )}

      {d.pending > 0 && (
        <Banner tone="accent">
          {d.pending} device{d.pending > 1 ? 's are' : ' is'} in the inventory but
          not adopted. They are not polled: there is no credential for them yet.
        </Banner>
      )}

      <div className="dashboard-detail-grid">
        <div className="dashboard-topology-card">
          <TopologySummary onOpenTopology={onOpenTopology} />
        </div>
        <Card title="Device status">
          <div style={{ display: 'grid', gap: 8 }}>
            {(
              [
                ['online', d.online],
                ['offline', d.offline],
                ['pending', d.pending],
                ['unknown', d.unknown],
              ] as const
            ).map(([k, n]) => (
              <div key={k} style={{ display: 'flex', justifyContent: 'space-between' }}>
                <Status value={k} />
                <span className="num">{n}</span>
              </div>
            ))}
            <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
              “unknown” means adopted but never successfully polled — different
              from offline, which means it answered once and has stopped.
            </div>
          </div>
        </Card>

        <Card title="Recent warnings and errors">
          {alertPayload == null ? (
            <div role="alert">
              <Banner tone="warning">
                The warning/error feed is unavailable. Its absence does not prove
                that no alerts were retained; open Logs for the complete record.
              </Banner>
            </div>
          ) : alerts.length === 0 && invalidAlerts === 0 ? (
            <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
              No retained warning or error events.
            </div>
          ) : (
            <div style={{ display: 'grid', gap: 6 }}>
              {invalidAlerts > 0 && (
                <Banner tone="warning">
                  {invalidAlerts} alert row{invalidAlerts === 1 ? '' : 's'} had an
                  unrecognized severity and {invalidAlerts === 1 ? 'was' : 'were'} omitted.
                  The feed is partial; open Logs for the complete record.
                </Banner>
              )}
              {alerts.slice(0, 8).map((e) => {
                const condition = ipv6RACondition(e)
                return (
                  <div key={e.ID} style={{ display: 'flex', gap: 10, fontSize: 12 }}>
                    <span
                      style={{
                        color:
                          e.Severity === 'error'
                            ? 'var(--critical)'
                            : e.Severity === 'warning'
                              ? 'var(--warning)'
                              : 'var(--text-secondary)',
                        minWidth: 58,
                      }}
                    >
                      {e.Severity}
                    </span>
                    <span style={{ flex: 1 }}>
                      {eventLabel(e)}
                      {condition && (
                        <> · {condition.occurrences.toLocaleString()} occurrence
                          {condition.occurrences === 1 ? '' : 's'}</>
                      )}
                    </span>
                    <span style={{ color: 'var(--text-muted)' }}>{ago(e.TS)}</span>
                  </div>
                )
              })}
            </div>
          )}
        </Card>
      </div>
    </div>
  )
}
