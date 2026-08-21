import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from './lib/api'
import { App } from './App'

const mocks = vi.hoisted(() => ({
  api: {
    setupState: vi.fn(),
    session: vi.fn(),
    dashboard: vi.fn(),
    devices: vi.fn(),
    logout: vi.fn(),
  },
  live: { connect: vi.fn(), close: vi.fn() },
}))

vi.mock('./lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./lib/api')>()),
  api: mocks.api,
}))
vi.mock('./lib/live', () => ({ live: mocks.live }))
vi.mock('./screens/Dashboard', () => ({ Dashboard: () => <div>dashboard data</div> }))
vi.mock('./screens/Topology', () => ({ Topology: () => <h1>Topology</h1> }))

function signedIn() {
  mocks.api.setupState.mockResolvedValue({ needs_setup: false })
  mocks.api.session.mockResolvedValue({ username: 'admin', csrf: 'token' })
  mocks.api.dashboard.mockResolvedValue({ devices: {}, recent_events: [] })
  mocks.api.devices.mockResolvedValue({ devices: [] })
}

describe('App session boundaries', () => {
  beforeEach(() => {
    vi.clearAllMocks()
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
  })
})
