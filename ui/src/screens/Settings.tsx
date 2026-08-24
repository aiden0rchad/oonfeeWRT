import { useCallback, useEffect, useRef, useState } from 'react'
import type React from 'react'
import { ApiError, api } from '../lib/api'
import type {
  APGroup,
  ApplyOperation,
  ApplyResult,
  Device,
  DeviceFunction,
  Mesh,
  MeshHealthResult,
  Uplink,
  MeshLink,
  NeighbourDevice,
  NeighbourResult,
  PreviewResult,
  SessionInfo,
  Site,
  SiteNetwork,
  WLAN,
} from '../lib/api'
import {
  Banner, Button, Card, DataGrid, Field, Notice, Prop, SlideOver, Toggle, Unknown,
} from '../components/ui'
import { ago } from '../components/Chart'
import { Account } from './Account'
import { Accounts } from './Accounts'
import { Diagnostics } from './Diagnostics'
import { Backups } from './Backups'

const applyOperationStorageKey = 'oonfee_last_apply_operation'
const applyOperationPollMs = 1000

function retainedApplyOperationID(): string {
  try {
    return localStorage.getItem(applyOperationStorageKey) ?? ''
  } catch {
    return ''
  }
}

function retainApplyOperationID(id: string) {
  try {
    localStorage.setItem(applyOperationStorageKey, id)
  } catch {
    // The durable server record is authoritative. Storage-disabled browsers
    // can still recover for as long as this screen remains open.
  }
}

function forgetApplyOperationID() {
  try {
    localStorage.removeItem(applyOperationStorageKey)
  } catch {
    // Recovery remains usable without browser storage.
  }
}

function newApplyOperationID(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 0x0f) | 0x40
  bytes[8] = (bytes[8] & 0x3f) | 0x80
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

function applyResultMessage(res: ApplyResult): string {
  if (res.aborted) {
    return `Stopped after ${res.aborted_after}: ${
        res.devices.find((d) => d.outcome !== 'applied')?.reason ?? 'apply failed'
      }`
  }
  const applied = res.devices.filter((d) => d.outcome === 'applied')
  const changed = applied.filter((d) => (d.changes ?? 0) > 0)
  const changes = changed.reduce((sum, d) => sum + (d.changes ?? 0), 0)
  const matched = applied.length - changed.length
  const parts: string[] = []
  if (changes > 0) {
    parts.push(`${changes} change${changes === 1 ? '' : 's'} applied to ${changed.length} device${changed.length === 1 ? '' : 's'}`)
  }
  if (matched > 0) {
    parts.push(`${matched} device${matched === 1 ? '' : 's'} already matched`)
  }
  return `${parts.join('; ') || 'No device changes were applied'}.`
}

function applyWriteSummary(
  operationID: string,
  operation: ApplyOperation | null,
  recovering: boolean,
): string {
  if (!operationID) {
    return 'Nothing above has touched a device. Preview reads each one and reports what would change.'
  }
  if (operation && (operation.state === 'queued' || operation.state === 'running')) {
    return `Apply operation ${operationID} is ${operation.state}. The request was accepted and its durable status above is authoritative; do not submit another Apply.`
  }
  if (recovering || !operation) {
    return `Checking the durable status of Apply operation ${operationID}. Its recorded status is authoritative; do not submit another Apply while recovery is in progress.`
  }

  const writeState = operation.write_state ?? 'none'
  const result = operation.result ? ' The recorded result above is authoritative.' : ''
  if (writeState === 'none') {
    return `Previous Apply operation ${operationID} is ${operation.state}. Durable write state: none — no device write began.${result}`
  }
  return `Previous Apply operation ${operationID} is ${operation.state}. Durable write state: possible — a router write may have started; use the recorded device outcomes above.${result}`
}

type SettingsTab = 'network' | 'account' | 'accounts' | 'diagnostics' | 'backups'

export function Settings({
  devices,
  devicesLoaded = true,
  devicesError = '',
  session,
  onSessionChange,
  onCurrentSessionRevoked,
}: {
  devices: Device[]
  devicesLoaded?: boolean
  devicesError?: string
  session?: SessionInfo
  onSessionChange?: (session: SessionInfo) => void
  onCurrentSessionRevoked?: () => void
}) {
  const [tab, setTab] = useState<SettingsTab>('network')
  const accountTabs = Boolean(session)
  const accountsTab = session?.role === 'owner'
  const diagnosticsTab = session?.role === 'owner' || session?.role === 'admin'
  const backupsTab = session?.role === 'owner'

  useEffect(() => {
    if ((!accountTabs && tab === 'account') || (!accountsTab && tab === 'accounts') ||
      (!diagnosticsTab && tab === 'diagnostics') || (!backupsTab && tab === 'backups')) {
      setTab('network')
    }
  }, [accountTabs, accountsTab, backupsTab, diagnosticsTab, tab])

  const tabs: { id: SettingsTab; label: string }[] = [
    { id: 'network', label: 'Network' },
    ...(accountTabs ? [{ id: 'account' as const, label: 'My account' }] : []),
    ...(accountsTab ? [{ id: 'accounts' as const, label: 'Accounts' }] : []),
    ...(diagnosticsTab ? [{ id: 'diagnostics' as const, label: 'Diagnostics' }] : []),
    ...(backupsTab ? [{ id: 'backups' as const, label: 'Backup & Restore' }] : []),
  ]

  return <div className="settings-page">
    <div className="settings-heading">
      <h1>Settings</h1>
      <span>{tab === 'network'
        ? 'Desired network state and controller operations.'
        : tab === 'diagnostics'
          ? 'Redacted, stored-only support bundles.'
          : tab === 'backups'
            ? 'Encrypted controller backup, preview, and restore.'
          : 'Controller-local identity and access.'}</span>
    </div>
    <div className="settings-tabs" role="tablist" aria-label="Settings sections">
      {tabs.map((item) => <button
        key={item.id}
        id={`settings-tab-${item.id}`}
        type="button"
        role="tab"
        aria-selected={tab === item.id}
        aria-controls={`settings-panel-${item.id}`}
        tabIndex={tab === item.id ? 0 : -1}
        onClick={() => setTab(item.id)}
        onKeyDown={(event) => {
          const index = tabs.findIndex((candidate) => candidate.id === item.id)
          let next = index
          if (event.key === 'ArrowRight') next = (index + 1) % tabs.length
          else if (event.key === 'ArrowLeft') next = (index - 1 + tabs.length) % tabs.length
          else if (event.key === 'Home') next = 0
          else if (event.key === 'End') next = tabs.length - 1
          else return
          event.preventDefault()
          setTab(tabs[next].id)
          requestAnimationFrame(() => document.getElementById(`settings-tab-${tabs[next].id}`)?.focus())
        }}
      >
        {item.label}
      </button>)}
    </div>
    <div
      id={`settings-panel-${tab}`}
      role="tabpanel"
      aria-labelledby={`settings-tab-${tab}`}
      className="settings-panel"
    >
      {tab === 'network' && (devicesLoaded
        ? <NetworkSettings devices={devices} />
        : devicesError
          ? <div role="alert"><Banner tone="critical">
              Device inventory is unavailable: {devicesError}. My account remains available above.
            </Banner></div>
          : <div role="status">Loading device inventory…</div>)}
      {tab === 'account' && session && onCurrentSessionRevoked && (
        <Account session={session} onCurrentSessionRevoked={onCurrentSessionRevoked} />
      )}
      {tab === 'accounts' && session?.role === 'owner' && onSessionChange && onCurrentSessionRevoked && (
        <Accounts
          session={session}
          onSessionChange={onSessionChange}
          onCurrentSessionRevoked={onCurrentSessionRevoked}
        />
      )}
      {tab === 'diagnostics' && diagnosticsTab && <Diagnostics />}
      {tab === 'backups' && backupsTab && session && <Backups session={session} />}
    </div>
  </div>
}

/**
 * Settings — the site model, and the flow that pushes it to hardware.
 *
 * The shape of this screen IS Phase 2's idea. Editing a WLAN changes nothing on
 * any device: it writes desired state. What reaches hardware is an explicit
 * apply, and the only path to it runs through a preview that says, per device,
 * exactly which UCI sections would be created, updated or removed.
 *
 * That is the difference between this and LuCI. LuCI edits one device's config
 * directly and you find out what it did afterwards; here one edit fans out
 * across every AP in a group and across every band, and you read the whole
 * consequence before any of it happens.
 */
function NetworkSettings({ devices }: { devices: Device[] }) {
  const [site, setSite] = useState<Site | null>(null)
  const [editing, setEditing] = useState<Partial<WLAN> | null>(null)
  const [editingMesh, setEditingMesh] = useState<Partial<Mesh> | null>(null)
  const [preview, setPreview] = useState<PreviewResult | null>(null)
  const previewGeneration = useRef(0)
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')
  const [applied, setApplied] = useState<string | null>(null)
  const [ackTraversal, setAckTraversal] = useState(false)
  const [ackFatal, setAckFatal] = useState(false)
  const [ackCautions, setAckCautions] = useState(false)
  const [operationID, setOperationID] = useState(retainedApplyOperationID)
  const [operation, setOperation] = useState<ApplyOperation | null>(null)
  const [recoveringOperation, setRecoveringOperation] = useState(
    () => retainedApplyOperationID() !== '',
  )

  const load = useCallback(async () => {
    try {
      setSite(await api.site())
      setErr('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [])

  // Any desired-state write invalidates the whole fleet preview. Apply must
  // never remain enabled beside a plan computed before a network, DHCP, WLAN,
  // mesh, group, uplink, override, or zone edit.
  const modelChanged = useCallback(async () => {
    previewGeneration.current += 1
    setPreview(null)
    setBusy((current) => current === 'preview' ? '' : current)
    setApplied(null)
    setAckTraversal(false)
    setAckFatal(false)
    setAckCautions(false)
    await load()
  }, [load])

  useEffect(() => {
    load()
  }, [load])

  useEffect(() => {
    if (!recoveringOperation || !operationID) return
    let stopped = false
    let timer = 0
    const poll = async () => {
      setBusy('recover')
      try {
        const found = await api.applyOperation(operationID)
        if (stopped) return
        setOperation(found)
        if (found.state === 'queued' || found.state === 'running') {
          timer = window.setTimeout(poll, applyOperationPollMs)
          return
        }
        if (found.state !== 'unknown') forgetApplyOperationID()
        setRecoveringOperation(false)
        setBusy('')
        setPreview(null)
        setAckTraversal(false)
        setAckFatal(false)
        setAckCautions(false)
        if (found.result) {
          setApplied(`Previous result: ${applyResultMessage(found.result)}`)
        } else {
          setApplied(null)
        }
        if (found.state === 'unknown') {
          setErr(found.write_state === 'none'
            ? `${found.error ?? 'This Apply was interrupted.'} No device write began. Preview again before applying.`
            : `${found.error ?? 'The controller restarted before this Apply finished.'} ` +
              `The outcome of operation ${operationID} is unknown; inspect the devices and audit log, then Preview again.`)
        } else if (found.error) {
          setErr(found.error)
        } else {
          setErr('')
        }
      } catch (e) {
        if (stopped) return
        if (e instanceof ApiError && e.status === 404) {
          forgetApplyOperationID()
          setOperationID('')
          setOperation(null)
          setRecoveringOperation(false)
          setBusy('')
          setErr('')
          setApplied('Cleared a saved Apply reference that does not exist on this controller.')
          return
        }
        const message = e instanceof Error ? e.message : String(e)
        setRecoveringOperation(false)
        setBusy('')
        setErr(
          `Could not recover Apply operation ${operationID}: ${message}. ` +
          'Keep this operation ID and use Check status before retrying.',
        )
      }
    }
    void poll()
    return () => {
      stopped = true
      window.clearTimeout(timer)
    }
  }, [operationID, recoveringOperation])

  async function runPreview() {
    const generation = ++previewGeneration.current
    setBusy('preview')
    setPreview(null)
    setApplied(null)
    // Every preview re-earns both acknowledgements.
    //
    // They used to persist. Tick "apply the network changes", edit the site,
    // preview again, and Apply was enabled for a DIFFERENT set of changes that
    // nobody had acknowledged — consent to one plan silently carried to the
    // next. The screen is careful that a stale preview never sits beside an
    // enabled Apply; a stale acknowledgement is the same defect one level down.
    setAckTraversal(false)
    setAckFatal(false)
    setAckCautions(false)
    try {
      const next = await api.preview()
      if (generation !== previewGeneration.current) return
      setPreview(next)
      setErr('')
    } catch (e) {
      if (generation !== previewGeneration.current) return
      setPreview(null)
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      if (generation === previewGeneration.current) {
        setBusy((current) => current === 'preview' ? '' : current)
      }
    }
  }

  async function runApply() {
    if (!preview) return
    setBusy('apply')
    const operationID = newApplyOperationID()
    const createdAt = Math.floor(Date.now() / 1000)
    retainApplyOperationID(operationID)
    setOperationID(operationID)
    setOperation({
      operation_id: operationID, state: 'queued', created_at: createdAt, devices: [],
    })
    setRecoveringOperation(false)
    let resultMessage = ''
    try {
      const res = await api.applySite({
        operation_id: operationID,
        preview_token: preview.preview_token,
        acknowledge_traversal: ackTraversal,
        acknowledge_driver_risk: ackFatal,
        acknowledge_cautions: ackCautions,
      })
      resultMessage = applyResultMessage(res)
      setOperation({
        operation_id: res.operation_id ?? operationID,
        state: res.aborted ? 'failed' : 'completed',
        created_at: createdAt,
        finished_at: Math.floor(Date.now() / 1000),
        result: res,
        write_state: res.devices.some((d) => (d.changes ?? 0) > 0) ? 'possible' : 'none',
        devices: [],
      })
      forgetApplyOperationID()
      setApplied(resultMessage)
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e)
      previewGeneration.current += 1
      setPreview(null)
      setAckTraversal(false)
      setAckFatal(false)
      setAckCautions(false)
      setApplied(null)
      if (e instanceof ApiError && e.writeState === 'none') {
        setOperation({
          operation_id: operationID,
          state: 'failed',
          created_at: createdAt,
          finished_at: Math.floor(Date.now() / 1000),
          error: message,
          write_state: 'none',
          devices: [],
        })
        setErr(message)
        setBusy('')
      } else {
        setErr(
          `The Apply response was lost or incomplete. Recovering operation ${operationID}; do not retry it.`,
        )
        setBusy('recover')
        setRecoveringOperation(true)
      }
      return
    }

    // A completed or partially aborted run invalidates both the preview and
    // consent attached to it. Refresh separately: a failed read after a
    // successful write must never be misreported as a failed/no-write apply.
    setPreview(null)
    setAckTraversal(false)
    setAckFatal(false)
    setAckCautions(false)
    const generation = ++previewGeneration.current
    try {
      const next = await api.preview()
      if (generation !== previewGeneration.current) return
      setPreview(next)
      setErr('')
    } catch (e) {
      if (generation !== previewGeneration.current) return
      const message = e instanceof Error ? e.message : String(e)
      setErr(
        `${resultMessage} Refresh failed: ${message}. ` +
        'Preview again before applying anything else.',
      )
    } finally {
      setBusy((current) => current === 'apply' ? '' : current)
    }
  }

  if (!site) {
    return (
      <div style={{ display: 'grid', gap: 14, maxWidth: 900 }}>
        <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
          {err
            ? <div role="alert"><Banner tone="critical">{err}</Banner></div>
            : 'Loading…'}
        </div>
      </div>
    )
  }

  const pending = preview?.devices.reduce((n, d) => n + d.changes.length, 0) ?? 0
  const operationNeedsAttention = Boolean(
    recoveringOperation || !operation ||
    operation.state === 'queued' || operation.state === 'running' || operation.state === 'unknown',
  )
  const showOperation = Boolean(operationID && (operationNeedsAttention || !preview))
  const traversal = preview?.devices.filter((d) => d.touches_traversal) ?? []
  const cautions =
    preview?.devices.flatMap((d) =>
      (d.cautions ?? []).map((caution) => ({ device: d.name, caution })),
    ) ?? []

  // Defects that this change ASKS FOR, and that kill the radio.
  //
  // Filtered on both halves, and both matter. `wlan` is set only when a WLAN's
  // configuration triggers the defect — the operator chose something the driver
  // cannot do. A defect with no `wlan` is a property of the hardware that no
  // configuration causes and none can avoid, and gating on those would demand a
  // tick before every apply to that device forever, which is the cry-wolf
  // failure that makes a warning worth ignoring on the day it matters.
  //
  // Severity is the other half. The screen already stops for `touches_traversal`
  // — editing the path the controller reaches a device through — and reassures
  // the reader that a rollback restores it within 90 seconds. That is true
  // there and FALSE here: a dead radio cannot be reached to confirm or revert,
  // and the firmware stays dead until somebody physically power-cycles the box
  // (STATUS §5an, measured). So the lesser, recoverable hazard asked for an
  // acknowledgement and the greater, unrecoverable one asked for nothing.
  const fatal =
    preview?.devices.flatMap((d) =>
      (d.driver_defects ?? [])
        .filter((f) => f.wlan && f.severity === 'radio-death')
        .map((f) => ({ device: d.name, ...f })),
    ) ?? []

  return (
    <div style={{ display: 'grid', gap: 14, maxWidth: 900 }}>
      {err && <div role="alert"><Banner tone="critical">{err}</Banner></div>}
      {site.problems.length > 0 && (
        <div role="alert">
          <Notice
            tone="warning"
            component="Configuration readiness"
            summary={`${site.problems.length} configuration problem${site.problems.length === 1 ? '' : 's'} block Apply.`}
            closedLabel="More information about configuration problems"
            openLabel="Hide configuration problems"
            details={<ul style={{ margin: 0, paddingLeft: 18 }}>
              {site.problems.map((p) => <li key={p}>{p}</li>)}
            </ul>}
          />
        </div>
      )}

      <Card title="Site">
        <div style={{ display: 'grid', gap: 6 }}>
          <Prop label="Name">{site.name}</Prop>
          <Prop label="Networks">{site.networks.length}</Prop>
          <Prop label="AP groups">{site.groups.length}</Prop>
          <Prop label="Wireless networks">{site.wlans.length}</Prop>
        </div>
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 8 }}>
          The site identifier <code>{site.uuid.slice(0, 8)}…</code> seeds the
          802.11r mobility domain, so every AP derives the same value with no
          coordination. It never changes — that is what keeps fast roaming
          working across the fleet.
        </div>
      </Card>

      <Card
        title="Wireless networks"
        actions={
          <Button
            onClick={() =>
              setEditing({
                ssid: '',
                bands: ['2g', '5g'],
                security_mode: 'sae-mixed',
                pmf: '1',
                enabled: true,
                network_id: site.networks[0]?.id ?? 0,
                group_id: site.groups[0]?.id ?? 0,
                roaming: { ft: true, ft_over_ds: true, kv: true, ft_with_psk2: false },
                hidden: false,
                isolate: false,
                max_assoc: 0,
              })
            }
          >
            Add a WLAN
          </Button>
        }
        pad={false}
      >
        {site.networks.length === 0 || site.groups.length === 0 ? (
          <div style={{ padding: 14 }}>
            <Banner>
              A WLAN needs a network to sit on and an AP group to publish it.
              Create one of each below first.
            </Banner>
          </div>
        ) : site.wlans.length === 0 ? (
          <div style={{ padding: 14, fontSize: 12, color: 'var(--text-secondary)' }}>
            No wireless networks yet.
          </div>
        ) : (
          <div>
            {site.wlans.map((w) => (
              <WLANRow
                key={w.id}
                w={w}
                site={site}
                onEdit={() => setEditing(w)}
                onDeleted={modelChanged}
              />
            ))}
          </div>
        )}
      </Card>

      <Card
        title="Mesh backhauls"
        actions={
          <Button
            onClick={() =>
              setEditingMesh({
                mesh_id: '',
                band: '5g',
                enabled: true,
                network_id: site.networks[0]?.id ?? 0,
                group_id: site.groups[0]?.id ?? 0,
              })
            }
          >
            Add a mesh
          </Button>
        }
        pad={false}
      >
        {site.networks.length === 0 || site.groups.length === 0 ? (
          <div style={{ padding: 14 }}>
            <Banner>
              A mesh needs a network to bridge and an AP group to carry it.
              Create one of each below first.
            </Banner>
          </div>
        ) : site.meshes.length === 0 ? (
          <div style={{ padding: 14, fontSize: 12, color: 'var(--text-secondary)' }}>
            No mesh backhauls. A mesh links APs over the air where you cannot run
            a cable — the devices still serve clients and their wired ports at the
            same time.
          </div>
        ) : (
          <div>
            {site.meshes.map((m) => (
              <MeshRow
                key={m.id}
                m={m}
                site={site}
                onEdit={() => setEditingMesh(m)}
                onDeleted={modelChanged}
              />
            ))}
          </div>
        )}
      </Card>

      <Uplinks site={site} devices={devices} onChanged={modelChanged} />
      <MeshHealth />
      <Groups site={site} devices={devices} onChanged={modelChanged} />
      <Neighbours site={site} />
      <Deviations site={site} devices={devices} onChanged={modelChanged} />
      <Networks site={site} onChanged={modelChanged} />

      {/* The pending-changes flow. Preview is a read; apply is the only thing
          that writes, and it is deliberately unreachable without previewing
          first — reading what a change does to each device is the point. */}
      <Card title="Pending changes">
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
          <Button onClick={runPreview} disabled={busy !== ''}>
            {busy === 'preview' ? 'Checking every device…' : 'Preview changes'}
          </Button>
          <Button
            kind="primary"
            disabled={
              busy !== '' ||
              !preview ||
              pending === 0 ||
              (preview.site_errors?.length ?? 0) > 0 ||
              preview.devices.some((d) => Boolean(d.error)) ||
              preview.devices.some((d) => d.blocked) ||
              (traversal.length > 0 && !ackTraversal) ||
              (cautions.length > 0 && !ackCautions) ||
              (fatal.length > 0 && !ackFatal)
            }
            onClick={runApply}
          >
            {busy === 'apply' ? 'Applying…' : `Apply${pending ? ` (${pending})` : ''}`}
          </Button>
          {applied && <span style={{ fontSize: 12 }}>{applied}</span>}
        </div>
        {showOperation && (
          <div
            role="status"
            aria-live="polite"
            style={{
              display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap',
              marginTop: 8, fontSize: 11, color: 'var(--text-secondary)',
            }}
          >
            <strong>{operationNeedsAttention ? 'Apply operation' : 'Previous Apply operation'}</strong>
            <code>{operationID}</code>
            <span>
              {recoveringOperation
                ? 'checking status…'
                : operation?.state ?? 'request sent'}
            </span>
            {!recoveringOperation && (
              <Button onClick={() => setRecoveringOperation(true)} disabled={busy !== ''}>
                Check status
              </Button>
            )}
            {(operation?.devices?.length ?? 0) > 0 && (
              <ul style={{ flexBasis: '100%', margin: '2px 0 0', paddingLeft: 18 }}>
                {operation!.devices.map((device) => (
                  <li key={`${device.ordinal}-${device.device_mac}`}>
                    <strong>{device.device_name}</strong> — {device.state}
                    {device.router_outcome && ` · router: ${device.router_outcome}`}
                    {device.outcome && device.outcome !== device.router_outcome &&
                      ` · controller: ${device.outcome}`}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
        <div style={{ marginTop: 8 }}>
          <Notice
            tone="accent"
            component="Apply behavior"
            summary={applyWriteSummary(
              showOperation ? operationID : '',
              showOperation ? operation : null,
              showOperation && recoveringOperation,
            )}
            closedLabel="More information about Apply"
            openLabel="Hide Apply information"
            details={(
              <div>
                Apply is the only step that writes, and it stops at the first
                device that fails to limit a partial rollout. Devices that
                already applied stay changed; the result names exactly where it
                stopped. Every change is applied with a rollback armed — a
                device that comes back unhealthy reverts itself.
              </div>
            )}
          />
        </div>

        {/* IMPLEMENTATION §6's traversal acknowledgment. The rollback protects
            this change like any other — that is what applying with one armed is
            for — but an operator should be told they are editing the road
            before driving down it. */}
        {traversal.length > 0 && (
          <div style={{ marginTop: 10 }}>
            <Notice
              tone="warning"
              component="Management path"
              summary={(
                <span>
                  <strong>{traversal.map((d) => d.name).join(', ')}</strong>{' '}
                  may become temporarily unreachable while this change edits the
                  network or firewall path the controller uses to reach{' '}
                  {traversal.length === 1 ? 'it' : 'them'}. An armed rollback
                  restores {traversal.length === 1 ? 'it' : 'them'} to{' '}
                  {traversal.length === 1 ? 'its' : 'their'} prior configuration
                  if {traversal.length === 1 ? 'it does' : 'they do'} not return.
                </span>
              )}
              closedLabel="More information about management-path rollback"
              openLabel="Hide management-path rollback information"
              details={(
                <div>
                It is applied with a rollback armed, so a device that comes back
                unreachable restores itself within 90 seconds. You should still
                know before, not after.
                </div>
              )}
              actions={<Toggle
                label="I understand — apply the network changes"
                on={ackTraversal}
                onChange={setAckTraversal}
              />}
            />
          </div>
        )}

        {cautions.length > 0 && (
          <div style={{ marginTop: 10 }}>
            <Banner tone="warning">
              <div>
                This plan has behavior that the controller cannot make safe by
                itself. Review it before writing to the fleet.
              </div>
              <ul style={{ margin: '6px 0 0', paddingLeft: 18, fontSize: 12 }}>
                {cautions.map((item, i) => (
                  <li key={`${item.device}-${i}`}>
                    <strong>{item.device}</strong> — {item.caution}
                  </li>
                ))}
              </ul>
              <Toggle
                label="I reviewed these cautions and want to apply"
                on={ackCautions}
                onChange={setAckCautions}
              />
            </Banner>
          </div>
        )}

        {/* The one hazard on this screen a rollback cannot undo. */}
        {fatal.length > 0 && (
          <div style={{ marginTop: 10 }}>
            <Notice
              tone="critical"
              component="Driver risk"
              defaultOpen
              summary={(
                <span>
                  <strong>The rollback does not cover this.</strong>{' '}
                This change asks{' '}
                <strong>
                  {[...new Set(fatal.map((f) => f.device))].join(', ')}
                </strong>{' '}
                to do something its wireless driver is known to get wrong badly
                enough to take the radio down until someone physically
                power-cycles the device.
                </span>
              )}
              closedLabel="More information about the driver risk"
              openLabel="Hide driver risk information"
              details={(
                <div>
                  <ul style={{ margin: 0, paddingLeft: 18 }}>
                    {fatal.map((f) => (
                      <li key={`${f.device}-${f.defect_id}`}>
                        <strong>{f.device}</strong> — {f.summary}{' '}
                        <span style={{ color: 'var(--text-muted)' }}>
                          [{f.confidence}]
                        </span>
                      </li>
                    ))}
                  </ul>
                  <div style={{ marginTop: 6 }}>
                    A radio that stops answering cannot be reached to confirm or
                    revert. Details and a mitigation are under each device below.
                  </div>
                </div>
              )}
              actions={<Toggle
                label="I understand this can take the radio down until someone power-cycles it"
                on={ackFatal}
                onChange={setAckFatal}
              />}
            />
          </div>
        )}

        {preview && <Preview p={preview} />}
      </Card>

      {editingMesh && (
        <MeshEditor
          m={editingMesh}
          site={site}
          onClose={() => setEditingMesh(null)}
          onSaved={async () => {
            setEditingMesh(null)
            // A saved mesh makes the previous preview stale, and a stale
            // preview is the one thing this screen must never show.
            await modelChanged()
          }}
        />
      )}

      {editing && (
        <WLANEditor
          w={editing}
          site={site}
          onClose={() => setEditing(null)}
          onSaved={async () => {
            setEditing(null)
            // A saved WLAN makes the previous preview stale, and a stale
            // preview next to an Apply button is the one thing this screen
            // must never show.
            await modelChanged()
          }}
        />
      )}
    </div>
  )
}

function WLANRow({
  w,
  site,
  onEdit,
  onDeleted,
}: {
  w: WLAN
  site: Site
  onEdit: () => void
  onDeleted: () => Promise<void>
}) {
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [deleteError, setDeleteError] = useState('')
  const group = site.groups.find((g) => g.id === w.group_id)
  const net = site.networks.find((n) => n.id === w.network_id)
  return (
    <div
      style={{
        padding: '10px 14px',
        borderTop: '1px solid var(--border)',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ fontSize: 13, fontWeight: 600 }}>
            {w.ssid}
            {!w.enabled && (
              <span style={{ color: 'var(--text-muted)', fontWeight: 400 }}> · disabled</span>
            )}
          </div>
          <div style={{ fontSize: 11, color: 'var(--text-secondary)' }}>
            {w.bands.join(' + ')} · {w.security_mode}
            {w.has_key ? '' : w.security_mode !== 'none' && w.security_mode !== 'owe'
              ? ' · no passphrase set'
              : ''}
            {' · '}
            {group?.name ?? `group ${w.group_id}`} · {net?.name ?? `network ${w.network_id}`}
            {w.roaming.ft && ' · 802.11r'}
          </div>
        </div>
        <Button aria-label={`Edit wireless network ${w.ssid}`} disabled={deleting} onClick={onEdit}>
          Edit
        </Button>
        <Button
          aria-label={`Delete wireless network ${w.ssid}`}
          disabled={deleting}
          onClick={() => {
            setDeleteError('')
            setConfirmDelete(true)
          }}
        >
          Delete
        </Button>
      </div>
      {confirmDelete && (
        <div style={{ marginTop: 10 }}>
          <Banner tone="warning">
            <div style={{ display: 'grid', gap: 8 }}>
              <span>
                Delete <strong>{w.ssid}</strong> from desired state? No router changes until
                you Preview and Apply.
              </span>
              <div style={{ display: 'flex', gap: 8 }}>
                <Button
                  disabled={deleting}
                  onClick={async () => {
                    setDeleting(true)
                    setDeleteError('')
                    try {
                      await api.deleteWLAN(w.id)
                      await onDeleted()
                    } catch (e) {
                      setDeleteError(e instanceof Error ? e.message : String(e))
                    } finally {
                      setDeleting(false)
                    }
                  }}
                >
                  {deleting ? 'Deleting…' : `Delete “${w.ssid}”`}
                </Button>
                <Button disabled={deleting} onClick={() => setConfirmDelete(false)}>
                  Keep “{w.ssid}”
                </Button>
              </div>
            </div>
          </Banner>
        </div>
      )}
      {deleteError && <div role="alert" style={{ marginTop: 8 }}><Banner tone="critical">{deleteError}</Banner></div>}
    </div>
  )
}

/**
 * The WLAN editor.
 *
 * The passphrase field starts empty on an edit and is only sent when typed.
 * The server treats an empty key on an update as "leave it alone", so changing
 * a band or a roaming toggle never requires the operator — or this screen — to
 * hold the secret.
 */
function WLANEditor({
  w,
  site,
  onClose,
  onSaved,
}: {
  w: Partial<WLAN>
  site: Site
  onClose: () => void
  onSaved: () => void
}) {
  // Seeded through the same clamp that guards the mode change.
  //
  // A WLAN persisted as sae-mixed with pmf="0" — writable by an earlier build,
  // by the mode-switch hole fixed in 08ed4a5, or by a plain POST to the API —
  // opened with two buttons and neither selected, then re-saved the value it
  // had never shown. Guarding one door and not the other is not guarding.
  const [draft, setDraft] = useState<Partial<WLAN>>({
    ...w,
    key: '',
    pmf: clampPMF((w.security_mode ?? 'sae-mixed') as WLAN['security_mode'], w.pmf),
  })
  // The last PMF the OPERATOR chose, before any mode-driven coercion.
  //
  // Mode changes clamp from THIS rather than from the already-clamped draft.
  // Clamping from the draft loses a deliberate choice on a round trip: a
  // Required network detoured through Open comes back Disabled, and a Disabled
  // one — the exact state the mwlwifi defect tells operators to use — detoured
  // through WPA3 comes back Required. Neither touched the PMF control, and the
  // apply preview names the section and a count of options, never the value, so
  // the change appears nowhere.
  const [chosenPMF, setChosenPMF] = useState<WLAN['pmf']>(w.pmf ?? '1')
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState('')
  const set = (patch: Partial<WLAN>) => setDraft((d) => ({ ...d, ...patch }))

  const needsKey =
    draft.security_mode === 'sae' ||
    draft.security_mode === 'sae-mixed' ||
    draft.security_mode === 'psk2'
  // 802.11r on WPA2-PSK breaks some older clients, so it is an explicit
  // opt-in rather than something the renderer decides quietly.
  const ftOnPSK2 = draft.roaming?.ft && draft.security_mode === 'psk2'

  async function save() {
    setSaving(true)
    try {
      await api.saveWLAN(draft)
      onSaved()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card title={w.id ? `Edit ${w.ssid}` : 'New wireless network'}>
      <div style={{ display: 'grid', gap: 12, maxWidth: 460 }}>
        {err && <div role="alert"><Banner tone="critical">{err}</Banner></div>}

        <Field
          label="SSID"
          value={draft.ssid ?? ''}
          autoFocus
          onChange={(e) => set({ ssid: e.target.value })}
        />

        <Choice
          label="Bands"
          multi
          options={[
            { v: '2g', l: '2.4 GHz' },
            { v: '5g', l: '5 GHz' },
            { v: '6g', l: '6 GHz' },
          ]}
          value={draft.bands ?? []}
          onChange={(bands) => set({ bands })}
        />
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: -6 }}>
          A band no device in the group has is simply not rendered there — the
          preview says which options were left out and why.
        </div>

        <Choice
          label="Security"
          options={[
            { v: 'sae-mixed', l: 'WPA2/WPA3' },
            { v: 'sae', l: 'WPA3 only' },
            { v: 'psk2', l: 'WPA2 only' },
            { v: 'owe', l: 'Enhanced open' },
            { v: 'none', l: 'Open' },
          ]}
          value={[draft.security_mode ?? 'sae-mixed']}
          onChange={([m]) =>
            // The PMF value travels with the mode. Changing to WPA2/WPA3 while
            // PMF was "Disabled" left a draft holding a value that mode does
            // not offer: no button rendered as selected, and saving stored a
            // setting the form never showed. The renderer coerces it safely,
            // but a form should not produce a value it will not display.
            set({
              security_mode: m as WLAN['security_mode'],
              pmf: clampPMF(m as WLAN['security_mode'], chosenPMF),
            })
          }
        />

        {/* Protected management frames.
            Absent from this form until now, while the renderer wrote
            ieee80211w on every WLAN from a value hardcoded at creation. That
            made the driver-defect warning unactionable: it told an operator to
            turn PMF off on hardware that cannot do it, and there was nowhere to
            do that. WPA3 mandates it, so the control is hidden rather than
            shown-and-ignored when SAE is selected — the renderer forces it back
            on there regardless, and offering a choice the renderer overrides is
            worse than offering none. */}
        {/* Hidden where the mode fixes the answer, and constrained where it
            sets a floor. Offering a value the renderer will override is worse
            than offering none: SAE and OWE both mandate PMF, and on a
            transitional network "Disabled" silently removes the WPA3 half of a
            network still advertising it. Open has no RSN, so nothing to
            protect — and a WLAN switched to Open used to keep the pmf="1" every
            draft is created with, which then rendered onto the device where an
            operator could not see it, let alone clear it. */}
        {draft.security_mode !== 'sae' &&
          draft.security_mode !== 'owe' &&
          draft.security_mode !== 'none' && (
          <>
            <Choice
              label="Protected management frames"
              options={
                draft.security_mode === 'sae-mixed'
                  ? [
                      { v: '1', l: 'Optional' },
                      { v: '2', l: 'Required' },
                    ]
                  : [
                      { v: '1', l: 'Optional' },
                      { v: '2', l: 'Required' },
                      { v: '0', l: 'Disabled' },
                    ]
              }
              value={[draft.pmf ?? '1']}
              onChange={([v]) => {
                // Remember what was actually picked, so a later mode detour
                // can return to it rather than to whatever the clamp produced.
                setChosenPMF(v as WLAN['pmf'])
                set({ pmf: v as WLAN['pmf'] })
              }}
            />
            <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: -6 }}>
              Protects deauthentication and disassociation frames. Leave it on
              unless a radio cannot do it — some drivers accept the setting and
              do not implement it, and the apply preview names the ones known to.
              It cannot be varied per device: APs in one mobility domain must
              agree, or 802.11r roaming fails intermittently rather than cleanly.
            </div>
          </>
        )}

        {needsKey && (
          <>
            <Field
              label={w.id ? 'Passphrase (leave blank to keep the current one)' : 'Passphrase'}
              type="password"
              autoComplete="new-password"
              value={draft.key ?? ''}
              onChange={(e) => set({ key: e.target.value })}
            />
            {w.id && !draft.key && (
              <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: -6 }}>
                {w.has_key
                  ? 'The existing passphrase stays as it is.'
                  : 'This network has no passphrase yet and will not apply until it does.'}
              </div>
            )}
          </>
        )}

        <Choice
          label="Network"
          options={site.networks.map((n) => ({ v: String(n.id), l: `${n.name} (VLAN ${n.vlan})` }))}
          value={[String(draft.network_id ?? '')]}
          onChange={([v]) => set({ network_id: Number(v) })}
        />
        <Choice
          label="AP group"
          options={site.groups.map((g) => ({
            v: String(g.id),
            l: `${g.name} (${g.device_ids.length} device${g.device_ids.length === 1 ? '' : 's'})`,
          }))}
          value={[String(draft.group_id ?? '')]}
          onChange={([v]) => set({ group_id: Number(v) })}
        />

        <div>
          <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 4 }}>
            Roaming
          </div>
          <Toggle
            label="802.11r fast transition"
            on={!!draft.roaming?.ft}
            onChange={(v) =>
              set({ roaming: { ...draft.roaming!, ft: v } })
            }
          />
          <Toggle
            label="802.11k/v neighbour reports and BSS transition"
            on={!!draft.roaming?.kv}
            onChange={(v) => set({ roaming: { ...draft.roaming!, kv: v } })}
          />
          <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
            Every AP in the group gets the same mobility domain, derived from the
            site identifier. This is the thing that is essentially impossible to
            keep consistent by hand.
          </div>
          {ftOnPSK2 && (
            <div style={{ marginTop: 8 }}>
              <Banner tone="warning">
                802.11r on WPA2-only breaks association for some older clients.
                It is applied only if you tick this.
              </Banner>
              <Toggle
                label="I accept the WPA2 + 802.11r compatibility risk"
                on={!!draft.roaming?.ft_with_psk2}
                onChange={(v) =>
                  set({ roaming: { ...draft.roaming!, ft_with_psk2: v } })
                }
              />
            </div>
          )}
        </div>

        <div>
          <Toggle label="Enabled" on={!!draft.enabled} onChange={(v) => set({ enabled: v })} />
          <Toggle label="Hide the SSID" on={!!draft.hidden} onChange={(v) => set({ hidden: v })} />
          <Toggle
            label="Isolate clients on this access point"
            on={!!draft.isolate}
            onChange={(v) => set({ isolate: v })}
          />
          <div style={{ fontSize: 11, color: 'var(--text-muted)', margin: '-2px 0 4px' }}>
            Requests BSS isolation and verifies bridge-port isolation across radios
            on one AP. Verify client behavior after applying; different APs still
            need additional switch or bridge policy.
          </div>
          {/* The AP half of a wireless uplink. Off unless asked for, because it
              changes what the access points accept from the air rather than
              merely what they advertise — and it is the half people forget:
              configure the joining device and not this, and the device
              associates as an ordinary client while everything behind it stays
              dark. */}
          <Toggle
            label="Allow devices to join this network as a wireless bridge"
            on={!!draft.allow_uplink}
            onChange={(v) => set({ allow_uplink: v })}
          />
        </div>

        <div style={{ display: 'flex', gap: 8 }}>
          <Button kind="primary" disabled={saving || !draft.ssid} onClick={save}>
            {saving ? 'Saving…' : 'Save'}
          </Button>
          <Button onClick={onClose}>Cancel</Button>
        </div>
        <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          Saving writes the site model only. Nothing reaches a device until you
          preview and apply.
        </div>
      </div>
    </Card>
  )
}

function Groups({
  site,
  devices,
  onChanged,
}: {
  site: Site
  devices: Device[]
  onChanged: () => Promise<void>
}) {
  const [name, setName] = useState('')
  const [membershipOverrides, setMembershipOverrides] = useState<Record<number, number[]>>({})
  const [membershipBusy, setMembershipBusy] = useState<Set<number>>(new Set())
  const [membershipErrors, setMembershipErrors] = useState<Record<number, string>>({})
  const desiredMemberships = useRef(new Map<number, number[]>())
  const membershipQueues = useRef(new Map<number, Promise<void>>())
  const adopted = devices.filter((d) => d.adopted)

  function toggle(g: APGroup, deviceID: number) {
    const current = desiredMemberships.current.get(g.id) ?? g.device_ids
    const next = current.includes(deviceID)
      ? current.filter((id) => id !== deviceID)
      : [...current, deviceID]
    desiredMemberships.current.set(g.id, next)
    setMembershipOverrides((values) => ({ ...values, [g.id]: next }))
    setMembershipBusy((values) => new Set(values).add(g.id))
    setMembershipErrors((values) => ({ ...values, [g.id]: '' }))

    const previous = membershipQueues.current.get(g.id) ?? Promise.resolve()
    const task = previous.catch(() => undefined).then(async () => {
      await api.saveGroup({ id: g.id, name: g.name, device_ids: next })
    })
    membershipQueues.current.set(g.id, task)

    void task.then(
      () => finishMembership(g.id, task, ''),
      (e) => finishMembership(g.id, task, e instanceof Error ? e.message : String(e)),
    )
  }

  async function finishMembership(groupID: number, task: Promise<void>, error: string) {
    if (membershipQueues.current.get(groupID) !== task) return
    await onChanged()
    if (membershipQueues.current.get(groupID) !== task) return
    membershipQueues.current.delete(groupID)
    desiredMemberships.current.delete(groupID)
    setMembershipOverrides((values) => {
      const next = { ...values }
      delete next[groupID]
      return next
    })
    setMembershipBusy((values) => {
      const next = new Set(values)
      next.delete(groupID)
      return next
    })
    if (error) setMembershipErrors((values) => ({ ...values, [groupID]: error }))
  }

  return (
    <Card
      title="AP groups"
      actions={
        <div style={{ display: 'flex', gap: 6 }}>
          <input
            aria-label="New AP group name"
            value={name}
            placeholder="new group"
            onChange={(e) => setName(e.target.value)}
            style={{
              height: 26,
              padding: '0 8px',
              borderRadius: 4,
              background: 'var(--surface-0)',
              border: '1px solid var(--border-strong)',
              color: 'var(--text-primary)',
              fontSize: 12,
            }}
          />
          <Button
            disabled={!name.trim()}
            onClick={async () => {
              await api.saveGroup({ name: name.trim(), device_ids: [] })
              setName('')
              onChanged()
            }}
          >
            Add
          </Button>
        </div>
      }
    >
      {site.groups.length === 0 ? (
        <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
          A group is the set of APs a WLAN publishes on. Create one to get started.
        </div>
      ) : (
        <div style={{ display: 'grid', gap: 12 }}>
          {site.groups.map((g) => (
            <div key={g.id}>
              <div
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 8,
                  fontSize: 13,
                  fontWeight: 600,
                }}
              >
                {g.name}
                <div style={{ flex: 1 }} />
                <Button
                  aria-label={`Delete AP group ${g.name}`}
                  onClick={async () => {
                    try {
                      await api.deleteGroup(g.id)
                      onChanged()
                    } catch (e) {
                      alert(e instanceof Error ? e.message : String(e))
                    }
                  }}
                >
                  Delete
                </Button>
              </div>
              <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginTop: 4 }}>
                {adopted.length === 0 && (
                  <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>
                    No adopted devices yet.
                  </span>
                )}
                {adopted.map((d) => (
                  <label
                    key={d.id}
                    style={{
                      display: 'inline-flex',
                      alignItems: 'center',
                      gap: 4,
                      fontSize: 12,
                      cursor: 'pointer',
                    }}
                  >
                    <input
                      type="checkbox"
                      checked={(membershipOverrides[g.id] ?? g.device_ids).includes(d.id)}
                      onChange={() => toggle(g, d.id)}
                    />
                    {d.name}
                  </label>
                ))}
              </div>
              {membershipBusy.has(g.id) && (
                <div style={{ marginTop: 4, fontSize: 11, color: 'var(--text-muted)' }}>
                  Saving membership changes…
                </div>
              )}
              {membershipErrors[g.id] && (
                <div role="alert" style={{ marginTop: 6 }}>
                  <Banner tone="critical">{membershipErrors[g.id]}</Banner>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </Card>
  )
}

/**
 * Per-device overrides.
 *
 * The list of what can be overridden is short on purpose, and the note says
 * why: SSID, passphrase, security mode and roaming are not on it. Keeping those
 * identical across every AP is what a controller is for, and a client roaming
 * between APs that disagree about them does not fail cleanly — it fails
 * intermittently, which is far worse to debug.
 *
 * Everything set here is listed back in one place, because the danger of
 * overrides is never a single one. It is a fleet that drifts apart device by
 * device until nobody can say what is actually deployed.
 */
function Deviations({
  site,
  devices,
  onChanged,
}: {
  site: Site
  devices: Device[]
  onChanged: () => Promise<void>
}) {
  const [deviceID, setDeviceID] = useState<number | null>(null)
  const [pending, setPending] = useState<Record<string, string>>({})
  const [changeError, setChangeError] = useState('')
  const adopted = devices.filter((d) => d.adopted)
  const target = deviceID ?? adopted[0]?.id ?? null

  if (site.wlans.length === 0 || adopted.length === 0) return null

  const forDevice = site.overrides.filter((o) => o.device_id === target)
  const pendingKey = (wlanID: number, key: string) => `${target}.${wlanID}.${key}`
  const valueOf = (wlanID: number, key: string) => {
    const k = pendingKey(wlanID, key)
    return Object.prototype.hasOwnProperty.call(pending, k)
      ? pending[k]
      : forDevice.find((o) => o.wlan_id === wlanID && o.key === key)?.value ?? ''
  }

  async function set(wlanID: number, key: string, value: string) {
    const k = pendingKey(wlanID, key)
    setPending((current) => ({ ...current, [k]: value }))
    setChangeError('')
    try {
      await api.setOverride(target!, wlanID, key, value)
      await onChanged()
    } catch (error) {
      setChangeError(error instanceof Error ? error.message : String(error))
    } finally {
      setPending((current) => {
        const next = { ...current }
        delete next[k]
        return next
      })
    }
  }

  return (
    <Card title="Per-device overrides">
      <div style={{ display: 'grid', gap: 10 }}>
        {changeError && <div role="alert"><Banner tone="critical">{changeError}</Banner></div>}
        {site.overrides.length > 0 && (
          <div style={{ fontSize: 11 }}>
            <strong>{new Set(site.overrides.map((o) => o.device_id)).size}</strong>{' '}
            device
            {new Set(site.overrides.map((o) => o.device_id)).size === 1 ? '' : 's'}{' '}
            currently deviate from the site model:
            <ul style={{ margin: '4px 0 0', paddingLeft: 18, color: 'var(--text-secondary)' }}>
              {site.overrides.map((o) => (
                <li key={`${o.device_id}.${o.wlan_id}.${o.key}`}>
                  {devices.find((d) => d.id === o.device_id)?.name ?? o.device_id}:{' '}
                  {o.describe}
                </li>
              ))}
            </ul>
          </div>
        )}

        <Choice
          label="Device"
          options={adopted.map((d) => ({ v: String(d.id), l: d.name }))}
          value={[String(target ?? '')]}
          onChange={([v]) => setDeviceID(Number(v))}
        />

        {site.wlans.map((w) => (
          <div key={w.id} style={{ fontSize: 12 }}>
            <div style={{ fontWeight: 600 }}>{w.ssid}</div>
            <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', marginTop: 2 }}>
              <Toggle
                label="Do not publish here"
                on={valueOf(w.id, 'disabled') === '1'}
                disabled={Object.prototype.hasOwnProperty.call(pending, pendingKey(w.id, 'disabled'))}
                onChange={(v) => set(w.id, 'disabled', v ? '1' : '')}
              />
              <Toggle
                label="Hide here"
                on={valueOf(w.id, 'hidden') === '1'}
                disabled={Object.prototype.hasOwnProperty.call(pending, pendingKey(w.id, 'hidden'))}
                onChange={(v) => set(w.id, 'hidden', v ? '1' : '')}
              />
              <Toggle
                label="Isolate clients here"
                on={valueOf(w.id, 'isolate') === '1'}
                disabled={Object.prototype.hasOwnProperty.call(pending, pendingKey(w.id, 'isolate'))}
                onChange={(v) => set(w.id, 'isolate', v ? '1' : '')}
              />
            </div>
          </div>
        ))}

        <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          {site.override_note}.
        </div>
      </div>
    </Card>
  )
}

// Zone names the device already owns. oonfeeWRT will not edit the operator's
// own firewall zones, so a network asking for one is refused at preview — said
// here too, where it can actually be fixed.
const deviceOwnedZones = ['lan', 'wan']

// fw4's limit on a zone NAME. Two zones that differ only past it are one zone
// to the device, which renderZones refuses rather than silently merging.
const maxZoneName = 11

// Exactly the identifier the model and renderer give fw4. Punctuation other
// than separators is dropped, separators become underscores, and fw4 reads at
// most 11 characters. Validation must reason about this value: `wan!` is still
// the device's real `wan` zone, and current fw4 rejects an identifier beginning
// with a digit even though UCI itself accepts it.
function firewallZoneIdentifier(zone: string): { base: string; id: string } {
  let base = ''
  for (const ch of zone.trim().toLowerCase()) {
    if (/[a-z0-9]/.test(ch)) base += ch
    else if (ch === '_' || ch === '-' || ch === ' ') base += '_'
  }
  base = base.replace(/^_+|_+$/g, '')
  return { base, id: (base || 'net').slice(0, maxZoneName) }
}

// A zone name the operator can actually type, derived from the network's own
// name — which is what a network created today defaults to.
function suggestZone(networkName: string): string {
  const base = networkName.trim().toLowerCase().replace(/[^a-z0-9]+/g, '_')
    .replace(/^_+|_+$/g, '')
  let distinct = base && !deviceOwnedZones.includes(base) ? base : `${base || 'net'}_zone`
  if (/^[0-9]/.test(distinct)) distinct = `net_${distinct}`
  return distinct.slice(0, maxZoneName).replace(/_+$/, '') || 'guest'
}

// What is wrong, and what to type instead.
//
// The first version said only what was wrong. It was accurate, and the
// operator's reply was "I'm not actually sure what to do" — so it was half a
// message. STATUS §6 already has the rule about advice that names an action,
// and this broke it on the very screen where it was written.
//
// vlan is taken because a network on VLAN 0 or 1 renders NO firewall zone at
// all: renderNetwork returns early, that traffic being the device's own LAN.
// Its zone is inert, so flagging it is a warning on a row nobody can act on.
function zoneWarning(zone: string, networkName: string, vlan: number): string | null {
  const raw = zone.trim()
  if (!raw || vlan <= 1) return null
  const { base, id } = firewallZoneIdentifier(raw)
  const fix = suggestZone(networkName)
  const owned = deviceOwnedZones.map((q) => `"${q}"`).join(' or ')
  if (!base) {
    return `Zone "${raw}" contains no usable letters or digits, so OpenWrt cannot create it. To fix it: type a name such as "${fix}".`
  }
  if (/^[0-9]/.test(base)) {
    return `Zone "${raw}" renders as "${id}", which starts with a digit and is rejected by current OpenWrt fw4. To fix it: start the name with a letter, for example "${fix}".`
  }
  if (deviceOwnedZones.includes(id)) {
    return `Zone "${raw}" renders as "${id}", which belongs to the device, not to oonfeeWRT. Applying this network would be refused because oonfeeWRT never edits config it did not write. To fix it: type a different name, for example "${fix}". The rendered name must not be ${owned}.`
  }
  if (base.length > maxZoneName) {
    return `Zone "${raw}" is longer than fw4 reads. Only the first ${maxZoneName} rendered characters count, so the device sees "${id}" — and any other zone matching that far becomes the same zone. To fix it: shorten it, for example "${fix}".`
  }
  return null
}

function zoneReassignmentWarning(
  original: string,
  target: string,
  vlan: number,
  zones: Site['zones'],
): string | null {
  const name = target.trim()
  if (vlan <= 1 || !name || name === original || zones.some(
    (zone) => zone.name === name && zone.explicit,
  )) return null
  return `Moving this network to zone “${name}” uses the legacy policy until you define one: Internet/WAN is allowed and every other managed zone is blocked. Review Policy Engine, then Preview before Apply.`
}

// The zone a network's firewall rules live in, editable in place.
//
// There was no control for this at all, and store.SaveNetwork used to default
// it to "lan" — so every network the product could create asked for a second
// firewall zone named lan beside the device's own, and nothing in the UI could
// change it. The default is the network's own name now; this is how the ones
// created before that get fixed.
function NetworkZone({
  n, zones, onChanged,
}: { n: SiteNetwork; zones: Site['zones']; onChanged: () => void }) {
  const [value, setValue] = useState(n.zone)
  const [busy, setBusy] = useState(false)
  const trimmed = value.trim()
  const dirty = trimmed !== n.zone && trimmed !== ''
  const warning = zoneWarning(dirty ? trimmed : n.zone, n.name, n.vlan)
  const policyWarning = dirty
    ? zoneReassignmentWarning(n.zone, trimmed, n.vlan, zones)
    : null

  const save = async () => {
    if (!dirty || busy) return
    setBusy(true)
    try {
      // The API merges partial updates while holding the site-mutation gate.
      // Sending only the intended field avoids a stale editor overwriting an
      // unrelated DHCP/address change made in another tab.
      await api.saveNetwork({ id: n.id, zone: trimmed })
      onChanged()
    } catch (e) {
      alert(e instanceof Error ? e.message : String(e))
      setValue(n.zone)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <span style={{ color: 'var(--text-secondary)' }}>zone</span>
      <input
        aria-label={`Firewall zone for ${n.name}`}
        value={value}
        disabled={busy}
        onClick={(event) => event.stopPropagation()}
        onChange={(e) => setValue(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') void save()
          if (e.key === 'Escape') setValue(n.zone)
        }}
        onBlur={() => void save()}
        style={{
          width: 110, fontSize: 12, padding: '2px 6px',
          background: 'var(--surface-2)', color: 'var(--text-primary)',
          border: `1px solid ${warning ? 'var(--warning)' : 'var(--border)'}`,
          borderRadius: 4,
        }}
      />
      {warning && (
        <span
          title={warning}
          role="note"
          style={{ color: 'var(--warning)', fontSize: 11, cursor: 'help' }}
        >
          ⚠ needs a different zone
        </span>
      )}
      {policyWarning && (
        <span role="note" style={{ color: 'var(--warning)', fontSize: 11 }}>
          {policyWarning}
        </span>
      )}
    </>
  )
}

type DHCPSettings = NonNullable<SiteNetwork['dhcp']>

const DEFAULT_DHCP: DHCPSettings = {
  enabled: true,
  start: 100,
  limit: 150,
  leasetime: '12h',
}

function dhcpFor(n: SiteNetwork): DHCPSettings {
  return n.dhcp ?? DEFAULT_DHCP
}

// What a /nn actually gives you, derived rather than stored. DHCP uses the
// same offset/count vocabulary OpenWrt writes to /etc/config/dhcp.
function subnetFacts(
  cidr: string,
  dhcp: DHCPSettings,
): { label: string; value: string }[] | null {
  const m = /^(\d+)\.(\d+)\.(\d+)\.(\d+)\/(\d+)$/.exec(cidr.trim())
  if (!m) return null
  const octets = [1, 2, 3, 4].map((i) => Number(m[i]))
  const bits = Number(m[5])
  if (octets.some((o) => o > 255) || bits < 8 || bits > 30) return null
  const addr = octets.reduce((acc, o) => acc * 256 + o, 0)
  const mask = bits === 0 ? 0 : (0xffffffff << (32 - bits)) >>> 0
  const network = (addr & mask) >>> 0
  const broadcast = (network | (~mask >>> 0)) >>> 0
  const dot = (n: number) =>
    [24, 16, 8, 0].map((sh) => (n >>> sh) & 255).join('.')
  const usable = Math.max(0, broadcast - network - 1)
  const poolStart = network + dhcp.start
  const poolEnd = network + dhcp.start + dhcp.limit - 1
  const facts = [
    { label: 'Gateway IP', value: dot(addr) },
    { label: 'Netmask', value: dot(mask) },
    { label: 'Broadcast IP', value: dot(broadcast) },
    { label: 'Usable IPs', value: String(usable) },
  ]
  const gatewayOffset = addr - network
  if (
    dhcp.enabled && dhcp.start >= 1 && dhcp.limit >= 1 &&
    poolEnd < broadcast &&
    (gatewayOffset < dhcp.start || gatewayOffset > dhcp.start + dhcp.limit - 1)
  ) {
    facts.push({ label: 'DHCP range', value: `${dot(poolStart)} – ${dot(poolEnd)}` })
    facts.push({ label: 'Lease time', value: dhcp.leasetime })
  }
  return facts
}

function dhcpProblem(cidr: string, dhcp: DHCPSettings): string | null {
  if (!dhcp.enabled) return null
  if (!Number.isInteger(dhcp.start) || dhcp.start < 1) {
    return 'Pool start must be at least 1.'
  }
  if (!Number.isInteger(dhcp.limit) || dhcp.limit < 1) {
    return 'Pool limit must be at least 1 lease.'
  }
  if (!/^(?:[1-9][0-9]*[smhdw]|infinite)$/.test(dhcp.leasetime.trim())) {
    return 'Lease time must be a number followed by s, m, h, d or w, or “infinite”.'
  }
  const m = /^(\d+)\.(\d+)\.(\d+)\.(\d+)\/(\d+)$/.exec(cidr.trim())
  if (!m) return null // the address field owns this diagnostic
  const octets = [1, 2, 3, 4].map((i) => Number(m[i]))
  const bits = Number(m[5])
  if (octets.some((o) => o > 255) || bits < 8 || bits > 32) return null
  if (bits > 30) return `DHCP needs usable host addresses; a /${bits} has none.`
  const addr = octets.reduce((acc, o) => acc * 256 + o, 0)
  const mask = (0xffffffff << (32 - bits)) >>> 0
  const network = (addr & mask) >>> 0
  const highest = 2 ** (32 - bits) - 2
  const end = dhcp.start + dhcp.limit - 1
  if (end > highest) {
    return `Pool offsets ${dhcp.start}–${end} do not fit this /${bits}; the highest usable offset is ${highest}.`
  }
  const gateway = addr - network
  if (gateway >= dhcp.start && gateway <= end) {
    return `The pool includes this network’s gateway at offset ${gateway}.`
  }
  return null
}

function networkCIDRProblem(cidr: string): string | null {
  const raw = cidr.trim()
  if (!raw) return 'A managed VLAN needs an IPv4 gateway address in CIDR form.'
  const m = /^(\d+)\.(\d+)\.(\d+)\.(\d+)\/(\d+)$/.exec(raw)
  if (!m) return 'The gateway must be an IPv4 address in CIDR form.'
  const octets = [1, 2, 3, 4].map((i) => Number(m[i]))
  const bits = Number(m[5])
  if (octets.some((o) => o > 255) || bits < 8 || bits > 32) {
    return 'The gateway must be IPv4 with a prefix from /8 through /32.'
  }
  if (bits > 30) return null
  const addr = octets.reduce((acc, o) => acc * 256 + o, 0) >>> 0
  const mask = (0xffffffff << (32 - bits)) >>> 0
  const network = (addr & mask) >>> 0
  const offset = addr - network
  const hosts = 2 ** (32 - bits)
  if (offset === 0) {
    return `${raw} is the subnet address, not a usable gateway. Choose a host address such as .1.`
  }
  if (offset === hosts - 1) {
    return `${raw} is the broadcast address, not a usable gateway. Choose a host address such as .1.`
  }
  return null
}

// The editor for a network that already exists.
//
// There was none: a network could be created and deleted and nothing else, so
// a typo in a VLAN or an address meant deleting the row and starting again —
// and the zone, once it defaulted to "lan", could not be corrected at all.
function NetworkEditor({
  n, zones, onClose, onChanged,
}: {
  n: SiteNetwork
  zones: Site['zones']
  onClose: () => void
  onChanged: () => void
}) {
  const [draft, setDraft] = useState({
    name: n.name, vlan: n.vlan, cidr: n.cidr ?? '', zone: n.zone,
    dhcp: { ...dhcpFor(n) }, enabled: n.enabled,
  })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const managesDHCP = draft.vlan > 1
  const addressWarning = draft.enabled && managesDHCP
    ? networkCIDRProblem(draft.cidr)
    : null
  const facts = subnetFacts(draft.cidr, managesDHCP
    ? draft.dhcp
    : { ...draft.dhcp, enabled: false })
  const dhcpWarning = draft.enabled && managesDHCP
    ? dhcpProblem(draft.cidr, draft.dhcp)
    : null
  const legacyDHCPWarning = n.dhcp?.legacy_default && dhcpWarning
    ? `This upgraded network still inherits the legacy DHCP pool. ${dhcpWarning} ` +
      'Choose one: customize Pool start and Pool limit, or turn DHCP server off, then Save. ' +
      'No device will be changed until you make that explicit choice.'
    : null
  const warning = zoneWarning(draft.zone, draft.name, draft.vlan)
  const policyWarning = zoneReassignmentWarning(
    n.zone, draft.zone, draft.vlan, zones,
  )
  const dirty =
    draft.name !== n.name || draft.vlan !== n.vlan ||
    (draft.cidr || '') !== (n.cidr ?? '') || draft.zone !== n.zone ||
    draft.enabled !== n.enabled ||
    JSON.stringify(draft.dhcp) !== JSON.stringify(dhcpFor(n))

  const save = async () => {
    setBusy(true)
    setErr('')
    try {
      await api.saveNetwork({
        id: n.id, name: draft.name.trim(), vlan: draft.vlan,
        cidr: draft.cidr.trim(), zone: draft.zone.trim(), dhcp: {
          enabled: draft.dhcp.enabled,
          start: draft.dhcp.start,
          limit: draft.dhcp.limit,
          leasetime: draft.dhcp.leasetime.trim(),
        }, enabled: draft.enabled,
      })
      onChanged()
      onClose()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <SlideOver title={n.name} onClose={onClose}>
      <div style={{ display: 'grid', gap: 14 }}>
        {err && <div role="alert"><Banner tone="critical">{err}</Banner></div>}
        <Card title="Network">
          <div style={{ display: 'grid', gap: 8 }}>
            <Field
              label="Name"
              value={draft.name}
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            />
            <Field
              label="VLAN"
              type="number"
              value={draft.vlan}
              onChange={(e) => setDraft({ ...draft, vlan: Number(e.target.value) })}
            />
            <Field
              label="Firewall zone"
              value={draft.zone}
              onChange={(e) => setDraft({ ...draft, zone: e.target.value })}
            />
            {warning && (
              <div style={{ fontSize: 11, color: 'var(--warning)' }}>{warning}</div>
            )}
            {policyWarning && (
              <div role="note" style={{ fontSize: 11, color: 'var(--warning)' }}>
                {policyWarning}
              </div>
            )}
            <Toggle
              label="Enabled"
              on={draft.enabled}
              onChange={(v) => setDraft({ ...draft, enabled: v })}
            />
          </div>
        </Card>

        <Card title="IPv4">
          <div style={{ display: 'grid', gap: 8 }}>
            <Field
              label="Address"
              placeholder="10.0.20.1/24"
              value={draft.cidr}
              onChange={(e) => setDraft({ ...draft, cidr: e.target.value })}
            />
            {draft.cidr.trim() !== '' && !facts && addressWarning && (
              <div role="alert" style={{ fontSize: 11, color: 'var(--warning)' }}>
                Saving is blocked because this is not an IPv4 network in CIDR
                form. Write it as address/prefix, for example
                "10.0.{draft.vlan}.1/24".
              </div>
            )}
            {draft.cidr.trim() === '' && addressWarning && (
              <div role="alert" style={{ fontSize: 11, color: 'var(--warning)' }}>
                {addressWarning} Saving is blocked until this is fixed.
              </div>
            )}
            {facts && (
              <div style={{ display: 'grid', gap: 3, fontSize: 11 }}>
                {facts.map((f) => (
                  <div key={f.label} style={{ display: 'flex', gap: 8 }}>
                    <span style={{ color: 'var(--text-secondary)', width: 110 }}>
                      {f.label}
                    </span>
                    <span>{f.value}</span>
                  </div>
                ))}
                <div style={{ color: 'var(--text-muted)', marginTop: 4 }}>
                  Derived from the address and the DHCP settings below.
                </div>
              </div>
            )}
            {facts && addressWarning && (
              <div role="alert" style={{ fontSize: 11, color: 'var(--warning)' }}>
                {addressWarning} Saving is blocked until this is fixed.
              </div>
            )}
          </div>
        </Card>

        {managesDHCP ? (
          <Card title="DHCP">
            <div style={{ display: 'grid', gap: 8 }}>
              <Toggle
                label="DHCP server"
                on={draft.dhcp.enabled}
                onChange={(enabled) => setDraft({
                  ...draft, dhcp: { ...draft.dhcp, enabled },
                })}
              />
              <Field
                label="Pool start"
                type="number"
                value={draft.dhcp.start}
                disabled={!draft.dhcp.enabled}
                onChange={(e) => setDraft({
                  ...draft, dhcp: { ...draft.dhcp, start: Number(e.target.value) },
                })}
              />
              <Field
                label="Pool limit"
                type="number"
                value={draft.dhcp.limit}
                disabled={!draft.dhcp.enabled}
                onChange={(e) => setDraft({
                  ...draft, dhcp: { ...draft.dhcp, limit: Number(e.target.value) },
                })}
              />
              <Field
                label="Lease time"
                placeholder="12h"
                value={draft.dhcp.leasetime}
                disabled={!draft.dhcp.enabled}
                onChange={(e) => setDraft({
                  ...draft, dhcp: { ...draft.dhcp, leasetime: e.target.value },
                })}
              />
              {legacyDHCPWarning ? (
                <div role="alert" style={{ fontSize: 11, color: 'var(--warning)' }}>
                  {legacyDHCPWarning}
                </div>
              ) : dhcpWarning && (
                <div role="alert" style={{ fontSize: 11, color: 'var(--warning)' }}>
                  {dhcpWarning} Applying is blocked until this is fixed.
                </div>
              )}
            </div>
          </Card>
        ) : (
          <Card title="DHCP">
            <div role="note" style={{ fontSize: 11, color: 'var(--text-muted)', lineHeight: 1.5 }}>
              VLAN 0 and 1 are the router’s existing management LAN. The controller
              leaves their addressing and DHCP server untouched so adopting an AP
              cannot take over or interrupt its management network. Configure this
              DHCP server on the router itself.
            </div>
          </Card>
        )}

        <div style={{ display: 'flex', gap: 8 }}>
          <Button disabled={
            !dirty || busy || !draft.name.trim() || !!addressWarning || !!dhcpWarning
          } onClick={save}>
            {busy ? 'Saving…' : 'Save'}
          </Button>
          <Button onClick={onClose}>Cancel</Button>
        </div>
      </div>
    </SlideOver>
  )
}

function Networks({ site, onChanged }: { site: Site; onChanged: () => void }) {
  const [editing, setEditing] = useState<SiteNetwork | null>(null)
  const [draft, setDraft] = useState({ name: '', vlan: 1, cidr: '', zone: '' })
  const draftWarning = zoneWarning(draft.zone, draft.name, draft.vlan)
  // Spelled out under the card, not only inside a tooltip. A hover on a glyph
  // is not where the only copy of an instruction belongs.
  const zoneProblems = site.networks
    .map((n) => zoneWarning(n.zone, n.name, n.vlan))
    .filter((w): w is string => w !== null)
  return (
    <Card title="Networks">
      {editing && (
        <NetworkEditor
          n={editing}
          zones={site.zones ?? []}
          onClose={() => setEditing(null)}
          onChanged={onChanged}
        />
      )}
      <div style={{ display: 'grid', gap: 10 }}>
        {/* The one grid, per UI-SPEC §5 — the same component the clients and
            devices screens use. This was a stack of flex rows with the fields
            inline, so nothing lined up between rows and the columns had no
            names: an operator had to infer that the bare number after the name
            was a VLAN. */}
        <DataGrid
          rows={site.networks}
          rowKey={(n) => String(n.id)}
          onRowClick={(n) => setEditing(n)}
          empty="No networks yet. Add one below."
          columns={[
            {
              key: 'name',
              header: 'Name',
              required: true,
              render: (n) => (
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                  <span
                    title={n.enabled ? 'enabled' : 'disabled'}
                    style={{
                      width: 6, height: 6, borderRadius: '50%',
                      background: n.enabled ? 'var(--good)' : 'var(--text-muted)',
                    }}
                  />
                  <strong>{n.name}</strong>
                </span>
              ),
              sortBy: (n) => n.name,
            },
            {
              key: 'vlan',
              header: 'VLAN',
              numeric: true,
              width: 80,
              render: (n) => n.vlan,
              sortBy: (n) => n.vlan,
            },
            {
              key: 'subnet',
              header: 'Subnet',
              width: 150,
              render: (n) =>
                n.cidr || <Unknown why="no address is set, so this network gets a VLAN and no addressing" />,
              sortBy: (n) => n.cidr ?? '',
            },
            {
              key: 'zone',
              header: 'Firewall zone',
              width: 240,
              render: (n) => (
                <NetworkZone n={n} zones={site.zones ?? []} onChanged={onChanged} />
              ),
              sortBy: (n) => n.zone,
            },
            {
              key: 'actions',
              header: '',
              width: 90,
              render: (n) => (
                <Button
                  aria-label={`Delete network ${n.name}`}
                  onClick={async (event) => {
                    event.stopPropagation()
                    try {
                      await api.deleteNetwork(n.id)
                      onChanged()
                    } catch (e) {
                      alert(e instanceof Error ? e.message : String(e))
                    }
                  }}
                >
                  Delete
                </Button>
              ),
            },
          ]}
        />
        <div style={{ display: 'flex', gap: 6, alignItems: 'flex-end', flexWrap: 'wrap' }}>
          <div style={{ width: 130 }}>
            <Field
              label="Name"
              value={draft.name}
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            />
          </div>
          <div style={{ width: 90 }}>
            <Field
              label="VLAN"
              type="number"
              value={draft.vlan}
              onChange={(e) => setDraft({ ...draft, vlan: Number(e.target.value) })}
            />
          </div>
          <div style={{ width: 160 }}>
            <Field
              label="Address"
              placeholder="192.168.1.1/24"
              value={draft.cidr}
              onChange={(e) => setDraft({ ...draft, cidr: e.target.value })}
            />
          </div>
          <div style={{ width: 130 }}>
            <Field
              label="Firewall zone"
              placeholder="same as the name"
              value={draft.zone}
              onChange={(e) => setDraft({ ...draft, zone: e.target.value })}
            />
          </div>
          <Button
            disabled={!draft.name.trim()}
            onClick={async () => {
              await api.saveNetwork({
                ...draft, name: draft.name.trim(),
                zone: draft.zone.trim(), dhcp: { ...DEFAULT_DHCP }, enabled: true,
              })
              setDraft({ name: '', vlan: 1, cidr: '', zone: '' })
              onChanged()
            }}
          >
            Add
          </Button>
        </div>
        {draftWarning && (
          <div style={{ fontSize: 11, color: 'var(--warning)' }}>{draftWarning}</div>
        )}
        {zoneProblems.map((w) => (
          <div key={w} style={{ fontSize: 11, color: 'var(--warning)' }}>
            {w}
          </div>
        ))}
        <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          A network is the L2/L3 segment a WLAN puts clients on. For a simple
          setup one network named <code>lan</code> on VLAN 1 is enough.
        </div>
      </div>
    </Card>
  )
}

/** The per-device diff an operator reads before applying anything. */
function Preview({ p }: { p: PreviewResult }) {
  if (p.site_errors && p.site_errors.length > 0) {
    return (
      <div role="alert" style={{ marginTop: 12 }}>
        <Banner tone="critical">
          No device was checked, because the configuration itself is not valid:
          <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
            {p.site_errors.map((e) => (
              <li key={e}>{e}</li>
            ))}
          </ul>
        </Banner>
      </div>
    )
  }
  const total = p.devices.reduce((n, d) => n + d.changes.length, 0)
  return (
    <div style={{ marginTop: 12, display: 'grid', gap: 10 }}>
      <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
        {p.devices.length} device{p.devices.length === 1 ? '' : 's'} checked ·{' '}
        {total} change{total === 1 ? '' : 's'} pending
        {p.devices.length === 0
          ? ' — no adopted devices to compare or apply'
          : total === 0 && ' — every device already matches the site model'}
      </div>
      {p.devices.map((d) => (
        <div
          key={d.device_id}
          style={{ display: 'grid', gap: 6 }}
        >
          <Notice
            tone={d.error || d.blocked || d.driver_defects?.some((f) => f.wlan && f.severity === 'radio-death')
              ? 'critical'
              : d.cautions?.length || d.touches_traversal || d.drift?.length
                ? 'warning'
                : 'accent'}
            component={`Apply preview · ${d.name}`}
            summary={(
              <div>
                <div>
                  {d.name}{' '}
                  <span style={{ color: 'var(--text-muted)', fontWeight: 400 }}>
                    · {previewFunctionNames(d.functions, d.role)}
                  </span>
                </div>
                <div style={{ color: 'var(--text-secondary)', fontSize: 11, fontWeight: 400 }}>
                  {previewBucketSummary(d)}
                </div>
              </div>
            )}
            closedLabel={`Show technical details for ${d.name}`}
            openLabel={`Hide technical details for ${d.name}`}
            details={<PreviewDeviceDetails d={d} />}
          />

          {d.error && <div role="alert"><Banner tone="critical">{d.error}</Banner></div>}

          {d.blocked && (
            <Banner tone="critical">
              Nothing will be applied to this device: something a person owns is
              in the way.
              <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
                {d.conflicts?.map((c) => (
                  <li key={c}>{c}</li>
                ))}
              </ul>
            </Banner>
          )}

          {d.driver_defects?.filter((f) => f.wlan).map((f) => (
            <div role="alert" key={`${f.defect_id}.${f.wlan}`}>
              <Banner tone={f.severity === 'radio-death' ? 'critical' : 'warning'}>
                {driverDefectConsequence(d.name, f)}
              </Banner>
            </div>
          ))}

          {/* Three lists, because they were one and the heading was true of
              about a fifth of it. A layer-2 loop warning and a section kept in
              place because we could not see the device were both being shown
              as "not an error — the hardware cannot take it", the first in
              muted grey and the second saying the reverse of what happened. */}
          {d.cautions && d.cautions.length > 0 && (
            <Banner tone="warning">
              These will be applied, and are worth a look first:
              <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
                {d.cautions.map((o) => (
                  <li key={o}>{o}</li>
                ))}
              </ul>
            </Banner>
          )}

          {d.touches_traversal && (
            <div style={{ fontSize: 11, color: 'var(--warning)', marginTop: 4 }}>
              Edits this device's network or firewall configuration.
            </div>
          )}

          {d.drift && d.drift.length > 0 && (
            <div style={{ marginTop: 6 }}>
              <Banner tone="warning">
                Someone edited config we own on this device. Applying will put it
                back to the site model.
                <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
                  {d.drift.map((x) => (
                    <li key={x}>{x}</li>
                  ))}
                </ul>
              </Banner>
            </div>
          )}
        </div>
      ))}
    </div>
  )
}

type PreviewDevice = PreviewResult['devices'][number]
type PreviewDriverDefect = NonNullable<PreviewDevice['driver_defects']>[number]

function driverDefectConsequence(device: string, defect: PreviewDriverDefect): string {
  const subject = `${defect.wlan}: ${defect.summary}`
  if (defect.severity === 'radio-death') {
    return `${subject} — Critical: This plan can take ${device}'s radio down until someone physically power-cycles the device. Rollback cannot cover a radio that stops answering.`
  }
  if (defect.severity === 'silently-ignored') {
    return `${subject} — Silently ignored: Apply will write this setting, but the driver is known not to honour it.`
  }
  return `${subject} — Degraded behavior: Apply will write this setting, but the driver is known to degrade the requested behavior.`
}

function previewBucketSummary(d: PreviewDevice): string {
  const buckets = [
    ...(d.error ? ['planning error'] : []),
    ...(d.blocked ? ['blocked'] : []),
    `${d.changes.length} planned change${d.changes.length === 1 ? '' : 's'}`,
    ...(d.cautions?.length
      ? [`${d.cautions.length} caution${d.cautions.length === 1 ? '' : 's'}`]
      : []),
    ...(d.driver_defects?.length
      ? [`${d.driver_defects.length} driver notice${d.driver_defects.length === 1 ? '' : 's'}`]
      : []),
    ...(d.omitted?.length
      ? [`${d.omitted.length} omission${d.omitted.length === 1 ? '' : 's'}`]
      : []),
    ...(d.undetermined?.length
      ? [`${d.undetermined.length} undetermined item${d.undetermined.length === 1 ? '' : 's'}`]
      : []),
    ...(d.capability_cause?.changes.length
      ? [`${d.capability_cause.changes.length} capability change${d.capability_cause.changes.length === 1 ? '' : 's'}`]
      : []),
    ...(d.deviations?.length
      ? [`${d.deviations.length} intentional deviation${d.deviations.length === 1 ? '' : 's'}`]
      : []),
    ...(d.drift?.length
      ? [`${d.drift.length} drift item${d.drift.length === 1 ? '' : 's'}`]
      : []),
    ...(d.touches_traversal ? ['management path affected'] : []),
  ]
  return buckets.join(' · ')
}

function PreviewDeviceDetails({ d }: { d: PreviewDevice }) {
  return (
    <div style={{ display: 'grid', gap: 8, fontSize: 11 }}>
      {d.changes.length === 0 ? (
        <div>No configuration changes were planned for this device.</div>
      ) : (
        <div>
          <strong>Exact configuration changes</strong>
          <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
            {d.changes.map((c, i) => (
              <li key={`${i}-${c.config}.${c.section}`}>
                <span
                  style={{
                    color:
                      c.action === 'remove' && !c.option
                        ? 'var(--critical)'
                        : c.action === 'create'
                          ? 'var(--good)'
                          : 'var(--text-primary)',
                  }}
                >
                  {c.action === 'remove' && c.option ? 'clear' : c.action}
                </span>{' '}
                <code>
                  {c.config}.{c.section}
                  {c.option ? `.${c.option}` : ''}
                </code>
                {c.options && c.options.length > 0 && (
                  <span style={{ color: 'var(--text-muted)' }}>
                    {' '}— {c.options.length} option{c.options.length === 1 ? '' : 's'}
                    {c.touches_key && ', including the passphrase'}
                  </span>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}

      {d.driver_defects && d.driver_defects.length > 0 && (
        <div>
          <strong>Driver evidence</strong>
          {d.driver_defects.map((f) => (
            <div
              key={`${f.defect_id}.${f.wlan ?? ''}`}
              style={{ borderLeft: '2px solid var(--warning)', paddingLeft: 8, marginTop: 6 }}
            >
              <div style={{ color: 'var(--warning)' }}>
                {f.wlan ? `${f.wlan}: technical evidence` : `This hardware: ${f.summary}`}
                <span
                  style={{ color: 'var(--text-muted)', marginLeft: 6 }}
                  title={
                    f.confidence === 'documented'
                      ? "From the device's own OpenWrt page, its driver documentation, or a maintainer"
                      : f.confidence === 'measured'
                        ? 'Reproduced on hardware by this project'
                        : f.confidence === 'reported'
                          ? 'A filed, accepted bug report'
                          : 'Repeated in forums with no primary source found — treat as a lead, not a fact'
                  }
                >
                  [{f.confidence}]
                </span>
              </div>
              <div style={{ color: 'var(--text-muted)' }}>{f.detail}</div>
              {f.mitigation && (
                <div style={{ color: 'var(--text-muted)' }}>
                  What to do instead: {f.mitigation}
                </div>
              )}
              {f.source && <div style={{ color: 'var(--text-muted)' }}>Source: {f.source}</div>}
            </div>
          ))}
        </div>
      )}

      {d.omitted && d.omitted.length > 0 && (
        <div>
          <strong>Not rendered on this device</strong>
          <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
            {d.omitted.map((item) => <li key={item}>{item}</li>)}
          </ul>
        </div>
      )}

      {d.undetermined && d.undetermined.length > 0 && (
        <div>
          <strong>Could not be determined, so nothing here was changed or removed</strong>
          <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
            {d.undetermined.map((item) => <li key={item}>{item}</li>)}
          </ul>
        </div>
      )}

      {d.capability_cause && (
        <div>
          <strong>
            This device&apos;s capabilities changed {ago(d.capability_cause.at)},
            which may explain what is missing
          </strong>
          <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
            {d.capability_cause.changes.map((item) => <li key={item}>{item}</li>)}
          </ul>
        </div>
      )}

      {d.deviations && d.deviations.length > 0 && (
        <div>
          <strong>This device intentionally deviates from the site model</strong>
          <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
            {d.deviations.map((item) => <li key={item}>{item}</li>)}
          </ul>
        </div>
      )}
    </div>
  )
}

function previewFunctionNames(functions: DeviceFunction[] | undefined, role: string): string {
  const selected = functions !== undefined
    ? functions
    : role === 'gateway'
      ? ['gateway', 'ap', 'switch'] as DeviceFunction[]
      : role === 'ap'
        ? ['ap', 'switch'] as DeviceFunction[]
        : role === 'switch'
          ? ['switch'] as DeviceFunction[]
          : []
  if (selected.length === 0) return 'None — invalid record'
  const labels: Record<DeviceFunction, string> = {
    gateway: 'Gateway',
    ap: 'AP',
    switch: 'Switch',
  }
  return selected.map((item) => labels[item]).join(' · ')
}

// ---- small controls ----

function Choice({
  label,
  options,
  value,
  onChange,
  multi,
}: {
  label: string
  options: { v: string; l: string }[]
  value: string[]
  onChange: (v: string[]) => void
  multi?: boolean
}) {
  return (
    <div>
      <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 4 }}>
        {label}
      </div>
      <div
        role="group"
        aria-label={label}
        style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}
      >
        {options.map((o) => {
          const on = value.includes(o.v)
          return (
            <button
              key={o.v}
              type="button"
              aria-pressed={on}
              onClick={() =>
                onChange(
                  multi
                    ? on
                      ? value.filter((x) => x !== o.v)
                      : [...value, o.v]
                    : [o.v],
                )
              }
              style={{
                fontSize: 12,
                padding: '4px 10px',
                borderRadius: 4,
                cursor: 'pointer',
                border: '1px solid var(--border-strong)',
                background: on ? 'var(--accent-soft)' : 'transparent',
                color: 'var(--text-primary)',
              }}
            >
              {o.l}
            </button>
          )
        })}
      </div>
    </div>
  )
}

/** One mesh in the list. */
function MeshRow({
  m,
  site,
  onEdit,
  onDeleted,
}: {
  m: Mesh
  site: Site
  onEdit: () => void
  onDeleted: () => Promise<void>
}) {
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [deleteError, setDeleteError] = useState('')
  const group = site.groups.find((g) => g.id === m.group_id)
  const net = site.networks.find((n) => n.id === m.network_id)

  return (
    <div
      style={{
        padding: '10px 14px',
        borderTop: '1px solid var(--border)',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <div style={{ flex: 1 }}>
          <div style={{ fontSize: 13, fontWeight: 600 }}>
            {m.mesh_id}
            {!m.enabled && (
              <span style={{ color: 'var(--text-muted)', fontWeight: 400, fontSize: 11 }}>
                {' '}· disabled
              </span>
            )}
          </div>
          <div style={{ fontSize: 11, color: 'var(--text-secondary)' }}>
            {m.band} · {group?.name ?? 'no group'} · {net?.name ?? 'no network'} ·{' '}
            {m.has_key ? (
              'encrypted (SAE)'
            ) : (
              <span style={{ color: 'var(--warning)' }}>
                open — anyone in range can join
              </span>
            )}
          </div>
        </div>
        <Button aria-label={`Edit mesh ${m.mesh_id}`} disabled={deleting} onClick={onEdit}>
          Edit
        </Button>
        <Button
          aria-label={`Delete mesh ${m.mesh_id}`}
          disabled={deleting}
          onClick={() => {
            setDeleteError('')
            setConfirmDelete(true)
          }}
        >
          Delete
        </Button>
      </div>
      {confirmDelete && (
        <div style={{ marginTop: 10 }}>
          <Banner tone="warning">
            <div style={{ display: 'grid', gap: 8 }}>
              <span>
                Delete <strong>{m.mesh_id}</strong> from desired state? No router changes
                until you Preview and Apply.
              </span>
              <div style={{ display: 'flex', gap: 8 }}>
                <Button
                  disabled={deleting}
                  onClick={async () => {
                    setDeleting(true)
                    setDeleteError('')
                    try {
                      await api.deleteMesh(m.id)
                      await onDeleted()
                    } catch (e) {
                      setDeleteError(e instanceof Error ? e.message : String(e))
                    } finally {
                      setDeleting(false)
                    }
                  }}
                >
                  {deleting ? 'Deleting…' : `Delete “${m.mesh_id}”`}
                </Button>
                <Button disabled={deleting} onClick={() => setConfirmDelete(false)}>
                  Keep “{m.mesh_id}”
                </Button>
              </div>
            </div>
          </Banner>
        </div>
      )}
      {deleteError && <div role="alert" style={{ marginTop: 8 }}><Banner tone="critical">{deleteError}</Banner></div>}
    </div>
  )
}

/**
 * The mesh editor.
 *
 * The passphrase starts empty on an edit and is only sent when typed — the
 * server treats an empty key on an update as "leave it alone". That matters
 * more here than for a WLAN: if an empty key meant "open", renaming a mesh
 * would silently drop its encryption, and an open mesh is joinable by anyone in
 * radio range with access to the network behind it.
 */
function MeshEditor({
  m,
  site,
  onClose,
  onSaved,
}: {
  m: Partial<Mesh>
  site: Site
  onClose: () => void
  onSaved: () => void
}) {
  const [draft, setDraft] = useState<Partial<Mesh>>({ ...m, key: '', clear_key: false })
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState('')
  const set = (patch: Partial<Mesh>) => setDraft((d) => ({ ...d, ...patch }))

  // An existing mesh keeps its stored key unless one is typed. A NEW one with
  // no key really is open, and says so rather than letting it pass unremarked.
  const willBeOpen = !!draft.clear_key || (m.id ? !m.has_key && !draft.key : !draft.key)

  async function save() {
    if (!(draft.mesh_id ?? '').trim()) return
    setSaving(true)
    try {
      await api.saveMesh(draft)
      onSaved()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card title={m.id ? `Edit ${m.mesh_id}` : 'New mesh backhaul'}>
      <div style={{ display: 'grid', gap: 12, maxWidth: 460 }}>
        {err && <div role="alert"><Banner tone="critical">{err}</Banner></div>}

        <Field
          label="Mesh ID"
          value={draft.mesh_id ?? ''}
          placeholder="e.g. backhaul"
          onChange={(e) => set({ mesh_id: e.target.value })}
        />
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: -8 }}>
          Not an SSID — it is not broadcast for clients. Nodes match on it to
          peer, so every device in the mesh must use the same one.
        </div>

        <Choice
          label="Band"
          options={[
            { v: '2g', l: '2.4 GHz' },
            { v: '5g', l: '5 GHz' },
            { v: '6g', l: '6 GHz' },
          ]}
          value={[draft.band ?? '5g']}
          onChange={([b]) => set({ band: b as Mesh['band'] })}
        />
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: -8 }}>
          One band, not several. Nodes peer only with nodes on the same band, so
          the same mesh on two bands would be two separate backhauls that cannot
          see each other.
        </div>

        <Field
          label={m.id ? 'Passphrase (leave blank to keep the current one)' : 'Passphrase'}
          type="password"
          value={draft.key ?? ''}
          disabled={!!draft.clear_key}
          placeholder={m.has_key ? '••••••••' : 'blank leaves the mesh open'}
          onChange={(e) => set({ key: e.target.value, clear_key: false })}
        />
        {m.id && m.has_key && (
          <Toggle
            label="Remove the passphrase and make this mesh open"
            on={!!draft.clear_key}
            onChange={(v) => set({ clear_key: v, key: v ? '' : draft.key })}
          />
        )}
        {willBeOpen && (
          <Banner tone="warning">
            With no passphrase this mesh is open: any device in radio range can
            peer with it and reach the network behind it. Encrypted meshes use
            WPA3-SAE.
          </Banner>
        )}

        <Choice
          label="Network"
          options={site.networks.map((n) => ({ v: String(n.id), l: n.name }))}
          value={[String(draft.network_id ?? '')]}
          onChange={([v]) => set({ network_id: Number(v) })}
        />
        <Choice
          label="AP group"
          options={site.groups.map((g) => ({ v: String(g.id), l: g.name }))}
          value={[String(draft.group_id ?? '')]}
          onChange={([v]) => set({ group_id: Number(v) })}
        />
        <Toggle
          label="Enabled"
          on={draft.enabled ?? true}
          onChange={(v) => set({ enabled: v })}
        />

        <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          Devices carrying this mesh keep serving clients and their wired ports.
          A mesh point is an extra interface, not a different kind of device.
        </div>

        <div style={{ display: 'flex', gap: 8 }}>
          <Button kind="primary" onClick={save} disabled={saving || !(draft.mesh_id ?? '').trim()}>
            {saving ? 'Saving…' : 'Save'}
          </Button>
          <Button onClick={onClose}>Cancel</Button>
        </div>
        <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          Saving changes nothing on any device. Preview and apply is what writes.
        </div>
      </div>
    </Card>
  )
}

/**
 * Neighbour reports — the one thing a controller can do that hand configuration
 * cannot.
 *
 * An AP knows its own BSS and nothing about the AP down the hall; the two never
 * talk. So 802.11k, which lets a client ask "what else is around?" and scan
 * three channels instead of all of them, is switched on across the fleet and
 * answered with an empty list unless something tells each AP about the others.
 * That something has to know the whole fleet, which is this.
 *
 * The card runs on demand and reports what it did. It is not a settings form:
 * there is nothing to configure, because every input is either the site model
 * (which WLANs asked for 802.11k) or the devices themselves (where their radios
 * currently are).
 */
function Neighbours({ site }: { site: Site }) {
  const [res, setRes] = useState<NeighbourResult | null>(null)
  // What the AUTOMATIC cycle last did. The card used to show nothing until
  // somebody pressed the button, on a feature whose own description says it
  // runs every fifteen minutes — so the only way to learn whether 802.11k was
  // working was to trigger it, which is not an observation.
  const [last, setLast] = useState<{
    ran: boolean
    at?: number
    error?: string
    devices_failed?: number
    // The full result, so the card can SHOW why a device failed instead of
    // guessing. api.lastNeighbours already returns it; the state type used to
    // discard it, and the summary then asserted a cause it had no evidence for.
    result?: NeighbourResult
  } | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  // What the card can say before it has run. Derived from the site model rather
  // than from a device, so it costs nothing and is honest about being a
  // statement of intent: the WLANs that ASKED for 802.11k, which is not the
  // same as the APs that can carry it.
  const asked = site.wlans.filter((w) => w.enabled && w.roaming.kv).map((w) => w.ssid)

  useEffect(() => {
    // Read-only: reports the last cycle, never triggers one. A card that had to
    // run the thing to tell you about it would make observing it change it.
    api
      .lastNeighbours()
      .then(setLast)
      .catch(() => {})
  }, [])

  async function run() {
    setBusy(true)
    try {
      setRes(await api.distributeNeighbours())
      setErr('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card
      title="Neighbour reports (802.11k)"
      actions={
        <Button onClick={run} disabled={busy || asked.length === 0}>
          {busy ? 'Distributing…' : 'Distribute now'}
        </Button>
      }
    >
      {err && <div role="alert"><Banner tone="critical">{err}</Banner></div>}

      <div style={{ marginBottom: 10 }}>
        <Notice
          tone="accent"
          component="802.11k neighbour reports"
          summary="Automatically refreshes neighbour reports every 15 minutes and after every Apply."
          closedLabel="More information about neighbour reports"
          openLabel="Hide neighbour report information"
          details="Each access point is told the BSSIDs and channels of the others carrying the same SSID, so a roaming client scans those channels instead of all of them. No AP can learn this by itself. An AP that restarts comes back with an empty list, so it is re-checked rather than assumed."
        />
      </div>

      {asked.length === 0 ? (
        <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
          No wireless network has neighbour reports switched on, so there is
          nothing to distribute. Turn on <strong>802.11k/v</strong> for a network
          above to use this.
        </div>
      ) : (
        <Prop label="Networks">{asked.join(', ')}</Prop>
      )}

      {/* The state on arrival, before anyone presses anything. */}
      {!res && last && asked.length > 0 && (
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 8 }}>
          {!last.ran
            ? 'No cycle has run since the controller started. The first lands within 15 minutes.'
            : last.error
              ? `Last automatic run failed: ${last.error}`
              : last.devices_failed
                ? // Cause-neutral. The reasons are printed below, verbatim.
                  // This line used to say "could not be reached", which sent an
                  // operator to check cabling for a device that was powered,
                  // on the network, answering, and refusing one ubus call —
                  // where the actual remedy is to re-adopt so its ACL is
                  // rewritten.
                  `Last automatic run ${ago(last.at ?? 0)}, but ${last.devices_failed} device${last.devices_failed === 1 ? '' : 's'} reported an error.`
                : `Last automatic run ${ago(last.at ?? 0)}, with no errors.`}
        </div>
      )}

      {/* The reasons, verbatim, from the same row component the on-demand path
          uses. A count with a guessed cause sends people to the wrong place:
          an ACL narrowed by a sysupgrade and a device that is genuinely off
          the network produce the same number and need opposite responses. */}
      {!res && last?.ran && (last.devices_failed ?? 0) > 0 && last.result && (
        <div style={{ marginTop: 6, display: 'grid', gap: 6 }}>
          {last.result.devices
            .filter((d) => d.error)
            .map((d) => (
              <NeighbourDeviceRow key={d.device_id} d={d} />
            ))}
        </div>
      )}

      {res && (
        <div style={{ marginTop: 10, display: 'grid', gap: 8 }}>
          <div style={{ fontSize: 12 }}>
            {res.updated > 0
              ? `Updated ${res.updated} access point radio${res.updated === 1 ? '' : 's'}`
              : 'Every access point was already up to date'}
            {res.unchanged > 0 && (
              <span style={{ color: 'var(--text-muted)' }}>
                {' '}
                · {res.unchanged} already correct
              </span>
            )}
          </div>
          {res.note && (
            <div style={{ fontSize: 11, color: 'var(--text-secondary)' }}>{res.note}</div>
          )}
          {res.devices.map((d) => (
            <NeighbourDeviceRow key={d.device_id} d={d} />
          ))}
        </div>
      )}
    </Card>
  )
}

function NeighbourDeviceRow({ d }: { d: NeighbourDevice }) {
  return (
    <div
      style={{
        border: '1px solid var(--border)',
        borderRadius: 6,
        padding: '6px 8px',
        fontSize: 12,
      }}
    >
      <div style={{ fontWeight: 600 }}>{d.name}</div>
      {/* A failure and a standing limitation are rendered differently on
          purpose. "Could not reach this device" is something to go and fix now;
          "this device was adopted before the controller could ask" is a fact
          about the device that will not change until it is re-adopted. Colouring
          both red teaches people to ignore red. */}
      {d.error && (
        <div style={{ color: 'var(--critical)', marginTop: 2 }}>{d.error}</div>
      )}
      {d.skipped && (
        <div style={{ color: 'var(--text-muted)', marginTop: 2 }}>{d.skipped}</div>
      )}
      {d.bsses?.map((b) => (
        <div
          key={b.iface}
          style={{
            display: 'flex',
            gap: 8,
            marginTop: 3,
            color: b.failed ? 'var(--critical)' : 'var(--text-secondary)',
          }}
        >
          <code style={{ minWidth: 78 }}>{b.iface}</code>
          <span style={{ minWidth: 120 }}>{b.ssid}</span>
          <span>
            {b.failed
              ? b.failed
              : `knows ${b.neighbours} neighbour${b.neighbours === 1 ? '' : 's'}`}
          </span>
          {b.changed && !b.failed && (
            <span style={{ color: 'var(--text-muted)' }}>updated</span>
          )}
        </div>
      ))}
    </div>
  )
}

/**
 * What each configured backhaul is actually doing.
 *
 * Deliberately a separate card from the mesh editor above it. That one is
 * desired state — what you want built. This is observed state — what is
 * running, or why nothing is. Merging them puts a green tick next to a form
 * field and invites the reading that saving the form made it true.
 *
 * The rule this card exists to hold: it switches on `state` and never decides
 * for itself what the other fields mean together. A screen that reads a null
 * peer count as zero is a second implementation of the controller's health
 * logic, and the two drift — which is how this project once had two screens
 * answering one question two different ways.
 */
function MeshHealth() {
  const [res, setRes] = useState<MeshHealthResult | null>(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    let live = true
    const load = async () => {
      try {
        const r = await api.meshHealth()
        if (live) {
          setRes(r)
          setErr('')
        }
      } catch (e) {
        if (live) setErr(e instanceof Error ? e.message : String(e))
      }
    }
    load()
    // Cheap to ask: the controller reads no device to answer this, so a
    // refresh costs nothing on anyone's request budget.
    const t = setInterval(load, 30_000)
    return () => {
      live = false
      clearInterval(t)
    }
  }, [])

  if (err) return <Card title="Backhaul health"><div role="alert"><Banner tone="critical">{err}</Banner></div></Card>
  if (!res) return null

  return (
    <Card title="Backhaul health">
      {res.links.length === 0 ? (
        <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
          {res.note}
        </div>
      ) : (
        <div style={{ display: 'grid', gap: 8 }}>
          {res.links.map((l) => (
            <MeshLinkRow key={`${l.device_id}:${l.mesh_id}`} l={l} />
          ))}
        </div>
      )}
    </Card>
  )
}

/** The tone-to-colour mapping, in one place. The controller decides the tone
 *  alongside the state, so a screen cannot disagree with another screen about
 *  how serious the same fact is. */
const meshTone: Record<string, string> = {
  ok: 'var(--ok, #3fb950)',
  normal: 'var(--text-secondary)',
  muted: 'var(--text-muted)',
  warning: 'var(--warning)',
  critical: 'var(--critical)',
}

function MeshLinkRow({ l }: { l: MeshLink }) {
  return (
    <div
      style={{
        border: '1px solid var(--border)',
        borderLeft: `3px solid ${meshTone[l.tone] ?? 'var(--border)'}`,
        borderRadius: 6,
        padding: '6px 8px',
        fontSize: 12,
      }}
    >
      <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8 }}>
        <span style={{ fontWeight: 600 }}>
          {l.name}{' '}
          <span style={{ fontWeight: 400, color: 'var(--text-muted)' }}>
            on {l.device_name}
            {l.iface ? ` · ${l.iface}` : ''}
          </span>
        </span>
        <span style={{ color: meshTone[l.tone] ?? 'inherit' }}>{l.state}</span>
      </div>
      <div style={{ color: 'var(--text-secondary)', marginTop: 2 }}>{l.reason}</div>
      {/* Peers render only when they were counted. `null` means nobody looked,
          and drawing "0 peers" for it would be the exact lie the state
          vocabulary exists to prevent. */}
      {l.peers && l.peers.length > 0 && (
        <div style={{ marginTop: 4, display: 'grid', gap: 2 }}>
          {l.peers.map((p) => (
            <div key={p.mac} style={{ color: 'var(--text-muted)' }}>
              <code>{p.mac}</code>
              {p.plink ? ` · ${p.plink}` : ' · link state not reported'}
              {p.signal_dbm != null ? ` · ${p.signal_dbm} dBm` : ''}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

/**
 * Wireless uplinks: devices that reach the network over the air.
 *
 * For the router in the room with no ethernet run to it — the awkward half of
 * "extend your network with hardware you already own", and the case a mesh
 * cannot cover when one end's driver refuses 802.11s.
 *
 * Two things this card insists on saying, because they are the two the
 * controller cannot check for anyone. A device bridged into a network it is
 * ALSO cabled to is a layer-2 loop, and OpenWrt bridges ship with STP off so
 * nothing breaks it. And once applied, that station is how the controller
 * reaches the device — so removing it later is editing the road while driving
 * down it.
 */
function Uplinks({
  site,
  devices,
  onChanged,
}: {
  site: Site
  devices: Device[]
  onChanged: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [note, setNote] = useState('')

  // Only networks that actually accept bridges. Offering the others would let
  // someone build the exact configuration whose failure mode is indisting-
  // uishable from a driver refusing 4-address frames.
  const bridgeable = site.wlans.filter((w) => w.enabled && w.allow_uplink)

  async function save(u: Partial<Uplink> & { id?: number }) {
    setBusy(true)
    try {
      const res = await api.saveUplink(u)
      setNote(res.note)
      setErr('')
      onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  async function remove(id: number) {
    setBusy(true)
    try {
      const res = await api.deleteUplink(id)
      setNote(res.note)
      setErr('')
      onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const nameOf = (id: number) =>
    devices.find((d) => d.id === id)?.name ?? `device ${id}`
  const ssidOf = (id: number) => site.wlans.find((w) => w.id === id)?.ssid ?? '—'

  return (
    <Card title="Wireless uplinks">
      {err && <div role="alert"><Banner tone="critical">{err}</Banner></div>}

      <div style={{ marginBottom: 10 }}>
        <Notice
          tone="accent"
          component="Wireless uplinks"
          summary="Lets an uncabled device bridge a network over the air; that network must accept wireless bridges first."
          closedLabel="More information about wireless uplinks"
          openLabel="Hide wireless uplink information"
          details={(
            <span>
              The device joins as a 4-address bridge, putting its wired ports and
              its own access points on the network over the air. Enable{' '}
              <strong>Allow devices to join this network as a wireless bridge</strong>{' '}
              on the network itself before adding the uplink.
            </span>
          )}
        />
      </div>

      {bridgeable.length === 0 ? (
        <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
          No network accepts wireless bridges yet, so there is nothing a device
          could join. Turn that on for a network above first.
        </div>
      ) : (
        <div style={{ display: 'grid', gap: 8 }}>
          {site.uplinks.length === 0 && (
            <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
              No device is using a wireless uplink.
            </div>
          )}
          {site.uplinks.map((u) => (
            <div
              key={u.id}
              style={{
                border: '1px solid var(--border)',
                borderRadius: 6,
                padding: '6px 8px',
                fontSize: 12,
                display: 'flex',
                justifyContent: 'space-between',
                alignItems: 'center',
                gap: 8,
              }}
            >
              <span>
                <strong>{nameOf(u.device_id)}</strong>{' '}
                <span style={{ color: 'var(--text-muted)' }}>
                  joins {ssidOf(u.wlan_id)} on {u.band}
                  {u.enabled ? '' : ' (disabled)'}
                </span>
              </span>
              <Button
                aria-label={`Remove wireless uplink for ${nameOf(u.device_id)}`}
                onClick={() => remove(u.id)}
                disabled={busy}
              >
                Remove
              </Button>
            </div>
          ))}

          <UplinkAdd
            devices={devices.filter(
              (d) => !site.uplinks.some((u) => u.device_id === d.id),
            )}
            wlans={bridgeable}
            busy={busy}
            onAdd={save}
          />
        </div>
      )}

      {/* The note comes from the server rather than being restated here, so
          there is one wording of a hazard rather than two that can drift. */}
      {note && (
        <div style={{ marginTop: 10 }}>
          <Banner tone="warning">{note}</Banner>
        </div>
      )}
    </Card>
  )
}

function UplinkAdd({
  devices,
  wlans,
  busy,
  onAdd,
}: {
  devices: Device[]
  wlans: WLAN[]
  busy: boolean
  onAdd: (u: Partial<Uplink>) => void
}) {
  const [deviceID, setDeviceID] = useState(0)
  const [wlanID, setWLANID] = useState(wlans[0]?.id ?? 0)
  const [band, setBand] = useState<'2g' | '5g'>('5g')

  if (devices.length === 0) {
    return (
      <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
        Every adopted device already has an uplink, or there are none to add.
        One per device: a router with two would bridge the same network to
        itself twice, which is a loop rather than redundancy.
      </div>
    )
  }

  return (
    <div style={{ display: 'flex', gap: 8, alignItems: 'flex-end', flexWrap: 'wrap' }}>
      {/* Not <Field>: that component renders an <input>, which is a void
          element, so giving it a <select> as children throws at render and
          takes the whole screen with it. Caught by a test rather than by
          somebody opening the page, which is the first time that has happened
          in this project. */}
      <Picker label="Device" value={deviceID} onChange={(v) => setDeviceID(Number(v))}>
        <option value={0}>Choose…</option>
        {devices.map((d) => (
          <option key={d.id} value={d.id}>
            {d.name}
          </option>
        ))}
      </Picker>
      <Picker label="Joins network" value={wlanID} onChange={(v) => setWLANID(Number(v))}>
        {wlans.map((w) => (
          <option key={w.id} value={w.id}>
            {w.ssid}
          </option>
        ))}
      </Picker>
      <Picker
        label="Band"
        value={band}
        onChange={(v) => setBand(String(v) as '2g' | '5g')}
      >
        <option value="5g">5 GHz</option>
        <option value="2g">2.4 GHz</option>
      </Picker>
      <Button
        kind="primary"
        disabled={busy || !deviceID || !wlanID}
        onClick={() => onAdd({ device_id: deviceID, wlan_id: wlanID, band, enabled: true })}
      >
        Add uplink
      </Button>
    </div>
  )
}

/** A labelled <select>. Field renders an <input> and cannot take children. */
function Picker({
  label,
  value,
  onChange,
  children,
}: {
  label: string
  value: string | number
  onChange: (v: string | number) => void
  children: React.ReactNode
}) {
  return (
    <label style={{ display: 'block' }}>
      <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 4 }}>
        {label}
      </div>
      <select
        value={value}
        onChange={(e) =>
          onChange(typeof value === 'number' ? Number(e.target.value) : e.target.value)
        }
        style={{
          height: 30,
          padding: '0 8px',
          borderRadius: 6,
          background: 'var(--surface-0)',
          border: '1px solid var(--border-strong)',
          color: 'var(--text-primary)',
          fontSize: 13,
        }}
      >
        {children}
      </select>
    </label>
  )
}


/**
 * The PMF value a security mode can actually hold.
 *
 * Mirrors the renderer, which is the authority: nothing for Open, required for
 * WPA3 and Enhanced Open since both mandate it, and at least optional for
 * transitional WPA2/WPA3 where disabling it silently removes the WPA3 half of a
 * network still advertising it. Keeping the two in step matters because the
 * renderer will coerce regardless — and a form that shows one thing while the
 * device gets another is the failure this project keeps finding.
 */
function clampPMF(mode: WLAN['security_mode'], pmf: WLAN['pmf'] | undefined): WLAN['pmf'] {
  switch (mode) {
    case 'none':
      return '0'
    case 'sae':
    case 'owe':
      return '2'
    case 'sae-mixed':
      return pmf === '2' ? '2' : '1'
    default:
      return pmf ?? '1'
  }
}
