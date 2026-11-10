import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { StrictMode } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { ControllerUpdateStatus, SessionInfo } from '../lib/api'
import { ControllerUpdateNotice } from './ControllerUpdateNotice'

const account: SessionInfo = {
  admin_id: 7, username: 'operator', role: 'owner', role_label: 'Owner', csrf: 'token', reauthenticated_until: null,
}

function available(tag = 'v1.2.4'): ControllerUpdateStatus {
  return {
    state: 'available', current_version: 'v1.2.3', latest_version: tag.replace(/^v/i, ''),
    latest_tag: tag, release_url: `https://github.com/aiden0rchad/oonfeeWRT/releases/tag/${tag}`,
    update_available: true, checked_at: '2026-09-18T12:00:00Z', next_check_at: '2026-09-19T00:00:00Z',
    message: 'A newer release is available.',
  }
}

describe('ControllerUpdateNotice', () => {
  beforeEach(() => window.localStorage.clear())

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
    expect(dialog.textContent).toMatch(/pull the newer image and recreate the controller container/i)
    expect(dialog.textContent).toMatch(/Stay current with the latest fixes and features\. Review the release notes before upgrading\./i)
    expect(screen.getByRole('link', { name: 'View release notes' }).getAttribute('href')).toBe(
      'https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v1.2.4',
    )
    expect(onOpenChange).toHaveBeenCalledWith(true)
  })

  it('persists Dismiss and Escape per account and target, then prompts for a newer target', () => {
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }))
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
    fireEvent.click(screen.getByRole('button', { name: 'View update instructions' }))
    expect(onView).toHaveBeenCalledOnce()
    expect(view.container.querySelector('dialog')?.hasAttribute('open')).toBe(false)

    view.rerender(<ControllerUpdateNotice status={available()} session={{ ...account, admin_id: 8 }} onViewInstructions={onView} />)
    expect(view.container.querySelector('dialog')?.hasAttribute('open')).toBe(true)
  })

  it('keeps an in-memory dismissal when storage is denied', () => {
    const setItem = vi.spyOn(window.localStorage, 'setItem').mockImplementation(() => { throw new Error('denied') })
    const view = render(<ControllerUpdateNotice status={available()} session={account} onViewInstructions={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Close update notice' }))
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
    const instructions = screen.getByRole('button', { name: 'View update instructions' })
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
