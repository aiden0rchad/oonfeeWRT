import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import type { RadiosResponse } from '../lib/api'

const api = {
  radios: vi.fn(),
  stats: vi.fn(),
  scanRadio: vi.fn(),
}

const live = { watch: vi.fn() }

vi.mock('../lib/api', () => ({ api }))
vi.mock('../lib/live', () => ({ live }))

const { Radios } = await import('./Radios')

const response: RadiosResponse = {
  generated_at: 1_787_100_000_000,
  gaps: [],
  devices: [{
    device_id: 7,
    name: 'Gateway AP',
    status: { observed_at: 1_787_099_940_000, last_poll_at: 1_787_099_940_000,
      last_poll_ok: true, consecutive_failures: 0, stale: false },
    radios: [{
      radio_key: 'radio0',
      up: true,
      band: '5g',
      configured_channel: 'auto',
      htmode: 'VHT80',
      current_mhz: 5180,
      current_channel: 36,
      inventory_observed_at: 1_787_099_940_000,
      channels_observed_at: 1_787_099_940_000,
      stale: false,
      interfaces: [{ name: 'phy0-ap0', mode: 'ap' }, { name: 'phy0-ap1', mode: 'ap' }],
      channels_known: true,
      channels: [
        { band: '5g', channel: 36, mhz: 5180, state: 'in-use', availability: 'enabled', in_use: true, restricted: false, dfs: null, excluded: null, flags: [] },
        { band: '5g', channel: 44, mhz: 5220, state: 'enabled', availability: 'enabled', in_use: false, restricted: false, dfs: null, excluded: null, flags: [] },
        { band: '5g', channel: 52, mhz: 5260, state: 'restricted', availability: 'restricted', in_use: false, restricted: true, dfs: null, excluded: null, flags: ['NO-IR'] },
      ],
      scan_capability: 'present',
      latest_observations: [],
      suggested: { channel: 44, mhz: 5220, score: 91.2, basis: 'scan-v1 spectral overlap',
        scan_id: 2, observed_at: 1_787_099_940_000 },
    }],
  }],
}

beforeEach(() => {
  vi.useRealTimers()
  vi.clearAllMocks()
  api.radios.mockResolvedValue(response)
  api.stats.mockImplementation(async (kind: string) => ({
    device_id: 7, kind, key: 'radio0', resolution: '5m',
    points: kind === 'radio_utilization_pct' ? [{ ts: Math.floor(Date.now() / 1000) - 60, avg: 23.4, min: 20, max: 25, cnt: 5 }] : [],
  }))
  api.scanRadio.mockResolvedValue({ scan: { id: 3, status: 'completed' }, observations: [] })
  live.watch.mockImplementation(() => vi.fn())
})

afterEach(() => vi.useRealTimers())

describe('Radios', () => {
  it('renders one stable radio, a categorical channel plan, and honest unavailable metrics', async () => {
    render(<Radios />)
    expect((await screen.findAllByText('Gateway AP')).length).toBeGreaterThan(0)
    expect(screen.getAllByText('radio0').length).toBeGreaterThan(0)
    const classification = screen.getByRole('group', { name: 'Warning: Channel classification' })
    expect(within(classification).getByText(/DFS and channel exclusions are unknown here/i)).toBeTruthy()
    const classificationToggle = within(classification).getByText('More information about channel classification')
    const classificationDetails = classificationToggle.closest('details')
    expect(classificationDetails?.open).toBe(false)
    fireEvent.click(classificationToggle)
    expect(classificationDetails?.open).toBe(true)
    expect(within(classification).getByText(/freqlist\.restricted/)).toBeTruthy()
    const plan = screen.getByRole('list', { name: /Channel plan for 1 radios/ })
    expect(within(plan).getByLabelText('Channel 52, restricted, DFS unknown, exclusion unknown')).toBeTruthy()

    const table = screen.getByRole('table', { name: 'Per-radio observability' })
    await waitFor(() => expect(within(table).getByText('23.4%')).toBeTruthy())
    expect(within(table).getAllByText('Unavailable').length).toBeGreaterThan(0)
    expect(within(table).getByText('Ch 44 · 91.2')).toBeTruthy()
    expect(within(table).getByText(/Based on scan .*scan-v1 spectral overlap/)).toBeTruthy()
    for (const [, , , from, to] of api.stats.mock.calls) {
      expect(to - from).toBe(20 * 60)
    }
  })

  it('summarizes partial radio coverage and discloses source gaps', async () => {
    api.radios.mockResolvedValueOnce({
      ...response,
      gaps: [
        'device:7/radio0: channel list unavailable',
        'device:8/radio1: inventory is stale',
      ],
    })

    render(<Radios />)

    const notice = await screen.findByRole('group', { name: 'Warning: Radio coverage' })
    expect(within(notice).getByText(/2 source gaps are recorded/i)).toBeTruthy()
    expect(within(notice).getByText(/missing data is not rendered as zero/i)).toBeTruthy()
    const toggle = within(notice).getByText('More information about radio coverage')
    const details = toggle.closest('details')
    expect(details?.open).toBe(false)
    fireEvent.click(toggle)
    expect(details?.open).toBe(true)
    expect(within(notice).getByText('device:7/radio0: channel list unavailable')).toBeTruthy()
    expect(within(notice).getByText('device:8/radio1: inventory is stale')).toBeTruthy()
  })

  it('distinguishes an initial inventory failure from an empty radio fleet', async () => {
    api.radios.mockRejectedValueOnce(new Error('inventory offline'))

    render(<Radios />)

    expect(await screen.findByText(/inventory offline — no radio inventory has loaded/)).toBeTruthy()
    expect(screen.getByText('Radio inventory could not load.')).toBeTruthy()
    expect(screen.queryByText('No radios match these filters.')).toBeNull()
    expect(screen.queryByText(/showing the last successful radio state/)).toBeNull()
    expect(api.stats).not.toHaveBeenCalled()
  })

  it('keeps a disabled radio with legacy null interfaces renderable', async () => {
    const disabled = structuredClone(response)
    ;(disabled.devices[0].radios[0] as unknown as { interfaces: null }).interfaces = null
    api.radios.mockResolvedValueOnce(disabled)

    render(<Radios />)

    expect((await screen.findAllByText('Gateway AP')).length).toBeGreaterThan(0)
    expect(screen.queryByRole('button', { name: 'Scan…' })).toBeNull()
    expect(screen.getAllByText('Unavailable').length).toBeGreaterThan(0)
  })

  it('retains and labels exact last-good inventory and metrics after a refresh failure', async () => {
    render(<Radios />)
    const table = await screen.findByRole('table', { name: 'Per-radio observability' })
    await waitFor(() => expect(within(table).getByText('23.4%')).toBeTruthy())

    api.radios.mockRejectedValueOnce(new Error('inventory refresh failed'))
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))

    expect(await screen.findByText(/inventory refresh failed — showing the last successful/)).toBeTruthy()
    expect(screen.getAllByText('Gateway AP').length).toBeGreaterThan(0)
    expect(within(table).getByText('23.4%')).toBeTruthy()
    expect(screen.getAllByText(/Last known · refresh failed/).length).toBeGreaterThan(0)
    expect(screen.queryByText('Radio inventory could not load.')).toBeNull()
  })

  it('updates successful metrics while retaining only each failed metric last-good', async () => {
    let refreshing = false
    api.stats.mockImplementation(async (kind: string) => {
      const point = (avg: number) => ({
        device_id: 7, kind, key: 'radio0', resolution: '5m',
        points: [{ ts: Math.floor(Date.now() / 1000) - 60, avg, min: avg, max: avg, cnt: 1 }],
      })
      if (kind === 'radio_utilization_pct') return point(refreshing ? 31 : 23)
      if (kind === 'radio_interference_pct') {
        if (refreshing) throw new Error('interference source offline')
        return point(11)
      }
      return { device_id: 7, kind, key: 'radio0', resolution: '5m', points: [] }
    })

    render(<Radios />)
    const table = await screen.findByRole('table', { name: 'Per-radio observability' })
    await waitFor(() => expect(within(table).getByText('23.0%')).toBeTruthy())
    expect(within(table).getByText('11.0%')).toBeTruthy()

    refreshing = true
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))

    await waitFor(() => expect(within(table).getByText('31.0%')).toBeTruthy())
    expect(within(table).getByText('11.0%')).toBeTruthy()
    expect(within(table).getByText('Last known · refresh failed')).toBeTruthy()
    expect(screen.getByText(/1 radio metric request failed to refresh/)).toBeTruthy()
  })

  it('does not let an older refresh replace a newer response', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    let resolveOld!: (value: RadiosResponse) => void
    let resolveNew!: (value: RadiosResponse) => void
    const oldRefresh = new Promise<RadiosResponse>((resolve) => { resolveOld = resolve })
    const newRefresh = new Promise<RadiosResponse>((resolve) => { resolveNew = resolve })
    api.radios
      .mockResolvedValueOnce(response)
      .mockReturnValueOnce(oldRefresh)
      .mockReturnValueOnce(newRefresh)

    let unmount = () => {}
    try {
      ;({ unmount } = render(<Radios />))
      await waitFor(() => expect(screen.getAllByText('Gateway AP').length).toBeGreaterThan(0))

      fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
      expect(api.radios).toHaveBeenCalledTimes(2)
      await act(async () => {
        vi.advanceTimersByTime(5 * 60 * 1000)
      })
      expect(api.radios).toHaveBeenCalledTimes(3)

      const newest = structuredClone(response)
      newest.devices[0].name = 'Newest AP'
      await act(async () => { resolveNew(newest) })
      await waitFor(() => expect(screen.getAllByText('Newest AP').length).toBeGreaterThan(0))

      const stale = structuredClone(response)
      stale.devices[0].name = 'Stale AP'
      await act(async () => { resolveOld(stale) })
      expect(screen.queryByText('Stale AP')).toBeNull()
      expect(screen.getAllByText('Newest AP').length).toBeGreaterThan(0)
    } finally {
      unmount()
      vi.useRealTimers()
    }
  })

  it('focuses and traps the confirmation, requires acknowledgement, then restores focus', async () => {
    render(<Radios />)
    const trigger = await screen.findByRole('button', { name: 'Scan…' }) as HTMLButtonElement
    trigger.focus()
    fireEvent.click(trigger)
    const dialog = screen.getByRole('dialog', { name: /Scan Gateway AP · radio0/ })
    await waitFor(() => expect(document.activeElement).toBe(dialog))
    const run = screen.getByRole('button', { name: 'Run one scan' }) as HTMLButtonElement
    expect(run.disabled).toBe(true)
    fireEvent.keyDown(dialog, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancel' }))
    fireEvent.click(screen.getByRole('checkbox'))
    expect(run.disabled).toBe(false)
    fireEvent.click(run)
    await waitFor(() => expect(api.scanRadio).toHaveBeenCalledWith(7, 'radio0', true))
    expect(api.scanRadio).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(document.activeElement).toBe(trigger))
    expect(screen.getByRole('status').textContent).toMatch(/scan completed/)
  })

  it('keeps a failed scan perceivable inside a pointer-blocking modal', async () => {
    api.scanRadio.mockRejectedValueOnce(new Error('device refused the scan'))
    render(<Radios />)
    fireEvent.click(await screen.findByRole('button', { name: 'Scan…' }))
    const dialog = screen.getByRole('dialog', { name: /Scan Gateway AP · radio0/ }) as HTMLDivElement
    expect(dialog.style.position).toBe('fixed')
    expect(dialog.previousElementSibling?.hasAttribute('inert')).toBe(true)
    fireEvent.click(screen.getByRole('checkbox'))
    const run = screen.getByRole('button', { name: 'Run one scan' })
    fireEvent.click(run)

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toMatch(/Scan failed: device refused the scan/)
    expect(screen.getByRole('dialog')).toBe(dialog)
    expect(document.activeElement).toBe(alert)
  })

  it('labels a 23-hour-old rollup stale while keeping its exact evidence', async () => {
    api.stats.mockImplementation(async (kind: string) => ({
      device_id: 7, kind, key: 'radio0', resolution: '5m',
      points: kind === 'radio_utilization_pct'
        ? [{ ts: Math.floor(Date.now() / 1000) - 23 * 3600, avg: 37, min: 37, max: 37, cnt: 1 }]
        : [],
    }))
    render(<Radios />)
    const table = await screen.findByRole('table', { name: 'Per-radio observability' })
    await waitFor(() => expect(within(table).getByText('Stale')).toBeTruthy())
    expect(within(table).getByText('37.0%')).toBeTruthy()
  })

  it('does not offer a scan for configured Wi-Fi with no runtime interface', async () => {
    const absent = structuredClone(response)
    absent.devices[0].radios[0].interfaces = [{ name: '', mode: 'ap' }]
    api.radios.mockResolvedValue(absent)
    render(<Radios />)
    await screen.findByRole('table', { name: 'Per-radio observability' })
    expect(screen.queryByRole('button', { name: 'Scan…' })).toBeNull()
  })

  it('focuses every shown device, releases focus, and refreshes after one rollup window', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const second = structuredClone(response.devices[0])
    second.device_id = 8
    second.name = 'Second AP'
    const focused = { ...response, devices: [response.devices[0], second] }
    api.radios.mockResolvedValue(focused)
    const releases = [vi.fn(), vi.fn()]
    live.watch.mockImplementationOnce(() => releases[0]).mockImplementationOnce(() => releases[1])

    const view = render(<Radios />)
    await waitFor(() => expect(live.watch.mock.calls.map(([id]) => id)).toEqual([7, 8]))
    const before = api.radios.mock.calls.length
    await act(async () => {
      vi.advanceTimersByTime(5 * 60 * 1000)
      await Promise.resolve()
    })
    await waitFor(() => expect(api.radios.mock.calls.length).toBeGreaterThan(before))
    view.unmount()
    expect(releases[0]).toHaveBeenCalledTimes(1)
    expect(releases[1]).toHaveBeenCalledTimes(1)
    vi.useRealTimers()
  })
})
