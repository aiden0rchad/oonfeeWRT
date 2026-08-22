import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent as ReactKeyboardEvent } from 'react'
import { api } from '../lib/api'
import { live } from '../lib/live'
import type { RadioChannel, RadiosResponse, RadioView } from '../lib/api'
import { Banner, Button, Card, Stat } from '../components/ui'

const metricKinds = [
  'radio_utilization_pct',
  'radio_interference_pct',
  'radio_rx_airtime_pct',
  'radio_tx_airtime_pct',
  'radio_retry_delta_pct',
  'radio_tx_fail_delta_pct',
  'radio_signal_avg_dbm',
] as const

type MetricKind = typeof metricKinds[number]
type MetricValue = { value?: number; ts?: number; refreshFailed?: boolean }
type Metrics = Record<string, Partial<Record<MetricKind, MetricValue>>>

const metricFreshnessSeconds = 15 * 60
// One 5m rollup bucket beyond the freshness contract catches a point whose
// boundary falls just before `from` without fetching a day we never display.
const metricQueryBoundaryAllowanceSeconds = 5 * 60
const metricRefreshMilliseconds = 5 * 60 * 1000

const stateColours: Record<RadioChannel['state'], string> = {
  'in-use': 'var(--accent)',
  enabled: 'var(--good)',
  restricted: 'var(--warning)',
  unknown: 'var(--text-muted)',
}

const radioID = (deviceID: number, key: string) => `${deviceID}/${key}`

export function Radios() {
  const [data, setData] = useState<RadiosResponse | null>(null)
  const [metrics, setMetrics] = useState<Metrics>({})
  const [deviceFilter, setDeviceFilter] = useState('all')
  const [bandFilter, setBandFilter] = useState('all')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [metricError, setMetricError] = useState('')
  const [scanTarget, setScanTarget] = useState<{ deviceID: number; name: string; radio: RadioView } | null>(null)
  const [acknowledged, setAcknowledged] = useState(false)
  const [scanning, setScanning] = useState(false)
  const [scanError, setScanError] = useState('')
  const [notice, setNotice] = useState('')
  const dialogRef = useRef<HTMLDivElement>(null)
  const scanAlertRef = useRef<HTMLDivElement>(null)
  const dialogWasOpenRef = useRef(false)
  const returnFocusRef = useRef<HTMLButtonElement | null>(null)
  const requestGeneration = useRef(0)
  const metricsRef = useRef<Metrics>({})

  const load = useCallback(async () => {
    const generation = ++requestGeneration.current
    setLoading(true)
    try {
      const next = await api.radios()
      if (generation !== requestGeneration.current) return
      const now = Math.floor(Date.now() / 1000)
      const entries = await Promise.all(next.devices.flatMap((device) => device.radios.flatMap((radio) =>
        metricKinds.map(async (kind) => {
          try {
            const series = await api.stats(
              kind,
              device.device_id,
              radio.radio_key,
              now - metricFreshnessSeconds - metricQueryBoundaryAllowanceSeconds,
              now,
            )
            const point = series.points.at(-1)
            return [radioID(device.device_id, radio.radio_key), kind,
              point ? { value: point.avg, ts: point.ts } : undefined, false] as const
          } catch {
            return [radioID(device.device_id, radio.radio_key), kind, undefined, true] as const
          }
        }),
      )))
      if (generation !== requestGeneration.current) return
      const nextMetrics: Metrics = {}
      let failures = 0
      for (const [id, kind, value, failed] of entries) {
        if (failed) failures++
        const retained = failed
          ? { ...(metricsRef.current[id]?.[kind] ?? {}), refreshFailed: true }
          : value
        if (retained == null) continue
        nextMetrics[id] = { ...(nextMetrics[id] ?? {}), [kind]: retained }
      }
      metricsRef.current = nextMetrics
      setData(next)
      setMetrics(nextMetrics)
      setError('')
      setMetricError(failures === 0 ? '' :
        `${failures} radio metric ${failures === 1 ? 'request' : 'requests'} failed to refresh. ` +
        'Successful metrics are current; failed metrics retain and label their last evidence.')
    } catch (e) {
      if (generation !== requestGeneration.current) return
      setError(e instanceof Error ? e.message : String(e))
      setMetricError('')
      const retained = markMetricsRefreshFailed(metricsRef.current)
      metricsRef.current = retained
      setMetrics(retained)
    } finally {
      if (generation === requestGeneration.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
    return () => { requestGeneration.current++ }
  }, [load])

  const watchKey = (data?.devices ?? []).map((device) => device.device_id).sort((a, b) => a - b).join(',')
  useEffect(() => {
    if (!watchKey) return
    const releases = watchKey.split(',').map(Number).map((id) => live.watch(id))
    const refresh = window.setInterval(() => void load(), metricRefreshMilliseconds)
    return () => {
      window.clearInterval(refresh)
      releases.forEach((release) => release())
    }
  }, [watchKey, load])

  useEffect(() => {
    if (scanTarget) {
      dialogWasOpenRef.current = true
      dialogRef.current?.focus()
    } else if (dialogWasOpenRef.current) {
      dialogWasOpenRef.current = false
      returnFocusRef.current?.focus()
    }
  }, [scanTarget])

  useEffect(() => { if (scanError) scanAlertRef.current?.focus() }, [scanError])

  const rows = useMemo(() => (data?.devices ?? []).flatMap((device) =>
    device.radios.map((radio) => ({ deviceID: device.device_id, deviceName: device.name, radio })))
    .filter((row) => deviceFilter === 'all' || String(row.deviceID) === deviceFilter)
    .filter((row) => bandFilter === 'all' || row.radio.band === bandFilter),
  [data, deviceFilter, bandFilter])

  const channels = rows.reduce((count, row) => count + row.radio.channels.length, 0)
  const knownPlans = rows.filter((row) => row.radio.channels_known).length
  const showingLastGood = error !== '' && data !== null

  const runScan = async () => {
    if (!scanTarget || !acknowledged) return
    setScanning(true)
    setScanError('')
    try {
      await api.scanRadio(scanTarget.deviceID, scanTarget.radio.radio_key, acknowledged)
      const completed = `${scanTarget.name} · ${scanTarget.radio.radio_key} scan completed.`
      closeScan()
      setNotice(completed)
      await load()
    } catch (e) {
      setScanError(e instanceof Error ? e.message : String(e))
    } finally {
      setScanning(false)
    }
  }

  const closeScan = () => {
    setScanTarget(null)
    setAcknowledged(false)
    setScanError('')
  }

  const trapDialog = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape' && !scanning) {
      event.preventDefault()
      closeScan()
      return
    }
    if (event.key !== 'Tab') return
    const focusable = Array.from(dialogRef.current?.querySelectorAll<HTMLElement>(
      'button:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex="0"]',
    ) ?? [])
    if (focusable.length === 0) return
    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    if (event.shiftKey && (document.activeElement === first || document.activeElement === dialogRef.current)) {
      event.preventDefault(); last.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault(); first.focus()
    }
  }

  return (
    <>
    <div inert={scanTarget != null} aria-hidden={scanTarget != null} style={{ display: 'grid', gap: 12 }}>
      <header style={{ display: 'flex', alignItems: 'end', justifyContent: 'space-between', gap: 12 }}>
        <div>
          <h1 style={{ margin: 0, fontSize: 20 }}>Radios &amp; Channel Plan</h1>
          <div style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            Stable UCI radios, measured channel occupancy, and explicit RF scans.
          </div>
        </div>
        <Button onClick={() => void load()} disabled={loading}>{loading ? 'Refreshing…' : 'Refresh'}</Button>
      </header>

      {error && <Banner tone="critical">
        {data == null
          ? `${error} — no radio inventory has loaded.`
          : `${error} — showing the last successful radio state.`}
      </Banner>}
      {metricError && <Banner tone="critical">{metricError}</Banner>}
      {notice && <div role="status"><Banner>{notice}</Banner></div>}
      {data && data.gaps.length > 0 && (
        <Banner>Radio coverage is partial: {data.gaps.join(', ')}. Missing data is not rendered as zero.</Banner>
      )}
      <Banner>
        DFS is unknown here. OpenWrt&apos;s <code>freqlist.restricted</code> flag is not proof of radar/DFS state,
        so restricted channels stay labelled Restricted. Channel exclusions are also unknown until a persisted
        exclusion model proves them.
      </Banner>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3,minmax(0,1fr))', gap: 10 }}>
        <Card><Stat label="Radios" value={data == null ? '—' : rows.length} /></Card>
        <Card><Stat label="Known channel plans" value={data == null ? '—' : `${knownPlans}/${rows.length}`} tone={data != null && knownPlans === rows.length ? 'good' : 'warning'} /></Card>
        <Card><Stat label="Reported channels" value={data == null ? '—' : channels} sub="Restricted is not relabelled as DFS" /></Card>
      </div>

      <Card title="Channel Plan" actions={(
        <div aria-label="Channel Plan legend" style={{ display: 'flex', gap: 10, fontSize: 11, color: 'var(--text-secondary)' }}>
          {(['in-use', 'enabled', 'restricted', 'unknown'] as const).map((state) => (
            <span key={state} style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
              <i aria-hidden style={{ width: 9, height: 9, borderRadius: 2, background: stateColours[state] }} />
              {state === 'in-use' ? 'In use' : state[0].toUpperCase() + state.slice(1)}
            </span>
          ))}
        </div>
      )}>
        <div style={{ display: 'flex', gap: 8, marginBottom: 12 }}>
          <label style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
            Access point{' '}
            <select aria-label="Filter by access point" value={deviceFilter} onChange={(e) => setDeviceFilter(e.target.value)}>
              <option value="all">All</option>
              {data?.devices.map((device) => <option key={device.device_id} value={device.device_id}>{device.name}</option>)}
            </select>
          </label>
          <label style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
            Band{' '}
            <select aria-label="Filter by band" value={bandFilter} onChange={(e) => setBandFilter(e.target.value)}>
              <option value="all">All</option><option value="2g">2.4 GHz</option><option value="5g">5 GHz</option><option value="6g">6 GHz</option>
            </select>
          </label>
        </div>
        {rows.length > 0 ? (
          <div role="list" aria-label={`Channel plan for ${rows.length} radios`} style={{ display: 'grid', gap: 10 }}>
            {rows.map((row) => (
              <div role="listitem" key={radioID(row.deviceID, row.radio.radio_key)} style={{ display: 'grid', gridTemplateColumns: '160px 1fr', gap: 10, alignItems: 'center' }}>
                <div style={{ fontSize: 12 }}>
                  <strong>{row.deviceName}</strong><br />
                  <span style={{ color: 'var(--text-secondary)' }}>{row.radio.radio_key} · {bandName(row.radio.band)}</span>
                  {(row.radio.stale || showingLastGood) && <><br /><span style={{ color: 'var(--warning)' }}>
                    {showingLastGood ? 'Last known · refresh failed' : `Last known · ${when(row.radio.inventory_observed_at)}`}
                  </span></>}
                </div>
                {row.radio.channels_known ? (
                  <div role="list" aria-label={`Channels for ${row.deviceName} ${row.radio.radio_key}`} style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
                    {row.radio.channels.map((channel) => (
                      <span
                        role="listitem"
                        key={`${channel.channel}/${channel.mhz}`}
                        title={channelLabel(channel, true)}
                        aria-label={channelLabel(channel, false)}
                        style={{ width: 34, height: 28, display: 'inline-grid', placeItems: 'center', borderRadius: 4,
                          background: stateColours[channel.state], color: channel.state === 'enabled' ? '#07140d' : '#fff', fontSize: 11, fontWeight: 600 }}
                      >{channel.channel}</span>
                    ))}
                  </div>
                ) : <span style={{ color: 'var(--text-muted)', fontSize: 12 }}>Channel list unavailable</span>}
              </div>
            ))}
          </div>
        ) : <div style={{ color: 'var(--text-secondary)' }}>
          {loading
            ? 'Loading radio inventory…'
            : error && data == null
              ? 'Radio inventory could not load.'
              : 'No radios match these filters.'}
        </div>}
      </Card>

      <Card title="Per-radio observability" pad={false}>
        <div style={{ overflowX: 'auto' }}>
          <table aria-label="Per-radio observability" style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
            <thead><tr style={{ textAlign: 'left', borderBottom: '1px solid var(--border)' }}>
              {['Device', 'Radio', 'Band', 'Channel', 'Utilization', 'Interference', 'RX/TX airtime', 'Retry / fail', 'Signal', 'Suggested', 'RF scan'].map((label) =>
                <th key={label} scope="col" style={{ padding: '8px 10px', whiteSpace: 'nowrap' }}>{label}</th>)}
            </tr></thead>
            <tbody>{rows.map((row) => {
              const values = metrics[radioID(row.deviceID, row.radio.radio_key)] ?? {}
              return (
                <tr key={radioID(row.deviceID, row.radio.radio_key)} style={{ borderBottom: '1px solid var(--border)' }}>
                  <td style={cell}>{row.deviceName}{(row.radio.stale || showingLastGood) && <div style={{ color: 'var(--warning)', fontSize: 10 }}>
                    {showingLastGood ? 'Last known · refresh failed' : `Last known · ${when(row.radio.inventory_observed_at)}`}
                  </div>}</td><td style={cell}><code>{row.radio.radio_key}</code></td>
                  <td style={cell}>{bandName(row.radio.band)}</td>
                  <td style={cell}>{row.radio.current_ambiguous ? 'Ambiguous' : row.radio.current_channel ?? 'Unknown'}{row.radio.htmode ? ` · ${row.radio.htmode}` : ''}</td>
                  <td style={cell}>{metric(values.radio_utilization_pct, '%')}</td>
                  <td style={cell}>{metric(values.radio_interference_pct, '%')}</td>
                  <td style={cell}>{metric(values.radio_rx_airtime_pct, '%')} / {metric(values.radio_tx_airtime_pct, '%')}</td>
                  <td style={cell}>{metric(values.radio_retry_delta_pct, '%')} / {metric(values.radio_tx_fail_delta_pct, '%')}</td>
                  <td style={cell}>{metric(values.radio_signal_avg_dbm, ' dBm')}</td>
                  <td style={cell}>{row.radio.suggested ? <>
                    <div>Ch {row.radio.suggested.channel} · {row.radio.suggested.score.toFixed(1)}</div>
                    <div style={{ color: 'var(--text-muted)', fontSize: 10, whiteSpace: 'normal', maxWidth: 260 }}>
                      Based on scan {when(row.radio.suggested.observed_at)}. {row.radio.suggested.basis}
                    </div>
                  </> : <Unknown />}</td>
                  <td style={cell}>
                    {row.radio.scan_capability === 'present' && (row.radio.interfaces ?? []).some((iface) => iface.name.trim() !== '') ? (
                      <Button onClick={() => {
                        returnFocusRef.current = document.activeElement instanceof HTMLButtonElement ? document.activeElement : null
                        setScanTarget({ deviceID: row.deviceID, name: row.deviceName, radio: row.radio }); setAcknowledged(false); setScanError(''); setNotice('')
                      }}>Scan…</Button>
                    ) : <span title={`Capability: ${row.radio.scan_capability}`}><Unknown /></span>}
                    {row.radio.latest_scan && <div style={{ color: 'var(--text-muted)', fontSize: 10, marginTop: 3 }}>{row.radio.latest_scan.status} · {row.radio.latest_observations.length} BSS · {when(row.radio.latest_scan.finished_at ?? row.radio.latest_scan.started_at)}</div>}
                  </td>
                </tr>
              )
            })}</tbody>
          </table>
        </div>
      </Card>
    </div>
    {scanTarget && (
      <div ref={dialogRef} role="dialog" aria-modal="true" aria-labelledby="radio-scan-title"
        tabIndex={-1} onKeyDown={trapDialog}
        style={{ position: 'fixed', inset: 0, zIndex: 1000, padding: 20, overflowY: 'auto',
          display: 'grid', placeItems: 'center', background: 'rgba(0,0,0,.62)' }}>
        <div style={{ width: 'min(560px, 100%)' }}>
          <Card title={<span id="radio-scan-title">Scan {scanTarget.name} · {scanTarget.radio.radio_key}</span>}>
            <Banner tone="critical">
              This takes one serving radio off-channel and may briefly interrupt every client on it. It targets only this device and this stable radio.
            </Banner>
            {scanError && <div ref={scanAlertRef} role="alert" tabIndex={-1} style={{ marginTop: 12 }}><Banner tone="critical">Scan failed: {scanError}</Banner></div>}
            <label style={{ display: 'flex', gap: 8, alignItems: 'start', margin: '12px 0', fontSize: 12 }}>
              <input type="checkbox" checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} />
              I understand this scan can disrupt connected clients on {scanTarget.radio.radio_key}.
            </label>
            <div style={{ display: 'flex', gap: 8 }}>
              <Button kind="primary" disabled={!acknowledged || scanning} onClick={() => void runScan()}>{scanning ? 'Scanning…' : 'Run one scan'}</Button>
              <Button disabled={scanning} onClick={closeScan}>Cancel</Button>
            </div>
          </Card>
        </div>
      </div>
    )}
    </>
  )
}

const cell = { padding: '8px 10px', verticalAlign: 'top', whiteSpace: 'nowrap' } as const

function Unknown() {
  return <span title="The required source or capability is unavailable" style={{ color: 'var(--text-muted)' }}>Unavailable</span>
}

function metric(point: MetricValue | undefined, suffix: string) {
  if (point == null) return <Unknown />
  if (point.value == null || point.ts == null) {
    return point.refreshFailed
      ? <span title="The latest metric request failed and no earlier sample is available" style={{ color: 'var(--warning)' }}>Refresh failed</span>
      : <Unknown />
  }
  const age = Math.floor(Date.now() / 1000) - point.ts
  const stale = age > metricFreshnessSeconds || age < -60
  const formatted = `${point.value.toFixed(1)}${suffix}`
  if (!stale && !point.refreshFailed) return formatted
  const label = [stale ? 'Stale' : 'Last known', point.refreshFailed ? 'refresh failed' : '']
    .filter(Boolean).join(' · ')
  return (
    <span title={`Last metric sample was ${Math.max(0, age)} seconds ago${point.refreshFailed ? '; the latest refresh failed' : ''}`}>
      <span>{formatted}</span><br />
      <span style={{ color: 'var(--warning)', fontSize: 10 }}>{label}</span>
    </span>
  )
}

function markMetricsRefreshFailed(metrics: Metrics): Metrics {
  const retained: Metrics = {}
  for (const [id, values] of Object.entries(metrics)) {
    retained[id] = Object.fromEntries(
      Object.entries(values).map(([kind, value]) => [kind, { ...value, refreshFailed: true }]),
    ) as Partial<Record<MetricKind, MetricValue>>
  }
  return retained
}

function bandName(band?: string) {
  return band === '2g' ? '2.4 GHz' : band === '5g' ? '5 GHz' : band === '6g' ? '6 GHz' : 'Unknown'
}

function when(ts?: number) {
  return ts ? new Date(ts).toLocaleString() : 'time unavailable'
}

function channelLabel(channel: RadioChannel, includeMHz: boolean) {
  const location = `Channel ${channel.channel}${includeMHz ? ` · ${channel.mhz} MHz` : ''}`
  const state = channel.in_use
    ? `in use, availability ${channel.availability}`
    : channel.availability
  return `${location}, ${state}, DFS unknown, exclusion unknown`
}
