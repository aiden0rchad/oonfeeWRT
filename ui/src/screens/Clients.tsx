import { useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api'
import type { Client, ClientPage } from '../lib/api'
import {
  Card,
  DataGrid,
  FilterRail,
  Pager,
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
 *
 * Fetches its own page, like the log. It used to be handed the whole inventory
 * and filter it here, which is correct only while one response holds the whole
 * table: past that, filtering the fetched window selects from the newest N
 * clients overall rather than the newest N matching, and the rail counts the
 * page instead of the table.
 */
export function Clients() {
  const [presence, setPresence] = useState('online')
  const [connection, setConnection] = useState('')
  // Defaults to the network this controller manages.
  //
  // A gateway's neighbour tables cover every interface, so an unscoped list
  // mixes the operator's devices with whatever is on the other side of the WAN
  // port — measured 11 of 14 on the reference device, including the upstream
  // router itself. Those are not this network's clients by any definition a
  // user has. They stay reachable through the rail rather than being dropped,
  // because "where did my device go" needs an answer that is not silence.
  const [scope, setScope] = useState('local')
  const [limit, setLimit] = useState(500)
  const [offset, setOffset] = useState(0)
  const [page, setPage] = useState<ClientPage | null>(null)
  const [err, setErr] = useState('')
  const [hidden, setHidden] = useHiddenColumns('clients')

  const load = useCallback(async () => {
    try {
      setPage(await api.clients({ limit, offset, presence, connection, scope }))
      setErr('')
    } catch (e) {
      // Keep the last good page. Blanking it on one dropped request would read
      // as "no clients", which is a different claim.
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [limit, offset, presence, connection, scope])

  useEffect(() => {
    load()
    const t = setInterval(load, 30_000)
    return () => clearInterval(t)
  }, [load])

  // Changing a filter resets the offset: page 4 of the unfiltered list is not
  // page 4 of the filtered one, and keeping it lands on an empty page that
  // reads as "no matches".
  const setFilter = (set: (v: string) => void) => (v: string) => {
    set(v)
    setOffset(0)
  }

  const rows = page?.clients ?? []
  const withRF = rows.filter((c) => c.signal != null).length

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
      {withRF === 0 && rows.length > 0 && page && (
        <Banner tone="accent">{page.note}. Open a device to populate them.</Banner>
      )}
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: '190px 1fr',
          gap: 14,
          alignItems: 'start',
        }}
      >
        {/* counted="all" now: the counts come from an aggregate over the whole
            filtered table rather than from the rows on screen. */}
        <FilterRail
          counted="all"
          groups={[
            {
              label: 'Network',
              options: page?.facets.scope ?? [],
              selected: scope,
              onChange: setFilter(setScope),
            },
            {
              label: 'Presence',
              options: page?.facets.presence ?? [],
              selected: presence,
              onChange: setFilter(setPresence),
            },
            {
              label: 'Connection',
              options: page?.facets.connection ?? [],
              selected: connection,
              onChange: setFilter(setConnection),
            },
          ]}
        />
        <Card title={`Client devices (${(page?.total ?? 0).toLocaleString()})`} pad={false}>
          {err && (
            <div style={{ padding: 12 }}>
              <Banner tone="critical">{err}</Banner>
            </div>
          )}
          <DataGrid
            rows={rows}
            columns={columns}
            hidden={hidden}
            onHiddenChange={setHidden}
            rowKey={(c) => c.mac}
            empty="No clients match these filters. The inventory is built from the baseline poll, so it fills in within a minute of a device coming online."
          />
          {page && (
            <Pager
              total={page.total}
              limit={limit}
              offset={offset}
              onChange={(l, o) => {
                setLimit(l)
                setOffset(o)
              }}
            />
          )}
        </Card>
      </div>
    </div>
  )
}

/** RSSI colouring. Additive only — the number is always shown (UI-SPEC §5). */
function signalTone(dbm: number): string {
  if (dbm >= -60) return 'var(--good)'
  if (dbm >= -70) return 'var(--warning)'
  return 'var(--serious)'
}
