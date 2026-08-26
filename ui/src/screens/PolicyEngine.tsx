import { useCallback, useEffect, useMemo, useState } from 'react'
import type { FormEvent, ReactNode } from 'react'
import { api } from '../lib/api'
import type {
  Client,
  FirewallRule,
  ObjectCompileResult,
  Policy,
  PolicyCapability,
  PolicyKind,
  PolicyObjectOutcome,
  PolicyObjectTarget,
  PolicyRow,
  PortForward,
  Site,
  SiteNetwork,
  SiteZonePolicy,
  StaticRoute,
} from '../lib/api'
import { Banner, Button, Card, DataGrid, Field, Notice, SlideOver } from '../components/ui'
import type { Column } from '../components/ui'

const WAN = 'wan'
const WAN_LABEL = 'Internet / WAN'

type Tab = 'objects' | 'master' | 'zones'
type PolicyState = 'Allow All' | 'Allow Return Traffic' | 'Block All' | 'Same zone'

function hasForward(policy: SiteZonePolicy, destination: string) {
  return policy.forward_to.includes(destination)
}

function effectiveState(
  source: SiteZonePolicy | typeof WAN,
  destination: SiteZonePolicy | typeof WAN,
): PolicyState {
  if (source !== WAN && destination !== WAN && source.name === destination.name) return 'Same zone'
  if (source === WAN) {
    return destination !== WAN && hasForward(destination, WAN)
      ? 'Allow Return Traffic'
      : 'Block All'
  }
  const destinationName = destination === WAN ? WAN : destination.name
  if (hasForward(source, destinationName)) return 'Allow All'
  if (destination !== WAN && hasForward(destination, source.name)) return 'Allow Return Traffic'
  return 'Block All'
}

function StatePill({ state }: { state: PolicyState }) {
  const colour = state === 'Allow All'
    ? 'var(--good)'
    : state === 'Block All'
      ? 'var(--critical)'
      : state === 'Allow Return Traffic'
        ? 'var(--warning)'
        : 'var(--text-muted)'
  return <Pill colour={colour}>{state}</Pill>
}

function Pill({ colour, children }: { colour: string; children: ReactNode }) {
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
      minHeight: 24, padding: '3px 8px', border: `1px solid ${colour}`,
      borderRadius: 999, color: colour, fontSize: 11, fontWeight: 600,
      textAlign: 'center',
    }}>
      {children}
    </span>
  )
}

function fallbackPolicyRows(zones: SiteZonePolicy[]): PolicyRow[] {
  return zones.map((zone, order) => ({
    id: `zone:${zone.name}`,
    origin: zone.explicit ? 'zone_matrix' : 'legacy_default',
    kind: 'zone_forward',
    name: `Forward from ${zone.name}`,
    enabled: true,
    order,
    order_scope: 'zone_forwarding',
    effective_scope: { source_zone: zone.name, destination_zones: zone.forward_to },
    mutable: zone.explicit,
    renderable: true,
    rule: { forward_to: zone.forward_to, explicit: zone.explicit },
  }))
}

export function PolicyEngine({ onReviewChanges }: { onReviewChanges?: () => void }) {
  const [site, setSite] = useState<Site | null>(null)
  // Preserve the shipped landing surface while making the broader policy
  // views one click away.
  const [tab, setTab] = useState<Tab>('zones')
  const [editing, setEditing] = useState<string | null>(null)
  const [draft, setDraft] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [loadError, setLoadError] = useState('')
  const [editError, setEditError] = useState('')
  const [confirmReset, setConfirmReset] = useState(false)
  const [saved, setSaved] = useState('')
  const [requestedClient, setRequestedClient] = useState<string | null>(null)

  const load = useCallback(async () => {
    try {
      setSite(await api.site())
      setLoadError('')
    } catch (error) {
      setLoadError(message(error))
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  const refreshPolicies = useCallback(async (success?: string) => {
    const [master, refreshedSite] = await Promise.all([api.policies(), api.site()])
    setSite((current) => current && ({
      ...current,
      policies: master.rows,
      policy_capabilities: master.capabilities,
      problems: refreshedSite.problems,
    }))
    if (success) setSaved(success)
  }, [])

  if (!site) {
    return (
      <div style={{ maxWidth: 1180 }}>
        {loadError ? (
          <div role="alert"><Banner tone="critical">{loadError}</Banner></div>
        ) : (
          <div style={{ color: 'var(--text-secondary)', fontSize: 12 }}>Loading…</div>
        )}
      </div>
    )
  }

  const zones = site.zones ?? []
  const edited = zones.find((zone) => zone.name === editing) ?? null
  const rows = site.policies ?? fallbackPolicyRows(zones)
  const capabilities = site.policy_capabilities ?? []

  function openEditor(policy: SiteZonePolicy) {
    setEditing(policy.name)
    setDraft([...policy.forward_to])
    setEditError('')
    setConfirmReset(false)
    setSaved('')
  }

  function replaceZone(policy: SiteZonePolicy) {
    setSite((current) => current && ({
      ...current,
      zones: current.zones.map((zone) => zone.name === policy.name ? policy : zone),
      policies: current.policies?.map((row) => row.id === `zone:${policy.name}`
        ? {
            ...row,
            origin: policy.explicit ? 'zone_matrix' : 'legacy_default',
            mutable: policy.explicit,
            effective_scope: {
              source_zone: policy.name,
              destination_zones: policy.forward_to,
            },
            rule: { forward_to: policy.forward_to, explicit: policy.explicit },
          }
        : row),
    }))
  }

  async function saveZone() {
    if (!edited) return
    setBusy(true)
    setEditError('')
    const ordered = [
      ...zones.filter((zone) => zone.name !== edited.name).map((zone) => zone.name),
      WAN,
    ].filter((destination) => draft.includes(destination))
    try {
      replaceZone(await api.saveZonePolicy(edited.name, ordered))
      setEditing(null)
      setSaved(`${edited.name} policy saved as desired state.`)
    } catch (error) {
      setEditError(message(error))
    } finally {
      setBusy(false)
    }
  }

  async function resetZone() {
    if (!edited) return
    setBusy(true)
    setEditError('')
    try {
      replaceZone(await api.resetZonePolicy(edited.name))
      setEditing(null)
      setSaved(`${edited.name} restored to the legacy default.`)
    } catch (error) {
      setEditError(message(error))
    } finally {
      setBusy(false)
    }
  }

  function editZoneFromMaster(name: string) {
    const zone = zones.find((candidate) => candidate.name === name)
    setTab('zones')
    if (zone) openEditor(zone)
  }

  function editClientFromMaster(mac: string) {
    setRequestedClient(mac)
    setTab('objects')
  }

  return (
    <div style={{ display: 'grid', gap: 14, maxWidth: 1180 }}>
      <header>
        <h1 style={{ margin: 0, fontSize: 20, fontWeight: 600 }}>Policy Engine</h1>
        <div style={{ marginTop: 3, color: 'var(--text-secondary)', fontSize: 12 }}>
          One inspectable desired-state model for firewall, NAT, routes, client policy and zones.
        </div>
      </header>

      {loadError && <div role="alert"><Banner tone="critical">{loadError}</Banner></div>}
      {site.problems.length > 0 && (
        <div role="alert">
          <Banner tone="critical">
            The site model is not valid, so Preview and Apply will refuse it. Fix these
            problems in Settings before treating policy as deployable:
            <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>
              {site.problems.map((problem) => <li key={problem}>{problem}</li>)}
            </ul>
          </Banner>
        </div>
      )}
      {saved && <Banner tone="accent">{saved} Preview and Apply are still required.</Banner>}
      <Notice
        tone="accent"
        component="Policy change lifecycle"
        summary="Every write here changes controller desired state only. No router changes until you Preview and Apply."
        closedLabel="More information about policy changes"
        openLabel="Hide policy change information"
        details={(
          <>
            Object Manager compiles visible drafts; it never installs a hidden policy or silently
            substitutes for an unavailable backend. Forwarded firewall and NAT changes govern new
            flows; existing tracked sessions and NAT mappings may persist until conntrack expiry.
          </>
        )}
        actions={onReviewChanges
          ? <Button onClick={onReviewChanges}>Review changes</Button>
          : undefined}
      />

      <PolicyTabs value={tab} onChange={(next) => {
        setTab(next)
        setSaved('')
      }} />

      <section role="tabpanel" id={`policy-panel-${tab}`} aria-labelledby={`policy-tab-${tab}`}>
        {tab === 'objects' && (
          <ObjectsPanel
            site={site}
            capabilities={capabilities}
            requestedClient={requestedClient}
            onRequestedClientHandled={() => setRequestedClient(null)}
            onPoliciesChanged={refreshPolicies}
            onReviewChanges={onReviewChanges}
          />
        )}
        {tab === 'master' && (
          <MasterTable
            site={site}
            rows={rows}
            capabilities={capabilities}
            onChanged={refreshPolicies}
            onEditZone={editZoneFromMaster}
            onEditClient={editClientFromMaster}
          />
        )}
        {tab === 'zones' && <ZoneMatrix zones={zones} openEditor={openEditor} />}
      </section>

      {edited && (
        <SlideOver title={`Traffic from ${edited.name}`} onClose={() => !busy && setEditing(null)}>
          <div style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            Choose every destination that devices in <strong>{edited.name}</strong> may initiate
            traffic to. Replies to established traffic are allowed automatically.
          </div>
          {editError && <div role="alert"><Banner tone="critical">{editError}</Banner></div>}
          <fieldset style={fieldsetStyle}>
            <legend style={legendStyle}>Forward to</legend>
            {zones.filter((zone) => zone.name !== edited.name).map((zone) => (
              <DestinationToggle
                key={zone.name}
                name={zone.name}
                label={zone.name}
                checked={draft.includes(zone.name)}
                disabled={busy}
                onChange={(checked) => setDraft(toggleDestination(draft, zone.name, checked))}
              />
            ))}
            <DestinationToggle
              name={WAN}
              label={WAN_LABEL}
              checked={draft.includes(WAN)}
              disabled={busy}
              onChange={(checked) => setDraft(toggleDestination(draft, WAN, checked))}
            />
          </fieldset>
          <Banner tone="accent">
            Save records desired state only. Use Preview and Apply before any router changes.
          </Banner>
          <div style={{ display: 'flex', gap: 8 }}>
            <Button kind="primary" disabled={busy} onClick={saveZone}>
              {busy ? 'Saving…' : 'Save source policy'}
            </Button>
            <Button disabled={busy} onClick={() => setEditing(null)}>Cancel</Button>
          </div>
          {edited.explicit ? (
            <div style={{ borderTop: '1px solid var(--border)', paddingTop: 12 }}>
              {!confirmReset ? (
                <Button disabled={busy} onClick={() => setConfirmReset(true)}>Reset policy</Button>
              ) : (
                <div style={{ display: 'grid', gap: 8 }}>
                  <Banner tone="warning">
                    Reset removes this explicit policy and restores the legacy default:
                    {' '}{edited.name} → {WAN_LABEL} is allowed. Other initiation from
                    {' '}{edited.name} is blocked.
                  </Banner>
                  <div style={{ display: 'flex', gap: 8 }}>
                    <Button disabled={busy} onClick={resetZone}>
                      {busy ? 'Resetting…' : 'Restore legacy default'}
                    </Button>
                    <Button disabled={busy} onClick={() => setConfirmReset(false)}>Keep policy</Button>
                  </div>
                </div>
              )}
            </div>
          ) : (
            <div style={{ color: 'var(--text-muted)', fontSize: 11 }}>
              This source already uses the legacy default: forward to {WAN_LABEL}.
            </div>
          )}
        </SlideOver>
      )}
    </div>
  )
}

function PolicyTabs({ value, onChange }: { value: Tab; onChange: (tab: Tab) => void }) {
  const tabs: Array<{ id: Tab; label: string }> = [
    { id: 'objects', label: 'Objects' },
    { id: 'master', label: 'Master Table' },
    { id: 'zones', label: 'Zone Matrix' },
  ]

  function moveFrom(current: Tab, key: string) {
    const at = tabs.findIndex((tab) => tab.id === current)
    let next = -1
    if (key === 'ArrowRight') next = (at + 1) % tabs.length
    if (key === 'ArrowLeft') next = (at - 1 + tabs.length) % tabs.length
    if (key === 'Home') next = 0
    if (key === 'End') next = tabs.length - 1
    if (next < 0) return false
    const target = tabs[next].id
    onChange(target)
    // All tab buttons stay mounted and keyed, so focus can move immediately;
    // the state update then makes this same element the sole tab stop.
    document.getElementById(`policy-tab-${target}`)?.focus()
    return true
  }

  return (
    <div className="policy-tabs" role="tablist" aria-label="Policy Engine views" style={{
      display: 'flex', gap: 2, padding: 3, width: 'fit-content',
      background: 'var(--surface-1)', border: '1px solid var(--border)', borderRadius: 8,
    }}>
      {tabs.map((tab) => (
        <button
          key={tab.id}
          id={`policy-tab-${tab.id}`}
          role="tab"
          aria-selected={value === tab.id}
          aria-controls={`policy-panel-${tab.id}`}
          tabIndex={value === tab.id ? 0 : -1}
          type="button"
          onClick={() => onChange(tab.id)}
          onKeyDown={(event) => {
            if (moveFrom(tab.id, event.key)) event.preventDefault()
          }}
          style={{
            minHeight: 30, padding: '0 13px', border: 0, borderRadius: 6,
            cursor: 'pointer', fontSize: 12, fontWeight: 600,
            color: value === tab.id ? 'var(--accent-text)' : 'var(--text-secondary)',
            background: value === tab.id ? 'var(--accent-soft)' : 'transparent',
          }}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}

function ZoneMatrix({ zones, openEditor }: {
  zones: SiteZonePolicy[]
  openEditor: (zone: SiteZonePolicy) => void
}) {
  return (
    <div style={{ display: 'grid', gap: 14 }}>
      <Notice
        tone="accent"
        popoverDetails
        component="Zone Matrix scope"
        summary="The Zone Matrix manages whole-zone forwarding only. Use Master Table for explicit firewall, port-forward, and route records."
        closedLabel="More information about Zone Matrix scope"
        openLabel="Hide Zone Matrix scope"
        details={(
          <>
            WAN-initiated allow rules, port forwards, per-client or per-port rules, application
            filtering, QoS, and DPI are not implemented by this Zone Matrix editor. QoS and
            application identity remain capability-gated. Preview also checks foreign OpenWrt rule,
            redirect and include sections. For explicit gateway policies it also reads active
            nftables transit hooks and reachable rules, blocking custom or unreadable evidence. The
            terse runtime view cannot prove include-file provenance or inspect set contents, and{' '}
            <code>!fw4:</code> attribution comments can be imitated by direct custom rules.
          </>
        )}
      />
      {zones.length === 0 ? (
        <Card title="Zone Matrix">
          <div style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            No managed routed zones. An enabled network with VLAN greater than 1 is required.
          </div>
        </Card>
      ) : (
        <Card title="Zone Matrix" pad={false}>
          <div style={{ overflowX: 'auto' }}>
            <table aria-label="Zone policy matrix" style={{ width: '100%', borderCollapse: 'collapse', minWidth: 660 }}>
              <caption style={{ textAlign: 'left', padding: '10px 14px', color: 'var(--text-secondary)', fontSize: 11 }}>
                Rows are source zones; columns are destinations. Select an editable cell to
                change the source zone&apos;s complete forwarding list. Same-zone traffic is not
                controlled here and may remain on Layer 2 without reaching the firewall.
              </caption>
              <thead>
                <tr>
                  <th style={headerCell}>Source ↓ / Destination →</th>
                  {zones.map((zone) => <th key={zone.name} scope="col" style={headerCell}>{zone.name}</th>)}
                  <th scope="col" style={headerCell}>{WAN_LABEL}</th>
                </tr>
              </thead>
              <tbody>
                {zones.map((source) => (
                  <tr key={source.name}>
                    <th scope="row" style={rowHeaderCell}>
                      <div>{source.name}</div>
                      <div style={{ color: 'var(--text-muted)', fontSize: 10, fontWeight: 400 }}>
                        {source.explicit ? 'Explicit' : 'Legacy default'}
                      </div>
                    </th>
                    {zones.map((destination) => {
                      const state = effectiveState(source, destination)
                      const same = source.name === destination.name
                      return (
                        <td key={destination.name} style={matrixCell}>
                          {same ? (
                            <div
                              aria-label={`${source.name} to ${destination.name}: Same zone`}
                              title="Not firewall-controlled; same-zone traffic may remain on Layer 2"
                              style={readOnlyCell}
                            ><StatePill state={state} /></div>
                          ) : (
                            <button
                              type="button"
                              aria-label={`${source.name} to ${destination.name}: ${state}. Edit ${source.name} policy`}
                              onClick={() => openEditor(source)}
                              style={editableCell}
                            ><StatePill state={state} /></button>
                          )}
                        </td>
                      )
                    })}
                    <td style={matrixCell}>
                      <button
                        type="button"
                        aria-label={`${source.name} to ${WAN_LABEL}: ${effectiveState(source, WAN)}. Edit ${source.name} policy`}
                        onClick={() => openEditor(source)}
                        style={editableCell}
                      ><StatePill state={effectiveState(source, WAN)} /></button>
                    </td>
                  </tr>
                ))}
                <tr>
                  <th scope="row" style={rowHeaderCell}>
                    <div>{WAN_LABEL}</div>
                    <div style={{ color: 'var(--text-muted)', fontSize: 10, fontWeight: 400 }}>Read only</div>
                  </th>
                  {zones.map((destination) => {
                    const state = effectiveState(WAN, destination)
                    return (
                      <td key={destination.name} style={matrixCell}>
                        <div
                          aria-label={`${WAN_LABEL} to ${destination.name}: ${state}. Read only`}
                          title="WAN-initiated allow is created as an explicit firewall rule or port forward"
                          style={readOnlyCell}
                        ><StatePill state={state} /></div>
                      </td>
                    )
                  })}
                  <td style={matrixCell}>
                    <div aria-label={`${WAN_LABEL} to ${WAN_LABEL}: Same zone`} style={readOnlyCell}>
                      <StatePill state="Same zone" />
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </div>
  )
}

function MasterTable({
  site,
  rows,
  capabilities,
  onChanged,
  onEditZone,
  onEditClient,
}: {
  site: Site
  rows: PolicyRow[]
  capabilities: PolicyCapability[]
  onChanged: (message?: string) => Promise<void>
  onEditZone: (name: string) => void
  onEditClient: (mac: string) => void
}) {
  const [editing, setEditing] = useState<Policy | null>(null)
  const [loadError, setLoadError] = useState('')

  const columns: Column<PolicyRow>[] = [
    { key: 'name', header: 'Policy', required: true, render: (row) => row.name, sortBy: (row) => row.name },
    { key: 'kind', header: 'Type', render: (row) => kindLabel(row.kind), sortBy: (row) => row.kind },
    {
      key: 'enabled', header: 'State',
      render: (row) => <Pill colour={row.enabled ? 'var(--good)' : 'var(--text-muted)'}>{row.enabled ? 'Enabled' : 'Disabled'}</Pill>,
      sortBy: (row) => row.enabled ? 1 : 0,
    },
    {
      key: 'order', header: 'Display order',
      render: (row) => `${scopeLabel(row.order_scope)} · ${row.order}`,
      sortBy: (row) => `${row.order_scope}:${String(row.order).padStart(10, '0')}`,
    },
    { key: 'scope', header: 'Effective scope', render: scopeText, sortBy: scopeText },
    { key: 'origin', header: 'Origin', render: (row) => originLabel(row.origin), sortBy: (row) => row.origin },
    {
      key: 'renderable', header: 'Deployability',
      render: (row) => row.renderable
        ? <Pill colour="var(--good)">Renderable</Pill>
        : <span title={row.gated_reason}><Pill colour="var(--warning)">Gated</Pill>{row.gated_reason ? ` ${row.gated_reason}` : ''}</span>,
      sortBy: (row) => row.renderable ? 1 : 0,
    },
    {
      key: 'actions', header: 'Actions',
      render: (row) => (
        <button
          type="button"
          aria-label={`Edit ${row.name}`}
          onClick={(event) => {
            event.stopPropagation()
            open(row)
          }}
          onKeyDown={(event) => {
            if (event.key !== 'Enter' && event.key !== ' ') return
            event.preventDefault()
            event.stopPropagation()
            open(row)
          }}
          style={tableActionStyle}
        >
          Edit
        </button>
      ),
    },
  ]

  function open(row: PolicyRow) {
    setLoadError('')
    if (row.kind === 'zone_forward') {
      const source = stringScope(row, 'source_zone')
      if (source) onEditZone(source)
      return
    }
    if (row.kind === 'client_block' || row.kind === 'fixed_ip') {
      const mac = stringScope(row, 'client_mac')
      if (mac) onEditClient(mac)
      return
    }
    const policy = policyFromRow(row)
    if (policy) setEditing(policy)
  }

  return (
    <div style={{ display: 'grid', gap: 14 }}>
      <CapabilityStrip capabilities={capabilities} />
      {loadError && <div role="alert"><Banner tone="critical">{loadError}</Banner></div>}
      <Card
        title="Master Table"
        actions={<Button kind="primary" onClick={() => setEditing(emptyPolicy('firewall_rule'))}>Create rule</Button>}
        pad={false}
      >
        <div style={{ padding: '9px 14px', borderBottom: '1px solid var(--border)', color: 'var(--text-secondary)', fontSize: 11 }}>
          Ordered-for-display controller policy across zones, firewall, NAT, routes and client desired state.
          Display order groups records for inspection; it does not claim UCI evaluation precedence. Semantically
          overlapping managed rules are refused instead of relying on an order the controller does not own.
          Explicit firewall, port-forward and static-route records are IPv4-only in this release; Client Block
          covers IPv4 and IPv6 managed-zone forwarding.
          Select a row to edit it at its source. A gate is an explicit refusal, not a deployment claim.
        </div>
        <DataGrid rows={rows} columns={columns} rowKey={(row) => row.id} onRowClick={open} empty="No policy records. Create a rule or configure a managed zone." />
      </Card>
      {editing && (
        <PolicyEditor
          site={site}
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={async (policy) => {
            try {
              await onChanged(`${policy.name} saved as desired state.`)
              setEditing(null)
            } catch (error) {
              throw new Error(`Rule saved, but the table could not refresh: ${message(error)}`)
            }
          }}
          onDeleted={async (name) => {
            setEditing(null)
            try {
              await onChanged(`${name} removed from desired state.`)
            } catch (error) {
              setLoadError(`Rule removed, but the table could not refresh: ${message(error)}`)
            }
          }}
        />
      )}
    </div>
  )
}

function PolicyEditor({
  site,
  initial,
  onClose,
  onSaved,
  onDeleted,
}: {
  site: Site
  initial: Policy
  onClose: () => void
  onSaved: (policy: Policy) => Promise<void>
  onDeleted: (name: string) => Promise<void>
}) {
  const [draft, setDraft] = useState<Policy>(() => clonePolicy(initial))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [confirmDelete, setConfirmDelete] = useState(false)
  const creating = draft.id === 0
  const zones = site.zones.map((zone) => zone.name)
  const managedNetworks = site.networks.filter((network) => network.enabled && network.vlan > 1)

  function changeKind(kind: PolicyKind) {
    const replacement = emptyPolicy(kind)
    setDraft({ ...replacement, name: draft.name, order: draft.order, enabled: draft.enabled })
  }

  async function save(event: FormEvent) {
    event.preventDefault()
    setError('')
    const validation = validatePolicyDraft(draft)
    if (validation) {
      setError(validation)
      return
    }
    setBusy(true)
    try {
      const saved = await api.savePolicy(cleanPolicy(draft))
      setDraft(clonePolicy(saved))
      await onSaved(saved)
    } catch (requestError) {
      setError(message(requestError))
    } finally {
      setBusy(false)
    }
  }

  async function remove() {
    if (!draft.id) return
    setBusy(true)
    setError('')
    try {
      await api.deletePolicy(draft.id)
      await onDeleted(draft.name)
    } catch (requestError) {
      setError(message(requestError))
    } finally {
      setBusy(false)
    }
  }

  return (
    <SlideOver title={creating ? 'Create policy rule' : `Edit ${draft.name}`} onClose={() => !busy && onClose()}>
      <form onSubmit={save} style={{ display: 'grid', gap: 13 }}>
        {error && <div role="alert"><Banner tone="critical">{error}</Banner></div>}
        {creating && (
          <SelectField label="Rule type" value={draft.kind} disabled={busy} onChange={(value) => changeKind(value as PolicyKind)}>
            <option value="firewall_rule">Firewall rule</option>
            <option value="port_forward">Port forward</option>
            <option value="static_route">Static route</option>
          </SelectField>
        )}
        <Field
          label="Name"
          required
          maxLength={128}
          disabled={busy}
          value={draft.name}
          onChange={(event) => setDraft({ ...draft, name: event.target.value })}
        />
        <Field
          label="Display order"
          type="number"
          min={0}
          max={2147483647}
          disabled={busy}
          value={draft.order}
          onChange={(event) => setDraft({ ...draft, order: numberValue(event.target.value) })}
        />
        <div style={hintStyle}>
          Display order is for the table only. It is not firewall precedence; overlapping managed rules are rejected.
        </div>
        <label style={checkStyle}>
          <input
            type="checkbox"
            checked={draft.enabled}
            disabled={busy}
            onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })}
          />
          Enabled in desired state
        </label>

        {draft.kind === 'firewall_rule' && draft.firewall && (
          <>
            <Banner tone="warning">
              This explicit firewall rule is IPv4-only. IPv6 traffic is unaffected.
            </Banner>
            <FirewallFields value={draft.firewall} zones={zones} disabled={busy} onChange={(firewall) => setDraft({ ...draft, firewall })} />
          </>
        )}
        {draft.kind === 'port_forward' && draft.port_forward && (
          <PortForwardFields value={draft.port_forward} zones={zones} disabled={busy} onChange={(port_forward) => setDraft({ ...draft, port_forward })} />
        )}
        {draft.kind === 'static_route' && draft.static_route && (
          <StaticRouteFields value={draft.static_route} networks={managedNetworks} disabled={busy} onChange={(static_route) => setDraft({ ...draft, static_route })} />
        )}

        <Banner tone="accent">
          Save writes desired state only. The rule reaches no router until a separate Preview and Apply.
        </Banner>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
          <Button kind="primary" type="submit" disabled={busy}>
            {busy ? 'Saving…' : 'Save desired rule'}
          </Button>
          <Button disabled={busy} onClick={onClose}>Cancel</Button>
        </div>
      </form>
      {!creating && (
        <div style={{ borderTop: '1px solid var(--border)', paddingTop: 12 }}>
          {!confirmDelete ? (
            <Button disabled={busy} onClick={() => setConfirmDelete(true)}>Delete rule</Button>
          ) : (
            <div style={{ display: 'grid', gap: 8 }}>
              <Banner tone="warning">
                Delete removes this rule from desired state. Existing router state remains unchanged until Preview and Apply.
              </Banner>
              <div style={{ display: 'flex', gap: 8 }}>
                <Button disabled={busy} onClick={remove}>{busy ? 'Deleting…' : 'Delete desired rule'}</Button>
                <Button disabled={busy} onClick={() => setConfirmDelete(false)}>Keep rule</Button>
              </div>
            </div>
          )}
        </div>
      )}
    </SlideOver>
  )
}

function FirewallFields({ value, zones, disabled, onChange }: {
  value: FirewallRule
  zones: string[]
  disabled: boolean
  onChange: (value: FirewallRule) => void
}) {
  return (
    <fieldset style={fieldsetStyle}>
      <legend style={legendStyle}>IPv4 firewall match and verdict</legend>
      <SelectField label="Action" value={value.action} disabled={disabled} onChange={(action) => onChange({ ...value, action: action as FirewallRule['action'] })}>
        <option value="accept">Accept</option>
        <option value="drop">Drop silently</option>
        <option value="reject">Reject</option>
      </SelectField>
      <SelectField label="Source zone" value={value.source_zone} disabled={disabled} onChange={(source_zone) => onChange({ ...value, source_zone })}>
        <option value="wan">{WAN_LABEL}</option>
        {zones.map((zone) => <option key={zone} value={zone}>{zone}</option>)}
      </SelectField>
      <SelectField label="Destination" value={value.destination_zone ?? ''} disabled={disabled} onChange={(destination_zone) => onChange({ ...value, destination_zone })}>
        <option value="">Gateway itself</option>
        <option value="wan">{WAN_LABEL}</option>
        {zones.map((zone) => <option key={zone} value={zone}>{zone}</option>)}
      </SelectField>
      <ProtocolChecks
        label="Protocols"
        choices={['all', 'tcp', 'udp', 'icmp']}
        selected={value.protocols}
        disabled={disabled}
        onChange={(protocols) => onChange({ ...value, protocols: protocols as FirewallRule['protocols'] })}
      />
      <Field label="Source IPv4 CIDR (optional)" placeholder="192.168.20.0/24" disabled={disabled} value={value.source_cidr ?? ''} onChange={(event) => onChange({ ...value, source_cidr: event.target.value })} />
      <Field label="Destination IPv4 CIDR (optional)" placeholder="10.0.0.0/8" disabled={disabled} value={value.destination_cidr ?? ''} onChange={(event) => onChange({ ...value, destination_cidr: event.target.value })} />
      <Field label="Source port or range (optional)" placeholder="1024-65535" disabled={disabled} value={value.source_port ?? ''} onChange={(event) => onChange({ ...value, source_port: event.target.value })} />
      <Field label="Destination port or range (optional)" placeholder="443 or 8000-8080" disabled={disabled} value={value.destination_port ?? ''} onChange={(event) => onChange({ ...value, destination_port: event.target.value })} />
      <Field label="Source MACs (comma separated, optional)" placeholder="aa:bb:cc:dd:ee:ff" disabled={disabled} value={(value.source_macs ?? []).join(', ')} onChange={(event) => onChange({ ...value, source_macs: splitList(event.target.value) })} />
    </fieldset>
  )
}

function PortForwardFields({ value, zones, disabled, onChange }: {
  value: PortForward
  zones: string[]
  disabled: boolean
  onChange: (value: PortForward) => void
}) {
  return (
    <fieldset style={fieldsetStyle}>
      <legend style={legendStyle}>IPv4 WAN port forward</legend>
      <div style={hintStyle}>Source zone is fixed to Internet / WAN.</div>
      <SelectField label="Destination zone" value={value.destination_zone} disabled={disabled} onChange={(destination_zone) => onChange({ ...value, destination_zone })}>
        <option value="">Choose a managed zone</option>
        {zones.map((zone) => <option key={zone} value={zone}>{zone}</option>)}
      </SelectField>
      <ProtocolChecks
        label="Protocols"
        choices={['tcp', 'udp']}
        selected={value.protocols}
        disabled={disabled}
        onChange={(protocols) => onChange({ ...value, protocols: protocols as PortForward['protocols'] })}
      />
      <Field label="External port" type="number" min={1} max={65535} required disabled={disabled} value={value.external_port || ''} onChange={(event) => onChange({ ...value, external_port: numberValue(event.target.value) })} />
      <Field label="Destination IPv4" placeholder="192.168.20.10" required disabled={disabled} value={value.destination_ip} onChange={(event) => onChange({ ...value, destination_ip: event.target.value })} />
      <Field label="Destination port" type="number" min={1} max={65535} required disabled={disabled} value={value.destination_port || ''} onChange={(event) => onChange({ ...value, destination_port: numberValue(event.target.value) })} />
      <Field label="Allowed source IPv4 CIDR (optional)" placeholder="203.0.113.0/24" disabled={disabled} value={value.source_cidr ?? ''} onChange={(event) => onChange({ ...value, source_cidr: event.target.value })} />
    </fieldset>
  )
}

function StaticRouteFields({ value, networks, disabled, onChange }: {
  value: StaticRoute
  networks: SiteNetwork[]
  disabled: boolean
  onChange: (value: StaticRoute) => void
}) {
  return (
    <fieldset style={fieldsetStyle}>
      <legend style={legendStyle}>Static network route</legend>
      <SelectField label="Egress network" value={String(value.network_id)} disabled={disabled} onChange={(network) => onChange({ ...value, network_id: numberValue(network) })}>
        <option value="0">{WAN_LABEL}</option>
        {networks.map((network) => <option key={network.id} value={network.id}>{network.name} · VLAN {network.vlan}</option>)}
      </SelectField>
      <Field label="Target IPv4 network" placeholder="10.40.0.0/16" required disabled={disabled} value={value.target} onChange={(event) => onChange({ ...value, target: event.target.value })} />
      <Field label="Next-hop IPv4" placeholder="192.168.20.2" required disabled={disabled} value={value.gateway} onChange={(event) => onChange({ ...value, gateway: event.target.value })} />
      <Field label="Metric" type="number" min={0} max={65535} disabled={disabled} value={value.metric} onChange={(event) => onChange({ ...value, metric: numberValue(event.target.value) })} />
    </fieldset>
  )
}

function ObjectsPanel({
  site,
  capabilities,
  requestedClient,
  onRequestedClientHandled,
  onPoliciesChanged,
  onReviewChanges,
}: {
  site: Site
  capabilities: PolicyCapability[]
  requestedClient: string | null
  onRequestedClientHandled: () => void
  onPoliciesChanged: (message?: string) => Promise<void>
  onReviewChanges?: () => void
}) {
  const [clients, setClients] = useState<Client[] | null>(null)
  const [clientsError, setClientsError] = useState('')
  const [clientsCoverage, setClientsCoverage] = useState<{ returned: number; total: number } | null>(null)
  const [objectKind, setObjectKind] = useState<PolicyObjectTarget['kind']>('network')
  const [objectID, setObjectID] = useState('')
  const [objects, setObjects] = useState<PolicyObjectTarget[]>([])
  const [outcomes, setOutcomes] = useState<Array<PolicyObjectOutcome['kind']>>(['secure'])
  const [destinationZone, setDestinationZone] = useState('wan')
  const [routeTarget, setRouteTarget] = useState('')
  const [routeGateway, setRouteGateway] = useState('')
  const [routeMetric, setRouteMetric] = useState(0)
  const [rateKbps, setRateKbps] = useState(10000)
  const [compiling, setCompiling] = useState(false)
  const [compileError, setCompileError] = useState('')
  const [result, setResult] = useState<ObjectCompileResult | null>(null)
  const [compiledFor, setCompiledFor] = useState('')
  const [savingDraft, setSavingDraft] = useState<number | null>(null)
  const [savedDrafts, setSavedDrafts] = useState<Set<number>>(() => new Set())
  const [selectedClient, setSelectedClient] = useState<string | null>(null)
  const [clientChoice, setClientChoice] = useState('')
  const [sourceRevision, setSourceRevision] = useState(0)

  const compileInput = JSON.stringify({
    objects,
    outcomes,
    destinationZone,
    routeTarget,
    routeGateway,
    routeMetric,
    rateKbps,
    sourceRevision,
  })
  const compiledResult = compiledFor === compileInput ? result : null

  useEffect(() => {
    if (!result || compiledFor === compileInput) return
    setResult(null)
    setCompiledFor('')
    setSavedDrafts(new Set())
  }, [compileInput, compiledFor, result])

  const loadClients = useCallback(async () => {
    try {
      const page = await api.clients({ all: true, limit: 5000 })
      const loaded = page.clients ?? []
      setClients(loaded)
      setClientsCoverage(page.total > loaded.length
        ? { returned: loaded.length, total: page.total }
        : null)
      setClientChoice((current) => current || loaded[0]?.mac || '')
      setClientsError('')
    } catch (error) {
      setClientsError(message(error))
      setClientsCoverage(null)
    }
  }, [])

  useEffect(() => {
    loadClients()
  }, [loadClients])

  useEffect(() => {
    if (!requestedClient || !clients) return
    if (clients.some((client) => client.mac.toLowerCase() === requestedClient.toLowerCase())) {
      setSelectedClient(requestedClient)
    }
    onRequestedClientHandled()
  }, [clients, onRequestedClientHandled, requestedClient])

  useEffect(() => {
    if (!clients?.some((client) => client.mac === clientChoice)) {
      setClientChoice(clients?.[0]?.mac ?? '')
    }
  }, [clientChoice, clients])

  const groups = useMemo(() => [...new Set(
    (clients ?? []).map((client) => client.group?.trim()).filter((group): group is string => !!group),
  )].sort(), [clients])

  const objectOptions = useMemo(() => {
    if (objectKind === 'device') {
      return (clients ?? []).map((client) => ({
        id: client.mac,
        label: `${client.name || 'Unnamed client'} · ${client.mac}`,
      }))
    }
    if (objectKind === 'group') return groups.map((group) => ({ id: group, label: group }))
    return [
      { id: 'wan', label: WAN_LABEL },
      ...site.networks
        .filter((network) => network.enabled && network.vlan > 1)
        .map((network) => ({ id: String(network.id), label: `${network.name} · VLAN ${network.vlan}` })),
    ]
  }, [clients, groups, objectKind, site.networks])

  useEffect(() => {
    if (!objectOptions.some((option) => option.id === objectID)) {
      setObjectID(objectOptions[0]?.id ?? '')
    }
  }, [objectID, objectOptions])

  function addObject() {
    if (!objectID) return
    const target = { kind: objectKind, id: objectID }
    if (!objects.some((object) => object.kind === target.kind && object.id === target.id)) {
      setObjects([...objects, target])
    }
  }

  async function compile() {
    if (objects.length === 0) {
      setCompileError('Add at least one object to the compile scope.')
      return
    }
    if (outcomes.length === 0) {
      setCompileError('Select at least one requested outcome.')
      return
    }
    const requests: PolicyObjectOutcome[] = outcomes.map((kind) => {
      if (kind === 'secure') return { kind, destination_zone: destinationZone }
      if (kind === 'route') return { kind, target: routeTarget, gateway: routeGateway, metric: routeMetric }
      if (kind === 'qos') return { kind, rate_kbps: rateKbps }
      return { kind }
    })
    setCompiling(true)
    setCompileError('')
    setResult(null)
    setCompiledFor('')
    setSavedDrafts(new Set())
    const input = compileInput
    try {
      setResult(await api.compilePolicyObjects(objects, requests))
      setCompiledFor(input)
    } catch (error) {
      setCompileError(message(error))
    } finally {
      setCompiling(false)
    }
  }

  async function saveCompiledDraft(policy: Policy, index: number) {
    if (!compiledResult) {
      setCompileError('Compile inputs changed. Compile and inspect fresh drafts before saving.')
      setResult(null)
      setSavedDrafts(new Set())
      return
    }
    setSavingDraft(index)
    setCompileError('')
    try {
      const saved = await api.savePolicy({ ...policy, id: undefined })
      setSavedDrafts((current) => new Set(current).add(index))
      await onPoliciesChanged(`${saved.name} saved from an Object Manager draft as desired state.`)
    } catch (error) {
      setCompileError(message(error))
    } finally {
      setSavingDraft(null)
    }
  }

  const client = selectedClient && clients?.find(
    (candidate) => candidate.mac.toLowerCase() === selectedClient.toLowerCase(),
  )

  return (
    <div style={{ display: 'grid', gap: 14 }}>
      <CapabilityStrip capabilities={capabilities} />
      {clientsError && <div role="alert"><Banner tone="critical">Client objects could not load: {clientsError}</Banner></div>}
      {clientsCoverage && (
        <div role="alert">
          <Banner tone="warning">
            Client inventory is incomplete: {clientsCoverage.returned} of {clientsCoverage.total} clients
            were returned. Device, group and client-policy choices below show only that subset;
            absence from these lists does not mean a client or group does not exist.
          </Banner>
        </div>
      )}
      <Card title="Object Manager">
        <div style={{ display: 'grid', gap: 14 }}>
          <div>
            <div style={stepTitleStyle}>1 · Choose objects</div>
            <div className="policy-object-picker">
              <SelectField label="Object type" value={objectKind} disabled={compiling} onChange={(value) => setObjectKind(value as PolicyObjectTarget['kind'])}>
                <option value="device">Client device</option>
                <option value="group">Client group</option>
                <option value="network">Network</option>
              </SelectField>
              <SelectField
                label={clientsCoverage && objectKind !== 'network' ? 'Object (partial client inventory)' : 'Object'}
                value={objectID}
                disabled={compiling || objectOptions.length === 0}
                onChange={setObjectID}
              >
                {objectOptions.length === 0 && (
                  <option value="">
                    {clientsCoverage && objectKind !== 'network'
                      ? 'No objects in returned subset'
                      : 'No objects available'}
                  </option>
                )}
                {objectOptions.map((option) => <option key={option.id} value={option.id}>{option.label}</option>)}
              </SelectField>
              <Button disabled={compiling || !objectID} onClick={addObject}>Add object</Button>
            </div>
            {objectKind === 'group' && groups.length === 0 && (
              <div style={hintStyle}>
                {clientsCoverage
                  ? 'No client groups were present in the returned subset.'
                  : 'No client groups yet. Assign a desired group in Client policy below.'}
              </div>
            )}
            <div aria-label="Selected policy objects" style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginTop: 9 }}>
              {objects.length === 0 && <span style={hintStyle}>No objects selected.</span>}
              {objects.map((object) => (
                <span key={`${object.kind}:${object.id}`} style={chipStyle}>
                  {object.kind}: {objectLabel(object, objectOptions, clients ?? [], site.networks)}
                  <button
                    type="button"
                    aria-label={`Remove ${object.kind} ${object.id}`}
                    disabled={compiling}
                    onClick={() => setObjects(objects.filter((candidate) => candidate.kind !== object.kind || candidate.id !== object.id))}
                    style={chipRemoveStyle}
                  >×</button>
                </span>
              ))}
            </div>
          </div>

          <div>
            <div style={stepTitleStyle}>2 · Request outcomes</div>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(185px, 1fr))', gap: 8 }}>
              <OutcomeToggle
                title="Secure (IPv4)"
                detail={`Compile an IPv4-only reject draft from each selected object's resolved source scope to ${destinationZone === 'wan' ? WAN_LABEL : destinationZone}. It affects new routed flows; existing conntrack sessions may continue until expiry. IPv6 is unaffected. Router input such as DHCP/DNS and same-L2 traffic are outside this forwarded rule.`}
                checked={outcomes.includes('secure')}
                disabled={compiling}
                onChange={(checked) => setOutcomes(toggleOutcome(outcomes, 'secure', checked))}
              />
              <OutcomeToggle
                title="Route"
                detail="Compile a static network route; device/group policy routing is explicitly gated."
                checked={outcomes.includes('route')}
                disabled={compiling}
                onChange={(checked) => setOutcomes(toggleOutcome(outcomes, 'route', checked))}
              />
              <OutcomeToggle
                title="QoS"
                detail="Request a visible gate; unavailable until an observed SQM/tc backend exists."
                checked={outcomes.includes('qos')}
                disabled={compiling}
                unavailable
                onChange={(checked) => setOutcomes(toggleOutcome(outcomes, 'qos', checked))}
              />
              <OutcomeToggle
                title="Application (DPI)"
                detail="Request a visible gate; unavailable until application identity is separately observed."
                checked={outcomes.includes('application')}
                disabled={compiling}
                unavailable
                onChange={(checked) => setOutcomes(toggleOutcome(outcomes, 'application', checked))}
              />
            </div>
            {outcomes.includes('secure') && (
              <div style={{ marginTop: 10, maxWidth: 360 }}>
                <SelectField label="Secure against destination" value={destinationZone} disabled={compiling} onChange={setDestinationZone}>
                  <option value="wan">{WAN_LABEL}</option>
                  {site.zones.map((zone) => <option key={zone.name} value={zone.name}>{zone.name}</option>)}
                </SelectField>
              </div>
            )}
            {outcomes.includes('route') && (
              <div className="policy-route-fields">
                <Field label="Route target" placeholder="10.40.0.0/16" disabled={compiling} value={routeTarget} onChange={(event) => setRouteTarget(event.target.value)} />
                <Field label="Next-hop IPv4" placeholder="192.168.20.2" disabled={compiling} value={routeGateway} onChange={(event) => setRouteGateway(event.target.value)} />
                <Field label="Metric" type="number" min={0} max={65535} disabled={compiling} value={routeMetric} onChange={(event) => setRouteMetric(numberValue(event.target.value))} />
              </div>
            )}
            {outcomes.includes('qos') && (
              <div style={{ marginTop: 10, maxWidth: 240 }}>
                <Field label="Requested rate (kbps)" type="number" min={1} disabled={compiling} value={rateKbps} onChange={(event) => setRateKbps(numberValue(event.target.value))} />
              </div>
            )}
          </div>

          <div>
            <div style={stepTitleStyle}>3 · Compile and inspect</div>
            <div style={{ color: 'var(--text-secondary)', fontSize: 12, marginBottom: 8 }}>
              Compile is read-only. It produces concrete candidate records and verbatim capability gates; it does not persist or apply them.
              Secure/firewall drafts are IPv4-only in this release.
            </div>
            <Button kind="primary" disabled={compiling} onClick={compile}>
              {compiling ? 'Compiling…' : 'Compile visible drafts'}
            </Button>
          </div>
          {compileError && <div role="alert"><Banner tone="critical">{compileError}</Banner></div>}
        </div>
      </Card>

      {compiledResult && (
        <Card title="Compiled result">
          <div style={{ display: 'grid', gap: 12 }}>
            <Banner tone="accent">Not persisted · Not applied. {compiledResult.note}</Banner>
            {compiledResult.gates.map((gate, index) => (
              <div role="status" key={`${gate.object.kind}:${gate.object.id}:${gate.outcome}:${index}`}>
                <Banner tone="warning">
                  <strong>{gate.outcome}</strong> for {gate.object.kind} <code>{gate.object.id}</code>: {gate.reason}
                </Banner>
              </div>
            ))}
            {compiledResult.drafts.length === 0 && compiledResult.gates.length === 0 && (
              <div style={hintStyle}>The compiler returned no drafts and no gates.</div>
            )}
            {compiledResult.drafts.map((policy, index) => (
              <section key={`${policy.kind}:${policy.name}:${index}`} style={draftCardStyle}>
                <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, alignItems: 'start' }}>
                  <div>
                    <strong style={{ fontSize: 13 }}>{policy.name}</strong>
                    <div style={hintStyle}>{kindLabel(policy.kind)} · {originLabel(policy.origin)} · {policy.enabled ? 'enabled' : 'disabled'}</div>
                  </div>
                  <Pill colour="var(--accent-text)">Draft</Pill>
                </div>
                {policy.kind === 'firewall_rule' && policy.firewall && (
                  <div style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
                    Concrete scope: <strong>IPv4 only</strong> · <strong>{policy.firewall.source_zone}</strong>
                    {policy.firewall.source_macs?.length ? ` · MAC ${policy.firewall.source_macs.join(', ')}` : ''}
                    {' '}→ <strong>{policy.firewall.destination_zone || 'gateway input'}</strong>
                    {' '}· {policy.firewall.action}. It affects new routed flows; existing conntrack
                    sessions may continue until expiry. Router input such as DHCP/DNS and same-L2
                    traffic are outside this forwarded-zone rule.
                  </div>
                )}
                <pre aria-label={`Concrete rule for ${policy.name}`} style={codeStyle}>{JSON.stringify(policyRulePayload(policy), null, 2)}</pre>
                <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                  <Button
                    kind="primary"
                    disabled={savingDraft !== null || savedDrafts.has(index)}
                    onClick={() => saveCompiledDraft(policy, index)}
                  >
                    {savedDrafts.has(index) ? 'Saved to desired state' : savingDraft === index ? 'Saving…' : 'Save this draft'}
                  </Button>
                  <span style={hintStyle}>Separate operator action; Preview and Apply still required.</span>
                </div>
              </section>
            ))}
            {onReviewChanges && compiledResult.drafts.some((_, index) => savedDrafts.has(index)) && (
              <div><Button onClick={onReviewChanges}>Go to Preview / Apply</Button></div>
            )}
          </div>
        </Card>
      )}

      <Card title="Client desired policy">
        <div style={{ color: 'var(--text-secondary)', fontSize: 12, marginBottom: 10 }}>
          Block, fixed IP and group are controller desired state. Group membership feeds Object Manager.
          Client Block covers IPv4 and IPv6 traffic routed from managed zones to every destination,
          including a routed foreign management LAN. Router input such as DHCP/DNS and same-L2 traffic are excluded.
          It affects new routed flows; existing conntrack sessions may continue until expiry.
        </div>
        {clients && clients.length > 0 && (
          <div className="policy-client-picker">
            <SelectField
              label={clientsCoverage ? 'Client (partial client inventory)' : 'Client'}
              value={clientChoice}
              onChange={setClientChoice}
            >
              {clients.map((candidate) => (
                <option key={candidate.mac} value={candidate.mac}>
                  {candidate.name || 'Unnamed client'} · {candidate.mac}
                </option>
              ))}
            </SelectField>
            <Button disabled={!clientChoice} onClick={() => setSelectedClient(clientChoice)}>Edit selected client</Button>
          </div>
        )}
        {clients && clients.length === 0 && (
          <div style={{ ...hintStyle, marginTop: 8 }}>
            {clientsCoverage
              ? 'No clients were present in the returned subset.'
              : 'No observed clients are available.'}
          </div>
        )}
      </Card>

      {client && (
        <ClientPolicyEditor
          client={client}
          onClose={() => setSelectedClient(null)}
          onSaved={async (updated) => {
            setClients((current) => (current ?? []).map((candidate) =>
              candidate.mac.toLowerCase() === updated.mac.toLowerCase()
                ? { ...candidate, blocked: updated.blocked, fixed_ip: updated.fixed_ip, group: updated.group }
                : candidate))
            setSourceRevision((current) => current + 1)
            try {
              await onPoliciesChanged(`${updated.mac} client policy saved as desired state.`)
            } catch (error) {
              throw new Error(`Client policy saved, but the policy view could not refresh: ${message(error)}`)
            }
            setSelectedClient(null)
          }}
        />
      )}
    </div>
  )
}

function ClientPolicyEditor({ client, onClose, onSaved }: {
  client: Client
  onClose: () => void
  onSaved: (client: { mac: string; blocked: boolean; fixed_ip?: string; group?: string }) => Promise<void>
}) {
  const [blocked, setBlocked] = useState(client.blocked)
  const [fixedIP, setFixedIP] = useState(client.fixed_ip ?? '')
  const [group, setGroup] = useState(client.group ?? '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function save(event: FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      const response = await api.saveClientPolicy(client.mac, {
        blocked,
        fixed_ip: fixedIP.trim(),
        group: group.trim(),
      })
      await onSaved(response.client)
    } catch (requestError) {
      setError(message(requestError))
    } finally {
      setBusy(false)
    }
  }

  return (
    <SlideOver title={`Client policy · ${client.name || client.mac}`} onClose={() => !busy && onClose()}>
      <form onSubmit={save} style={{ display: 'grid', gap: 13 }}>
        <div style={hintStyle}><code>{client.mac}</code>{client.ipv4 ? ` · observed ${client.ipv4}` : ''}</div>
        {error && <div role="alert"><Banner tone="critical">{error}</Banner></div>}
        <label style={checkStyle}>
          <input type="checkbox" checked={blocked} disabled={busy} onChange={(event) => setBlocked(event.target.checked)} />
          Block IPv4 and IPv6 managed-zone forwarded traffic
        </label>
        <div style={hintStyle}>
          Covers every routed destination, including a foreign management LAN. Router input such as
          DHCP/DNS and same-L2 traffic are excluded. Blocking affects new routed flows; existing
          conntrack sessions may continue until expiry.
        </div>
        <Field label="Fixed IPv4 (blank clears)" placeholder="192.168.20.50" disabled={busy} value={fixedIP} onChange={(event) => setFixedIP(event.target.value)} />
        <Field label="Client group (blank clears)" maxLength={128} disabled={busy} value={group} onChange={(event) => setGroup(event.target.value)} />
        <Banner tone="accent">
          Save records desired state only. Preview shows concrete firewall and DHCP sections per gateway.
        </Banner>
        <div style={{ display: 'flex', gap: 8 }}>
          <Button kind="primary" type="submit" disabled={busy}>{busy ? 'Saving…' : 'Save client policy'}</Button>
          <Button disabled={busy} onClick={onClose}>Cancel</Button>
        </div>
      </form>
    </SlideOver>
  )
}

function CapabilityStrip({ capabilities }: { capabilities: PolicyCapability[] }) {
  if (capabilities.length === 0) {
    return (
      <Banner tone="warning">
        This controller did not return policy capability gates. Preview remains authoritative;
        no backend is assumed available.
      </Banner>
    )
  }
  return (
    <Card title="Policy backends">
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
        {capabilities.map((capability) => (
          <span key={capability.kind} title={capability.reason} style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
            <Pill colour={capability.available ? 'var(--good)' : 'var(--warning)'}>
              {kindLabel(capability.kind)} · {capability.available ? 'available' : 'gated'}
            </Pill>
          </span>
        ))}
      </div>
      {capabilities.filter((capability) => !capability.available && capability.reason).map((capability) => (
        <div key={`${capability.kind}-reason`} style={{ ...hintStyle, marginTop: 7 }}>
          <strong>{kindLabel(capability.kind)}:</strong> {capability.reason}
        </div>
      ))}
    </Card>
  )
}

function OutcomeToggle({
  title,
  detail,
  checked,
  disabled,
  unavailable,
  onChange,
}: {
  title: string
  detail: string
  checked: boolean
  disabled: boolean
  unavailable?: boolean
  onChange: (checked: boolean) => void
}) {
  return (
    <label style={{
      display: 'grid', gridTemplateColumns: 'auto 1fr', gap: 8, alignItems: 'start',
      padding: 10, border: `1px solid ${checked ? 'var(--accent)' : 'var(--border)'}`,
      borderRadius: 7, background: checked ? 'var(--accent-soft)' : 'var(--surface-0)',
      cursor: disabled ? 'default' : 'pointer',
    }}>
      <input type="checkbox" checked={checked} disabled={disabled} onChange={(event) => onChange(event.target.checked)} />
      <span>
        <span style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, fontWeight: 600 }}>
          {title}{unavailable && <span style={{ color: 'var(--warning)', fontSize: 10 }}>GATED</span>}
        </span>
        <span style={{ display: 'block', color: 'var(--text-muted)', fontSize: 11, marginTop: 2 }}>{detail}</span>
      </span>
    </label>
  )
}

function SelectField({ label, value, disabled, onChange, children }: {
  label: string
  value: string
  disabled?: boolean
  onChange: (value: string) => void
  children: ReactNode
}) {
  return (
    <label style={{ display: 'block' }}>
      <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginBottom: 4 }}>{label}</div>
      <select value={value} disabled={disabled} onChange={(event) => onChange(event.target.value)} style={{
        width: '100%', height: 30, padding: '0 9px', borderRadius: 6,
        background: 'var(--surface-0)', border: '1px solid var(--border-strong)',
        color: 'var(--text-primary)', fontSize: 12,
      }}>
        {children}
      </select>
    </label>
  )
}

function ProtocolChecks({ label, choices, selected, disabled, onChange }: {
  label: string
  choices: string[]
  selected: string[]
  disabled: boolean
  onChange: (selected: string[]) => void
}) {
  return (
    <fieldset style={fieldsetStyle}>
      <legend style={legendStyle}>{label}</legend>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10 }}>
        {choices.map((choice) => (
          <label key={choice} style={checkStyle}>
            <input
              type="checkbox"
              checked={selected.includes(choice)}
              disabled={disabled}
              onChange={(event) => {
                if (choice === 'all') {
                  onChange(event.target.checked ? ['all'] : [])
                  return
                }
                const withoutAll = selected.filter((protocol) => protocol !== 'all')
                onChange(event.target.checked
                  ? [...withoutAll, choice]
                  : withoutAll.filter((protocol) => protocol !== choice))
              }}
            />
            {choice.toUpperCase()}
          </label>
        ))}
      </div>
    </fieldset>
  )
}

function DestinationToggle({ name, label, checked, disabled, onChange }: {
  name: string
  label: string
  checked: boolean
  disabled: boolean
  onChange: (checked: boolean) => void
}) {
  return (
    <label style={checkStyle}>
      <input
        type="checkbox"
        name={name}
        checked={checked}
        disabled={disabled}
        onChange={(event) => onChange(event.target.checked)}
      />
      {label}
    </label>
  )
}

function emptyPolicy(kind: PolicyKind): Policy {
  const base: Policy = { id: 0, order: 0, name: '', kind, origin: 'manual', enabled: true }
  if (kind === 'firewall_rule') {
    base.firewall = { action: 'reject', source_zone: 'wan', destination_zone: '', protocols: ['all'] }
  } else if (kind === 'port_forward') {
    base.port_forward = {
      source_zone: 'wan', destination_zone: '', protocols: ['tcp'],
      external_port: 0, destination_ip: '', destination_port: 0,
    }
  } else {
    base.static_route = { network_id: 0, target: '', gateway: '', metric: 0 }
  }
  return base
}

function policyFromRow(row: PolicyRow): Policy | null {
  if (!row.record_id || (row.kind !== 'firewall_rule' && row.kind !== 'port_forward' && row.kind !== 'static_route')) return null
  const policy: Policy = {
    id: row.record_id,
    order: row.order,
    name: row.name,
    kind: row.kind,
    origin: row.origin === 'object_manager' ? 'object_manager' : 'manual',
    enabled: row.enabled,
  }
  if (row.kind === 'firewall_rule') policy.firewall = row.rule as FirewallRule
  if (row.kind === 'port_forward') policy.port_forward = row.rule as PortForward
  if (row.kind === 'static_route') policy.static_route = row.rule as StaticRoute
  return clonePolicy(policy)
}

function clonePolicy(policy: Policy): Policy {
  return JSON.parse(JSON.stringify(policy)) as Policy
}

function cleanPolicy(policy: Policy): Omit<Policy, 'id'> & { id?: number } {
  const common = {
    id: policy.id || undefined,
    order: policy.order,
    name: policy.name.trim(),
    kind: policy.kind,
    origin: policy.origin,
    enabled: policy.enabled,
  }
  if (policy.kind === 'firewall_rule') {
    const firewall = policy.firewall!
    return {
      ...common,
      firewall: {
        ...firewall,
        source_cidr: firewall.source_cidr?.trim() || undefined,
        destination_cidr: firewall.destination_cidr?.trim() || undefined,
        source_port: firewall.source_port?.trim() || undefined,
        destination_port: firewall.destination_port?.trim() || undefined,
        source_macs: firewall.source_macs?.length ? firewall.source_macs : undefined,
      },
    }
  }
  if (policy.kind === 'port_forward') {
    return {
      ...common,
      port_forward: {
        ...policy.port_forward!,
        destination_ip: policy.port_forward!.destination_ip.trim(),
        source_cidr: policy.port_forward!.source_cidr?.trim() || undefined,
      },
    }
  }
  return {
    ...common,
    static_route: {
      ...policy.static_route!,
      target: policy.static_route!.target.trim(),
      gateway: policy.static_route!.gateway.trim(),
    },
  }
}

function validatePolicyDraft(policy: Policy): string {
  if (!policy.name.trim()) return 'Name is required.'
  if (policy.order < 0) return 'Display order cannot be negative.'
  if (policy.kind === 'firewall_rule') {
    if (!policy.firewall?.source_zone) return 'Source zone is required.'
    if (policy.firewall.protocols.length === 0) return 'Select at least one protocol.'
    if ((policy.firewall.source_port || policy.firewall.destination_port) &&
      !policy.firewall.protocols.some((protocol) => protocol === 'tcp' || protocol === 'udp')) {
      return 'Port matches require TCP or UDP.'
    }
  }
  if (policy.kind === 'port_forward') {
    const forward = policy.port_forward
    if (!forward?.destination_zone) return 'Destination zone is required.'
    if (forward.protocols.length === 0) return 'Select TCP and/or UDP.'
    if (!forward.external_port || !forward.destination_port || !forward.destination_ip.trim()) {
      return 'External port, destination IPv4 and destination port are required.'
    }
  }
  if (policy.kind === 'static_route' &&
    (!policy.static_route?.target.trim() || !policy.static_route.gateway.trim())) {
    return 'Target network and next-hop IPv4 are required.'
  }
  return ''
}

function policyRulePayload(policy: Policy) {
  if (policy.kind === 'firewall_rule') return policy.firewall
  if (policy.kind === 'port_forward') return policy.port_forward
  return policy.static_route
}

function toggleDestination(current: string[], destination: string, checked: boolean) {
  return checked
    ? current.includes(destination) ? current : [...current, destination]
    : current.filter((value) => value !== destination)
}

function toggleOutcome(
  current: Array<PolicyObjectOutcome['kind']>,
  outcome: PolicyObjectOutcome['kind'],
  checked: boolean,
) {
  return checked
    ? current.includes(outcome) ? current : [...current, outcome]
    : current.filter((value) => value !== outcome)
}

function splitList(raw: string) {
  return raw.split(/[\s,]+/).map((value) => value.trim()).filter(Boolean)
}

function numberValue(raw: string) {
  const value = Number(raw)
  return Number.isFinite(value) ? value : 0
}

function message(error: unknown) {
  return error instanceof Error ? error.message : String(error)
}

function kindLabel(value: string) {
  const labels: Record<string, string> = {
    zone_forward: 'Zone forwarding',
    firewall_rule: 'Firewall (IPv4)',
    port_forward: 'Port forward (IPv4)',
    static_route: 'Static route (IPv4)',
    client_block: 'Client Block (managed-zone forwarding only)',
    fixed_ip: 'Fixed IP',
    legacy_default: 'Legacy default',
    zone_matrix: 'Zone Matrix',
    object_manager: 'Object Manager',
    manual: 'Manual',
    client: 'Client',
    firewall: 'Firewall',
    nat: 'NAT',
    route: 'Routes',
    fixed_ip_capability: 'Fixed IP',
    qos: 'QoS',
    rate_limit: 'Rate limit',
    application: 'Application / DPI',
    priority: 'Rule priority',
  }
  return labels[value] ?? value.replaceAll('_', ' ')
}

function originLabel(origin: PolicyRow['origin'] | Policy['origin']) {
  return kindLabel(origin)
}

function scopeLabel(scope: string) {
  const labels: Record<string, string> = {
    zone_forwarding: 'Zone forwarding',
    firewall: 'Firewall',
    network_route: 'Network route',
    dhcp: 'DHCP',
    display_only: 'Display only',
  }
  return labels[scope] ?? scope
}

function scopeText(row: PolicyRow) {
  if (row.kind === 'client_block') {
    const mac = stringScope(row, 'client_mac') || 'client'
    return `${mac}: new IPv4 + IPv6 flows routed from managed zones to every destination, including foreign management LAN; excludes DHCP/DNS/router input and same-L2; existing conntrack sessions may continue until expiry`
  }
  const values = Object.entries(row.effective_scope).map(([key, value]) => {
    const rendered = Array.isArray(value) ? value.join(', ') : String(value ?? '—')
    return `${key.replaceAll('_', ' ')}: ${rendered || '—'}`
  })
  const rendered = values.join(' · ') || 'Site'
  if ((row.kind === 'firewall_rule' || row.kind === 'port_forward' || row.kind === 'static_route') &&
    !('address_families' in row.effective_scope)) {
    return `address families: ipv4 · ${rendered}`
  }
  return rendered
}

function stringScope(row: PolicyRow, key: string) {
  const value = row.effective_scope[key]
  return typeof value === 'string' ? value : ''
}

function objectLabel(
  object: PolicyObjectTarget,
  currentOptions: Array<{ id: string; label: string }>,
  clients: Client[],
  networks: SiteNetwork[],
) {
  if (object.kind === 'device') {
    const client = clients.find((candidate) => candidate.mac.toLowerCase() === object.id.toLowerCase())
    return client ? `${client.name || 'Unnamed client'} · ${client.mac}` : object.id
  }
  if (object.kind === 'network') {
    if (object.id === 'wan') return WAN_LABEL
    const network = networks.find((candidate) => String(candidate.id) === object.id)
    return network?.name ?? object.id
  }
  return currentOptions.find((option) => option.id === object.id)?.label ?? object.id
}

const headerCell = {
  padding: '9px 10px', borderBottom: '1px solid var(--border)',
  borderRight: '1px solid var(--border)', background: 'var(--surface-2)',
  color: 'var(--text-secondary)', fontSize: 11, fontWeight: 600,
  textAlign: 'left' as const, whiteSpace: 'nowrap' as const,
}

const rowHeaderCell = {
  ...headerCell, minWidth: 140, background: 'var(--surface-1)', color: 'var(--text-primary)',
}

const matrixCell = {
  minWidth: 130, padding: 0, borderRight: '1px solid var(--border)',
  borderBottom: '1px solid var(--border)', textAlign: 'center' as const,
}

const editableCell = {
  width: '100%', minHeight: 58, padding: 8, border: 0,
  background: 'transparent', color: 'inherit', cursor: 'pointer',
}

const readOnlyCell = {
  minHeight: 58, padding: 8, display: 'flex', alignItems: 'center',
  justifyContent: 'center', background: 'var(--surface-0)',
}

const fieldsetStyle = { display: 'grid', gap: 9, border: 0, padding: 0, margin: 0 }
const legendStyle = { fontSize: 12, fontWeight: 600, marginBottom: 6 }
const checkStyle = { display: 'flex', alignItems: 'center', gap: 7, fontSize: 12 }
const hintStyle = { color: 'var(--text-muted)', fontSize: 11 }
const stepTitleStyle = { marginBottom: 8, color: 'var(--text-primary)', fontSize: 12, fontWeight: 600 }

const chipStyle = {
  display: 'inline-flex', alignItems: 'center', gap: 6, minHeight: 26,
  padding: '2px 4px 2px 9px', border: '1px solid var(--border-strong)',
  borderRadius: 999, background: 'var(--surface-2)', color: 'var(--text-primary)', fontSize: 11,
}

const chipRemoveStyle = {
  width: 20, height: 20, border: 0, borderRadius: '50%', background: 'transparent',
  color: 'var(--text-secondary)', cursor: 'pointer', fontSize: 15, lineHeight: 1,
}

const draftCardStyle = {
  display: 'grid', gap: 10, padding: 12, border: '1px solid var(--border)',
  borderRadius: 7, background: 'var(--surface-0)',
}

const codeStyle = {
  margin: 0, padding: 10, overflowX: 'auto' as const, border: '1px solid var(--border)',
  borderRadius: 6, background: 'var(--surface-1)', color: 'var(--text-secondary)',
  fontSize: 11, lineHeight: 1.5, whiteSpace: 'pre-wrap' as const,
}

const tableActionStyle = {
  minHeight: 26,
  padding: '2px 9px',
  border: '1px solid var(--border-strong)',
  borderRadius: 5,
  background: 'var(--surface-2)',
  color: 'var(--accent-text)',
  cursor: 'pointer',
  fontSize: 11,
  fontWeight: 600,
}
