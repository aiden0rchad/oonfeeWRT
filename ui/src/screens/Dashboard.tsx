import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import type {
  Dashboard as DashboardData,
  DashboardMetric,
  DashboardMetricPoint,
  SpeedTestCollection,
  SpeedTestJob,
  TopologySnapshot,
} from '../lib/api'
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

function formatTestRate(value: number | null | undefined) {
  return value == null ? '—' : `${value.toFixed(1)} Mbps`
}

function formatMilliseconds(value: number | null | undefined) {
  return value == null ? '—' : `${value.toFixed(1)} ms`
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

function Trend({ label, points }: { label: string; points: DashboardMetricPoint[] }) {
  const observed = points.flatMap((point) => point.value == null ? [] : [point.value])
  if (observed.length < 2) return null

  const width = 180
  const height = 38
  const low = Math.min(...observed)
  const high = Math.max(...observed)
  const span = high - low || 1
  const segments: string[] = []
  let current = ''
  points.forEach((point, index) => {
    if (point.value == null) {
      if (current) segments.push(current)
      current = ''
      return
    }
    const x = points.length === 1 ? 0 : (index / (points.length - 1)) * width
    const y = height - 2 - ((point.value - low) / span) * (height - 4)
    current += `${current ? ' L' : 'M'} ${x.toFixed(1)} ${y.toFixed(1)}`
  })
  if (current) segments.push(current)

  return (
    <svg
      className="dashboard-trend"
      viewBox={`0 0 ${width} ${height}`}
      role="img"
      aria-label={`${label} six-hour trend: ${observed.length} of ${points.length} five-minute buckets observed`}
      preserveAspectRatio="none"
    >
      {segments.map((path, index) => <path key={index} d={path} />)}
    </svg>
  )
}

function WANMetric({
  label,
  metric,
  format,
}: {
  label: string
  metric?: DashboardMetric
  format: (value: number, unit?: string) => string
}) {
  const available = metric?.value != null && metric.status !== 'unavailable'
  return (
    <div className="dashboard-metric">
      <div className="dashboard-metric-label">{label}</div>
      <div className="dashboard-metric-value num">
        {available
          ? format(metric.value!, metric.unit)
          : <Unknown why={metric?.meaning || 'the controller has no matching WAN telemetry'} />}
      </div>
      {metric && <Trend label={label} points={metric.points} />}
      <div className="dashboard-metric-note">
        {metric?.status === 'fresh' && metric.as_of ? `Updated ${agoMilliseconds(metric.as_of)}` : null}
        {metric?.status === 'last_observed' && metric.as_of ? `Last observed ${agoMilliseconds(metric.as_of)}` : null}
        {!metric || metric.status === 'unavailable' ? 'Current value unavailable' : null}
      </div>
    </div>
  )
}

function mergeSpeedTest(current: SpeedTestCollection | null, job: SpeedTestJob) {
  if (!current) return null
  const jobs = [job, ...current.jobs.filter((item) => item.id !== job.id)]
  return { ...current, jobs, active: job.state === 'completed' || job.state === 'failed' ? null : job }
}

function SpeedTestCard() {
  const [collection, setCollection] = useState<SpeedTestCollection | null>(null)
  const [review, setReview] = useState(false)
  const [acknowledgedPlanID, setAcknowledgedPlanID] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const refreshGeneration = useRef(0)
  const refreshInFlight = useRef<Promise<SpeedTestCollection | null> | null>(null)
  const planID = collection?.test?.plan_id ?? ''
  const acknowledged = planID !== '' && acknowledgedPlanID === planID

  const refresh = useCallback(async () => {
    if (refreshInFlight.current) return refreshInFlight.current
    const generation = ++refreshGeneration.current
    const request = api.speedTests(20).then((next) => {
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
    if (!acknowledged || collection?.active) return
    setAcknowledgedPlanID('')
    refreshGeneration.current++
    refreshInFlight.current = null
    setBusy(true)
    setError('')
    try {
      const job = await api.startSpeedTest(planID, true)
      refreshGeneration.current++
      refreshInFlight.current = null
      setCollection((current) => mergeSpeedTest(current, job))
      setReview(false)
      setAcknowledgedPlanID('')
      void refresh()
    } catch (cause) {
      setError(errorText(cause))
      void refresh()
    } finally {
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
  const history = (collection?.jobs ?? []).filter((job) => !active || job.id !== active.id).slice(0, 5)

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
        <Notice
          tone="accent"
          component="Controller speed test"
          summary="Runs from this controller host/container; it does not install packages or change router settings."
          defaultOpen={review}
          closedLabel="Test impact and consent"
          openLabel="Hide test impact"
          details={(
            <div style={{ display: 'grid', gap: 8 }}>
              <div>
                The test can temporarily saturate the Internet connection for up to{' '}
                {maxDuration ? `${maxDuration} seconds` : 'the controller limit'}
                {' '}and transfers approximately{' '}
                {estimatedBytes ? formatBytes(estimatedBytes) : 'an unavailable amount of data'}
                {' '}plus protocol overhead.
              </div>
              {plan && (
                <dl className="speedtest-plan">
                  <div><dt>Vantage point</dt><dd>{disclosure?.vantage_point ? speedTestProvenance(disclosure.vantage_point) : 'Unavailable'}</dd></div>
                  <div><dt>Provider</dt><dd>{plan.provider || 'Unavailable'}</dd></div>
                  <div><dt>Provider origin</dt><dd>{plan.endpoint || 'Unavailable'}</dd></div>
                  <div><dt>Download endpoint</dt><dd>{plan.download_endpoint || 'Unavailable'}</dd></div>
                  <div><dt>Upload endpoint</dt><dd>{plan.upload_endpoint || 'Unavailable'}</dd></div>
                  <div><dt>Method</dt><dd>{plan.method || 'Unavailable'}</dd></div>
                  <div><dt>Router management calls</dt><dd><code>{typeof disclosure?.router_management_calls === 'boolean' ? String(disclosure.router_management_calls) : 'Unavailable'}</code></dd></div>
                  <div><dt>Router changes</dt><dd><code>{typeof disclosure?.router_changes === 'boolean' ? String(disclosure.router_changes) : 'Unavailable'}</code></dd></div>
                </dl>
              )}
              {!planReady && (
                <div className="speedtest-plan-unavailable" role="status">
                  Exact provider, endpoints, method, provenance, limits, and safety disclosures are unavailable or incomplete; Start remains disabled.
                </div>
              )}
              <div>
                {disclosure?.privacy || 'Privacy disclosure unavailable.'}
              </div>
              {disclosure?.saturation_warning && <div>{disclosure.saturation_warning}</div>}
              <label className="speedtest-consent">
                <input
                  type="checkbox"
                  checked={acknowledged}
                  onChange={(event) => setAcknowledgedPlanID(event.target.checked ? planID : '')}
                />
                <span>I understand this may use data and temporarily saturate the connection.</span>
              </label>
            </div>
          )}
          actions={review ? (
            <>
              <Button disabled={busy} onClick={() => {
                setReview(false)
                setAcknowledgedPlanID('')
              }}>Cancel review</Button>
              <Button kind="primary" disabled={busy || !acknowledged || !planReady} onClick={start}>
                {busy ? 'Starting…' : 'Start speed test'}
              </Button>
            </>
          ) : (
            <Button onClick={() => setReview(true)}>Review speed test</Button>
          )}
        />
      )}

      {error && (
        <div className="speedtest-error" role="alert">
          <span>Speed test unavailable: {error}</span>
          <Button disabled={busy} onClick={() => void refresh()}>Retry</Button>
        </div>
      )}

      <div className="speedtest-history">
        <div className="speedtest-history-heading">Recent tests</div>
        {history.length === 0 ? (
          <div className="dashboard-metric-note">
            {collection == null && !error ? 'Loading history…' : 'No completed tests yet.'}
          </div>
        ) : history.map((job) => (
          <div className="speedtest-history-row" key={job.id}>
            <div>
              <strong>{job.state}</strong>
              <div className="dashboard-metric-note">
                {job.provider} · {job.method} · {speedTestProvenance(job.provenance)}
                {' '}· {agoMilliseconds(job.finished_at ?? job.created_at)}
              </div>
            </div>
            <div className="speedtest-history-result num">
              {job.state === 'completed'
                ? `${formatTestRate(job.download_mbps)} ↓ · ${formatTestRate(job.upload_mbps)} ↑ · ${formatMilliseconds(job.idle_latency_ms)} idle latency · ${formatMilliseconds(job.idle_jitter_ms)} idle jitter · ${formatMilliseconds(job.loaded_latency_ms)} loaded latency · ${formatMilliseconds(job.loaded_jitter_ms)} loaded jitter`
                : job.error || 'No result'}
            </div>
          </div>
        ))}
      </div>
    </Card>
  )
}

function InternetHealth({ data }: { data: DashboardData }) {
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
      <div className="dashboard-wan-context">
        <div>
          <strong>{wan?.gateway?.name ?? up[0]?.name ?? missing[0]?.name ?? 'Gateway unavailable'}</strong>
          <div className="dashboard-metric-note">
            {wan?.gateway?.route_interface
              ? `Default route · ${wan.gateway.route_interface}`
              : 'Default-route interface unavailable'}
          </div>
        </div>
        <div className="dashboard-wan-reachability">
          <span className="dashboard-metric-label">ICMP reachability to {wan?.target ?? '1.1.1.1'}</span>
          <strong>
            {reachableValue == null || reachable?.status === 'unavailable'
              ? <Unknown why={reachable?.meaning || 'fixed-target ICMP evidence is unavailable'} />
              : reachableValue > 0 ? 'Reachable' : 'No reply'}
          </strong>
        </div>
      </div>

      <div className="dashboard-wan-metrics">
        <WANMetric label="Download traffic" metric={wan?.metrics.download_bps} format={formatRate} />
        <WANMetric label="Upload traffic" metric={wan?.metrics.upload_bps} format={formatRate} />
        <WANMetric label="ICMP latency" metric={wan?.metrics.latency_ms} format={(value) => `${value.toFixed(1)} ms`} />
        <WANMetric label="ICMP loss" metric={wan?.metrics.loss_pct} format={(value) => `${value.toFixed(1)}%`} />
      </div>

      <div className="dashboard-wan-footnote">
        Fixed-target ICMP to {wan?.target ?? 'an unavailable target'} measures reachability, not gateway uptime.
        {' '}Traffic requires an exact default-route interface series key; gaps remain gaps.
        {wan?.as_of ? ` Evidence ${wan.freshness === 'fresh' ? 'updated' : 'last observed'} ${agoMilliseconds(wan.as_of)}.` : ''}
      </div>
    </Card>
  )
}

function TopologySummary({ onOpenTopology }: { onOpenTopology?: () => void }) {
  const [snapshot, setSnapshot] = useState<TopologySnapshot | null>(null)
  const [error, setError] = useState('')
  const generation = useRef(0)

  const load = useCallback(async () => {
    const request = ++generation.current
    try {
      const next = await api.topology()
      if (request === generation.current) {
        setSnapshot(next)
        setError('')
      }
    } catch (cause) {
      if (request === generation.current) setError(errorText(cause))
    }
  }, [])

  useEffect(() => {
    void load()
    const timer = window.setInterval(load, 30_000)
    return () => {
      generation.current++
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
            actions={<Button onClick={() => void load()}>Retry</Button>}
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
        component="Dashboard metrics"
        summary="Counts use current, scoped evidence; definitions and exclusions are available below."
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
              {alerts.slice(0, 8).map((e) => (
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
                  <span style={{ flex: 1 }}>{e.Event}</span>
                  <span style={{ color: 'var(--text-muted)' }}>{ago(e.TS)}</span>
                </div>
              ))}
            </div>
          )}
        </Card>
      </div>
    </div>
  )
}
