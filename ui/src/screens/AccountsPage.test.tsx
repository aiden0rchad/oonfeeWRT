import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { ComponentProps } from 'react'
import type { SessionInfo } from '../lib/api'
import { AccountsPage } from './AccountsPage'

const children = vi.hoisted(() => ({ account: vi.fn(), accounts: vi.fn() }))

vi.mock('./Account', () => ({ Account: (props: unknown) => {
  children.account(props)
  return <div>Personal account controls</div>
} }))
vi.mock('./Accounts', () => ({ Accounts: (props: unknown) => {
  children.accounts(props)
  return <div>Owner account controls</div>
} }))

const owner: SessionInfo = {
  admin_id: 1, username: 'owner', role: 'owner', role_label: 'Owner',
  csrf: 'token', reauthenticated_until: null,
}

function propsFor(session = owner): ComponentProps<typeof AccountsPage> {
  return { session, onSessionChange: vi.fn(), onCurrentSessionRevoked: vi.fn() }
}

describe('Accounts workspace', () => {
  beforeEach(() => vi.clearAllMocks())

  it.each(['owner', 'admin', 'operator', 'viewer'] as const)(
    'opens My account for %s without mounting owner management by default', (role) => {
      const props = propsFor({ ...owner, role })
      render(<AccountsPage {...props} />)

      expect(screen.getByRole('heading', { level: 1, name: 'Accounts' })).toBeTruthy()
      const account = screen.getByRole('tab', { name: 'My account' })
      expect(account.getAttribute('aria-selected')).toBe('true')
      expect(account.tabIndex).toBe(0)
      expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe(account.id)
      expect(children.account).toHaveBeenLastCalledWith({
        session: props.session, onCurrentSessionRevoked: props.onCurrentSessionRevoked,
      })
      expect(children.accounts).not.toHaveBeenCalled()
      expect(screen.queryByRole('tab', { name: 'Manage accounts' }) !== null).toBe(role === 'owner')
    },
  )

  it('preserves owner management callbacks and keyboard tab navigation', async () => {
    const props = propsFor()
    render(<AccountsPage {...props} />)
    const account = screen.getByRole('tab', { name: 'My account' })
    const manage = screen.getByRole('tab', { name: 'Manage accounts' })

    fireEvent.keyDown(account, { key: 'ArrowRight' })
    await waitFor(() => expect(document.activeElement).toBe(manage))
    expect(manage.getAttribute('aria-selected')).toBe('true')
    expect(account.tabIndex).toBe(-1)
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe(manage.id)
    expect(children.accounts).toHaveBeenLastCalledWith(props)
    expect(screen.queryByText('Personal account controls')).toBeNull()

    fireEvent.keyDown(manage, { key: 'Home' })
    await waitFor(() => expect(document.activeElement).toBe(account))
    fireEvent.keyDown(account, { key: 'End' })
    await waitFor(() => expect(document.activeElement).toBe(manage))
    fireEvent.keyDown(manage, { key: 'ArrowRight' })
    await waitFor(() => expect(document.activeElement).toBe(account))
    fireEvent.keyDown(account, { key: 'ArrowLeft' })
    await waitFor(() => expect(document.activeElement).toBe(manage))
    fireEvent.keyDown(manage, { key: 'ArrowLeft' })
    await waitFor(() => expect(document.activeElement).toBe(account))
  })

  it('immediately removes management content and returns to My account after a role downgrade', () => {
    const props = propsFor()
    const view = render(<AccountsPage {...props} />)
    fireEvent.click(screen.getByRole('tab', { name: 'Manage accounts' }))
    expect(screen.getByText('Owner account controls')).toBeTruthy()

    const viewer = { ...owner, role: 'viewer' as const, role_label: 'Read only' }
    view.rerender(<AccountsPage {...props} session={viewer} />)

    expect(screen.queryByRole('tab', { name: 'Manage accounts' })).toBeNull()
    expect(screen.queryByText('Owner account controls')).toBeNull()
    const account = screen.getByRole('tab', { name: 'My account' })
    expect(account.getAttribute('aria-selected')).toBe('true')
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe(account.id)
    expect(children.account).toHaveBeenLastCalledWith({
      session: viewer, onCurrentSessionRevoked: props.onCurrentSessionRevoked,
    })
  })
})
