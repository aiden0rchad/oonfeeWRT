import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { SpeedTestJob } from '../lib/api'
import { SpeedTestHistory, speedTestScale } from './Dashboard'

const finished = Date.UTC(2026, 7, 24, 12, 30)

function job(overrides: Partial<SpeedTestJob> = {}): SpeedTestJob {
  return {
    id: '11111111111111111111111111111111',
    plan_id: 'plan',
    state: 'completed',
    phase: 'complete',
    progress_percent: 100,
    provider: 'Cloudflare',
    method: 'single stream',
    provenance: 'controller-host',
    endpoint: 'speed.example',
    estimated_bytes: 15_000_000,
    created_at: finished - 30_000,
    finished_at: finished,
    download_mbps: 125.3,
    upload_mbps: 107.4,
    idle_latency_ms: 18.2,
    idle_jitter_ms: 2.4,
    loaded_latency_ms: null,
    loaded_jitter_ms: null,
    bytes_downloaded: 12_000_000,
    bytes_uploaded: 3_000_000,
    ...overrides,
  }
}

describe('dashboard speed-test history chart', () => {
  it('uses one shared zero-based Mbps scale and keeps missing results out of the bars', () => {
    const failed = job({
      id: '22222222222222222222222222222222',
      state: 'failed',
      download_mbps: null,
      upload_mbps: null,
      error: 'cancelled by operator',
    })
    const partial = job({
      id: '33333333333333333333333333333333',
      finished_at: finished - 86_400_000,
      download_mbps: 0,
      upload_mbps: null,
    })

    render(<SpeedTestHistory jobs={[job(), failed, partial]} />)

    expect(screen.getByLabelText(/Throughput history for 2 completed tests, shared scale zero to/)).toBeTruthy()
    expect(screen.getByLabelText('Throughput series legend').textContent).toContain('Download')
    expect(screen.getByLabelText('Throughput series legend').textContent).toContain('Upload')
    const meters = screen.getAllByRole('meter')
    expect(meters).toHaveLength(3)
    expect(meters.every((meter) => meter.getAttribute('aria-valuemin') === '0')).toBe(true)
    expect(new Set(meters.map((meter) => meter.getAttribute('aria-valuemax'))).size).toBe(1)
    expect(meters.find((meter) => meter.getAttribute('aria-valuenow') === '0')
      ?.querySelector<HTMLElement>('.speedtest-bar-fill')?.style.width).toBe('0%')
    expect(screen.getByText(/cancelled by operator/)).toBeTruthy()
    expect(screen.getAllByText('Loaded unavailable')).toHaveLength(2)
    expect(screen.getAllByText('—')).toHaveLength(1)
  })

  it('provides keyboard-reachable per-bar details with exact millisecond timestamps', () => {
    render(<SpeedTestHistory jobs={[job()]} />)

    const meter = screen.getByRole('meter', { name: 'Download throughput' })
    fireEvent.focus(meter)
    const tooltip = document.getElementById(meter.getAttribute('aria-describedby') || '')
    expect(tooltip?.getAttribute('role')).toBe('tooltip')
    expect(tooltip?.textContent).toContain('Download 125.3 Mbps')
    expect(tooltip?.textContent).toContain(new Date(finished).toLocaleString())
    expect(tooltip?.textContent).toContain('Controller host/container')
  })

  it('keeps every exact result and unavailable field in the table view', () => {
    const failed = job({
      id: '44444444444444444444444444444444',
      state: 'failed',
      download_mbps: null,
      upload_mbps: null,
      error: 'provider request failed',
    })
    render(<SpeedTestHistory jobs={[job(), failed]} />)

    fireEvent.click(screen.getByRole('button', { name: 'Table' }))
    expect(screen.getByRole('button', { name: 'Table' }).getAttribute('aria-pressed')).toBe('true')
    const table = screen.getByRole('table')
    expect(within(table).getAllByRole('row')).toHaveLength(3)
    expect(within(table).getByText('provider request failed')).toBeTruthy()
    expect(within(table).getAllByText('—').length).toBeGreaterThanOrEqual(6)
    expect(within(table).getAllByText(/Controller host\/container/)).toHaveLength(2)
  })

  it('keeps a finite scale for zero-only and empty histories', () => {
    expect(speedTestScale([])).toBe(1)
    expect(speedTestScale([job({ download_mbps: 0, upload_mbps: 0 })])).toBe(1)
    expect(speedTestScale([job({ download_mbps: 216, upload_mbps: 412 })])).toBe(500)
  })
})
