import { useEffect, useMemo, useState } from 'react'
import { TimeChart, ago, fmt } from '../components/Chart'
import type { TimeChartPoint } from '../components/Chart'
import { Banner, Button, PageHeader } from '../components/ui'
import { api } from '../lib/api'
import type { Dashboard, Device, Point, Series } from '../lib/api'

type RangeID = '6h' | '24h' | '7d' | '30d'
type MetricGroup = 'system' | 'interface' | 'radio'

interface RangeOption {
  id: RangeID
  label: string
  shortLabel: string
  seconds: number
}

interface MetricPresentation {
  label: string
  note: string
  colour: string
  format: (value: number, step?: number) => string
  bounds?: readonly [number, number]
  minSpan?: number
}

interface SeriesQuery extends MetricPresentation {
  id: string
  deviceID: number
  kind: string
  key: string
  group?: MetricGroup
}

interface LoadedSeries {
  data?: Series
  range?: RangeID
  window?: readonly [number, number]
  error?: string
}

const ranges: readonly RangeOption[] = [
  { id: '6h', label: '6 hours', shortLabel: '6h', seconds: 6 * 60 * 60 },
  { id: '24h', label: '24 hours', shortLabel: '24h', seconds: 24 * 60 * 60 },
  { id: '7d', label: '7 days', shortLabel: '7d', seconds: 7 * 24 * 60 * 60 },
  { id: '30d', label: '30 days', shortLabel: '30d', seconds: 30 * 24 * 60 * 60 },
]

const presentations: Record<string, MetricPresentation> = {
  iface_rx_bps: {
    label: 'Download traffic',
    note: 'Bytes received per second on the selected interface',
    colour: 'var(--series-1)',
    format: fmt.bytesPerSec,
    bounds: [0, Number.MAX_VALUE],
  },
  iface_tx_bps: {
    label: 'Upload traffic',
    note: 'Bytes transmitted per second on the selected interface',
    colour: 'var(--series-2)',
    format: fmt.bytesPerSec,
    bounds: [0, Number.MAX_VALUE],
  },
  site_wan_latency_ms: {
    label: 'ICMP latency',
    note: 'Round-trip ICMP latency from the selected gateway to 1.1.1.1',
    colour: 'var(--series-7)',
    format: (value) => `${value.toFixed(value < 10 ? 1 : 0)} ms`,
    bounds: [0, Number.MAX_VALUE],
  },
  site_wan_loss_pct: {
    label: 'ICMP loss',
    note: 'ICMP packet loss from the selected gateway to 1.1.1.1',
    colour: 'var(--series-3)',
    format: fmt.percent,
    bounds: [0, 100],
    minSpan: 1,
  },
  sys_load1: {
    label: 'Load average',
    note: 'One-minute device load average',
    colour: 'var(--series-4)',
    format: fmt.plain,
    bounds: [0, Number.MAX_VALUE],
  },
  sys_mem_pct: {
    label: 'Memory in use',
    note: 'Device memory in use, using the kernel available-memory figure when reported',
    colour: 'var(--series-5)',
    format: fmt.percent,
    bounds: [0, 100],
    minSpan: 1,
  },
  sys_mem_used: {
    label: 'Memory in use',
    note: 'Bytes of device memory in use',
    colour: 'var(--series-5)',
    format: formatBytes,
    bounds: [0, Number.MAX_VALUE],
  },
  radio_utilization_pct: {
    label: 'Radio utilization',
    note: 'Stable-radio utilization reported by the device',
    colour: 'var(--series-3)',
    format: fmt.percent,
    bounds: [0, 100],
    minSpan: 1,
  },
  radio_interference_pct: {
    label: 'Radio interference',
    note: 'Interference reported for the stable radio',
    colour: 'var(--series-7)',
    format: fmt.percent,
    bounds: [0, 100],
    minSpan: 1,
  },
  radio_rx_airtime_pct: {
    label: 'Receive airtime',
    note: 'Receive airtime reported for the stable radio',
    colour: 'var(--series-1)',
    format: fmt.percent,
    bounds: [0, 100],
    minSpan: 1,
  },
  radio_tx_airtime_pct: {
    label: 'Transmit airtime',
    note: 'Transmit airtime reported for the stable radio',
    colour: 'var(--series-2)',
    format: fmt.percent,
    bounds: [0, 100],
    minSpan: 1,
  },
  radio_retry_delta_pct: {
    label: 'Radio retries',
    note: 'Transmit retries divided by packets over each observation interval',
    colour: 'var(--series-4)',
    format: fmt.percent,
    bounds: [0, 100],
    minSpan: 1,
  },
  radio_tx_fail_delta_pct: {
    label: 'Radio transmit failures',
    note: 'Transmit failures divided by packets over each observation interval',
    colour: 'var(--series-8)',
    format: fmt.percent,
    bounds: [0, 100],
    minSpan: 1,
  },
  radio_signal_avg_dbm: {
    label: 'Average client signal',
    note: 'Mean signal of stations associated with the stable radio; absent with no measured stations',
    colour: 'var(--series-6)',
    format: fmt.dbm,
  },
}

const radioKinds = [
  'radio_utilization_pct',
  'radio_interference_pct',
  'radio_rx_airtime_pct',
  'radio_tx_airtime_pct',
  'radio_retry_delta_pct',
  'radio_tx_fail_delta_pct',
  'radio_signal_avg_dbm',
] as const

const emptySeriesCatalog: Record<string, string[]> = {}

export function Statistics() {
  const [dashboard, setDashboard] = useState<Dashboard | null>(null)
  const [devices, setDevices] = useState<Device[] | null>(null)
  const [dashboardError, setDashboardError] = useState('')
  const [devicesError, setDevicesError] = useState('')
  const [coreLoading, setCoreLoading] = useState(true)
  const [range, setRange] = useState<RangeID>('6h')
  const [revision, setRevision] = useState(0)
  const [selectedDeviceID, setSelectedDeviceID] = useState<number | null>(null)
  const [catalogState, setCatalogState] = useState<{
    deviceID: number
    series: Record<string, string[]>
  } | null>(null)
  const [catalogError, setCatalogError] = useState('')
  const [catalogLoading, setCatalogLoading] = useState(false)
  const [selectedInterface, setSelectedInterface] = useState('')
  const [selectedRadio, setSelectedRadio] = useState('')
  const [themeRevision, setThemeRevision] = useState(0)

  useEffect(() => {
    let active = true
    setCoreLoading(true)
    const controller = new AbortController()
    Promise.allSettled([api.dashboard(controller.signal), api.devices(controller.signal)]).then((results) => {
      if (!active) return
      const [dashboardResult, devicesResult] = results
      if (dashboardResult.status === 'fulfilled') {
        setDashboard(dashboardResult.value)
        setDashboardError('')
      } else {
        setDashboardError(errorText(dashboardResult.reason))
      }
      if (devicesResult.status === 'fulfilled') {
        setDevices(devicesResult.value.devices)
        setDevicesError('')
      } else {
        setDevicesError(errorText(devicesResult.reason))
      }
      setCoreLoading(false)
    })
    return () => {
      active = false
      controller.abort()
    }
  }, [revision])

  useEffect(() => {
    const timer = window.setInterval(() => setRevision((value) => value + 1), 5 * 60 * 1000)
    return () => window.clearInterval(timer)
  }, [])

  useEffect(() => {
    const observer = new MutationObserver(() => setThemeRevision((value) => value + 1))
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
    return () => observer.disconnect()
  }, [])

  const gatewayID = dashboard?.wan.gateway?.device_id ?? null
  useEffect(() => {
    if (!devices) return
    setSelectedDeviceID((current) => {
      if (current != null && devices.some((device) => device.id === current)) return current
      if (gatewayID != null && devices.some((device) => device.id === gatewayID)) return gatewayID
      return devices[0]?.id ?? null
    })
  }, [devices, gatewayID])

  useEffect(() => {
    if (selectedDeviceID == null) {
      setCatalogState(null)
      setCatalogError('')
      setCatalogLoading(false)
      return
    }
    let active = true
    setCatalogLoading(true)
    api.deviceSeries(selectedDeviceID).then((result) => {
      if (!active) return
      setCatalogState({ deviceID: selectedDeviceID, series: result.series })
      setCatalogError('')
    }).catch((reason: unknown) => {
      if (active) setCatalogError(errorText(reason))
    }).finally(() => {
      if (active) setCatalogLoading(false)
    })
    return () => { active = false }
  }, [selectedDeviceID, revision])

  const catalog = catalogState?.deviceID === selectedDeviceID ? catalogState.series : emptySeriesCatalog
  const interfaceKeys = useMemo(
    () => uniqueKeys(catalog.iface_rx_bps, catalog.iface_tx_bps),
    [catalog.iface_rx_bps, catalog.iface_tx_bps],
  )
  const radioKeys = useMemo(
    () => uniqueKeys(...radioKinds.map((kind) => catalog[kind])),
    [catalog],
  )

  useEffect(() => {
    const gatewayKey = selectedDeviceID === gatewayID ? dashboard?.wan.gateway?.series_key : null
    setSelectedInterface((current) => interfaceKeys.includes(current)
      ? current
      : gatewayKey && interfaceKeys.includes(gatewayKey)
        ? gatewayKey
        : interfaceKeys[0] ?? '')
  }, [interfaceKeys, selectedDeviceID, gatewayID, dashboard?.wan.gateway?.series_key])

  useEffect(() => {
    setSelectedRadio((current) => radioKeys.includes(current) ? current : radioKeys[0] ?? '')
  }, [radioKeys])

  const wanQueries = useMemo(() => buildWANQueries(dashboard), [dashboard])
  const deviceQueries = useMemo(
    () => buildDeviceQueries(selectedDeviceID, catalog, selectedInterface, selectedRadio),
    [selectedDeviceID, catalog, selectedInterface, selectedRadio],
  )
  const wanSeries = useSeriesQueries(wanQueries, range, revision)
  const deviceSeries = useSeriesQueries(deviceQueries, range, revision)
  const selectedDevice = devices?.find((device) => device.id === selectedDeviceID) ?? null
  const refreshing = coreLoading || catalogLoading || wanSeries.loading || deviceSeries.loading
  const currentRange = ranges.find((option) => option.id === range)!

  return (
    <div className="statistics-page">
      <PageHeader
        title="Statistics"
        purpose="Historical network and device telemetry, with source coverage and missing evidence kept visible."
        actions={<Button disabled={refreshing} onClick={() => setRevision((value) => value + 1)}>
          {refreshing ? 'Refreshing…' : 'Refresh'}
        </Button>}
      />

      <section className="statistics-toolbar" aria-label="Statistics controls">
        <div>
          <span className="statistics-control-label">Time range</span>
          <div className="statistics-range-control" role="group" aria-label="Statistics time range">
            {ranges.map((option) => (
              <button
                key={option.id}
                type="button"
                aria-label={option.label}
                aria-pressed={range === option.id}
                onClick={() => setRange(option.id)}
              >
                {option.shortLabel}
              </button>
            ))}
          </div>
        </div>
        <label className="statistics-select-control">
          <span className="statistics-control-label">Device detail</span>
          <select
            aria-label="Device detail"
            value={selectedDeviceID ?? ''}
            disabled={!devices?.length}
            onChange={(event) => setSelectedDeviceID(Number(event.target.value))}
          >
            {!devices?.length && <option value="">No devices available</option>}
            {devices?.map((device) => (
              <option key={device.id} value={device.id}>{device.name || device.mac}</option>
            ))}
          </select>
        </label>
        <div className="statistics-read-boundary" role="note">
          Stored rollups only · viewing this page does not focus devices or raise their polling rate.
        </div>
      </section>

      <details className="statistics-history-help">
        <summary>About history and missing samples</summary>
        <p>
          Charts show stored observations, not a continuous recording. A new or recently
          restarted controller may have only a short stretch of history. Blank intervals
          mean no stored measurement—not zero traffic or a confirmed outage.
        </p>
        <p>
          Keep the controller running and its devices reachable to build history. Allow a
          complete five-minute interval and the next storage flush; Refresh only reads
          saved data. If an expected metric stays empty, check the device&apos;s connection
          and capability details. Some sources need focused collection or are not exposed
          by the device. Past gaps cannot be filled retrospectively.
        </p>
        <p>
          Use 6h for recent trends or select a longer range to compare history. Lines show
          averages, the subtle band retains the measured minimum and maximum, and a lone
          dot means an isolated sample. Missing intervals are never joined by a line.
        </p>
      </details>

      {dashboardError && <Banner tone={dashboard ? 'warning' : 'critical'}>
        Dashboard sources could not refresh: {dashboardError}.
        {dashboard && ' Showing the last successful gateway selection.'}
      </Banner>}
      {devicesError && <Banner tone={devices ? 'warning' : 'critical'}>
        Device inventory could not refresh: {devicesError}.
        {devices && ' Showing the last successful inventory.'}
      </Banner>}

      <section className="statistics-section" aria-labelledby="statistics-internet-title">
        <div className="statistics-section-heading">
          <div>
            <h2 id="statistics-internet-title">Internet history</h2>
            <p>
              {dashboard?.wan.gateway
                ? `${dashboard.wan.gateway.name} · ${dashboard.wan.gateway.route_interface || 'route interface unavailable'} · ${currentRange.label}`
                : `Selected managed gateway · ${currentRange.label}`}
            </p>
          </div>
          {newestTimestamp(wanSeries.results) != null && (
            <span className="statistics-freshness">Newest bucket {ago(newestTimestamp(wanSeries.results))}</span>
          )}
        </div>

        {!dashboard && coreLoading && <div className="statistics-empty" role="status">Loading gateway telemetry…</div>}
        {dashboard && !dashboard.wan.gateway && (
          <div className="statistics-empty">
            No managed gateway has current default-route evidence. WAN history is unavailable until the controller can
            select one without guessing.
          </div>
        )}
        {dashboard?.wan.gateway && (
          <>
            {!dashboard.wan.gateway.series_key && (
              <Banner tone="accent">
                WAN throughput is unavailable because the route-interface name does not exactly match stored interface
                series. ICMP history remains independent and is shown below.
              </Banner>
            )}
            {wanSeries.failures > 0 && (
              <Banner tone="warning">
                {wanSeries.failures} Internet metric {wanSeries.failures === 1 ? 'request' : 'requests'} failed.
                Successful series are current; failed series retain and label their last response when available.
              </Banner>
            )}
            <div className="statistics-summary-grid" aria-label="Latest Internet readings">
              {wanQueries.filter((query) => query.kind !== 'site_wan_up').map((query) => (
                <MetricSummary key={query.id} query={query} loaded={wanSeries.results[query.id]} />
              ))}
            </div>
            <div className="statistics-chart-grid">
              {wanQueries.filter((query) => query.kind !== 'site_wan_up').map((query) => (
                <MetricCard
                  key={query.id}
                  query={query}
                  loaded={wanSeries.results[query.id]}
                  requestedRange={range}
                  loading={wanSeries.loading}
                  themeRevision={themeRevision}
                />
              ))}
            </div>
            <ReachabilityStrip
              loaded={wanSeries.results['wan:reachability']}
              requestedRange={range}
              loading={wanSeries.loading}
            />
            <p className="statistics-source-note">
              Traffic is read only from the selected gateway&apos;s exact default-route interface. ICMP measures
              reachability to {dashboard.wan.target || '1.1.1.1'} from that gateway; it is not a claim about ISP or
              gateway uptime. Missing buckets remain missing and are never converted to zero.
            </p>
          </>
        )}
      </section>

      <section className="statistics-section" aria-labelledby="statistics-device-title">
        <div className="statistics-section-heading">
          <div>
            <h2 id="statistics-device-title">Device history</h2>
            <p>{selectedDevice ? `${selectedDevice.name || selectedDevice.mac} · ${selectedDevice.status}` : 'Select a device with stored telemetry'}</p>
          </div>
          {newestTimestamp(deviceSeries.results) != null && (
            <span className="statistics-freshness">Newest bucket {ago(newestTimestamp(deviceSeries.results))}</span>
          )}
        </div>

        {catalogError && <Banner tone={catalogState?.deviceID === selectedDeviceID ? 'warning' : 'critical'}>
          Metric catalog could not refresh: {catalogError}.
          {catalogState?.deviceID === selectedDeviceID && ' Showing the last successful catalog.'}
        </Banner>}
        {deviceSeries.failures > 0 && <Banner tone="warning">
          {deviceSeries.failures} device metric {deviceSeries.failures === 1 ? 'request' : 'requests'} failed.
          Successful series are current; failed series retain and label their last response when available.
        </Banner>}

        {selectedDeviceID == null ? (
          <div className="statistics-empty">No adopted devices are available.</div>
        ) : catalogLoading && catalogState?.deviceID !== selectedDeviceID ? (
          <div className="statistics-empty" role="status">Discovering stored series for this device…</div>
        ) : (
          <>
            <div className="statistics-dimension-controls">
              <label className="statistics-select-control">
                <span className="statistics-control-label">Interface</span>
                <select
                  aria-label="Network interface"
                  value={selectedInterface}
                  disabled={interfaceKeys.length === 0}
                  onChange={(event) => setSelectedInterface(event.target.value)}
                >
                  {interfaceKeys.length === 0 && <option value="">No stored interface series</option>}
                  {interfaceKeys.map((key) => <option key={key} value={key}>{key}</option>)}
                </select>
              </label>
              <label className="statistics-select-control">
                <span className="statistics-control-label">Stable radio</span>
                <select
                  aria-label="Stable radio"
                  value={selectedRadio}
                  disabled={radioKeys.length === 0}
                  onChange={(event) => setSelectedRadio(event.target.value)}
                >
                  {radioKeys.length === 0 && <option value="">No stored radio series</option>}
                  {radioKeys.map((key) => <option key={key} value={key}>{key}</option>)}
                </select>
              </label>
            </div>

            {deviceQueries.length === 0 ? (
              <div className="statistics-empty">
                No stored system, interface, or stable-radio series are discoverable for this device. This is missing
                evidence, not a zero reading.
              </div>
            ) : (
              <>
                {(['system', 'interface', 'radio'] as const).map((group) => {
                  const queries = deviceQueries.filter((query) => query.group === group)
                  if (queries.length === 0) return null
                  return (
                    <div className="statistics-metric-group" key={group}>
                      <h3>{group === 'system' ? 'System' : group === 'interface' ? `Interface · ${selectedInterface}` : `Radio · ${selectedRadio}`}</h3>
                      <div className="statistics-chart-grid">
                        {queries.map((query) => (
                          <MetricCard
                            key={query.id}
                            query={query}
                            loaded={deviceSeries.results[query.id]}
                            requestedRange={range}
                            loading={deviceSeries.loading}
                            themeRevision={themeRevision}
                          />
                        ))}
                      </div>
                    </div>
                  )
                })}
              </>
            )}
            <p className="statistics-source-note">
              Only catalogued series are requested. Interface names and stable radio keys come from this device&apos;s
              stored series inventory; this page does not infer, merge, or total them. Radio history can be sparse
              because survey and station telemetry is collected only while a device is in the focused polling tier;
              Statistics never raises that polling rate.
            </p>
          </>
        )}
      </section>
    </div>
  )
}

function buildWANQueries(dashboard: Dashboard | null): SeriesQuery[] {
  const gateway = dashboard?.wan.gateway
  if (!gateway) return []
  const queries: SeriesQuery[] = []
  if (gateway.series_key) {
    queries.push(query('wan:download', gateway.device_id, 'iface_rx_bps', gateway.series_key))
    queries.push(query('wan:upload', gateway.device_id, 'iface_tx_bps', gateway.series_key))
  }
  queries.push(query('wan:latency', gateway.device_id, 'site_wan_latency_ms', ''))
  queries.push(query('wan:loss', gateway.device_id, 'site_wan_loss_pct', ''))
  queries.push({
    id: 'wan:reachability',
    deviceID: gateway.device_id,
    kind: 'site_wan_up',
    key: '',
    label: 'ICMP reachability',
    note: 'Whether the fixed ICMP target replied in each stored bucket',
    colour: 'var(--good)',
    format: (value) => value >= 0.5 ? 'Reachable' : 'Unreachable',
    bounds: [0, 1],
  })
  return queries
}

function buildDeviceQueries(
  deviceID: number | null,
  catalog: Record<string, string[]>,
  interfaceKey: string,
  radioKey: string,
): SeriesQuery[] {
  if (deviceID == null) return []
  const queries: SeriesQuery[] = []
  const has = (kind: string, key: string) => catalog[kind]?.includes(key) ?? false
  if (has('sys_load1', '')) queries.push({ ...query(`device:${deviceID}:load`, deviceID, 'sys_load1', ''), group: 'system' })
  const memoryKind = has('sys_mem_pct', '') ? 'sys_mem_pct' : has('sys_mem_used', '') ? 'sys_mem_used' : ''
  if (memoryKind) queries.push({ ...query(`device:${deviceID}:memory`, deviceID, memoryKind, ''), group: 'system' })
  if (interfaceKey && has('iface_rx_bps', interfaceKey)) {
    queries.push({ ...query(`device:${deviceID}:interface:rx:${interfaceKey}`, deviceID, 'iface_rx_bps', interfaceKey, 'Receive traffic'), group: 'interface' })
  }
  if (interfaceKey && has('iface_tx_bps', interfaceKey)) {
    queries.push({ ...query(`device:${deviceID}:interface:tx:${interfaceKey}`, deviceID, 'iface_tx_bps', interfaceKey, 'Transmit traffic'), group: 'interface' })
  }
  if (radioKey) {
    for (const kind of radioKinds) {
      if (has(kind, radioKey)) {
        queries.push({ ...query(`device:${deviceID}:radio:${kind}:${radioKey}`, deviceID, kind, radioKey), group: 'radio' })
      }
    }
  }
  return queries
}

function query(id: string, deviceID: number, kind: string, key: string, label?: string): SeriesQuery {
  const presentation = presentations[kind]
  if (!presentation) throw new Error(`Statistics presentation is missing for ${kind}`)
  return { id, deviceID, kind, key, ...presentation, label: label ?? presentation.label }
}

function seriesSource(query: SeriesQuery): string {
  return JSON.stringify([query.deviceID, query.kind, query.key])
}

function useSeriesQueries(queries: SeriesQuery[], range: RangeID, revision: number) {
  const [results, setResults] = useState<Record<string, LoadedSeries & { source: string }>>({})
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (queries.length === 0) {
      setResults({})
      setLoading(false)
      return
    }
    let active = true
    const option = ranges.find((item) => item.id === range)!
    const to = Math.floor(Date.now() / 1000)
    const from = to - option.seconds
    setLoading(true)
    Promise.allSettled(queries.map((item) => api.stats(item.kind, item.deviceID, item.key, from, to))).then((settled) => {
      if (!active) return
      setResults((previous) => Object.fromEntries(queries.map((item, index) => {
        const result = settled[index]
        const source = seriesSource(item)
        const retained = previous[item.id]?.source === source ? previous[item.id] : undefined
        return result.status === 'fulfilled'
          ? [item.id, { source, data: result.value, range, window: [from, to] as const }]
          : [item.id, { ...retained, source, error: errorText(result.reason) }]
      })))
      setLoading(false)
    })
    return () => { active = false }
  }, [queries, range, revision])

  // A gateway, interface, or metric kind can change while its display slot
  // stays the same. Never show the old source under the new source's label,
  // including the render before the replacement request starts or settles.
  const currentResults = Object.fromEntries(queries.flatMap((item) => {
    const loaded = results[item.id]
    return loaded?.source === seriesSource(item) ? [[item.id, loaded]] : []
  }))
  return {
    results: currentResults,
    loading,
    failures: Object.values(currentResults).filter((result) => result.error).length,
  }
}

function MetricSummary({ query, loaded }: { query: SeriesQuery; loaded?: LoadedSeries }) {
  const points = usablePoints(loaded, query.bounds)
  const latest = points.at(-1)
  return (
    <div className="statistics-summary-card">
      <span>{query.label}</span>
      <strong className="num">{latest ? query.format(latest.avg) : 'Unavailable'}</strong>
      <small>{latest ? `Bucket ended ${ago(latest.ts + resolutionSeconds(loaded?.data?.resolution))}` : 'No stored bucket in this range'}</small>
    </div>
  )
}

function MetricCard({
  query,
  loaded,
  requestedRange,
  loading,
  themeRevision,
}: {
  query: SeriesQuery
  loaded?: LoadedSeries
  requestedRange: RangeID
  loading: boolean
  themeRevision: number
}) {
  const points = usablePoints(loaded, query.bounds)
  const chartPoints = alignChartPoints(loaded, points)
  const coverage = seriesCoverage(loaded, points.length)
  const invalid = Math.max(0, (loaded?.data?.points.length ?? 0) - points.length)
  const previousRange = loaded?.range && loaded.range !== requestedRange
  return (
    <article className="statistics-chart-card">
      <header>
        <div>
          <h4>{query.label}</h4>
          {query.key && <code>{query.key}</code>}
        </div>
        <CoveragePill coverage={coverage} label={query.label} />
      </header>
      {loaded?.error && <div className="statistics-series-warning" role="alert">
        Refresh failed: {loaded.error}.{loaded.data && ' Last successful response retained.'}
      </div>}
      {loading && !loaded?.data && <div className="statistics-series-loading" role="status">Loading stored buckets…</div>}
      {loading && previousRange && <div className="statistics-series-loading" role="status">
        Loading {rangeName(requestedRange)}; showing {rangeName(loaded.range!)} until it arrives.
      </div>}
      <TimeChart
        key={`${query.id}:${themeRevision}`}
        points={chartPoints}
        label={query.label}
        format={query.format}
        colour={query.colour}
        height={176}
        resolution={loaded?.data?.resolution}
        window={loaded?.window ? [loaded.window[0], loaded.window[1]] : undefined}
        note={query.note}
        emptyNote={`No stored ${query.label.toLocaleLowerCase()} buckets in this range.`}
        minSpan={query.minSpan}
      />
      {invalid > 0 && <div className="statistics-series-warning" role="note">
        {invalid} invalid {invalid === 1 ? 'bucket was' : 'buckets were'} omitted.
      </div>}
    </article>
  )
}

export function ReachabilityStrip({
  loaded,
  requestedRange,
  loading,
}: {
  loaded?: LoadedSeries
  requestedRange: RangeID
  loading: boolean
}) {
  const points = usablePoints(loaded, [0, 1])
  const coverage = seriesCoverage(loaded, points.length)
  const reachable = points.filter((point) => reachabilityState(point) === 'up').length
  const mixed = points.filter((point) => reachabilityState(point) === 'mixed').length
  const unreachable = points.filter((point) => reachabilityState(point) === 'down').length
  const missing = coverage?.missing ?? 0
  const runs = reachabilityRuns(points, loaded)
  const previousRange = loaded?.range && loaded.range !== requestedRange
  const label = points.length > 0
    ? `ICMP reachability: ${bucketCount(reachable, 'all-reply')}, ${bucketCount(mixed, 'mixed-reply')}, ${bucketCount(unreachable, 'no-reply')}, and ${bucketCount(missing, 'missing')}.`
    : 'ICMP reachability: no stored buckets in this range.'

  return (
    <article className="statistics-reachability-card">
      <header>
        <div>
          <h3>ICMP reachability</h3>
          <p>Fixed target reply evidence; blank intervals are missing observations</p>
        </div>
        <CoveragePill coverage={coverage} label="ICMP reachability" />
      </header>
      {loaded?.error && <div className="statistics-series-warning" role="alert">
        Refresh failed: {loaded.error}.{loaded.data && ' Last successful response retained.'}
      </div>}
      {loading && !loaded?.data && <div className="statistics-series-loading" role="status">Loading reachability buckets…</div>}
      {loading && previousRange && <div className="statistics-series-loading" role="status">
        Loading {rangeName(requestedRange)}; showing {rangeName(loaded.range!)} until it arrives.
      </div>}
      <div className="statistics-reachability-figure" role="img" aria-label={label}>
        {loaded?.window && points.length > 0 ? (
          <svg viewBox="0 0 800 34" preserveAspectRatio="none" aria-hidden="true" focusable="false">
            <rect className="statistics-reachability-missing" x="0" y="4" width="800" height="26" rx="3" />
            {runs.map((run) => (
              <rect
                key={`${run.start}:${run.state}`}
                className={`statistics-reachability-${run.state}`}
                x={run.x}
                y="4"
                width={run.width}
                height="26"
              />
            ))}
          </svg>
        ) : <span>No stored reachability buckets in this range.</span>}
      </div>
      <div className="statistics-reachability-legend" aria-hidden="true">
        <span data-state="up">All replied · {reachable}</span>
        <span data-state="mixed">Mixed replies · {mixed}</span>
        <span data-state="down">No replies · {unreachable}</span>
        <span data-state="missing">Missing · {missing}</span>
      </div>
    </article>
  )
}

function CoveragePill({ coverage, label }: { coverage: ReturnType<typeof seriesCoverage>; label: string }) {
  if (!coverage) return <span className="statistics-coverage" data-state="unavailable">Awaiting data</span>
  return (
    <details className="statistics-coverage-disclosure" data-state={coverage.state}>
      <summary className="statistics-coverage" aria-label={`${label} history coverage`}>
        {coverage.state === 'complete' ? 'Full history' : coverage.state === 'partial' ? 'History gaps' : 'No history'}
      </summary>
      <p>
        {coverage.observed} of {coverage.expected} completed intervals have stored samples
        in this range. {coverage.missing > 0
          ? `${coverage.missing} intervals are unobserved; blank space does not mean zero activity or downtime.`
          : 'Every completed interval in this range has a stored sample.'}
      </p>
    </details>
  )
}

function usablePoints(loaded: LoadedSeries | undefined, bounds?: readonly [number, number]): Point[] {
  if (!loaded?.data || !loaded.window) return []
  const [from, to] = loaded.window
  const seconds = resolutionSeconds(loaded.data.resolution)
  const unique = new Map<number, Point>()
  for (const point of loaded.data.points) {
    const values = [point.avg, point.min, point.max]
    if (!Number.isSafeInteger(point.ts) || point.ts % seconds !== 0 || point.ts < from || point.ts + seconds > to ||
      !Number.isSafeInteger(point.cnt) || point.cnt <= 0 ||
      values.some((value) => !Number.isFinite(value)) || point.min > point.avg || point.avg > point.max ||
      (bounds && values.some((value) => value < bounds[0] || value > bounds[1]))) continue
    unique.set(point.ts, point)
  }
  return [...unique.values()].sort((a, b) => a.ts - b.ts)
}

export function alignChartPoints(loaded: LoadedSeries | undefined, observed: Point[]): TimeChartPoint[] {
  if (!loaded?.data || !loaded.window) return []
  const seconds = resolutionSeconds(loaded.data.resolution)
  const [from, to] = loaded.window
  const first = Math.ceil(from / seconds) * seconds
  const end = Math.floor(to / seconds) * seconds
  const byTimestamp = new Map(observed.map((point) => [point.ts, point]))
  const aligned: TimeChartPoint[] = []
  for (let ts = first; ts < end; ts += seconds) {
    aligned.push(byTimestamp.get(ts) ?? { ts, avg: null, min: null, max: null, cnt: 0 })
  }
  return aligned
}

function seriesCoverage(loaded: LoadedSeries | undefined, observed: number) {
  if (!loaded?.data || !loaded.window) return null
  const seconds = resolutionSeconds(loaded.data.resolution)
  const first = Math.ceil(loaded.window[0] / seconds) * seconds
  const end = Math.floor(loaded.window[1] / seconds) * seconds
  const expected = Math.max(0, Math.floor((end - first) / seconds))
  const missing = Math.max(0, expected - observed)
  return {
    observed,
    expected,
    missing,
    state: observed === 0 ? 'unavailable' : missing === 0 ? 'complete' : 'partial',
  } as const
}

function reachabilityRuns(points: Point[], loaded?: LoadedSeries) {
  if (!loaded?.window || !loaded.data) return []
  const [from, to] = loaded.window
  const span = Math.max(1, to - from)
  const bucket = resolutionSeconds(loaded.data.resolution)
  const runs: { start: number; end: number; state: ReturnType<typeof reachabilityState>; x: number; width: number }[] = []
  for (const point of points) {
    const state = reachabilityState(point)
    const previous = runs.at(-1)
    if (previous && previous.state === state && point.ts - previous.end <= bucket * 1.5) {
      previous.end = point.ts
      previous.width = Math.max(1, ((Math.min(to, point.ts + bucket) - previous.start) / span) * 800)
      continue
    }
    runs.push({
      start: point.ts,
      end: point.ts,
      state,
      x: ((point.ts - from) / span) * 800,
      width: Math.max(1, (bucket / span) * 800),
    })
  }
  return runs
}

function reachabilityState(point: Point): 'up' | 'mixed' | 'down' {
  if (point.min >= 0.5) return 'up'
  if (point.max < 0.5) return 'down'
  return 'mixed'
}

function newestTimestamp(results: Record<string, LoadedSeries>): number | null {
  let newest: number | null = null
  for (const result of Object.values(results)) {
    const point = result.data?.points.at(-1)
    if (point && Number.isSafeInteger(point.ts) && (newest == null || point.ts > newest)) newest = point.ts
  }
  return newest
}

function uniqueKeys(...groups: (string[] | undefined)[]): string[] {
  return [...new Set(groups.flatMap((group) => group ?? []).filter((key) => key.trim() !== ''))].sort()
}

function resolutionSeconds(resolution?: Series['resolution']) {
  return resolution === '1h' ? 60 * 60 : 5 * 60
}

function rangeName(id: RangeID) {
  return ranges.find((option) => option.id === id)?.label ?? id
}

function bucketCount(count: number, label: string) {
  return `${count} ${label} bucket${count === 1 ? '' : 's'}`
}

function errorText(reason: unknown) {
  return reason instanceof Error ? reason.message : String(reason)
}

function formatBytes(value: number) {
  const units = ['B', 'kB', 'MB', 'GB', 'TB']
  let amount = value
  let unit = 0
  while (amount >= 1000 && unit < units.length - 1) {
    amount /= 1000
    unit++
  }
  return `${amount.toFixed(amount < 10 && unit > 0 ? 1 : 0)} ${units[unit]}`
}
