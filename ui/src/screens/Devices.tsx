import { useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api'
import type { Device, DeviceDetail, Point, Series } from '../lib/api'
import {
  Card, DataGrid, SlideOver, Status, Prop, Unknown, Banner, Button,
} from '../components/ui'
import type { Column } from '../components/ui'
import { TimeChart, fmt, ago, duration } from '../components/Chart'

export function Devices({ devices }: { devices: Device[] }) {
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
      <Card title={`Devices (${devices.length})`} pad={false}>
        <DataGrid
          rows={devices}
          columns={columns}
          rowKey={(d) => d.mac}
          onRowClick={(d) => setOpenID(d.id)}
          empty="No devices yet. Adoption has no UI in this phase — seed one with the integration test helper."
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
  const [err, setErr] = useState('')

  useEffect(() => {
    let live = true
    const load = async () => {
      try {
        const [d, s] = await Promise.all([api.device(id), api.deviceSeries(id)])
        if (!live) return
        setDetail(d)
        setSeries(s.series)
      } catch (e) {
        if (live) setErr(e instanceof Error ? e.message : String(e))
      }
    }
    load()

    // The lease. 30-second grant, renewed every 20 so it never lapses while the
    // panel is open, and gone within 30 once it is not.
    const focus = () => api.focus(id, 30).catch(() => {})
    focus()
    const renew = setInterval(focus, 20_000)
    const refresh = setInterval(load, 10_000)
    return () => {
      live = false
      clearInterval(renew)
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
          {detail.quiesced ? 'paused for an apply' : (detail.tier ?? detail.poll_state)}
        </Prop>
        <Prop label="Last seen">
          {detail.last_seen ? ago(detail.last_seen) : <Unknown why="never polled" />}
        </Prop>
      </div>

      {detail.tier !== 'focused' && (
        <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
          Opening this panel raises the poll rate for this device. Per-station and
          survey data appear within a few seconds.
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
