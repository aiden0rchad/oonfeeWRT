import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Trend, WANMetric } from './Dashboard'

describe('dashboard WAN trend', () => {
  it('marks missing time without connecting across it and keeps isolated samples visible', () => {
    const { container } = render(<Trend label="Download traffic" points={[
      { ts: 0, value: 10 },
      { ts: 1, value: null },
      { ts: 2, value: 20 },
      { ts: 3, value: 30 },
      { ts: 4, value: null },
      { ts: 5, value: 40 },
    ]} />)

    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('4 available and 2 unavailable')
    expect(container.querySelectorAll('.dashboard-trend-gap')).toHaveLength(2)
    expect(container.querySelectorAll('.dashboard-trend-series')).toHaveLength(1)
    expect(container.querySelectorAll('.dashboard-trend-point')).toHaveLength(2)
  })

  it('centres a flat series instead of making it look clipped at the bottom', () => {
    const { container } = render(<Trend label="ICMP loss" points={[
      { ts: 0, value: 0 },
      { ts: 1, value: 0 },
      { ts: 2, value: 0 },
    ]} />)

    expect(container.querySelector('.dashboard-trend-series')?.getAttribute('d'))
      .toBe('M 0.0 19.0 L 90.0 19.0 L 180.0 19.0')
  })

  it('explains partial coverage beside the freshness note', () => {
    render(<WANMetric label="Download traffic" format={(value) => `${value} bps`} metric={{
      kind: 'download_bps',
      unit: 'bps',
      meaning: 'WAN download traffic',
      status: 'fresh',
      value: 30,
      as_of: Date.now(),
      points: [{ ts: 0, value: 10 }, { ts: 1, value: null }, { ts: 2, value: 30 }],
    }} />)

    expect(screen.getByText(/Updated .* · 1 missing/)).toBeTruthy()
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('2 available and 1 unavailable')
  })
})
