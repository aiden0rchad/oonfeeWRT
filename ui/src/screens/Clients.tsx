import { useState } from 'react'
import type { Client } from '../lib/api'
import { Card, DataGrid, Status, Unknown, Banner } from '../components/ui'
import type { Column } from '../components/ui'
import { ago } from '../components/Chart'

/**
 * The Client Devices grid.
 *
 * The honest part of this screen is the columns that are usually empty. Signal
 * and retry come from `iwinfo.assoclist`, which is ~92% of a focused poll and
 * therefore only runs while a device screen is open. So most rows, most of the
 * time, have no RF data — and they show a dash with an explanation rather than
 * a zero. A grid full of "0 dBm" would be worse than one that admits it does
 * not know.
 */
export function Clients({ clients, note }: { clients: Client[]; note: string }) {
  const [onlyOnline, setOnlyOnline] = useState(true)
  const rows = onlyOnline ? clients.filter((c) => c.online) : clients
  const withRF = clients.filter((c) => c.signal != null).length

  const columns: Column<Client>[] = [
    {
      key: 'online',
      header: 'Status',
      width: 100,
      render: (c) => <Status value={c.online ? 'online' : 'offline'} />,
      sortBy: (c) => (c.online ? 0 : 1),
    },
    {
      key: 'name',
      header: 'Name',
      render: (c) => c.name || <span style={{ color: 'var(--text-muted)' }}>{c.mac}</span>,
      sortBy: (c) => c.name || c.mac,
    },
    { key: 'mac', header: 'MAC', render: (c) => c.mac, sortBy: (c) => c.mac },
    {
      key: 'ip',
      header: 'IPv4',
      render: (c) => c.ipv4 || <Unknown why="no address seen for this client" />,
      sortBy: (c) => c.ipv4 ?? '',
    },
    {
      key: 'conn',
      header: 'Connection',
      render: (c) =>
        c.connection === 'wireless' ? (
          <Status value="wireless" />
        ) : (
          <Unknown why="no focused poll has seen this client associated. Absence of wireless evidence is not evidence of a cable." />
        ),
      sortBy: (c) => c.connection,
    },
    {
      key: 'signal',
      header: 'Signal',
      numeric: true,
      render: (c) =>
        c.signal == null ? (
          <Unknown why="signal comes from the focused poll tier, which runs only while a device screen is open" />
        ) : (
          <span style={{ color: signalTone(c.signal) }}>{c.signal} dBm</span>
        ),
      sortBy: (c) => c.signal ?? -999,
    },
    {
      key: 'retry',
      header: 'TX retries',
      numeric: true,
      render: (c) =>
        c.tx_retry_pct == null ? (
          <Unknown why="retry data comes from the focused poll tier" />
        ) : (
          `${c.tx_retry_pct.toFixed(1)}%`
        ),
      sortBy: (c) => c.tx_retry_pct ?? -1,
    },
    {
      key: 'seen',
      header: 'Last seen',
      numeric: true,
      render: (c) => ago(c.last_seen),
      sortBy: (c) => c.last_seen ?? 0,
    },
  ]

  return (
    <div style={{ display: 'grid', gap: 14 }}>
      {withRF === 0 && clients.length > 0 && (
        <Banner tone="accent">{note}. Open a device to populate them.</Banner>
      )}
      <Card
        title={`Client devices (${rows.length}${onlyOnline && clients.length !== rows.length ? ` of ${clients.length}` : ''})`}
        actions={
          <label style={{ fontSize: 11, color: 'var(--text-secondary)', display: 'flex', gap: 6, alignItems: 'center' }}>
            <input
              type="checkbox"
              checked={onlyOnline}
              onChange={(e) => setOnlyOnline(e.target.checked)}
            />
            Active only
          </label>
        }
        pad={false}
      >
        <DataGrid
          rows={rows}
          columns={columns}
          rowKey={(c) => c.mac}
          empty="No clients seen yet. The inventory is built from the baseline poll, so it fills in within a minute of a device coming online."
        />
      </Card>
    </div>
  )
}

/** RSSI colouring. Additive only — the number is always shown (UI-SPEC §5). */
function signalTone(dbm: number): string {
  if (dbm >= -60) return 'var(--good)'
  if (dbm >= -70) return 'var(--warning)'
  return 'var(--serious)'
}
