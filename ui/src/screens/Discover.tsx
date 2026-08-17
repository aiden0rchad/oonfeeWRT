import { useEffect, useState } from 'react'
import { api, ApiError } from '../lib/api'
import type { Discovered, ScanPlan, ScanResult } from '../lib/api'
import { Button, Banner, Card } from '../components/ui'

/**
 * Look for OpenWrt devices on the local network.
 *
 * Sits above the add-by-address form rather than replacing it. Discovery cannot
 * work at all on a bridged container or on Docker Desktop — there is no layer 2
 * to sweep — so a UI that made scanning the primary path would be broken for a
 * large share of installs (ARCHITECTURE §1). Scan when it helps; type an
 * address when it does not.
 *
 * The scan probes with ubus `list` on no session: no credential is sent, no
 * session is created, nothing is written. That is stated on the screen, because
 * "let this thing scan my network" deserves an answer to "and do what to it?".
 */
export function Discover({ onPick }: { onPick: (host: string) => void }) {
  const [plan, setPlan] = useState<ScanPlan | null>(null)
  const [result, setResult] = useState<ScanResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  useEffect(() => {
    api
      .scanPlan()
      .then(setPlan)
      .catch((e) => setErr(e instanceof ApiError ? e.message : String(e)))
  }, [])

  async function scan() {
    setErr('')
    setBusy(true)
    setResult(null)
    try {
      setResult(await api.scan())
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card title="Find devices on this network">
      <div style={{ display: 'grid', gap: 10 }}>
        <p style={{ fontSize: 12, color: 'var(--text-secondary)', margin: 0 }}>
          Probes each address for an OpenWrt management endpoint. No password is
          sent and no session is created — the check is a single unauthenticated
          request that asks the device what it can do, which stock OpenWrt
          answers to anyone who can reach it.
        </p>

        {err && <Banner tone="critical">{err}</Banner>}

        {plan && (
          <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
            {plan.networks.length > 0 ? (
              <>
                Will probe <strong>{plan.hosts}</strong> address
                {plan.hosts === 1 ? '' : 'es'} on{' '}
                {/* Comma-separated, not margin-separated. A gap made only of
                    CSS disappears in copied text and in a screen reader, so two
                    networks read as one token — "192.168.1.0/2410.7.42.0/24" is
                    what the DOM actually said. */}
                {plan.networks.map((n, i) => (
                  <span key={n}>
                    {i > 0 && ', '}
                    <code>{n}</code>
                  </span>
                ))}
              </>
            ) : (
              <>No local network can be swept from here.</>
            )}
          </div>
        )}

        {/* Never silent. A subnet that was not looked at is the difference
            between "your device is not there" and "I did not look", and those
            are identical in an empty result list.
            Only before a scan, though: the result carries its own copy of the
            same list, and rendering both put "2 things not scanned" on the
            screen twice. */}
        {!result && plan?.skipped && plan.skipped.length > 0 && (
          <Skipped items={plan.skipped} />
        )}

        <div>
          <Button kind="primary" onClick={scan} disabled={busy || !plan}>
            {busy ? 'Scanning…' : 'Scan'}
          </Button>
        </div>

        {result && <Results result={result} onPick={onPick} />}
      </div>
    </Card>
  )
}

function Results({
  result,
  onPick,
}: {
  result: ScanResult
  onPick: (host: string) => void
}) {
  return (
    <div style={{ display: 'grid', gap: 8 }}>
      {/* The summary comes first and is always shown, including when nothing
          was found. "Probed 508, 12 answered, none was OpenWrt" is a result;
          an empty list on its own could equally be a broken scanner. */}
      <div style={{ fontSize: 11, color: 'var(--text-muted)' }}>
        Probed {result.swept} address{result.swept === 1 ? '' : 'es'} in{' '}
        {(result.elapsed_ms / 1000).toFixed(1)}s · {result.answered} answered ·{' '}
        {result.found.length} running OpenWrt
      </div>

      {result.found.length === 0 && (
        <Banner>
          Nothing on {result.networks.join(', ')} answered as an OpenWrt device.
          If yours is on another subnet, or this controller is in a container
          that cannot see your LAN, add it by address below — that path works
          everywhere and needs no discovery.
        </Banner>
      )}

      {result.found.map((d) => (
        <Candidate key={`${d.host}:${d.port}`} d={d} onPick={onPick} />
      ))}

      {result.skipped && result.skipped.length > 0 && <Skipped items={result.skipped} />}
    </div>
  )
}

function Candidate({ d, onPick }: { d: Discovered; onPick: (host: string) => void }) {
  const managed = d.known_device_id != null
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        padding: '8px 10px',
        borderRadius: 6,
        border: '1px solid var(--border-strong)',
        background: 'var(--surface-2)',
      }}
    >
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{ fontSize: 13, fontWeight: 600 }}>
          {d.host}
          {d.port !== 80 && `:${d.port}`}
        </div>
        <div style={{ fontSize: 11, color: 'var(--text-secondary)' }}>
          {describe(d, managed)}
        </div>
      </div>
      {managed ? (
        <span
          style={{ fontSize: 11, color: 'var(--text-muted)', whiteSpace: 'nowrap' }}
        >
          already managed as <strong>{d.known_name}</strong>
        </span>
      ) : (
        <Button onClick={() => onPick(d.host)}>Adopt this</Button>
      )}
    </div>
  )
}

/**
 * What can honestly be said about a device before authenticating.
 *
 * Not the model, and not the firmware: stock OpenWrt refuses `system.board` to
 * an unauthenticated caller (measured), so anything claiming to know the model
 * here would be inventing it. What the endpoint does reveal is the shape of the
 * device — how many radios have an SSID up, whether it routes, whether it hands
 * out leases — which is enough to tell two routers apart in a list.
 *
 * The caveat is dropped for a device already in the inventory, because there it
 * is simply false: the model was read at adoption and is on the row beside it.
 * Printing "model unknown until you sign in" next to "already managed as
 * Linksys WRT3200ACM" is the screen contradicting itself in one line.
 */
function describe(d: Discovered, managed: boolean): string {
  const parts: string[] = ['OpenWrt']
  if (d.signals.gateway) parts.push('gateway')
  if (d.signals.radios > 0) {
    parts.push(`${d.signals.radios} radio${d.signals.radios === 1 ? '' : 's'} up`)
  } else if (d.signals.wireless) {
    parts.push('wireless, no SSID up')
  }
  if (d.signals.dhcp) parts.push('DHCP server')
  const shape = parts.join(' · ')
  return managed ? shape : shape + ' — model unknown until you sign in'
}

function Skipped({ items }: { items: string[] }) {
  return (
    <details style={{ fontSize: 11, color: 'var(--text-muted)' }}>
      <summary style={{ cursor: 'pointer' }}>
        {items.length} thing{items.length === 1 ? '' : 's'} not scanned
      </summary>
      <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>
        {items.map((s) => (
          <li key={s} style={{ marginBottom: 3 }}>
            {s}
          </li>
        ))}
      </ul>
    </details>
  )
}
