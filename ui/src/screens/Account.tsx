import { useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api'
import type { Account as AccountDTO, AccountSession, SessionInfo } from '../lib/api'
import { ago } from '../components/Chart'
import { Banner, Button, Card, Field, Notice } from '../components/ui'

export function Account({
  session,
  onCurrentSessionRevoked,
}: {
  session: SessionInfo
  onCurrentSessionRevoked: () => void
}) {
  const [account, setAccount] = useState<AccountDTO | null>(null)
  const [sessions, setSessions] = useState<AccountSession[]>([])
  const [loaded, setLoaded] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState('')
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [confirmRevoke, setConfirmRevoke] = useState<AccountSession | null>(null)

  const load = useCallback(async () => {
    const [accountResult, sessionsResult] = await Promise.allSettled([
      api.account(),
      api.accountSessions(),
    ])
    if (accountResult.status === 'fulfilled') setAccount(accountResult.value.account)
    if (sessionsResult.status === 'fulfilled') setSessions(sessionsResult.value.sessions)
    const failures = [accountResult, sessionsResult]
      .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
      .map((result) => result.reason instanceof Error ? result.reason.message : String(result.reason))
    setError(failures.join(' '))
    setLoaded(true)
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  async function changePassword(event: React.FormEvent) {
    event.preventDefault()
    setError('')
    setNotice('')
    if (newPassword !== confirmPassword) {
      setError('The two new passwords do not match.')
      return
    }
    if (newPassword.length < 12) {
      setError('The new password must be at least 12 characters.')
      return
    }
    setBusy('password')
    try {
      const result = await api.changePassword(currentPassword, newPassword)
      if (!result.ok) throw new Error('the controller did not confirm the password change')
      onCurrentSessionRevoked()
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause))
    } finally {
      setCurrentPassword('')
      setNewPassword('')
      setConfirmPassword('')
      setBusy('')
    }
  }

  async function revoke(target: AccountSession) {
    setBusy(`session:${target.id}`)
    setError('')
    setNotice('')
    try {
      const result = await api.revokeAccountSession(target.id)
      if (!result.ok) throw new Error('the controller did not confirm session revocation')
      if (target.current || result.signed_out) {
        onCurrentSessionRevoked()
        return
      }
      setSessions((current) => current.filter((item) => item.id !== target.id))
      setAccount((current) => current
        ? { ...current, active_session_count: Math.max(0, current.active_session_count - 1) }
        : current)
      setNotice('Session revoked.')
      setConfirmRevoke(null)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause))
    } finally {
      setBusy('')
    }
  }

  if (!loaded) return <div role="status">Loading account…</div>

  return (
    <div className="account-page">
      {error && <div role="alert"><Banner tone="critical">{error}</Banner></div>}
      {notice && <div role="status"><Banner tone="accent">{notice}</Banner></div>}

      <Card title="My account">
        <dl className="account-facts">
          <div><dt>Username</dt><dd>{account?.username ?? session.username}</dd></div>
          <div><dt>Role</dt><dd>{account?.role_label ?? session.role_label}</dd></div>
          <div><dt>Status</dt><dd>{account?.enabled === false ? 'Disabled' : 'Enabled'}</dd></div>
          <div><dt>Active sessions</dt><dd className="num">{account?.active_session_count ?? sessions.length}</dd></div>
        </dl>
      </Card>

      <Card title="Change password">
        <form className="account-form" aria-busy={busy === 'password'} onSubmit={changePassword}>
          <Field
            label="Current password"
            type="password"
            value={currentPassword}
            autoComplete="current-password"
            disabled={busy !== ''}
            required
            onChange={(event) => setCurrentPassword(event.target.value)}
          />
          <Field
            label="New password"
            type="password"
            value={newPassword}
            autoComplete="new-password"
            minLength={12}
            disabled={busy !== ''}
            required
            onChange={(event) => setNewPassword(event.target.value)}
          />
          <Field
            label="Repeat new password"
            type="password"
            value={confirmPassword}
            autoComplete="new-password"
            minLength={12}
            disabled={busy !== ''}
            required
            onChange={(event) => setConfirmPassword(event.target.value)}
          />
          <p className="account-help">
            Changing the password ends every session, including this one. Sign in again with the new password.
          </p>
          <div><Button type="submit" kind="primary" disabled={busy !== '' || !currentPassword || !newPassword || !confirmPassword}>
            {busy === 'password' ? 'Changing…' : 'Change password and sign out'}
          </Button></div>
        </form>
      </Card>

      <Card title={`Active sessions (${sessions.length})`} pad={false}>
        <div style={{ padding: 14, borderBottom: '1px solid var(--border)' }}>
          <Notice
            tone="accent"
            component="Controller sessions"
            summary="Review where this account is signed in and revoke access you do not recognize."
            details="Session records live in controller memory. Restarting the controller invalidates all sessions. Peer addresses identify the connection seen by the controller and may be a proxy address."
          />
        </div>
        {sessions.length === 0 ? (
          <div className="account-empty">No active sessions.</div>
        ) : (
          <div className="account-list">
            {sessions.map((item) => (
              <div className="account-list-row" key={item.id}>
                <div>
                  <div className="account-row-title">
                    {item.current ? 'Current session' : item.peer_address || 'Unknown peer'}
                    {item.current && <span className="account-pill">Current</span>}
                  </div>
                  <div className="account-row-detail">
                    {item.peer_address || 'Peer address unavailable'} · last active {ago(item.last_seen_at)} · expires {new Date(item.expires_at * 1000).toLocaleString()}
                  </div>
                </div>
                <Button
                  aria-label={item.current ? 'Revoke current session and sign out' : `Revoke session from ${item.peer_address || 'unknown peer'}`}
                  disabled={busy !== ''}
                  onClick={() => setConfirmRevoke(item)}
                >
                  Revoke
                </Button>
              </div>
            ))}
          </div>
        )}
      </Card>

      {confirmRevoke && (
        <Notice
          tone={confirmRevoke.current ? 'critical' : 'warning'}
          component="Session revocation"
          summary={confirmRevoke.current
            ? 'Revoke the current session and sign out now?'
            : `Revoke the session from ${confirmRevoke.peer_address || 'the unknown peer'}?`}
          details="Revocation is immediate and cannot be undone. It does not change the account password."
          defaultOpen
          actions={<>
            <Button
              disabled={busy !== ''}
              onClick={() => void revoke(confirmRevoke)}
            >
              {busy ? 'Revoking…' : confirmRevoke.current ? 'Revoke current session and sign out' : `Revoke ${confirmRevoke.peer_address || 'session'}`}
            </Button>
            <Button disabled={busy !== ''} onClick={() => setConfirmRevoke(null)}>Keep session</Button>
          </>}
        />
      )}
    </div>
  )
}
