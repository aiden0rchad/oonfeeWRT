import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Reports } from './Reports'

const mocks = vi.hoisted(() => ({ dashboard: vi.fn(), stats: vi.fn() }))
vi.mock('../lib/api', () => ({ api: mocks }))

function dashboard(key: string | null = 'observed-wan') {
  return { wan: { target: '1.1.1.1', gateway: { name: 'Gateway', device_id: 7, series_key: key } } }
}

describe('Reports', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.dashboard.mockResolvedValue(dashboard())
    mocks.stats.mockImplementation((kind, device_id, key, from) => Promise.resolve({
      kind, device_id, key, resolution: '5m', points: [{ ts: from, avg: 1, min: 1, max: 1, cnt: 2 }],
    }))
  })
  it('compares stored observations from the proven gateway without guessing an interface', async () => {
    render(<Reports />)
    await screen.findByRole('heading', { name: 'Gateway' })
    expect(mocks.stats).toHaveBeenCalledTimes(10)
    expect(mocks.stats).toHaveBeenCalledWith('iface_rx_bps', 7, 'observed-wan', expect.any(Number), expect.any(Number))
    expect(screen.getByText('100.00%')).toBeTruthy()
    expect(screen.getAllByText('1 / 2016 intervals observed')).toHaveLength(5)
    expect(screen.getByRole('button', { name: 'Export CSV' }).hasAttribute('disabled')).toBe(false)
  })
  it('does not invent traffic sources or query without a gateway', async () => {
    mocks.dashboard.mockResolvedValue(dashboard(null))
    const view = render(<Reports />)
    await screen.findByRole('heading', { name: 'Gateway' })
    expect(mocks.stats).toHaveBeenCalledTimes(6)
    expect(screen.getAllByText('No verified WAN interface series')).toHaveLength(2)
    view.unmount()
    mocks.stats.mockClear()
    mocks.dashboard.mockResolvedValue({ wan: { gateway: null } })
    render(<Reports />)
    await screen.findByText('No gateway evidence yet')
    expect(mocks.stats).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Export CSV' }).hasAttribute('disabled')).toBe(true)
  })
  it('shows errors independently rather than presenting an unavailable report as zero', async () => {
    mocks.stats.mockRejectedValue(new Error('collection unavailable'))
    render(<Reports />)
    await screen.findByRole('heading', { name: 'Gateway' })
    expect(screen.getAllByText('Could not load: collection unavailable')).toHaveLength(5)
    expect(screen.getByRole('button', { name: 'Export CSV' }).hasAttribute('disabled')).toBe(true)
  })
  it('discards a previous period request that completes after a new selection', async () => {
    let resolveOld!: (value: unknown) => void
    mocks.dashboard.mockReturnValueOnce(new Promise((resolve) => { resolveOld = resolve }))
    render(<Reports />)
    fireEvent.click(screen.getByRole('button', { name: '24 hours' }))
    await screen.findByRole('heading', { name: 'Gateway' })
    expect(screen.getAllByText('1 / 288 intervals observed')).toHaveLength(5)
    await act(async () => { resolveOld({ wan: { gateway: null } }) })
    await waitFor(() => expect(screen.queryByText('No gateway evidence yet')).toBeNull())
  })
  it.each([
    ['30 days', 30], ['Refresh report', 7],
  ] as const)('invalidates the previous report immediately after %s until fresh data is ready', async (selection, expectedDays) => {
    const createURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:fixture-report')
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    try {
      render(<Reports />)
      await screen.findByRole('heading', { name: 'Gateway' })
      let resolveNext!: (value: unknown) => void
      mocks.dashboard.mockReturnValueOnce(new Promise((resolve) => { resolveNext = resolve }))

      fireEvent.click(screen.getByRole('button', { name: selection }))
      const exportButton = screen.getByRole('button', { name: 'Export CSV' })
      expect(exportButton.hasAttribute('disabled')).toBe(true)
      expect(screen.queryByRole('heading', { name: 'Gateway' })).toBeNull()
      fireEvent.click(exportButton)
      expect(createURL).not.toHaveBeenCalled()

      await act(async () => { resolveNext(dashboard()) })
      await screen.findByRole('heading', { name: 'Gateway' })
      expect(screen.getAllByText(`1 / ${expectedDays * 288} intervals observed`)).toHaveLength(5)
      expect(exportButton.hasAttribute('disabled')).toBe(false)
      fireEvent.click(exportButton)
      const csv = await (createURL.mock.calls[0][0] as Blob).text()
      const row = csv.split(/\r?\n/)[1].split(',')
      expect(Date.parse(JSON.parse(row[4])) - Date.parse(JSON.parse(row[3]))).toBe(expectedDays * 86_400_000)
    } finally { createURL.mockRestore(); click.mockRestore() }
  })
  it('replaces positive, negative, and exact zero comparisons at display precision without rounding CSV values', async () => {
    const values: Record<string, [number, number]> = {
      site_wan_up: [0.900001, 0.9], site_wan_latency_ms: [9.999, 10], site_wan_loss_pct: [0.9999, 1],
      iface_rx_bps: [100.1, 100], iface_tx_bps: [100, 100],
    }
    const calls = new Map<string, number>()
    mocks.stats.mockImplementation((kind, device_id, key, from) => {
      const index = calls.get(kind) ?? 0
      calls.set(kind, index + 1)
      const value = values[kind][index % 2]
      return Promise.resolve({ kind, device_id, key, resolution: '5m', points: [{ ts: from, avg: value, min: value, max: value, cnt: 1 }] })
    })
    const createURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:fixture-report')
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    try {
      render(<Reports />)
      await screen.findByRole('heading', { name: 'Gateway' })
      expect(screen.getAllByText('No change at displayed precision')).toHaveLength(5)
      fireEvent.click(screen.getByRole('button', { name: 'Export CSV' }))
      const csv = await (createURL.mock.calls[0][0] as Blob).text()
      expect(csv).toContain('"9.999","1","2016","1","10"')
      expect(csv).toContain('"0.9999","1","2016","1","1"')
      expect(csv).toContain('"100.1","1","2016","1","100"')
    } finally { createURL.mockRestore(); click.mockRestore() }
  })
  it('keeps meaningful positive and negative comparison signs', async () => {
    const values: Record<string, [number, number]> = {
      site_wan_up: [0.91, 0.9], site_wan_latency_ms: [9.9, 10], site_wan_loss_pct: [1.01, 1],
      iface_rx_bps: [102, 100], iface_tx_bps: [99, 100],
    }
    const calls = new Map<string, number>()
    mocks.stats.mockImplementation((kind, device_id, key, from) => {
      const index = calls.get(kind) ?? 0
      calls.set(kind, index + 1)
      const value = values[kind][index % 2]
      return Promise.resolve({ kind, device_id, key, resolution: '5m', points: [{ ts: from, avg: value, min: value, max: value, cnt: 1 }] })
    })
    render(<Reports />)
    await screen.findByRole('heading', { name: 'Gateway' })
    for (const change of ['+1.00 pp', '−0.1 ms', '+0.01 pp', '+2 B/s', '−1 B/s']) {
      expect(screen.getByText(`${change} vs. prior observations`)).toBeTruthy()
    }
  })
})
