import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiError, type SessionInfo } from '../lib/api'
import { Accounts } from './Accounts'

const api = vi.hoisted(() => ({
  accounts: vi.fn(),
  createAccount: vi.fn(),
  setAccountRole: vi.fn(),
  setAccountEnabled: vi.fn(),
  deleteAccount: vi.fn(),
  resetAccountPassword: vi.fn(),
  managedAccountSessions: vi.fn(),
  revokeManagedAccountSession: vi.fn(),
  revokeManagedAccountSessions: vi.fn(),
  reauthenticate: vi.fn(),
}))

vi.mock('../lib/api', async () => ({
  ...(await vi.importActual<typeof import('../lib/api')>('../lib/api')),
  api,
}))

const session: SessionInfo = {
  admin_id: 1,
  username: 'owner',
  role: 'owner',
  role_label: 'Owner',
  csrf: 'csrf-token',
  reauthenticated_until: null,
}

const owner = {
  id: 1,
  username: 'owner',
  role: 'owner' as const,
  role_label: 'Owner',
  enabled: true,
  created_at: 1_725_000_000,
  last_login_at: 1_725_000_900,
  active_session_count: 1,
}

const operator = {
  id: 2,
  username: 'router-operator',
  role: 'operator' as const,
  role_label: 'Operator',
  enabled: true,
  created_at: 1_725_000_000,
  last_login_at: null,
  active_session_count: 0,
}

const roles = [
  { value: 'owner' as const, label: 'Owner', description: 'Full controller access.' },
  { value: 'admin' as const, label: 'Administrator', description: 'Manage the network.' },
  { value: 'operator' as const, label: 'Operator', description: 'Operate the network.' },
  { value: 'viewer' as const, label: 'Read only', description: 'View controller state.' },
]

function renderAccounts() {
  return render(<Accounts
    session={session}
    onSessionChange={vi.fn()}
    onCurrentSessionRevoked={vi.fn()}
  />)
}

describe('Accounts', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    api.accounts.mockResolvedValue({ accounts: [owner, operator], roles })
    api.managedAccountSessions.mockResolvedValue({ sessions: [] })
  })

  it('runs a named role change and requires a username-specific delete confirmation', async () => {
    api.setAccountRole.mockResolvedValue({
      account: { ...operator, role: 'admin', role_label: 'Administrator' },
    })
    renderAccounts()

    await screen.findByText('router-operator')
    let row = screen.getByText('router-operator').closest('.account-list-row')
    fireEvent.click(within(row as HTMLElement).getByRole('button', { name: 'Role' }))
    fireEvent.change(screen.getByLabelText(/new role/i), { target: { value: 'admin' } })
    fireEvent.click(screen.getByRole('button', { name: /change.*router-operator.*admin/i }))

    await waitFor(() => expect(api.setAccountRole).toHaveBeenCalledWith(2, 'admin'))
    row = screen.getByText('router-operator').closest('.account-list-row')
    expect(row).not.toBeNull()
    fireEvent.click(within(row as HTMLElement).getByRole('button', { name: 'Delete' }))

    expect(screen.getByRole('button', { name: /^delete.*router-operator/i })).toBeTruthy()
    expect(api.deleteAccount).not.toHaveBeenCalled()
  })

  it('creates an account with the selected role and clears its password', async () => {
    api.createAccount.mockResolvedValue({
      account: { ...operator, id: 3, username: 'auditor', role: 'viewer', role_label: 'Read only' },
    })
    renderAccounts()

    await screen.findByText('router-operator')
    const username = screen.getByLabelText(/^username/i)
    const password = screen.getByLabelText(/^password/i) as HTMLInputElement
    const confirm = screen.getByLabelText(/repeat password/i) as HTMLInputElement
    fireEvent.change(username, { target: { value: 'auditor' } })
    fireEvent.change(password, { target: { value: 'auditor correct horse staple' } })
    fireEvent.change(confirm, { target: { value: 'auditor correct horse staple' } })
    fireEvent.change(screen.getByLabelText(/^role/i), { target: { value: 'viewer' } })
    fireEvent.click(screen.getByRole('button', { name: /create account/i }))

    await waitFor(() => expect(api.createAccount).toHaveBeenCalledWith(
      'auditor',
      'auditor correct horse staple',
      'viewer',
    ))
    expect(password.value).toBe('')
    expect(confirm.value).toBe('')
  })

  it('requires reauthentication and an explicit retry without retaining either password', async () => {
    api.resetAccountPassword
      .mockRejectedValueOnce(new ApiError(428, 'reauth_required', { error: 'reauth_required' }))
      .mockResolvedValueOnce({ ok: true, revoked_sessions: 0 })
    api.reauthenticate.mockResolvedValue({ reauthenticated_until: 1_725_004_600 })
    renderAccounts()

    await screen.findByText('router-operator')
    const row = screen.getByText('router-operator').closest('.account-list-row')
    fireEvent.click(within(row as HTMLElement).getByRole('button', { name: /reset password/i }))
    const next = screen.getByLabelText(/^new password/i) as HTMLInputElement
    const confirm = screen.getByLabelText(/repeat new password/i) as HTMLInputElement
    fireEvent.change(next, { target: { value: 'replacement correct horse staple' } })
    fireEvent.change(confirm, { target: { value: 'replacement correct horse staple' } })
    fireEvent.click(screen.getByRole('button', { name: /reset password for.*router-operator/i }))

    await waitFor(() => expect(api.resetAccountPassword).toHaveBeenCalledTimes(1))
    expect(next.value).toBe('')
    expect(confirm.value).toBe('')

    const ownerPassword = screen.getByLabelText(/current password/i) as HTMLInputElement
    fireEvent.change(ownerPassword, { target: { value: 'owner correct horse staple' } })
    fireEvent.click(screen.getByRole('button', { name: /confirm identity/i }))

    await waitFor(() => expect(api.reauthenticate).toHaveBeenCalledWith('owner correct horse staple'))
    await waitFor(() => expect(screen.queryByDisplayValue('owner correct horse staple')).toBeNull())
    expect(api.resetAccountPassword).toHaveBeenCalledTimes(1)

    expect((screen.getByRole('button', {
      name: /retry reset.*router-operator/i,
    }) as HTMLButtonElement).disabled).toBe(true)
    fireEvent.change(next, { target: { value: 'second replacement horse staple' } })
    fireEvent.change(confirm, { target: { value: 'second replacement horse staple' } })
    fireEvent.click(screen.getByRole('button', { name: /retry reset.*router-operator/i }))

    await waitFor(() => expect(api.resetAccountPassword).toHaveBeenNthCalledWith(
      2,
      2,
      'second replacement horse staple',
    ))
  })

  it('requires a named confirmation before revoking a managed session', async () => {
    api.managedAccountSessions.mockResolvedValue({ sessions: [{
      id: 'opaque-session-id', current: false, created_at: 1_725_000_000,
      last_seen_at: 1_725_000_900, expires_at: 1_725_004_600,
      peer_address: '192.168.1.22',
    }] })
    api.revokeManagedAccountSession.mockResolvedValue({ ok: true, revoked: 1, signed_out: false })
    renderAccounts()

    await screen.findByText('router-operator')
    const row = screen.getByText('router-operator').closest('.account-list-row')
    fireEvent.click(within(row as HTMLElement).getByRole('button', { name: 'Sessions' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Revoke 192.168.1.22' }))

    expect(api.revokeManagedAccountSession).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: /revoke 192\.168\.1\.22 for.*router-operator/i }))
    await waitFor(() => expect(api.revokeManagedAccountSession).toHaveBeenCalledWith(2, 'opaque-session-id'))
  })
})
