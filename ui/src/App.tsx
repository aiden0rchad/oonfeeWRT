import { Component, Fragment, useCallback, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { api, ApiError, onControllerRestart, onUnauthorized } from './lib/api'
import type { Dashboard as DashboardData, Device, SessionInfo } from './lib/api'
import { Auth } from './screens/Auth'
import { Dashboard } from './screens/Dashboard'
import { Devices } from './screens/Devices'
import { Clients } from './screens/Clients'
import { Logs } from './screens/Logs'
import { Adopt } from './screens/Adopt'
import { Settings } from './screens/Settings'
import { PolicyEngine } from './screens/PolicyEngine'
import { Topology } from './screens/Topology'
import { Radios } from './screens/Radios'
import { Banner, Button } from './components/ui'
import { NavigationIcon } from './components/icons'
import type { NavigationIconName } from './components/icons'
import { live } from './lib/live'

type Screen = 'dashboard' | 'topology' | 'radios' | 'devices' | 'clients' | 'policy' | 'settings' | 'adopt' | 'logs'

const NAV: { id: Screen; label: string; icon: NavigationIconName }[] = [
  { id: 'dashboard', label: 'Dashboard', icon: 'dashboard' },
  { id: 'topology', label: 'Topology', icon: 'topology' },
  { id: 'radios', label: 'Radios', icon: 'radios' },
  { id: 'devices', label: 'Devices', icon: 'devices' },
  { id: 'clients', label: 'Client Devices', icon: 'clients' },
  { id: 'policy', label: 'Policy Engine', icon: 'policy' },
  { id: 'settings', label: 'Settings', icon: 'settings' },
  { id: 'adopt', label: 'Adopt a device', icon: 'adopt' },
  { id: 'logs', label: 'Logs', icon: 'logs' },
]

function navigationPreferenceKey(username: string) {
  return `oonfeewrt:navigation:expanded:${encodeURIComponent(window.location.origin)}:${encodeURIComponent(username)}`
}

function readNavigationPreference(username: string) {
  try {
    return window.localStorage.getItem(navigationPreferenceKey(username)) === 'true'
  } catch {
    return false
  }
}

function writeNavigationPreference(username: string, expanded: boolean) {
  try {
    window.localStorage.setItem(navigationPreferenceKey(username), String(expanded))
  } catch {
    // Storage can be blocked; navigation remains usable for this session.
  }
}

function screenFromPath(pathname: string): Screen {
  const id = pathname.replace(/^\/+|\/+$/g, '')
  return NAV.some((item) => item.id === id) ? id as Screen : 'dashboard'
}

function screenPath(screen: Screen) {
  return screen === 'dashboard' ? '/' : `/${screen}`
}

class ScreenBoundary extends Component<{ name: string; children: ReactNode }, { error: string }> {
  state = { error: '' }

  static getDerivedStateFromError(error: unknown) {
    return { error: error instanceof Error ? error.message : String(error) }
  }

  render() {
    if (!this.state.error) return this.props.children
    return (
      <div style={{ display: 'grid', gap: 12 }}>
        <h1 style={{ margin: 0, fontSize: 20 }}>{this.props.name} unavailable</h1>
        <div role="alert"><Banner tone="critical">
          This screen could not render: {this.state.error}. Other controller screens remain available.
        </Banner></div>
        <div><Button onClick={() => this.setState({ error: '' })}>Retry screen</Button></div>
      </div>
    )
  }
}

export function App() {
  const [ready, setReady] = useState(false)
  const [bootstrapAttempt, setBootstrapAttempt] = useState(0)
  const [bootstrapErr, setBootstrapErr] = useState('')
  const [needsSetup, setNeedsSetup] = useState(false)
  const [session, setSession] = useState<SessionInfo | null>(null)
  const username = session?.username ?? null
  const [screen, setScreen] = useState<Screen>(() => screenFromPath(window.location.pathname))
  const [theme, setTheme] = useState<'dark' | 'light'>('dark')
  const [navigationExpanded, setNavigationExpanded] = useState(false)

  const [dash, setDash] = useState<DashboardData | null>(null)
  const [devices, setDevices] = useState<Device[]>([])
  const [devicesLoaded, setDevicesLoaded] = useState(false)
  const [refreshErrors, setRefreshErrors] = useState<{ dashboard?: string; devices?: string }>({})
  const [accountErr, setAccountErr] = useState('')
  const [signingOut, setSigningOut] = useState(false)
  const sessionGeneration = useRef(0)
  const refreshGeneration = useRef(0)
  const mainRef = useRef<HTMLElement>(null)

  const clearProtectedState = useCallback(() => {
    refreshGeneration.current++
    setDash(null)
    setDevices([])
    setDevicesLoaded(false)
    setRefreshErrors({})
    setAccountErr('')
  }, [])

  const navigate = useCallback((next: Screen) => {
    const path = screenPath(next)
    if (window.location.pathname !== path) window.history.pushState(null, '', path)
    setScreen(next)
  }, [])

  const dropSession = useCallback(() => {
    sessionGeneration.current++
    clearProtectedState()
    setSession(null)
  }, [clearProtectedState])

  const beginSession = useCallback((nextSession: SessionInfo) => {
    sessionGeneration.current++
    clearProtectedState()
    setNeedsSetup(false)
    setSession(nextSession)
    setNavigationExpanded(readNavigationPreference(nextSession.username))
  }, [clearProtectedState])

  useEffect(() => {
    document.documentElement.dataset.theme = theme
  }, [theme])

  useEffect(() => {
    const followHistory = () => setScreen(screenFromPath(window.location.pathname))
    window.addEventListener('popstate', followHistory)
    return () => window.removeEventListener('popstate', followHistory)
  }, [])

  // A 401 anywhere drops us back to the sign-in screen rather than leaving a
  // signed-out page showing whatever it last loaded.
  useEffect(() => {
    onUnauthorized.add(dropSession)
    const reload = () => window.location.reload()
    onControllerRestart.add(reload)
    return () => {
      onUnauthorized.delete(dropSession)
      onControllerRestart.delete(reload)
    }
  }, [dropSession])

  useEffect(() => {
    let cancelled = false
    setReady(false)
    setBootstrapErr('')
    ;(async () => {
      try {
        const state = await api.setupState()
        if (cancelled) return
        setNeedsSetup(state.needs_setup)
        if (!state.needs_setup) {
          try {
            const s = await api.session()
            if (!cancelled) beginSession(s)
          } catch (e) {
            // A 401 means the sign-in screen is correct. Transport and server
            // failures do not: setup mode is unknown until the controller answers.
            if (!(e instanceof ApiError && e.status === 401)) throw e
          }
        }
      } catch (e) {
        if (!cancelled) {
          setBootstrapErr(e instanceof Error ? e.message : 'Cannot reach the controller.')
        }
      } finally {
        if (!cancelled) setReady(true)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [beginSession, bootstrapAttempt])

  const refresh = useCallback(async () => {
    if (!username) return
    const generation = ++refreshGeneration.current
    const session = sessionGeneration.current
    const [dashboardResult, devicesResult] = await Promise.allSettled([
      api.dashboard(),
      api.devices(),
    ])
    if (generation !== refreshGeneration.current || session !== sessionGeneration.current) return

    if (dashboardResult.status === 'fulfilled') setDash(dashboardResult.value)
    if (devicesResult.status === 'fulfilled') {
      setDevices(devicesResult.value.devices)
      setDevicesLoaded(true)
    }
    const reason = (value: unknown) => value instanceof Error ? value.message : String(value)
    setRefreshErrors({
      dashboard: dashboardResult.status === 'rejected' ? reason(dashboardResult.reason) : undefined,
      devices: devicesResult.status === 'rejected' ? reason(devicesResult.reason) : undefined,
    })
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

  useEffect(() => {
    if (!username) return
    document.title = `${NAV.find((item) => item.id === screen)?.label ?? 'oonfeeWRT'} — oonfeeWRT`
    const timer = window.setTimeout(() => {
      const target = mainRef.current?.querySelector<HTMLElement>('h1') ?? mainRef.current
      if (target && target !== mainRef.current) target.tabIndex = -1
      target?.focus()
    }, 0)
    return () => window.clearTimeout(timer)
  }, [screen, username])

  if (!ready) {
    return (
      <main style={{ height: '100%', display: 'grid', placeItems: 'center' }}>
        <div role="status">Connecting to oonfeeWRT…</div>
      </main>
    )
  }
  if (bootstrapErr) {
    return (
      <main style={{ height: '100%', display: 'grid', placeItems: 'center', padding: 24 }}>
        <div style={{ width: 420, maxWidth: '100%', display: 'grid', gap: 12 }}>
          <h1 style={{ margin: 0, fontSize: 18 }}>oonfeeWRT is unavailable</h1>
          <div role="alert"><Banner tone="critical">{bootstrapErr}</Banner></div>
          <Button onClick={() => setBootstrapAttempt((attempt) => attempt + 1)}>Retry connection</Button>
        </div>
      </main>
    )
  }
  if (!username) {
    return (
      <Auth
        needsSetup={needsSetup}
        onSignedIn={beginSession}
      />
    )
  }

  const navigationWidth = navigationExpanded ? 208 : 64

  return (
    <div style={{ height: '100%', display: 'flex', flexDirection: 'column' }}>
      <a className="skip-link" href="#main-content">Skip to main content</a>
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
            aria-label={`${theme === 'dark' ? 'Dark' : 'Light'} theme active; switch to ${theme === 'dark' ? 'light' : 'dark'} theme`}
            title={`Switch to ${theme === 'dark' ? 'light' : 'dark'} theme`}
            style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'var(--text-secondary)', fontSize: 14 }}
          >
            ◐
          </button>
          <span style={{ color: 'var(--text-secondary)' }}>{username}</span>
          <button
            disabled={signingOut}
            onClick={async () => {
              setSigningOut(true)
              setAccountErr('')
              try {
                const result = await api.logout()
                if (!result.ok) throw new Error('the controller did not confirm logout')
                dropSession()
              } catch (e) {
                // A 401 already fired onUnauthorized and cleared local state.
                if (!(e instanceof ApiError && e.status === 401)) {
                  setAccountErr(`Sign out failed: ${e instanceof Error ? e.message : String(e)}. You are still signed in.`)
                }
              } finally {
                setSigningOut(false)
              }
            }}
            style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'var(--accent-text)', fontSize: 12 }}
          >
            {signingOut ? 'Signing out…' : 'Sign out'}
          </button>
        </div>
      </header>

      <div style={{ flex: 1, display: 'flex', minHeight: 0 }}>
        <nav
          aria-label="Main navigation"
          style={{
            width: navigationWidth,
            flex: `0 0 ${navigationWidth}px`,
            background: 'var(--surface-1)',
            borderRight: '1px solid var(--border)',
            padding: '8px 0',
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'center',
            gap: 4,
            overflowX: 'hidden',
            overflowY: 'auto',
          }}
        >
          <button
            className="app-nav-control"
            type="button"
            aria-label={navigationExpanded ? 'Collapse navigation' : 'Expand navigation'}
            aria-expanded={navigationExpanded}
            title={navigationExpanded ? 'Collapse navigation' : 'Expand navigation'}
            onClick={() => {
              const next = !navigationExpanded
              setNavigationExpanded(next)
              writeNavigationPreference(username, next)
            }}
            style={{
              width: navigationExpanded ? 'calc(100% - 16px)' : 44,
              minHeight: 44,
              padding: navigationExpanded ? '0 10px' : 0,
              display: 'flex',
              alignItems: 'center',
              justifyContent: navigationExpanded ? 'flex-start' : 'center',
              gap: 10,
              flex: '0 0 auto',
              borderRadius: 8,
              border: '1px solid var(--border)',
              color: 'var(--text-secondary)',
              cursor: 'pointer',
              marginBottom: 4,
            }}
          >
            <NavigationIcon name={navigationExpanded ? 'collapse' : 'expand'} />
            {navigationExpanded && <span style={{ whiteSpace: 'nowrap' }}>Collapse</span>}
          </button>
          {NAV.map((n) => (
            <Fragment key={n.id}>
              {n.id === 'settings' && (
                <div
                  className="app-nav-divider"
                  data-expanded={navigationExpanded}
                  role="separator"
                  aria-label="Controller tools"
                >
                  {navigationExpanded && <span>Controller</span>}
                </div>
              )}
              <button
                className="app-nav-item"
                type="button"
                title={n.label}
                aria-label={n.label}
                aria-current={screen === n.id ? 'page' : undefined}
                onClick={() => navigate(n.id)}
                style={{
                  width: navigationExpanded ? 'calc(100% - 16px)' : 44,
                  minHeight: 44,
                  padding: navigationExpanded ? '0 10px' : 0,
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: navigationExpanded ? 'flex-start' : 'center',
                  gap: 10,
                  flex: '0 0 auto',
                  borderRadius: 8,
                  border: 'none',
                  cursor: 'pointer',
                  boxShadow: screen === n.id ? 'inset 3px 0 0 var(--accent)' : 'none',
                  fontWeight: screen === n.id ? 650 : 500,
                }}
              >
                <NavigationIcon name={n.icon} />
                {navigationExpanded && <span style={{ whiteSpace: 'nowrap' }}>{n.label}</span>}
              </button>
            </Fragment>
          ))}
        </nav>

        <main ref={mainRef} id="main-content" tabIndex={-1} style={{ flex: 1, overflow: 'auto', padding: 14, minWidth: 0, outline: 'none' }}>
          {accountErr && (
            <div style={{ marginBottom: 12 }}>
              <div role="alert"><Banner tone="critical">{accountErr}</Banner></div>
            </div>
          )}
          {screen === 'dashboard' && refreshErrors.dashboard && (
            <div role="alert" style={{ marginBottom: 12 }}>
              <Banner tone="critical">
                Dashboard refresh failed: {refreshErrors.dashboard}
                {dash ? ' The last successful dashboard remains visible.' : ''}
              </Banner>
            </div>
          )}
          {screen === 'devices' && refreshErrors.devices && (
            <div role="alert" style={{ marginBottom: 12 }}>
              <Banner tone="critical">
                Device refresh failed: {refreshErrors.devices}
                {devicesLoaded ? ' The last successful device list remains visible.' : ''}
              </Banner>
            </div>
          )}
          <ScreenBoundary key={screen} name={NAV.find((item) => item.id === screen)?.label ?? 'Screen'}>
            {screen === 'dashboard' && (dash
              ? <Dashboard data={dash} onOpenTopology={() => navigate('topology')} />
              : !refreshErrors.dashboard && <div role="status">Loading dashboard…</div>)}
            {screen === 'topology' && (
              <Topology onReviewCapabilities={() => navigate('devices')} />
            )}
            {screen === 'radios' && <Radios />}
            {screen === 'devices' && (
              <Devices
                devices={devices}
                devicesLoaded={devicesLoaded}
                devicesError={refreshErrors.devices}
                onAdopt={() => navigate('adopt')}
                onChanged={refresh}
              />
            )}
            {screen === 'clients' && <Clients />}
            {screen === 'policy' && (
              <PolicyEngine onReviewChanges={() => navigate('settings')} />
            )}
            {screen === 'settings' && session && (
              <Settings
                devices={devices}
                devicesLoaded={devicesLoaded}
                devicesError={refreshErrors.devices}
                session={session}
                onSessionChange={setSession}
                onCurrentSessionRevoked={dropSession}
              />
            )}
            {screen === 'adopt' && <Adopt onAdopted={refresh} />}
            {screen === 'logs' && <Logs />}
          </ScreenBoundary>
        </main>
      </div>
    </div>
  )
}
