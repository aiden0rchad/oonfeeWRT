import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Alerts } from './Alerts'
import type { Device, SessionInfo } from '../lib/api'
import type { AlertResponse } from '../lib/alerts'

const mocks = vi.hoisted(() => ({ alerts: vi.fn(), saveAlertRule: vi.fn(), deleteAlertRule: vi.fn(), saveAlertDelivery: vi.fn() }))
vi.mock('../lib/api', () => ({ api: mocks }))
const session = (role = 'owner') => ({ role } as SessionInfo)
const devices = [{ id: 7, name: 'Gateway' }] as Device[]
const response = (): AlertResponse => ({ rules: [], incidents: [], evaluated_at: null, delivery: { configured: false, enabled: false, host: '', last_attempt_at: null, last_success_at: null, last_error: '' } })

describe('Alerts', () => {
  beforeEach(() => { vi.clearAllMocks(); mocks.alerts.mockResolvedValue(response()); mocks.saveAlertRule.mockResolvedValue({ id: 1 }); mocks.saveAlertDelivery.mockResolvedValue(response().delivery) })
  it('shows useful empty guidance without enabling rules or contacting a webhook', async () => {
    render(<Alerts devices={devices} session={session()} />)
    await screen.findByText('No retained incidents.')
    expect(mocks.saveAlertRule).not.toHaveBeenCalled()
    expect(mocks.saveAlertDelivery).not.toHaveBeenCalled()
    expect(screen.getByText('No external notifications enabled')).toBeTruthy()
  })
  it('keeps owner controls out of the viewer workflow', async () => {
    render(<Alerts devices={devices} session={session('viewer')} />)
    await screen.findByText('No retained incidents.')
    expect(screen.queryByRole('button', { name: 'Create rule' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Save delivery settings' })).toBeNull()
  })
  it('creates disabled-by-default rules with explicit durations', async () => {
    render(<Alerts devices={devices} session={session()} />)
    await screen.findByText('No retained incidents.')
    fireEvent.click(screen.getByRole('button', { name: 'Create rule' }))
    fireEvent.change(screen.getByLabelText('Rule name'), { target: { value: 'Gateway offline' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save rule' }))
    await waitFor(() => expect(mocks.saveAlertRule).toHaveBeenCalledWith({ name: 'Gateway offline', condition: 'device_offline', device_id: 7, threshold: 0, hold_seconds: 300, cooldown_seconds: 3600, enabled: false }, undefined))
  })
  it('preserves omitted delivery secrets and requires explicit removal', async () => {
    mocks.alerts.mockResolvedValue({ ...response(), delivery: { ...response().delivery, configured: true, enabled: true, host: 'notify.example' } })
    render(<Alerts devices={devices} session={session()} />)
    await screen.findByText('No retained incidents.')
    fireEvent.click(screen.getByRole('button', { name: 'Save delivery settings' }))
    await waitFor(() => expect(mocks.saveAlertDelivery).toHaveBeenCalledWith({ enabled: true }))
    await screen.findByText('Notification delivery saved.')
    fireEvent.click(screen.getByRole('button', { name: 'Remove webhook' }))
    expect(mocks.saveAlertDelivery).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByRole('button', { name: 'Confirm removal' }))
    await waitFor(() => expect(mocks.saveAlertDelivery).toHaveBeenLastCalledWith({ enabled: false, url: '', bearer_token: '' }))
  })
  it('does not call failed loading a clean alert state', async () => {
    mocks.alerts.mockRejectedValue(new Error('storage unavailable'))
    render(<Alerts devices={devices} session={session()} />)
    await screen.findByText('Alerts could not refresh: storage unavailable')
    expect(screen.queryByText('No retained incidents.')).toBeNull()
  })
  it('retains unresolved incident counts when evidence is unavailable or evaluation stops', async () => {
    mocks.alerts.mockResolvedValue({ ...response(), counts: { open_incidents: 4 }, evaluated_at: Date.now() / 1000 - 600 })
    render(<Alerts devices={devices} session={session()} />)
    await screen.findByText('Open retained incidents')
    expect(screen.getByText('4')).toBeTruthy()
    expect(screen.getByText(/Rule evaluation is not recent/)).toBeTruthy()
  })
})
