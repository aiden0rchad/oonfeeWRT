import { useRef, useState } from 'react'
import { api, ApiError } from '../lib/api'
import type { AdoptResult } from '../lib/api'
import { Button, Field, Banner, Card, Prop } from '../components/ui'
import type { DeviceRole } from '../lib/api'

/** Kept in step with internal/model/role.go, which is where the rule lives:
 *  an unrecognised role is refused by the API rather than stored. */
const ROLES: { value: DeviceRole; describe: string }[] = [
  {
    value: 'gateway',
    describe:
      'Routes between networks and to the internet. Gets addressing, DHCP and firewall rules.',
  },
  {
    value: 'ap',
    describe:
      'Publishes WLANs and passes tagged traffic through. Does not route or serve DHCP.',
  },
  {
    value: 'switch',
    describe:
      'Passes tagged traffic only. No WLANs are sent to it even if it has radios.',
  },
]
import { Discover } from './Discover'

/**
 * Bring a device under management.
 *
 * The form asks for the ROUTER's existing administrator credential, which the
 * controller uses once and never stores — it creates its own scoped login and
 * keeps only that. The screen says so, because "type your router password into
 * this box" deserves an explanation rather than a tooltip.
 *
 * Adoption is synchronous and takes a few seconds: the capability probe samples
 * the survey twice on purpose, and the controller verifies the login it just
 * created before reporting success. A spinner with the actual steps beats a
 * progress bar that means nothing.
 */
export function Adopt({ onAdopted }: { onAdopted: () => void }) {
  const [host, setHost] = useState('')
  const [name, setName] = useState('')
  const [username, setUsername] = useState('root')
  const [password, setPassword] = useState('')
  const [scheme, setScheme] = useState<'http' | 'https'>('http')
  // Defaults to an access point: the role that changes least about a device.
  // Defaulting to gateway would hand an unlabelled router addressing, DHCP and
  // firewall rules nobody asked for.
  const [role, setRole] = useState<DeviceRole>('ap')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [result, setResult] = useState<AdoptResult | null>(null)
  const passwordRef = useRef<HTMLInputElement>(null)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setErr('')
    setBusy(true)
    try {
      const res = await api.adopt({ host, name, username, password, scheme, role })
      setResult(res)
      // The password is gone from this component the moment it is not needed.
      setPassword('')
      onAdopted()
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  if (result) {
    return (
      <div style={{ display: 'grid', gap: 14, maxWidth: 620 }}>
        <Banner tone="accent">
          <strong>{result.name}</strong> is now managed. The controller created
          its own login on the device; the password you typed was not stored.
        </Banner>
        {result.warnings?.map((w) => (
          <Banner key={w} tone="critical">
            {w}
          </Banner>
        ))}
        <Card title="What the capability probe found">
          <div style={{ display: 'grid', gap: 6 }}>
            <Prop label="Model">{result.model || '—'}</Prop>
            <Prop label="MAC">{result.mac}</Prop>
            <Prop label="Class">{result.class || '—'}</Prop>
            <Prop label="Firmware">{result.firmware || '—'}</Prop>
          </div>
          <Section title="Available" items={result.features} />
          {/* Not "missing": a check that was refused is a different state from
              a feature the hardware lacks, and only a wider ACL would change
              it — which is the operator's decision, not ours. */}
          <Section
            title="Could not be determined"
            items={result.unobservable}
            note="These checks were refused rather than answered. Nothing is
                  rendered from them; widening the ACL is the only thing that
                  would change that."
          />
          <Section
            title="Driver quirks"
            items={result.quirks}
            note="Fields that are present and wrong. Metrics derived from them
                  are not shown anywhere."
          />
          {result.notes && result.notes.length > 0 && (
            <Section title="Notes" items={result.notes} />
          )}
        </Card>
        <div>
          <Button onClick={() => setResult(null)}>Adopt another device</Button>
        </div>
      </div>
    )
  }

  return (
    <form onSubmit={submit} style={{ display: 'grid', gap: 14, maxWidth: 620 }}>
      {/* Above the form, not instead of it. Discovery cannot see the LAN from a
          bridged container, so add-by-address stays the path that always
          works — a scan that comes up empty must not look like a dead end. */}
      <Discover
        onPick={(h) => {
          setHost(h)
          setErr('')
          // Straight to the credential: the address came from the scan, so the
          // only thing still missing is the one thing a scan must never ask for.
          passwordRef.current?.focus()
        }}
      />
      <Card title="Adopt a device">
        <div style={{ display: 'grid', gap: 12 }}>
          <p style={{ fontSize: 12, color: 'var(--text-secondary)', margin: 0 }}>
            Enter the address of an OpenWrt device and its existing administrator
            login. The controller uses that login <strong>once</strong> — to
            install one ACL file and create its own scoped account — and never
            stores it. Removing the device later asks for it again, because a
            controller that could delete its own permissions could also widen
            them.
          </p>

          {err && <Banner tone="critical">{err}</Banner>}

          <Field
            label="Address"
            placeholder="192.168.1.1"
            value={host}
            autoFocus
            onChange={(e) => setHost(e.target.value)}
          />
          <Field
            label="Name (optional)"
            placeholder="taken from the device model if left blank"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          {/* The role decides what the controller will and will not send to
              this device, so it is asked at adoption rather than assumed. The
              screen had no field for it at all, which made a gateway
              impossible to adopt from the UI — and setting it through the API
              was worse, because an unrecognised value was stored verbatim and
              silently behaved as an access point. */}
          <label style={{ display: 'block' }}>
            <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 4 }}>
              Role
            </div>
            <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
              {ROLES.map((r) => (
                <button
                  key={r.value}
                  type="button"
                  title={r.describe}
                  onClick={() => setRole(r.value)}
                  style={{
                    fontSize: 12,
                    padding: '4px 10px',
                    borderRadius: 4,
                    cursor: 'pointer',
                    border: '1px solid var(--border-strong)',
                    background: role === r.value ? 'var(--accent-soft)' : 'transparent',
                    color: 'var(--text-primary)',
                  }}
                >
                  {r.value}
                </button>
              ))}
            </div>
            <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
              {ROLES.find((r) => r.value === role)?.describe}
            </div>
          </label>
          <label style={{ display: 'block' }}>
            <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 4 }}>
              Protocol
            </div>
            <div style={{ display: 'flex', gap: 6 }}>
              {(['http', 'https'] as const).map((s) => (
                <button
                  key={s}
                  type="button"
                  onClick={() => setScheme(s)}
                  style={{
                    fontSize: 12,
                    padding: '4px 10px',
                    borderRadius: 4,
                    cursor: 'pointer',
                    border: '1px solid var(--border-strong)',
                    background: scheme === s ? 'var(--accent-soft)' : 'transparent',
                    color: 'var(--text-primary)',
                  }}
                >
                  {s}
                </button>
              ))}
            </div>
          </label>
          <Field
            label="Device username"
            value={username}
            autoComplete="off"
            onChange={(e) => setUsername(e.target.value)}
          />
          <Field
            label="Device password"
            type="password"
            ref={passwordRef}
            value={password}
            autoComplete="off"
            onChange={(e) => setPassword(e.target.value)}
          />

          <Button type="submit" kind="primary" disabled={busy || !host || !username}>
            {busy ? 'Probing and adopting…' : 'Adopt'}
          </Button>
          {busy && (
            <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
              Probing capabilities, installing the ACL, creating the login, then
              verifying it works. A few seconds — the survey is deliberately
              sampled twice.
            </div>
          )}
        </div>
      </Card>
    </form>
  )
}

function Section({
  title,
  items,
  note,
}: {
  title: string
  items?: string[]
  note?: string
}) {
  if (!items || items.length === 0) return null
  return (
    <div style={{ marginTop: 12 }}>
      <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4 }}>{title}</div>
      <ul style={{ margin: 0, paddingLeft: 18, fontSize: 12, color: 'var(--text-secondary)' }}>
        {items.map((f) => (
          <li key={f}>{f}</li>
        ))}
      </ul>
      {note && (
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>{note}</div>
      )}
    </div>
  )
}
