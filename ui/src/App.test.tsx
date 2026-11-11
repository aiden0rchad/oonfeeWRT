import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from './lib/api'
import { App } from './App'
import { Auth } from './screens/Auth'

const mocks = vi.hoisted(() => ({
  api: {
    setupState: vi.fn(),
    session: vi.fn(),
    updates: vi.fn(),
    checkUpdates: vi.fn(),
    dashboard: vi.fn(),
    devices: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    account: vi.fn(),
    accountSessions: vi.fn(),
    accounts: vi.fn(),
  },
  live: { connect: vi.fn(), close: vi.fn() },
  radioCrash: false,
}))

vi.mock('./lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./lib/api')>()),
  api: mocks.api,
}))
vi.mock('./lib/live', () => ({ live: mocks.live }))
vi.mock('./screens/Dashboard', () => ({
  Dashboard: ({ onOpenTopology }: { onOpenTopology?: () => void }) => (
    <div>
      dashboard data
      {onOpenTopology && <button onClick={onOpenTopology}>Open topology</button>}
    </div>
  ),
}))
vi.mock('./screens/Statistics', () => ({ Statistics: () => <h1>Statistics</h1> }))
vi.mock('./screens/Firmware', () => ({ Firmware: () => <h2>Firmware inventory</h2> }))
vi.mock('./screens/Integrations', () => ({ Integrations: () => <h2>Integration connections</h2> }))
vi.mock('./screens/Topology', () => ({ Topology: () => <h1>Topology</h1> }))
vi.mock('./screens/Radios', () => ({ Radios: () => {
  if (mocks.radioCrash) throw new Error('radio fixture failed')
  return <h1>Radios &amp; Channel Plan</h1>
} }))
vi.mock('./screens/Logs', () => ({
  Logs: ({ onConfigureIPv6 }: { onConfigureIPv6?: () => void }) => (
    <div>
      <h1>Logs</h1>
      <button onClick={onConfigureIPv6}>Configure network IPv6</button>
    </div>
  ),
}))

function signedIn(username = 'admin') {
  mocks.api.setupState.mockResolvedValue({ needs_setup: false })
  mocks.api.session.mockResolvedValue({
    admin_id: 1,
    username,
    role: username === 'viewer' ? 'viewer' : 'owner',
    role_label: username === 'viewer' ? 'Read only' : 'Owner',
    csrf: 'token',
    reauthenticated_until: null,
  })
  mocks.api.dashboard.mockResolvedValue({ devices: {}, recent_events: [], recent_alert_events: [] })
  mocks.api.devices.mockResolvedValue({ devices: [] })
  mocks.api.account.mockResolvedValue({ account: {
    id: 1, username, role: username === 'viewer' ? 'viewer' : 'owner',
    role_label: username === 'viewer' ? 'Read only' : 'Owner', enabled: true,
    created_at: 1, last_login_at: 1, active_session_count: 0,
  } })
  mocks.api.accountSessions.mockResolvedValue({ sessions: [] })
  mocks.api.accounts.mockResolvedValue({ accounts: [], roles: [] })
}

function updateAvailable(tag = 'v0.1.9') {
  return {
    state: 'available' as const,
    install_kind: 'unknown' as const,
    current_version: 'v0.1.8',
    latest_version: tag.replace(/^v/, ''),
    latest_tag: tag,
    release_url: `https://github.com/aiden0rchad/oonfeeWRT/releases/tag/${tag}`,
    update_available: true,
    checked_at: '2026-09-18T12:00:00Z',
    next_check_at: '2026-09-19T00:00:00Z',
    message: 'A newer published stable release is available.',
  }
}

describe('App session boundaries', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    window.localStorage.clear()
    window.history.replaceState(null, '', '/')
    mocks.radioCrash = false
    mocks.api.logout.mockResolvedValue({ ok: true })
    mocks.api.updates.mockResolvedValue({
      state: 'unknown', current_version: 'dev', update_available: false,
      message: 'Development build.',
    })
  })

  it('shows controller bootstrap failures instead of guessing that sign-in mode is valid', async () => {
    mocks.api.setupState.mockRejectedValueOnce(new Error('controller offline'))
    render(<App />)

    expect(await screen.findByRole('heading', { name: 'oonfeeWRT is unavailable' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Sign in' })).toBeNull()

    mocks.api.setupState.mockResolvedValue({ needs_setup: false })
    mocks.api.session.mockRejectedValue(new ApiError(401, 'not signed in'))
    fireEvent.click(screen.getByRole('button', { name: 'Retry connection' }))
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeTruthy()
  })

  it('does not swallow non-authentication session failures', async () => {
    mocks.api.setupState.mockResolvedValue({ needs_setup: false })
    mocks.api.session.mockRejectedValue(new ApiError(503, 'database unavailable'))
    render(<App />)

    expect(await screen.findByText('database unavailable')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Sign in' })).toBeNull()
  })

  it('preserves a deep link through bootstrap and sign-in', async () => {
    window.history.replaceState(null, '', '/topology')
    mocks.api.setupState.mockResolvedValue({ needs_setup: false })
    mocks.api.session.mockRejectedValue(new ApiError(401, 'not signed in'))
    mocks.api.login.mockResolvedValue({
      admin_id: 1,
      username: 'admin',
      role: 'owner',
      role_label: 'Owner',
      csrf: 'token',
      reauthenticated_until: null,
    })
    mocks.api.dashboard.mockResolvedValue({ devices: {}, recent_events: [], recent_alert_events: [] })
    mocks.api.devices.mockResolvedValue({ devices: [] })
    render(<App />)

    fireEvent.change(await screen.findByLabelText('Username'), { target: { value: 'admin' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'controller password' } })
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('heading', { name: 'Topology' })).toBeTruthy()
    expect(window.location.pathname).toBe('/topology')
    expect(screen.queryByText('dashboard data')).toBeNull()
  })

  it('opens the full Topology workspace from the Dashboard summary', async () => {
    signedIn()
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Open topology' }))
    expect(await screen.findByRole('heading', { name: 'Topology' })).toBeTruthy()
    expect(window.location.pathname).toBe('/topology')
  })

  it('opens Statistics from navigation with route, title, and focus', async () => {
    signedIn()
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Statistics' }))
    const heading = await screen.findByRole('heading', { name: 'Statistics' })
    await waitFor(() => expect(document.activeElement).toBe(heading))
    expect(window.location.pathname).toBe('/statistics')
    expect(document.title).toBe('Statistics — oonfeeWRT')
  })

  it('opens Network Settings from an actionable IPv6 log warning', async () => {
    signedIn()
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Logs' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Configure network IPv6' }))

    expect(await screen.findByRole('heading', { name: 'Settings' })).toBeTruthy()
    expect(window.location.pathname).toBe('/settings')
    expect(screen.getByRole('tab', { name: 'Network' }).getAttribute('aria-selected')).toBe('true')
  })

  it('opens Accounts from navigation with its own route, title, and focus', async () => {
    signedIn()
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Accounts' }))
    const heading = await screen.findByRole('heading', { level: 1, name: 'Accounts' })
    await waitFor(() => expect(document.activeElement).toBe(heading))
    expect(window.location.pathname).toBe('/accounts')
    expect(document.title).toBe('Accounts — oonfeeWRT')
    expect(screen.getByRole('button', { name: 'Accounts' }).getAttribute('aria-current')).toBe('page')
    expect(screen.getByRole('tab', { name: 'My account' }).getAttribute('aria-selected')).toBe('true')
    expect(await screen.findByLabelText('Current password')).toBeTruthy()
  })

  it('restores Accounts deep links and follows browser history for a read-only account', async () => {
    window.history.replaceState(null, '', '/accounts')
    signedIn('viewer')
    render(<App />)

    expect(await screen.findByRole('heading', { level: 1, name: 'Accounts' })).toBeTruthy()
    expect(await screen.findByLabelText('Current password')).toBeTruthy()
    expect(screen.queryByRole('tab', { name: 'Manage accounts' })).toBeNull()

    fireEvent.click(screen.getByRole('button', { name: 'Logs' }))
    expect(await screen.findByRole('heading', { name: 'Logs' })).toBeTruthy()
    act(() => {
      window.history.replaceState(null, '', '/accounts')
      window.dispatchEvent(new PopStateEvent('popstate'))
    })
    const heading = await screen.findByRole('heading', { level: 1, name: 'Accounts' })
    await waitFor(() => expect(document.activeElement).toBe(heading))
    expect(document.title).toBe('Accounts — oonfeeWRT')
  })

  it('keeps the authenticated UI when logout fails', async () => {
    signedIn()
    mocks.api.logout.mockRejectedValue(new Error('network down'))
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Sign out' }))
    expect(await screen.findByText(/Sign out failed: network down\. You are still signed in\./)).toBeTruthy()
    expect(screen.getByText('admin')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Sign in' })).toBeNull()
  })

  it('requires an affirmative logout response before clearing local state', async () => {
    signedIn()
    mocks.api.logout.mockResolvedValue({ ok: false })
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Sign out' }))
    expect(await screen.findByText(/controller did not confirm logout/)).toBeTruthy()
    expect(screen.getByText('admin')).toBeTruthy()
  })

  it('clears protected content only after logout succeeds', async () => {
    signedIn()
    render(<App />)

    expect(await screen.findByText('dashboard data')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Sign in' })).toBeTruthy())
    expect(screen.queryByText('dashboard data')).toBeNull()
    expect(mocks.live.close).toHaveBeenCalled()
  })

  it('fetches update status independently, badges Settings in both sidebar modes, and routes the popup CTA', async () => {
    signedIn()
    mocks.api.devices.mockRejectedValue(new Error('inventory offline'))
    mocks.api.updates.mockResolvedValue(updateAvailable())
    render(<App />)

    const dialog = await screen.findByRole('dialog', { name: 'A newer oonfeeWRT version is available' })
    const settings = screen.getByRole('button', { name: 'Settings' })
    expect(settings.getAttribute('aria-describedby')).toBe('controller-update-navigation-status')
    expect(settings.querySelector('.controller-update-nav-dot')?.textContent).toBe('1')
    expect(settings.querySelector('.controller-update-nav-pill')).toBeNull()

    fireEvent.click(screen.getByRole('button', { name: 'Expand navigation' }))
    expect(settings.querySelector('.controller-update-nav-dot')).toBeNull()
    expect(settings.querySelector('.controller-update-nav-pill')?.textContent).toBe('Update')

    fireEvent.click(within(dialog).getByRole('button', { name: 'How to update' }))
    expect(await screen.findByRole('heading', { level: 1, name: 'Settings' })).toBeTruthy()
    expect(screen.getByRole('tab', { name: 'Updates' }).getAttribute('aria-selected')).toBe('true')
    expect(window.location.pathname + window.location.search).toBe('/settings?section=updates')
    expect(screen.getByText('Update available — v0.1.9')).toBeTruthy()
    expect(settings.querySelector('.controller-update-nav-pill')?.textContent).toBe('Update')
    expect(screen.queryByText(/Device inventory is unavailable/)).toBeNull()
  })

  it('does not let delayed dashboard focus steal focus from the update modal', async () => {
    signedIn()
    let resolveDashboard!: (value: { devices: object; recent_events: never[]; recent_alert_events: never[] }) => void
    mocks.api.dashboard.mockReturnValue(new Promise((resolve) => { resolveDashboard = resolve }))
    mocks.api.updates.mockResolvedValue(updateAvailable())
    render(<App />)

    const close = await screen.findByRole('button', { name: 'Close update notice' })
    close.focus()
    await act(async () => resolveDashboard({ devices: {}, recent_events: [], recent_alert_events: [] }))
    expect(await screen.findByText('dashboard data')).toBeTruthy()
    expect(document.activeElement).toBe(close)
  })

  it('uses manual checks only on request and explains unchanged timestamps', async () => {
    window.history.replaceState(null, '', '/settings?section=updates')
    signedIn()
    mocks.api.updates.mockResolvedValue(updateAvailable())
    mocks.api.checkUpdates.mockResolvedValueOnce(updateAvailable()).mockResolvedValueOnce({
      ...updateAvailable(), checked_at: '2026-09-18T12:01:00Z',
    })
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: 'Skip this version' }))
    expect(mocks.api.checkUpdates).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: 'Check now' }))
    expect(await screen.findByText('Checked a moment ago — try again in a minute.')).toBeTruthy()
    expect(mocks.api.checkUpdates).toHaveBeenCalledOnce()
    expect(mocks.api.updates).toHaveBeenCalledOnce()
    fireEvent.click(screen.getByRole('button', { name: 'Check now' }))
    await waitFor(() => expect(mocks.api.checkUpdates).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(screen.queryByText('Checked a moment ago — try again in a minute.')).toBeNull())
  })

  it('keeps the badge and Updates tab for viewers without opening a dialog', async () => {
    signedIn('viewer')
    mocks.api.updates.mockResolvedValue(updateAvailable())
    render(<App />)
    const settings = await screen.findByRole('button', { name: 'Settings' })
    await waitFor(() => expect(settings.getAttribute('aria-describedby')).toBe('controller-update-navigation-status'))
    expect(screen.queryByRole('dialog')).toBeNull()
    fireEvent.click(settings)
    fireEvent.click(screen.getByRole('tab', { name: 'Updates' }))
    expect(screen.getByText(/Ask the deployment owner/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Check now' })).toBeNull()
  })

  it('aborts and ignores a late update response after logout', async () => {
    signedIn()
    let resolveUpdates!: (value: ReturnType<typeof updateAvailable>) => void
    mocks.api.updates.mockReturnValue(new Promise((resolve) => { resolveUpdates = resolve }))
    render(<App />)
    await waitFor(() => expect(mocks.api.updates).toHaveBeenCalledOnce())
    const signal = mocks.api.updates.mock.calls[0][0] as AbortSignal

    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeTruthy()
    expect(signal.aborted).toBe(true)
    await act(async () => resolveUpdates(updateAvailable()))
    expect(screen.queryByRole('dialog', { name: 'A newer oonfeeWRT version is available' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Settings' })).toBeNull()
  })

  it('opens a deep-linked Updates tab even when inventory fails', async () => {
    window.history.replaceState(null, '', '/settings?section=updates')
    signedIn('viewer')
    mocks.api.devices.mockRejectedValue(new Error('inventory offline'))
    mocks.api.updates.mockResolvedValue({
      state: 'unknown', current_version: 'dev', update_available: false,
      message: 'Development build; no release request was made.',
    })
    render(<App />)

    expect(await screen.findByRole('heading', { level: 1, name: 'Settings' })).toBeTruthy()
    expect(screen.getByRole('tab', { name: 'Updates' }).getAttribute('aria-selected')).toBe('true')
    expect(await screen.findByText("Can't tell which version this is")).toBeTruthy()
    expect(screen.queryByText(/Device inventory is unavailable/)).toBeNull()
    await waitFor(() => expect(document.title).toBe('Updates · Settings — oonfeeWRT'))
  })

  it('names the theme control and moves focus on desktop navigation', async () => {
    signedIn()
    render(<App />)

    expect(await screen.findByRole('button', { name: /Dark theme active; switch to light theme/ })).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Skip to main content' }).getAttribute('href')).toBe('#main-content')
    fireEvent.click(screen.getByRole('button', { name: 'Topology' }))
    const heading = await screen.findByRole('heading', { name: 'Topology' })
    await waitFor(() => expect(document.activeElement).toBe(heading))
    expect(document.title).toBe('Topology — oonfeeWRT')
    expect(window.location.pathname).toBe('/topology')
  })

  it('restores the selected theme after the app remounts', async () => {
    signedIn()
    const first = render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: /switch to light theme/ }))
    expect(document.documentElement.dataset.theme).toBe('light')
    first.unmount()

    render(<App />)
    expect(await screen.findByRole('button', { name: /Light theme active; switch to dark theme/ })).toBeTruthy()
    expect(document.documentElement.dataset.theme).toBe('light')
  })

  it('keeps theme switching usable when preference storage is blocked', async () => {
    signedIn()
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked') })
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('blocked') })
    try {
      render(<App />)
      fireEvent.click(await screen.findByRole('button', { name: /switch to light theme/ }))
      expect(await screen.findByRole('button', { name: /Light theme active/ })).toBeTruthy()
      expect(document.documentElement.dataset.theme).toBe('light')
    } finally {
      get.mockRestore()
      set.mockRestore()
    }
  })

  it('mounts and focuses a deep-linked Devices heading before inventory resolves without stealing focus later', async () => {
    window.history.replaceState(null, '', '/devices')
    signedIn()
    let resolveDevices!: (value: { devices: [] }) => void
    mocks.api.devices.mockReturnValue(new Promise((resolve) => {
      resolveDevices = resolve
    }))
    render(<App />)

    const heading = await screen.findByRole('heading', { level: 1, name: 'Devices' })
    expect(screen.getByText('Managed devices (…)')).toBeTruthy()
    expect(screen.getByText('Loading devices…')).toBeTruthy()
    expect(screen.queryByText('Managed devices (0)')).toBeNull()
    await waitFor(() => expect(document.activeElement).toBe(heading))

    const adopt = heading.closest('.page-header')?.querySelector('button') as HTMLButtonElement
    expect(adopt.textContent).toBe('Adopt a device')
    adopt.focus()
    await act(async () => resolveDevices({ devices: [] }))
    expect(await screen.findByText('Managed devices (0)')).toBeTruthy()
    expect(document.activeElement).toBe(adopt)
  })

  it('keeps a deep-linked Devices heading and truthful unavailable state after a first-load failure', async () => {
    window.history.replaceState(null, '', '/devices')
    signedIn()
    mocks.api.devices.mockRejectedValue(new Error('inventory offline'))
    render(<App />)

    const heading = await screen.findByRole('heading', { level: 1, name: 'Devices' })
    expect(await screen.findByText('Managed devices (Unavailable)')).toBeTruthy()
    expect(screen.getByText(/Device inventory is unavailable\. Retry/)).toBeTruthy()
    expect(screen.queryByText('Managed devices (0)')).toBeNull()
    await waitFor(() => expect(document.activeElement).toBe(heading))
  })

  it('does not overlap global refreshes or let a stalled device request block the dashboard', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      signedIn()
      let resolveDashboard!: (value: { devices: object; recent_events: never[]; recent_alert_events: never[] }) => void
      const pendingDashboard = new Promise<{ devices: object; recent_events: never[]; recent_alert_events: never[] }>((resolve) => {
        resolveDashboard = resolve
      })
      mocks.api.dashboard
        .mockReturnValueOnce(pendingDashboard)
        .mockResolvedValue({ devices: {}, recent_events: [], recent_alert_events: [] })
      mocks.api.devices.mockReturnValue(new Promise(() => {}))
      const view = render(<App />)

      await waitFor(() => {
        expect(mocks.api.dashboard).toHaveBeenCalledTimes(1)
        expect(mocks.api.devices).toHaveBeenCalledTimes(1)
      })
      act(() => vi.advanceTimersByTime(90_000))
      expect(mocks.api.dashboard).toHaveBeenCalledTimes(1)
      expect(mocks.api.devices).toHaveBeenCalledTimes(1)

      await act(async () => {
        resolveDashboard({ devices: {}, recent_events: [], recent_alert_events: [] })
      })
      expect(await screen.findByText('dashboard data')).toBeTruthy()

      act(() => vi.advanceTimersByTime(30_000))
      await waitFor(() => expect(mocks.api.dashboard).toHaveBeenCalledTimes(2))
      expect(mocks.api.devices).toHaveBeenCalledTimes(1)

      const deviceSignal = mocks.api.devices.mock.calls[0][0] as AbortSignal
      view.unmount()
      expect(deviceSignal.aborted).toBe(true)
    } finally {
      vi.useRealTimers()
    }
  })

  it('renders accessible SVG navigation and persists its expanded state per controller account', async () => {
    signedIn()
    render(<App />)

    const navigation = await screen.findByRole('navigation', { name: 'Main navigation' })
    expect(navigation.style.width).toBe('56px')
    const routeNames = [
      'Dashboard', 'Statistics', 'Topology', 'Radios', 'Devices', 'Client Devices',
      'Policy Engine', 'Adopt a device', 'Settings', 'Accounts', 'Logs',
    ]
    for (const name of routeNames) {
      const button = screen.getByRole('button', { name })
      expect(button.querySelector('svg')?.getAttribute('width')).toBe('24')
      expect(button.getAttribute('title')).toBe(name)
      expect(button.classList.contains('app-nav-item')).toBe(true)
    }
    const divider = screen.getByRole('separator', { name: 'Controller tools' })
    expect(navigation.getAttribute('data-expanded')).toBe('false')
    expect(within(navigation).getByRole('group', { name: 'Workspace' }).contains(screen.getByRole('button', { name: 'Adopt a device' }))).toBe(true)
    const insights = within(navigation).getByRole('group', { name: 'Insights' })
    expect(within(insights).getAllByRole('button').map((button) => button.getAttribute('aria-label'))).toEqual(['Statistics', 'Reports', 'Alerts'])
    expect(within(navigation).queryByRole('button', { name: 'Firmware' })).toBeNull()
    expect(within(navigation).queryByRole('button', { name: 'Integrations' })).toBeNull()
    expect(divider.nextElementSibling).toBe(screen.getByRole('button', { name: 'Settings' }))
    expect(divider.nextElementSibling?.nextElementSibling).toBe(screen.getByRole('button', { name: 'Accounts' }))
    expect(screen.getByRole('button', { name: 'Accounts' }).nextElementSibling).toBe(screen.getByRole('button', { name: 'Logs' }))
    expect(screen.getByRole('button', { name: 'Dashboard' }).getAttribute('aria-current')).toBe('page')

    const expand = screen.getByRole('button', { name: 'Expand navigation' })
    expect(expand.getAttribute('aria-expanded')).toBe('false')
    fireEvent.click(expand)

    expect(navigation.style.width).toBe('184px')
    expect(screen.getByRole('button', { name: 'Collapse navigation' }).getAttribute('aria-expanded')).toBe('true')
    expect(navigation.getAttribute('data-expanded')).toBe('true')
    expect(within(navigation).getByText('Workspace')).toBeTruthy()
    expect(within(navigation).getByText('Insights')).toBeTruthy()
    expect(within(navigation).getByText('Dashboard')).toBeTruthy()
    expect(within(navigation).getByRole('button', { name: 'My account: admin, Owner' })).toBeTruthy()
    const key = `oonfeewrt:navigation:expanded:${encodeURIComponent(window.location.origin)}:admin`
    expect(window.localStorage.getItem(key)).toBe('true')
  })

  it.each([
    ['/firmware', 'Firmware', 'Firmware inventory'],
    ['/integrations', 'Integrations', 'Integration connections'],
    ['/settings?section=firmware', 'Firmware', 'Firmware inventory'],
    ['/settings?section=integrations', 'Integrations', 'Integration connections'],
  ])('opens %s inside Settings without restoring a standalone sidebar item', async (path, tab, heading) => {
    signedIn()
    window.history.replaceState(null, '', path)
    render(<App />)
    expect(await screen.findByRole('heading', { level: 1, name: 'Settings' })).toBeTruthy()
    expect(await screen.findByRole('heading', { name: heading })).toBeTruthy()
    expect(screen.getByRole('tab', { name: tab }).getAttribute('aria-selected')).toBe('true')
    expect(screen.getByRole('button', { name: 'Settings' }).getAttribute('aria-current')).toBe('page')
    await waitFor(() => expect(document.title).toBe(`${tab} · Settings — oonfeeWRT`))
    expect(screen.queryByRole('button', { name: tab })).toBeNull()
  })

  it('updates Settings URLs and follows history without losing the selected section', async () => {
    signedIn()
    window.history.replaceState(null, '', '/settings?section=firmware')
    render(<App />)
    fireEvent.click(await screen.findByRole('tab', { name: 'Integrations' }))
    expect(window.location.pathname + window.location.search).toBe('/settings?section=integrations')
    expect(await screen.findByRole('heading', { name: 'Integration connections' })).toBeTruthy()
    act(() => {
      window.history.replaceState(null, '', '/settings?section=firmware')
      window.dispatchEvent(new PopStateEvent('popstate'))
    })
    expect(await screen.findByRole('heading', { name: 'Firmware inventory' })).toBeTruthy()
    expect(screen.getByRole('tab', { name: 'Firmware' }).getAttribute('aria-selected')).toBe('true')
    fireEvent.click(screen.getByRole('button', { name: 'My account: admin, Owner' }))
    expect(await screen.findByRole('heading', { level: 1, name: 'Accounts' })).toBeTruthy()
    expect(window.location.pathname).toBe('/accounts')
  })

  it('restores only the signed-in account navigation preference', async () => {
    const adminKey = `oonfeewrt:navigation:expanded:${encodeURIComponent(window.location.origin)}:admin`
    window.localStorage.setItem(adminKey, 'true')
    signedIn('viewer')
    const first = render(<App />)

    expect(await screen.findByRole('button', { name: 'Expand navigation' })).toBeTruthy()
    first.unmount()

    signedIn('admin')
    render(<App />)
    expect(await screen.findByRole('button', { name: 'Collapse navigation' })).toBeTruthy()
  })

  it('keeps navigation available when one screen fails to render', async () => {
    signedIn()
    mocks.radioCrash = true
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Radios' }))
    expect(await screen.findByRole('heading', { name: 'Radios unavailable' })).toBeTruthy()
    expect(screen.getByRole('alert').textContent).toMatch(/radio fixture failed/)
    expect(screen.getByRole('button', { name: 'Dashboard' })).toBeTruthy()

    mocks.radioCrash = false
    fireEvent.click(screen.getByRole('button', { name: 'Retry screen' }))
    expect(await screen.findByRole('heading', { name: 'Radios & Channel Plan' })).toBeTruthy()
  })

  it('keeps Accounts available independently when the device inventory fails', async () => {
    signedIn()
    mocks.api.devices.mockRejectedValue(new Error('inventory offline'))
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }))
    expect((await screen.findByRole('alert')).textContent).toMatch(/Device inventory is unavailable: inventory offline/)
    expect(screen.queryByRole('tab', { name: 'My account' })).toBeNull()

    fireEvent.click(screen.getByRole('button', { name: 'Accounts' }))
    expect(await screen.findByRole('tab', { name: 'My account' })).toBeTruthy()
    expect(screen.getByRole('tab', { name: 'Manage accounts' })).toBeTruthy()
    expect(await screen.findByLabelText('Current password')).toBeTruthy()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

describe('Authentication target sizing', () => {
  it('keeps setup inputs and the primary action at least 44px tall without horizontal overflow', () => {
    render(<Auth needsSetup onSignedIn={() => {}} />)

    for (const control of [
      screen.getByLabelText('Username'),
      screen.getByLabelText('Password'),
      screen.getByLabelText('Repeat password'),
      screen.getByRole('button', { name: 'Create account' }),
    ]) {
      expect(getComputedStyle(control).minHeight).toBe('44px')
    }
    expect(getComputedStyle(screen.getByRole('button', { name: 'Create account' }).closest('form')!).maxWidth)
      .toBe('100%')
  })
})
