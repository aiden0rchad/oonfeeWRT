import { useState } from 'react'
import type { Client } from '../lib/api'
import {
  Card,
  DataGrid,
  FilterRail,
  Status,
  Unknown,
  Banner,
  useHiddenColumns,
} from '../components/ui'
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
  const [presence, setPresence] = useState('online')
  const [connection, setConnection] = useState('')
  // Defaults to the network this controller manages.
  //
  // A gateway's neighbour tables cover every interface, so an unscoped list
  // mixes the operator's devices with whatever is on the other side of the WAN
  // port — measured 8 of 16 on the reference device, including the upstream
  // router itself. Those are not this network's clients by any definition a
  // user has. They stay reachable through the rail rather than being dropped,
  // because "where did my device go" needs an answer that is not silence.
  const [scope, setScope] = useState('local')
  const [hidden, setHidden] = useHiddenColumns('clients')
  const withRF = clients.filter((c) => c.signal != null).length

  // Faceted the same way the server does it for the log: each rail counts with
  // the OTHER filter applied but not its own, so every option answers "how many
  // would I get if I clicked that instead?" rather than showing 0 beside
  // everything not currently selected.
  const presenceOf = (c: Client) => (c.online ? 'online' : 'offline')
  const match = (c: Client, skip: 'presence' | 'connection' | 'scope' | null) =>
    (skip === 'presence' || presence === '' || presenceOf(c) === presence) &&
    (skip === 'connection' || connection === '' || c.connection === connection) &&
    (skip === 'scope' || scope === '' || c.scope === scope)

  const rows = clients.filter((c) => match(c, null))

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
      required: true,
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
      key: 'scope',
      header: 'Network',
      width: 110,
      render: (c) =>
        c.scope === 'local' ? (
          'this network'
        ) : c.scope === 'upstream' ? (
          <span
            style={{ color: 'var(--text-muted)' }}
            title="On the subnet of the interface carrying the default route — a neighbour on the uplink, not a client of this network."
          >
            upstream
          </span>
        ) : (
          <Unknown why="no address has been seen for this host, or its address falls in none of the device's own subnets, so which side of the router it is on has not been established" />
        ),
      sortBy: (c) => c.scope,
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
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: '190px 1fr',
          gap: 14,
          alignItems: 'start',
        }}
      >
        {/* counted="loaded" and not "all": /clients returns the entire
            inventory in one response, so counting it here IS counting
            everything. The rail says which it is rather than leaving the
            reader to assume — the assumption is wrong on the log screen. */}
        <FilterRail
          counted="loaded"
          groups={[
            {
              label: 'Network',
              options: tally(clients.filter((c) => match(c, 'scope')), (c) => c.scope),
              selected: scope,
              onChange: setScope,
            },
            {
              label: 'Presence',
              options: tally(clients.filter((c) => match(c, 'presence')), presenceOf),
              selected: presence,
              onChange: setPresence,
            },
            {
              label: 'Connection',
              options: tally(clients.filter((c) => match(c, 'connection')), (c) => c.connection),
              selected: connection,
              onChange: setConnection,
            },
          ]}
        />
        <Card
          title={`Client devices (${rows.length.toLocaleString()}${
            rows.length !== clients.length ? ` of ${clients.length.toLocaleString()}` : ''
          })`}
          pad={false}
        >
          <DataGrid
            rows={rows}
            columns={columns}
            hidden={hidden}
            onHiddenChange={setHidden}
            rowKey={(c) => c.mac}
            empty="No clients match these filters. The inventory is built from the baseline poll, so it fills in within a minute of a device coming online."
          />
        </Card>
      </div>
    </div>
  )
}

/** Count each distinct value of `of` across rows, commonest first. */
function tally(rows: Client[], of: (c: Client) => string) {
  const m = new Map<string, number>()
  for (const c of rows) m.set(of(c), (m.get(of(c)) ?? 0) + 1)
  return [...m.entries()]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([value, count]) => ({ value, count }))
}

/** RSSI colouring. Additive only — the number is always shown (UI-SPEC §5). */
function signalTone(dbm: number): string {
  if (dbm >= -60) return 'var(--good)'
  if (dbm >= -70) return 'var(--warning)'
  return 'var(--serious)'
}
