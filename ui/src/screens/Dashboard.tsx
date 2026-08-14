import type { Dashboard as DashboardData } from '../lib/api'
import { Card, Stat, Banner, Status, Unknown } from '../components/ui'
import { ago } from '../components/Chart'

/**
 * The fleet summary.
 *
 * The client total is the interesting part: it is `null` whenever any AP's
 * count could not be read, and the UI says so rather than showing a number
 * that is quietly short. Summing the radios that answered would draw a dip
 * meaning "a radio did not reply", which reads exactly like clients leaving.
 */
export function Dashboard({ data }: { data: DashboardData }) {
  const d = data.devices
  const events = data.recent_events ?? []

  return (
    <div style={{ display: 'grid', gap: 14 }}>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 14 }}>
        <Card>
          <Stat label="Devices online" value={`${d.online}/${d.total}`}
            tone={d.online === d.total && d.total > 0 ? 'good' : d.offline > 0 ? 'critical' : undefined} />
        </Card>
        <Card>
          <Stat
            label="Wireless clients"
            value={
              data.wireless_clients === null ? (
                <Unknown why="at least one access point's client count could not be read" />
              ) : (
                data.wireless_clients
              )
            }
            tone={data.wireless_clients === null ? 'muted' : undefined}
          />
        </Card>
        <Card>
          <Stat label="Devices on the LAN" value={data.active_devices} />
        </Card>
        <Card>
          <Stat label="Focused polls" value={data.focused_devices} />
        </Card>
        <Card>
          <Stat label="Series collected" value={data.series_count} />
        </Card>
      </div>

      {data.wireless_clients === null && data.wireless_clients_unknown_on && (
        <Banner>
          The wireless client total is unavailable because{' '}
          <strong>{data.wireless_clients_unknown_on.join(', ')}</strong> did not
          report a client count. Adding up the rest would show a dip that looks
          like clients leaving, so no total is shown at all.
        </Banner>
      )}

      <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
        “Wireless clients” counts stations associated to the radios, from
        hostapd. “Devices on the LAN” counts everything that answered ARP or
        DHCP, wired included. They are different questions and will not match.
      </div>

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
