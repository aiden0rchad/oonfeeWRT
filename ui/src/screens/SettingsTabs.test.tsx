import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { SessionInfo } from '../lib/api'
import { Settings } from './Settings'

const mocks = vi.hoisted(() => ({
  site: vi.fn(),
  diagnostics: vi.fn(),
  backups: vi.fn(),
  firmware: vi.fn(),
  checkFirmware: vi.fn(),
  adguard: vi.fn(),
  checkAdguard: vi.fn(),
  checkWireGuard: vi.fn(),
  saveAdguard: vi.fn(),
}))

vi.mock('../lib/api', async (importOriginal) => {
  const original = await importOriginal<typeof import('../lib/api')>()
  return { ...original, api: { ...original.api, ...mocks } }
})

const owner: SessionInfo = {
  admin_id: 1,
  username: 'owner',
  role: 'owner',
  role_label: 'Owner',
  csrf: 'token',
  reauthenticated_until: null,
}

describe('Settings sections', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.site.mockResolvedValue({
      name: 'Default', uuid: 'site-uuid', wlans: [], meshes: [], uplinks: [], groups: [],
      networks: [], zones: [], problems: [], overrides: [], overridable: [], override_note: '',
    })
    mocks.firmware.mockResolvedValue({
      devices: [{
        device_id: 7, name: 'Gateway', status: 'online', management_mode: 'managed',
        identity: { board_name: 'vendor,board', release: 'OpenWrt 24.10.1' },
      }],
      checking: { note: 'Checks run only when requested.' },
      installation: { enabled: false, required_checks: ['Validate the image on the device.'] },
      agent: { note: 'No helper is installed automatically.', package_path: 'deploy/openwrt-agent' },
    })
    mocks.adguard.mockResolvedValue({
      configured: true, url: 'https://dns.example', username: 'service', has_password: true,
      tls_fingerprint: '',
    })
    mocks.diagnostics.mockResolvedValue({
      mode: 'stored', router_management_calls: false, router_changes: false,
      sections: [{ id: 'controller', label: 'Controller', description: 'Stored controller state.' }],
      excluded_secret_classes: ['passwords'],
      limits: {
        devices: 100, sources: 100, events: 1_000,
        controller_log_input_bytes: 1_000_000, controller_log_output_bytes: 250_000,
        archive_bytes: 10_000_000, history: 10, retention_seconds: 86_400,
        collection_timeout_seconds: 30,
      },
      controller_log: { available: true, gaps: [] }, jobs: [],
    })
    mocks.backups.mockResolvedValue({
      descriptor: {
        plan_id: 'controller-backup-export-v1', format: 'oonfeewrt-portable-backup',
        format_version: 1, file_extension: '.oowrtbak', snapshot: 'Online snapshot.',
        encryption: 'Encrypted artifact.', includes: ['one', 'two', 'three'],
        excludes: ['one', 'two', 'three'],
      },
      disclosure: {
        router_management_calls: false, router_changes: false, automatic_router_apply: false,
        separate_export_passphrase: true, export_passphrase_recoverable: false, summary: 'Sensitive.',
      },
      limits: {
        history: 5, retention_seconds: 900, export_timeout_seconds: 1800,
        min_export_passphrase_characters: 16, max_export_passphrase_bytes: 4096,
      },
      jobs: [],
    })
  })

  it('keeps Network first and separates account management from controller settings', async () => {
    render(<Settings devices={[]} devicesLoaded={false} session={owner} />)

    const network = screen.getByRole('tab', { name: 'Network' })
    expect(network.getAttribute('aria-selected')).toBe('true')
    expect(screen.getAllByRole('tab').map((tab) => tab.textContent)).toEqual([
      'Network', 'Firmware', 'Integrations', 'Diagnostics', 'Backup & Restore',
    ])
    expect(screen.getByRole('tab', { name: 'Diagnostics' })).toBeTruthy()
    expect(screen.getByRole('tab', { name: 'Backup & Restore' })).toBeTruthy()

    fireEvent.keyDown(network, { key: 'ArrowRight' })
    await waitFor(() => expect(mocks.firmware).toHaveBeenCalledOnce())
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe('settings-tab-firmware')
  })

  it('shows Diagnostics to administrators while hiding owner-only backups', async () => {
    render(<Settings
      devices={[]}
      devicesLoaded={false}
      session={{ ...owner, role: 'admin', role_label: 'Administrator' }}
    />)

    await screen.findByRole('status')
    expect(screen.queryByRole('tab', { name: 'My account' })).toBeNull()
    expect(screen.queryByRole('tab', { name: 'Manage accounts' })).toBeNull()
    expect(screen.getByRole('tab', { name: 'Diagnostics' })).toBeTruthy()
    expect(screen.queryByRole('tab', { name: 'Backup & Restore' })).toBeNull()
  })

  it('hides Diagnostics from operators without treating visibility as authorization', async () => {
    render(<Settings
      devices={[]}
      devicesLoaded={false}
      session={{ ...owner, role: 'operator', role_label: 'Operator' }}
    />)

    await screen.findByRole('status')
    expect(screen.getAllByRole('tab').map((tab) => tab.textContent)).toEqual(['Network', 'Firmware', 'Integrations'])
    expect(screen.queryByRole('tab', { name: 'Diagnostics' })).toBeNull()
    expect(screen.queryByRole('tab', { name: 'Backup & Restore' })).toBeNull()
  })

  it('returns to Network when an active privileged section is no longer available', async () => {
    const onTabChange = vi.fn()
    const view = render(<Settings devices={[]} devicesLoaded={false} session={owner} onTabChange={onTabChange} />)
    fireEvent.click(screen.getByRole('tab', { name: 'Diagnostics' }))
    await waitFor(() => expect(mocks.diagnostics).toHaveBeenCalledOnce())

    view.rerender(<Settings devices={[]} devicesLoaded={false} session={{ ...owner, role: 'viewer' }} onTabChange={onTabChange} />)

    expect(screen.queryByRole('tab', { name: 'Backup & Restore' })).toBeNull()
    expect(screen.queryByRole('tab', { name: 'Diagnostics' })).toBeNull()
    expect(screen.getByRole('tab', { name: 'Network' }).getAttribute('aria-selected')).toBe('true')
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe('settings-tab-network')
    expect(onTabChange).toHaveBeenLastCalledWith('network', 'replace')
  })

  it('opens embedded Firmware directly without loading network settings or checking the catalogue', async () => {
    render(<Settings devices={[]} devicesLoaded={false} session={owner} initialTab="firmware" />)

    await screen.findByRole('heading', { name: 'Gateway' })
    expect(screen.getAllByRole('heading', { level: 1 }).map((heading) => heading.textContent)).toEqual(['Settings'])
    expect(screen.getByRole('tab', { name: 'Firmware' }).getAttribute('aria-selected')).toBe('true')
    expect(screen.getByRole('button', { name: 'Refresh inventory' })).toBeTruthy()
    expect(mocks.site).not.toHaveBeenCalled()
    expect(mocks.checkFirmware).not.toHaveBeenCalled()
    expect(mocks.adguard).not.toHaveBeenCalled()
  })

  it('follows routed section changes and loads only saved integration configuration', async () => {
    const onTabChange = vi.fn()
    const view = render(<Settings devices={[]} session={owner} initialTab="firmware" onTabChange={onTabChange} />)
    await screen.findByRole('heading', { name: 'Gateway' })

    view.rerender(<Settings devices={[]} session={owner} initialTab="integrations" onTabChange={onTabChange} />)

    await screen.findByText('https://dns.example')
    expect(screen.getAllByRole('heading', { level: 1 }).map((heading) => heading.textContent)).toEqual(['Settings'])
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe('settings-tab-integrations')
    expect(mocks.site).not.toHaveBeenCalled()
    expect(mocks.checkAdguard).not.toHaveBeenCalled()
    expect(mocks.checkWireGuard).not.toHaveBeenCalled()
    expect(mocks.saveAdguard).not.toHaveBeenCalled()
    expect(onTabChange).not.toHaveBeenCalled()
  })

  it('retains viewer restrictions inside Firmware and Integrations', async () => {
    const viewer: SessionInfo = { ...owner, role: 'viewer', role_label: 'Read-only' }
    const view = render(<Settings devices={[]} session={viewer} initialTab="firmware" />)
    await screen.findByRole('heading', { name: 'Gateway' })
    expect(screen.queryByRole('button', { name: 'Check OpenWrt catalogue' })).toBeNull()

    view.rerender(<Settings devices={[]} session={viewer} initialTab="integrations" />)
    await screen.findByText('https://dns.example')
    for (const name of ['Edit connection', 'Check AdGuard', 'Check WireGuard']) {
      expect(screen.queryByRole('button', { name })).toBeNull()
    }
  })

  it('announces selected sections through the routing callback and supports wrapping keyboard focus', async () => {
    const onTabChange = vi.fn()
    render(<Settings devices={[]} devicesLoaded={false} session={{ ...owner, role: 'admin' }} onTabChange={onTabChange} />)
    fireEvent.click(screen.getByRole('tab', { name: 'Integrations' }))
    await screen.findByText('https://dns.example')
    expect(onTabChange).toHaveBeenLastCalledWith('integrations')

    fireEvent.keyDown(screen.getByRole('tab', { name: 'Integrations' }), { key: 'Home' })
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('tab', { name: 'Network' })))
    expect(onTabChange).toHaveBeenLastCalledWith('network')

    fireEvent.keyDown(screen.getByRole('tab', { name: 'Network' }), { key: 'ArrowLeft' })
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('tab', { name: 'Diagnostics' })))
    expect(onTabChange).toHaveBeenLastCalledWith('diagnostics')

    fireEvent.keyDown(screen.getByRole('tab', { name: 'Diagnostics' }), { key: 'ArrowRight' })
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('tab', { name: 'Network' })))
    expect(onTabChange).toHaveBeenLastCalledWith('network')
  })

  it('never mounts an unauthorized privileged deep link', async () => {
    const onTabChange = vi.fn()
    render(<Settings devices={[]} devicesLoaded={false} session={{ ...owner, role: 'viewer' }} initialTab="backups" onTabChange={onTabChange} />)

    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe('settings-tab-network')
    await waitFor(() => expect(onTabChange).toHaveBeenCalledWith('network', 'replace'))
    expect(mocks.backups).not.toHaveBeenCalled()
    expect(mocks.diagnostics).not.toHaveBeenCalled()
  })
})
