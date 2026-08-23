import { useCallback, useEffect, useState } from 'react'
import { ApiError, api } from '../lib/api'
import type {
  Account,
  AccountRole,
  AccountRoleOption,
  AccountSession,
  SessionInfo,
} from '../lib/api'
import { ago } from '../components/Chart'
import { Banner, Button, Card, Field, Notice } from '../components/ui'

type PendingOperation =
  | { type: 'create'; username: string; role: AccountRole }
  | { type: 'role'; accountID: number; username: string; role: AccountRole }
  | { type: 'enabled'; accountID: number; username: string; enabled: boolean }
  | { type: 'password'; accountID: number; username: string }
  | { type: 'delete'; accountID: number; username: string }
  | { type: 'revoke-session'; accountID: number; username: string; sessionID: string; peer: string }
  | { type: 'revoke-sessions'; accountID: number; username: string }

type AccountAction =
  | { type: 'role'; account: Account; role: AccountRole }
  | { type: 'enabled'; account: Account }
  | { type: 'password'; account: Account }
  | { type: 'delete'; account: Account }
  | { type: 'sessions'; account: Account }

const fallbackRoles: AccountRoleOption[] = [
  { value: 'owner', label: 'Owner', description: 'Full controller and account administration.' },
  { value: 'admin', label: 'Administrator', description: 'Full network administration without account ownership.' },
  { value: 'operator', label: 'Operator', description: 'Operate the network without administrative account changes.' },
  { value: 'viewer', label: 'Read only', description: 'View controller state without mutations.' },
]

function operationLabel(operation: PendingOperation) {
  switch (operation.type) {
    case 'create': return `create “${operation.username}”`
    case 'role': return `change “${operation.username}” to ${operation.role}`
    case 'enabled': return `${operation.enabled ? 'enable' : 'disable'} “${operation.username}”`
    case 'password': return `reset the password for “${operation.username}”`
    case 'delete': return `delete “${operation.username}”`
    case 'revoke-session': return `revoke ${operation.peer} for “${operation.username}”`
    case 'revoke-sessions': return `revoke every session for “${operation.username}”`
  }
}

export function Accounts({
  session,
  onSessionChange,
  onCurrentSessionRevoked,
}: {
  session: SessionInfo
  onSessionChange: (session: SessionInfo) => void
  onCurrentSessionRevoked: () => void
}) {
  const [accounts, setAccounts] = useState<Account[]>([])
  const [roles, setRoles] = useState<AccountRoleOption[]>(fallbackRoles)
  const [loaded, setLoaded] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState('')
  const [action, setAction] = useState<AccountAction | null>(null)
  const [accountSessions, setAccountSessions] = useState<AccountSession[]>([])
  const [createUsername, setCreateUsername] = useState('')
  const [createRole, setCreateRole] = useState<AccountRole>('viewer')
  const [createPassword, setCreatePassword] = useState('')
  const [createConfirm, setCreateConfirm] = useState('')
  const [resetPassword, setResetPassword] = useState('')
  const [resetConfirm, setResetConfirm] = useState('')
  const [pending, setPending] = useState<PendingOperation | null>(null)
  const [reauthenticated, setReauthenticated] = useState(false)
  const [reauthPassword, setReauthPassword] = useState('')

  const refresh = useCallback(async () => {
    try {
      const response = await api.accounts()
      setAccounts(response.accounts)
      setRoles(response.roles.length ? response.roles : fallbackRoles)
      setError('')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause))
    } finally {
      setLoaded(true)
    }
  }, [])

  useEffect(() => {
    void refresh()
  }, [refresh])

  function requireReauthentication(cause: unknown, operation: PendingOperation) {
    if (!(cause instanceof ApiError && cause.status === 428)) return false
    setPending(operation)
    setReauthenticated(false)
    setCreatePassword('')
    setCreateConfirm('')
    setResetPassword('')
    setResetConfirm('')
    setError('Confirm your current password before this owner action. The action has not run.')
    return true
  }

  async function refreshCurrentSession(accountID: number) {
    if (accountID !== session.admin_id) return
    try {
      onSessionChange(await api.session())
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) onCurrentSessionRevoked()
    }
  }

  async function run(operation: PendingOperation, password = '') {
    setBusy(operation.type)
    setError('')
    setNotice('')
    try {
      switch (operation.type) {
        case 'create':
          if (password.length < 12) throw new Error('The password must be at least 12 characters.')
          await api.createAccount(operation.username, password, operation.role)
          setCreateUsername('')
          setAction(null)
          break
        case 'role':
          if ((await api.setAccountRole(operation.accountID, operation.role)).signed_out) {
            onCurrentSessionRevoked()
            return
          }
          await refreshCurrentSession(operation.accountID)
          setAction(null)
          break
        case 'enabled':
          if ((await api.setAccountEnabled(operation.accountID, operation.enabled)).signed_out) {
            onCurrentSessionRevoked()
            return
          }
          await refreshCurrentSession(operation.accountID)
          setAction(null)
          break
        case 'password':
          if (password.length < 12) throw new Error('The password must be at least 12 characters.')
          const resetResult = await api.resetAccountPassword(operation.accountID, password)
          if (resetResult.signed_out) {
            onCurrentSessionRevoked()
            return
          }
          setAction(null)
          break
        case 'delete':
          const deleteResult = await api.deleteAccount(operation.accountID)
          if (deleteResult.signed_out) {
            onCurrentSessionRevoked()
            return
          }
          setAction(null)
          break
        case 'revoke-session': {
          const result = await api.revokeManagedAccountSession(operation.accountID, operation.sessionID)
          const current = accountSessions.find((item) => item.id === operation.sessionID)?.current
          if (current || result.signed_out) {
            onCurrentSessionRevoked()
            return
          }
          setAccountSessions((items) => items.filter((item) => item.id !== operation.sessionID))
          break
        }
        case 'revoke-sessions': {
          const result = await api.revokeManagedAccountSessions(operation.accountID)
          if (result.signed_out) {
            onCurrentSessionRevoked()
            return
          }
          setAccountSessions([])
          break
        }
      }
      setPending(null)
      setReauthenticated(false)
      setNotice(`${operationLabel(operation)} completed.`)
      await refresh()
    } catch (cause) {
      if (!requireReauthentication(cause, operation)) {
        setError(cause instanceof Error ? cause.message : String(cause))
      }
    } finally {
      if (operation.type === 'create') {
        setCreatePassword('')
        setCreateConfirm('')
      }
      if (operation.type === 'password') {
        setResetPassword('')
        setResetConfirm('')
      }
      setBusy('')
    }
  }

  async function reauthenticate(event: React.FormEvent) {
    event.preventDefault()
    setBusy('reauth')
    setError('')
    try {
      await api.reauthenticate(reauthPassword)
      setReauthenticated(true)
      setNotice(`Identity confirmed. Explicitly retry ${pending ? operationLabel(pending) : 'the action'}.`)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause))
    } finally {
      setReauthPassword('')
      setBusy('')
    }
  }

  async function loadSessions(account: Account) {
    setBusy('sessions')
    setError('')
    try {
      const response = await api.managedAccountSessions(account.id)
      setAccountSessions(response.sessions)
      setAction({ type: 'sessions', account })
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause))
    } finally {
      setBusy('')
    }
  }

  function cancelPending() {
    setPending(null)
    setReauthenticated(false)
    setReauthPassword('')
    setCreatePassword('')
    setCreateConfirm('')
    setResetPassword('')
    setResetConfirm('')
    setError('')
  }

  if (!loaded) return <div role="status">Loading accounts…</div>

  return (
    <div className="account-page">
      <Notice
        tone="accent"
        component="Account authorization"
        summary="Only owners can administer controller accounts."
        details="This tab is a convenience, not the security boundary. The controller checks the signed-in account role on every request and records account changes in the audit history."
      />
      {error && <div role="alert"><Banner tone="critical">{error}</Banner></div>}
      {notice && <div role="status"><Banner tone="accent">{notice}</Banner></div>}

      {pending && (
        <Notice
          tone="warning"
          component="Owner reauthentication"
          summary={reauthenticated
            ? `Identity confirmed. ${operationLabel(pending)} has not run.`
            : `${operationLabel(pending)} needs your current password.`}
          details="The password is sent only for this reauthentication request, cleared from the form immediately, and never cached. Reauthentication does not retry the owner action automatically."
          defaultOpen
          actions={reauthenticated ? (
            pending.type === 'create' || pending.type === 'password' ? (
              <span>Re-enter the new account password below, then use the named Retry button.</span>
            ) : <>
              <Button disabled={busy !== ''} onClick={() => void run(pending)}>
                {busy ? 'Retrying…' : `Retry ${operationLabel(pending)}`}
              </Button>
              <Button disabled={busy !== ''} onClick={cancelPending}>Cancel action</Button>
            </>
          ) : (
            <form className="account-reauth" onSubmit={reauthenticate}>
              <Field
                label="Current password"
                type="password"
                value={reauthPassword}
                autoComplete="current-password"
                disabled={busy !== ''}
                required
                onChange={(event) => setReauthPassword(event.target.value)}
              />
              <Button type="submit" kind="primary" disabled={busy !== '' || !reauthPassword}>
                {busy === 'reauth' ? 'Confirming…' : 'Confirm identity'}
              </Button>
              <Button disabled={busy !== ''} onClick={cancelPending}>Cancel action</Button>
            </form>
          )}
        />
      )}

      <Card title="Create account">
        <form
          className="account-form account-create-form"
          onSubmit={(event) => {
            event.preventDefault()
            if (createPassword !== createConfirm) {
              setError('The two passwords do not match.')
              return
            }
            const operation = pending?.type === 'create' && reauthenticated
              ? pending
              : { type: 'create' as const, username: createUsername, role: createRole }
            void run(operation, createPassword)
          }}
        >
          <Field
            label="Username"
            value={createUsername}
            autoComplete="off"
            disabled={busy !== '' || pending?.type === 'create'}
            required
            onChange={(event) => setCreateUsername(event.target.value)}
          />
          <RolePicker label="Role" roles={roles} value={createRole} disabled={busy !== '' || pending?.type === 'create'} onChange={setCreateRole} />
          <Field label="Password" type="password" value={createPassword} autoComplete="new-password" minLength={12} disabled={busy !== '' || Boolean(pending && !reauthenticated)} required onChange={(event) => setCreatePassword(event.target.value)} />
          <Field label="Repeat password" type="password" value={createConfirm} autoComplete="new-password" minLength={12} disabled={busy !== '' || Boolean(pending && !reauthenticated)} required onChange={(event) => setCreateConfirm(event.target.value)} />
          <div><Button type="submit" kind="primary" disabled={busy !== '' || !createUsername || !createPassword || !createConfirm || Boolean(pending && (!reauthenticated || pending.type !== 'create'))}>
            {busy === 'create' ? 'Creating…' : pending?.type === 'create' && reauthenticated ? `Retry create “${pending.username}”` : 'Create account'}
          </Button></div>
        </form>
      </Card>

      <Card title={`Controller accounts (${accounts.length})`} pad={false}>
        {accounts.length === 0 ? <div className="account-empty">No accounts returned.</div> : (
          <div className="account-list">
            {accounts.map((account) => (
              <div className="account-list-row account-admin-row" key={account.id}>
                <div>
                  <div className="account-row-title">
                    {account.username}
                    {account.id === session.admin_id && <span className="account-pill">You</span>}
                    {!account.enabled && <span className="account-pill account-pill-muted">Disabled</span>}
                  </div>
                  <div className="account-row-detail">
                    {account.role_label} · {account.active_session_count} active {account.active_session_count === 1 ? 'session' : 'sessions'} · last sign-in {ago(account.last_login_at)}
                  </div>
                </div>
                <div className="account-row-actions">
                  <Button disabled={busy !== '' || Boolean(pending)} onClick={() => setAction({ type: 'role', account, role: account.role })}>Role</Button>
                  <Button disabled={busy !== '' || Boolean(pending)} onClick={() => setAction({ type: 'enabled', account })}>{account.enabled ? 'Disable' : 'Enable'}</Button>
                  <Button disabled={busy !== '' || Boolean(pending)} onClick={() => setAction({ type: 'password', account })}>Reset password</Button>
                  <Button disabled={busy !== '' || Boolean(pending)} onClick={() => void loadSessions(account)}>Sessions</Button>
                  <Button disabled={busy !== '' || Boolean(pending)} onClick={() => setAction({ type: 'delete', account })}>Delete</Button>
                </div>
              </div>
            ))}
          </div>
        )}
      </Card>

      {action && <AccountActionPanel
        action={action}
        roles={roles}
        sessions={accountSessions}
        busy={busy}
        pending={pending}
        reauthenticated={reauthenticated}
        resetPassword={resetPassword}
        resetConfirm={resetConfirm}
        onResetPassword={setResetPassword}
        onResetConfirm={setResetConfirm}
        onRole={(role) => setAction(action.type === 'role' ? { ...action, role } : action)}
        onClose={() => {
          setAction(null)
          setAccountSessions([])
          setResetPassword('')
          setResetConfirm('')
        }}
        onRun={(operation, password) => void run(operation, password)}
      />}
    </div>
  )
}

function AccountActionPanel({
  action,
  roles,
  sessions,
  busy,
  pending,
  reauthenticated,
  resetPassword,
  resetConfirm,
  onResetPassword,
  onResetConfirm,
  onRole,
  onClose,
  onRun,
}: {
  action: AccountAction
  roles: AccountRoleOption[]
  sessions: AccountSession[]
  busy: string
  pending: PendingOperation | null
  reauthenticated: boolean
  resetPassword: string
  resetConfirm: string
  onResetPassword: (value: string) => void
  onResetConfirm: (value: string) => void
  onRole: (role: AccountRole) => void
  onClose: () => void
  onRun: (operation: PendingOperation, password?: string) => void
}) {
  const account = action.account
  const blocked = busy !== '' || Boolean(pending && !reauthenticated)
  const [confirmation, setConfirmation] = useState<PendingOperation | null>(null)
  useEffect(() => setConfirmation(null), [account.id, action.type])

  if (action.type === 'sessions') {
    return <Card title={`Sessions for ${account.username}`} actions={<Button disabled={busy !== '' || Boolean(pending)} onClick={onClose}>Close</Button>} pad={false}>
      {sessions.length === 0 ? <div className="account-empty">No active sessions.</div> : <div className="account-list">
        {sessions.map((item) => <div className="account-list-row" key={item.id}>
          <div>
            <div className="account-row-title">{item.peer_address || 'Unknown peer'}{item.current && <span className="account-pill">Current</span>}</div>
            <div className="account-row-detail">Last active {ago(item.last_seen_at)} · expires {new Date(item.expires_at * 1000).toLocaleString()}</div>
          </div>
          <Button disabled={busy !== '' || Boolean(pending)} onClick={() => setConfirmation({ type: 'revoke-session', accountID: account.id, username: account.username, sessionID: item.id, peer: item.peer_address || 'unknown session' })}>
            Revoke {item.current ? 'current session' : item.peer_address || 'session'}
          </Button>
        </div>)}
        <div className="account-list-row">
          <span>End every active session for <strong>{account.username}</strong>.</span>
          <Button disabled={busy !== '' || Boolean(pending)} onClick={() => setConfirmation({ type: 'revoke-sessions', accountID: account.id, username: account.username })}>
            Revoke all for “{account.username}”
          </Button>
        </div>
      </div>}
      {confirmation && <div style={{ padding: 14, borderTop: '1px solid var(--border)' }}>
        <Notice
          tone="critical"
          component="Session revocation"
          summary={`${operationLabel(confirmation)}?`}
          details="Revocation is immediate and cannot be undone. Account credentials are unchanged."
          defaultOpen
          actions={<>
            <Button disabled={busy !== '' || Boolean(pending)} onClick={() => {
              const operation = confirmation
              setConfirmation(null)
              onRun(operation)
            }}>{operationLabel(confirmation)}</Button>
            <Button disabled={busy !== ''} onClick={() => setConfirmation(null)}>Keep sessions</Button>
          </>}
        />
      </div>}
    </Card>
  }

  if (action.type === 'password') {
    const retry = pending?.type === 'password' && reauthenticated
    return <Card title={`Reset password for ${account.username}`} actions={<Button disabled={busy !== ''} onClick={onClose}>Close</Button>}>
      <form className="account-form" onSubmit={(event) => {
        event.preventDefault()
        if (resetPassword !== resetConfirm) return
        onRun({ type: 'password', accountID: account.id, username: account.username }, resetPassword)
      }}>
        <Field label="New password" type="password" value={resetPassword} autoComplete="new-password" minLength={12} disabled={blocked} required onChange={(event) => onResetPassword(event.target.value)} />
        <Field label="Repeat new password" type="password" value={resetConfirm} autoComplete="new-password" minLength={12} disabled={blocked} required onChange={(event) => onResetConfirm(event.target.value)} />
        {resetPassword !== resetConfirm && resetConfirm && <div role="alert" className="account-inline-error">The two passwords do not match.</div>}
        <div><Button type="submit" kind="primary" disabled={blocked || !resetPassword || resetPassword !== resetConfirm}>
          {busy === 'password' ? 'Resetting…' : retry ? `Retry reset for “${account.username}”` : `Reset password for “${account.username}”`}
        </Button></div>
      </form>
    </Card>
  }

  const operation: PendingOperation = action.type === 'role'
    ? { type: 'role', accountID: account.id, username: account.username, role: action.role }
    : action.type === 'enabled'
      ? { type: 'enabled', accountID: account.id, username: account.username, enabled: !account.enabled }
      : { type: 'delete', accountID: account.id, username: account.username }

  return <Notice
    tone={action.type === 'role' ? 'warning' : 'critical'}
    component="Account change"
    summary={action.type === 'role'
      ? `Change the role for “${account.username}”?`
      : action.type === 'enabled'
        ? `${account.enabled ? 'Disable' : 'Enable'} “${account.username}”?`
        : `Delete “${account.username}”?`}
    details={action.type === 'role' ? <RolePicker label="New role" roles={roles} value={action.role} disabled={blocked} onChange={onRole} />
      : action.type === 'enabled' ? (account.enabled
        ? 'Disabling an account immediately prevents new sign-ins and ends its sessions.'
        : 'Enabling the account permits sign-in again. Its password is unchanged.')
        : 'Deletion ends every session and removes the account. This cannot be undone.'}
    defaultOpen
    actions={<>
      <Button disabled={blocked || action.type === 'role' && action.role === account.role} onClick={() => onRun(operation)}>
        {busy ? 'Working…' : `${pending && reauthenticated ? 'Retry ' : ''}${operationLabel(operation)}`}
      </Button>
      <Button disabled={busy !== ''} onClick={onClose}>Keep “{account.username}” unchanged</Button>
    </>}
  />
}

function RolePicker({
  label,
  roles,
  value,
  disabled,
  onChange,
}: {
  label: string
  roles: AccountRoleOption[]
  value: AccountRole
  disabled: boolean
  onChange: (role: AccountRole) => void
}) {
  return <label className="account-picker">
    <span>{label}</span>
    <select value={value} disabled={disabled} onChange={(event) => onChange(event.target.value as AccountRole)}>
      {roles.map((role) => <option key={role.value} value={role.value}>{role.label}</option>)}
    </select>
    <small>{roles.find((role) => role.value === value)?.description}</small>
  </label>
}
