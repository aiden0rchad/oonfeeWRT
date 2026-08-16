import { useCallback, useEffect, useState } from 'react'
import type React from 'react'
import { api } from '../lib/api'
import type {
  APGroup,
  Device,
  Mesh,
  MeshHealthResult,
  Uplink,
  MeshLink,
  NeighbourDevice,
  NeighbourResult,
  PreviewResult,
  Site,
  WLAN,
} from '../lib/api'
import { Banner, Button, Card, Field, Prop } from '../components/ui'
import { ago } from '../components/Chart'

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
export function Settings({ devices }: { devices: Device[] }) {
  const [site, setSite] = useState<Site | null>(null)
  const [editing, setEditing] = useState<Partial<WLAN> | null>(null)
  const [editingMesh, setEditingMesh] = useState<Partial<Mesh> | null>(null)
  const [preview, setPreview] = useState<PreviewResult | null>(null)
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')
  const [applied, setApplied] = useState<string | null>(null)
  const [ackTraversal, setAckTraversal] = useState(false)

  const load = useCallback(async () => {
    try {
      setSite(await api.site())
      setErr('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  async function runPreview() {
    setBusy('preview')
    setApplied(null)
    try {
      setPreview(await api.preview())
      setErr('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  async function runApply() {
    setBusy('apply')
    try {
      const res = await api.applySite({ acknowledge_traversal: ackTraversal })
      const ok = res.devices.filter((d) => d.outcome === 'applied').length
      setApplied(
        res.aborted
          ? `Stopped after ${res.aborted_after}: ${
              res.devices.find((d) => d.outcome !== 'applied')?.reason ?? 'apply failed'
            }`
          : `Applied to ${ok} device${ok === 1 ? '' : 's'}.`,
      )
      // Re-preview so the screen shows the new truth rather than the plan that
      // has just stopped being pending.
      setPreview(await api.preview())
      setErr('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  if (!site) {
    return (
      <div style={{ padding: 20, fontSize: 12, color: 'var(--text-secondary)' }}>
        {err ? <Banner tone="critical">{err}</Banner> : 'Loading…'}
      </div>
    )
  }

  const pending = preview?.devices.reduce((n, d) => n + d.changes.length, 0) ?? 0
  const traversal = preview?.devices.filter((d) => d.touches_traversal) ?? []

  return (
    <div style={{ display: 'grid', gap: 14, maxWidth: 900 }}>
      {err && <Banner tone="critical">{err}</Banner>}
      {site.problems.length > 0 && (
        <Banner tone="warning">
          <strong>This configuration is not ready to apply:</strong>
          <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
            {site.problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </Banner>
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
                onDeleted={load}
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
                onDeleted={load}
              />
            ))}
          </div>
        )}
      </Card>

      <Uplinks site={site} devices={devices} onChanged={load} />
      <MeshHealth />
      <Groups site={site} devices={devices} onChanged={load} />
      <Neighbours site={site} />
      <Deviations site={site} devices={devices} onChanged={load} />
      <Networks site={site} onChanged={load} />

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
              preview.devices.some((d) => d.blocked) ||
              (traversal.length > 0 && !ackTraversal)
            }
            onClick={runApply}
          >
            {busy === 'apply' ? 'Applying…' : `Apply${pending ? ` (${pending})` : ''}`}
          </Button>
          {applied && <span style={{ fontSize: 12 }}>{applied}</span>}
        </div>
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>
          Nothing above has touched a device. Preview reads each one and reports
          what would change; apply is the only step that writes, and it stops at
          the first device that fails rather than leaving the fleet half
          converted. Every change is applied with a rollback armed — a device
          that comes back unhealthy reverts itself.
        </div>

        {/* IMPLEMENTATION §6's traversal acknowledgment. The rollback protects
            this change like any other — that is what applying with one armed is
            for — but an operator should be told they are editing the road
            before driving down it. */}
        {traversal.length > 0 && (
          <div style={{ marginTop: 10 }}>
            <Banner tone="warning">
              <div>
                This change edits the network or firewall configuration of{' '}
                <strong>{traversal.map((d) => d.name).join(', ')}</strong> — the
                path this controller reaches {traversal.length === 1 ? 'it' : 'them'}{' '}
                through.
              </div>
              <div style={{ marginTop: 4, fontSize: 11 }}>
                It is applied with a rollback armed, so a device that comes back
                unreachable restores itself within 90 seconds. You should still
                know before, not after.
              </div>
              <Toggle
                label="I understand — apply the network changes"
                on={ackTraversal}
                onChange={setAckTraversal}
              />
            </Banner>
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
            setPreview(null)
            await load()
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
            await load()
            // A saved WLAN makes the previous preview stale, and a stale
            // preview next to an Apply button is the one thing this screen
            // must never show.
            setPreview(null)
            setApplied(null)
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
  onDeleted: () => void
}) {
  const group = site.groups.find((g) => g.id === w.group_id)
  const net = site.networks.find((n) => n.id === w.network_id)
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        padding: '10px 14px',
        borderTop: '1px solid var(--border)',
      }}
    >
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
      <Button onClick={onEdit}>Edit</Button>
      <Button
        onClick={async () => {
          const res = await api.deleteWLAN(w.id)
          alert(res.note)
          onDeleted()
        }}
      >
        Delete
      </Button>
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
  const [draft, setDraft] = useState<Partial<WLAN>>({ ...w, key: '' })
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
        {err && <Banner tone="critical">{err}</Banner>}

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
          onChange={([m]) => set({ security_mode: m as WLAN['security_mode'] })}
        />

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
            label="Isolate clients from each other"
            on={!!draft.isolate}
            onChange={(v) => set({ isolate: v })}
          />
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
  onChanged: () => void
}) {
  const [name, setName] = useState('')
  const adopted = devices.filter((d) => d.adopted)

  async function toggle(g: APGroup, deviceID: number) {
    const has = g.device_ids.includes(deviceID)
    await api.saveGroup({
      ...g,
      device_ids: has
        ? g.device_ids.filter((x) => x !== deviceID)
        : [...g.device_ids, deviceID],
    })
    onChanged()
  }

  return (
    <Card
      title="AP groups"
      actions={
        <div style={{ display: 'flex', gap: 6 }}>
          <input
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
                      checked={g.device_ids.includes(d.id)}
                      onChange={() => toggle(g, d.id)}
                    />
                    {d.name}
                  </label>
                ))}
              </div>
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
  onChanged: () => void
}) {
  const [deviceID, setDeviceID] = useState<number | null>(null)
  const adopted = devices.filter((d) => d.adopted)
  const target = deviceID ?? adopted[0]?.id ?? null

  if (site.wlans.length === 0 || adopted.length === 0) return null

  const forDevice = site.overrides.filter((o) => o.device_id === target)
  const valueOf = (wlanID: number, key: string) =>
    forDevice.find((o) => o.wlan_id === wlanID && o.key === key)?.value ?? ''

  async function set(wlanID: number, key: string, value: string) {
    await api.setOverride(target!, wlanID, key, value)
    onChanged()
  }

  return (
    <Card title="Per-device overrides">
      <div style={{ display: 'grid', gap: 10 }}>
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
                onChange={(v) => set(w.id, 'disabled', v ? '1' : '')}
              />
              <Toggle
                label="Hide here"
                on={valueOf(w.id, 'hidden') === '1'}
                onChange={(v) => set(w.id, 'hidden', v ? '1' : '')}
              />
              <Toggle
                label="Isolate clients here"
                on={valueOf(w.id, 'isolate') === '1'}
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

function Networks({ site, onChanged }: { site: Site; onChanged: () => void }) {
  const [draft, setDraft] = useState({ name: '', vlan: 1, cidr: '' })
  return (
    <Card title="Networks">
      <div style={{ display: 'grid', gap: 8 }}>
        {site.networks.map((n) => (
          <div key={n.id} style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 12 }}>
            <strong>{n.name}</strong>
            <span style={{ color: 'var(--text-secondary)' }}>
              VLAN {n.vlan} · {n.cidr || 'no address'} · zone {n.zone}
            </span>
            <div style={{ flex: 1 }} />
            <Button
              onClick={async () => {
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
          </div>
        ))}
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
          <Button
            disabled={!draft.name.trim()}
            onClick={async () => {
              await api.saveNetwork({ ...draft, name: draft.name.trim(), enabled: true })
              setDraft({ name: '', vlan: 1, cidr: '' })
              onChanged()
            }}
          >
            Add
          </Button>
        </div>
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
      <div style={{ marginTop: 12 }}>
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
        {total === 0 && ' — every device already matches the site model'}
      </div>
      {p.devices.map((d) => (
        <div
          key={d.device_id}
          style={{
            border: '1px solid var(--border-strong)',
            borderRadius: 6,
            padding: '8px 10px',
            background: 'var(--surface-2)',
          }}
        >
          <div style={{ fontSize: 13, fontWeight: 600 }}>
            {d.name}
            <span style={{ color: 'var(--text-muted)', fontWeight: 400, fontSize: 11 }}>
              {' '}
              · {d.role}
            </span>
          </div>

          {d.error && <Banner tone="critical">{d.error}</Banner>}

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

          {!d.error && !d.blocked && d.changes.length === 0 && (
            <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
              Already matches — nothing to do.
            </div>
          )}

          {d.changes.length > 0 && (
            <ul style={{ margin: '4px 0 0', paddingLeft: 18, fontSize: 11 }}>
              {d.changes.map((c) => (
                <li key={`${c.config}.${c.section}`}>
                  <span
                    style={{
                      color:
                        c.action === 'remove'
                          ? 'var(--critical)'
                          : c.action === 'create'
                            ? 'var(--good)'
                            : 'var(--text-primary)',
                    }}
                  >
                    {c.action}
                  </span>{' '}
                  <code>{c.config}.{c.section}</code>
                  {c.options && c.options.length > 0 && (
                    <span style={{ color: 'var(--text-muted)' }}>
                      {' '}
                      — {c.options.length} option{c.options.length === 1 ? '' : 's'}
                      {c.touches_key && ', including the passphrase'}
                    </span>
                  )}
                </li>
              ))}
            </ul>
          )}

          {d.omitted && d.omitted.length > 0 && (
            <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
              Left out on this device (not an error — the hardware or firmware
              cannot take it):
              <ul style={{ margin: 0, paddingLeft: 18 }}>
                {d.omitted.map((o) => (
                  <li key={o}>{o}</li>
                ))}
              </ul>
            </div>
          )}

          {/* A probable cause, worded as one.
              "device has no 5 GHz radio" describes a misconfigured band and a
              radio that failed on Tuesday equally well, and the operator has
              to act differently in each case. The server knows a capability
              changed recently; it does not know that is why. Saying "may
              explain" gives them the fact and leaves the inference where it
              belongs. */}
          {d.capability_cause && (
            <div style={{ fontSize: 11, color: 'var(--warning)', marginTop: 4 }}>
              This device's capabilities changed {ago(d.capability_cause.at)},
              which may explain what is missing above:
              <ul style={{ margin: 0, paddingLeft: 18 }}>
                {d.capability_cause.changes.map((c) => (
                  <li key={c} style={{ color: 'var(--text-secondary)' }}>{c}</li>
                ))}
              </ul>
            </div>
          )}

          {d.touches_traversal && (
            <div style={{ fontSize: 11, color: 'var(--warning)', marginTop: 4 }}>
              Edits this device's network or firewall configuration.
            </div>
          )}

          {d.deviations && d.deviations.length > 0 && (
            <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
              This device deviates from the site model on purpose:
              <ul style={{ margin: 0, paddingLeft: 18 }}>
                {d.deviations.map((x) => (
                  <li key={x}>{x}</li>
                ))}
              </ul>
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
      <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
        {options.map((o) => {
          const on = value.includes(o.v)
          return (
            <button
              key={o.v}
              type="button"
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

function Toggle({
  label,
  on,
  onChange,
}: {
  label: string
  on: boolean
  onChange: (v: boolean) => void
}) {
  return (
    <label
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 6,
        fontSize: 12,
        cursor: 'pointer',
        marginTop: 3,
      }}
    >
      <input type="checkbox" checked={on} onChange={(e) => onChange(e.target.checked)} />
      {label}
    </label>
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
  onDeleted: () => void
}) {
  const [busy, setBusy] = useState(false)
  const group = site.groups.find((g) => g.id === m.group_id)
  const net = site.networks.find((n) => n.id === m.network_id)

  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        padding: '10px 14px',
        borderTop: '1px solid var(--border)',
      }}
    >
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
      <Button onClick={onEdit}>Edit</Button>
      <Button
        disabled={busy}
        onClick={async () => {
          setBusy(true)
          try {
            await api.deleteMesh(m.id)
            onDeleted()
          } finally {
            setBusy(false)
          }
        }}
      >
        Delete
      </Button>
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
  const [draft, setDraft] = useState<Partial<Mesh>>({ ...m, key: '' })
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState('')
  const set = (patch: Partial<Mesh>) => setDraft((d) => ({ ...d, ...patch }))

  // An existing mesh keeps its stored key unless one is typed. A NEW one with
  // no key really is open, and says so rather than letting it pass unremarked.
  const willBeOpen = m.id ? !m.has_key && !draft.key : !draft.key

  async function save() {
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
        {err && <Banner tone="critical">{err}</Banner>}

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
          placeholder={m.has_key ? '••••••••' : 'blank leaves the mesh open'}
          onChange={(e) => set({ key: e.target.value })}
        />
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
          <Button kind="primary" onClick={save} disabled={saving}>
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
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  // What the card can say before it has run. Derived from the site model rather
  // than from a device, so it costs nothing and is honest about being a
  // statement of intent: the WLANs that ASKED for 802.11k, which is not the
  // same as the APs that can carry it.
  const asked = site.wlans.filter((w) => w.enabled && w.roaming.kv).map((w) => w.ssid)

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
      {err && <Banner tone="critical">{err}</Banner>}

      <div style={{ fontSize: 11, color: 'var(--text-muted)', marginBottom: 10 }}>
        Each access point is told the BSSIDs and channels of the others carrying
        the same SSID, so a roaming client scans those channels instead of all of
        them. No AP can learn this by itself. This runs automatically every 15
        minutes and after every apply — an AP that restarts comes back with an
        empty list, so it is re-checked rather than assumed.
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

  if (err) return <Card title="Backhaul health"><Banner tone="critical">{err}</Banner></Card>
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
      {err && <Banner tone="critical">{err}</Banner>}

      <div style={{ fontSize: 11, color: 'var(--text-muted)', marginBottom: 10 }}>
        A device with no cable to it can join one of your networks as a 4-address
        bridge, putting its wired ports and its own access points on the network
        over the air. The network has to accept bridges — that is the
        <strong> Allow devices to join this network as a wireless bridge </strong>
        switch on the network itself, and it is the half people forget.
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
              <Button onClick={() => remove(u.id)} disabled={busy}>
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
