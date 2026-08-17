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
  note,
  emptyNote,
  minSpan,
}: {
  points: Point[]
  label: string
  format: (v: number, step?: number) => string
  height?: number
  colour?: string
  resolution?: string
  /** The narrowest y-range worth drawing, in the series' own units.
   *
   *  uPlot fits the axis to the data, so a series that barely moves gets
   *  magnified until its rounding noise fills the chart: channel occupancy
   *  sitting between 63.020% and 63.030% was drawn as a dramatic climb, with
   *  three-decimal labels too wide for the gutter and clipped to "i3.030%".
   *  That is the same wrong this file already refuses for the noise floor — a
   *  convincing line made of nothing. A flat series should look flat, so give
   *  percentages a floor of about a point. */
  minSpan?: number
  /** The requested [from, to] in unix seconds. */
  window?: [number, number]
  /** What this series measures, when two charts of the same quantity from
   *  different sources sit next to each other and the title cannot say it
   *  without becoming a sentence. Rendered with the resolution rather than in
   *  the title row, which is a flex row whose buttons a sentence squeezes. */
  note?: string
  /** What to say when there is nothing to draw.
   *
   *  The default explains the ordinary case — a series that is recorded on
   *  every poll and has not had five minutes yet. It is WRONG for a series
   *  collected only in the focused tier, where waiting is precisely the thing
   *  that does not help: the operator would close the panel to wait, and
   *  closing it is what stops the collection. Any chart whose series is not
   *  written on every baseline poll has to say so itself. */
  emptyNote?: string
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
      scales: {
        x: { time: true, range: window ? () => window : undefined },
        y: minSpan
          ? { range: (_u, dMin, dMax) => widenTo(dMin, dMax, minSpan) }
          : {},
      },
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
          values: (_u, vals) => axisLabels(vals, format),
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

  // Below the chart in BOTH states, deliberately. The note is what tells two
  // charts of the same quantity apart, and an empty one is exactly when that
  // matters most — "why is this one blank and the one above it not?" is a
  // question only the note answers.
  const footnote = (resolution || note) && (
    <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
      {note}
      {note && resolution && points.length > 0 && ' · '}
      {resolution && points.length > 0 && (
        <>
          {resolution === '1h' ? 'hourly' : '5-minute'} rollup · shaded band is
          min/max within each bucket
        </>
      )}
    </div>
  )

  if (points.length === 0) {
    return (
      <div>
        <div
          style={{
            height,
            display: 'grid',
            placeItems: 'center',
            color: 'var(--text-muted)',
            fontSize: 12,
            border: '1px dashed var(--border)',
            borderRadius: 6,
            padding: '0 12px',
            textAlign: 'center',
          }}
        >
          {emptyNote ?? 'No data yet — telemetry is written every five minutes'}
        </div>
        {footnote}
      </div>
    )
  }

  return (
    <div>
      <div ref={host} />
      {footnote}
    </div>
  )
}

function hexWithAlpha(hex: string, alpha: number): string {
  const m = hex.trim().match(/^#([0-9a-f]{6})$/i)
  if (!m) return hex
  const n = parseInt(m[1], 16)
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`
}

/**
 * Widen a data range to at least `min`, keeping it centred.
 *
 * Clamped at zero on the low side, because every series this is used for is a
 * percentage or a rate and a negative floor would draw axis room for values
 * that cannot occur.
 */
export function widenTo(
  dMin: number,
  dMax: number,
  min: number,
): [number, number] {
  const span = dMax - dMin
  if (!(min > 0) || span >= min) return [dMin, dMax]
  const pad = (min - span) / 2
  const lo = dMin - pad
  return lo < 0 ? [0, Math.max(min, dMax)] : [lo, dMax + pad]
}

/**
 * Label a y-axis's ticks, telling the formatter how far apart they are.
 *
 * Precision has to come from the SPACING between ticks, not from how big the
 * numbers are. Exported and named rather than left inline in the uPlot options,
 * because the property worth testing is this composition — that neighbouring
 * ticks come out as different strings — and a closure inside a chart config
 * cannot be reached by a test. The formatter alone passing its own tests proved
 * nothing about whether the axis ever handed it a step; it did not, and no test
 * noticed.
 *
 * What is still NOT covered, stated rather than implied: the single call site
 * in the uPlot options. Inlining `vals.map(v => format(v))` there again would
 * restore the bug and break no test, because that options object needs a real
 * canvas to reach. One line, and it is this one — treat it as load-bearing.
 */
export function axisLabels(
  vals: number[],
  format: (v: number, step?: number) => string,
): string[] {
  const step = vals.length > 1 ? Math.abs(vals[1] - vals[0]) : 0
  return vals.map((v) => format(v, step))
}

// ---- value formatters ----

/**
 * Decimals needed for two ticks `step` apart to render differently.
 *
 * Capped at three: past that the labels are wider than the information in them,
 * and an axis that needs four decimals to separate its ticks is telling you the
 * series is flat, which is better said by a flat line than by a precise one.
 */
function decimalsFor(step: number): number {
  return Math.min(3, Math.max(0, Math.ceil(-Math.log10(step))))
}

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
  /**
   * A percentage, with enough decimals that neighbouring ticks differ.
   *
   * `step` is the spacing between axis ticks when the caller knows it. Without
   * it the decimals were chosen from the VALUE's magnitude, which is the wrong
   * input: an axis spanning 0.6 points around 63 wants a decimal just as much
   * as one spanning 0.6 around 6, and rendered every tick as "63%" instead.
   * The magnitude rule stays as the fallback for callers with no axis — a
   * tooltip or a table cell, where one reading stands alone.
   */
  percent: (v: number, step?: number) =>
    `${v.toFixed(step === undefined || step <= 0 ? (v < 10 ? 1 : 0) : decimalsFor(step))}%`,
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
