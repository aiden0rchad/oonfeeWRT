import { useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api'
import type {
  CapEffect, Device, DeviceDetail, OverheadReport, Point, ReprobeResult, Series,
} from '../lib/api'
import {
  Card, DataGrid, SlideOver, Status, Prop, Unknown, Banner, Button,
  useColumnPrefs,
} from '../components/ui'
import type { Column } from '../components/ui'
import { TimeChart, fmt, ago, duration } from '../components/Chart'
import { live } from '../lib/live'
import { Unadopt } from './Unadopt'
import type { LiveStats } from '../lib/live'

export function Devices({
  devices,
  onAdopt,
  onChanged,
}: {
  devices: Device[]
  onAdopt?: () => void
  onChanged?: () => void
}) {
  // Column preferences, the same as Clients and Logs have.
  //
  // Their absence here was not a decision, and it read as a broken feature
  // rather than a missing one: without onPrefsChange the header is not
  // `draggable` at all, so someone who tried to drag a column on the screen
  // they look at most got no reordering, no picker, and not even the tooltip
  // that says dragging is possible. Nothing anywhere said why.
  const [colPrefs, setColPrefs] = useColumnPrefs('devices')
  const [openID, setOpenID] = useState<number | null>(null)

  const columns: Column<Device>[] = [
    {
      key: 'status',
      header: 'Status',
      width: 110,
      render: (d) => <Status value={d.status} />,
      sortBy: (d) => d.status,
    },
    { key: 'name', header: 'Name', render: (d) => d.name || d.mac, sortBy: (d) => d.name },
    { key: 'host', header: 'Address', render: (d) => d.host, sortBy: (d) => d.host },
    {
      key: 'class',
      header: 'Class',
      render: (d) =>
        d.class ? d.class : <Unknown why="the capability probe has not classified this device" />,
      sortBy: (d) => d.class ?? '',
    },
    {
      key: 'fw',
      header: 'Firmware',
      render: (d) => d.firmware || <Unknown why="not read yet" />,
      sortBy: (d) => d.firmware,
    },
    {
      key: 'tier',
      header: 'Poll',
      render: (d) => (
        <span style={{ color: d.tier === 'focused' ? 'var(--accent)' : undefined }}>
          {d.quiesced ? 'paused (applying)' : (d.tier ?? d.poll_state)}
        </span>
      ),
      sortBy: (d) => d.tier ?? '',
    },
    {
      key: 'seen',
      header: 'Last seen',
      numeric: true,
      render: (d) =>
        d.last_seen ? ago(d.last_seen) : <Unknown why="this device has never been polled successfully" />,
      sortBy: (d) => d.last_seen ?? 0,
    },
  ]

  return (
    <>
      <Card
        title={`Devices (${devices.length})`}
        actions={onAdopt && <Button onClick={onAdopt}>Adopt a device</Button>}
        pad={false}
      >
        <DataGrid
          rows={devices}
          columns={columns}
          rowKey={(d) => d.mac}
          onRowClick={(d) => setOpenID(d.id)}
          prefs={colPrefs}
          onPrefsChange={setColPrefs}
          empty="No devices yet. Adopt one to get started."
        />
      </Card>
      {openID !== null && (
        <DeviceDetailPanel
          id={openID}
          onClose={() => setOpenID(null)}
          onRemoved={() => {
            setOpenID(null)
            onChanged?.()
          }}
        />
      )}
    </>
  )
}

/**
 * The device slide-over.
 *
 * While it is open it holds a FOCUS on the device, re-posted on a timer. That
 * is what raises the poll rate from 60 s to 5-10 s, and it is deliberately a
 * lease rather than an acquire/release pair: a closed laptop lid never runs
 * cleanup, and a focus that had to be explicitly released would pin a router at
 * the fast rate forever.
 */
function DeviceDetailPanel({
  id,
  onClose,
  onRemoved,
}: {
  id: number
  onClose: () => void
  onRemoved: () => void
}) {
  const [removing, setRemoving] = useState(false)
  const [detail, setDetail] = useState<DeviceDetail | null>(null)
  const [series, setSeries] = useState<Record<string, string[]>>({})
  const [overhead, setOverhead] = useState<OverheadReport | null>(null)
  const [stats, setStats] = useState<LiveStats | null>(null)
  const [err, setErr] = useState('')

  // Provenance per INTERFACE, not per SSID.
  //
  // Keyed on the interface because two BSSes can carry the same SSID and have
  // different owners — which is exactly the case an SSID-keyed lookup got
  // wrong. Joining the live AP list to the detail response on the SSID string
  // was the same mistake in the other direction.
  const originOf = new Map(
    (detail?.broadcasting ?? []).map((b) => [b.iface, b] as const),
  )

  // Hoisted out of the effect so a re-probe can refresh the pane: a probe
  // rewrites the capability record, and leaving the panel showing the previous
  // one is how "I pressed re-probe and nothing happened" happens.
  const load = useCallback(async () => {
    try {
      const [d, s] = await Promise.all([api.device(id), api.deviceSeries(id)])
      setDetail(d)
      setSeries(s.series)
      // Not fatal: a device in the inventory but not yet polled has no
      // overhead to report, which is a real state rather than zero cost.
      api.overhead(id).then(setOverhead).catch(() => {})
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [id])

  useEffect(() => {
    let alive = true
    load()

    // Watching the device IS the focus. The server reference-counts it on the
    // subscription, so closing this panel — or closing the tab, or losing the
    // network — releases it exactly. The renewal timer this replaced could only
    // ever approximate that.
    const unwatch = live.watch(id)
    const off = live.on((msg) => {
      if (!alive) return
      if (msg.type === 'stats' && (msg as LiveStats).device_id === id) {
        setStats(msg as LiveStats)
      }
    })
    // The slower things — the series index and the overhead totals — still come
    // from REST, because they change on the scale of minutes and pushing them
    // would be noise.
    const refresh = setInterval(load, 30_000)
    return () => {
      alive = false
      off()
      unwatch()
      clearInterval(refresh)
    }
  }, [id, load])

  if (err) {
    return (
      <SlideOver title="Device" onClose={onClose}>
        <Banner tone="critical">{err}</Banner>
      </SlideOver>
    )
  }
  if (!detail) {
    return (
      <SlideOver title="Device" onClose={onClose}>
        <div style={{ color: 'var(--text-secondary)', fontSize: 12 }}>Loading…</div>
      </SlideOver>
    )
  }

  const quirks = detail.capabilities?.Quirks ?? []
  const wanKey =
    detail.interfaces.find((i) => i === 'wan') ??
    detail.interfaces.find((i) => i.startsWith('eth')) ??
    detail.interfaces[0]

  if (removing) {
    return (
      <SlideOver title={`Remove ${detail.name || detail.mac}`} onClose={onClose}>
        <Unadopt
          deviceID={id}
          deviceName={detail.name || detail.mac}
          onDone={onRemoved}
          onCancel={() => setRemoving(false)}
        />
      </SlideOver>
    )
  }

  return (
    <SlideOver title={detail.name || detail.mac} onClose={onClose}>
      <div style={{ display: 'grid', gap: 6 }}>
        <Prop label="Status">
          <Status value={detail.status} />
        </Prop>
        <Prop label="Address">{detail.host}</Prop>
        <Prop label="MAC">{detail.mac}</Prop>
        <Prop label="Firmware">
          {detail.firmware || <Unknown why="not read yet" />}
        </Prop>
        <Prop label="Class">
          <DeviceClass cls={detail.class} target={detail.capabilities?.Board?.Target} />
        </Prop>
        <Prop label="Poll rate">
          {/* The live frame wins: `detail` comes from a REST refresh every 30 s
              and would show the tier this panel had before it subscribed. */}
          {detail.quiesced
            ? 'paused for an apply'
            : (stats?.tier ?? detail.tier ?? detail.poll_state)}
        </Prop>
        <Prop label="Last seen">
          {stats ? 'just now (live)' : detail.last_seen ? ago(detail.last_seen) : <Unknown why="never polled" />}
        </Prop>
        {stats && (
          <>
            <Prop label="Load average">{stats.load1.toFixed(2)}</Prop>
            {stats.mem_pct !== undefined && (
              <Prop label="Memory">{stats.mem_pct.toFixed(0)}%</Prop>
            )}
            <Prop label="Clients">
              {stats.clients === null ? (
                <Unknown why="an access point could not report its client count" />
              ) : (
                stats.clients
              )}
            </Prop>
            <Prop label="Poll time">{stats.poll_ms} ms</Prop>
          </>
        )}
      </div>

      {stats && stats.aps.length > 0 && (
        <div>
          {/* One row per BSS, not per radio.
              
              This said "Radios" and listed `stats.aps`, which is one entry per
              broadcasting interface. On a two-radio AP carrying two SSIDs that
              rendered four "radios"; two of them had the same SSID on different
              bands and were told apart only by a channel number; and the
              airtime figure appeared twice per radio, which reads as two
              measurements of one quantity rather than one channel's occupancy
              reported by each BSS sitting on it. */}
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>
            Broadcasting
          </div>
          <div style={{ display: 'grid', gap: 6 }}>
            {stats.aps.map((ap) => (
              <div key={ap.iface} style={{ fontSize: 12 }}>
                <div style={{ display: 'flex', justifyContent: 'space-between' }}>
                  <span>
                    {ap.ssid || ap.iface}{' '}
                    <span style={{ color: 'var(--text-muted)' }}>
                      {ap.iface} · ch {ap.channel}
                    </span>
                    {originOf.get(ap.iface)?.origin === 'foreign' && (
                      <span style={{ color: 'var(--warning)', marginLeft: 6 }}>
                        unmanaged
                      </span>
                    )}
                    {originOf.get(ap.iface)?.origin === 'unknown' && (
                      <Unknown why="this device did not report which config section created this interface, so who owns it could not be determined. That is not the same as it being unmanaged." />
                    )}
                  </span>
                  <span className="num">
                    {ap.clients === null ? (
                      <Unknown why="this interface did not report a client count" />
                    ) : (
                      `${ap.clients} client${ap.clients === 1 ? '' : 's'}`
                    )}
                  </span>
                </div>
                {ap.airtime_pct !== undefined && (
                  <div style={{ color: 'var(--text-secondary)', fontSize: 11 }}>
                    channel {ap.channel} is {ap.airtime_pct.toFixed(1)}% busy
                  </div>
                )}
                {/* Spelled out rather than hidden in a hover title. A tooltip
                    is unreachable on a touch device and invisible to anyone
                    not already suspicious, and this is the sentence that
                    explains why a button to change it does not exist. */}
                {originOf.get(ap.iface)?.origin === 'foreign' && (
                  <div style={{ color: 'var(--text-secondary)', fontSize: 11 }}>
                    oonfeeWRT did not create this network — it is section{' '}
                    <code>{originOf.get(ap.iface)?.section ?? 'unknown'}</code>{' '}
                    on the device, from before adoption or made by hand. The
                    controller leaves config it did not write alone, so changing
                    or removing this means editing the device directly.
                  </div>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {stats && stats.stations.length > 0 && (
        <div>
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>
            Associated now
          </div>
          <div style={{ display: 'grid', gap: 4 }}>
            {stats.stations.map((st) => (
              <div key={st.mac} style={{ display: 'flex', justifyContent: 'space-between', fontSize: 11 }}>
                <span style={{ color: 'var(--text-secondary)' }}>{st.mac}</span>
                <span className="num">{st.signal} dBm</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {!stats && (
        <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          Opening this panel subscribes to this device, which raises its poll
          rate. Live values appear on the next poll — a few seconds.
        </div>
      )}

      {wanKey && (
        <ChartBlock
          title={`Throughput — ${wanKey}`}
          deviceID={id}
          kind="iface_rx_bps"
          seriesKey={wanKey}
          format={fmt.bytesPerSec}
          colour="var(--series-1)"
        />
      )}
      {series['chan_busy_pct']?.map((radio) => (
        <ChartBlock
          key={radio}
          title={`Channel utilization — ${radio}`}
          deviceID={id}
          kind="chan_busy_pct"
          seriesKey={radio}
          format={fmt.percent}
          colour="var(--series-3)"
        />
      ))}
      <ChartBlock
        title="Load average"
        deviceID={id}
        kind="sys_load1"
        seriesKey=""
        format={fmt.plain}
        colour="var(--series-4)"
      />

      {overhead && (
        <ManagementOverhead
          report={overhead}
          deviceID={id}
          onChanged={() => api.overhead(id).then(setOverhead).catch(() => {})}
        />
      )}

      {detail.degraded && detail.degraded.length > 0 && (
        <div>
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>
            What the controller cannot read here
          </div>
          <div style={{ display: 'grid', gap: 6 }}>
            {detail.degraded.map((g) => (
              <div
                key={g.call}
                style={{
                  fontSize: 11,
                  color: 'var(--text-secondary)',
                  borderLeft: `2px solid ${g.permanent ? 'var(--warning)' : 'var(--border-strong)'}`,
                  paddingLeft: 8,
                }}
              >
                <code style={{ color: 'var(--text-primary)' }}>{g.call}</code>{' '}
                — {g.error}
                {g.costs && <div>{g.costs}</div>}
              </div>
            ))}
          </div>
          <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>
            These are standing limits of this device's access-control file or
            driver, not failures. Polling works; these particular answers are
            unavailable, and each line says what that costs.
          </div>
        </div>
      )}

      <Reprobe deviceID={id} onProbed={load} />

      <div style={{ borderTop: '1px solid var(--border)', paddingTop: 12 }}>
        <Button onClick={() => setRemoving(true)}>Remove from controller</Button>
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>
          Hands the device's configuration back and deletes the controller's
          login and ACL file.
        </div>
      </div>

      {quirks.length > 0 && (
        <div>
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>
            Driver quirks
          </div>
          <div style={{ display: 'grid', gap: 6 }}>
            {quirks.map((q, i) => (
              <div
                key={i}
                style={{
                  fontSize: 11,
                  color: 'var(--text-secondary)',
                  borderLeft: '2px solid var(--warning)',
                  paddingLeft: 8,
                }}
              >
                <code style={{ color: 'var(--text-primary)' }}>
                  {q.Source}.{q.Field}
                </code>
                <br />
                {q.Reason}
              </div>
            ))}
          </div>
          <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>
            Metrics derived from these fields are not rendered anywhere. A field
            that is present and wrong is worse than one that is missing.
          </div>
        </div>
      )}
    </SlideOver>
  )
}

/**
 * What the controller costs this device.
 *
 * DEVICE-BUDGET §7 asks for this explicitly: "UniFi never shows you this, and
 * the reason it can afford not to is that it owns the hardware. We don't.
 * Surfacing our own cost is both the honest thing to do and a real feature —
 * it turns 'is this thing slowing down my router?' from an anxiety into a
 * number the user can read and act on."
 */
/**
 * What the controller costs this device (DEVICE-BUDGET §7).
 *
 * The CPU figure is derived, not sampled, and says so. A baseline poll costs
 * about 5 ms of device CPU once a minute — roughly fifty times below the
 * device's own idle CPU — so a live sample would be reporting noise with a
 * decimal point on it. The number comes from a control experiment instead, and
 * the tooltip carries the whole basis rather than a reassuring word.
 */
function ManagementOverhead({
  report,
  deviceID,
  onChanged,
}: {
  report: OverheadReport
  deviceID: number
  onChanged: () => void
}) {
  const o = report.overhead
  const budget = o.tier === 'focused' ? 6 : 1
  const overBudget = o.polls_per_minute > budget * 1.05
  const [saving, setSaving] = useState(false)

  async function setInterval(seconds: number) {
    setSaving(true)
    try {
      await api.setPollInterval(deviceID, seconds)
      onChanged()
    } finally {
      setSaving(false)
    }
  }

  return (
    <div>
      <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>
        Management overhead
      </div>
      <div style={{ display: 'grid', gap: 6 }}>
        <Prop label="Poll interval">
          {o.quiesced
            ? 'paused for an apply'
            : `${o.interval_seconds.toFixed(0)}s (${o.tier})`}
        </Prop>
        <Prop label="Requests to this device">
          <span style={{ color: overBudget ? 'var(--warning)' : undefined }}>
            {o.polls_per_minute.toFixed(2)}/min
          </span>
        </Prop>
        <Prop label="Device CPU used">
          {o.cpu_percent_of_core != null ? (
            <span title={o.cpu_basis}>
              {o.cpu_percent_of_core < 0.01
                ? '<0.01'
                : o.cpu_percent_of_core.toFixed(2)}
              % of one core
              <span style={{ color: 'var(--text-muted)' }}>
                {' '}
                ({o.cpu_ms_per_poll?.toFixed(1)} ms/poll, derived)
              </span>
            </span>
          ) : (
            <Unknown why={o.cpu_basis} />
          )}
        </Prop>
        {/* Not "packages installed" — this is what the CONTROLLER installed,
            which is always nothing, and is reported so that claim can be
            checked rather than believed. Under the old label the value read as
            a statement about the device, which for any real router is plainly
            false and made the field look broken. */}
        <Prop label="Packages we installed">
          {report.packages.length === 0 ? (
            <span title={report.packages_note}>none</span>
          ) : (
            report.packages.join(', ')
          )}
        </Prop>
        <Prop label="Data sent">{formatBytes(o.bytes_out)}</Prop>
        <Prop label="Polls">
          {o.polls}
          {o.failed_polls > 0 && (
            <span style={{ color: 'var(--warning)' }}> ({o.failed_polls} failed)</span>
          )}
        </Prop>
      </div>

      {/* The control DEVICE-BUDGET §7 asks for. It only loosens: every option
          is at or above the default, because a knob that could raise the rate
          would turn the budget into a suggestion no test measures. */}
      <div style={{ marginTop: 10 }}>
        <div style={{ fontSize: 11, color: 'var(--text-secondary)', marginBottom: 4 }}>
          Poll this device less often
        </div>
        <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
          {[
            { label: 'Default (60s)', s: 0 },
            { label: '2 min', s: 120 },
            { label: '5 min', s: 300 },
            { label: '15 min', s: 900 },
          ].map((opt) => (
            <button
              key={opt.s}
              disabled={saving}
              onClick={() => setInterval(opt.s)}
              style={{
                fontSize: 11,
                padding: '2px 8px',
                borderRadius: 4,
                cursor: saving ? 'default' : 'pointer',
                border: '1px solid var(--border-strong)',
                background:
                  report.poll_interval_s === opt.s
                    ? 'var(--accent-soft)'
                    : 'transparent',
                color: 'var(--text-primary)',
              }}
            >
              {opt.label}
            </button>
          ))}
        </div>
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
          {report.poll_interval_note} Charts get coarser as the interval grows,
          and a device can be down for up to one interval before it is noticed.
        </div>
      </div>

      <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 8 }}>
        Budget is one request per minute idle, one per 10 seconds while this
        panel is open. Opening it raises the rate; closing it lowers it within
        30 seconds.
        {o.non_poll_requests > 5 && (
          <>
            {' '}
            <strong style={{ color: 'var(--warning)' }}>
              {o.non_poll_requests} requests were not polls
            </strong>{' '}
            — that should only be session logins.
          </>
        )}
      </div>
    </div>
  )
}

function formatBytes(n: number): string {
  const u = ['B', 'kB', 'MB', 'GB']
  let i = 0
  while (n >= 1000 && i < u.length - 1) {
    n /= 1000
    i++
  }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${u[i]}`
}

function ChartBlock({
  title,
  deviceID,
  kind,
  seriesKey,
  format,
  colour,
}: {
  title: string
  deviceID: number
  kind: string
  seriesKey: string
  format: (v: number) => string
  colour: string
}) {
  const [data, setData] = useState<Series | null>(null)
  const [range, setRange] = useState<1 | 24 | 168>(1)
  const [window, setWindow] = useState<[number, number] | undefined>()

  const load = useCallback(async () => {
    const now = Math.floor(Date.now() / 1000)
    const from = now - range * 3600
    setWindow([from, now])
    try {
      setData(await api.stats(kind, deviceID, seriesKey, from, now))
    } catch {
      setData(null)
    }
  }, [kind, deviceID, seriesKey, range])

  useEffect(() => {
    load()
    const t = setInterval(load, 30_000)
    return () => clearInterval(t)
  }, [load])

  const points: Point[] = data?.points ?? []
  return (
    <div>
      <div
        style={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
          marginBottom: 6,
        }}
      >
        <span style={{ fontSize: 12, fontWeight: 600 }}>{title}</span>
        <span style={{ display: 'flex', gap: 4 }}>
          {([1, 24, 168] as const).map((h) => (
            <button
              key={h}
              onClick={() => setRange(h)}
              style={{
                fontSize: 11,
                padding: '2px 7px',
                borderRadius: 4,
                cursor: 'pointer',
                border: '1px solid var(--border-strong)',
                background: range === h ? 'var(--accent-soft)' : 'transparent',
                color: 'var(--text-primary)',
              }}
            >
              {h === 1 ? '1h' : h === 24 ? '1D' : '1W'}
            </button>
          ))}
        </span>
      </div>
      <TimeChart
        points={points}
        label={title}
        format={format}
        colour={colour}
        height={140}
        resolution={data?.resolution}
        window={window}
      />
    </div>
  )
}

export { duration, Button }

/**
 * Re-probe this device's capabilities.
 *
 * The controller probes at adoption and again whenever the firmware changes.
 * This is the manual path, for the cases the automatic trigger cannot see: a
 * package installed, an ACL widened, a radio added.
 *
 * The interesting part of the result is not the list of changes but their
 * classification. "802.11r can no longer be checked" and "802.11r is gone" look
 * identical in the raw states and mean completely different things — the first
 * is almost always a narrowed ACL on a device that is fine. Rendering them the
 * same colour would recreate, in the UI, exactly the bug the three-state
 * capability model exists to prevent.
 */
function Reprobe({ deviceID, onProbed }: { deviceID: number; onProbed: () => void }) {
  const [busy, setBusy] = useState(false)
  const [res, setRes] = useState<ReprobeResult | null>(null)
  const [err, setErr] = useState('')

  const run = async () => {
    setBusy(true)
    setErr('')
    try {
      const r = await api.reprobe(deviceID)
      setRes(r)
      onProbed()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const changes = res?.changes ?? []

  return (
    <div style={{ borderTop: '1px solid var(--border)', paddingTop: 12 }}>
      <Button onClick={run} disabled={busy}>
        {busy ? 'Probing…' : 'Re-probe capabilities'}
      </Button>
      <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>
        Re-reads what this device can do. Runs automatically after a firmware
        change; do it by hand after installing a package or widening the ACL.
        It is a burst of reads, so polling pauses while it runs.
      </div>

      {err && (
        <div style={{ marginTop: 8 }}>
          <Banner tone="critical">{err}</Banner>
        </div>
      )}

      {res?.role_fit && res.role_fit.length > 0 && (
        <div style={{ marginTop: 8, display: 'grid', gap: 6 }}>
          {res.role_fit.map((r) => (
            <Banner key={r} tone="warning">
              {r}
            </Banner>
          ))}
        </div>
      )}

      {res?.unchanged && (
        <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginTop: 8 }}>
          Probed — nothing changed. {res.summary}
        </div>
      )}

      {changes.length > 0 && (
        <div style={{ marginTop: 10, display: 'grid', gap: 6 }}>
          {changes.map((c, i) => (
            <div
              key={i}
              style={{
                fontSize: 11,
                borderLeft: `2px solid ${effectTone(c.effect)}`,
                paddingLeft: 8,
              }}
            >
              <div style={{ display: 'flex', gap: 8, alignItems: 'baseline' }}>
                <code style={{ color: 'var(--text-primary)' }}>{c.name}</code>
                <span style={{ color: effectTone(c.effect) }}>
                  {effectLabel(c.effect)}
                </span>
              </div>
              <div style={{ color: 'var(--text-secondary)' }}>{c.detail}</div>
            </div>
          ))}
          {res && res.actionable === 0 && (
            <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
              None of these change what this device can be sent — they are
              changes in what the controller can see.
            </div>
          )}
        </div>
      )}
    </div>
  )
}

/** Colour by what the change licenses you to conclude, not by whether it reads
 *  as good news. A visibility change is muted because the device did not
 *  change; showing it in the "lost" colour sends someone hunting a fault. */
function effectTone(e: CapEffect): string {
  switch (e) {
    case 'gained':
      return 'var(--good)'
    case 'lost':
      return 'var(--critical)'
    case 'changed':
      return 'var(--warning)'
    default:
      return 'var(--text-muted)'
  }
}

function effectLabel(e: CapEffect): string {
  switch (e) {
    case 'now-observable':
      return 'visible now (may have been there all along)'
    case 'no-longer-observable':
      return 'can no longer be checked — not a loss'
    case 'first-observation':
      return 'first reading'
    default:
      return e
  }
}

/**
 * The DEVICE-BUDGET hardware class, and what "?" actually means.
 *
 * `?` is not "the probe failed" and not "unclassifiable hardware". It means the
 * SoC family has never been measured by this project — `classify()` covers
 * mvebu, filogic/MT7981 and MT7621, and everything else is most old routers:
 * ath79, ramips/MT7620, ipq40xx, bcm53xx, lantiq. Rendering a bare `?` invites
 * an operator to go looking for a fault; naming the target tells them what the
 * controller is actually looking at, which is the only thing that would let
 * them (or anyone) close the gap.
 *
 * Deliberately NOT solved by adding targets to the map. A class carries a CPU
 * and RAM budget, and assigning one from a family nobody has measured would be
 * a guess wearing a measurement's clothes. The consequence of `?` is mild and
 * correct: the conservative poll default, and no CPU figure claimed.
 */
export function DeviceClass({ cls, target }: { cls?: string | null; target?: string }) {
  if (!cls) {
    return <Unknown why="the capability probe has not classified this device" />
  }
  if (cls !== '?') {
    return <>{cls}</>
  }
  return (
    <span>
      ?{' '}
      <span style={{ color: 'var(--text-muted)', fontSize: 11 }}>
        {target ? `— ${target} ` : ''}has not been measured, so this device is
        polled at the conservative default and no CPU cost is claimed for it
      </span>
    </span>
  )
}
