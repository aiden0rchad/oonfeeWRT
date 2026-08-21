import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { Client, ClientPage, Device } from '../lib/api'
import {
  Card,
  DataGrid,
  FilterRail,
  Pager,
  Status,
  Unknown,
  Banner,
  useColumnPrefs,
} from '../components/ui'
import type { Column } from '../components/ui'
import { ago } from '../components/Chart'
import { ClientObservability } from './ClientObservability'

/**
 * The Client Devices grid.
 *
 * The honest part of this screen is the columns that may be empty. Association,
 * access point and signal come from hostapd on every baseline poll; TX retries
 * alone require the focused iwinfo tier. A row no managed AP reports stays
 * unknown rather than being called wired or given a made-up zero.
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
  const [loaded, setLoaded] = useState<{ query: string; page: ClientPage } | null>(null)
  const [failure, setFailure] = useState<{ query: string; message: string } | null>(null)
  const requestGeneration = useRef(0)
  const inFlight = useRef<{ query: string; generation: number } | null>(null)
  const [colPrefs, setColPrefs] = useColumnPrefs('clients')
  // Only to turn the AP id on a client row into a name. Fetched once rather
  // than per refresh: the client list reloads every 30s and the fleet roster
  // does not change on that timescale.
  const [devices, setDevices] = useState<Device[]>([])
  const [selectedClient, setSelectedClient] = useState<Client | null>(null)

  // Bind every response to the exact page and filters that started it. A slow
  // periodic refresh must not replace a newer filtered page when it completes.
  const query = JSON.stringify([limit, offset, presence, connection, scope])
  const load = useCallback(async () => {
    if (inFlight.current?.query === query) return
    const generation = ++requestGeneration.current
    inFlight.current = { query, generation }
    try {
      const next = await api.clients({ limit, offset, presence, connection, scope })
      if (generation !== requestGeneration.current) return
      setLoaded({ query, page: next })
      setSelectedClient((selected) => {
        if (selected == null) return null
        const current = (next.clients ?? []).find((candidate) =>
          candidate.mac.toLowerCase() === selected.mac.toLowerCase())
        if (current) return current
        return {
          ...selected,
          connection: 'unknown',
          device_id: undefined,
          signal: undefined,
          tx_retry_pct: undefined,
          association_ambiguous: undefined,
        }
      })
      setFailure(null)
    } catch (e) {
      if (generation !== requestGeneration.current) return
      // A same-query refresh keeps its last good page. A page fetched with
      // different filters is hidden below because it does not answer this query.
      setFailure({ query, message: e instanceof Error ? e.message : String(e) })
    } finally {
      if (inFlight.current?.generation === generation) inFlight.current = null
    }
  }, [limit, offset, presence, connection, scope, query])

  useEffect(() => {
    void load()
    const t = setInterval(() => void load(), 30_000)
    return () => {
      clearInterval(t)
      requestGeneration.current++
      inFlight.current = null
    }
  }, [load])

  useEffect(() => {
    // A failure here costs the AP column its names, not its answers — the row
    // falls back to the device id — so it is deliberately not surfaced as an
    // error over the client list.
    api
      .devices()
      .then((r) => setDevices(r.devices))
      .catch(() => {})
  }, [])

  // Changing a filter resets the offset: page 4 of the unfiltered list is not
  // page 4 of the filtered one, and keeping it lands on an empty page that
  // reads as "no matches".
  const setFilter = (set: (v: string) => void) => (v: string) => {
    set(v)
    setOffset(0)
  }

  const page = loaded?.query === query ? loaded.page : null
  const err = failure?.query === query ? failure.message : ''
  const loading = page === null && err === ''
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
      render: (c) => (
        <button
          type="button"
          aria-label={`Open observability for ${c.name || c.mac}`}
          onClick={(event) => {
            event.stopPropagation()
            setSelectedClient(c)
          }}
          style={{
            padding: 0, border: 0, background: 'none', color: c.name ? 'inherit' : 'var(--text-muted)',
            cursor: 'pointer', font: 'inherit', textAlign: 'left',
          }}
        >
          {c.name || c.mac}
        </button>
      ),
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
          <Unknown why="no access point this controller manages currently reports this client. Association is read from hostapd on every baseline poll; absence of wireless evidence is still not evidence of a cable." />
        ),
      sortBy: (c) => c.connection,
    },
    {
      key: 'signal',
      header: 'Signal',
      numeric: true,
      render: (c) =>
        c.signal == null ? (
          <Unknown why={c.association_ambiguous
            ? 'multiple managed AP or BSS observations currently report this MAC, so no single RSSI is attributed'
            : "no access point this controller manages is reporting this client, so nothing has measured its signal. Associated clients are read from hostapd on every poll — a client on another network's access point will never have a reading here."} />
        ) : (
          <span style={{ color: signalTone(c.signal) }}>{c.signal} dBm</span>
        ),
      sortBy: (c) => c.signal ?? -999,
    },
    {
      key: 'ap',
      header: 'Access point',
      width: 150,
      render: (c) => {
        if (c.device_id == null) {
          return (
            <Unknown why={c.association_ambiguous
              ? 'multiple managed access points currently report this MAC; no single access point is selected'
              : 'no access point this controller manages is reporting this client. It reached this list through ARP or DHCP, which sees every host on the network — including ones served by equipment oonfeeWRT does not run.'} />
          )
        }
        const ap = devices.find((d) => d.id === c.device_id)
        // The id is a real answer even when the name is not to hand: the device
        // may have been unadopted since the reading, and printing nothing would
        // claim we do not know which AP it was.
        return ap ? ap.name : `device ${c.device_id}`
      },
      sortBy: (c) =>
        c.device_id == null ? '' : (devices.find((d) => d.id === c.device_id)?.name ?? ''),
    },
    {
      key: 'retry',
      header: 'TX retries',
      numeric: true,
      render: (c) =>
        c.tx_retry_pct == null ? (
          <Unknown why="the retry rate is the one radio figure hostapd does not report, so it comes from the focused poll tier. Open this client's access point from the Devices screen to start one; the figure appears after the next five-minute flush." />
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
          <Unknown why="placement is unknown because no address was seen, the address matched none of the successfully read subnets, or a device's network.interface.dump could not be read. Open Devices and check ‘What the controller cannot read here’; re-probe transient failures and re-adopt only for a permanent permission gap." />
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
      <h1 style={{ margin: 0, fontSize: 20 }}>Client Devices</h1>
      {/* The server decides what this says: the remedy differs by cause, and
          "Open a device to populate them" used to be appended to all of them.
          On a fleet whose radios have no associated stations at all, opening a
          device runs a focused poll against an empty assoclist and changes
          nothing. */}
      {withRF === 0 && rows.length > 0 && page && (
        <Banner tone="accent">{page.note}.</Banner>
      )}
      <div className={`client-observability-workspace${selectedClient ? ' is-open' : ''}`}>
        {/* counted="all" now: the counts come from an aggregate over the whole
            filtered table rather than from the rows on screen. */}
        <div className="client-observability-filters" role="region" aria-label="Client filters">
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
        </div>
        <div className="client-observability-list" role="region" aria-label="Client list">
          <Card title={`Client devices (${(page?.total ?? 0).toLocaleString()})`} pad={false}>
            {err && (
              <div role="alert" style={{ padding: 12 }}>
                <Banner tone="critical">{err}</Banner>
              </div>
            )}
            <DataGrid
              tableLabel="Client devices"
              totalRows={page?.total}
              rowOffset={offset}
              rows={rows}
              columns={columns}
              prefs={colPrefs}
              onPrefsChange={setColPrefs}
              rowKey={(c) => c.mac}
              onRowClick={setSelectedClient}
              empty={loading
                ? 'Loading clients…'
                : err
                  ? 'Clients could not load for these filters.'
                  : 'No clients match these filters. The inventory is built from the baseline poll, so it fills in within a minute of a device coming online.'}
            />
            {page && rows.some((c) => c.scope === 'unknown') && (
              <div
                role="note"
                style={{ padding: '8px 12px', fontSize: 11, color: 'var(--text-muted)' }}
              >
                {page.scope_note}
              </div>
            )}
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
        {selectedClient && (
          <ClientObservability
            key={selectedClient.mac}
            client={selectedClient}
            onClose={() => setSelectedClient(null)}
          />
        )}
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
