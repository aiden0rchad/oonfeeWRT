import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'

const api = {
  clients: vi.fn(), devices: vi.fn(), saveWLAN: vi.fn(), noteForeign: vi.fn(),
  lastNeighbours: vi.fn(), events: vi.fn(), site: vi.fn(), saveMesh: vi.fn(),
  deleteMesh: vi.fn(), preview: vi.fn(), applySite: vi.fn(), device: vi.fn(),
  deviceSeries: vi.fn(), overhead: vi.fn(), reprobe: vi.fn(),
  distributeNeighbours: vi.fn(), meshHealth: vi.fn(), saveUplink: vi.fn(),
  deleteUplink: vi.fn(), unadopt: vi.fn(), stats: vi.fn(),
}
vi.mock('../lib/api', () => ({
  api, ApiError: class extends Error {}, onUnauthorized: new Set<() => void>(),
}))
vi.mock('../lib/live', () => ({
  live: { watch: () => () => {}, on: () => () => {} },
}))

const site = {
  name: 'Site', uuid: 'abcdef01-2345-6789-abcd-ef0123456789',
  wlans: [], meshes: [], groups: [{ id: 1, name: 'all', device_ids: [] }],
  networks: [{ id: 1, name: 'lan', vlan: 1, cidr: '192.168.1.1/24', zone: 'lan', enabled: true }],
  problems: [], overrides: [], overridable: [], override_note: '',
}

api.devices.mockResolvedValue({ devices: [] })
api.lastNeighbours.mockResolvedValue({ ran: false })
api.meshHealth.mockResolvedValue({ links: [], note: '' })

const { Settings } = await import('./Settings')

describe('scratch', () => {
  beforeEach(() => { api.site.mockResolvedValue(site) })

  // Exactly what internal/daemon.previewDevice emits when Connect fails:
  // Changes is nil, json tag has no omitempty, so the wire carries null.
  it('an unreachable device in a preview', async () => {
    api.preview.mockResolvedValue({
      site_name: 'Site',
      devices: [
        { device_id: 1, name: 'ap-c6', role: 'ap', changes: null, blocked: false,
          error: 'could not reach this device: dial tcp 192.168.1.2:80: connect: no route to host' },
        { device_id: 2, name: 'ap-wrt', role: 'ap', blocked: false,
          changes: [{ config: 'wireless', section: 's', action: 'update', options: ['ssid'] }] },
      ],
      site_errors: [],
    })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalled())
    await waitFor(() => expect(screen.getByText(/ap-wrt/)).toBeTruthy())
  })

  // Two omissions with the same text under one device: React key={o}.
  it('duplicate omission strings', async () => {
    const warn = vi.spyOn(console, 'error').mockImplementation(() => {})
    api.preview.mockResolvedValue({
      site_name: 'Site',
      devices: [
        { device_id: 1, name: 'ap-wrt', role: 'ap', blocked: false,
          changes: [{ config: 'wireless', section: 's', action: 'update', options: ['ssid'] }],
          omitted: ['home: this device has no 6g radio', 'home: this device has no 6g radio'] },
      ],
      site_errors: [],
    })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalled())
    await waitFor(() => expect(screen.getAllByText(/no 6g radio/).length).toBeGreaterThan(0))
    const dupes = warn.mock.calls.filter((c) => String(c[0]).includes('same key'))
    console.log('DUPLICATE-KEY WARNINGS:', dupes.length, JSON.stringify(dupes[0] ?? null).slice(0, 300))
    console.log('RENDERED LI COUNT:', screen.getAllByText(/no 6g radio/).length)
    warn.mockRestore()
  })

  // Two ops on the SAME section: the option-to-list repair (render/plan.go
  // emits OpDelete of the option, then OpSet of the section).
  it('option-to-list repair renders two lines with one key', async () => {
    const warn = vi.spyOn(console, 'error').mockImplementation(() => {})
    api.preview.mockResolvedValue({
      site_name: 'Site',
      devices: [
        { device_id: 1, name: 'ap-wrt', role: 'ap', blocked: false, touches_traversal: true,
          changes: [
            { config: 'network', section: 'oowrt_br_vlan_iot', action: 'remove' },
            { config: 'network', section: 'oowrt_br_vlan_iot', action: 'update',
              options: ['device', 'vlan', 'ports'] },
          ] },
      ],
      site_errors: [],
    })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalled())
    await waitFor(() => expect(screen.getAllByText((_t, e) => e?.tagName === 'CODE').length).toBeGreaterThan(0))
    const dupes = warn.mock.calls.filter((c) => String(c[0]).includes('same key'))
    console.log('OPTION-REPAIR DUPLICATE-KEY WARNINGS:', dupes.length)
    console.log('CODE nodes:', screen.getAllByText((_t, e) => e?.tagName === 'CODE').map((n) => n.textContent))
    console.log('remove lines:', screen.queryAllByText('remove').length,
      'update lines:', screen.queryAllByText('update').length)
    console.log('pending label:', screen.getAllByText(/^Apply/).map((n) => n.textContent))
    warn.mockRestore()
  })
})
