import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Trend, WANMetric, wanTrendCeiling } from './Dashboard'

describe('dashboard WAN trend', () => {
  it('renders only observed samples as bars and consolidates missing runs', () => {
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
    expect(container.querySelectorAll('.dashboard-trend-bar')).toHaveLength(4)
    expect(container.querySelectorAll('.dashboard-trend-coverage')).toHaveLength(1)
    expect(container.querySelector('.dashboard-trend-series')).toBeNull()
  })

  it('keeps a real zero visible and rejects invalid loss samples', () => {
    const { container } = render(<Trend label="ICMP loss" points={[
      { ts: 0, value: 0 },
      { ts: 1, value: null },
      { ts: 2, value: -1 },
      { ts: 3, value: 101 },
      { ts: 4, value: Number.NaN },
    ]} kind="site_wan_loss_pct" unit="percent" tone="loss" format={(value) => `${value.toFixed(1)}%`} />)

    const trend = screen.getByRole('img')
    expect(trend.getAttribute('aria-label')).toContain('1 available and 4 unavailable')
    expect(trend.getAttribute('aria-label')).toContain('zero-based scale to 100.0%')
    const zero = container.querySelector('.dashboard-trend-bar[data-zero="true"]')
    expect(zero?.getAttribute('height')).toBe('2')
    expect(container.querySelectorAll('.dashboard-trend-bar')).toHaveLength(1)
    expect(container.querySelectorAll('.dashboard-trend-gap')).toHaveLength(1)
  })

  it('uses finite zero-based ceilings for empty and flat metric ranges', () => {
    expect(wanTrendCeiling('download_bps', [])).toBe(1)
    expect(wanTrendCeiling('download_bps', [0])).toBe(1)
    expect(wanTrendCeiling('site_wan_latency_ms', [0])).toBe(10)
    expect(wanTrendCeiling('site_wan_loss_pct', [0, 2])).toBe(100)
    expect(wanTrendCeiling('download_bps', [30])).toBe(40)
  })

  it('renders an explicit no-samples state instead of an empty plot', () => {
    const { container } = render(<Trend label="Upload traffic" points={[
      { ts: 0, value: null },
      { ts: 1, value: null },
    ]} tone="upload" />)

    expect(screen.getByRole('img', { name: 'Upload traffic: no samples available in the past six hours' })).toBeTruthy()
    expect(screen.getByText('No samples in the past 6 hours')).toBeTruthy()
    expect(container.querySelector('.dashboard-trend-bar')).toBeNull()
  })

  it('explains partial coverage in the chart without weakening freshness', () => {
    render(<WANMetric label="Download traffic" format={(value) => `${value} bps`} metric={{
      kind: 'download_bps',
      unit: 'bps',
      meaning: 'WAN download traffic',
      status: 'fresh',
      value: 30,
      as_of: Date.now(),
      points: [{ ts: 0, value: 10 }, { ts: 1, value: null }, { ts: 2, value: 30 }],
    }} tone="download" />)

    expect(screen.getByText(/Updated /)).toBeTruthy()
    expect(screen.getByText('2/3 samples')).toBeTruthy()
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('2 available and 1 unavailable')
  })
})
