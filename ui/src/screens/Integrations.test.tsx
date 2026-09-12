import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Integrations } from './Integrations'
import type { Device, SessionInfo } from '../lib/api'

const mocks = vi.hoisted(() => ({ adguard: vi.fn(), saveAdguard: vi.fn(), deleteAdguard: vi.fn(), reauthenticate: vi.fn(), checkAdguard: vi.fn(), checkWireGuard: vi.fn() }))
vi.mock('../lib/api', () => ({ api: mocks, isDemo: false }))
const devices = [{ id: 7, name: 'Gateway', adopted: true }, { id: 8, name: 'Office', adopted: true }] as Device[]
const config = { configured: true, url: 'https://dns.example', username: 'service', has_password: true, tls_fingerprint: '' }

describe('Integrations', () => {
  beforeEach(() => { vi.clearAllMocks(); mocks.adguard.mockResolvedValue(config); mocks.reauthenticate.mockResolvedValue({}); mocks.saveAdguard.mockResolvedValue(config) })
  it('loads only saved configuration and does not auto-contact services or routers', async () => {
    render(<Integrations devices={devices} session={{ role: 'owner' } as SessionInfo} />)
    await screen.findByText('https://dns.example')
    expect(mocks.checkAdguard).not.toHaveBeenCalled()
    expect(mocks.checkWireGuard).not.toHaveBeenCalled()
  })
  it('hides connection and active-check controls from viewers', async () => {
    render(<Integrations devices={devices} session={{ role: 'viewer' } as SessionInfo} />)
    await screen.findByText('https://dns.example')
    expect(screen.queryByRole('button', { name: 'Edit connection' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Check AdGuard' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Check WireGuard' })).toBeNull()
  })
  it('lets an owner remove unreadable saved settings only after reauthentication', async () => {
    mocks.adguard.mockRejectedValueOnce(new Error('Saved connection cannot be decrypted'))
    mocks.deleteAdguard.mockResolvedValue({})
    render(<Integrations devices={devices} session={{ role: 'owner' } as SessionInfo} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Remove saved connection' }))
    expect(mocks.deleteAdguard).not.toHaveBeenCalled()
    expect(screen.queryByRole('button', { name: 'Connect AdGuard' })).toBeNull()
    fireEvent.change(screen.getByLabelText('Your controller password'), { target: { value: 'fixture-reauth' } })
    fireEvent.click(screen.getByRole('button', { name: 'Confirm removal' }))
    await waitFor(() => expect(mocks.deleteAdguard).toHaveBeenCalledOnce())
    expect(mocks.reauthenticate).toHaveBeenCalledWith('fixture-reauth')
    expect(mocks.saveAdguard).not.toHaveBeenCalled()
  })
  it('allows viewers to retry unreadable settings but never remove them', async () => {
    mocks.adguard.mockRejectedValueOnce(new Error('Settings unavailable'))
    render(<Integrations devices={devices} session={{ role: 'viewer' } as SessionInfo} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Retry loading settings' }))
    await screen.findByText('https://dns.example')
    expect(screen.queryByRole('button', { name: 'Remove saved connection' })).toBeNull()
    expect(mocks.deleteAdguard).not.toHaveBeenCalled()
  })
  it('requires controller reauthentication and omits unchanged saved passwords', async () => {
    render(<Integrations devices={devices} session={{ role: 'owner' } as SessionInfo} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Edit connection' }))
    fireEvent.change(screen.getByLabelText('Your controller password'), { target: { value: 'fixture-reauth' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save connection' }))
    await waitFor(() => expect(mocks.saveAdguard).toHaveBeenCalledWith({ url: 'https://dns.example', username: 'service', tls_fingerprint: '' }))
    expect(mocks.reauthenticate).toHaveBeenCalledWith('fixture-reauth')
    expect(mocks.checkAdguard).not.toHaveBeenCalled()
  })
  it('does not save after unsuccessful reauthentication', async () => {
    mocks.reauthenticate.mockRejectedValue(new Error('Password not accepted'))
    render(<Integrations devices={devices} session={{ role: 'owner' } as SessionInfo} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Edit connection' }))
    fireEvent.change(screen.getByLabelText('Your controller password'), { target: { value: 'bad-password' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save connection' }))
    await screen.findByText('Password not accepted')
    expect(mocks.saveAdguard).not.toHaveBeenCalled()
    expect((screen.getByLabelText('Your controller password') as HTMLInputElement).value).toBe('')
  })
  it('discards WireGuard observations from a previously selected device', async () => {
    let resolve!: (value: unknown) => void
    mocks.checkWireGuard.mockReturnValueOnce(new Promise((done) => { resolve = done }))
    render(<Integrations devices={devices} session={{ role: 'owner' } as SessionInfo} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Check WireGuard' }))
    fireEvent.change(screen.getByLabelText('Router'), { target: { value: '8' } })
    await act(async () => { resolve({ device_id: 7, state: 'observed', checked_at: Date.now(), interfaces: [], notes: [] }) })
    expect(screen.queryByRole('region', { name: 'WireGuard observations' })).toBeNull()
  })
  it('distinguishes observed empty data, disabled protection, and unavailable values', async () => {
    mocks.checkAdguard.mockResolvedValue({ state: 'partial', checked_at: Date.now(), source_url: config.url, running: true, protection_enabled: false, dns_queries: 0, blocked_filtering: null, avg_processing_ms: null, notes: ['Statistics partial'] })
    render(<Integrations devices={devices} session={{ role: 'owner' } as SessionInfo} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Check AdGuard' }))
    await screen.findByText('Some observations unavailable')
    expect(screen.getByText('Disabled')).toBeTruthy()
    expect(screen.getByText('0')).toBeTruthy()
    expect(screen.getAllByText('—')).toHaveLength(2)
  })
})
