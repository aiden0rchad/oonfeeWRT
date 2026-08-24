import { useEffect, useRef, useState } from 'react'
import { api, ApiError } from '../lib/api'
import type {
  AdoptResult,
  DeviceFunction,
  Discovered,
  InspectResult,
} from '../lib/api'
import { Button, Field, TextAreaField, Banner, Card, Notice, Prop } from '../components/ui'
import type { DeviceRole } from '../lib/api'
import { Discover } from './Discover'

/** Kept in step with internal/model/role.go, which is where the rule lives:
 *  an unrecognised role is refused by the API rather than stored. */
const FUNCTIONS: {
  value: DeviceFunction
  label: string
  describe: string
}[] = [
  {
    value: 'gateway',
    label: 'Gateway',
    describe:
      'Manage routing, addressing, DHCP and firewall policy. Adopt this device first when it anchors the network.',
  },
  {
    value: 'ap',
    label: 'Access point',
    describe:
      'Manage radios and publish WLANs. This can be combined with Gateway and Switch.',
  },
  {
    value: 'switch',
    label: 'Switch',
    describe:
      'Record wired switching responsibility and visibility. Per-port or managed-VLAN control depends on the hardware and its existing bridge mode.',
  },
]

function primaryRole(functions: DeviceFunction[]): DeviceRole {
  if (functions.includes('gateway')) return 'gateway'
  if (functions.includes('ap')) return 'ap'
  return 'switch'
}

function discoveredFunctions(d: Discovered): DeviceFunction[] {
  const found: DeviceFunction[] = []
  if (d.signals.wireless || d.signals.radios > 0) found.push('ap')
  return found
}

/**
 * Bring a device under management.
 *
 * The form asks for the ROUTER's existing administrator credential, which the
 * controller uses only for read-only inspection and the one-time adoption
 * transaction, and never stores. Adoption creates its own scoped login and
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
  const [privateKey, setPrivateKey] = useState('')
  const [scheme, setScheme] = useState<'http' | 'https'>('http')
  // Manual entry defaults to the least invasive function. Discovery may add
  // only functions its pre-auth signals prove; switch port topology is not
  // visible until the credentialed probe, so Switch is never guessed here.
  const [functions, setFunctions] = useState<DeviceFunction[]>(['ap'])
  const [recommended, setRecommended] = useState<DeviceFunction[]>([])
  const [possibleGateway, setPossibleGateway] = useState(false)
  const [hasAdoptedDevice, setHasAdoptedDevice] = useState<boolean | null>(null)
  const [inspection, setInspection] = useState<InspectResult | null>(null)
  const [inspectBusy, setInspectBusy] = useState(false)
  const [inspectErr, setInspectErr] = useState('')
  const [payloadReviewOpen, setPayloadReviewOpen] = useState(false)
  const [routerChangesAccepted, setRouterChangesAccepted] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [result, setResult] = useState<AdoptResult | null>(null)
  const passwordRef = useRef<HTMLInputElement>(null)
  const inspectGeneration = useRef(0)

  useEffect(() => {
    let current = true
    api.devices()
      .then(({ devices }) => {
        if (current) setHasAdoptedDevice(devices.some((d) => d.adopted))
      })
      // The server repeats this validation authoritatively. A failed roster
      // read must not turn the whole form into a dead end.
      .catch(() => {
        if (current) setHasAdoptedDevice(null)
      })
    return () => {
      current = false
    }
  }, [])

  useEffect(() => () => {
    inspectGeneration.current++
  }, [])

  function clearInspection() {
    inspectGeneration.current++
    setInspection(null)
    setInspectErr('')
    setInspectBusy(false)
    setRecommended([])
  }

  function toggleFunction(value: DeviceFunction) {
    setFunctions((current) =>
      current.includes(value)
        ? current.filter((f) => f !== value)
        : FUNCTIONS.map((f) => f.value).filter(
            (f) => f === value || current.includes(f),
          ),
    )
  }

  function pickDiscovered(h: string, candidate?: Discovered) {
    setHost(h)
    setRouterChangesAccepted(false)
    setErr('')
    clearInspection()
    setPossibleGateway(false)
    if (candidate) {
      const next = discoveredFunctions(candidate)
      setRecommended(next)
      // A WAN-named object or dnsmasq is a hint, not proof: an AP may retain
      // both while bridged. The credentialed inspection decides Gateway.
      setPossibleGateway(candidate.signals.gateway || candidate.signals.dhcp)
      if (next.length > 0) setFunctions(next)
    }
    passwordRef.current?.focus()
  }

  async function inspect() {
    const generation = ++inspectGeneration.current
    const request = { host, username, password, scheme }
    setInspectErr('')
    setInspectBusy(true)
    try {
      const found = await api.inspectDevice(request)
      if (generation !== inspectGeneration.current) return
      setInspection(found)
      setRecommended(found.functions_recommended ?? [])
      if (found.functions_recommended?.length > 0) {
        setFunctions(found.functions_recommended)
      }
    } catch (e) {
      if (generation !== inspectGeneration.current) return
      const detail = e instanceof ApiError ? e.message : String(e)
      setInspectErr(
        `Inspection did not complete: ${detail}. You can still adopt directly; adoption performs the full probe after creating its scoped access.`,
      )
    } finally {
      if (generation === inspectGeneration.current) setInspectBusy(false)
    }
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setErr('')
    if (!routerChangesAccepted) {
      setErr('Confirm the required, opt-in oonfeeWRT controller capability installation before adopting this device.')
      return
    }
    inspectGeneration.current++
    setInspectBusy(false)
    setBusy(true)
    try {
      const role = primaryRole(functions)
      const res = await api.adopt({
        host,
        name,
        username,
        password,
        ...(privateKey ? { private_key: privateKey } : {}),
        scheme,
        functions,
        // Compatibility fallback for older rows/clients. The backend uses the
        // same precedence when it emits a single primary role.
        role,
        acknowledge_router_changes: true,
      })
      setResult(res)
      // The administrator credentials are gone from this component the moment
      // they are not needed. Neither is sent back by the API or persisted.
      setPassword('')
      setPrivateKey('')
      setRouterChangesAccepted(false)
      setHasAdoptedDevice(true)
      onAdopted()
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  function adoptAnother() {
    clearInspection()
    setHost('')
    setName('')
    setUsername('root')
    setPassword('')
    setPrivateKey('')
    setScheme('http')
    setFunctions(['ap'])
    setPossibleGateway(false)
    setPayloadReviewOpen(false)
    setRouterChangesAccepted(false)
    setErr('')
    setResult(null)
  }

  if (result) {
    return (
      <div style={{ display: 'grid', gap: 14, maxWidth: 620 }}>
        <h1 style={{ margin: 0, fontSize: 20 }}>Adopt a device</h1>
        <Banner tone="accent">
          <strong>{result.name}</strong> is now managed. The controller created
          its own scoped login and installed the oonfeeWRT controller
          capability; the password and private key you supplied were not stored.
        </Banner>
        {result.warnings?.map((w) => (
          <div key={w} role="alert">
            <Banner tone="critical">{w}</Banner>
          </div>
        ))}
        <Card title="What the capability probe found">
          <div style={{ display: 'grid', gap: 6 }}>
            <Prop label="Model">{result.model || '—'}</Prop>
            <Prop label="MAC">{result.mac}</Prop>
            <Prop label="Class">{result.class || '—'}</Prop>
            <Prop label="Firmware">{result.firmware || '—'}</Prop>
            {result.functions && result.functions.length > 0 && (
              <Prop label="Functions">{functionNames(result.functions)}</Prop>
            )}
          </div>
          <Section title="Available" items={result.features} />
          {/* Not "missing": permission, inactive-interface, idle-counter, and
              driver-uncertainty outcomes all mean the probe could not prove a
              result. Each note carries the actual cause and remediation. */}
          <Section
            title="Could not be determined"
            items={result.unobservable}
            note="These checks did not produce enough evidence. That can mean
                  unavailable permission, an inactive interface, idle counters,
                  or hardware/driver uncertainty. Nothing is inferred or
                  rendered; widen access only when the corresponding note names
                  a permission denial."
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
          <Button onClick={adoptAnother}>Adopt another device</Button>
        </div>
      </div>
    )
  }

  return (
    <form onSubmit={submit} style={{ display: 'grid', gap: 14, maxWidth: 620 }}>
      <h1 style={{ margin: 0, fontSize: 20 }}>Adopt a device</h1>
      {/* Above the form, not instead of it. Discovery cannot see the LAN from a
          bridged container, so add-by-address stays the path that always
          works — a scan that comes up empty must not look like a dead end. */}
      <Discover
        onPick={pickDiscovered}
      />
      <Card title="Adopt a device">
        <div style={{ display: 'grid', gap: 12 }}>
          <p style={{ fontSize: 12, color: 'var(--text-secondary)', margin: 0 }}>
            Enter the OpenWrt device address and its existing administrator login.
            The controller uses these credentials only for inspection and this
            adoption attempt; it never stores them.
          </p>

          {err && <div role="alert"><Banner tone="critical">{err}</Banner></div>}

          <Field
            label="Address"
            placeholder="192.168.1.1"
            value={host}
            autoFocus
            onChange={(e) => {
              setHost(e.target.value)
              setRouterChangesAccepted(false)
              clearInspection()
              setPossibleGateway(false)
            }}
          />
          <Field
            label="Name (optional)"
            placeholder="taken from the device model if left blank"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <div style={{ display: 'block' }}>
            <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 4 }}>
              Protocol
            </div>
            <div role="group" aria-label="Protocol" style={{ display: 'flex', gap: 6 }}>
              {(['http', 'https'] as const).map((s) => (
                <button
                  key={s}
                  type="button"
                  aria-pressed={scheme === s}
                  onClick={() => {
                    setScheme(s)
                    clearInspection()
                  }}
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
          </div>
          <Field
            label="Device username"
            value={username}
            autoComplete="off"
            onChange={(e) => {
              setUsername(e.target.value)
              clearInspection()
            }}
          />
          <Field
            label="Device password (for ubus)"
            type="password"
            ref={passwordRef}
            value={password}
            autoComplete="off"
            onChange={(e) => {
              setPassword(e.target.value)
              clearInspection()
            }}
          />
          <div>
            <Button
              type="button"
              onClick={inspect}
              disabled={inspectBusy || busy || !host || !username}
            >
              {inspectBusy ? 'Inspecting…' : 'Inspect capabilities'}
            </Button>
            <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 5 }}>
              Recommended before adoption. Uses read-only ubus calls and creates
              no account or configuration on the router.
            </div>
          </div>
          {inspectErr && <Banner>{inspectErr}</Banner>}
          {inspection && <Inspection result={inspection} />}

          {hasAdoptedDevice === false && (
            <Banner
              tone={
                inspection?.functions_recommended.includes('gateway') ||
                (!inspection && possibleGateway)
                  ? 'accent'
                  : 'warning'
              }
            >
              <strong>
                {inspection?.functions_recommended.includes('gateway')
                  ? 'Gateway confirmed — adopt it first.'
                  : !inspection && possibleGateway
                    ? 'Possible gateway found — inspect it first.'
                    : 'Starting a new device ecosystem?'}
              </strong>{' '}
              {inspection?.functions_recommended.includes('gateway')
                ? 'The authenticated probe measured its routing role. Make it the routing anchor for devices adopted afterward.'
                : !inspection && possibleGateway
                  ? 'The unauthenticated scan saw a WAN-named interface or DHCP service. If inspection confirms Gateway, adopt it before the devices behind it.'
                  : 'Adopt the router that provides DHCP and routing first and select Gateway. AP-only is still valid when the gateway is intentionally managed elsewhere.'}
            </Banner>
          )}
          <fieldset
            style={{
              border: '1px solid var(--border-strong)',
              borderRadius: 6,
              padding: '10px 12px 12px',
              margin: 0,
            }}
          >
            <legend style={{ fontSize: 12, color: 'var(--text-secondary)', padding: '0 4px' }}>
              Device functions
            </legend>
            <div style={{ display: 'grid', gap: 10 }}>
              {FUNCTIONS.map((item) => {
                const isRecommended = recommended.includes(item.value)
                const isAvailable = inspection?.functions_supported.includes(item.value)
                const isUnknown = inspection?.functions_unknown?.includes(item.value)
                return (
                  <label
                    key={item.value}
                    style={{
                      display: 'grid',
                      gridTemplateColumns: 'auto 1fr',
                      columnGap: 8,
                      alignItems: 'start',
                      cursor: 'pointer',
                    }}
                  >
                    <input
                      type="checkbox"
                      checked={functions.includes(item.value)}
                      onChange={() => toggleFunction(item.value)}
                      style={{ marginTop: 2 }}
                    />
                    <span>
                      <span style={{ fontSize: 12, fontWeight: 600 }}>
                        {item.label}
                        {isRecommended && (
                          <span style={{ color: 'var(--accent-text)', fontWeight: 500 }}>
                            {' '}· recommended
                          </span>
                        )}
                        {!isRecommended && !isUnknown && isAvailable && (
                          <span style={{ color: 'var(--accent-text)', fontWeight: 500 }}>
                            {' '}· available
                          </span>
                        )}
                        {!isRecommended && isUnknown && (
                          <span style={{ color: 'var(--text-muted)', fontWeight: 500 }}>
                            {' '}· not observable
                          </span>
                        )}
                      </span>
                      <span
                        style={{
                          display: 'block',
                          fontSize: 11,
                          color: 'var(--text-muted)',
                          marginTop: 2,
                          lineHeight: 1.4,
                        }}
                      >
                        {item.describe}
                      </span>
                    </span>
                  </label>
                )
              })}
            </div>
            <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 10 }}>
              Select every function this device should perform. Switch records
              wired responsibility and visibility; it does not promise per-port
              configuration. Unknown evidence is never treated as absent.
            </div>
          </fieldset>
          <TextAreaField
            label="SSH private key (optional)"
            value={privateKey}
            autoComplete="off"
            spellCheck={false}
            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
            onChange={(e) => setPrivateKey(e.target.value)}
          />
          <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: -6 }}>
            The ubus sign-in still uses the password; the SSH key does not
            replace it. Leave the key blank when Dropbear accepts password
            authentication, including a passwordless lab router. Supply it when
            SSH password authentication is disabled. The key is used only for
            the one-time SSH bootstrap. Neither credential is stored.
          </div>

          <Notice
            tone="warning"
            component="Optional controller access payload"
            summary="Adoption adds one scoped rpcd ACL file and login. It installs no package, binary, daemon, service, or firmware."
            defaultOpen={payloadReviewOpen}
            closedLabel="What adoption installs and rolls back"
            openLabel="Hide exact router changes"
            actions={(
              <Button
                aria-pressed={payloadReviewOpen}
                onClick={() => setPayloadReviewOpen((current) => !current)}
              >
                {payloadReviewOpen ? 'Close payload review' : 'Review exact router changes'}
              </Button>
            )}
            details={(
              <>
                <strong>Exact adoption changes</strong>
                <ul style={{ margin: '6px 0 0', paddingLeft: 20, lineHeight: 1.5 }}>
                  <li>
                    Writes <code>/usr/share/rpcd/acl.d/oonfeewrt.json</code> and creates
                    the scoped <code>rpcd.oonfeewrt</code> login.
                  </li>
                  <li>
                    Grants read access for supported inventory, topology, radio/scan,
                    OpenWrt log, and fixed-target <code>1.1.1.1</code> ICMP observations.
                  </li>
                  <li>
                    Grants writes only for controller-owned network, wireless, firewall,
                    and DHCP sections after a separate Preview and acknowledged Apply.
                  </li>
                  <li>
                    Allows runtime 802.11k neighbour-list updates on managed WLANs that
                    request them. It cannot disconnect or steer clients.
                  </li>
                  <li>
                    Adoption itself does not change network, WLAN, firewall, or DHCP
                    settings. Those changes require Preview and Apply later.
                  </li>
                </ul>
                <p style={{ margin: '8px 0 0' }}>
                  Rollback asks for the device administrator login again, removes only
                  this ACL file and scoped login, and leaves controller-managed network
                  configuration for a separately reviewed rollback.
                </p>
              </>
            )}
          />

          <label
            style={{
              display: 'grid',
              gridTemplateColumns: 'auto 1fr',
              gap: 8,
              alignItems: 'start',
              fontSize: 12,
              lineHeight: 1.45,
              cursor: 'pointer',
            }}
          >
            <input
              type="checkbox"
              checked={routerChangesAccepted}
              onChange={(e) => setRouterChangesAccepted(e.target.checked)}
              style={{ marginTop: 2 }}
            />
            <span>
              <strong>Install the oonfeeWRT controller access payload?</strong>{' '}
              Leaving this acknowledgement unchecked or cancelling leaves the router
              unchanged and keeps Adopt unavailable.
            </span>
          </label>

          <Button
            type="submit"
            kind="primary"
            disabled={
              busy || !host || !username || functions.length === 0 ||
              !routerChangesAccepted
            }
          >
            {busy ? 'Probing and adopting…' : 'Adopt'}
          </Button>
          {functions.length === 0 && (
            <div role="alert" style={{ fontSize: 11, color: 'var(--critical)' }}>
              Select at least one device function.
            </div>
          )}
          {busy && (
            <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
              Installing and verifying the controller capability:
              writing the rpcd ACL JSON file, creating the scoped login, then
              probing capabilities. A few seconds — the survey is deliberately
              sampled twice.
            </div>
          )}
        </div>
      </Card>
    </form>
  )
}

function Inspection({ result }: { result: InspectResult }) {
  return (
    <div
      role="status"
      aria-live="polite"
      style={{
        border: '1px solid var(--accent)',
        borderRadius: 6,
        padding: 10,
        display: 'grid',
        gap: 6,
        background: 'var(--surface-2)',
      }}
    >
      <strong style={{ fontSize: 12 }}>Read-only inspection complete</strong>
      <Prop label="Model">{result.model || '—'}</Prop>
      <Prop label="Firmware">{result.firmware || '—'}</Prop>
      <Prop label="Radios">
        {result.radio_count ?? 'Unknown — radio inventory was not observable'}
      </Prop>
      <Prop label="LAN ports observed">
        {result.lan_ports.length > 0
          ? `${result.lan_ports.length} observed: ${result.lan_ports.join(', ')}`
          : 'None observed'}
      </Prop>
      <Prop label="WAN port observed">{result.wan_port || 'None'}</Prop>
      <Prop label="Active WAN default route">
        {evidenceText(
          result.gateway_evidence.active_wan_default_route,
          'Observed — Gateway recommendation evidence',
          'Not observed',
        )}
      </Prop>
      <Prop label="LAN DHCP server">
        {evidenceText(
          result.gateway_evidence.lan_dhcp_enabled,
          'Enabled — Gateway recommendation evidence',
          'Not enabled',
        )}
      </Prop>
      <Prop label="Switch capability">{switchModeText(result.switch_mode)}</Prop>
      <Prop label="Recommended">
        {functionNames(result.functions_recommended) || 'None automatically'}
      </Prop>
      {result.functions_unknown && result.functions_unknown.length > 0 && (
        <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          Could not determine: {functionNames(result.functions_unknown)}. You can
          still select those functions when you know the device should provide them.
        </div>
      )}
      {result.notes?.map((note) => (
        <div key={note} style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          {note}
        </div>
      ))}
      {result.unobservable && result.unobservable.length > 0 && (
        <details style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          <summary>Inspection limits</summary>
          <ul style={{ margin: '5px 0 0', paddingLeft: 18 }}>
            {result.unobservable.map((item) => <li key={item}>{item}</li>)}
          </ul>
        </details>
      )}
    </div>
  )
}

function functionNames(functions: DeviceFunction[]): string {
  return functions
    .map((value) => FUNCTIONS.find((item) => item.value === value)?.label ?? value)
    .join(', ')
}

function evidenceText(value: boolean | null, yes: string, no: string): string {
  if (value === true) return yes
  if (value === false) return no
  return 'Unknown — inspection could not determine this'
}

function switchModeText(mode: InspectResult['switch_mode']): string {
  switch (mode) {
    case 'dsa-conditional':
      return 'DSA detected — managed VLAN carriage requires an existing VLAN-aware LAN bridge'
    case 'observe-only':
      return 'Observe only — port/FDB telemetry; no per-port or managed-VLAN configuration'
    case 'none':
      return 'No switch capability observed'
    default:
      return 'Unknown — switching evidence could not be determined'
  }
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
