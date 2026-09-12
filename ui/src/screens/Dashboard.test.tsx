import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { FleetStatus } from './Dashboard'

describe('fleet status presentation', () => {
  it('visualizes the exact reported distribution with a text equivalent', () => {
    const { container } = render(<FleetStatus devices={{ total: 8, online: 4, offline: 2, pending: 1, unknown: 1 }} />)

    expect(screen.getByRole('img', { name: 'Device status: 4 online, 2 offline, 1 pending, 1 unknown' })).toBeTruthy()
    const arcs = container.querySelectorAll('.dashboard-fleet-segment')
    expect(arcs).toHaveLength(4)
    expect(arcs[0].getAttribute('stroke-dasharray')).toBe('50 50')
    expect(arcs[1].getAttribute('stroke-dashoffset')).toBe('-50')
    expect(container.querySelectorAll('dt')).toHaveLength(4)
    expect(container.querySelector('[data-state="unknown"]')).toBeTruthy()
    expect(screen.getByText(/never successfully polled/)).toBeTruthy()
    expect(container.textContent).not.toMatch(/100%|health score/i)
  })

  it('keeps an empty inventory neutral instead of reporting healthy', () => {
    const { container } = render(<FleetStatus devices={{ total: 0, online: 0, offline: 0, pending: 0, unknown: 0 }} />)

    expect(screen.getByText(/No devices in the inventory yet/)).toBeTruthy()
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('0 online, 0 offline')
    expect(container.querySelectorAll('.dashboard-fleet-segment')).toHaveLength(0)
    expect(container.textContent).not.toMatch(/healthy|100%/i)
  })

  it('does not draw a misleading complete ring when state totals disagree', () => {
    const { container } = render(<FleetStatus devices={{ total: 4, online: 2, offline: 0, pending: 0, unknown: 0 }} />)

    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('distribution unavailable')
    expect(screen.getByText(/reported state counts do not match/)).toBeTruthy()
    expect(container.querySelectorAll('.dashboard-fleet-segment')).toHaveLength(0)
    expect(container.querySelector('dd')?.textContent).toBe('2')
  })

  it.each([Number.NaN, Number.POSITIVE_INFINITY, -1, 1.5])('withholds invalid counts (%s) from the graphic', (online) => {
    const { container } = render(<FleetStatus devices={{ total: 2, online, offline: 0, pending: 0, unknown: 0 }} />)

    expect(container.querySelectorAll('.dashboard-fleet-segment')).toHaveLength(0)
    expect(container.querySelector('dd')?.textContent).toBe('—')
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain('distribution unavailable')
  })
})
