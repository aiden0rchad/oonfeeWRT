import type { Point, Series } from './api'

export interface ReportSummary {
  points: Point[]
  expected: number
  observed: number
  samples: number
  average: number | null
  peak: number | null
  coverage: number
}

/** Only complete, aligned, valid buckets contribute. Coverage is time-bucket
 * coverage; it is deliberately not a claim that every poll succeeded. */
export function summarizeSeries(series: Series, from: number, to: number, bounds: readonly [number, number]): ReportSummary {
  const step = series.resolution === '1h' ? 3600 : 300
  const start = Math.ceil(from / step) * step
  const end = Math.floor(to / step) * step
  const unique = new Map<number, Point>()
  for (const point of series.points ?? []) {
    if (!Number.isSafeInteger(point.ts) || point.ts % step !== 0 || point.ts < start || point.ts + step > end ||
        !Number.isSafeInteger(point.cnt) || point.cnt <= 0 ||
        ![point.avg, point.min, point.max].every(Number.isFinite) ||
        point.min > point.avg || point.avg > point.max || point.min < bounds[0] || point.max > bounds[1]) continue
    unique.set(point.ts, point)
  }
  const points = [...unique.values()].sort((a, b) => a.ts - b.ts)
  const expected = Math.max(0, (end - start) / step)
  const samples = points.reduce((sum, point) => sum + point.cnt, 0)
  return {
    points, expected, observed: points.length, samples,
    average: samples ? points.reduce((sum, point) => sum + point.avg * (point.cnt / samples), 0) : null,
    peak: points.length ? Math.max(...points.map((point) => point.max)) : null,
    coverage: expected ? points.length / expected : 0,
  }
}

export function reportCSV(rows: Array<Array<string | number | null>>) {
  return rows.map((row) => row.map((value) => {
    let text = value == null ? '' : String(value)
    // Quoting alone does not prevent spreadsheet formula interpretation.
    if (typeof value === 'string' && /^[\s]*[=+@-]/.test(text)) text = `'${text}`
    return `"${text.replaceAll('"', '""')}"`
  }).join(',')).join('\r\n') + '\r\n'
}
