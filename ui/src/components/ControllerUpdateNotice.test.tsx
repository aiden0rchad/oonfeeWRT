import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { StrictMode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ControllerUpdateStatus, SessionInfo } from '../lib/api'
import { ControllerUpdateNotice } from './ControllerUpdateNotice'

const account: SessionInfo = {
  admin_id: 7, username: 'operator', role: 'owner', role_label: 'Owner', csrf: 'token', reauthenticated_until: null,
}

function available(tag = 'v1.2.4'): ControllerUpdateStatus {
  return {
    install_kind: 'unknown', state: 'available', current_version: 'v1.2.3', latest_version: tag.replace(/^v/i, ''),
    latest_tag: tag, release_url: `https://github.com/aiden0rchad/oonfeeWRT/releases/tag/${tag}`,
    update_available: true, checked_at: '2026-09-18T12:00:00Z', next_check_at: '2026-09-19T00:00:00Z',
    message: 'A newer release is available.',
  }
}

describe('ControllerUpdateNotice', () => {
  beforeEach(() => window.localStorage.clear())
  afterEach(() => {
    vi.restoreAllMocks()
    vi.useRealTimers()
  })

  it('never prompts a viewer, including after a role change', () => {
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    expect(screen.getByRole('dialog')).toBeTruthy()
    view.rerender(<ControllerUpdateNotice status={available()} session={{ ...account, role: 'viewer' }} onViewInstructions={vi.fn()} />)
    expect(view.container.querySelector('dialog')).toBeNull()
  })

  it.each(['container', 'binary', 'unknown'] as const)('shows install-specific dialog guidance for %s', (kind) => {
    render(<ControllerUpdateNotice status={{ ...available(), install_kind: kind }} session={account} onViewInstructions={vi.fn()} />)
    expect(Boolean(screen.queryByText(/For Docker Compose/))).toBe(kind === 'container')
    expect(Boolean(screen.queryByText(/For a standalone binary/))).toBe(kind === 'binary')
  })

  it.each(['Remind me later', 'Close update notice', 'Escape'])('%s snoozes for seven days without permanently dismissing', (action) => {
    vi.useFakeTimers()
    const start = Date.now()
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    if (action === 'Escape') {
      fireEvent(view.container.querySelector('dialog')!, new Event('cancel', { bubbles: true, cancelable: true }))
    } else {
      fireEvent.click(screen.getByRole('button', { name: action }))
    }
    const key = window.localStorage.key(0)!
    expect(key.endsWith(':snoozed-until')).toBe(true)
    expect(Number(window.localStorage.getItem(key))).toBe(start + 7 * 24 * 60 * 60_000)
    expect(window.localStorage.getItem(key.replace(':snoozed-until', ''))).toBeNull()
    view.unmount()
    const same = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    expect(same.container.querySelector('dialog')?.hasAttribute('open')).toBe(false)
    act(() => vi.advanceTimersByTime(7 * 24 * 60 * 60_000 - 1))
    expect(same.container.querySelector('dialog')?.hasAttribute('open')).toBe(false)
    act(() => vi.advanceTimersByTime(1))
    expect(same.container.querySelector('dialog')?.hasAttribute('open')).toBe(true)
  })

  it('prompts immediately for a newer target while the previous one is snoozed', () => {
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Remind me later' }))
    view.rerender(<ControllerUpdateNotice status={available('v1.2.5')} session={account} onViewInstructions={vi.fn()} />)
    expect(screen.getByRole('dialog').textContent).toContain('v1.2.5')
  })

  it.each(['not-a-number', 'Infinity', '0'])('ignores an invalid or expired snooze: %s', (value) => {
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Remind me later' }))
    const key = window.localStorage.key(0)!
    view.unmount()
    window.localStorage.setItem(key, value)
    render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    expect(screen.getByRole('dialog')).toBeTruthy()
  })

  it('opens a native modal with calm version, release-note, and recreate guidance', () => {
    const onOpenChange = vi.fn()
    render(<StrictMode><ControllerUpdateNotice
      status={available()}
      session={account}
      onViewInstructions={vi.fn()}
      onOpenChange={onOpenChange}
    /></StrictMode>)

    const dialog = screen.getByRole('dialog', { name: 'A newer oonfeeWRT version is available' })
    expect(dialog.hasAttribute('open')).toBe(true)
    expect(dialog.textContent).toMatch(/v1\.2\.3.*v1\.2\.4/s)
    expect(dialog.textContent).not.toMatch(/For Docker Compose/i)
    expect(dialog.textContent).toMatch(/Stay current with the latest fixes and features\. Review the release notes before upgrading\./i)
    expect(screen.getByRole('link', { name: 'View release notes' }).getAttribute('href')).toBe(
      'https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v1.2.4',
    )
    expect(onOpenChange).toHaveBeenCalledWith(true)
  })

  it('persists skipped versions per account and target, then prompts for a newer target', () => {
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Skip this version' }))
    expect(view.container.querySelector('dialog')?.hasAttribute('open')).toBe(false)

    view.unmount()
    const same = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    expect(same.container.querySelector('dialog')?.hasAttribute('open')).toBe(false)
    same.rerender(<ControllerUpdateNotice status={available('v1.2.5')} session={account} onViewInstructions={vi.fn()} />)
    expect(same.container.querySelector('dialog')?.hasAttribute('open')).toBe(true)

    const cancel = new Event('cancel', { bubbles: true, cancelable: true })
    fireEvent(same.container.querySelector('dialog')!, cancel)
    expect(cancel.defaultPrevented).toBe(true)
    expect(same.container.querySelector('dialog')?.hasAttribute('open')).toBe(false)
  })

  it('isolates dismissal by account and navigates only after recording the CTA dismissal', () => {
    const onView = vi.fn()
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={onView} />)
    fireEvent.click(screen.getByRole('button', { name: 'How to update' }))
    expect(onView).toHaveBeenCalledOnce()
    expect(view.container.querySelector('dialog')?.hasAttribute('open')).toBe(false)

    view.rerender(<ControllerUpdateNotice status={available()} session={{ ...account, admin_id: 8 }} onViewInstructions={onView} />)
    expect(view.container.querySelector('dialog')?.hasAttribute('open')).toBe(true)
  })

  it('keeps an in-memory snooze across remounts when storage is denied, then expires it', () => {
    vi.useFakeTimers()
    const memorySnoozes = new Map<string, number>()
    vi.spyOn(window.localStorage, 'getItem').mockImplementation(() => { throw new Error('denied') })
    vi.spyOn(window.localStorage, 'setItem').mockImplementation(() => { throw new Error('denied') })
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} memorySnoozes={memorySnoozes} />)
    fireEvent.click(screen.getByRole('button', { name: 'Remind me later' }))
    view.unmount()
    const same = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} memorySnoozes={memorySnoozes} />)
    expect(same.container.querySelector('dialog')?.hasAttribute('open')).toBe(false)
    act(() => vi.advanceTimersByTime(7 * 24 * 60 * 60_000))
    expect(same.container.querySelector('dialog')?.hasAttribute('open')).toBe(true)
  })

  it('keeps an in-memory permanent dismissal when storage is denied', () => {
    const setItem = vi.spyOn(window.localStorage, 'setItem').mockImplementation(() => { throw new Error('denied') })
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Skip this version' }))
    view.rerender(<ControllerUpdateNotice status={null} session={account} onViewInstructions={vi.fn()} />)
    view.rerender(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    expect(view.container.querySelector('dialog')?.hasAttribute('open')).toBe(false)
    setItem.mockRestore()
  })

  it('defers behind another modal and opens after that editor closes', async () => {
    const view = render(<>
      <div role="dialog" aria-modal="true" aria-label="Existing editor"><button>Close editor</button></div>
      <ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />
    </>)
    const update = view.container.querySelector('.controller-update-dialog')!
    expect(update.hasAttribute('open')).toBe(false)
    expect(screen.getByRole('dialog', { name: 'Existing editor' })).toBeTruthy()

    view.rerender(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    await waitFor(() => expect(view.container.querySelector('.controller-update-dialog')?.hasAttribute('open')).toBe(true))
  })

  it('wraps focus from the first control to the final CTA', () => {
    render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    const close = screen.getByRole('button', { name: 'Close update notice' })
    const instructions = screen.getByRole('button', { name: 'How to update' })
    close.focus()
    fireEvent.keyDown(close, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(instructions)
    fireEvent.keyDown(instructions, { key: 'Tab' })
    expect(document.activeElement).toBe(close)
  })

  it('does not open for current, unavailable, unknown, or incomplete statuses', () => {
    const base = available()
    const view = render(<ControllerUpdateNotice status={{ ...base, state: 'current', update_available: false }} session={account} onViewInstructions={vi.fn()} />)
    expect(view.container.querySelector('dialog')).toBeNull()
    for (const state of ['unknown', 'unavailable'] as const) {
      view.rerender(<ControllerUpdateNotice status={{ ...base, state, update_available: false }} session={account} onViewInstructions={vi.fn()} />)
      expect(view.container.querySelector('dialog')).toBeNull()
    }
    view.rerender(<ControllerUpdateNotice status={{ ...base, release_url: undefined }} session={account} onViewInstructions={vi.fn()} />)
    expect(view.container.querySelector('dialog')).toBeNull()
  })
})
