import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Dashboard, DashboardMetric, Device, Point, Series } from '../lib/api'
import { ReachabilityStrip, Statistics, alignChartPoints } from './Statistics'

const apiMocks = vi.hoisted(() => ({
  dashboard: vi.fn(),
  devices: vi.fn(),
  deviceSeries: vi.fn(),
  stats: vi.fn(),
}))

vi.mock('../lib/api', async (importOriginal) => {
  const original = await importOriginal<typeof import('../lib/api')>()
  return { ...original, api: { ...original.api, ...apiMocks } }
})

vi.mock('../components/Chart', () => ({
  TimeChart: ({ label, points }: { label: string; points: Array<{ avg: number | null }> }) => (
    <div
      data-testid={`chart-${label}`}
      data-missing={points.filter((point) => point.avg == null).length}
      data-observed={points.filter((point) => point.avg != null).length}
    >
      {label} chart
    </div>
  ),
  ago: () => 'recently',
  fmt: {
    bytesPerSec: (value: number) => `${value} B/s`,
    percent: (value: number) => `${value}%`,
    plain: (value: number) => String(value),
    dbm: (value: number) => `${value} dBm`,
  },
}))

const device: Device = {
  id: 7,
  mac: '02:00:00:00:00:07',
  name: 'Office gateway',
  host: '192.0.2.1',
  role: 'gateway',
  adopted: true,
  adopted_at: 1_700_000_000_000,
  class: 'router',
  firmware: 'OpenWrt 24.10',
  last_seen: 1_700_000_000_000,
  poll_state: 'ready',
  status: 'online',
}

function metric(kind: string): DashboardMetric {
  return {
    kind,
    unit: '',
    meaning: kind,
    status: 'fresh',
    value: 1,
    as_of: 1_700_000_000_000,
    points: [],
  }
}

function dashboard(seriesKey: string | null = 'wan-proof-key'): Dashboard {
  return {
    devices: { total: 1, online: 1, offline: 0, pending: 0, unknown: 0 },
    wireless_clients: 0,
    wireless_clients_complete: true,
    known_devices: 0,
    active_devices: 0,
    upstream_devices: 0,
    unscoped_devices: 0,
    gateway_uplinks: [{ device_id: 7, name: 'Office gateway', state: 'up' }],
    focused_devices: 0,
    quiesced_devices: 0,
    recent_events: [],
    recent_alert_events: [],
    series_count: 1,
    wan: {
      target: '1.1.1.1',
      probe: 'icmp',
      freshness: 'fresh',
      as_of: 1_700_000_000_000,
      gateway: {
        device_id: 7,
        name: 'Office gateway',
        route_interface: 'pppoe-wan',
        series_key: seriesKey,
      },
      resolution: '5m',
      bucket_ms: 300_000,
      from: 1_699_999_700_000,
      to: 1_700_000_000_000,
      metrics: {
        download_bps: metric('iface_rx_bps'),
        upload_bps: metric('iface_tx_bps'),
        latency_ms: metric('site_wan_latency_ms'),
        loss_pct: metric('site_wan_loss_pct'),
        reachable: metric('site_wan_up'),
      },
    },
  }
}

function storedSeries(kind: string, deviceID: number, key: string, from: number, to: number): Series {
  const resolution = to - from > 7 * 24 * 60 * 60 ? '1h' : '5m'
  const step = resolution === '1h' ? 3_600 : 300
  const ts = Math.ceil(from / step) * step
  return {
    device_id: deviceID,
    kind,
    key,
    resolution,
    points: ts < to ? [{ ts, avg: 1, min: 1, max: 1, cnt: 1 }] : [],
  }
}

describe('Statistics', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    apiMocks.dashboard.mockResolvedValue(dashboard())
    apiMocks.devices.mockResolvedValue({ devices: [device] })
    apiMocks.deviceSeries.mockResolvedValue({ series: { sys_load1: [''] } })
    apiMocks.stats.mockImplementation(storedSeries)
  })

  it('requests WAN traffic only with the server-proved series key', async () => {
    render(<Statistics />)

    expect(await screen.findByRole('heading', { name: 'Internet history' })).toBeTruthy()
    await waitFor(() => expect(apiMocks.stats).toHaveBeenCalledWith(
      'iface_rx_bps',
      7,
      'wan-proof-key',
      expect.any(Number),
      expect.any(Number),
    ))
    expect(apiMocks.stats).toHaveBeenCalledWith(
      'iface_tx_bps',
      7,
      'wan-proof-key',
      expect.any(Number),
      expect.any(Number),
    )
    expect(apiMocks.deviceSeries).toHaveBeenCalledWith(7)
    expect(apiMocks.stats.mock.calls.some((call) => call[2] === 'pppoe-wan')).toBe(false)

    const downloadCall = apiMocks.stats.mock.calls.find((call) => call[0] === 'iface_rx_bps')
    expect(downloadCall?.[4] - downloadCall?.[3]).toBe(6 * 60 * 60)
    expect(screen.getByRole('button', { name: '6 hours' }).getAttribute('aria-pressed')).toBe('true')
  })

  it('keeps partial history neutral and discloses exact missing counts and remedies on demand', async () => {
    render(<Statistics />)
    const summary = await screen.findByLabelText('Download traffic history coverage')
    expect(summary.textContent).toBe('History gaps')
    const details = summary.closest('details')!
    expect(details.open).toBe(false)
    fireEvent.click(summary)
    expect(details.open).toBe(true)
    expect(details.textContent).toMatch(/1 of \d+ completed intervals/)
    expect(details.textContent).toContain('blank space does not mean zero activity or downtime')
    expect(screen.queryByRole('alert')).toBeNull()
    const help = screen.getByText('About history and missing samples').closest('details')!
    expect(help.open).toBe(false)
    fireEvent.click(screen.getByText('About history and missing samples'))
    expect(help.open).toBe(true)
    expect(help.textContent).toContain('Keep the controller running and its devices reachable')
    expect(help.textContent).toContain('Past gaps cannot be filled retrospectively')
  })

  it('does not invent a WAN interface when the dashboard has no exact series match', async () => {
    apiMocks.dashboard.mockResolvedValue(dashboard(null))

    render(<Statistics />)

    expect(await screen.findByText(/WAN throughput is unavailable because the route-interface name/)).toBeTruthy()
    await waitFor(() => expect(apiMocks.stats).toHaveBeenCalledWith(
      'site_wan_latency_ms',
      7,
      '',
      expect.any(Number),
      expect.any(Number),
    ))
    expect(apiMocks.stats.mock.calls.some((call) => call[0] === 'iface_rx_bps' || call[0] === 'iface_tx_bps')).toBe(false)
    expect(screen.queryByRole('heading', { name: 'Download traffic' })).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Upload traffic' })).toBeNull()
    expect(screen.getByText(/ICMP history remains independent/)).toBeTruthy()
  })

  it('keeps successful metrics visible when one stored-series request fails', async () => {
    apiMocks.dashboard.mockResolvedValue(dashboard(null))
    apiMocks.stats.mockImplementation((kind, deviceID, key, from, to) => {
      if (kind === 'site_wan_loss_pct') return Promise.reject(new Error('rollup unavailable'))
      return Promise.resolve(storedSeries(kind, deviceID, key, from, to))
    })

    render(<Statistics />)

    expect(await screen.findByText(/1 Internet metric request failed/)).toBeTruthy()
    expect(screen.getByText(/Refresh failed: rollup unavailable/)).toBeTruthy()
    expect(screen.getByTestId('chart-ICMP latency').textContent).toContain('ICMP latency chart')
    expect(screen.getByText(/Successful series are current/)).toBeTruthy()
  })

  it.each(['gateway', 'interface'] as const)('does not relabel old WAN data after a %s change and failed refresh', async (change) => {
    render(<Statistics />)
    await waitFor(() => expect(screen.getByTestId('chart-Download traffic').getAttribute('data-observed')).toBe('1'))
    await waitFor(() => expect((screen.getByRole('button', { name: 'Refresh' }) as HTMLButtonElement).disabled).toBe(false))

    const next = dashboard(change === 'interface' ? 'new-wan-key' : 'wan-proof-key')
    if (change === 'gateway') {
      next.wan.gateway = { ...next.wan.gateway!, device_id: 8, name: 'Replacement gateway' }
    } else {
      next.wan.gateway = { ...next.wan.gateway!, route_interface: 'new-wan-key' }
    }
    apiMocks.dashboard.mockResolvedValue(next)
    let rejectSource!: (reason: Error) => void
    const replacement = new Promise<Series>((_resolve, reject) => { rejectSource = reject })
    apiMocks.stats.mockImplementation((kind, deviceID, key, from, to) =>
      deviceID === 8 || key === 'new-wan-key' ? replacement : storedSeries(kind, deviceID, key, from, to))

    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(apiMocks.stats).toHaveBeenCalledWith(
      'iface_rx_bps', next.wan.gateway!.device_id, next.wan.gateway!.series_key,
      expect.any(Number), expect.any(Number),
    ))
    expect(screen.getByTestId('chart-Download traffic').getAttribute('data-observed')).toBe('0')
    expect(screen.getByTestId('chart-Upload traffic').getAttribute('data-observed')).toBe('0')
    expect(screen.getByTestId('chart-ICMP latency').getAttribute('data-observed')).toBe(change === 'gateway' ? '0' : '1')
    expect(screen.getByRole('img', { name: /^ICMP reachability:/ }).getAttribute('aria-label')).toContain(
      change === 'gateway' ? 'no stored buckets' : '1 all-reply bucket',
    )

    rejectSource(new Error('replacement source unavailable'))
    await screen.findByText(new RegExp(`${change === 'gateway' ? 5 : 2} Internet metric requests failed`))
    expect(screen.getByTestId('chart-Download traffic').getAttribute('data-observed')).toBe('0')
    expect(screen.getByTestId('chart-Upload traffic').getAttribute('data-observed')).toBe('0')
    expect(screen.queryByText(/Last successful response retained/)).toBeNull()
  })

  it('does not reuse percentage history when the memory source changes to bytes', async () => {
    apiMocks.deviceSeries.mockResolvedValue({ series: { sys_mem_pct: [''] } })
    render(<Statistics />)
    await waitFor(() => expect(screen.getByTestId('chart-Memory in use').getAttribute('data-observed')).toBe('1'))
    await waitFor(() => expect((screen.getByRole('button', { name: 'Refresh' }) as HTMLButtonElement).disabled).toBe(false))

    apiMocks.deviceSeries.mockResolvedValue({ series: { sys_mem_used: [''] } })
    let rejectSource!: (reason: Error) => void
    const replacement = new Promise<Series>((_resolve, reject) => { rejectSource = reject })
    apiMocks.stats.mockImplementation((kind, deviceID, key, from, to) =>
      kind === 'sys_mem_used' ? replacement : storedSeries(kind, deviceID, key, from, to))
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(apiMocks.stats).toHaveBeenCalledWith(
      'sys_mem_used', 7, '', expect.any(Number), expect.any(Number),
    ))
    expect(screen.getByTestId('chart-Memory in use').getAttribute('data-observed')).toBe('0')
    rejectSource(new Error('memory source unavailable'))
    await screen.findByText(/Refresh failed: memory source unavailable/)
    expect(screen.getByTestId('chart-Memory in use').getAttribute('data-observed')).toBe('0')
    expect(screen.queryByText(/Last successful response retained/)).toBeNull()
  })

  it('retains recorded data when a refresh fails for the same source', async () => {
    render(<Statistics />)
    await waitFor(() => expect(screen.getByTestId('chart-Download traffic').getAttribute('data-observed')).toBe('1'))
    await waitFor(() => expect((screen.getByRole('button', { name: 'Refresh' }) as HTMLButtonElement).disabled).toBe(false))
    apiMocks.stats.mockRejectedValue(new Error('temporary storage failure'))
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))

    await screen.findByText(/5 Internet metric requests failed/)
    expect(screen.getByTestId('chart-Download traffic').getAttribute('data-observed')).toBe('1')
    expect(screen.getByRole('img', { name: /^ICMP reachability:/ }).getAttribute('aria-label')).toContain('1 all-reply bucket')
    expect(screen.getAllByText(/Last successful response retained/).length).toBeGreaterThan(0)
  })
})

describe('alignChartPoints', () => {
  it('inserts explicit null buckets instead of turning missing evidence into zero', () => {
    const observed: Point[] = [
      { ts: 0, avg: 12, min: 10, max: 14, cnt: 2 },
      { ts: 600, avg: 18, min: 17, max: 19, cnt: 2 },
    ]
    const aligned = alignChartPoints({
      data: {
        device_id: 7,
        kind: 'iface_rx_bps',
        key: 'wan-proof-key',
        resolution: '5m',
        points: observed,
      },
      range: '24h',
      window: [0, 1_200],
    }, observed)

    expect(aligned.map((point) => point.ts)).toEqual([0, 300, 600, 900])
    expect(aligned.map((point) => point.avg)).toEqual([12, null, 18, null])
    expect(aligned[1]).toEqual({ ts: 300, avg: null, min: null, max: null, cnt: 0 })
  })
})

describe('ReachabilityStrip', () => {
  it('distinguishes mixed replies from all-replied, no-reply, and missing buckets', () => {
    const points: Point[] = [
      { ts: 0, avg: 1, min: 1, max: 1, cnt: 3 },
      { ts: 300, avg: 0.5, min: 0, max: 1, cnt: 2 },
      { ts: 600, avg: 0, min: 0, max: 0, cnt: 3 },
    ]

    render(<ReachabilityStrip
      loaded={{
        data: {
          device_id: 7,
          kind: 'site_wan_up',
          key: '',
          resolution: '5m',
          points,
        },
        range: '24h',
        window: [0, 1_200],
      }}
      requestedRange="24h"
      loading={false}
    />)

    expect(screen.getByRole('img').getAttribute('aria-label')).toBe(
      'ICMP reachability: 1 all-reply bucket, 1 mixed-reply bucket, 1 no-reply bucket, and 1 missing bucket.',
    )
    expect(screen.getByText('All replied · 1')).toBeTruthy()
    expect(screen.getByText('Mixed replies · 1')).toBeTruthy()
    expect(screen.getByText('No replies · 1')).toBeTruthy()
    expect(screen.getByText('Missing · 1')).toBeTruthy()
  })
})
