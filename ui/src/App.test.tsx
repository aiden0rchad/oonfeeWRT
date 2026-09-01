import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from './lib/api'
import { App } from './App'
import { Auth } from './screens/Auth'

const mocks = vi.hoisted(() => ({
  api: {
    setupState: vi.fn(),
    session: vi.fn(),
    dashboard: vi.fn(),
    devices: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
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
}

describe('App session boundaries', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    window.localStorage.clear()
    window.history.replaceState(null, '', '/')
    mocks.radioCrash = false
    mocks.api.logout.mockResolvedValue({ ok: true })
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

  it('opens Network Settings from an actionable IPv6 log warning', async () => {
    signedIn()
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Logs' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Configure network IPv6' }))

    expect(await screen.findByRole('heading', { name: 'Settings' })).toBeTruthy()
    expect(window.location.pathname).toBe('/settings')
    expect(screen.getByRole('tab', { name: 'Network' }).getAttribute('aria-selected')).toBe('true')
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
    expect(navigation.style.width).toBe('64px')
    const routeNames = [
      'Dashboard', 'Topology', 'Radios', 'Devices', 'Client Devices',
      'Policy Engine', 'Settings', 'Adopt a device', 'Logs',
    ]
    for (const name of routeNames) {
      const button = screen.getByRole('button', { name })
      expect(button.querySelector('svg')?.getAttribute('width')).toBe('24')
      expect(button.getAttribute('title')).toBe(name)
      expect(button.style.minHeight).toBe('44px')
      expect(button.classList.contains('app-nav-item')).toBe(true)
    }
    const divider = screen.getByRole('separator', { name: 'Controller tools' })
    expect(divider.getAttribute('data-expanded')).toBe('false')
    expect(divider.nextElementSibling).toBe(screen.getByRole('button', { name: 'Settings' }))
    expect(screen.getByRole('button', { name: 'Dashboard' }).getAttribute('aria-current')).toBe('page')

    const expand = screen.getByRole('button', { name: 'Expand navigation' })
    expect(expand.getAttribute('aria-expanded')).toBe('false')
    fireEvent.click(expand)

    expect(navigation.style.width).toBe('208px')
    expect(screen.getByRole('button', { name: 'Collapse navigation' }).getAttribute('aria-expanded')).toBe('true')
    expect(divider.getAttribute('data-expanded')).toBe('true')
    expect(divider.textContent).toBe('Controller')
    expect(screen.getByText('Dashboard')).toBeTruthy()
    const key = `oonfeewrt:navigation:expanded:${encodeURIComponent(window.location.origin)}:admin`
    expect(window.localStorage.getItem(key)).toBe('true')
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

  it('keeps account settings available when the device inventory fails', async () => {
    signedIn()
    mocks.api.devices.mockRejectedValue(new Error('inventory offline'))
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }))
    expect(await screen.findByRole('tab', { name: 'My account' })).toBeTruthy()
    expect(screen.getByRole('tab', { name: 'Accounts' })).toBeTruthy()
    expect((await screen.findByRole('alert')).textContent).toMatch(/Device inventory is unavailable: inventory offline/)
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
