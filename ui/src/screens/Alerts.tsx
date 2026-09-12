import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { Device, SessionInfo } from '../lib/api'
import type { AlertCondition, AlertResponse, AlertRule, AlertRuleInput } from '../lib/alerts'
import { Banner, Button, Card, Field, PageHeader } from '../components/ui'
import './Alerts.css'

const conditions: Record<AlertCondition, { label: string; unit: string; remedy: string }> = {
  device_offline: { label: 'Device offline', unit: '', remedy: 'Check power, the Ethernet uplink, management reachability, and controller credentials. A collection gap alone does not confirm recovery.' },
  wan_latency: { label: 'High WAN latency', unit: 'ms', remedy: 'Check WAN utilization and wired latency. Compare Statistics with local gateway reachability before changing your ISP or QoS settings.' },
  wan_loss: { label: 'WAN packet loss', unit: '%', remedy: 'Check the WAN cable, modem and provider status. The probe target may also limit ICMP; packet loss is not a diagnosis on its own.' },
}
const newRule = (): AlertRuleInput => ({ name: '', condition: 'device_offline', device_id: 0, threshold: 0, hold_seconds: 300, cooldown_seconds: 3600, enabled: false })
const when = (value: number | null) => value == null ? 'Not yet' : new Date(value * 1000).toLocaleString()

export function Alerts({ devices, session }: { devices: Device[]; session: SessionInfo }) {
  const owner = session.role === 'owner'
  const [data, setData] = useState<AlertResponse | null>(null)
  const [now, setNow] = useState(() => Date.now() / 1000)
  const [loadError, setLoadError] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState<number | null>(null)
  const [showForm, setShowForm] = useState(false)
  const [draft, setDraft] = useState<AlertRuleInput>(newRule)
  const [confirmDelete, setConfirmDelete] = useState<number | null>(null)
  const [deliveryEnabled, setDeliveryEnabled] = useState(false)
  const [url, setURL] = useState('')
  const [token, setToken] = useState('')
  const [clearToken, setClearToken] = useState(false)
  const [confirmClear, setConfirmClear] = useState(false)
  const generation = useRef(0)
  const request = useRef<AbortController | null>(null)

  const refresh = useCallback(async () => {
    const revision = ++generation.current
    request.current?.abort()
    const controller = new AbortController()
    request.current = controller
    try {
      const result = await api.alerts(controller.signal)
      if (revision !== generation.current) return
      setData(result)
      setLoadError('')
    } catch (cause) {
      if (revision === generation.current && !controller.signal.aborted) setLoadError(cause instanceof Error ? cause.message : String(cause))
    }
  }, [])
  useEffect(() => {
    void refresh()
    const timer = window.setInterval(() => { setNow(Date.now() / 1000); void refresh() }, 30_000)
    return () => { clearInterval(timer); generation.current++; request.current?.abort() }
  }, [refresh])
  useEffect(() => { setDeliveryEnabled(data?.delivery.enabled ?? false) }, [data?.delivery.enabled])

  async function mutate(action: () => Promise<unknown>, message: string) {
    setBusy(true); setError(''); setNotice('')
    try { await action(); setNotice(message); await refresh(); return true }
    catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); return false }
    finally { setBusy(false) }
  }

  function edit(rule?: AlertRule) {
    setEditing(rule?.id ?? null)
    setDraft(rule ? { name: rule.name, condition: rule.condition, device_id: rule.device_id, threshold: rule.threshold,
      hold_seconds: rule.hold_seconds, cooldown_seconds: rule.cooldown_seconds, enabled: rule.enabled } : { ...newRule(), device_id: devices[0]?.id ?? 0 })
    setShowForm(true); setError(''); setNotice('')
  }
  const rules = data?.rules ?? []
  const incidents = data?.incidents ?? []
  const evaluationStale = data?.evaluated_at != null && (now - data.evaluated_at > 180 || data.evaluated_at - now > 60)

  return <div className="alerts-page">
    <PageHeader title="Alerts" purpose="Know what needs attention, why it matters, and what to check next."
      actions={<><Button onClick={() => void refresh()} disabled={busy}>Refresh</Button>{owner && <Button kind="primary" onClick={() => edit()} disabled={busy || !data}>Create rule</Button>}</>} />
    {loadError && <div role="alert"><Banner tone="critical">Alerts could not refresh: {loadError}{data && '. Last successful state remains visible.'}</Banner></div>}
    {error && <div role="alert"><Banner tone="critical">{error}</Banner></div>}
    {notice && <div role="status" className="alerts-notice">{notice}</div>}
    {!data && !loadError && <div role="status">Loading alert rules…</div>}
    {data && <>
      <div className="alerts-summary">
        <div><span>Rules</span><strong>{rules.length}</strong><small>{rules.filter((rule) => rule.enabled).length} enabled</small></div>
        <div><span>Need attention</span><strong>{data.counts?.open_incidents ?? incidents.filter((incident) => incident.state === 'firing').length}</strong><small>{data.counts ? 'Open retained incidents' : 'Open incidents shown'}</small></div>
        <div><span>Delivery</span><strong className="alerts-summary-word">{data.delivery.enabled && data.delivery.configured ? 'Webhook' : 'In app'}</strong><small>{data.delivery.enabled && data.delivery.configured ? data.delivery.host : 'No external notifications enabled'}</small></div>
      </div>
      {(data.evaluated_at == null || evaluationStale) && <p className="alerts-hint" role="status">{data.evaluated_at == null
        ? 'Waiting for the first rule evaluation. Stored rule state is not a new check.'
        : 'Rule evaluation is not recent. These are retained states, not current health confirmation. Check the controller process and refresh.'}</p>}
      {!owner && <p className="alerts-hint">You can review rules and incidents. Only the controller owner can change rules or notification delivery.</p>}
      {showForm && owner && <Card title={editing == null ? 'Create an alert rule' : 'Edit alert rule'}>
        <form onSubmit={async (event) => {
          event.preventDefault()
          if (await mutate(() => api.saveAlertRule(draft, editing ?? undefined), 'Alert rule saved.')) setShowForm(false)
        }}>
          <fieldset disabled={busy} className="alerts-fields">
            <Field label="Rule name" value={draft.name} required maxLength={80} onChange={(event) => setDraft({ ...draft, name: event.target.value })} />
            <label>Device<select value={draft.device_id || ''} required onChange={(event) => setDraft({ ...draft, device_id: Number(event.target.value) })}>
              <option value="" disabled>Select a device</option>{devices.map((device) => <option key={device.id} value={device.id}>{device.name || device.host}</option>)}
            </select></label>
            <label>Condition<select value={draft.condition} onChange={(event) => {
              const condition = event.target.value as AlertCondition
              setDraft({ ...draft, condition, threshold: condition === 'device_offline' ? 0 : condition === 'wan_latency' ? 100 : 5 })
            }}>{Object.entries(conditions).map(([id, info]) => <option key={id} value={id}>{info.label}</option>)}</select></label>
            {draft.condition !== 'device_offline' && <Field label={`Above (${conditions[draft.condition].unit})`} type="number" required min="0.01" max={draft.condition === 'wan_loss' ? 100 : 60000} step="any"
              value={draft.threshold} onChange={(event) => setDraft({ ...draft, threshold: Number(event.target.value) })} />}
            <Field label="Sustained for (minutes)" type="number" required min="1" max="1440" step="1" value={draft.hold_seconds / 60}
              onChange={(event) => setDraft({ ...draft, hold_seconds: Number(event.target.value) * 60 })} />
            <Field label="Notification cooldown (minutes)" type="number" required min="1" max="10080" step="1" value={draft.cooldown_seconds / 60}
              onChange={(event) => setDraft({ ...draft, cooldown_seconds: Number(event.target.value) * 60 })} />
          </fieldset>
          <p className="alerts-hint">{conditions[draft.condition].remedy} WAN rules need fresh stored gateway probes; missing measurements remain unknown.</p>
          <label className="alerts-checkbox"><input type="checkbox" checked={draft.enabled} disabled={busy} onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })} />Enable this rule</label>
          <div className="alerts-actions"><Button type="submit" kind="primary" disabled={busy || !draft.device_id}>{busy ? 'Saving…' : 'Save rule'}</Button><Button disabled={busy} onClick={() => setShowForm(false)}>Cancel</Button></div>
        </form>
      </Card>}
      <section className="alert-rules" aria-label="Alert rules">
        {rules.length === 0 && <Card title="A quieter network starts with a few useful rules"><p className="alerts-hint">Start with a gateway offline rule, then add sustained latency or packet loss if those matter to you. Short spikes and missing observations do not need to become a wall of alarms.</p></Card>}
        {rules.map((rule) => <article className="alert-rule" key={rule.id}>
          <div className="alert-rule-heading"><div><h2>{rule.name}</h2><p>{devices.find((device) => device.id === rule.device_id)?.name || `Device ${rule.device_id}`} · {conditions[rule.condition]?.label ?? rule.condition}</p></div>
            <span className="alert-state" data-state={rule.state}>{rule.state === 'clear' ? 'Clear' : rule.state === 'unknown' ? 'Awaiting evidence' : rule.state === 'pending' ? 'Observing' : rule.state === 'firing' ? 'Needs attention' : 'Disabled'}</span></div>
          <p className="alert-rule-reason">{rule.reason || (rule.enabled ? 'Waiting for evaluation.' : 'Enable this rule to begin evaluation.')}</p>
          <div className="alert-rule-facts"><span>Sustained {rule.hold_seconds / 60} min</span><span>Cooldown {rule.cooldown_seconds / 60} min</span>{rule.condition !== 'device_offline' && <span>Threshold &gt; {rule.threshold} {conditions[rule.condition].unit}</span>}</div>
          <details><summary>What to check</summary><p>{conditions[rule.condition]?.remedy}</p><p>Last evidence: {when(rule.observed_at)}. Missing evidence never confirms that an incident has recovered.</p></details>
          {owner && <div className="alerts-actions">
            <Button disabled={busy} onClick={() => edit(rule)}>Edit</Button>
            <Button disabled={busy} onClick={() => void mutate(() => api.saveAlertRule({ name: rule.name, device_id: rule.device_id, condition: rule.condition, threshold: rule.threshold, hold_seconds: rule.hold_seconds, cooldown_seconds: rule.cooldown_seconds, enabled: !rule.enabled }, rule.id), rule.enabled ? 'Rule disabled.' : 'Rule enabled.')}>{rule.enabled ? 'Disable' : 'Enable'}</Button>
            {confirmDelete === rule.id ? <><span>Delete this rule?</span><Button disabled={busy} onClick={async () => { if (await mutate(() => api.deleteAlertRule(rule.id), 'Rule deleted.')) setConfirmDelete(null) }}>Confirm delete</Button><Button onClick={() => setConfirmDelete(null)} disabled={busy}>Cancel</Button></>
              : <Button disabled={busy} onClick={() => setConfirmDelete(rule.id)}>Delete</Button>}
          </div>}
        </article>)}
      </section>
      <Card title="Recent incidents">
        <p className="alerts-hint">Newest 100 retained incidents. Unknown evidence is not a recovery signal. Last evaluation: {when(data.evaluated_at)}.</p>
        {incidents.length === 0 ? <p>No retained incidents.</p> : <div className="alert-incidents">{incidents.map((incident) => <article key={incident.id}>
          <div><strong>{incident.rule_name}</strong><p>{incident.device_name} · {when(incident.started_at)}</p></div>
          <div><span className="alert-state" data-state={incident.state}>{incident.state === 'resolved' ? 'Recovered' : 'Open'}</span><p>{incident.resolved_at ? `Recovered ${when(incident.resolved_at)}` : 'No confirmed recovery'}</p></div>
          <div><span>Delivery: {incident.delivery_state.replaceAll('_', ' ')}</span>{incident.delivery_error && <p>{incident.delivery_error}</p>}</div>
        </article>)}</div>}
      </Card>
      <Card title="Notification delivery">
        <p className="alerts-hint">Rules work in app without an external service. An enabled webhook sends incident details, including device and rule names, to the destination you configure. No automatic test notification is sent when you save.</p>
        <div className="alert-delivery-status"><span>Destination: <strong>{data.delivery.configured ? data.delivery.host : 'Not configured'}</strong></span><span>Last successful delivery: {when(data.delivery.last_success_at)}</span></div>
        {data.delivery.last_error && <p role="alert">Delivery needs attention: {data.delivery.last_error}</p>}
        {owner && <form onSubmit={async (event) => {
          event.preventDefault()
          const settings = { enabled: deliveryEnabled, ...(url ? { url } : {}), ...(clearToken ? { bearer_token: '' } : token ? { bearer_token: token } : {}) }
          const saved = await mutate(() => api.saveAlertDelivery(settings), 'Notification delivery saved.')
          setToken(''); setURL(''); if (saved) setClearToken(false)
        }}>
          <fieldset className="alerts-fields" disabled={busy}>
            <Field label={data.delivery.configured ? 'Replace webhook URL (optional)' : 'HTTPS webhook URL'} type="url" value={url} placeholder="https://notifications.example/webhook" autoComplete="off" onChange={(event) => setURL(event.target.value)} />
            <Field label="Replace bearer token (optional)" type="password" value={token} autoComplete="new-password" onChange={(event) => { setToken(event.target.value); setClearToken(false) }} />
          </fieldset>
          <p className="alerts-hint">HTTPS public destinations only. Redirects and private-network targets are blocked. Existing URL and token stay unchanged when their fields are blank.</p>
          <label className="alerts-checkbox"><input type="checkbox" checked={clearToken} disabled={busy} onChange={(event) => { setClearToken(event.target.checked); setToken('') }} />Remove the stored bearer token</label>
          <label className="alerts-checkbox"><input type="checkbox" checked={deliveryEnabled} disabled={busy} onChange={(event) => setDeliveryEnabled(event.target.checked)} />Enable external notification delivery</label>
          <div className="alerts-actions"><Button type="submit" kind="primary" disabled={busy}>Save delivery settings</Button>
            {data.delivery.configured && (confirmClear ? <><span>Remove this destination?</span><Button disabled={busy} onClick={async () => {
              if (await mutate(() => api.saveAlertDelivery({ enabled: false, url: '', bearer_token: '' }), 'Webhook removed.')) { setConfirmClear(false); setURL(''); setToken('') }
            }}>Confirm removal</Button><Button disabled={busy} onClick={() => setConfirmClear(false)}>Cancel</Button></> : <Button disabled={busy} onClick={() => setConfirmClear(true)}>Remove webhook</Button>)}
          </div>
        </form>}
      </Card>
    </>}
  </div>
}
