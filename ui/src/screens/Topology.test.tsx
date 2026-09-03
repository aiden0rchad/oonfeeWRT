import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import type { TopologySnapshot } from '../lib/api'

const api = {
  topology: vi.fn(),
  topologyHistory: vi.fn(),
  device: vi.fn(),
  deviceSeries: vi.fn(),
  overhead: vi.fn(),
  stats: vi.fn(),
  lldpCapability: vi.fn(),
  changeLLDPCapability: vi.fn(),
}

vi.mock('../lib/api', () => ({ api }))

const {
  Topology, detailText, edgeLaneOffsets, layoutTopology, topologyEdgeRoute, topologyEdgesAt, topologyLastKnownRoute,
  topologyNodeLabelLines,
} = await import('./Topology')

const current: TopologySnapshot = {
  at: 1_787_100_000_000,
  complete: false,
  truncated: false,
  gaps: ['device:2/lldp: unknown', 'edge:7: bridge port number could not be mapped to an interface'],
  nodes: [
    { id: 'device:aa:bb:cc:dd:ee:01', kind: 'device', name: 'Gateway', device_id: 1, online: true, synthetic: false },
    { id: 'device:aa:bb:cc:dd:ee:02', kind: 'device', name: 'Hall AP', device_id: 2, online: true, synthetic: false },
  ],
  edges: [{
    id: 7,
    child_id: 'device:aa:bb:cc:dd:ee:02',
    parent_id: 'device:aa:bb:cc:dd:ee:01',
    parent_device_id: 1,
    parent_port: 'lan2',
    medium: 'wired',
    confidence: 'ambiguous',
    valid_from: 1_787_099_900_000,
    last_seen: 1_787_100_000_000,
    evidence: [{
      kind: 'bridge_fdb', source: 'brctl.showmacs', device_id: 1,
      detail: { bridge: 'br-lan', observed_mac: 'aa:bb:cc:dd:ee:02' },
    }],
    ambiguities: ['bridge port number could not be mapped to an interface'],
  }],
}

it('renders nested evidence without JavaScript object placeholders', () => {
  const text = detailText({
    attachment: { scope: 'physical', ports: ['lan1', 'lan3'] },
    vlan: 3,
  })
  expect(text).toBe('attachment: {ports: [lan1, lan3], scope: physical} · vlan: 3')
  expect(text).not.toContain('[object Object]')
})

beforeEach(() => {
  vi.resetAllMocks()
  api.topology.mockResolvedValue(current)
  api.topologyHistory.mockResolvedValue({ ...current, complete: true, gaps: [] })
  api.device.mockResolvedValue({
    id: 2, mac: 'aa:bb:cc:dd:ee:02', name: 'Hall AP', host: '192.0.2.2', role: 'ap',
    functions: ['ap', 'switch'], adopted: true, adopted_at: 1, class: 'A', firmware: 'OpenWrt',
    last_seen: 2, poll_state: 'baseline', status: 'online', capabilities: null,
    interfaces: [], radios: [], stations: [],
  })
  api.deviceSeries.mockResolvedValue({ series: {} })
  api.overhead.mockRejectedValue(new Error('none'))
  api.stats.mockRejectedValue(new Error('none'))
  api.lldpCapability.mockResolvedValue({
    device_id: 2, name: 'Hall AP', state: 'not_installed',
    requested_packages: ['lldpd'], added_packages: [],
  })
})

describe('Topology', () => {
  it('keeps the neutral VLAN note when every active link lacks VLAN metadata', async () => {
    render(<Topology />)

    expect(await screen.findByText('VLAN evidence is unavailable; no VLAN path filter is shown.')).toBeTruthy()
    expect(screen.queryByRole('group', { name: 'VLAN filter' })).toBeNull()
  })

  it('summarizes unknown VLAN metadata once when active links are mixed', async () => {
    api.topology.mockResolvedValueOnce({
      ...current,
      edges: [current.edges[0], {
        ...current.edges[0],
        id: 8,
        evidence: [{
          ...current.edges[0].evidence[0],
          detail: { ...current.edges[0].evidence[0].detail, vlan: 20 },
        }],
      }],
    })
    render(<Topology />)

    const filter = await screen.findByRole('group', { name: 'VLAN filter' })
    expect(within(filter).getByRole('button', { name: 'Unknown' })).toBeTruthy()
    expect(within(filter).getByRole('button', { name: '20' })).toBeTruthy()
    expect(within(filter).getByRole('note').textContent)
      .toBe('1 of 2 links has no VLAN metadata. Use the Unknown filter to isolate it.')
  })

  it('preserves a valid VLAN selection while another topology query loads', async () => {
    const withVLAN = {
      ...current,
      edges: [{
        ...current.edges[0],
        evidence: [{...current.edges[0].evidence[0], detail: {vlan: 20}}],
      }],
    }
    let resolveHistory!: (value: TopologySnapshot) => void
    api.topology.mockResolvedValueOnce(withVLAN)
    api.topologyHistory.mockReturnValueOnce(new Promise<TopologySnapshot>((resolve) => {
      resolveHistory = resolve
    }))
    render(<Topology />)

    const currentFilter = await screen.findByRole('group', {name: 'VLAN filter'})
    fireEvent.click(within(currentFilter).getByRole('button', {name: '20'}))
    fireEvent.click(screen.getByRole('button', {name: 'History'}))
    await screen.findByText('Loading topology…')
    resolveHistory(withVLAN)

    const historyFilter = await screen.findByRole('group', {name: 'VLAN filter'})
    expect(within(historyFilter).getByRole('button', {name: '20'}).getAttribute('aria-pressed')).toBe('true')
  })

  it('renders partial evidence as unknown and exposes the graph in a table', async () => {
    const reviewCapabilities = vi.fn()
    render(<Topology onReviewCapabilities={reviewCapabilities} />)

    const coverageNotice = await screen.findByRole('group', { name: 'Warning: Topology coverage' })
    expect(within(coverageNotice).getByText(/Topology is partial/)).toBeTruthy()
    expect(within(coverageNotice).getByText(/2 coverage issues are recorded/)).toBeTruthy()
    const coverageToggle = within(coverageNotice).getByText('More information about coverage')
    const details = coverageToggle.closest('details')
    expect(details?.open).toBe(false)
    fireEvent.click(coverageToggle)
    expect(details?.open).toBe(true)
    expect(within(coverageNotice).getByText(/device:2\/lldp: unknown/)).toBeTruthy()
    const graph = screen.getByRole('group', { name: /2 topology nodes and 1 links/ })
    expect(graph).toBeTruthy()
    expect(within(graph).getAllByRole('button')).toHaveLength(2)
    expect(screen.queryByRole('img', { name: /topology nodes/ })).toBeNull()

    const table = screen.getByRole('table', { name: /Matching active parent-child links/ })
    expect(within(table).getByText('Hall AP')).toBeTruthy()
    expect(within(table).getByText('Gateway')).toBeTruthy()
    expect(within(table).getByText('ambiguous')).toBeTruthy()
    fireEvent.click(within(table).getByText(/1 source · 1 ambiguity/))
    expect(within(table).getByText(/bridge port number could not be mapped to an interface/)).toBeTruthy()
    expect(within(table).getByText(/bridge_fdb via brctl.showmacs/)).toBeTruthy()
    expect(screen.queryByText(/optional oonfeeWRT topology capability/i)).toBeNull()
    const lldpNotice = screen.getByRole('group', { name: 'Information: LLDP source' })
    expect(within(lldpNotice).getByText(/Wired peer and port evidence is unavailable on 1 router/i)).toBeTruthy()
    const lldpToggle = within(lldpNotice).getByText('More information about LLDP')
    const lldpDetails = lldpToggle.closest('details')
    expect(lldpDetails?.open).toBe(false)
    const reviewLLDP = within(lldpNotice).getByRole('button', { name: 'Review LLDP capability' })
    expect(reviewLLDP.closest('.notice-disclosure')).toBeNull()
    fireEvent.click(lldpToggle)
    expect(lldpDetails?.open).toBe(true)
    expect(within(lldpNotice).getByText(/never installed automatically/i)).toBeTruthy()
    fireEvent.click(reviewLLDP)
    expect(reviewCapabilities).toHaveBeenCalledTimes(1)
  })

  it('connects an unplaced device with explicitly expired last-known evidence', async () => {
    const gateway = current.nodes[0]
    const hall = current.nodes[1]
    const internet = { id: 'synthetic:internet', kind: 'synthetic' as const, name: 'Internet', synthetic: true }
    const closedAt = current.at - 5 * 60_000
    api.topology.mockResolvedValueOnce({
      ...current,
      nodes: [gateway, hall, internet],
      edges: [{
        ...current.edges[0], id: 8, child_id: gateway.id, parent_id: internet.id,
        parent_device_id: undefined, parent_port: 'wan', medium: 'uplink', confidence: 'measured',
      }],
      last_known_edges: [{ ...current.edges[0], parent_port: 'lan3', valid_to: closedAt }],
    })
    render(<Topology />)

    const graph = await screen.findByRole('group', { name: /3 topology nodes and 1 links, plus 1 last-known placement/ })
    expect(within(graph).getByText('last known · lan3')).toBeTruthy()
    expect(screen.getByText(/Dashed gray links are expired placements, not current proof/)).toBeTruthy()
    expect(screen.getByText(/Hall AP → Gateway lan3 ended/)).toBeTruthy()
    expect(screen.getByText('last known')).toBeTruthy()
  })

  it('paints every port label above every edge path', async () => {
    const branch = {
      ...current.nodes[1], id: 'device:aa:bb:cc:dd:ee:03', name: 'Second AP', device_id: 3,
    }
    api.topology.mockResolvedValueOnce({
      ...current,
      nodes: [...current.nodes, branch],
      edges: [current.edges[0], {
        ...current.edges[0], id: 8, child_id: branch.id, parent_port: 'lan3', confidence: 'measured',
      }],
    })
    render(<Topology />)

    const graph = await screen.findByRole('group', { name: /3 topology nodes and 2 links/ })
    const painted = [...graph.querySelectorAll('path, text')]
    const lastPath = painted.map((element) => element.tagName.toLowerCase()).lastIndexOf('path')
    for (const label of ['lan2', 'lan3']) {
      expect(painted.findIndex((element) => element.textContent === label)).toBeGreaterThan(lastPath)
    }
  })

  it('offers the opt-in capability path without installing from Topology', async () => {
    const reviewCapabilities = vi.fn()
    api.topology.mockResolvedValueOnce({
      ...current,
      gaps: [
        'device:1/brctl.showstp: source call failure: access/permission denied',
        'device:1/ip-4-neigh: source call failure: access/permission denied',
        'device:2/ip-6-neigh: source call failure: access/permission denied, decode/invalid data',
      ],
    })
    render(<Topology onReviewCapabilities={reviewCapabilities} />)

    const notice = await screen.findByRole('group', { name: 'Information: Bridge and neighbor sources' })
    expect(within(notice).getByText(/Topology evidence is unavailable on 2 routers/i)).toBeTruthy()
    const toggle = within(notice).getByText('More information about topology sources')
    const details = toggle.closest('details')
    expect(details?.open).toBe(false)
    const review = within(notice).getByRole('button', { name: 'Review optional capability' })
    expect(review.closest('.notice-disclosure')).toBeNull()
    fireEvent.click(toggle)
    expect(details?.open).toBe(true)
    expect(within(notice).getByText(/never runs automatically/i)).toBeTruthy()
    fireEvent.click(review)
    expect(reviewCapabilities).toHaveBeenCalledTimes(1)
    expect(api.topology).toHaveBeenCalledTimes(1)
  })

  it('does not offer an ACL change for stale or non-permission failures', async () => {
    const reviewCapabilities = vi.fn()
    api.topology.mockResolvedValueOnce({
      ...current,
      gaps: [
        'device:1/ip-4-neigh: source state is stale',
        'device:2/brctl.showstp: source call failure: unsupported operation',
        'device:3/brctl.showmacs: source call failure: decode/invalid data',
        'device:4/ip-6-neigh: source call failure: transport error',
        'device:5/lldp: source call failure: access/permission denied',
        'device:6/ip-4-neigh: a legacy permission check failed',
      ],
    })
    render(<Topology onReviewCapabilities={reviewCapabilities} />)
    await screen.findByText(/Topology is partial/)
    expect(screen.queryByText(/Would you like to add this functionality/i)).toBeNull()
  })

  it('requests a bounded history interval and draws only the selected instant', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    vi.setSystemTime(new Date('2026-08-19T12:00:00Z'))
    try {
      const transition = current.edges[0].valid_from
      api.topologyHistory.mockResolvedValueOnce({
        ...current,
        complete: true,
        gaps: [],
        edges: [
          { ...current.edges[0], id: 6, parent_port: 'lan1', valid_from: transition - 10_000, valid_to: transition },
          current.edges[0],
        ],
      })
      render(<Topology />)
      await waitFor(() => expect(api.topology).toHaveBeenCalledTimes(1))
      fireEvent.click(screen.getByRole('button', { name: 'History' }))
      await waitFor(() => expect(api.topologyHistory).toHaveBeenCalledTimes(1))

      const [from, to] = api.topologyHistory.mock.calls[0]
      expect(to - from).toBe(24 * 60 * 60 * 1000)
      expect(Math.abs(Date.now() - to)).toBeLessThan(100)
      expect(await screen.findByText(/All link intervals intersecting/)).toBeTruthy()
      const graph = screen.getByRole('group', { name: /topology nodes/ })
      expect(within(graph).getByText('lan2')).toBeTruthy()
      expect(within(graph).queryByText('lan1')).toBeNull()
      fireEvent.change(screen.getByLabelText('Selected topology history time'), { target: { value: transition - 1 } })
      expect(within(graph).getByText('lan1')).toBeTruthy()
      expect(within(graph).queryByText('lan2')).toBeNull()
      const selectedTable = screen.getByRole('table', { name: /Links active at/ })
      expect(within(selectedTable).getByText('lan1')).toBeTruthy()
      expect(within(selectedTable).queryByText('lan2')).toBeNull()
      const table = screen.getByRole('table', { name: /All link intervals intersecting/ })
      expect(within(table).getByText('lan1')).toBeTruthy()
      expect(within(table).getByText('lan2')).toBeTruthy()
    } finally {
      vi.useRealTimers()
    }
  })

  it('clears a VLAN filter when the selected history interval no longer has that VLAN', async () => {
    vi.useFakeTimers({shouldAdvanceTime: true})
    vi.setSystemTime(new Date('2026-08-19T12:00:00Z'))
    try {
      const transition = current.edges[0].valid_from
      api.topologyHistory.mockResolvedValueOnce({
        ...current,
        complete: true,
        gaps: [],
        edges: [
          {...current.edges[0], id: 6, valid_from: transition - 10_000, valid_to: transition},
          {
            ...current.edges[0],
            evidence: [{...current.edges[0].evidence[0], detail: {vlan: 20}}],
            valid_from: transition,
          },
        ],
      })
      render(<Topology />)
      await screen.findByText('VLAN evidence is unavailable; no VLAN path filter is shown.')
      fireEvent.click(screen.getByRole('button', {name: 'History'}))

      const vlanFilter = await screen.findByRole('group', {name: 'VLAN filter'})
      fireEvent.click(within(vlanFilter).getByRole('button', {name: '20'}))
      fireEvent.change(screen.getByLabelText('Selected topology history time'), {target: {value: transition - 1}})

      await screen.findByText('VLAN evidence is unavailable; no VLAN path filter is shown.')
      expect(screen.getByRole('group', {name: /2 topology nodes and 1 links/})).toBeTruthy()
    } finally {
      vi.useRealTimers()
    }
  })

  it('filters, zooms, and opens keyboard-accessible node details', async () => {
    render(<Topology />)
    const graph = await screen.findByRole('group', { name: /topology nodes/ })

    fireEvent.change(screen.getByLabelText('Filter topology by confidence'), { target: { value: 'measured' } })
    expect(screen.getByRole('group', { name: /2 topology nodes and 0 links/ })).toBeTruthy()
    expect(screen.getByRole('table', { name: /Matching active parent-child links/ }).textContent)
      .toMatch(/No links match/)
    fireEvent.change(screen.getByLabelText('Filter topology by confidence'), { target: { value: 'all' } })

    const viewport = screen.getByRole('region', { name: 'Topology graph viewport' })
    expect(viewport.style.maxHeight).toBe('520px')
    fireEvent.click(screen.getByLabelText('Zoom in topology'))
    expect(screen.getByLabelText('Reset topology zoom').textContent).toBe('125%')
    expect(graph.style.width).toBe('125%')
    expect(viewport.style.maxHeight).toBe('520px')
    viewport.scrollLeft = 80
    viewport.scrollTop = 30
    fireEvent.pointerDown(viewport, { button: 0, pointerId: 7, clientX: 200, clientY: 150 })
    expect(viewport.style.cursor).toBe('grabbing')
    fireEvent.pointerMove(viewport, { pointerId: 7, clientX: 160, clientY: 130 })
    expect(viewport.scrollLeft).toBe(120)
    expect(viewport.scrollTop).toBe(50)
    fireEvent.pointerUp(viewport, { pointerId: 7 })
    expect(viewport.style.cursor).toBe('grab')
    expect(screen.getByText('Drag background to pan')).toBeTruthy()

    const hallAP = within(graph).getByRole('button', { name: 'Open details for Hall AP' })
    hallAP.focus()
    fireEvent.keyDown(hallAP, { key: 'Enter' })
    const panel = await screen.findByRole('dialog', { name: 'Hall AP' })
    expect(panel).toBeTruthy()
    fireEvent.click(within(panel).getByRole('button', { name: 'Close' }))
    expect(screen.queryByRole('dialog', { name: 'Hall AP' })).toBeNull()
    expect(document.activeElement).toBe(hallAP)
  })

  it('never relabels a stale or late response under a new mode', async () => {
		let resolveHistory!: (value: TopologySnapshot) => void
		const history = new Promise<TopologySnapshot>((resolve) => { resolveHistory = resolve })
		api.topologyHistory.mockReturnValueOnce(history)
		render(<Topology />)
		await screen.findByText(/Matching active parent-child links/)
		fireEvent.click(screen.getByRole('button', { name: 'History' }))
		await waitFor(() => expect(api.topologyHistory).toHaveBeenCalledTimes(1))
		expect(screen.queryByRole('group', { name: /topology nodes/ })).toBeNull()
		expect(screen.getByText('Loading topology…')).toBeTruthy()
		fireEvent.click(screen.getByRole('button', { name: 'Current' }))
		await waitFor(() => expect(api.topology).toHaveBeenCalledTimes(2))
		await screen.findByText(/Matching active parent-child links/)
		resolveHistory({ ...current, complete: true, gaps: [], edges: [] })
		await Promise.resolve()
		expect(screen.getByRole('group', { name: /2 topology nodes and 1 links/ })).toBeTruthy()
		expect(screen.queryByText(/Infrastructure history/)).toBeNull()
  })

  it('aborts topology requests abandoned by mode, range, or unmount changes', async () => {
    api.topology.mockReturnValue(new Promise(() => {}))
    api.topologyHistory.mockReturnValue(new Promise(() => {}))
    const view = render(<Topology />)

    await waitFor(() => expect(api.topology).toHaveBeenCalledTimes(1))
    const currentSignal = api.topology.mock.calls[0][1] as AbortSignal
    expect(currentSignal.aborted).toBe(false)

    fireEvent.click(screen.getByRole('button', { name: 'History' }))
    await waitFor(() => expect(api.topologyHistory).toHaveBeenCalledTimes(1))
    expect(currentSignal.aborted).toBe(true)
    const firstHistorySignal = api.topologyHistory.mock.calls[0][2] as AbortSignal

    fireEvent.change(screen.getByLabelText('Topology history range'), { target: { value: '168' } })
    await waitFor(() => expect(api.topologyHistory).toHaveBeenCalledTimes(2))
    expect(firstHistorySignal.aborted).toBe(true)
    const secondHistorySignal = api.topologyHistory.mock.calls[1][2] as AbortSignal

    view.unmount()
    expect(secondHistorySignal.aborted).toBe(true)
  })

	it('hides a prior graph when the newly selected mode fails', async () => {
		api.topologyHistory.mockRejectedValueOnce(new Error('history unavailable'))
		render(<Topology />)
		await screen.findByText(/Matching active parent-child links/)
		fireEvent.click(screen.getByRole('button', { name: 'History' }))
		await screen.findByText(/history unavailable/)
		expect(screen.queryByRole('group', { name: /topology nodes/ })).toBeNull()
		expect(screen.getByText(/no graph is available for this request/i)).toBeTruthy()
	})

  it('labels a failed refresh as stale data instead of claiming the graph is absent', async () => {
    api.topology.mockResolvedValueOnce(current).mockRejectedValueOnce(new Error('refresh failed'))
    render(<Topology />)
    await screen.findByRole('group', { name: /topology nodes/ })
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent', 'refresh failed — showing the last topology that loaded successfully.',
    )
    expect(screen.getByRole('group', { name: /topology nodes/ })).toBeTruthy()
  })

  it('exposes toggle state, selected time text, and direct truncation', async () => {
    api.topologyHistory.mockResolvedValueOnce({ ...current, complete: true, truncated: true, gaps: [] })
    render(<Topology />)
    await screen.findByRole('group', { name: /topology nodes/ })
    expect(screen.getByRole('button', { name: 'Current' }).getAttribute('aria-pressed')).toBe('true')
    fireEvent.click(screen.getByRole('button', { name: 'History' }))
    await screen.findByText(/Topology history reached its retained or response limit/)
    expect(screen.getByRole('button', { name: 'History' }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByLabelText('Selected topology history time').getAttribute('aria-valuetext')).toBeTruthy()
  })

  it('reaches the full retained range and applies an accessible bounded custom range', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    vi.setSystemTime(new Date('2026-08-20T12:00:00Z'))
    try {
      render(<Topology />)
      await waitFor(() => expect(api.topology).toHaveBeenCalledTimes(1))
      fireEvent.click(screen.getByRole('button', { name: 'History' }))
      await waitFor(() => expect(api.topologyHistory).toHaveBeenCalledTimes(1))

      fireEvent.change(screen.getByLabelText('Topology history range'), { target: { value: '744' } })
      await waitFor(() => expect(api.topologyHistory).toHaveBeenCalledTimes(2))
      const [retainedFrom, retainedTo] = api.topologyHistory.mock.calls[1]
      expect(retainedTo - retainedFrom).toBe(31 * 24 * 60 * 60 * 1000)

      fireEvent.change(screen.getByLabelText('Topology history range'), { target: { value: 'custom' } })
      expect(api.topologyHistory).toHaveBeenCalledTimes(2)
      const fromInput = screen.getByLabelText('Custom topology history start') as HTMLInputElement
      const toInput = screen.getByLabelText('Custom topology history end') as HTMLInputElement
      expect(fromInput.type).toBe('datetime-local')
      expect(toInput.type).toBe('datetime-local')
      fireEvent.change(fromInput, { target: { value: '2026-08-01T08:00' } })
      fireEvent.change(toInput, { target: { value: '2026-08-20T08:00' } })
      fireEvent.click(screen.getByRole('button', { name: 'Apply custom range' }))
      await waitFor(() => expect(api.topologyHistory).toHaveBeenCalledTimes(3))
      expect(api.topologyHistory.mock.calls[2].slice(0, 2)).toEqual([
        new Date('2026-08-01T08:00').getTime(),
        new Date('2026-08-20T08:00').getTime(),
      ])

      fireEvent.change(fromInput, { target: { value: '2026-07-01T08:00' } })
      fireEvent.click(screen.getByRole('button', { name: 'Apply custom range' }))
      expect(screen.getByRole('alert').textContent).toMatch(/cannot exceed 31 days/)
      expect(api.topologyHistory).toHaveBeenCalledTimes(3)
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('layoutTopology', () => {
  it('places parents above children and retains disconnected nodes', () => {
    const detached = { id: 'mac:aa:bb:cc:dd:ee:ff', kind: 'client' as const, name: 'Unknown', mac: 'aa:bb:cc:dd:ee:ff', synthetic: false }
    const result = layoutTopology([...current.nodes, detached], current.edges)
    expect(result.positions.get('device:aa:bb:cc:dd:ee:01')!.y)
      .toBeLessThan(result.positions.get('device:aa:bb:cc:dd:ee:02')!.y)
    expect(result.positions.has(detached.id)).toBe(true)
    expect(result.unplaced).toEqual([detached.id])
    expect(result.positions.get(detached.id)!.x)
      .toBeGreaterThan(result.positions.get('device:aa:bb:cc:dd:ee:01')!.x)
  })

  it('bounds the canvas when ambiguous evidence contains a cycle', () => {
    const reversed = { ...current.edges[0], id: 8, child_id: current.edges[0].parent_id, parent_id: current.edges[0].child_id }
    const result = layoutTopology(current.nodes, [...current.edges, reversed])
    expect(result.height).toBeLessThanOrEqual(244)
  })

  it('routes simultaneous parent candidates through separate visual lanes', () => {
    const alternate = {
      ...current.edges[0], id: 8,
      parent_id: 'device:aa:bb:cc:dd:ee:02', parent_device_id: 2, parent_port: 'eth0.1',
    }
    const offsets = edgeLaneOffsets([current.edges[0], alternate])
    expect(offsets.get(current.edges[0].id)).not.toBe(offsets.get(alternate.id))
    expect((offsets.get(current.edges[0].id) ?? 0) + (offsets.get(alternate.id) ?? 0)).toBe(0)
  })

  it('uses separate orthogonal routes and label lanes', () => {
    const first = topologyEdgeRoute({ x: 500, y: 64 }, { x: 300, y: 214 }, -12)
    const second = topologyEdgeRoute({ x: 500, y: 64 }, { x: 300, y: 214 }, 12)
    expect(first.path).toMatch(/^M 500 102 V .* H 300 V 176$/)
    expect(first.label.y).not.toBe(second.label.y)
    expect(first.label.x).toBe(400)
  })

  it('places a vertical-link label on its unique lower segment', () => {
    const route = topologyEdgeRoute({ x: 500, y: 214 }, { x: 500, y: 364 })
    expect(route.path).toBe('M 500 252 V 289 H 500 V 326')
    expect(route.label).toEqual({ x: 500, y: 307.5 })
  })

  it('wraps long device names without hiding their second line', () => {
    expect(topologyNodeLabelLines('TP-Link Archer C6 v2 (US) / A6 v2 (US/TW)'))
      .toEqual(['TP-Link Archer C6 v2', '(US) / A6 v2 (US/TW)'])
  })

  it('routes last-known evidence across the unplaced divider', () => {
    const route = topologyLastKnownRoute({ x: 400, y: 214 }, { x: 960, y: 76 })
    expect(route.path).toBe('M 488 214 H 800 V 76 H 872')
    expect(route.label).toEqual({ x: 644, y: 214 })
  })
})

describe('topologyEdgesAt', () => {
  it('uses half-open validity intervals', () => {
    const first = { ...current.edges[0], valid_from: 100, valid_to: 200 }
    const second = { ...current.edges[0], id: 8, valid_from: 200 }
    expect(topologyEdgesAt([first, second], 199).map((edge) => edge.id)).toEqual([7])
    expect(topologyEdgesAt([first, second], 200).map((edge) => edge.id)).toEqual([8])
  })
})
