import { Component, useCallback, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { api, ApiError, isDemo, onControllerRestart, onUnauthorized } from './lib/api'
import type { Dashboard as DashboardData, Device, SessionInfo } from './lib/api'
import { Auth } from './screens/Auth'
import { Dashboard } from './screens/Dashboard'
import { Statistics } from './screens/Statistics'
import { Reports } from './screens/Reports'
import { Alerts } from './screens/Alerts'
import { Firmware } from './screens/Firmware'
import { Integrations } from './screens/Integrations'
import { Devices } from './screens/Devices'
import { Clients } from './screens/Clients'
import { Logs } from './screens/Logs'
import { Adopt } from './screens/Adopt'
import { Settings } from './screens/Settings'
import { AccountsPage } from './screens/AccountsPage'
import { PolicyEngine } from './screens/PolicyEngine'
import { Topology } from './screens/Topology'
import { Radios } from './screens/Radios'
import { Banner, Button } from './components/ui'
import { NavigationIcon } from './components/icons'
import type { NavigationIconName } from './components/icons'
import { live } from './lib/live'
import './Shell.css'

type Screen = 'dashboard' | 'statistics' | 'reports' | 'alerts' | 'firmware' | 'integrations' | 'topology' | 'radios' | 'devices' | 'clients' | 'policy' | 'adopt' | 'settings' | 'accounts' | 'logs'
type SettingsIntent = 'ipv6' | null
type Theme = 'dark' | 'light'

const themePreferenceKey = 'oonfeewrt:theme'

function readThemePreference(): Theme {
  try {
    return window.localStorage.getItem(themePreferenceKey) === 'light' ? 'light' : 'dark'
  } catch {
    return 'dark'
  }
}

const NAV: { id: Screen; label: string; icon: NavigationIconName }[] = [
  { id: 'dashboard', label: 'Dashboard', icon: 'dashboard' },
  { id: 'statistics', label: 'Statistics', icon: 'statistics' },
  { id: 'reports', label: 'Reports', icon: 'reports' },
  { id: 'alerts', label: 'Alerts', icon: 'alerts' },
  { id: 'topology', label: 'Topology', icon: 'topology' },
  { id: 'radios', label: 'Radios', icon: 'radios' },
  { id: 'devices', label: 'Devices', icon: 'devices' },
  { id: 'clients', label: 'Client Devices', icon: 'clients' },
  { id: 'policy', label: 'Policy Engine', icon: 'policy' },
  { id: 'adopt', label: 'Adopt a device', icon: 'adopt' },
  { id: 'firmware', label: 'Firmware', icon: 'firmware' },
  { id: 'integrations', label: 'Integrations', icon: 'integrations' },
  { id: 'settings', label: 'Settings', icon: 'settings' },
  { id: 'accounts', label: 'Accounts', icon: 'accounts' },
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
  const [mobileNavigationOpen, setMobileNavigationOpen] = useState(false)
  const navigationRef = useRef<HTMLElement | null>(null)
  const menuButtonRef = useRef<HTMLButtonElement | null>(null)
  useEffect(() => {
    if (!mobileNavigationOpen) return
    const nav = navigationRef.current
    const focusable = () => Array.from(nav?.querySelectorAll<HTMLElement>('button:not(:disabled)') ?? [])
      .filter((el) => getComputedStyle(el).display !== 'none')
    focusable()[0]?.focus()
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') { setMobileNavigationOpen(false); menuButtonRef.current?.focus() }
      if (event.key !== 'Tab') return
      const items = focusable()
      const first = items[0], last = items.at(-1)
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus() }
      if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus() }
    }
    const onResize = () => { if (window.innerWidth > 720) setMobileNavigationOpen(false) }
    document.addEventListener('keydown', onKey)
    window.addEventListener('resize', onResize)
    return () => { document.removeEventListener('keydown', onKey); window.removeEventListener('resize', onResize) }
  }, [mobileNavigationOpen])
  const [ready, setReady] = useState(false)
  const [bootstrapAttempt, setBootstrapAttempt] = useState(0)
  const [bootstrapErr, setBootstrapErr] = useState('')
  const [needsSetup, setNeedsSetup] = useState(false)
  const [session, setSession] = useState<SessionInfo | null>(null)
  const username = session?.username ?? null
  const [screen, setScreen] = useState<Screen>(() => screenFromPath(window.location.pathname))
  const [settingsIntent, setSettingsIntent] = useState<SettingsIntent>(null)
  const [theme, setTheme] = useState<Theme>(readThemePreference)
  const [navigationExpanded, setNavigationExpanded] = useState(false)

  const [dash, setDash] = useState<DashboardData | null>(null)
  const [devices, setDevices] = useState<Device[]>([])
  const [devicesLoaded, setDevicesLoaded] = useState(false)
  const [refreshErrors, setRefreshErrors] = useState<{ dashboard?: string; devices?: string }>({})
  const [accountErr, setAccountErr] = useState('')
  const [signingOut, setSigningOut] = useState(false)
  const sessionGeneration = useRef(0)
  const refreshGeneration = useRef(0)
  const dashboardRefresh = useRef<AbortController | null>(null)
  const devicesRefresh = useRef<AbortController | null>(null)
  const mainRef = useRef<HTMLElement>(null)

  const clearProtectedState = useCallback(() => {
    setMobileNavigationOpen(false)
    refreshGeneration.current++
    dashboardRefresh.current?.abort()
    devicesRefresh.current?.abort()
    dashboardRefresh.current = null
    devicesRefresh.current = null
    setDash(null)
    setDevices([])
    setDevicesLoaded(false)
    setRefreshErrors({})
    setAccountErr('')
    setSettingsIntent(null)
  }, [])

  const navigate = useCallback((next: Screen, intent: SettingsIntent = null) => {
    setMobileNavigationOpen(false)
    const path = screenPath(next)
    if (window.location.pathname !== path) window.history.pushState(null, '', path)
    setSettingsIntent(next === 'settings' ? intent : null)
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
    try {
      window.localStorage.setItem(themePreferenceKey, theme)
    } catch {
      // Theme switching remains available when browser storage is blocked.
    }
  }, [theme])

  useEffect(() => {
    const followHistory = () => {
      setSettingsIntent(null)
      setScreen(screenFromPath(window.location.pathname))
    }
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

  const refresh = useCallback(() => {
    if (!username) return Promise.resolve()
    const generation = refreshGeneration.current
    const session = sessionGeneration.current
    const current = () => generation === refreshGeneration.current && session === sessionGeneration.current
    const reason = (value: unknown) => value instanceof Error ? value.message : String(value)
    const requests: Promise<void>[] = []

    if (!dashboardRefresh.current) {
      const controller = new AbortController()
      dashboardRefresh.current = controller
      requests.push((async () => {
        try {
          const next = await api.dashboard(controller.signal)
          if (!current()) return
          setDash(next)
          setRefreshErrors((errors) => ({ ...errors, dashboard: undefined }))
        } catch (error) {
          if (current()) setRefreshErrors((errors) => ({ ...errors, dashboard: reason(error) }))
        } finally {
          if (dashboardRefresh.current === controller) dashboardRefresh.current = null
        }
      })())
    }

    if (!devicesRefresh.current) {
      const controller = new AbortController()
      devicesRefresh.current = controller
      requests.push((async () => {
        try {
          const next = await api.devices(controller.signal)
          if (!current()) return
          setDevices(next.devices)
          setDevicesLoaded(true)
          setRefreshErrors((errors) => ({ ...errors, devices: undefined }))
        } catch (error) {
          if (current()) setRefreshErrors((errors) => ({ ...errors, devices: reason(error) }))
        } finally {
          if (devicesRefresh.current === controller) devicesRefresh.current = null
        }
      })())
    }

    return Promise.all(requests).then(() => undefined)
  }, [username])

  useEffect(() => {
    if (!username) return
    refresh()
    live.connect()
    // The fleet list still refreshes on a timer, but slowly: it changes when a
    // device is adopted or goes offline, not every poll. Per-device detail is
    // pushed over the live channel instead.
    const t = setInterval(refresh, 30_000)
    return () => {
      clearInterval(t)
      dashboardRefresh.current?.abort()
      devicesRefresh.current?.abort()
      dashboardRefresh.current = null
      devicesRefresh.current = null
    }
  }, [username, refresh])

  // Close the live channel on sign-out, but not before we know whether anyone
  // is signed in: `ready` gates it so the initial render does not close a
  // channel that has not been opened.
  useEffect(() => {
    if (!ready || username) return
    live.close()
  }, [ready, username])

  const headingReady = screen !== 'dashboard' || dash != null
  useEffect(() => {
    if (!username) return
    document.title = `${NAV.find((item) => item.id === screen)?.label ?? 'oonfeeWRT'} — oonfeeWRT`
    if (!headingReady) return
    const timer = window.setTimeout(() => {
      const target = mainRef.current?.querySelector<HTMLElement>('h1') ?? mainRef.current
      if (target && target !== mainRef.current) target.tabIndex = -1
      target?.focus()
    }, 0)
    return () => window.clearTimeout(timer)
  }, [screen, username, headingReady])

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

  const showNavigationLabels = navigationExpanded || mobileNavigationOpen
  const navigationWidth = showNavigationLabels ? 208 : 64

  return (
    <div style={{ height: '100%', display: 'flex', flexDirection: 'column' }}>
      <a className="skip-link" href="#main-content">Skip to main content</a>
      <header className="app-topbar">
        <button ref={menuButtonRef} className="mobile-menu-button" type="button" aria-label="Open navigation"
          aria-expanded={mobileNavigationOpen} aria-controls="app-navigation" onClick={() => setMobileNavigationOpen(true)}>
          <NavigationIcon name="expand" />
        </button>
        <strong className="app-brand">oonfeeWRT</strong>
        {isDemo && <span className="app-demo-label">Demo · synthetic data · read only</span>}
        <div className="app-account-controls">
          <button
            className="app-theme-control"
            onClick={() => setTheme((t) => (t === 'dark' ? 'light' : 'dark'))}
            aria-label={`${theme === 'dark' ? 'Dark' : 'Light'} theme active; switch to ${theme === 'dark' ? 'light' : 'dark'} theme`}
            title={`Switch to ${theme === 'dark' ? 'light' : 'dark'} theme`}
          >
            ◐
          </button>
          <span className="app-account-name" title={username}>{username}</span>
          {!isDemo && <button
            className="app-signout-control"
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
          >
            {signingOut ? 'Signing out…' : 'Sign out'}
          </button>}
        </div>
      </header>

      <div style={{ flex: 1, display: 'flex', minHeight: 0 }}>
        {mobileNavigationOpen && <button className="mobile-nav-backdrop" type="button" tabIndex={-1} aria-label="Dismiss navigation"
          onClick={() => { setMobileNavigationOpen(false); menuButtonRef.current?.focus() }} />}
        <nav
          id="app-navigation"
          ref={navigationRef}
          className="app-navigation"
          data-mobile-open={mobileNavigationOpen}
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
          <button className="mobile-close-button" type="button"
            onClick={() => { setMobileNavigationOpen(false); menuButtonRef.current?.focus() }}>Close navigation ×</button>
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
          {[false, true].map((controller) => (
            <div key={String(controller)} className={`app-nav-section ${controller ? 'app-nav-controller' : 'app-nav-primary'}`}>
              {controller && (
                <div
                  className="app-nav-divider"
                  data-expanded={showNavigationLabels}
                  role="separator"
                  aria-label="Controller tools"
                >
                  {showNavigationLabels && <span>Controller</span>}
                </div>
              )}
              {NAV.filter((n) => ['settings', 'accounts', 'logs'].includes(n.id) === controller).map((n) => (
              <button
                key={n.id}
                className="app-nav-item"
                type="button"
                title={n.label}
                aria-label={n.label}
                aria-current={screen === n.id ? 'page' : undefined}
                onClick={() => navigate(n.id)}
                style={{
                  width: showNavigationLabels ? 'calc(100% - 16px)' : 44,
                  minHeight: 44,
                  padding: showNavigationLabels ? '0 10px' : 0,
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: showNavigationLabels ? 'flex-start' : 'center',
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
                {showNavigationLabels && <span style={{ whiteSpace: 'nowrap' }}>{n.label}</span>}
              </button>
              ))}
            </div>
          ))}
        </nav>

        <main ref={mainRef} id="main-content" className="app-main" tabIndex={-1} inert={mobileNavigationOpen || undefined}>
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
            {screen === 'statistics' && <Statistics />}
            {screen === 'reports' && <Reports />}
            {screen === 'alerts' && session && <Alerts devices={devices} session={session} />}
            {screen === 'firmware' && session && <Firmware session={session} />}
            {screen === 'integrations' && session && <Integrations devices={devices} session={session} />}
            {screen === 'topology' && (
              <Topology userKey={session ? `${session.admin_id}:${session.username}` : undefined} onReviewCapabilities={() => navigate('devices')} />
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
                initialNetworkSection={settingsIntent}
                onInitialNetworkSectionHandled={() => setSettingsIntent(null)}
              />
            )}
            {screen === 'accounts' && session && (
              <AccountsPage
                session={session}
                onSessionChange={setSession}
                onCurrentSessionRevoked={dropSession}
              />
            )}
            {screen === 'adopt' && <Adopt onAdopted={refresh} />}
            {screen === 'logs' && (
              <Logs onConfigureIPv6={() => {
                navigate('settings', 'ipv6')
              }} />
            )}
          </ScreenBoundary>
        </main>
      </div>
    </div>
  )
}
