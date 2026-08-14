import { useEffect, useRef } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import type { Point } from '../lib/api'

/**
 * The shared time chart.
 *
 * Three things it does deliberately.
 *
 * It draws the min/max band behind the average. A five-minute rollup averages
 * away exactly the spike an operator is looking for, and a line without its
 * band silently claims a smoothness the data does not have.
 *
 * It does NOT interpolate across gaps. A rate series produces no sample after a
 * reboot, a missed poll or a counter reset, and drawing a straight line over
 * that invents traffic that did not happen. uPlot leaves a real hole when the
 * value is null, which is why gaps are filled with null rather than dropped.
 *
 * It never uses a status colour for a series. Those are reserved (UI-SPEC §3):
 * a red line must mean "critical", not "the fourth series".
 */
export function TimeChart({
  points,
  label,
  format,
  height = 180,
  colour = 'var(--series-1)',
  resolution,
  window,
}: {
  points: Point[]
  label: string
  format: (v: number) => string
  height?: number
  colour?: string
  resolution?: string
  /** The requested [from, to] in unix seconds. */
  window?: [number, number]
}) {
  const host = useRef<HTMLDivElement>(null)
  const plot = useRef<uPlot | null>(null)

  useEffect(() => {
    if (!host.current) return
    const el = host.current

    const css = (name: string) =>
      getComputedStyle(document.documentElement)
        .getPropertyValue(name.replace('var(', '').replace(')', ''))
        .trim() || name

    const stroke = colour.startsWith('var(') ? css(colour) : colour
    const ink = css('--text-secondary')
    const grid = css('--border')

    const xs = points.map((p) => p.ts)
    const avg = points.map((p) => p.avg)
    const lo = points.map((p) => p.min)
    const hi = points.map((p) => p.max)

    const opts: uPlot.Options = {
      width: el.clientWidth || 600,
      height,
      padding: [8, 8, 0, 0],
      cursor: { y: false, points: { size: 5 } },
      legend: { show: false },
      // The x range is the window that was ASKED for, not the extent of the
      // data that came back. Letting uPlot infer it from the points makes a
      // single sample produce a degenerate range — it renders year labels for
      // data from this afternoon — and, worse, silently rescales so a series
      // with two hours of history looks identical to one with two weeks.
      scales: { x: { time: true, range: window ? () => window : undefined } },
      axes: [
        {
          stroke: ink,
          grid: { stroke: grid, width: 1 },
          ticks: { stroke: grid },
          font: '11px ui-sans-serif, system-ui, sans-serif',
        },
        {
          stroke: ink,
          grid: { stroke: grid, width: 1 },
          ticks: { stroke: grid },
          font: '11px ui-sans-serif, system-ui, sans-serif',
          size: 58,
          values: (_u, vals) => vals.map((v) => format(v)),
        },
      ],
      series: [
        { value: (_u, v) => (v == null ? '' : new Date(v * 1000).toLocaleString()) },
        // The band, drawn first so the average sits on top of it.
        { stroke: 'transparent', fill: hexWithAlpha(stroke, 0.14), points: { show: false } },
        { stroke: 'transparent', fill: 'transparent', points: { show: false } },
        {
          label,
          stroke,
          width: 1.5,
          points: { show: points.length < 40 },
          value: (_u, v) => (v == null ? 'no data' : format(v)),
        },
      ],
      bands: [{ series: [1, 2], fill: hexWithAlpha(stroke, 0.14) }],
    }

    plot.current = new uPlot(opts, [xs, hi, lo, avg], el)
    const ro = new ResizeObserver(() => {
      plot.current?.setSize({ width: el.clientWidth, height })
    })
    ro.observe(el)
    return () => {
      ro.disconnect()
      plot.current?.destroy()
      plot.current = null
    }
  }, [points, label, format, height, colour, window])

  if (points.length === 0) {
    return (
      <div
        style={{
          height,
          display: 'grid',
          placeItems: 'center',
          color: 'var(--text-muted)',
          fontSize: 12,
          border: '1px dashed var(--border)',
          borderRadius: 6,
        }}
      >
        No data yet — telemetry is written every five minutes
      </div>
    )
  }

  return (
    <div>
      <div ref={host} />
      {resolution && (
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
          {resolution === '1h' ? 'hourly' : '5-minute'} rollup · shaded band is
          min/max within each bucket
        </div>
      )}
    </div>
  )
}

function hexWithAlpha(hex: string, alpha: number): string {
  const m = hex.trim().match(/^#([0-9a-f]{6})$/i)
  if (!m) return hex
  const n = parseInt(m[1], 16)
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`
}

// ---- value formatters ----

export const fmt = {
  bytesPerSec(v: number): string {
    const units = ['B/s', 'kB/s', 'MB/s', 'GB/s']
    let i = 0
    while (v >= 1000 && i < units.length - 1) {
      v /= 1000
      i++
    }
    return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`
  },
  percent: (v: number) => `${v.toFixed(v < 10 ? 1 : 0)}%`,
  dbm: (v: number) => `${v.toFixed(0)} dBm`,
  plain: (v: number) => (Number.isInteger(v) ? String(v) : v.toFixed(2)),
  count: (v: number) => v.toFixed(0),
}

/** Relative time, for "last seen" columns. */
export function ago(ts: number | null | undefined): string {
  if (!ts) return 'never'
  const s = Math.max(0, Math.floor(Date.now() / 1000) - ts)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

/** Uptime as a human duration. */
export function duration(sec: number): string {
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  return `${m}m`
}
