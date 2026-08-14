import { useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api'
import type { Device, DeviceDetail, Overhead, Point, Series } from '../lib/api'
import {
  Card, DataGrid, SlideOver, Status, Prop, Unknown, Banner, Button,
} from '../components/ui'
import type { Column } from '../components/ui'
import { TimeChart, fmt, ago, duration } from '../components/Chart'
import { live } from '../lib/live'
import type { LiveStats } from '../lib/live'

export function Devices({
  devices,
  onAdopt,
}: {
  devices: Device[]
  onAdopt?: () => void
}) {
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
          empty="No devices yet. Adopt one to get started."
        />
      </Card>
      {openID !== null && (
        <DeviceDetailPanel id={openID} onClose={() => setOpenID(null)} />
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
function DeviceDetailPanel({ id, onClose }: { id: number; onClose: () => void }) {
  const [detail, setDetail] = useState<DeviceDetail | null>(null)
  const [series, setSeries] = useState<Record<string, string[]>>({})
  const [overhead, setOverhead] = useState<Overhead | null>(null)
  const [stats, setStats] = useState<LiveStats | null>(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    let alive = true
    const load = async () => {
      try {
        const [d, s] = await Promise.all([api.device(id), api.deviceSeries(id)])
        if (!alive) return
        setDetail(d)
        setSeries(s.series)
        // Not fatal: a device in the inventory but not yet polled has no
        // overhead to report, which is a real state rather than zero cost.
        api.overhead(id).then((o) => alive && setOverhead(o)).catch(() => {})
      } catch (e) {
        if (alive) setErr(e instanceof Error ? e.message : String(e))
      }
    }
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
  }, [id])

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
          {detail.class ?? <Unknown why="not classified" />}
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
          <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 6 }}>Radios</div>
          <div style={{ display: 'grid', gap: 6 }}>
            {stats.aps.map((ap) => (
              <div key={ap.iface} style={{ fontSize: 12 }}>
                <div style={{ display: 'flex', justifyContent: 'space-between' }}>
                  <span>
                    {ap.ssid || ap.iface}{' '}
                    <span style={{ color: 'var(--text-muted)' }}>ch {ap.channel}</span>
                  </span>
                  <span className="num">
                    {ap.clients === null ? (
                      <Unknown why="this radio did not report a client count" />
                    ) : (
                      `${ap.clients} client${ap.clients === 1 ? '' : 's'}`
                    )}
                  </span>
                </div>
                {ap.airtime_pct !== undefined && (
                  <div style={{ color: 'var(--text-secondary)', fontSize: 11 }}>
                    airtime {ap.airtime_pct.toFixed(1)}%
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

      {overhead && <ManagementOverhead o={overhead} />}

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
function ManagementOverhead({ o }: { o: Overhead }) {
  const budget = o.tier === 'focused' ? 6 : 1
  const overBudget = o.polls_per_minute > budget * 1.05
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
        <Prop label="Data sent">{formatBytes(o.bytes_out)}</Prop>
        <Prop label="Polls">
          {o.polls}
          {o.failed_polls > 0 && (
            <span style={{ color: 'var(--warning)' }}> ({o.failed_polls} failed)</span>
          )}
        </Prop>
      </div>
      <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 6 }}>
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
