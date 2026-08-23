import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { SessionInfo } from '../lib/api'
import { Account } from './Account'

const api = vi.hoisted(() => ({
  account: vi.fn(),
  accountSessions: vi.fn(),
  changePassword: vi.fn(),
  revokeAccountSession: vi.fn(),
  session: vi.fn(),
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

const ownAccount = {
  id: 1,
  username: 'owner',
  role: 'owner' as const,
  role_label: 'Owner',
  enabled: true,
  created_at: 1_725_000_000,
  last_login_at: 1_725_000_900,
  active_session_count: 2,
}

const sessions = [
  {
    id: 'current-session',
    current: true,
    created_at: 1_725_000_000,
    last_seen_at: 1_725_001_000,
    expires_at: 1_725_004_600,
    peer_address: '192.168.1.10',
  },
  {
    id: 'other-session',
    current: false,
    created_at: 1_724_000_000,
    last_seen_at: 1_724_001_000,
    expires_at: 1_726_000_000,
    peer_address: '192.168.1.11',
  },
]

function renderAccount(overrides: Partial<React.ComponentProps<typeof Account>> = {}) {
  const props: React.ComponentProps<typeof Account> = {
    session,
    onCurrentSessionRevoked: vi.fn(),
    ...overrides,
  }
  return { ...render(<Account {...props} />), props }
}

describe('My Account', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    api.account.mockResolvedValue({ account: ownAccount })
    api.accountSessions.mockResolvedValue({ sessions })
    api.session.mockResolvedValue(session)
  })

  it('loads the signed-in account and identifies the current session', async () => {
    renderAccount()

    expect(await screen.findByRole('button', { name: /revoke current session/i })).toBeTruthy()
    expect(document.body.textContent).toContain('192.168.1.10')
    expect(screen.getByText('Current session')).toBeTruthy()
    expect(screen.getByText('Current')).toBeTruthy()
    expect(document.body.textContent).toContain('192.168.1.11')
    expect(api.account).toHaveBeenCalledOnce()
    expect(api.accountSessions).toHaveBeenCalledOnce()
  })

  it('changes the password and clears every secret field', async () => {
    api.changePassword.mockResolvedValue({ ok: true, message: 'Password changed.' })
    renderAccount()

    await screen.findByRole('button', { name: /revoke current session/i })
    const current = screen.getByLabelText(/current password/i) as HTMLInputElement
    const next = screen.getByLabelText(/^new password/i) as HTMLInputElement
    const confirm = screen.getByLabelText(/repeat new password/i) as HTMLInputElement
    fireEvent.change(current, { target: { value: 'correct horse battery staple' } })
    fireEvent.change(next, { target: { value: 'new correct horse battery staple' } })
    fireEvent.change(confirm, { target: { value: 'new correct horse battery staple' } })
    fireEvent.click(screen.getByRole('button', { name: /change password/i }))

    await waitFor(() => expect(api.changePassword).toHaveBeenCalledWith(
      'correct horse battery staple',
      'new correct horse battery staple',
    ))
    await waitFor(() => {
      expect(current.value).toBe('')
      expect(next.value).toBe('')
      expect(confirm.value).toBe('')
    })
  })

  it('signs out after confirmed revocation of the current session', async () => {
    api.revokeAccountSession.mockResolvedValue({
      ok: true,
      signed_out: true,
      revoked: 1,
    })
    const onCurrentSessionRevoked = vi.fn()
    renderAccount({ onCurrentSessionRevoked })

    await screen.findByRole('button', { name: /revoke current session/i })
    fireEvent.click(screen.getByRole('button', { name: /revoke current session/i }))
    const confirmations = screen.getAllByRole('button', {
      name: /revoke current session and sign out/i,
    })
    fireEvent.click(confirmations.at(-1)!)

    await waitFor(() => expect(api.revokeAccountSession).toHaveBeenCalledWith('current-session'))
    expect(onCurrentSessionRevoked).toHaveBeenCalledOnce()
  })

  it('keeps the account count consistent after revoking another session', async () => {
    api.revokeAccountSession.mockResolvedValue({ ok: true, signed_out: false, revoked: 1 })
    renderAccount()

    await screen.findByRole('button', { name: /revoke session from 192\.168\.1\.11/i })
    expect(screen.getByText('Active sessions').parentElement?.textContent).toContain('2')
    fireEvent.click(screen.getByRole('button', { name: /revoke session from 192\.168\.1\.11/i }))
    fireEvent.click(screen.getByRole('button', { name: 'Revoke 192.168.1.11' }))

    await waitFor(() => expect(screen.getByText('Active sessions').parentElement?.textContent).toContain('1'))
    expect(screen.queryByText('192.168.1.11')).toBeNull()
  })
})
