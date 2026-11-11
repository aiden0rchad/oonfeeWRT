import { useEffect, useRef } from 'react'
import type { KeyboardEvent as ReactKeyboardEvent } from 'react'
import type { ControllerUpdateStatus, SessionInfo } from '../lib/api'
import { Button } from './ui'

const blockingModalSelector = [
  'dialog[open]',
  '[role="dialog"][aria-modal="true"]',
  '[role="alertdialog"][aria-modal="true"]',
].join(',')

function hasBlockingModal(updateDialog: HTMLDialogElement) {
  return Array.from(document.querySelectorAll<HTMLElement>(blockingModalSelector)).some((candidate) =>
    candidate !== updateDialog && !candidate.hidden && candidate.getAttribute('aria-hidden') !== 'true',
  )
}

function focusableElements(dialog: HTMLDialogElement) {
  return Array.from(dialog.querySelectorAll<HTMLElement>(
    'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), ' +
    'textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
  )).filter((element) => !element.hidden && element.getAttribute('aria-hidden') !== 'true')
}

function dismissalKey(session: SessionInfo, status: ControllerUpdateStatus) {
  if (!status.latest_version) return ''
  const target = status.latest_version.toLowerCase()
  return `oonfeewrt:controller-update-dismissed:${encodeURIComponent(window.location.origin)}:${session.admin_id}:${target}`
}

export function ControllerUpdateNotice({
  status,
  session,
  onViewInstructions,
  onBeforeOpen,
  onOpenChange,
  memoryDismissals: suppliedMemoryDismissals,
  memorySnoozes: suppliedMemorySnoozes,
}: {
  status: ControllerUpdateStatus | null
  session: SessionInfo
  onViewInstructions: () => void
  onBeforeOpen?: () => void
  onOpenChange?: (open: boolean) => void
  memoryDismissals?: Set<string>
  memorySnoozes?: Map<string, number>
}) {
  const dialog = useRef<HTMLDialogElement>(null)
  const previousFocus = useRef<HTMLElement | null>(null)
  const localMemoryDismissals = useRef(new Set<string>())
  const memoryDismissals = suppliedMemoryDismissals ?? localMemoryDismissals.current
  const localMemorySnoozes = useRef(new Map<string, number>())
  const memorySnoozes = suppliedMemorySnoozes ?? localMemorySnoozes.current
  const available = session.role !== 'viewer' && status?.state === 'available' && status.update_available &&
    Boolean(status.latest_version && status.latest_tag && status.release_url)
  const key = status && available ? dismissalKey(session, status) : ''

  useEffect(() => {
    const element = dialog.current
    if (!element) return
    const dismissed = () => {
      if (!key || memoryDismissals.has(key)) return true
      try {
        return window.localStorage.getItem(key) === '1'
      } catch {
        // Storage-disabled browsers retain dismissals in this mounted app.
        return false
      }
    }
    if (dismissed()) {
      if (element.open) element.close()
      onOpenChange?.(false)
      return
    }

    const snoozeRemaining = () => {
      let until = memorySnoozes.get(key + ':snoozed-until') ?? 0
      try {
        const stored = Number(window.localStorage.getItem(key + ':snoozed-until'))
        if (Number.isFinite(stored)) until = Math.max(until, stored)
      } catch {
        // Storage-disabled browsers retain snoozes in this mounted app.
      }
      return Number.isFinite(until) ? until - Date.now() : 0
    }
    let stopped = false
    let prepared = !onBeforeOpen
    let preparationPending = false
    let timer = 0
    let snoozeTimer = 0
    const open = () => {
      if (stopped || element.open || dismissed()) return
      const remaining = snoozeRemaining()
      if (remaining > 0) {
        prepared = !onBeforeOpen
        window.clearTimeout(snoozeTimer)
        snoozeTimer = window.setTimeout(open, Math.min(remaining, 2_147_483_647))
        return
      }
      if (hasBlockingModal(element)) return
      if (!prepared) {
        prepared = true
        preparationPending = true
        onBeforeOpen?.()
        timer = window.setTimeout(() => {
          preparationPending = false
          open()
        }, 0)
        return
      }
      if (preparationPending) return
      const active = document.activeElement
      if (active instanceof HTMLElement && !element.contains(active)) previousFocus.current = active
      element.showModal()
      onOpenChange?.(true)
    }
    const observer = new MutationObserver(open)
    observer.observe(document.body, {
      subtree: true,
      childList: true,
      attributes: true,
      attributeFilter: ['open', 'role', 'aria-modal', 'aria-hidden', 'hidden'],
    })
    open()
    return () => {
      stopped = true
      window.clearTimeout(timer)
      window.clearTimeout(snoozeTimer)
      observer.disconnect()
      if (element.open) element.close()
      onOpenChange?.(false)
    }
  }, [key, memoryDismissals, memorySnoozes, onBeforeOpen, onOpenChange])

  if (!status || !available || !key) return null

  const dismiss = (action: 'snooze' | 'skip' | 'instructions' = 'snooze') => {
    const storageKey = action === 'snooze' ? key + ':snoozed-until' : key
    const until = Date.now() + 7 * 24 * 60 * 60_000
    if (action === 'snooze') memorySnoozes.set(storageKey, until)
    else memoryDismissals.add(key)
    try {
      window.localStorage.setItem(storageKey, action === 'snooze' ? String(until) : '1')
    } catch {
      // In-memory preferences still apply when storage is denied.
    }
    const restore = previousFocus.current
    previousFocus.current = null
    dialog.current?.close()
    onOpenChange?.(false)
    if (action === 'instructions') {
      onViewInstructions()
    } else {
      requestAnimationFrame(() => {
        if (restore?.isConnected && !restore.closest('[inert]')) restore.focus()
      })
    }
  }

  const trapFocus = (event: ReactKeyboardEvent<HTMLDialogElement>) => {
    if (event.key !== 'Tab' || !dialog.current) return
    const items = focusableElements(dialog.current)
    const first = items[0]
    const last = items.at(-1)
    if (!first || !last) {
      event.preventDefault()
      dialog.current.focus()
    } else if (event.shiftKey && (document.activeElement === first || !dialog.current.contains(document.activeElement))) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && (document.activeElement === last || !dialog.current.contains(document.activeElement))) {
      event.preventDefault()
      first.focus()
    }
  }

  return <dialog
    ref={dialog}
    className="controller-update-dialog"
    aria-labelledby="controller-update-dialog-title"
    aria-describedby="controller-update-dialog-description"
    onKeyDown={trapFocus}
    onCancel={(event) => { event.preventDefault(); dismiss() }}
    onClose={() => onOpenChange?.(false)}
  >
    <button className="controller-update-dialog-close" type="button" autoFocus aria-label="Close update notice" onClick={() => dismiss()}>×</button>
    <div className="controller-update-dialog-kicker">Controller update</div>
    <h2 id="controller-update-dialog-title">A newer oonfeeWRT version is available</h2>
    <p id="controller-update-dialog-description">
      <code>{status.current_version}</code> <span aria-hidden="true">→</span><span className="controller-update-sr-only">to</span> <code>{status.latest_tag}</code>
    </p>
    <p>Stay current with the latest fixes and features. Review the release notes before upgrading.</p>
    {status.install_kind === 'container' && <p>For Docker Compose, pull the newer image and recreate the controller container while preserving its data volume and passphrase.</p>}
    {status.install_kind === 'binary' && <p>For a standalone binary, verify the release checksum, stop the daemon, replace the executable, and restart with the unchanged data directory and passphrase source.</p>}
    <p><a href={status.release_url} target="_blank" rel="noreferrer">View release notes</a></p>
    <div className="controller-update-dialog-actions">
      <Button onClick={() => dismiss()}>Remind me later</Button>
      <Button onClick={() => dismiss('skip')}>Skip this version</Button>
      <Button kind="primary" onClick={() => dismiss('instructions')}>How to update</Button>
    </div>
  </dialog>
}
