import { useEffect, useRef, useState } from 'react'
import { api, isDemo } from '../lib/api'
import type { Device, SessionInfo } from '../lib/api'
import type { AdGuardConfig, AdGuardResult, WireGuardResult } from '../lib/integrations'
import { Banner, Button, Card, Field, PageHeader } from '../components/ui'
import './Integrations.css'

const bytes = (value: number | null) => {
  if (value == null || !Number.isFinite(value) || value < 0) return 'Unavailable'
  if (value < 1024) return `${value} B`
  if (value < 1048576) return `${(value / 1024).toFixed(1)} KiB`
  if (value < 1073741824) return `${(value / 1048576).toFixed(1)} MiB`
  return `${(value / 1073741824).toFixed(2)} GiB`
}
const flag = (value: boolean | null) => value === true ? 'Enabled' : value === false ? 'Disabled' : 'Unavailable'

export function Integrations({ devices, session, embedded = false }: { devices: Device[]; session: SessionInfo; embedded?: boolean }) {
  const owner = session.role === 'owner' && !isDemo
  const canCheck = !isDemo && (session.role === 'owner' || session.role === 'admin')
  const [config, setConfig] = useState<AdGuardConfig | null>(null)
  const [configError, setConfigError] = useState('')
  const [url, setURL] = useState('')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [fingerprint, setFingerprint] = useState('')
  const [replacePassword, setReplacePassword] = useState(false)
  const [controllerPassword, setControllerPassword] = useState('')
  const [editing, setEditing] = useState(false)
  const [removing, setRemoving] = useState(false)
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<AdGuardResult | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [revision, setRevision] = useState(0)
  const [deviceID, setDeviceID] = useState(0)
  const [wireguard, setWireguard] = useState<WireGuardResult | null>(null)
  const [wgError, setWGError] = useState('')
  const [wgBusy, setWGBusy] = useState(false)
  const generation = useRef(0)
  const wgGeneration = useRef(0)

  useEffect(() => {
    const current = ++generation.current
    setConfig(null); setConfigError(''); setResult(null)
    api.adguard().then((value) => { if (current === generation.current) setConfig(value) })
      .catch((cause) => { if (current === generation.current) setConfigError(cause instanceof Error ? cause.message : String(cause)) })
    return () => { generation.current++ }
  }, [revision])
  useEffect(() => {
    if (!devices.some((device) => device.id === deviceID)) setDeviceID(devices.find((device) => device.adopted)?.id ?? 0)
  }, [devices, deviceID])
  useEffect(() => {
    wgGeneration.current++; setWireguard(null); setWGError(''); setWGBusy(false)
    return () => { wgGeneration.current++ }
  }, [deviceID])
  function beginEdit(remove = false) {
    setURL(config?.url ?? ''); setUsername(config?.username ?? ''); setFingerprint(config?.tls_fingerprint ?? '')
    setPassword(''); setControllerPassword(''); setReplacePassword(false); setEditing(true); setRemoving(remove); setError(''); setNotice('')
  }
  async function save(event: React.FormEvent) {
    event.preventDefault()
    const current = ++generation.current
    setBusy(true); setError(''); setNotice(''); setResult(null)
    try {
      await api.reauthenticate(controllerPassword)
      if (removing) await api.deleteAdguard()
      else await api.saveAdguard({ url, username, tls_fingerprint: fingerprint,
        ...(!config?.configured || replacePassword ? { password } : {}) })
      if (current === generation.current) { setEditing(false); setNotice(removing ? 'AdGuard connection removed.' : 'AdGuard connection saved. No service request has been made.'); setRevision((value) => value + 1) }
    } catch (cause) { if (current === generation.current) setError(cause instanceof Error ? cause.message : String(cause)) }
    finally { setPassword(''); setControllerPassword(''); setBusy(false) }
  }
  async function checkAdGuard() {
    const current = ++generation.current
    setBusy(true); setError(''); setResult(null); setNotice('')
    try { const response = await api.checkAdguard(); if (current === generation.current) setResult(response) }
    catch (cause) { if (current === generation.current) setError(cause instanceof Error ? cause.message : String(cause)) }
    finally { if (current === generation.current) setBusy(false) }
  }
  async function checkWG() {
    const current = ++wgGeneration.current
    setWGBusy(true); setWGError(''); setWireguard(null)
    try {
      const response = await api.checkWireGuard(deviceID)
      if (response.device_id !== deviceID) throw new Error('The controller returned a different device')
      if (current === wgGeneration.current) setWireguard(response)
    } catch (cause) { if (current === wgGeneration.current) setWGError(cause instanceof Error ? cause.message : String(cause)) }
    finally { if (current === wgGeneration.current) setWGBusy(false) }
  }

  return <div className="integrations-page">
    {!embedded && <PageHeader title="Integrations" purpose="A clear view of adjacent services, with explicit connections and read-only checks." />}
    <div className="integrations-intro"><strong>Visibility without service changes</strong><p>These checks do not create VPN tunnels, change DNS protection, install packages, or alter your network. Nothing is polled automatically.</p></div>
    <div className="integration-grid">
      <Card title="AdGuard Home">
        <p className="integration-description">Review DNS protection and aggregate statistics from your own AdGuard Home service. Domain lists and client query logs are not collected.</p>
        {configError && <>
          <div role="alert"><Banner tone="critical">Connection settings unavailable: {configError}</Banner></div>
          <p className="integration-footnote">The controller could not read the saved settings. This does not mean the connection is unconfigured. Retry, or ask the Owner to remove the saved connection and set it up again.</p>
          <div className="integration-actions">
            <Button disabled={busy || editing} onClick={() => setRevision((value) => value + 1)}>Retry loading settings</Button>
            {owner && <Button disabled={busy || editing} onClick={() => beginEdit(true)}>Remove saved connection</Button>}
          </div>
        </>}
        {!config && !configError && <div role="status">Loading connection settings…</div>}
        {error && <p role="alert" className="integration-error">{error}</p>}
        {notice && <p role="status">{notice}</p>}
        {config && <>
          <div className="integration-source"><span>{config.configured ? 'Configured endpoint' : 'Not connected'}</span><strong>{config.configured ? config.url : 'Your DNS service, when you choose'}</strong></div>
          <div className="integration-actions">
            {canCheck && config.configured && <Button kind="primary" disabled={busy || editing} onClick={() => void checkAdGuard()}>{busy ? 'Checking…' : 'Check AdGuard'}</Button>}
            {owner && <Button disabled={busy} onClick={() => beginEdit()}>{config.configured ? 'Edit connection' : 'Connect AdGuard'}</Button>}
            {owner && config.configured && <Button disabled={busy} onClick={() => beginEdit(true)}>Remove connection</Button>}
            <Button disabled={busy || editing} onClick={() => setRevision((value) => value + 1)}>Refresh settings</Button>
          </div>
          {!canCheck && <p className="integration-footnote">{isDemo ? 'The demo never connects to an external DNS service.' : 'An Admin or Owner can run checks; only the Owner can change this connection.'}</p>}
        </>}
        {editing && owner && <form className="integration-form" onSubmit={(event) => void save(event)}>
          <h3>{removing ? 'Remove the saved connection?' : 'Connection settings'}</h3>
          {!removing && <fieldset disabled={busy}>
            <Field label="AdGuard HTTPS origin" type="url" required value={url} placeholder="https://dns.example:3000" onChange={(event) => setURL(event.target.value)} />
            <Field label="AdGuard username" value={username} autoComplete="off" onChange={(event) => setUsername(event.target.value)} />
            {config?.configured && <label className="integration-checkbox"><input type="checkbox" checked={replacePassword} onChange={(event) => { setReplacePassword(event.target.checked); setPassword('') }} />Replace the saved AdGuard password</label>}
            {(!config?.configured || replacePassword) && <Field label="AdGuard password" type="password" value={password} autoComplete="new-password" onChange={(event) => setPassword(event.target.value)} />}
            <Field label="TLS certificate SHA-256 pin (optional)" value={fingerprint} autoComplete="off" onChange={(event) => setFingerprint(event.target.value)} />
            <p className="integration-footnote">Use a trusted HTTPS origin without a path, query, or embedded credentials. A pin explicitly trusts one certificate; obtain it through a trusted channel. Blank uses normal certificate validation. Changing origin, username, or pin requires an explicit replacement password.</p>
          </fieldset>}
          <p className="integration-footnote">{removing ? 'This removes only the controller connection and saved credentials. AdGuard keeps running unchanged.' : 'Credentials are encrypted by the controller keyring. Saving does not test or change AdGuard. Confirm your controller identity to continue.'}</p>
          <Field label="Your controller password" type="password" required disabled={busy} value={controllerPassword} autoComplete="current-password" onChange={(event) => setControllerPassword(event.target.value)} />
          <div className="integration-actions"><Button type="submit" kind="primary" disabled={busy}>{busy ? 'Saving…' : removing ? 'Confirm removal' : 'Save connection'}</Button><Button disabled={busy} onClick={() => { setEditing(false); setPassword(''); setControllerPassword('') }}>Cancel</Button></div>
        </form>}
        {result && <section className="integration-result" aria-label="AdGuard observations">
          <h3>{result.state === 'observed' ? 'Service observations' : result.state === 'partial' ? 'Some observations unavailable' : 'Service unavailable'}</h3>
          <p className="integration-footnote">{result.source_url} · Checked {new Date(result.checked_at).toLocaleString()}{result.version && ` · ${result.version}`}</p>
          <div className="integration-metrics">
            <div><span>Service</span><strong>{result.running === true ? 'Running' : result.running === false ? 'Stopped' : 'Unavailable'}</strong></div>
            <div><span>Protection</span><strong>{flag(result.protection_enabled)}</strong></div>
            <div><span>DNS queries</span><strong>{result.dns_queries?.toLocaleString() ?? '—'}</strong></div>
            <div><span>Filtered queries</span><strong>{result.blocked_filtering?.toLocaleString() ?? '—'}</strong></div>
            <div><span>Average processing</span><strong>{result.avg_processing_ms == null ? '—' : `${result.avg_processing_ms.toFixed(2)} ms`}</strong></div>
          </div>
          <p className="integration-footnote">Counts cover the statistics window configured by AdGuard Home; they are not lifetime totals or a controller-selected reporting window.</p>
          {result.notes?.length > 0 && <ul className="integration-notes">{result.notes.map((note) => <li key={note}>{note}</li>)}</ul>}
        </section>}
      </Card>
      <Card title="WireGuard">
        <p className="integration-description">Read peer handshakes and transfer counters through the optional router helper. The existing agent-free controller does not need this helper.</p>
        <label className="integration-device-picker">Router<select value={deviceID || ''} onChange={(event) => setDeviceID(Number(event.target.value))}>
          <option value="" disabled>Select an adopted device</option>{devices.filter((device) => device.adopted).map((device) => <option key={device.id} value={device.id}>{device.name || device.host}</option>)}
        </select></label>
        {canCheck && <Button kind="primary" disabled={wgBusy || !deviceID} onClick={() => void checkWG()}>{wgBusy ? 'Checking WireGuard…' : 'Check WireGuard'}</Button>}
        <p className="integration-footnote">Manual helper installation and a separate scoped read grant are required. No private or pre-shared key is requested. Handshake age is not proof of end-to-end VPN connectivity.</p>
        {!canCheck && <p className="integration-footnote">{isDemo ? 'This demo does not contact a router.' : 'An Admin or Owner can request this read-only check.'}</p>}
        {wgError && <p role="alert" className="integration-error">WireGuard check unavailable: {wgError}</p>}
        {wireguard && <section className="integration-result" aria-label="WireGuard observations">
          <h3>{wireguard.state === 'observed' ? 'Peer observations' : wireguard.state === 'partial' ? 'Partial peer observations' : 'WireGuard unavailable'}</h3>
          <p className="integration-footnote">Checked {new Date(wireguard.checked_at).toLocaleString()}. Counters are cumulative, not rates.</p>
          {wireguard.state === 'observed' && wireguard.interfaces.length === 0 && <p>No WireGuard interfaces were reported.</p>}
          {wireguard.interfaces.map((iface) => <div className="wireguard-interface" key={iface.name}>
            <h4>{iface.name}</h4>{iface.peers.length === 0 && <p>No peers reported for this interface.</p>}
            {iface.peers.map((peer) => <article className="wireguard-peer" key={peer.public_key}>
              <code title={peer.public_key}>{peer.public_key.slice(0, 14)}…</code>
              <dl><dt>Last handshake</dt><dd>{peer.handshake_state === 'never' ? 'No handshake yet' : peer.last_handshake == null ? 'Unavailable' : new Date(peer.last_handshake).toLocaleString()}</dd>
                <dt>Received</dt><dd>{bytes(peer.rx_bytes)}</dd><dt>Sent</dt><dd>{bytes(peer.tx_bytes)}</dd></dl>
            </article>)}
          </div>)}
          {wireguard.notes?.length > 0 && <ul className="integration-notes">{wireguard.notes.map((note) => <li key={note}>{note}</li>)}</ul>}
        </section>}
      </Card>
    </div>
  </div>
}
