import { useEffect, useState } from 'react'
import { api, ApiError } from '../lib/api'
import type { UnadoptResult } from '../lib/api'
import { Button, Field, Banner, Card, Toggle } from '../components/ui'

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
 *
 * And there is a third outcome, which this screen used not to offer at all:
 * the device cannot be reached AT ALL. Dead hardware, a reflash, a lost
 * administrator password. The API has always taken `force` for exactly that
 * case and no screen ever sent it, so a router that no longer exists could
 * never leave the inventory — it stayed listed, polled and counted forever,
 * and the only way out was a hand-written API call.
 */
/**
 * Is this response body an un-adopt report rather than a plain error?
 *
 * Keyed on `removed_from_inventory`, which is the one field the Go type always
 * emits — no `omitempty` — so its presence distinguishes a report from the
 * `{"error": "..."}` every other failure returns. Checking a field that can be
 * omitted would read a real report as a plain error whenever it happened to be
 * empty.
 */
function isReport(b: unknown): b is UnadoptResult {
  return (
    typeof b === 'object' && b !== null && 'removed_from_inventory' in b
  )
}

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
  const [confirmed, setConfirmed] = useState(false)
  // null means the list could not be read, which is NOT the same as owning no
  // sections — the panel says so rather than showing an empty list.
  const [sections, setSections] = useState<string[] | null>([])
  const [username, setUsername] = useState('root')

  useEffect(() => {
    // Read the sections this controller wrote, so the operator sees WHAT is
    // about to be reverted rather than a count afterwards. A failed read sets
    // null, which the panel renders as "could not be read" — never as an empty
    // list, which would read as "this controller wrote nothing here".
    api
      .device(deviceID)
      .then((d) => setSections(d.owned_sections ?? []))
      .catch(() => setSections(null))
  }, [deviceID])
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [result, setResult] = useState<UnadoptResult | null>(null)
  // Its own confirmation, separate from the one above, because it is its own
  // decision: the first says "revert this device", this one says "give up on
  // reaching it and lose the record of what is still installed".
  const [forceOK, setForceOK] = useState(false)
  // Whether the attempt that failed carried a credential. A forced retry sends
  // the same thing: the daemon still TRIES phase 2 and only skips it when the
  // connection fails, so carrying the credential means a device that turns out
  // to be reachable is cleaned properly rather than abandoned on our say-so.
  const [triedWithCredential, setTriedWithCredential] = useState(false)

  async function run(withCredential: boolean, force = false) {
    setErr('')
    setBusy(true)
    setTriedWithCredential(withCredential)
    // Every attempt re-earns the forced-removal confirmation. It used to
    // persist, so someone who ticked it, thought better of it, and retried with
    // a corrected password was one click from a forced removal the moment the
    // second attempt failed — which is the speed bump gone at exactly the point
    // it exists for. The ordinary confirmation above deliberately does NOT
    // reset: that one is consent to un-adopt this device, which a retry of the
    // same operation does not withdraw.
    setForceOK(false)
    try {
      const res = await api.unadopt(deviceID, {
        ...(withCredential ? { username, password } : {}),
        ...(force ? { force: true } : {}),
      })
      setResult(res)
      setPassword('')
      // NOT onDone() here. That unmounted the whole panel the instant a removal
      // succeeded, so the report underneath — including the residue, which is
      // the last copy of what is still on the device once the row is gone —
      // was rendered and discarded in the same tick. Close does it instead.
    } catch (e) {
      // Any report-shaped body IS the answer, whatever the status.
      //
      // This used to accept only 409 — phase 1 done, phase 2 needing a
      // credential the controller deliberately does not hold. But a 502 can
      // carry the same report: a phase-2 failure with the credential supplied,
      // and a forced removal that connected and then could not commit. In that
      // last case the row is already gone and the residue list is the only
      // record left of what is on the device, so falling through to a bare
      // error banner destroyed it.
      if (e instanceof ApiError && isReport(e.body)) {
        setResult(e.body)
      } else {
        setErr(e instanceof Error ? e.message : String(e))
      }
    } finally {
      setBusy(false)
    }
  }

  // Offered only after something has actually failed. Before that it is a
  // shortcut past the part that gives the device its configuration back, and
  // the ordinary path is the one that should be easy to take.
  const forceBlock = (
    <Card title="If the device cannot be reached at all">
      <p style={{ fontSize: 12, color: 'var(--text-secondary)', margin: '0 0 10px' }}>
        Remove <strong>{deviceName}</strong> from the inventory anyway. This
        does <strong>not</strong> touch the device: the controller's login and
        its ACL file stay on it, and once this row is gone the controller no
        longer holds a record of what to delete. The response will list it — it
        is the last copy, so keep it. Use this for hardware that is gone for
        good, that was reflashed, or whose administrator password is lost.
      </p>
      <Toggle
        label="I understand — the controller's footprint stays on the device"
        on={forceOK}
        onChange={setForceOK}
      />
      <div style={{ marginTop: 10 }}>
        <Button
          disabled={busy || !forceOK}
          onClick={() => run(triedWithCredential, true)}
        >
          {busy ? 'Removing…' : 'Remove from the inventory anyway'}
        </Button>
      </div>
    </Card>
  )

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
        ) : result.needs_operator_credential ? (
          <Banner>
            {deviceName} is still in the inventory. Phase 1 finished; removing
            the login and the ACL file needs the device's administrator
            credential.
          </Banner>
        ) : (
          /* Not the same thing, and it used to say it was. This branch is
             reached when a credential WAS supplied and phase 2 failed anyway,
             which the old copy described as needing a credential — sending the
             operator to re-type one that is already correct. */
          <Banner tone="warning">
            {deviceName} is still in the inventory, and the removal did not
            finish. {result.error ?? 'See what happened below.'}
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
              {/* "Try again" is advice only while there is still a row to try
                  against. After a forced removal there is not, and this list is
                  the only remaining record of what is on that device — telling
                  someone to come back to a device the controller has forgotten
                  is how a footprint gets left behind for good. */}
              <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
                {result.removed_from_inventory
                  ? 'Copy these before closing. The device is no longer in the ' +
                    'inventory, so this is the last time the controller can tell ' +
                    'you what it left behind — delete them over SSH.'
                  : 'Delete these over SSH, or supply the credential and try again.'}
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

        {!result.removed_from_inventory && forceBlock}

        <div style={{ display: 'flex', gap: 8 }}>
          {result.needs_operator_credential && (
            <Button kind="primary" onClick={() => setResult(null)}>
              Supply the credential
            </Button>
          )}
          {/* onDone refreshes the fleet and closes the panel; onCancel only
              closes this form. Which one is correct depends on whether the row
              is actually gone, and calling onDone the moment the request
              returned is what threw the report away. */}
          <Button onClick={result.removed_from_inventory ? onDone : onCancel}>
            Close
          </Button>
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

      {/* Named, not counted, and BEFORE the button rather than after it.
          Un-adopt is the most destructive thing this controller does and it is
          the one operation with no rollback armed — yet it used to report a
          number, once it was already done, while the safer apply path got a
          full preview and a confirmation. */}
      <Card title="What will be reverted on the device">
        {sections === null ? (
          <div style={{ fontSize: 12, color: 'var(--warning)' }}>
            The list of sections this controller owns could not be read. That is
            not the same as owning none — check before continuing.
          </div>
        ) : sections.length === 0 ? (
          <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
            This controller has no recorded sections on {deviceName}, so nothing
            of its configuration will be changed. Its login and ACL file are
            still removed.
          </div>
        ) : (
          <ul style={{ margin: 0, paddingLeft: 18, fontSize: 12 }}>
            {sections.map((sec) => (
              <li key={sec}>
                <code>{sec}</code>
              </li>
            ))}
          </ul>
        )}
      </Card>

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

      {/* The same speed bump the apply path has, on the operation that needs
          it more: an apply comes back by itself if the device is unhealthy,
          and this does not. */}
      <Toggle
        label="I understand — this reverts the sections above and removes the controller's access"
        on={confirmed}
        onChange={setConfirmed}
      />

      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        {/* Not the primary button. The visually dominant action should not be
            the irreversible one. */}
        <Button disabled={busy || !username || !confirmed} onClick={() => run(true)}>
          {busy ? 'Removing…' : 'Remove completely'}
        </Button>
        <Button disabled={busy || !confirmed} onClick={() => run(false)}>
          Revert config only
        </Button>
        <Button disabled={busy} onClick={onCancel}>
          Cancel
        </Button>
      </div>
      {/* Stays with the buttons it explains. */}
      <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
        “Revert config only” hands the settings back and leaves the login and
        ACL file in place, listed so you can delete them by hand. Use it when
        the device's password is lost.
      </div>

      {/* Last, deliberately. A hard failure never produces a result, so without
          this the only way past a refused SSH connection — a changed host key,
          a dead address, a wrong password — is to give up. But it sits BELOW
          the credential form and the ordinary buttons, because the commonest
          reason to be here is a typo in the password, and an escape hatch
          offered above the retry is an invitation to abandon a device that was
          one keystroke from being cleaned properly. */}
      {err && forceBlock}
    </div>
  )
}
