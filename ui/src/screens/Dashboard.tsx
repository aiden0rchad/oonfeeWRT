import type { Dashboard as DashboardData } from '../lib/api'
import { Card, Stat, Banner, Status, Unknown } from '../components/ui'
import { ago } from '../components/Chart'

/**
 * The fleet summary.
 *
 * "Wireless clients" is the same server-side online/local/wireless filter as
 * Client Devices. It is intentionally not a sum of per-radio counters: those
 * counters have no client address and therefore cannot apply network scope.
 */
export function Dashboard({ data }: { data: DashboardData }) {
  const d = data.devices
  const events = data.recent_events ?? []
  const wirelessUnknownOn = data.wireless_clients_unknown_on ?? []
  const missingWAN = (data.gateway_uplinks ?? []).filter((gateway) => gateway.state === 'missing')

  // What "Devices on the LAN" leaves out, named under the number itself.
  //
  // The count is scoped to this network, so on a gateway it excludes the
  // neighbours on the uplink — 11 of 14 on the reference device. Without this
  // line the headline is simply smaller than the previous build's and the
  // operator has no way to tell a correct rescoping from lost devices.
  const elsewhere: string[] = []
  if (data.upstream_devices > 0) elsewhere.push(`${data.upstream_devices} upstream`)
  if (data.unscoped_devices > 0) elsewhere.push(`${data.unscoped_devices} unplaced`)

  return (
    <div style={{ display: 'grid', gap: 14 }}>
      <h1 style={{ margin: 0, fontSize: 20 }}>Dashboard</h1>
      {missingWAN.length > 0 && (
        <div role="alert">
          <Banner tone="critical">
            No active WAN/default route was observed on{' '}
            <strong>{missingWAN.map((gateway) => gateway.name).join(', ')}</strong>.
            {' '}The gateway is reachable, but clients may not have Internet access.
            Check its WAN cable, upstream modem, and OpenWrt interface status.
          </Banner>
        </div>
      )}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 14 }}>
        <Card>
          <Stat label="Devices online" value={`${d.online}/${d.total}`}
            tone={d.online === d.total && d.total > 0 ? 'good' : d.offline > 0 ? 'critical' : undefined} />
        </Card>
        <Card>
          <Stat
            label="Wireless clients"
            value={
              data.wireless_clients_complete ? (
                data.wireless_clients
              ) : (
                <Unknown why="one or more devices did not report their current station set" />
              )
            }
            tone={data.wireless_clients_complete ? undefined : 'muted'}
            sub={
              data.wireless_clients_complete
                ? undefined
                : `${data.wireless_clients} matching row${data.wireless_clients === 1 ? '' : 's'} identified; full total unavailable`
            }
          />
        </Card>
        <Card>
          <Stat
            label="Devices on the LAN"
            value={data.active_devices}
            sub={elsewhere.length > 0 ? `${elsewhere.join(', ')} not counted` : undefined}
          />
        </Card>
        <Card>
          {/* Labelled for what it counts. It said "Focused polls" over
              focused_devices — a count of DEVICES under a label promising a
              count of polls, on a dashboard whose own code comment two files
              away says that showing one number under another's label is how a
              dashboard gets quietly distrusted.

              It also reads 0 almost always, and that is correct rather than
              broken: focus is held by an open device panel, and anyone reading
              this screen does not have one open. Said in the note below, so a
              permanent zero is not mistaken for a stuck counter. */}
          <Stat
            label="Devices in focus"
            value={data.focused_devices}
            sub={data.focused_devices === 0 ? 'no panel is open' : undefined}
          />
        </Card>
        <Card>
          <Stat label="Series collected" value={data.series_count} />
        </Card>
      </div>

      <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
        “Wireless clients” is the same count as Client Devices with this network,
        online presence and wireless connection selected. It uses current
        hostapd associations plus recent station telemetry, so private MACs and
        clients on another managed VLAN count when their client row is local;
        uplink-side and unplaced rows do not. If any device cannot report its
        station set, the matching-row count remains available but is not shown
        as a complete fleet total. “Devices on the LAN” counts every online row on{' '}
        <em>this</em> network, wired included. “Devices in focus” counts the ones
        being polled every few seconds instead of every minute, which happens
        only while somebody has a device panel open — so from this screen it is
        normally zero, and that is the honest answer rather than a stuck counter.
      </div>

      {!data.wireless_clients_complete && (
        <Banner>
          The wireless client total is unavailable because{' '}
          <strong>
            {wirelessUnknownOn.length > 0
              ? wirelessUnknownOn.join(', ')
              : 'one or more managed devices'}
          </strong>{' '}
          did not report a current station set. Client Devices still identifies{' '}
          {data.wireless_clients} matching row
          {data.wireless_clients === 1 ? '' : 's'}, but presenting that partial
          evidence as the fleet total would show a false zero or dip.
        </Banner>
      )}

      {d.pending > 0 && (
        <Banner tone="accent">
          {d.pending} device{d.pending > 1 ? 's are' : ' is'} in the inventory but
          not adopted. They are not polled: there is no credential for them yet.
        </Banner>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 14 }}>
        <Card title="Device status">
          <div style={{ display: 'grid', gap: 8 }}>
            {(
              [
                ['online', d.online],
                ['offline', d.offline],
                ['pending', d.pending],
                ['unknown', d.unknown],
              ] as const
            ).map(([k, n]) => (
              <div key={k} style={{ display: 'flex', justifyContent: 'space-between' }}>
                <Status value={k} />
                <span className="num">{n}</span>
              </div>
            ))}
            <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
              “unknown” means adopted but never successfully polled — different
              from offline, which means it answered once and has stopped.
            </div>
          </div>
        </Card>

        <Card title="Recent events">
          {events.length === 0 ? (
            <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
              Nothing logged yet.
            </div>
          ) : (
            <div style={{ display: 'grid', gap: 6 }}>
              {events.slice(0, 8).map((e, i) => (
                <div key={i} style={{ display: 'flex', gap: 10, fontSize: 12 }}>
                  <span
                    style={{
                      color:
                        e.Severity === 'error'
                          ? 'var(--critical)'
                          : e.Severity === 'warning'
                            ? 'var(--warning)'
                            : 'var(--text-secondary)',
                      minWidth: 58,
                    }}
                  >
                    {e.Severity}
                  </span>
                  <span style={{ flex: 1 }}>{e.Event}</span>
                  <span style={{ color: 'var(--text-muted)' }}>{ago(e.TS)}</span>
                </div>
              ))}
            </div>
          )}
        </Card>
      </div>
    </div>
  )
}
