import { describe, expect, it } from 'vitest'
import { reportCSV, summarizeSeries } from './reports'
import type { Point, Series } from './api'

const series = (points: Point[]): Series => ({ device_id: 1, kind: 'site_wan_up', key: '', resolution: '5m', points })
const point = (ts: number, avg = 1, cnt = 1): Point => ({ ts, avg, min: avg, max: avg, cnt })

describe('report evidence', () => {
  it('weights samples while keeping missing intervals separate', () => {
    const result = summarizeSeries(series([point(300, 1, 3), point(900, 0)]), 0, 1200, [0, 1])
    expect(result).toMatchObject({ expected: 4, observed: 2, samples: 4, average: .75, coverage: .5 })
  })
  it('never turns no evidence into zero or full coverage', () => {
    expect(summarizeSeries(series([]), 0, 1200, [0, 1])).toMatchObject({ average: null, peak: null, observed: 0, coverage: 0 })
  })
  it('rejects corrupt, duplicate, unaligned and incomplete buckets', () => {
    const result = summarizeSeries(series([point(0), point(300), point(300), point(301), point(600, 2),
      point(900, NaN), point(1200, 1, -1), point(1500)]), 1, 1600, [0, 1])
    expect(result).toMatchObject({ expected: 4, observed: 1, samples: 1, average: 1 })
  })
  it('escapes CSV cells and prevents formula injection without changing numbers', () => {
    expect(reportCSV([[' =SUM(A1)', 'a,"b', -2, null]])).toBe('"\' =SUM(A1)","a,""b","-2",""\r\n')
  })
})
