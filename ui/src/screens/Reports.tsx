import { useEffect, useState } from 'react'
import { api } from '../lib/api'
import type { Dashboard, Series } from '../lib/api'
import { summarizeSeries, reportCSV } from '../lib/reports'
import type { ReportSummary } from '../lib/reports'
import { Banner, Button, Card, PageHeader } from '../components/ui'
import { fmt } from '../components/Chart'
import './Reports.css'

const metrics = [
  { id: 'site_wan_up', label: 'Observed reachability', unit: '%', bounds: [0, 1], format: (n: number) => `${(n * 100).toFixed(2)}%`, scale: 100 },
  { id: 'site_wan_latency_ms', label: 'Average latency', unit: 'ms', bounds: [0, Number.MAX_VALUE], format: (n: number) => `${n.toFixed(1)} ms`, scale: 1 },
  { id: 'site_wan_loss_pct', label: 'Average packet loss', unit: '%', bounds: [0, 100], format: (n: number) => `${n.toFixed(2)}%`, scale: 1 },
  { id: 'iface_rx_bps', label: 'Average download', unit: 'B/s', bounds: [0, Number.MAX_VALUE], format: fmt.bytesPerSec, scale: 1 },
  { id: 'iface_tx_bps', label: 'Average upload', unit: 'B/s', bounds: [0, Number.MAX_VALUE], format: fmt.bytesPerSec, scale: 1 },
] as const

interface Result { current?: ReportSummary; previous?: ReportSummary; error?: string; previousError?: string }
interface Report {
  days: number; revision: number
  from: number; to: number; previousFrom: number; gateway: Dashboard['wan']['gateway']; target: string
  results: Record<string, Result>
}

const dateTime = (seconds: number) => new Date(seconds * 1000).toLocaleString()

export function Reports() {
  const [days, setDays] = useState(7)
  const [revision, setRevision] = useState(0)
  const [loadedReport, setReport] = useState<Report | null>(null)
  const report = loadedReport?.days === days && loadedReport.revision === revision ? loadedReport : null
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  useEffect(() => {
    let active = true
    setLoading(true)
    setReport(null)
    setError('')
    const run = async () => {
      try {
        const dashboard = await api.dashboard()
        // Hour alignment gives both periods the same complete-window boundary,
        // including hourly rollups for longer reports.
        const to = Math.floor(Date.now() / 3_600_000) * 3600
        const from = to - days * 86400
        const previousFrom = from - days * 86400
        const gateway = dashboard.wan.gateway
        const results: Record<string, Result> = {}
        if (gateway) await Promise.all(metrics.map(async (metric) => {
          const traffic = metric.id.startsWith('iface_')
          if (traffic && !gateway.series_key) return
          const key = traffic ? gateway.series_key! : ''
          const [current, previous] = await Promise.allSettled([
            api.stats(metric.id, gateway.device_id, key, from, to),
            api.stats(metric.id, gateway.device_id, key, previousFrom, from),
          ])
          const summarize = (series: Series, start: number, end: number) => {
            if (series.kind !== metric.id || series.device_id !== gateway.device_id || series.key !== key ||
              !['5m', '1h'].includes(series.resolution)) throw new Error('The controller returned a different series')
            return summarizeSeries(series, start, end, metric.bounds)
          }
          const result: Result = {}
          try {
            if (current.status === 'rejected') throw current.reason
            result.current = summarize(current.value, from, to)
          } catch (e) { result.error = e instanceof Error ? e.message : String(e) }
          try {
            if (previous.status === 'rejected') throw previous.reason
            result.previous = summarize(previous.value, previousFrom, from)
          } catch (e) { result.previousError = e instanceof Error ? e.message : String(e) }
          results[metric.id] = result
        }))
        if (active) setReport({ days, revision, from, to, previousFrom, gateway, target: dashboard.wan.target, results })
      } catch (e) {
        if (active) setError(e instanceof Error ? e.message : String(e))
      } finally { if (active) setLoading(false) }
    }
    void run()
    return () => { active = false }
  }, [days, revision])

  function exportReport() {
    if (!report) return
    const rows: Array<Array<string | number | null>> = [[
      'metric', 'unit', 'gateway', 'from_utc', 'to_utc', 'sample_weighted_average',
      'observed_buckets', 'expected_buckets', 'samples', 'previous_average', 'previous_observed_buckets', 'previous_expected_buckets',
    ]]
    for (const metric of metrics) {
      const result = report.results[metric.id]
      if (!result?.current) continue
      rows.push([metric.label, metric.unit, report.gateway?.name ?? '', new Date(report.from * 1000).toISOString(),
        new Date(report.to * 1000).toISOString(), result.current.average == null ? null : result.current.average * metric.scale,
        result.current.observed, result.current.expected, result.current.samples,
        result.previous?.average == null ? null : result.previous.average * metric.scale,
        result.previous?.observed ?? null, result.previous?.expected ?? null])
    }
    const url = URL.createObjectURL(new Blob([reportCSV(rows)], { type: 'text/csv;charset=utf-8' }))
    const link = document.createElement('a')
    link.href = url
    link.download = `oonfeewrt-report-${new Date(report.to * 1000).toISOString().slice(0, 10)}.csv`
    link.click()
    window.setTimeout(() => URL.revokeObjectURL(url), 1000)
  }

  return <div className="reports-page">
    <PageHeader title="Reports" purpose="A readable review of your network, grounded in the observations you actually have."
      actions={<><Button onClick={() => setRevision((n) => n + 1)} disabled={loading}>Refresh report</Button>
        <Button onClick={exportReport} disabled={loading || !report?.gateway || !Object.values(report?.results ?? {}).some((result) => result.current?.observed)}>Export CSV</Button></>} />
    <div className="reports-toolbar">
      <div className="reports-range" role="group" aria-label="Report period">
        {[1, 7, 30].map((value) => <Button key={value} aria-pressed={days === value} kind={days === value ? 'primary' : 'default'}
          onClick={() => setDays(value)}>{value === 1 ? '24 hours' : `${value} days`}</Button>)}
      </div>
      <span>Compared with the preceding equal-length period</span>
    </div>
    {loading && <div role="status">Preparing your report…</div>}
    {error && <div role="alert"><Banner tone="critical">Report unavailable: {error}</Banner></div>}
    {report && !report.gateway && <Card title="No gateway evidence yet"><p>A report needs a selected gateway with observed WAN data. Review Devices and Statistics to check collection. No router is contacted by this page.</p></Card>}
    {report?.gateway && <>
      <section className="reports-intro">
        <div><span className="reports-eyebrow">NETWORK REVIEW</span><h2>{report.gateway.name || 'Gateway'}</h2>
          <p>{dateTime(report.from)} — {dateTime(report.to)}</p></div>
        <div className="reports-intro-note">Completed intervals only<br /><strong>Unknown is not downtime</strong></div>
      </section>
      <div className="reports-metrics">
        {metrics.map((metric) => {
          const result = report.results[metric.id]
          const current = result?.current
          const previous = result?.previous
          const delta = current?.average != null && previous?.average != null ? current.average - previous.average : null
          const deltaLabel = delta == null ? null : metric.id === 'site_wan_up' || metric.id === 'site_wan_loss_pct'
            ? `${(Math.abs(delta) * metric.scale).toFixed(2)} pp` : metric.format(Math.abs(delta))
          const comparison = delta == null || deltaLabel == null ? 'No comparable prior observations'
            : Number.parseFloat(deltaLabel) === 0 ? 'No change at displayed precision'
              : `${delta > 0 ? '+' : '−'}${deltaLabel} vs. prior observations`
          return <article className="report-metric" key={metric.id}>
            <h3>{metric.label}</h3>
            <div className="report-value">{current?.average == null ? '—' : metric.format(current.average)}</div>
            <p className="report-comparison">{comparison}</p>
            <div className="report-coverage-track" aria-hidden="true"><span style={{ width: `${(current?.coverage ?? 0) * 100}%` }} /></div>
            <p className="report-coverage">{current ? `${current.observed} / ${current.expected} intervals observed` : metric.id.startsWith('iface_') && !report.gateway?.series_key ? 'No verified WAN interface series' : 'No report data'}</p>
            {result?.error && <p role="alert">Could not load: {result.error}</p>}
            {result?.previousError && <p role="alert">Prior period unavailable: {result.previousError}</p>}
            {previous && <p className="report-prior-coverage">Prior coverage: {previous.observed} / {previous.expected} intervals</p>}
          </article>
        })}
      </div>
      <Card title="What this report tells you">
        <div className="report-explanations">
          <div><h3>Reachability, not an uptime guarantee</h3><p>The gateway&apos;s probes to {report.target} responded in the observed samples shown. A missing sample can mean a collection gap; it is not proof that your Internet connection went down.</p></div>
          <div><h3>Comparable numbers need comparable coverage</h3><p>Averages are weighted by stored sample counts. Each period shows its own coverage, so a short burst of observations cannot quietly stand in for a whole month. Buckets with samples can still contain missed polls.</p></div>
          <div><h3>Traffic stays attached to its source</h3><p>Download and upload use the current gateway&apos;s verified interface key, including for the prior period. Route or gateway changes can reduce coverage. These are observed rates, not billed usage or per-client totals.</p></div>
        </div>
      </Card>
    </>}
  </div>
}
