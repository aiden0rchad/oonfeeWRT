import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Device, ClientPage } from '../lib/api'
import { Devices } from './Devices'
import { Clients } from './Clients'

const apiMocks = vi.hoisted(() => ({ clients: vi.fn(), devices: vi.fn() }))
vi.mock('../lib/api', async (importOriginal) => {
  const original = await importOriginal<typeof import('../lib/api')>()
  return { ...original, api: { ...original.api, ...apiMocks } }
})

const gateway: Device = {
  id: 1, mac: '02:00:00:00:00:01', name: 'Office gateway', host: '192.0.2.1', role: 'gateway',
  functions: ['gateway', 'ap', 'switch'], adopted: true, adopted_at: 1, class: null,
  firmware: 'OpenWrt 24.10.2', last_seen: 1, poll_state: 'ready', status: 'online',
}
const accessPoint: Device = {
  ...gateway, id: 2, mac: '02:00:00:00:00:02', name: 'Studio access point', host: '192.0.2.2',
  functions: ['ap'], role: 'ap', management_mode: 'monitor_only', status: 'offline', firmware: '', last_seen: null,
}

beforeEach(() => {
  vi.clearAllMocks()
  apiMocks.devices.mockResolvedValue({ devices: [gateway, accessPoint] })
})

describe('Device inventory presentation', () => {
  it('opens with illustrated cards and only known device facts', () => {
    render(<Devices devices={[gateway, accessPoint]} />)
    expect(screen.getByRole('button', { name: 'Cards' }).getAttribute('aria-pressed')).toBe('true')
    const card = screen.getByRole('button', { name: 'Open device Studio access point' })
    expect(within(card).getByText('Monitor only')).toBeTruthy()
    expect(within(card).getByText('Not read yet')).toBeTruthy()
    expect(within(card).getByText('Not polled yet')).toBeTruthy()
    expect(within(card).getByText('offline')).toBeTruthy()
    expect(card.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true')
    expect(screen.queryByRole('table')).toBeNull()
  })

  it('combines full-inventory search and status filtering, with a clear reset', () => {
    render(<Devices devices={[gateway, accessPoint]} />)
    fireEvent.change(screen.getByRole('searchbox', { name: 'Search devices' }), { target: { value: '24.10' } })
    expect(screen.getByRole('status').textContent).toBe('Showing 1 of 2 devices')
    expect(screen.queryByRole('button', { name: 'Open device Studio access point' })).toBeNull()
    fireEvent.change(screen.getByRole('combobox', { name: 'Status' }), { target: { value: 'offline' } })
    expect(screen.getByText('No devices match your search and status filters.')).toBeTruthy()
    expect(screen.queryByText('No devices yet. Adopt one to get started.')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }))
    expect(screen.getByRole('button', { name: 'Open device Office gateway' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Open device Studio access point' })).toBeTruthy()
  })

  it('matches function and address facts without classifying from names', () => {
    render(<Devices devices={[gateway, accessPoint]} />)
    const search = screen.getByRole('searchbox', { name: 'Search devices' })
    fireEvent.change(search, { target: { value: '192.0.2.2' } })
    expect(screen.getByRole('status').textContent).toBe('Showing 1 of 2 devices')
    fireEvent.change(search, { target: { value: 'switch' } })
    expect(screen.getByRole('button', { name: 'Open device Office gateway' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Open device Studio access point' })).toBeNull()
  })

  it('persists list presentation and keeps the filtered table row count truthful', () => {
    const first = render(<Devices devices={[gateway, accessPoint]} />)
    fireEvent.click(screen.getByRole('button', { name: 'List' }))
    expect(localStorage.getItem('oonfeewrt:devices:view')).toBe('list')
    fireEvent.change(screen.getByRole('combobox', { name: 'Status' }), { target: { value: 'online' } })
    expect(screen.getByRole('table', { name: 'Managed devices' }).getAttribute('aria-rowcount')).toBe('2')
    expect(screen.getByText(/Customize columns/)).toBeTruthy()
    first.unmount()
    render(<Devices devices={[gateway, accessPoint]} />)
    expect(screen.getByRole('button', { name: 'List' }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByRole('table', { name: 'Managed devices' })).toBeTruthy()
  })

  it('defaults safely when the saved preference is invalid or storage is unavailable', () => {
    localStorage.setItem('oonfeewrt:devices:view', 'unexpected')
    const first = render(<Devices devices={[]} />)
    expect(screen.getByRole('button', { name: 'Cards' }).getAttribute('aria-pressed')).toBe('true')
    first.unmount()
    const read = vi.spyOn(localStorage, 'getItem').mockImplementation(() => { throw new Error('disabled') })
    const write = vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('disabled') })
    try {
      render(<Devices devices={[gateway]} />)
      fireEvent.click(screen.getByRole('button', { name: 'List' }))
      expect(screen.getByRole('table', { name: 'Managed devices' })).toBeTruthy()
    } finally { read.mockRestore(); write.mockRestore() }
  })

  it('does not turn unresolved summary counts into zero and explains retained data', () => {
    const view = render(<Devices devices={[]} devicesLoaded={false} />)
    const summary = screen.getByLabelText('Device inventory summary')
    expect(summary.querySelectorAll('strong')).toHaveLength(4)
    expect([...summary.querySelectorAll('strong')].every((node) => node.textContent === '—')).toBe(true)
    view.rerender(<Devices devices={[gateway]} devicesLoaded devicesError="Controller unreachable" />)
    expect(screen.getByText(/Showing the last successful inventory/)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Open device Office gateway' })).toBeTruthy()
  })
})

describe('Client inventory presentation', () => {
  it('distinguishes server-wide matching totals from measured rows on the current page', async () => {
    const page: ClientPage = {
      clients: [{ mac: '02:00:00:00:10:01', name: 'Kitchen sensor', ipv4: '192.0.2.10', first_seen: 1, last_seen: 2,
        online: true, blocked: false, connection: 'unknown', scope: 'local' }],
      total: 850, limit: 500, offset: 0, facets: { presence: [], connection: [], scope: [] }, note: 'No RF readings', scope_note: '',
    }
    apiMocks.clients.mockResolvedValue(page)
    render(<Clients />)
    await waitFor(() => expect(screen.getByText('Kitchen sensor')).toBeTruthy())
    const summary = screen.getByLabelText('Client inventory summary')
    expect(within(summary).getByText('Matching clients').parentElement?.textContent).toContain('850')
    expect(within(summary).getByText('On this page').parentElement?.textContent).toContain('1')
    expect(within(summary).getByText('Signal readings').parentElement?.textContent).toContain('0')
    expect(within(screen.getByRole('table', { name: 'Client devices' })).queryByText('wired')).toBeNull()
    expect(screen.getByRole('button', { name: 'Open observability for Kitchen sensor' })).toBeTruthy()
    expect(apiMocks.clients).toHaveBeenCalledWith({ limit: 500, offset: 0, presence: 'online', connection: '', scope: 'local' })
  })
})
