import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { ControllerUpdateStatus, SessionInfo } from '../lib/api'
import { ControllerUpdates } from './ControllerUpdates'

const owner: SessionInfo = {
  admin_id: 1, username: 'owner', role: 'owner', role_label: 'Owner', csrf: 'token', reauthenticated_until: null,
}

const status: ControllerUpdateStatus = {
  state: 'available', current_version: 'v0.1.8', latest_version: '0.1.9', latest_tag: 'v0.1.9',
  release_url: 'https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v0.1.9', update_available: true,
  checked_at: '2026-09-18T12:00:00Z', next_check_at: '2026-09-19T00:00:00Z',
  message: 'A newer published stable release is available.',
}

describe('ControllerUpdates', () => {
  it('shows validated Compose and binary guidance without implying automatic install or a vulnerability', () => {
    render(<ControllerUpdates status={status} session={owner} />)
    expect(screen.getByText('Update available')).toBeTruthy()
    expect(screen.getByText('v0.1.8')).toBeTruthy()
    expect(screen.getAllByText('v0.1.9').length).toBeGreaterThan(0)
    expect(screen.getByText('OONFEE_VERSION=v0.1.9')).toBeTruthy()
    expect(screen.getByText(/docker compose pull oonfeewrt/).textContent).toContain('docker compose up -d oonfeewrt')
    expect(screen.getByText(/matching database, keyring, and runtime passphrase/)).toBeTruthy()
    expect(screen.getByText(/does not establish that this controller is vulnerable or secure/)).toBeTruthy()
    expect(screen.getByText(/public request IP/)).toBeTruthy()
    expect(screen.getByText(/separate from OpenWrt router firmware/)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Read the complete upgrade and rollback guide' }).getAttribute('href')).toBe(
      'https://aiden0rchad.github.io/oonfeeWRT/installation/upgrades',
    )
  })

  it('shows no fake target commands for current, unknown, unavailable, or browser-error states', () => {
    const view = render(<ControllerUpdates status={{ ...status, state: 'current', update_available: false }} session={owner} />)
    expect(screen.getByText('No newer stable release found')).toBeTruthy()
    expect(screen.queryByText(/docker compose pull/)).toBeNull()

    view.rerender(<ControllerUpdates status={{ ...status, state: 'unknown', latest_tag: undefined, latest_version: undefined, release_url: undefined, update_available: false }} session={owner} />)
    expect(screen.getByText('Running version unknown')).toBeTruthy()
    expect(screen.queryByText(/OONFEE_VERSION=/)).toBeNull()

    view.rerender(<ControllerUpdates status={{ ...status, state: 'unavailable', latest_tag: undefined, latest_version: undefined, release_url: undefined, update_available: false, error: { code: 'offline', message: 'Release service unavailable.' } }} session={owner} />)
    expect(screen.getByText('Check unavailable')).toBeTruthy()
    expect(screen.getByText('Release service unavailable.')).toBeTruthy()

    view.rerender(<ControllerUpdates status={null} error="controller offline" session={owner} />)
    expect(screen.getByRole('alert').textContent).toMatch(/not being reported as current/i)
  })

  it('explains cached refresh behavior and directs viewers to the deployment owner', () => {
    const refresh = vi.fn()
    render(<ControllerUpdates status={status} session={{ ...owner, role: 'viewer', role_label: 'Read-only' }} onRefresh={refresh} />)
    fireEvent.click(screen.getByRole('button', { name: 'Refresh status' }))
    expect(refresh).toHaveBeenCalledOnce()
    expect(screen.getByText(/does not force a new GitHub request/)).toBeTruthy()
    expect(screen.getByText(/Ask the deployment owner/)).toBeTruthy()
  })
})
