import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import type {
  Broadcast,
  CapEffect,
  Degradation,
  Device,
  DeviceDetail,
  OverheadReport,
  Point,
  ReprobeResult,
  Series,
  DeviceFunction,
  LLDPCapabilityResult,
} from '../lib/api'
import { Card, DataGrid, SlideOver, Status, Prop, Unknown, Banner, Button, Notice, PageHeader, useColumnPrefs } from '../components/ui'
import type { Column } from '../components/ui'
import { TimeChart, fmt, ago, duration } from '../components/Chart'
import { live } from '../lib/live'
import { Unadopt } from './Unadopt'
import type { LiveStats } from '../lib/live'

export function Devices({
  devices,
  devicesLoaded = true,
  devicesError = '',
  onAdopt,
  onChanged,
}: {
  devices: Device[]
  devicesLoaded?: boolean
  devicesError?: string
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
  const count = devicesLoaded
    ? devices.length.toLocaleString()
    : devicesError
      ? 'Unavailable'
      : '…'
  const empty = !devicesLoaded
    ? devicesError
      ? 'Device inventory is unavailable. Retry when the controller is reachable.'
      : 'Loading devices…'
    : devicesError
      ? 'No devices were present in the last successful inventory.'
      : 'No devices yet. Adopt one to get started.'

  const columns: Column<Device>[] = [
    {
      key: 'status',
      header: 'Status',
      width: 110,
      render: (d) => <Status value={d.status} />,
      sortBy: (d) => d.status,
    },
    {
      key: 'name',
      header: 'Name',
      render: (d) => d.name || d.mac,
      sortBy: (d) => d.name,
    },
    {
      key: 'functions',
      header: 'Functions',
      render: (d) => functionNames(deviceFunctions(d)),
      sortBy: (d) => deviceFunctions(d).join(','),
    },
    {
      key: 'host',
      header: 'Address',
      render: (d) => d.host,
      sortBy: (d) => d.host,
    },
    {
      key: 'class',
      header: 'Class',
      render: (d) => (d.class ? d.class : <Unknown why="the capability probe has not classified this device" />),
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
        <span style={{ color: d.tier === 'focused' ? 'var(--accent-text)' : undefined }}>
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
    <div style={{ display: 'grid', gap: 14 }}>
      <PageHeader
        title="Devices"
        purpose="Managed OpenWrt inventory, adoption status, and live device details."
        actions={onAdopt && <Button onClick={onAdopt}>Adopt a device</Button>}
      />
      <Card
        title={`Managed devices (${count})`}
        pad={false}
      >
        <DataGrid
          tableLabel="Managed devices"
          totalRows={devicesLoaded ? devices.length : undefined}
          rows={devices}
          columns={columns}
          rowKey={(d) => d.mac}
          onRowClick={(d) => setOpenID(d.id)}
          prefs={colPrefs}
          onPrefsChange={setColPrefs}
          empty={empty}
        />
      </Card>
      {openID !== null && (
        <DeviceDetailPanel
          id={openID}
          onClose={() => setOpenID(null)}
          onChanged={() => onChanged?.()}
          onRemoved={() => {
            setOpenID(null)
            onChanged?.()
          }}
        />
      )}
    </div>
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
export function DeviceDetailPanel({
  id,
  onClose,
  onChanged,
  onRemoved,
}: {
  id: number
  onClose: () => void
  onChanged: () => void
  onRemoved: () => void
}) {
  const [removing, setRemoving] = useState(false)
  const [detail, setDetail] = useState<DeviceDetail | null>(null)
  const [series, setSeries] = useState<Record<string, string[]>>({})
  const [overhead, setOverhead] = useState<OverheadReport | null>(null)
  const [stats, setStats] = useState<LiveStats | null>(null)
  const [err, setErr] = useState('')
  const [seriesErr, setSeriesErr] = useState('')
  const loadGeneration = useRef(0)

  // Provenance per INTERFACE, not per SSID.
  //
  // Keyed on the interface because two BSSes can carry the same SSID and have
  // different owners — which is exactly the case an SSID-keyed lookup got
  // wrong. Joining the live AP list to the detail response on the SSID string
  // was the same mistake in the other direction.
  const originOf = new Map((detail?.broadcasting ?? []).map((b) => [b.iface, b] as const))

  // Hoisted out of the effect so a re-probe can refresh the pane: a probe
  // rewrites the capability record, and leaving the panel showing the previous
  // one is how "I pressed re-probe and nothing happened" happens.
  const load = useCallback(async () => {
    const generation = ++loadGeneration.current
    const [detailResult, seriesResult] = await Promise.allSettled([api.device(id), api.deviceSeries(id)])
    if (generation !== loadGeneration.current) return
    if (detailResult.status === 'fulfilled') {
      setErr('')
      setDetail(detailResult.value)
      // Not fatal: a device in the inventory but not yet polled has no
      // overhead to report, which is a real state rather than zero cost.
      api
        .overhead(id)
        .then(setOverhead)
        .catch(() => {})
    } else {
      setErr(detailResult.reason instanceof Error ? detailResult.reason.message : String(detailResult.reason))
    }
    if (seriesResult.status === 'fulfilled') {
      setSeries(seriesResult.value.series)
      setSeriesErr('')
    } else {
      setSeriesErr(seriesResult.reason instanceof Error ? seriesResult.reason.message : String(seriesResult.reason))
    }
  }, [id])

  const refresh = useCallback(async () => {
    await load()
    onChanged()
  }, [load, onChanged])

  useEffect(() => {
    let alive = true
    loadGeneration.current++
    setDetail(null)
    setSeries({})
    setOverhead(null)
    setStats(null)
    setErr('')
    setSeriesErr('')
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
      loadGeneration.current++
    }
  }, [id, load])

  // A WebSocket frame is a timestamped observation, not a permanent lease on
  // the word "live". If the socket or controller disappears, fall back to the
  // durable device state rather than showing old stations as associated now.
  useEffect(() => {
    if (!stats) return
    const now = Date.now()
    const delay = (stats.ts + 30) * 1000 - now
    if (delay <= 0 || stats.ts * 1000 > now + 30_000) {
      setStats(null)
      return
    }
    const timer = window.setTimeout(() => {
      setStats((current) => (current === stats ? null : current))
    }, delay)
    return () => window.clearTimeout(timer)
  }, [stats])

  // Only when there is nothing to show. A panel that has loaded keeps its
  // content and carries the error above it — the rule App, Logs and Clients all
  // state: keep the last good data on screen, because blanking it on one
  // dropped request is its own kind of lie.
  if (err && !detail) {
    return (
      <SlideOver title="Device" onClose={onClose}>
        <div role="alert">
          <Banner tone="critical">{err}</Banner>
        </div>
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
  const degradations = detail.degraded ?? []
  const standingDegradations = degradations.filter(
    (g) => g.permanent && ['permission', 'unsupported', 'device'].includes(g.cause),
  )
  const currentPollFailures = degradations.filter((g) => !standingDegradations.includes(g))
  const degradationRow = (g: Degradation) => (
    <div
      key={g.call}
      style={{
        fontSize: 11,
        color: 'var(--text-secondary)',
        borderLeft: `2px solid ${standingDegradations.includes(g) ? 'var(--warning)' : 'var(--critical)'}`,
        paddingLeft: 8,
      }}
    >
      <code style={{ color: 'var(--text-primary)' }}>{g.call}</code> — {g.error}
      <div style={{ color: 'var(--text-muted)' }}>
        Cause: {g.cause || 'unknown'}
        {g.status && ` · ubus ${g.status.name} (${g.status.code})`}
      </div>
      {g.costs && <div>{g.costs}</div>}
    </div>
  )
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
      {/* Above the content, not instead of it. What is on screen is the last
          reading that succeeded; this says the newest attempt did not. */}
      {err && (
        <Banner tone="warning">
          The last refresh failed ({err}). The readings below are from the last one that worked.
        </Banner>
      )}
      {seriesErr && (
        <Banner tone="warning">
          Metric catalog refresh failed ({seriesErr}). Core device facts remain available; charts use the last catalog
          that loaded successfully.
        </Banner>
      )}
      <div style={{ display: 'grid', gap: 6 }}>
        <Prop label="Status">
          <Status value={stats ? 'online' : detail.status} />
        </Prop>
        <Prop label="Name">
          <DeviceName detail={detail} onRenamed={refresh} />
        </Prop>
        <Prop label="Address">{detail.host}</Prop>
        <Prop label="MAC">{detail.mac}</Prop>
        <Prop label="Firmware">{detail.firmware || <Unknown why="not read yet" />}</Prop>
        <Prop label="Class">
          <DeviceClass cls={detail.class} target={detail.capabilities?.Board?.Target} />
        </Prop>
        <Prop label="Functions">
          {functionNames(deviceFunctions(detail))}
          {!detail.functions && <span title="derived from this older row's legacy role"> · legacy</span>}
        </Prop>
        <Prop label="Poll rate">
          {/* The live frame wins: `detail` comes from a REST refresh every 30 s
              and would show the tier this panel had before it subscribed. */}
          {detail.quiesced ? 'paused for an apply' : (stats?.tier ?? detail.tier ?? detail.poll_state)}
        </Prop>
        <Prop label="Last seen">
          {stats ? 'just now (live)' : detail.last_seen ? ago(detail.last_seen) : <Unknown why="never polled" />}
        </Prop>
        {stats && (
          <>
            <Prop label="Load average">{stats.load1.toFixed(2)}</Prop>
            {stats.mem_pct !== undefined && <Prop label="Memory">{stats.mem_pct.toFixed(0)}%</Prop>}
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
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>Reported enabled BSSs</div>
          <div
            style={{
              color: 'var(--text-muted)',
              fontSize: 11,
              marginBottom: 6,
            }}
          >
            Current hostapd/interface state—not an independent on-air scan.
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
                      <span style={{ color: 'var(--warning)', marginLeft: 6 }}>unmanaged</span>
                    )}
                    {/* No entry is the SAME answer as origin "unknown": the
                        controller has not been told who owns this BSS. It used
                        to fall through to nothing, which renders identically to
                        "ours" — and that is reachable without any device quirk.
                        This list is painted from the live frame while
                        provenance comes from the REST detail refreshed every
                        30s, so on a freshly adopted device, or for 30s after a
                        restart, every foreign SSID read as one we manage. */}
                    {(originOf.get(ap.iface)?.origin ?? 'unknown') === 'unknown' && (
                      <Unknown why="the controller has not been told which config section created this interface, so who owns it is not established. That is not the same as it being managed here." />
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
                  <TakeoverBriefBlock deviceID={id} b={originOf.get(ap.iface)!} onNoted={load} />
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {stats && stats.stations.length > 0 && (
        <div>
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>Associated now</div>
          <div style={{ display: 'grid', gap: 4 }}>
            {stats.stations.map((st) => (
              <div
                key={st.mac}
                style={{
                  display: 'flex',
                  justifyContent: 'space-between',
                  fontSize: 11,
                }}
              >
                <span style={{ color: 'var(--text-secondary)' }}>{st.mac}</span>
                <span className="num">
                  {st.signal === null ? <Unknown why="this station did not report signal" /> : `${st.signal} dBm`}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}

      {!stats && (
        <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          Opening this panel subscribes to this device, which raises its poll rate. Live values appear on the next poll
          — a few seconds.
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
      {/* Two utilization charts per radio, from two sources, because they are
          two measurements and only one of them has history.

          BSS load comes from hostapd.get_status, which runs on EVERY poll, so
          it is the one with a continuous line — and it is the figure hostapd
          advertises in its beacons, so it is what clients actually act on when
          deciding whether to roam. It was recorded on every poll and charted
          nowhere: 189 buckets sitting unused while the panel showed a live
          percentage from the same field in the card above.

          The survey figure is the driver's own channel occupancy and is finer,
          but iwinfo.survey is focused-tier only, so it is recorded while this
          panel is open and not otherwise. Measured on the reference device: 31
          buckets against 189, and the newest an hour old with the panel shut.
          Its empty message has to say that — the shared one ("telemetry is
          written every five minutes") told the operator to wait, and waiting
          means closing the panel, which is the one thing that guarantees it
          stays empty. Confirmed by opening the panel and watching a bucket
          appear.

          They agree closely but are NOT interchangeable — measured over paired
          buckets on both devices, the means are within 1.6 points while single
          buckets diverge by up to 16 on a busy 2.4 GHz radio. Neither is
          therefore a substitute for the other, which is why both are drawn.

          One chart per RADIO, not per BSS. Both quantities are properties of
          the radio, and both sources report them per interface, so a device
          with two SSIDs on a radio produced two identical series and the panel
          drew the same chart twice — four charts on the Archer C6, two of them
          duplicates to the decimal. */}
      {oneKeyPerRadio(series['ap_airtime_pct']).map((iface) => (
        <ChartBlock
          key={`bss-${iface}`}
          title={`Channel utilization — ${radioOf(iface)}`}
          deviceID={id}
          kind="ap_airtime_pct"
          seriesKey={iface}
          format={fmt.percent}
          colour="var(--series-3)"
          minSpan={1}
          note="BSS load, as hostapd advertises it to clients"
        />
      ))}
      {oneKeyPerRadio(series['chan_busy_pct']).map((iface) => (
        <ChartBlock
          key={`survey-${iface}`}
          title={`Channel occupancy (survey) — ${radioOf(iface)}`}
          deviceID={id}
          kind="chan_busy_pct"
          seriesKey={iface}
          format={fmt.percent}
          colour="var(--series-5)"
          minSpan={1}
          note="the driver's own busy/active ratio, read only while this panel is open"
          emptyNote={
            'Nothing in this window. The survey this comes from is too ' +
            'expensive for the idle budget, so it is recorded only while this ' +
            'panel is open. Leave it open — waiting with it closed will not ' +
            'fill this in. The chart above is recorded continuously.'
          }
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
          onChanged={() =>
            api
              .overhead(id)
              .then(setOverhead)
              .catch(() => {})
          }
        />
      )}

      {degradations.length > 0 && (
        <div>
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>What the controller cannot read here</div>
          {standingDegradations.length > 0 && (
            <div style={{ display: 'grid', gap: 6 }}>
              <div style={{ fontSize: 11, fontWeight: 600 }}>Permission or device limits</div>
              {standingDegradations.map(degradationRow)}
              <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
                These are standing limits: retrying the same call will not fix an ACL refusal or an operation the
                firmware or driver does not provide. Each line says what remains unavailable.
              </div>
            </div>
          )}
          {currentPollFailures.length > 0 && (
            <div
              style={{
                display: 'grid',
                gap: 6,
                marginTop: standingDegradations.length ? 10 : 0,
              }}
            >
              <div style={{ fontSize: 11, fontWeight: 600 }}>Current poll failures</div>
              {currentPollFailures.map(degradationRow)}
              <div role="note" style={{ fontSize: 11, color: 'var(--text-muted)' }}>
                These failures describe the latest poll, not a confirmed hardware or permission limit. The controller
                will try again; do not treat the missing values as proof that the device lacks the feature.
              </div>
            </div>
          )}
        </div>
      )}

      <ACLRefresh deviceID={id} onUpdated={refresh} />
      <LLDPCapability deviceID={id} onUpdated={refresh} />
      <Reprobe deviceID={id} onProbed={refresh} />

      <div style={{ borderTop: '1px solid var(--border)', paddingTop: 12 }}>
        <Button onClick={() => setRemoving(true)}>Remove from controller</Button>
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>
          Hands the device's configuration back and deletes the controller's login and ACL file.
        </div>
      </div>

      {quirks.length > 0 && (
        <div>
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>Driver quirks</div>
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
            Metrics derived from these fields are not rendered anywhere. A field that is present and wrong is worse than
            one that is missing.
          </div>
        </div>
      )}
    </SlideOver>
  )
}

/** Older inventory rows carry one role. Preserve their historical behavior
 * while presenting the same additive model as newly adopted devices. */
function deviceFunctions(device: Pick<Device, 'functions' | 'role'>): DeviceFunction[] {
  if (device.functions !== undefined) return device.functions
  if (device.role === 'gateway') return ['gateway', 'ap', 'switch']
  if (device.role === 'ap') return ['ap', 'switch']
  if (device.role === 'switch') return ['switch']
  return []
}

function functionNames(functions: DeviceFunction[]): string {
  if (functions.length === 0) return 'None — invalid record'
  const labels: Record<DeviceFunction, string> = {
    gateway: 'Gateway',
    ap: 'AP',
    switch: 'Switch',
  }
  return functions.map((item) => labels[item]).join(' · ')
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
  const [saveErr, setSaveErr] = useState('')

  async function setInterval(seconds: number) {
    setSaving(true)
    setSaveErr('')
    try {
      await api.setPollInterval(deviceID, seconds)
      onChanged()
    } catch (e) {
      setSaveErr(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div>
      <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>Management overhead</div>
      <div style={{ display: 'grid', gap: 6 }}>
        <Prop label="Poll interval">
          {o.quiesced ? 'paused for an apply' : `${o.interval_seconds.toFixed(0)}s (${o.tier})`}
        </Prop>
        <Prop label="Scheduled poll rate">
          <span style={{ color: overBudget ? 'var(--warning)' : undefined }}>{o.polls_per_minute.toFixed(2)}/min</span>
        </Prop>
        <Prop label="HTTP request rate">{o.requests_per_minute.toFixed(2)}/min</Prop>
        <Prop label="Device CPU used">
          {o.cpu_percent_of_core != null ? (
            <span title={o.cpu_basis}>
              {o.cpu_percent_of_core < 0.01 ? '<0.01' : o.cpu_percent_of_core.toFixed(2)}% of one core
              <span style={{ color: 'var(--text-muted)' }}> ({o.cpu_ms_per_poll?.toFixed(1)} ms/poll, derived)</span>
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
          {report.packages.length === 0 ? <span title={report.packages_note}>none</span> : report.packages.join(', ')}
        </Prop>
        <Prop label="Data sent">{formatBytes(o.bytes_out)}</Prop>
        <Prop label="Polls">
          {o.polls}
          {o.failed_polls > 0 && <span style={{ color: 'var(--warning)' }}> ({o.failed_polls} failed)</span>}
        </Prop>
      </div>

      {/* The control DEVICE-BUDGET §7 asks for. It only loosens: every option
          is at or above the default, because a knob that could raise the rate
          would turn the budget into a suggestion no test measures. */}
      <div style={{ marginTop: 10 }}>
        <div
          style={{
            fontSize: 11,
            color: 'var(--text-secondary)',
            marginBottom: 4,
          }}
        >
          Poll this device less often
        </div>
        <div
          role="group"
          aria-label="Poll this device less often"
          style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}
        >
          {[
            { label: 'Default (60s)', s: 0 },
            { label: '2 min', s: 120 },
            { label: '5 min', s: 300 },
            { label: '15 min', s: 900 },
          ].map((opt) => (
            <button
              key={opt.s}
              disabled={saving}
              aria-pressed={report.poll_interval_s === opt.s}
              onClick={() => setInterval(opt.s)}
              style={{
                fontSize: 11,
                padding: '2px 8px',
                borderRadius: 4,
                cursor: saving ? 'default' : 'pointer',
                border: '1px solid var(--border-strong)',
                background: report.poll_interval_s === opt.s ? 'var(--accent-soft)' : 'transparent',
                color: 'var(--text-primary)',
              }}
            >
              {opt.label}
            </button>
          ))}
        </div>
        {saveErr && (
          <div role="alert" style={{ marginTop: 6 }}>
            <Banner tone="critical">Poll interval was not changed: {saveErr}</Banner>
          </div>
        )}
        <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
          {report.poll_interval_note} Charts get coarser as the interval grows; the online/offline window scales with
          the effective interval.
        </div>
      </div>

      <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 8 }}>
        Budget is one request per minute idle, one per 10 seconds while this panel is open. Opening it raises the rate;
        closing it lowers it within 30 seconds.
        {o.non_poll_requests > 5 && (
          <>
            {' '}
            <strong style={{ color: 'var(--warning)' }}>{o.non_poll_requests} requests were not polls</strong> — this
            includes session setup and explicit actions such as discovery, capability probes, and RF scans; it is not
            evidence of unexpected logins by itself.
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

/** The radio an AP interface belongs to: "phy0-ap1" → "phy0". */
function radioOf(iface: string): string {
  const cut = iface.indexOf('-')
  return cut > 0 ? iface.slice(0, cut) : iface
}

/**
 * One series key per radio, in the order given.
 *
 * Channel utilization and BSS load are properties of the RADIO, and both
 * hostapd and iwinfo report them per interface — so a radio carrying two SSIDs
 * yields two series holding the same numbers. Measured on the Archer C6:
 * phy0-ap0 and phy0-ap1 agreed to the decimal across every paired bucket. The
 * panel drew both, so a two-SSID device showed each chart twice.
 *
 * Keeping the FIRST key rather than merging them: they are not two readings to
 * reconcile, they are one reading reported twice, and picking one is honest
 * about that in a way averaging would not be.
 */
function oneKeyPerRadio(keys: string[] | undefined): string[] {
  const seen = new Set<string>()
  return (keys ?? []).filter((k) => {
    const radio = radioOf(k)
    if (seen.has(radio)) return false
    seen.add(radio)
    return true
  })
}

function ChartBlock({
  title,
  deviceID,
  kind,
  seriesKey,
  format,
  colour,
  note,
  emptyNote,
  minSpan,
}: {
  title: string
  deviceID: number
  kind: string
  seriesKey: string
  format: (v: number, step?: number) => string
  colour: string
  /** What this series actually measures, when the title alone would let two
   *  charts of the same quantity be mistaken for each other. */
  note?: string
  emptyNote?: string
  minSpan?: number
}) {
  const [loaded, setLoaded] = useState<{
    data: Series
    range: 1 | 24 | 168
    window: [number, number]
  } | null>(null)
  const [range, setRange] = useState<1 | 24 | 168>(1)
  const [loadErr, setLoadErr] = useState('')
  const [loading, setLoading] = useState(false)
  const loadGeneration = useRef(0)

  const load = useCallback(async () => {
    const generation = ++loadGeneration.current
    const requestedRange = range
    const now = Math.floor(Date.now() / 1000)
    const from = now - requestedRange * 3600
    setLoading(true)
    try {
      const data = await api.stats(kind, deviceID, seriesKey, from, now)
      if (generation !== loadGeneration.current) return
      setLoaded({ data, range: requestedRange, window: [from, now] })
      setLoadErr('')
    } catch (e) {
      if (generation !== loadGeneration.current) return
      setLoadErr(e instanceof Error ? e.message : String(e))
    } finally {
      if (generation === loadGeneration.current) setLoading(false)
    }
  }, [kind, deviceID, seriesKey, range])

  useEffect(() => {
    load()
    const t = setInterval(load, 30_000)
    return () => {
      clearInterval(t)
      loadGeneration.current++
    }
  }, [load])

  const points: Point[] = loaded?.data.points ?? []
  const rangeLabel = (value: 1 | 24 | 168) => (value === 1 ? '1 hour' : value === 24 ? '1 day' : '1 week')
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
        {/* Title alone on this row. The note went here first and wrapped mid
            phrase into the 1h/1D/1W buttons — this is a flex row with
            space-between, so a sentence in it squeezes them. It belongs with
            the resolution footnote, which is the same kind of fact about the
            series. */}
        <span style={{ fontSize: 12, fontWeight: 600 }}>{title}</span>
        <span role="group" aria-label={`${title} time range`} style={{ display: 'flex', gap: 4 }}>
          {([1, 24, 168] as const).map((h) => (
            <button
              key={h}
              aria-pressed={range === h}
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
      {(loading || loadErr) && (
        <div
          role={loadErr ? 'alert' : 'status'}
          style={{
            fontSize: 11,
            color: loadErr ? 'var(--warning)' : 'var(--text-muted)',
            marginBottom: 5,
          }}
        >
          {loadErr
            ? `Could not refresh ${rangeLabel(range)} data: ${loadErr}.${loaded ? ` Showing the last successful ${rangeLabel(loaded.range)} response.` : ''}`
            : `Loading ${rangeLabel(range)} data…${loaded && loaded.range !== range ? ` Showing ${rangeLabel(loaded.range)} until it arrives.` : ''}`}
        </div>
      )}
      <TimeChart
        points={points}
        label={title}
        format={format}
        colour={colour}
        height={140}
        resolution={loaded?.data.resolution}
        window={loaded?.window}
        note={note}
        emptyNote={emptyNote}
        minSpan={minSpan}
      />
    </div>
  )
}

export { duration, Button }

function ACLRefresh({ deviceID, onUpdated }: { deviceID: number; onUpdated: () => void }) {
  const [open, setOpen] = useState(false)
  const [username, setUsername] = useState('root')
  const [password, setPassword] = useState('')
  const [privateKey, setPrivateKey] = useState('')
  const [acknowledged, setAcknowledged] = useState(false)
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')
  const [error, setError] = useState('')

  const run = async () => {
    if (!acknowledged) return
    setBusy(true)
    setError('')
    setMessage('')
    try {
      const result = await api.refreshACL(deviceID, {
        username,
        password,
        private_key: privateKey || undefined,
        acknowledge_router_changes: true,
      })
      setMessage(
        `oonfeeWRT controller access payload installed or refreshed and verified. ${result.features.length} capabilities are observable.`,
      )
      onUpdated()
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason))
    } finally {
      setPassword('')
      setPrivateKey('')
      setAcknowledged(false)
      setBusy(false)
    }
  }

  return (
    <div style={{ borderTop: '1px solid var(--border)', paddingTop: 12 }}>
      <Button
        onClick={() => {
          if (open) {
            setPassword('')
            setPrivateKey('')
          }
          setOpen(!open)
          setAcknowledged(false)
        }}
      >
        {open ? 'Cancel payload review' : 'Review or refresh controller access payload'}
      </Button>
      <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>
        This default-off action installs the payload if missing or replaces its one rpcd ACL JSON file on the router:{' '}
        <code>/usr/share/rpcd/acl.d/oonfeewrt.json</code>. It adds controller access to supported observations and
        permits later acknowledged Apply operations for controller-owned network, wireless, firewall and DHCP sections,
        plus managed 802.11k neighbour-list updates. It cannot disconnect or steer clients and installs no package,
        binary, daemon, service, or firmware. Leave it off or cancel to keep the router unchanged; blocked observations
        remain explicit gaps.
      </div>
      {open && (
        <div style={{ display: 'grid', gap: 8, marginTop: 10 }}>
          <label style={{ display: 'grid', gap: 3, fontSize: 11 }}>
            Device administrator username
            <input value={username} autoComplete="off" onChange={(event) => setUsername(event.target.value)} />
          </label>
          <label style={{ display: 'grid', gap: 3, fontSize: 11 }}>
            Device administrator password
            <input
              type="password"
              value={password}
              autoComplete="off"
              onChange={(event) => setPassword(event.target.value)}
            />
          </label>
          <label style={{ display: 'grid', gap: 3, fontSize: 11 }}>
            SSH private key (optional)
            <textarea
              value={privateKey}
              autoComplete="off"
              spellCheck={false}
              rows={4}
              onChange={(event) => setPrivateKey(event.target.value)}
            />
          </label>
          <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
            Credentials are used for this one SSH request only. The password and private key are never stored.
          </div>
          <label
            style={{
              display: 'flex',
              gap: 8,
              alignItems: 'start',
              fontSize: 11,
            }}
          >
            <input
              type="checkbox"
              checked={acknowledged}
              disabled={busy}
              onChange={(event) => setAcknowledged(event.target.checked)}
            />
            I understand that accepting installs the payload if missing or replaces the controller&apos;s single rpcd
            ACL JSON file and grants the read and write access described above.
          </label>
          <Button disabled={busy || username.trim() === '' || !acknowledged} onClick={() => void run()}>
            {busy ? 'Installing or refreshing payload…' : 'Install or refresh controller access payload and verify'}
          </Button>
        </div>
      )}
      {error && (
        <div role="alert" style={{ marginTop: 8 }}>
          <Banner tone="critical">{error}</Banner>
        </div>
      )}
      {message && (
        <div style={{ marginTop: 8 }}>
          <Banner tone="accent">{message}</Banner>
        </div>
      )}
    </div>
  )
}

function LLDPCapability({ deviceID, onUpdated }: { deviceID: number; onUpdated: () => void }) {
  const [status, setStatus] = useState<LLDPCapabilityResult | null>(null)
  const [open, setOpen] = useState(false)
  const [username, setUsername] = useState('root')
  const [password, setPassword] = useState('')
  const [privateKey, setPrivateKey] = useState('')
  const [indexAck, setIndexAck] = useState(false)
  const [changeAck, setChangeAck] = useState(false)
  const [diagnosticAck, setDiagnosticAck] = useState(false)
  const [configReadAck, setConfigReadAck] = useState(false)
  const [configChangeAck, setConfigChangeAck] = useState(false)
  const [configPlan, setConfigPlan] = useState<LLDPCapabilityResult | null>(null)
  const [plan, setPlan] = useState<LLDPCapabilityResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(() => {
    api
      .lldpCapability(deviceID)
      .then(setStatus)
      .catch((reason) => {
        setError(reason instanceof Error ? reason.message : String(reason))
      })
  }, [deviceID])

  useEffect(load, [load])
  const installed = status != null && status.state !== 'not_installed'
  const planningRemoval = installed

  const credentials = () => ({
    username,
    password,
    private_key: privateKey || undefined,
  })

  const resolvePlan = async () => {
    if (!planningRemoval && !indexAck) return
    setBusy(true)
    setError('')
    setPlan(null)
    setChangeAck(false)
    try {
      setPlan(
        await api.changeLLDPCapability(deviceID, {
          action: planningRemoval ? 'plan_remove' : 'plan_install',
          ...credentials(),
          acknowledge_package_index_refresh: planningRemoval ? undefined : true,
        }),
      )
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason))
      setPassword('')
      setPrivateKey('')
    } finally {
      setBusy(false)
    }
  }

  const applyPlan = async () => {
    if (!plan?.plan_hash || !changeAck) return
    setBusy(true)
    setError('')
    try {
      const next = await api.changeLLDPCapability(deviceID, {
        action: planningRemoval ? 'remove' : 'install',
        ...credentials(),
        plan_hash: plan.plan_hash,
        acknowledge_router_changes: true,
        acknowledge_package_index_refresh: planningRemoval ? undefined : true,
      })
      setStatus(next)
      setPlan(null)
      setOpen(false)
      onUpdated()
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason))
      load()
    } finally {
      setPassword('')
      setPrivateKey('')
      setIndexAck(false)
      setChangeAck(false)
      setBusy(false)
    }
  }

  const diagnose = async () => {
    if (!diagnosticAck) return
    setBusy(true)
    setError('')
    try {
      const next = await api.changeLLDPCapability(deviceID, {
        action: 'diagnose',
        ...credentials(),
        acknowledge_read_only_diagnostics: true,
      })
      setStatus(next)
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason))
    } finally {
      setPassword('')
      setPrivateKey('')
      setDiagnosticAck(false)
      setBusy(false)
    }
  }

  const resolveConfigPlan = async () => {
    if (!configReadAck) return
    setBusy(true)
    setError('')
    setConfigPlan(null)
    setConfigChangeAck(false)
    try {
      setConfigPlan(
        await api.changeLLDPCapability(deviceID, {
          action: 'plan_configure',
          ...credentials(),
          acknowledge_read_only_diagnostics: true,
        }),
      )
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason))
      setPassword('')
      setPrivateKey('')
    } finally {
      setBusy(false)
    }
  }

  const applyConfigPlan = async () => {
    if (!configPlan?.plan_hash || !configChangeAck) return
    setBusy(true)
    setError('')
    try {
      const next = await api.changeLLDPCapability(deviceID, {
        action: 'configure',
        ...credentials(),
        plan_hash: configPlan.plan_hash,
        acknowledge_router_changes: true,
      })
      setStatus(next)
      setConfigPlan(null)
      onUpdated()
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason))
      load()
    } finally {
      setPassword('')
      setPrivateKey('')
      setConfigReadAck(false)
      setConfigChangeAck(false)
      setBusy(false)
    }
  }

  return (
    <div style={{ borderTop: '1px solid var(--border)', paddingTop: 12 }}>
      <Notice
        tone={installed ? 'accent' : 'warning'}
        component="Optional LLDP topology capability"
        summary={installed
          ? 'LLDP wired-neighbour discovery is available. Review its controller-recorded package and service baseline before changing or removing it.'
          : 'Adds measured wired-neighbour discovery by installing OpenWrt lldpd. No router change occurs until an exact plan is accepted.'}
        defaultOpen={open}
        closedLabel="What this installs and rolls back"
        openLabel="Hide capability details"
        details={(
          <>
            <p style={{ margin: 0 }}>
              Installing adds the official OpenWrt <code>lldpd</code> package and dependencies shown in the exact
              package-manager plan, then enables and starts <code>lldpd</code>. It installs no controller binary or firmware.
              Removal uses the durable baseline, removes the exact controller-added package set, keeps every pre-existing
              package, and restores the prior <code>lldpd</code> service state.
            </p>
            {installed && (
              <p style={{ margin: '8px 0 0' }}>
                Controller record: {status?.state}. Controller-added packages:{' '}
                {status?.added_packages.join(', ') || 'none; lldpd existed before adoption'}.
              </p>
            )}
          </>
        )}
        actions={<Button
          aria-pressed={open}
          onClick={() => {
            setOpen(!open)
            setPlan(null)
            setError('')
            setIndexAck(false)
            setChangeAck(false)
            setConfigReadAck(false)
            setConfigChangeAck(false)
            setConfigPlan(null)
            if (open) {
              setPassword('')
              setPrivateKey('')
            }
          }}
        >
          {open ? 'Cancel LLDP capability review' : installed ? 'Review LLDP rollback' : 'Review LLDP installation'}
        </Button>}
      />
      {open && (
        <div style={{ display: 'grid', gap: 8, marginTop: 10 }}>
          <label style={{ display: 'grid', gap: 3, fontSize: 11 }}>
            Device administrator username
            <input value={username} autoComplete="off" onChange={(event) => setUsername(event.target.value)} />
          </label>
          <label style={{ display: 'grid', gap: 3, fontSize: 11 }}>
            Device administrator password
            <input
              type="password"
              value={password}
              autoComplete="off"
              onChange={(event) => setPassword(event.target.value)}
            />
          </label>
          <label style={{ display: 'grid', gap: 3, fontSize: 11 }}>
            SSH private key (optional)
            <textarea
              value={privateKey}
              autoComplete="off"
              spellCheck={false}
              rows={4}
              onChange={(event) => setPrivateKey(event.target.value)}
            />
          </label>
          <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
            Credentials remain only in this open review and may be reused for its plan/apply pair. They are never stored
            and are cleared when the review closes or after a router change.
          </div>
          {installed && (
            <>
              <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
                Interface configuration:{' '}
                {status?.configuration_state === 'configured'
                  ? `controller-managed (${status.configured_interfaces?.join(', ')})`
                  : status?.configuration_state === 'incomplete'
                    ? 'an earlier configuration action did not complete; rollback baseline retained'
                    : 'OpenWrt package default (physical neighbor discovery is not yet verified)'}
                .
              </div>
              {status?.configuration_state !== 'configured' && (
                <>
                  <label
                    style={{
                      display: 'flex',
                      gap: 8,
                      alignItems: 'start',
                      fontSize: 11,
                    }}
                  >
                    <input
                      type="checkbox"
                      checked={configReadAck}
                      disabled={busy}
                      onChange={(event) => setConfigReadAck(event.target.checked)}
                    />
                    <span style={{ minWidth: 0, lineHeight: 1.45 }}>
                      I authorize reading the current <code>lldpd</code> UCI export and wired bridge members to produce
                      an exact configuration plan. This read changes nothing.
                    </span>
                  </label>
                  <Button
                    disabled={busy || username.trim() === '' || !configReadAck}
                    onClick={() => void resolveConfigPlan()}
                  >
                    {busy ? 'Resolving interface plan…' : 'Show exact LLDP interface plan'}
                  </Button>
                </>
              )}
              <label
                style={{
                  display: 'flex',
                  gap: 8,
                  alignItems: 'start',
                  fontSize: 11,
                }}
              >
                <input
                  type="checkbox"
                  checked={diagnosticAck}
                  disabled={busy}
                  onChange={(event) => setDiagnosticAck(event.target.checked)}
                />
                <span style={{ minWidth: 0, lineHeight: 1.45 }}>
                  I authorize a read-only inspection of the router&apos;s <code>lldpd</code> configuration, runtime
                  interfaces, and reported neighbors. It changes no router setting, package, or service; the controller
                  records only that this diagnostic ran.
                </span>
              </label>
              <Button disabled={busy || username.trim() === '' || !diagnosticAck} onClick={() => void diagnose()}>
                {busy ? 'Inspecting LLDP runtime…' : 'Inspect LLDP runtime (read only)'}
              </Button>
              {configPlan?.plan && (
                <>
                  <div style={{ fontSize: 11, fontWeight: 600 }}>Exact LLDP interface configuration plan</div>
                  <pre
                    style={{
                      whiteSpace: 'pre-wrap',
                      maxHeight: 220,
                      overflow: 'auto',
                      padding: 8,
                      border: '1px solid var(--border)',
                      borderRadius: 4,
                    }}
                  >
                    {configPlan.plan}
                  </pre>
                  <label
                    style={{
                      display: 'flex',
                      gap: 8,
                      alignItems: 'start',
                      fontSize: 11,
                    }}
                  >
                    <input
                      type="checkbox"
                      checked={configChangeAck}
                      disabled={busy}
                      onChange={(event) => setConfigChangeAck(event.target.checked)}
                    />
                    <span style={{ minWidth: 0, lineHeight: 1.45 }}>
                      I reviewed this exact plan and authorize replacing only <code>lldpd.config.interface</code>,
                      committing only <code>/etc/config/lldpd</code>, and restarting only <code>lldpd</code>. The exact
                      current UCI export is retained for drift-checked rollback.
                    </span>
                  </label>
                  <Button disabled={busy || !configChangeAck} onClick={() => void applyConfigPlan()}>
                    {busy ? 'Applying LLDP interface plan…' : 'Apply LLDP interface configuration'}
                  </Button>
                </>
              )}
            </>
          )}
          {!planningRemoval && (
            <label
              style={{
                display: 'flex',
                gap: 8,
                alignItems: 'start',
                fontSize: 11,
              }}
            >
              <input
                type="checkbox"
                checked={indexAck}
                disabled={busy}
                onChange={(event) => setIndexAck(event.target.checked)}
              />
              <span style={{ minWidth: 0, lineHeight: 1.45 }}>
                I authorize refreshing the router&apos;s package index cache to resolve the exact <code>lldpd</code>{' '}
                installation plan. This installs no package or service.
              </span>
            </label>
          )}
          <Button
            disabled={busy || username.trim() === '' || (!planningRemoval && !indexAck)}
            onClick={() => void resolvePlan()}
          >
            {busy
              ? 'Resolving package plan…'
              : planningRemoval
                ? 'Show exact rollback plan'
                : 'Refresh index and show exact install plan'}
          </Button>
          {plan?.plan && (
            <>
              <div style={{ fontSize: 11, fontWeight: 600 }}>
                Exact {planningRemoval ? 'rollback' : 'installation'} plan from {plan.package_manager}
              </div>
              <pre
                style={{
                  whiteSpace: 'pre-wrap',
                  maxHeight: 220,
                  overflow: 'auto',
                  padding: 8,
                  border: '1px solid var(--border)',
                  borderRadius: 4,
                }}
              >
                {plan.plan}
              </pre>
              <label
                style={{
                  display: 'flex',
                  gap: 8,
                  alignItems: 'start',
                  fontSize: 11,
                }}
              >
                <input
                  type="checkbox"
                  checked={changeAck}
                  disabled={busy}
                  onChange={(event) => setChangeAck(event.target.checked)}
                />
                <span style={{ minWidth: 0, lineHeight: 1.45 }}>
                  I reviewed this exact plan and authorize{' '}
                  {planningRemoval
                    ? 'removing only the controller-owned LLDP capability and restoring the recorded service baseline.'
                    : 'installing these packages, refreshing the router package index once more immediately beforehand to revalidate this plan, and enabling and starting the lldpd service.'}
                </span>
              </label>
              <Button disabled={busy || !changeAck} onClick={() => void applyPlan()}>
                {busy ? 'Applying…' : planningRemoval ? 'Remove LLDP capability' : 'Install LLDP capability'}
              </Button>
            </>
          )}
        </div>
      )}
      {status?.detail && (
        <div role="alert" style={{ marginTop: 8 }}>
          <Banner tone="critical">{status.detail}</Banner>
        </div>
      )}
      {status?.diagnostics && (
        <details style={{ marginTop: 8 }}>
          <summary>LLDP runtime diagnostic</summary>
          <pre
            style={{
              whiteSpace: 'pre-wrap',
              maxHeight: 260,
              overflow: 'auto',
              padding: 8,
              border: '1px solid var(--border)',
              borderRadius: 4,
            }}
          >
            {status.diagnostics}
          </pre>
        </details>
      )}
      {error && (
        <div role="alert" style={{ marginTop: 8 }}>
          <Banner tone="critical">{error}</Banner>
        </div>
      )}
    </div>
  )
}

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
        Re-reads what this device can do. Runs automatically after a firmware change; do it by hand after installing a
        package or widening the ACL. It is a burst of reads, so polling pauses while it runs.
      </div>

      {err && (
        <div role="alert" style={{ marginTop: 8 }}>
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
                <span style={{ color: effectTone(c.effect) }}>{effectLabel(c.effect)}</span>
              </div>
              <div style={{ color: 'var(--text-secondary)' }}>{c.detail}</div>
            </div>
          ))}
          {res && res.actionable === 0 && (
            <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
              None of these change what this device can be sent — they are changes in what the controller can see.
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
        {target ? `— ${target} ` : ''}has not been measured, so this device is polled at the conservative default and no
        CPU cost is claimed for it
      </span>
    </span>
  )
}

/**
 * What it would take to bring a foreign SSID under management — and the reason
 * oonfeeWRT will not do it for you.
 *
 * The controller only manages sections it wrote, because that is what makes
 * un-adopt a promise rather than a hope: it can put back exactly what it added.
 * Automating a takeover means deleting a section it did not write, which is the
 * one thing that rule exists to forbid. Two automated designs were written and
 * reviewed before this one, and both would have confirmed their own
 * irreversible step with a health check that could not see it.
 *
 * So this prints the recipe and the cost, and the operator runs it on their own
 * device. The other half is the note: most foreign SSIDs should simply be left
 * alone, and someone who has decided that deserves to stop being asked.
 */
function TakeoverBriefBlock({ deviceID, b, onNoted }: { deviceID: number; b: Broadcast; onNoted: () => void }) {
  const [open, setOpen] = useState(false)
  const [note, setNote] = useState(b.brief?.note ?? '')
  const [saving, setSaving] = useState(false)
  const [noteErr, setNoteErr] = useState('')
  const brief = b.brief

  return (
    <div style={{ fontSize: 11, color: 'var(--text-secondary)', marginTop: 2 }}>
      <div>
        oonfeeWRT did not create this network — it is section <code>{b.section || 'unknown'}</code> on the device, from
        before adoption or made by hand. The controller leaves config it did not write alone.
      </div>
      {brief?.note && (
        <div style={{ color: 'var(--text-muted)', marginTop: 2 }}>
          Left alone deliberately: {brief.note}
          {brief.decided_by ? ` — ${brief.decided_by}` : ''}
        </div>
      )}
      <button
        onClick={() => setOpen((v) => !v)}
        style={{
          background: 'none',
          border: 'none',
          padding: 0,
          marginTop: 2,
          color: 'var(--accent-text)',
          cursor: 'pointer',
          font: 'inherit',
        }}
      >
        {open ? 'Hide' : 'What would it take to manage this?'}
      </button>

      {open && brief && (
        <div
          style={{
            borderLeft: '2px solid var(--border-strong)',
            paddingLeft: 8,
            marginTop: 6,
            display: 'grid',
            gap: 6,
          }}
        >
          {!brief.safe_to_disable ? (
            <div style={{ color: 'var(--warning)' }}>{brief.refusal}</div>
          ) : (
            <>
              <div>
                Run this on the device. oonfeeWRT will not do it for you: it would mean deleting config it did not
                write, and then it could not put it back.
              </div>
              <pre
                style={{
                  margin: 0,
                  padding: 8,
                  overflowX: 'auto',
                  background: 'var(--bg-inset, rgba(127,127,127,0.12))',
                  color: 'var(--text-primary)',
                }}
              >
                {brief.recipe?.join('\n')}
              </pre>
              <div>Then recreate it as a wireless network here. What it costs:</div>
              <ul style={{ margin: 0, paddingLeft: 18 }}>
                {brief.cost?.map((c: string) => (
                  <li key={c}>{c}</li>
                ))}
              </ul>
            </>
          )}

          <div>
            Or leave it alone and say why — it stops being an open question and the reason survives for whoever looks
            next.
          </div>
          <div style={{ display: 'flex', gap: 6 }}>
            <input
              aria-label={`Reason for leaving ${b.ssid || b.section || 'this network'} unmanaged`}
              value={note}
              onChange={(e) => setNote(e.target.value)}
              placeholder="e.g. guest network a flatmate maintains by hand"
              style={{ flex: 1, font: 'inherit', padding: 4 }}
            />
            <Button
              disabled={saving}
              onClick={async () => {
                setSaving(true)
                setNoteErr('')
                try {
                  await api.noteForeign(deviceID, b.section ?? '', b.ssid, note)
                  onNoted()
                } catch (e) {
                  // Reported, not swallowed. request() throws on any non-2xx,
                  // and a try/finally with no catch left a rejected write
                  // looking exactly like a successful one — the operator
                  // believes a decision is recorded that is not.
                  setNoteErr(e instanceof Error ? e.message : String(e))
                } finally {
                  setSaving(false)
                }
              }}
            >
              {note.trim() ? 'Record' : 'Clear'}
            </Button>
          </div>
          {noteErr && (
            <div role="alert" style={{ color: 'var(--critical, #d05a5a)' }}>
              The note was not saved: {noteErr}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * The device name, editable in place.
 *
 * The default is the device's own board model — "TP-Link Archer C6 v2" rather
 * than "ap-192-168-1-2" — which is what someone recognises when looking at a
 * shelf of routers. That default only breaks down when a site has two of the
 * same model, so the name has to be editable, and until now it was not: there
 * was no rename anywhere, in the store, the API or here.
 *
 * Clearing the field restores the model rather than being refused. That is the
 * useful reading of an empty box, and it is adoption's own fallback chain, so
 * "undo my rename" needs no separate control.
 */
function DeviceName({ detail, onRenamed }: { detail: DeviceDetail; onRenamed: () => void }) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(detail.name)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  async function save() {
    setBusy(true)
    setErr('')
    try {
      await api.renameDevice(detail.id, draft)
      setEditing(false)
      onRenamed()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  if (!editing) {
    return (
      <span style={{ display: 'inline-flex', gap: 8, alignItems: 'center' }}>
        {detail.name || detail.mac}
        <Button
          aria-label={`Rename ${detail.name || detail.mac}`}
          onClick={() => {
            setDraft(detail.name)
            setErr('')
            setEditing(true)
          }}
        >
          Rename
        </Button>
      </span>
    )
  }
  return (
    <span style={{ display: 'grid', gap: 4 }}>
      <span style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
        <input
          aria-label={`New name for ${detail.name || detail.mac}`}
          value={draft}
          autoFocus
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') save()
            if (e.key === 'Escape') setEditing(false)
          }}
          style={{
            background: 'var(--surface-2)',
            color: 'var(--text-primary)',
            border: '1px solid var(--border)',
            borderRadius: 4,
            padding: '4px 6px',
            fontSize: 12,
            minWidth: 220,
          }}
        />
        <Button kind="primary" disabled={busy} onClick={save}>
          {busy ? 'Saving…' : 'Save'}
        </Button>
        <Button disabled={busy} onClick={() => setEditing(false)}>
          Cancel
        </Button>
      </span>
      <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>
        Leave it empty to go back to the name the device reports for itself.
      </span>
      {err && (
        <span role="alert" style={{ fontSize: 11, color: 'var(--critical)' }}>
          {err}
        </span>
      )}
    </span>
  )
}
