import { useState } from 'react'
import { api, ApiError } from '../lib/api'
import type { SessionInfo } from '../lib/api'
import { Button, Field, Banner } from '../components/ui'

/**
 * Sign-in and first-run enrolment.
 *
 * One screen for both, chosen by the server's answer to "does an account
 * exist" rather than by a route the user could pick. There is no default
 * credential to change afterwards, which is the whole point: a shipped default
 * nobody rotates is the most common way a self-hosted controller ends up on the
 * internet with a known password.
 */
export function Auth({
  needsSetup,
  onSignedIn,
}: {
  needsSetup: boolean
  onSignedIn: (session: SessionInfo) => void
}) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setErr('')
    if (needsSetup && password !== confirm) {
      setErr('The two passwords do not match.')
      return
    }
    setBusy(true)
    try {
      const info = needsSetup
        ? await api.setup(username, password)
        : await api.login(username, password)
      onSignedIn(info)
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : 'Could not reach the controller.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div
      className="auth-screen"
      style={{
        minHeight: '100%',
        display: 'grid',
        placeItems: 'center',
        background: 'var(--surface-0)',
      }}
    >
      <form
        className="auth-card"
        onSubmit={submit}
        aria-busy={busy}
        style={{
          width: 340,
          maxWidth: '100%',
          display: 'grid',
          gap: 14,
          padding: 24,
          background: 'var(--surface-1)',
          border: '1px solid var(--border)',
          borderRadius: 8,
        }}
      >
        <div>
          <h1 style={{ fontSize: 18, margin: 0 }}>oonfeeWRT</h1>
          <p style={{ fontSize: 12, color: 'var(--text-secondary)', margin: '4px 0 0' }}>
            {needsSetup
              ? 'Create the administrator account. This happens once — there is no default password.'
              : 'Sign in to the controller.'}
          </p>
        </div>

        {err && <div role="alert"><Banner tone="critical">{err}</Banner></div>}

        <Field
          label="Username"
          style={{ minHeight: 44 }}
          value={username}
          autoComplete="username"
          autoFocus
          onChange={(e) => setUsername(e.target.value)}
        />
        <Field
          label="Password"
          style={{ minHeight: 44 }}
          type="password"
          value={password}
          autoComplete={needsSetup ? 'new-password' : 'current-password'}
          onChange={(e) => setPassword(e.target.value)}
        />
        {needsSetup && (
          <>
            <Field
              label="Repeat password"
              style={{ minHeight: 44 }}
              type="password"
              value={confirm}
              autoComplete="new-password"
              onChange={(e) => setConfirm(e.target.value)}
            />
            <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
              At least 12 characters. Length is the only rule — composition rules
              push people toward predictable substitutions.
            </div>
          </>
        )}

        <Button type="submit" kind="primary" style={{ minHeight: 44 }}
          disabled={busy || !username || !password}>
          {busy ? 'Working…' : needsSetup ? 'Create account' : 'Sign in'}
        </Button>
      </form>
    </div>
  )
}
