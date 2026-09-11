import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { SessionInfo } from '../lib/api'
import { Settings } from './Settings'

const mocks = vi.hoisted(() => ({
  site: vi.fn(),
  diagnostics: vi.fn(),
  backups: vi.fn(),
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
      'Network', 'Diagnostics', 'Backup & Restore',
    ])
    expect(screen.getByRole('tab', { name: 'Diagnostics' })).toBeTruthy()
    expect(screen.getByRole('tab', { name: 'Backup & Restore' })).toBeTruthy()

    fireEvent.keyDown(network, { key: 'ArrowRight' })
    await waitFor(() => expect(mocks.diagnostics).toHaveBeenCalledOnce())
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe('settings-tab-diagnostics')
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
    expect(screen.getAllByRole('tab').map((tab) => tab.textContent)).toEqual(['Network'])
    expect(screen.queryByRole('tab', { name: 'Diagnostics' })).toBeNull()
    expect(screen.queryByRole('tab', { name: 'Backup & Restore' })).toBeNull()
  })

  it('returns to Network when an active privileged section is no longer available', async () => {
    const view = render(<Settings devices={[]} devicesLoaded={false} session={owner} />)
    fireEvent.click(screen.getByRole('tab', { name: 'Diagnostics' }))
    await waitFor(() => expect(mocks.diagnostics).toHaveBeenCalledOnce())

    view.rerender(<Settings devices={[]} devicesLoaded={false} session={{ ...owner, role: 'viewer' }} />)

    expect(screen.queryByRole('tab', { name: 'Backup & Restore' })).toBeNull()
    expect(screen.queryByRole('tab', { name: 'Diagnostics' })).toBeNull()
    expect(screen.getByRole('tab', { name: 'Network' }).getAttribute('aria-selected')).toBe('true')
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe('settings-tab-network')
  })
})
