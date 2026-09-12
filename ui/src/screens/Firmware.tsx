import { useEffect, useRef, useState } from 'react'
import { api, isDemo } from '../lib/api'
import type { SessionInfo } from '../lib/api'
import type { FirmwareInventory, FirmwareResult } from '../lib/firmware'
import { Banner, Button, Card, PageHeader, Status } from '../components/ui'
import { DeviceGlyph } from '../components/DeviceGlyph'
import './Firmware.css'

const stateNames: Record<FirmwareResult['state'], string> = {
  available: 'Maintenance update available', current: 'Current in this branch', ahead: 'Ahead of catalogue',
  unsupported: 'Manual review needed', error: 'Check unavailable',
}

export function officialFirmwareURL(value: string) {
  try {
    const url = new URL(value)
    return url.protocol === 'https:' && url.hostname === 'downloads.openwrt.org' && !url.username && !url.password && !url.port
      ? url.href : undefined
  } catch { return undefined }
}

export function Firmware({ session, embedded = false }: { session: SessionInfo; embedded?: boolean }) {
  const [inventory, setInventory] = useState<FirmwareInventory | null>(null)
  const [error, setError] = useState('')
  const [results, setResults] = useState<Record<number, FirmwareResult>>({})
  const [errors, setErrors] = useState<Record<number, string>>({})
  const [checking, setChecking] = useState<number | null>(null)
  const [revision, setRevision] = useState(0)
  const generation = useRef(0)
  const canCheck = !isDemo && (session.role === 'owner' || session.role === 'admin')
  useEffect(() => {
    const current = ++generation.current
    setInventory(null); setResults({}); setErrors({}); setError(''); setChecking(null)
    api.firmware().then((value) => { if (current === generation.current) setInventory(value) })
      .catch((cause) => { if (current === generation.current) setError(cause instanceof Error ? cause.message : String(cause)) })
    return () => { generation.current++ }
  }, [revision])
  async function check(id: number) {
    const current = generation.current
    setChecking(id)
    setResults((values) => { const next = { ...values }; delete next[id]; return next })
    setErrors((values) => ({ ...values, [id]: '' }))
    try {
      const response = await api.checkFirmware(id)
      if (response.device_id !== id) throw new Error('The controller returned a result for a different device')
      if (current === generation.current) setResults((values) => ({ ...values, [id]: response.result }))
    } catch (cause) {
      if (current === generation.current) setErrors((values) => ({ ...values, [id]: cause instanceof Error ? cause.message : String(cause) }))
    } finally { if (current === generation.current) setChecking(null) }
  }
  return <div className="firmware-page">
    {embedded
      ? <div className="page-header-actions"><Button disabled={checking != null} onClick={() => setRevision((value) => value + 1)}>Refresh inventory</Button></div>
      : <PageHeader title="Firmware" purpose="Know what your routers run and review official maintenance updates."
          actions={<Button disabled={checking != null} onClick={() => setRevision((value) => value + 1)}>Refresh inventory</Button>} />}
    {error && <div role="alert"><Banner tone="critical">Firmware inventory unavailable: {error}</Banner></div>}
    {!inventory && !error && <div role="status">Loading stored firmware identity…</div>}
    {inventory && <>
      <div className="firmware-intro"><div><span className="firmware-eyebrow">STOCK OPENWRT</span><h2>Your firmware. Your decision.</h2>
        <p>{inventory.checking.note}</p></div><span className="firmware-boundary">Catalogue checks only<br /><strong>No automatic upgrades</strong></span></div>
      {!canCheck && <p className="firmware-note">{isDemo ? 'This demo does not contact OpenWrt or a router.' : 'An Admin or Owner can run catalogue checks. You can review stored device identity.'}</p>}
      <div className="firmware-grid">
        {(inventory.devices ?? []).map((device) => {
          const result = results[device.device_id]
          return <article className="firmware-device" key={device.device_id}>
            <header><span className="firmware-device-icon"><DeviceGlyph kind="client" size={32} /></span><div><h2>{device.name || `Device ${device.device_id}`}</h2><p>{device.management_mode === 'monitor_only' ? 'Monitor only' : device.management_mode === 'managed' ? 'Managed' : device.management_mode}</p></div><Status value={device.status} /></header>
            <div className="firmware-version">{device.identity.release || 'Firmware not reported'}</div>
            <dl><dt>Board</dt><dd>{device.identity.board_name || 'Not established'}</dd><dt>Target</dt><dd>{device.identity.target || 'Not established'}</dd><dt>Filesystem</dt><dd>{device.identity.rootfs_type || 'Not established'}</dd></dl>
            <p className="firmware-note">Identity from the stored capability probe. Re-probe in Devices after a firmware change.</p>
            {canCheck && <Button onClick={() => void check(device.device_id)} disabled={checking != null}>{checking === device.device_id ? 'Checking catalogue…' : 'Check OpenWrt catalogue'}</Button>}
            {errors[device.device_id] && <p role="alert">Check failed: {errors[device.device_id]}. No current result is available.</p>}
            {result && <section className="firmware-result" aria-label={`Firmware result for ${device.name}`}>
              <strong>{stateNames[result.state] ?? 'Unrecognized catalogue response'}</strong>
              {result.latest_version && <p>Latest listed in branch: <b>{result.latest_version}</b></p>}
              <p>{result.message}</p><small>Checked {new Date(result.checked_at).toLocaleString()}</small>
              {result.image && <details><summary>Image metadata and limitations</summary>
                <p className="firmware-image-name">{result.image.name}</p><dl><dt>Size</dt><dd>{(result.image.size / 1048576).toFixed(1)} MiB</dd><dt>SHA-256</dt><dd><code>{result.image.sha256}</code></dd></dl>
                <p>This is published catalogue metadata, not verification of downloaded bytes or permission to flash.</p>
                {officialFirmwareURL(result.image.url) && <a href={officialFirmwareURL(result.image.url)} target="_blank" rel="noopener noreferrer">Open official image link ↗</a>}
                <ul>{(result.limitations ?? []).map((item) => <li key={item}>{item}</li>)}</ul>
              </details>}
            </section>}
          </article>
        })}
      </div>
      {inventory.devices.length === 0 && <Card title="No adopted devices yet"><p>Adopt a device to include its stored board and firmware identity here.</p></Card>}
      <Card title="Before any firmware installation">
        <p className="firmware-note">Firmware flashing replaces the operating system. The controller&apos;s UCI rollback timer cannot undo a firmware flash. Installation is not enabled in this build; these checks remain required.</p>
        <ol className="firmware-checklist">{inventory.installation.required_checks.map((check) => <li key={check}>{check}</li>)}</ol>
      </Card>
      <Card title="Optional router helper">
        <p className="firmware-note">{inventory.agent.note}</p><p>Source package: <code>{inventory.agent.package_path}</code></p>
        <p className="firmware-note">The helper is opt-in and read-only. It does not provide a firmware execution service. Normal agent-free adoption remains available.</p>
      </Card>
    </>}
  </div>
}
