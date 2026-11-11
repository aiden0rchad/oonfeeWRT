import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ControllerUpdateStatus, SessionInfo } from '../lib/api'
import { ControllerUpdates } from './ControllerUpdates'

const owner: SessionInfo = {
  admin_id: 1, username: 'owner', role: 'owner', role_label: 'Owner', csrf: 'token', reauthenticated_until: null,
}

const status: ControllerUpdateStatus = {
  install_kind: 'unknown', state: 'available', current_version: 'v0.1.8', latest_version: '0.1.9', latest_tag: 'v0.1.9',
  release_url: 'https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v0.1.9', update_available: true,
  checked_at: '2026-09-18T12:00:00Z', next_check_at: '2026-09-19T00:00:00Z',
  message: 'A newer published stable release is available.',
}

describe('ControllerUpdates', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    Reflect.deleteProperty(document, 'execCommand')
  })

  it.each(['container', 'binary', 'unknown'] as const)('shows only applicable instructions for %s', (kind) => {
    render(<ControllerUpdates status={{ ...status, install_kind: kind }} session={owner} />)
    expect(Boolean(screen.queryByRole('heading', { name: 'Docker Compose' }))).toBe(kind !== 'binary')
    expect(Boolean(screen.queryByRole('heading', { name: 'Standalone binary' }))).toBe(kind !== 'container')
    expect(Boolean(screen.queryByRole('button', { name: 'Copy Docker Compose commands' }))).toBe(kind !== 'binary')
    const disclosure = screen.getByText("What this check does and doesn't do").closest('details')!
    expect(disclosure.open).toBe(false)
  })

  it.each(['owner', 'admin'] as const)('lets %s check now and announces an unchanged result', (role) => {
    const refresh = vi.fn()
    render(<ControllerUpdates status={status} session={{ ...owner, role }} onRefresh={refresh} checkNote />)
    fireEvent.click(screen.getByRole('button', { name: 'Check now' }))
    expect(refresh).toHaveBeenCalledOnce()
    expect(screen.getByText('Checked a moment ago — try again in a minute.')).toBeTruthy()
  })

  it('copies the exact command block with the clipboard API and announces success', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    render(<ControllerUpdates status={status} session={owner} />)
    fireEvent.click(screen.getByRole('button', { name: 'Copy Docker Compose commands' }))
    await waitFor(() => expect(screen.getByText('Copied')).toBeTruthy())
    expect(writeText).toHaveBeenCalledWith('docker compose pull oonfeewrt\ndocker compose up -d oonfeewrt')
    expect(screen.getByText('Copied').getAttribute('aria-live')).toBe('polite')
  })

  it.each([undefined, { writeText: vi.fn().mockRejectedValue(new Error('denied')) }])('falls back to selected text when clipboard is unavailable or denied: %s', async (clipboard) => {
    vi.stubGlobal('navigator', { clipboard })
    const execCommand = vi.fn().mockReturnValue(true)
    Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand })
    render(<ControllerUpdates status={status} session={owner} />)
    fireEvent.click(screen.getByRole('button', { name: 'Copy release environment line' }))
    await waitFor(() => expect(screen.getByText('Copied')).toBeTruthy())
    expect(execCommand).toHaveBeenCalledWith('copy')
    expect(window.getSelection()?.toString()).toBe('OONFEE_VERSION=v0.1.9')
  })

  it.each([false, 'throws'])('leaves commands selected for keyboard copying if both methods fail: %s', async (result) => {
    vi.stubGlobal('navigator', {})
    Object.defineProperty(document, 'execCommand', { configurable: true, value: () => {
      if (result === 'throws') throw new Error('denied')
      return result
    } })
    render(<ControllerUpdates status={status} session={owner} />)
    fireEvent.click(screen.getByRole('button', { name: 'Copy Docker Compose commands' }))
    expect(await screen.findByText('Press Ctrl/Cmd+C to copy')).toBeTruthy()
    expect(window.getSelection()?.toString()).toBe('docker compose pull oonfeewrt\ndocker compose up -d oonfeewrt')
  })
  it('shows validated Compose and binary guidance without implying automatic install or a vulnerability', () => {
    render(<ControllerUpdates status={status} session={owner} />)
    expect(screen.getByText('Update available — v0.1.9')).toBeTruthy()
    expect(screen.getByText('v0.1.8')).toBeTruthy()
    expect(screen.getAllByText('v0.1.9').length).toBeGreaterThan(0)
    expect(screen.getByText('OONFEE_VERSION=v0.1.9')).toBeTruthy()
    expect(screen.getByText(/docker compose pull oonfeewrt/).textContent).toContain('docker compose up -d oonfeewrt')
    expect(screen.getByText(/matching database, keyring, and runtime passphrase/)).toBeTruthy()
    expect(screen.getByText(/This does not assess/)).toBeTruthy()
    expect(screen.getByText(/public request IP/)).toBeTruthy()
    expect(screen.getByText(/separate from OpenWrt router firmware/)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Read the complete upgrade and rollback guide' }).getAttribute('href')).toBe(
      'https://aiden0rchad.github.io/oonfeeWRT/installation/upgrades',
    )
  })

  it('shows no fake target commands for current, unknown, unavailable, or browser-error states', () => {
    const view = render(<ControllerUpdates status={{ ...status, state: 'current', update_available: false }} session={owner} />)
    expect(screen.getByText("You're up to date — v0.1.8")).toBeTruthy()
    expect(screen.queryByText(/docker compose pull/)).toBeNull()

    view.rerender(<ControllerUpdates status={{ ...status, state: 'unknown', latest_tag: undefined, latest_version: undefined, release_url: undefined, update_available: false }} session={owner} />)
    expect(screen.getByText("Can't tell which version this is")).toBeTruthy()
    expect(screen.queryByText(/OONFEE_VERSION=/)).toBeNull()

    view.rerender(<ControllerUpdates status={{ ...status, state: 'unavailable', latest_tag: undefined, latest_version: undefined, release_url: undefined, update_available: false, error: { code: 'offline', message: 'Release service unavailable.' } }} session={owner} />)
    expect(screen.getByText("Couldn't check for updates")).toBeTruthy()
    expect(screen.getByText('Release service unavailable.')).toBeTruthy()

    view.rerender(<ControllerUpdates status={null} error="controller offline" session={owner} />)
    expect(screen.getByRole('alert').textContent).toMatch(/not being reported as current/i)

    view.rerender(<ControllerUpdates status={status} error="controller offline" session={owner} />)
    expect(screen.queryByText(/docker compose pull/)).toBeNull()
  })

  it('hides manual checking from viewers and directs them to the deployment owner', () => {
    const refresh = vi.fn()
    render(<ControllerUpdates status={status} session={{ ...owner, role: 'viewer', role_label: 'Read-only' }} onRefresh={refresh} />)
    expect(screen.queryByRole('button', { name: 'Check now' })).toBeNull()
    expect(refresh).not.toHaveBeenCalled()
    expect(screen.getByText('Checks run automatically about twice a day.')).toBeTruthy()
    expect(screen.getByText(/Ask the deployment owner/)).toBeTruthy()
  })
})
