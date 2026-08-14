import { useCallback, useEffect, useState } from 'react'
import { api, onUnauthorized } from './lib/api'
import type { Client, Dashboard as DashboardData, Device } from './lib/api'
import { Auth } from './screens/Auth'
import { Dashboard } from './screens/Dashboard'
import { Devices } from './screens/Devices'
import { Clients } from './screens/Clients'
import { Logs } from './screens/Logs'
import { Adopt } from './screens/Adopt'
import { Settings } from './screens/Settings'
import { Banner } from './components/ui'
import { live } from './lib/live'

type Screen = 'dashboard' | 'devices' | 'clients' | 'settings' | 'adopt' | 'logs'

const NAV: { id: Screen; label: string; glyph: string }[] = [
  { id: 'dashboard', label: 'Dashboard', glyph: '◱' },
  { id: 'devices', label: 'Devices', glyph: '⬡' },
  { id: 'clients', label: 'Client Devices', glyph: '⬤' },
  { id: 'settings', label: 'Settings', glyph: '⚙' },
  { id: 'adopt', label: 'Adopt a device', glyph: '＋' },
  { id: 'logs', label: 'Logs', glyph: '≣' },
]

export function App() {
  const [ready, setReady] = useState(false)
  const [needsSetup, setNeedsSetup] = useState(false)
  const [username, setUsername] = useState<string | null>(null)
  const [screen, setScreen] = useState<Screen>('dashboard')
  const [theme, setTheme] = useState<'dark' | 'light'>('dark')

  const [dash, setDash] = useState<DashboardData | null>(null)
  const [devices, setDevices] = useState<Device[]>([])
  const [clients, setClients] = useState<Client[]>([])
  const [clientNote, setClientNote] = useState('')
  const [err, setErr] = useState('')

  useEffect(() => {
    document.documentElement.dataset.theme = theme
  }, [theme])

  // A 401 anywhere drops us back to the sign-in screen rather than leaving a
  // signed-out page showing whatever it last loaded.
  useEffect(() => {
    const drop = () => setUsername(null)
    onUnauthorized.add(drop)
    return () => {
      onUnauthorized.delete(drop)
    }
  }, [])

  useEffect(() => {
    ;(async () => {
      try {
        const state = await api.setupState()
        setNeedsSetup(state.needs_setup)
        if (!state.needs_setup) {
          try {
            const s = await api.session()
            setUsername(s.username)
          } catch {
            /* not signed in; the auth screen handles it */
          }
        }
      } catch {
        setErr('Cannot reach the controller.')
      } finally {
        setReady(true)
      }
    })()
  }, [])

  const refresh = useCallback(async () => {
    if (!username) return
    try {
      const [d, dv, cl] = await Promise.all([
        api.dashboard(),
        api.devices(),
        api.clients(),
      ])
      setDash(d)
      setDevices(dv.devices)
      setClients(cl.clients)
      setClientNote(cl.note)
      setErr('')
    } catch (e) {
      // A failed refresh keeps the last good data on screen and says so. Blanking
      // the page on one dropped request would be its own kind of lie.
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [username])

  useEffect(() => {
    if (!username) return
    refresh()
    live.connect()
    // The fleet list still refreshes on a timer, but slowly: it changes when a
    // device is adopted or goes offline, not every poll. Per-device detail is
    // pushed over the live channel instead.
    const t = setInterval(refresh, 30_000)
    return () => clearInterval(t)
  }, [username, refresh])

  // Close the live channel on sign-out, but not before we know whether anyone
  // is signed in: `ready` gates it so the initial render does not close a
  // channel that has not been opened.
  useEffect(() => {
    if (!ready || username) return
    live.close()
  }, [ready, username])

  if (!ready) return null
  if (!username) {
    return (
      <Auth
        needsSetup={needsSetup}
        onSignedIn={(u) => {
          setNeedsSetup(false)
          setUsername(u)
        }}
      />
    )
  }

  return (
    <div style={{ height: '100%', display: 'flex', flexDirection: 'column' }}>
      <header
        style={{
          height: 40,
          flex: '0 0 40px',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          padding: '0 14px',
          background: 'var(--surface-1)',
          borderBottom: '1px solid var(--border)',
        }}
      >
        <strong style={{ fontSize: 13 }}>oonfeeWRT</strong>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, fontSize: 12 }}>
          <button
            onClick={() => setTheme((t) => (t === 'dark' ? 'light' : 'dark'))}
            title="Toggle theme"
            style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'var(--text-secondary)', fontSize: 14 }}
          >
            ◐
          </button>
          <span style={{ color: 'var(--text-secondary)' }}>{username}</span>
          <button
            onClick={async () => {
              await api.logout().catch(() => {})
              setUsername(null)
            }}
            style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'var(--accent)', fontSize: 12 }}
          >
            Sign out
          </button>
        </div>
      </header>

      <div style={{ flex: 1, display: 'flex', minHeight: 0 }}>
        <nav
          style={{
            width: 52,
            flex: '0 0 52px',
            background: 'var(--surface-1)',
            borderRight: '1px solid var(--border)',
            paddingTop: 8,
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'center',
            gap: 4,
          }}
        >
          {NAV.map((n) => (
            <button
              key={n.id}
              title={n.label}
              aria-label={n.label}
              aria-current={screen === n.id ? 'page' : undefined}
              onClick={() => setScreen(n.id)}
              style={{
                width: 36,
                height: 36,
                borderRadius: 6,
                border: 'none',
                cursor: 'pointer',
                fontSize: 16,
                background: screen === n.id ? 'var(--accent-soft)' : 'transparent',
                color: screen === n.id ? 'var(--accent)' : 'var(--text-secondary)',
              }}
            >
              {n.glyph}
            </button>
          ))}
        </nav>

        <main style={{ flex: 1, overflow: 'auto', padding: 14, minWidth: 0 }}>
          {err && (
            <div style={{ marginBottom: 12 }}>
              <Banner tone="critical">
                {err} — showing the last data that loaded successfully.
              </Banner>
            </div>
          )}
          {screen === 'dashboard' && dash && <Dashboard data={dash} />}
          {screen === 'devices' && (
            <Devices
              devices={devices}
              onAdopt={() => setScreen('adopt')}
              onChanged={refresh}
            />
          )}
          {screen === 'clients' && <Clients clients={clients} note={clientNote} />}
          {screen === 'settings' && <Settings devices={devices} />}
          {screen === 'adopt' && <Adopt onAdopted={refresh} />}
          {screen === 'logs' && <Logs />}
        </main>
      </div>
    </div>
  )
}
