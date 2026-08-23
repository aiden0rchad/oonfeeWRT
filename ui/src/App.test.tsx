import { fireEvent, render, screen, waitFor } from '@testing-library/react'
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
vi.mock('./screens/Dashboard', () => ({ Dashboard: () => <div>dashboard data</div> }))
vi.mock('./screens/Topology', () => ({ Topology: () => <h1>Topology</h1> }))
vi.mock('./screens/Radios', () => ({ Radios: () => {
  if (mocks.radioCrash) throw new Error('radio fixture failed')
  return <h1>Radios &amp; Channel Plan</h1>
} }))

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
  mocks.api.dashboard.mockResolvedValue({ devices: {}, recent_events: [] })
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
    mocks.api.dashboard.mockResolvedValue({ devices: {}, recent_events: [] })
    mocks.api.devices.mockResolvedValue({ devices: [] })
    render(<App />)

    fireEvent.change(await screen.findByLabelText('Username'), { target: { value: 'admin' } })
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'controller password' } })
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('heading', { name: 'Topology' })).toBeTruthy()
    expect(window.location.pathname).toBe('/topology')
    expect(screen.queryByText('dashboard data')).toBeNull()
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
