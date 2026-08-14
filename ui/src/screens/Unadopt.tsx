import { useState } from 'react'
import { api, ApiError } from '../lib/api'
import type { UnadoptResult } from '../lib/api'
import { Button, Field, Banner, Card } from '../components/ui'

/**
 * Remove the controller from a device.
 *
 * Two phases, and the screen says why rather than hiding it. Phase 1 gives the
 * device's config back and runs under the controller's own login. Phase 2
 * removes that login and the ACL file, and needs the device's administrator
 * credential — the controller deliberately cannot do it, because write access
 * to its own ACL file is write access to arbitrary permissions after the next
 * login.
 *
 * Skipping the credential is a supported outcome, not an error. The device
 * keeps a listed, visible residue instead of a silently half-removed one, and
 * the list is exactly what someone has to delete by hand.
 */
export function Unadopt({
  deviceID,
  deviceName,
  onDone,
  onCancel,
}: {
  deviceID: number
  deviceName: string
  onDone: () => void
  onCancel: () => void
}) {
  const [username, setUsername] = useState('root')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [result, setResult] = useState<UnadoptResult | null>(null)

  async function run(withCredential: boolean) {
    setErr('')
    setBusy(true)
    try {
      const res = await api.unadopt(
        deviceID,
        withCredential ? { username, password } : {},
      )
      setResult(res)
      setPassword('')
      if (res.removed_from_inventory) onDone()
    } catch (e) {
      if (e instanceof ApiError && e.status === 409 && e.body) {
        // Not a failure. Phase 1 ran; phase 2 needs a credential the controller
        // deliberately does not hold, and the body carries the residue — which
        // is the useful part of the answer.
        setResult(e.body as UnadoptResult)
      } else {
        setErr(e instanceof Error ? e.message : String(e))
      }
    } finally {
      setBusy(false)
    }
  }

  if (result) {
    return (
      <div style={{ display: 'grid', gap: 12 }}>
        {result.removed_from_inventory ? (
          <Banner tone="accent">
            <strong>{deviceName}</strong> was removed.{' '}
            {result.footprint_remains
              ? 'A footprint remains on the device — see below.'
              : 'Nothing of ours is left on it.'}
          </Banner>
        ) : (
          <Banner>
            {deviceName} is still in the inventory. Phase 1 finished; removing
            the login and the ACL file needs the device's administrator
            credential.
          </Banner>
        )}

        <Card title="What happened">
          <ul style={{ margin: 0, paddingLeft: 18, fontSize: 12 }}>
            <li>
              {result.reverted_sections} configuration section
              {result.reverted_sections === 1 ? '' : 's'} handed back
            </li>
            <li>rpcd login {result.login_removed ? 'removed' : 'still present'}</li>
            <li>ACL file {result.acl_removed ? 'removed' : 'still present'}</li>
          </ul>
          {result.residue && result.residue.length > 0 && (
            <div style={{ marginTop: 10 }}>
              <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4 }}>
                Left on the device
              </div>
              <ul style={{ margin: 0, paddingLeft: 18, fontSize: 11, color: 'var(--text-secondary)' }}>
                {result.residue.map((r) => (
                  <li key={r}>
                    <code>{r}</code>
                  </li>
                ))}
              </ul>
              <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
                Delete these over SSH, or supply the credential and try again.
              </div>
            </div>
          )}
          {result.errors && result.errors.length > 0 && (
            <div style={{ marginTop: 10 }}>
              {result.errors.map((e) => (
                <Banner key={e} tone="warning">
                  {e}
                </Banner>
              ))}
            </div>
          )}
        </Card>

        <div style={{ display: 'flex', gap: 8 }}>
          {result.needs_operator_credential && (
            <Button kind="primary" onClick={() => setResult(null)}>
              Supply the credential
            </Button>
          )}
          <Button onClick={onCancel}>Close</Button>
        </div>
      </div>
    )
  }

  return (
    <div style={{ display: 'grid', gap: 12 }}>
      <Banner tone="warning">
        This gives <strong>{deviceName}</strong> back: the configuration
        sections the controller owns are reverted, then its login and ACL file
        are deleted. Its own settings are not touched.
      </Banner>

      {err && <Banner tone="critical">{err}</Banner>}

      <Card title="Device administrator credential">
        <p style={{ fontSize: 12, color: 'var(--text-secondary)', margin: '0 0 10px' }}>
          Needed to delete the controller's own login and permissions. The
          controller cannot do that itself — being able to rewrite its own ACL
          file would let it grant itself anything.
        </p>
        <div style={{ display: 'grid', gap: 10 }}>
          <Field
            label="Username"
            value={username}
            autoComplete="off"
            onChange={(e) => setUsername(e.target.value)}
          />
          <Field
            label="Password"
            type="password"
            value={password}
            autoComplete="off"
            onChange={(e) => setPassword(e.target.value)}
          />
        </div>
      </Card>

      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        <Button kind="primary" disabled={busy || !username} onClick={() => run(true)}>
          {busy ? 'Removing…' : 'Remove completely'}
        </Button>
        <Button disabled={busy} onClick={() => run(false)}>
          Revert config only
        </Button>
        <Button disabled={busy} onClick={onCancel}>
          Cancel
        </Button>
      </div>
      <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
        “Revert config only” hands the settings back and leaves the login and
        ACL file in place, listed so you can delete them by hand. Use it when
        the device's password is lost.
      </div>
    </div>
  )
}
