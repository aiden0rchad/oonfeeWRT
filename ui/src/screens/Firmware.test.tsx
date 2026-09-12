import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Firmware, officialFirmwareURL } from './Firmware'
import type { SessionInfo } from '../lib/api'

const mocks = vi.hoisted(() => ({ firmware: vi.fn(), checkFirmware: vi.fn() }))
vi.mock('../lib/api', () => ({ api: mocks, isDemo: false }))
const inventory = { devices: [{ device_id: 7, name: 'Gateway', status: 'online', management_mode: 'managed', identity: { board_name: 'vendor,board', target: 'target/device', rootfs_type: 'squashfs', release: 'OpenWrt 24.10.1' } }], checking: { note: 'Only checks when requested' }, agent: { note: 'No helper is installed automatically', package_path: 'deploy/openwrt-agent' }, installation: { enabled: false, required_checks: ['Validate the image on the device.'] } }

describe('Firmware', () => {
  beforeEach(() => { vi.clearAllMocks(); mocks.firmware.mockResolvedValue(inventory) })
  it('does not automatically contact a catalogue or offer an Install action', async () => {
    render(<Firmware session={{ role: 'owner' } as SessionInfo} />)
    await screen.findByRole('heading', { name: 'Gateway' })
    expect(mocks.checkFirmware).not.toHaveBeenCalled()
    expect(screen.queryByRole('button', { name: /install/i })).toBeNull()
  })
  it('removes earlier success when a new check fails', async () => {
    mocks.checkFirmware.mockResolvedValueOnce({ device_id: 7, result: { state: 'current', checked_at: Date.now(), message: 'Previously checked', limitations: [] } })
    render(<Firmware session={{ role: 'owner' } as SessionInfo} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Check OpenWrt catalogue' }))
    await screen.findByText('Current in this branch')
    mocks.checkFirmware.mockRejectedValueOnce(new Error('catalogue offline'))
    fireEvent.click(screen.getByRole('button', { name: 'Check OpenWrt catalogue' }))
    await screen.findByText('Check failed: catalogue offline. No current result is available.')
    expect(screen.queryByText('Current in this branch')).toBeNull()
  })
  it('does not offer catalogue requests to viewers', async () => {
    render(<Firmware session={{ role: 'viewer' } as SessionInfo} />)
    await screen.findByRole('heading', { name: 'Gateway' })
    expect(screen.queryByRole('button', { name: 'Check OpenWrt catalogue' })).toBeNull()
  })
  it('restricts artifact links to the official HTTPS origin', () => {
    expect(officialFirmwareURL('https://downloads.openwrt.org/releases/image.bin')).toBeTruthy()
    for (const value of ['javascript:alert(1)', 'http://downloads.openwrt.org/a', 'https://downloads.openwrt.org.evil/a', 'https://user:pass@downloads.openwrt.org/a']) expect(officialFirmwareURL(value)).toBeUndefined()
  })
})
