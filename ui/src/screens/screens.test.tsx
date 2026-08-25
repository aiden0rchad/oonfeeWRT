import { beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'

/**
 * Screen-level tests.
 *
 * The shared grid has its own file; these cover rules that live in the screens
 * and had no coverage at all — including one that is security-relevant, where
 * getting it wrong silently removes encryption from a wireless backhaul.
 *
 * The api module is mocked rather than a server stubbed: what is under test is
 * the screen's behaviour given a response, and every response shape here is one
 * the Go tests already pin down on the other side.
 */

const api = {
  clients: vi.fn(),
  clientObservability: vi.fn(),
  saveNetwork: vi.fn(),
  deleteNetwork: vi.fn(),
  devices: vi.fn(),
  saveWLAN: vi.fn(),
  deleteWLAN: vi.fn(),
  saveGroup: vi.fn(),
  deleteGroup: vi.fn(),
  setOverride: vi.fn(),
  noteForeign: vi.fn(),
  lastNeighbours: vi.fn(),
  events: vi.fn(),
  eventDetail: vi.fn(),
  site: vi.fn(),
  saveMesh: vi.fn(),
  deleteMesh: vi.fn(),
  preview: vi.fn(),
  applySite: vi.fn(),
  applyOperation: vi.fn(),
  device: vi.fn(),
  deviceSeries: vi.fn(),
  overhead: vi.fn(),
  reprobe: vi.fn(),
  refreshACL: vi.fn(),
  lldpCapability: vi.fn(),
  changeLLDPCapability: vi.fn(),
  distributeNeighbours: vi.fn(),
  meshHealth: vi.fn(),
  saveUplink: vi.fn(),
  deleteUplink: vi.fn(),
  unadopt: vi.fn(),
  stats: vi.fn(),
  scanPlan: vi.fn(),
  scan: vi.fn(),
  inspectDevice: vi.fn(),
  adopt: vi.fn(),
  speedTests: vi.fn(),
  startSpeedTest: vi.fn(),
  cancelSpeedTest: vi.fn(),
  topology: vi.fn(),
}

vi.mock('../lib/api', () => ({
  api,
  ApiError: class extends Error {
    status: number
    body?: unknown
    writeState?: 'none' | 'possible'

    constructor(status: number, message: string, body?: unknown) {
      super(message)
      this.status = status
      this.body = body
      if (body && typeof body === 'object' && 'write_state' in body) {
        const state = (body as { write_state?: unknown }).write_state
        if (state === 'none' || state === 'possible') this.writeState = state
      }
    }
  },
  onUnauthorized: new Set<() => void>(),
}))
// The live channel, with a way to push a frame. Devices.tsx paints its
// Broadcasting list from the pushed stats and looks provenance up in the REST
// detail, so a test that cannot push a frame cannot exercise the join between
// them at all — which is the whole subject of the provenance rendering.
const liveHandlers: ((msg: unknown) => void)[] = []
function pushLive(msg: unknown) {
  liveHandlers.forEach((h) => h(msg))
}
const live = {
  watch: vi.fn<(deviceID: number) => () => void>(() => () => {}),
  on: (h: (msg: unknown) => void) => {
    liveHandlers.push(h)
    return () => {
      const i = liveHandlers.indexOf(h)
      if (i >= 0) liveHandlers.splice(i, 1)
    }
  },
  connect: () => {},
}
vi.mock('../lib/live', () => ({
  live,
}))

// The Clients grid resolves its "Access point" column against the fleet roster.
// Defaulted here rather than in each test: a screen that throws because an
// auxiliary fetch was not stubbed fails for a reason unrelated to what the test
// is about, which is how two unrelated Clients tests broke when the column
// landed.
api.devices.mockResolvedValue({ devices: [] })
// The 802.11k card asks what the last automatic cycle did, on mount.
api.lastNeighbours.mockResolvedValue({ ran: false })
api.speedTests.mockReturnValue(new Promise(() => {}))
api.topology.mockReturnValue(new Promise(() => {}))

// Dynamic, like the screens below: the mock factory defines ApiError, and the
// panel checks `e instanceof ApiError` before trusting a body, so a test that
// constructs some other Error cannot reach that branch at all.
const { ApiError } = await import('../lib/api')

const { Clients } = await import('./Clients')
const { Settings } = await import('./Settings')
const { DeviceClass, Devices } = await import('./Devices')
// Dynamic, like the others: a static import evaluates the module before the
// api mock is registered, and the factory then reads `api` in its TDZ.
const { Logs } = await import('./Logs')
const { Discover } = await import('./Discover')
const { Adopt } = await import('./Adopt')

const emptyFacets = { presence: [], connection: [], scope: [] }

function clientPage(over: Record<string, unknown> = {}) {
  return {
    clients: [],
    total: 0,
    limit: 500,
    offset: 0,
    facets: emptyFacets,
    note: 'signal comes from the focused tier',
    scope_note: '',
    ...over,
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  live.watch.mockImplementation(() => () => {})
})

function expectSinglePageHeading(name: string) {
  const headings = screen.getAllByRole('heading', { level: 1 })
  expect(headings).toHaveLength(1)
  expect(headings[0].textContent).toBe(name)
}

describe('Discover', () => {
  it('does not offer a meaningless scan when no local addresses are eligible', async () => {
    api.scanPlan.mockResolvedValue({ networks: [], hosts: 0 })
    render(<Discover onPick={vi.fn()} />)

    const scan = (await screen.findByRole('button', {
      name: 'Scan',
    })) as HTMLButtonElement
    expect(scan.disabled).toBe(true)
    expect(screen.getByText(/found no eligible local addresses/i)).toBeTruthy()
    fireEvent.click(scan)
    expect(api.scan).not.toHaveBeenCalled()
  })

  it('never offers adoption again for an already managed discovery row', async () => {
    const onPick = vi.fn()
    api.scanPlan.mockResolvedValue({ networks: ['192.168.1.0/24'], hosts: 1 })
    api.scan.mockResolvedValue({
      found: [
        {
          host: '192.168.1.1',
          port: 80,
          scheme: 'http',
          verdict: 'openwrt',
          signals: {
            objects: 12,
            radios: 2,
            gateway: true,
            dhcp: true,
            wireless: true,
          },
          known_device_id: 4,
          known_name: 'main-router',
        },
      ],
      swept: 1,
      answered: 1,
      networks: ['192.168.1.0/24'],
      elapsed_ms: 4,
    })

    render(<Discover onPick={onPick} />)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Scan' })).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: 'Scan' }))

    expect(await screen.findByText(/already managed as/i)).toBeTruthy()
    expect(screen.getByText(/WAN interface object/i)).toBeTruthy()
    expect(screen.getByText(/DHCP service object/i)).toBeTruthy()
    expect(screen.queryByText(/dnsmasq object/i)).toBeNull()
    expect(screen.queryByText(/OpenWrt.*gateway/i)).toBeNull()
    expect(screen.queryByText(/DHCP server/i)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Adopt this' })).toBeNull()
    expect(onPick).not.toHaveBeenCalled()
  })

  it('reports an unroutable sweep without claiming the subnet is empty', async () => {
    api.scanPlan.mockResolvedValue({
      networks: ['192.168.1.0/24'],
      hosts: 254,
    })
    api.scan.mockResolvedValue({
      found: [],
      swept: 254,
      answered: 0,
      networks: ['192.168.1.0/24'],
      elapsed_ms: 3,
      failures: [
        {
          network: '192.168.1.0/24',
          reason: 'unreachable',
          attempts: 254,
        },
      ],
    })

    render(<Discover onPick={vi.fn()} />)
    await waitFor(() => expect(screen.getByText('Scan')).toBeTruthy())
    fireEvent.click(screen.getByText('Scan'))

    await waitFor(() => expect(screen.getAllByText(/could not route to any address/i)).not.toHaveLength(0))
    expect(screen.getByText(/does not establish whether devices are present/i)).toBeTruthy()
    expect(screen.queryByText(/Nothing on .* answered as an OpenWrt device/i)).toBeNull()
  })

  it('keeps a successfully completed empty sweep as an empty result', async () => {
    api.scanPlan.mockResolvedValue({
      networks: ['192.168.1.0/24'],
      hosts: 254,
    })
    api.scan.mockResolvedValue({
      found: [],
      swept: 254,
      answered: 0,
      networks: ['192.168.1.0/24'],
      elapsed_ms: 1200,
    })

    render(<Discover onPick={vi.fn()} />)
    await waitFor(() => expect(screen.getByText('Scan')).toBeTruthy())
    fireEvent.click(screen.getByText('Scan'))

    await waitFor(() => expect(screen.getByText(/Nothing on .* answered as an OpenWrt device/i)).toBeTruthy())
    expect(screen.queryByText(/could not route to any address/i)).toBeNull()
  })
})

describe('Adopt', () => {
  it('sends an optional SSH key alongside the required ubus password', async () => {
    const key = '-----BEGIN OPENSSH PRIVATE KEY-----\nui-key-sentinel\n-----END OPENSSH PRIVATE KEY-----'
    api.scanPlan.mockResolvedValue({ networks: [], hosts: 0 })
    api.adopt.mockResolvedValue({
      device_id: 7,
      mac: 'aa:bb:cc:dd:ee:07',
      name: 'bench-ap',
      model: 'OpenWrt bench',
      class: 'C',
      firmware: 'OpenWrt',
      features: [],
      unobservable: ['iwinfo-survey'],
      notes: ['channel utilization is unknown because the radio is inactive'],
    })

    render(<Adopt onAdopted={vi.fn()} />)
    expectSinglePageHeading('Adopt a device')
    await screen.findByText(/found no eligible local addresses/i)
    await screen.findByText(/Starting a new device ecosystem/i)
    const protocol = screen.getByRole('group', { name: 'Protocol' })
    expect(within(protocol).getByRole('button', { name: 'http' }).getAttribute('aria-pressed')).toBe('true')
    expect(within(protocol).getByRole('button', { name: 'https' }).getAttribute('aria-pressed')).toBe('false')
    fireEvent.change(screen.getByLabelText('Address'), {
      target: { value: '192.0.2.7' },
    })
    fireEvent.change(screen.getByLabelText('Device password (for ubus)'), {
      target: { value: 'router-password' },
    })
    fireEvent.change(screen.getByLabelText('SSH private key (optional)'), {
      target: { value: key },
    })

    const submit = screen.getByRole('button', {
      name: 'Adopt',
    }) as HTMLButtonElement
    expect(screen.getByText(/SSH key does not replace it/)).toBeTruthy()
    expect(submit.disabled).toBe(true)
    const optIn = screen.getByRole('checkbox', {
      name: /Install the oonfeeWRT controller access payload/i,
    })
    const acknowledgement = optIn.closest('label')?.textContent
    expect(acknowledgement).toMatch(/unchecked or cancelling leaves the router unchanged/i)
    expect(acknowledgement).toMatch(/keeps Adopt unavailable/i)
    const payloadNotice = screen.getByRole('group', {
      name: 'Warning: Optional controller access payload',
    })
    expect(within(payloadNotice).getByText(/adds one scoped rpcd ACL file and login/i)).toBeTruthy()
    expect(within(payloadNotice).getByText(/installs no package, binary, daemon, service, or firmware/i)).toBeTruthy()
    const capabilityDetails = within(payloadNotice)
      .getByText('What adoption installs and rolls back')
      .closest('details') as HTMLDetailsElement
    const reviewPayload = within(payloadNotice).getByRole('button', {
      name: 'Review exact router changes',
    })
    expect(capabilityDetails.open).toBe(false)
    expect(reviewPayload.closest('details')).toBeNull()
    expect(optIn.closest('details')).toBeNull()
    fireEvent.click(reviewPayload)
    expect(capabilityDetails.open).toBe(true)
    expect(reviewPayload.getAttribute('aria-pressed')).toBe('true')
    const permissionDetails = capabilityDetails.textContent
    expect(permissionDetails).toMatch(/\/usr\/share\/rpcd\/acl\.d\/oonfeewrt\.json/i)
    expect(permissionDetails).toMatch(/controller-owned network, wireless, firewall, and DHCP/i)
    expect(permissionDetails).toMatch(/runtime 802\.11k neighbour-list updates/i)
    expect(permissionDetails).toMatch(/cannot disconnect or steer clients/i)
    expect(permissionDetails).toMatch(/require Preview and Apply later/i)
    expect(permissionDetails).toMatch(/Rollback asks for the device administrator login again/i)
    fireEvent.click(optIn)
    expect(submit.disabled).toBe(false)
    fireEvent.click(submit)

    await waitFor(() => expect(api.adopt).toHaveBeenCalledTimes(1))
    expect(api.adopt.mock.calls[0][0]).toMatchObject({
      host: '192.0.2.7',
      username: 'root',
      password: 'router-password',
      private_key: key,
      functions: ['ap'],
      role: 'ap',
      acknowledge_router_changes: true,
    })
    await waitFor(() => expect(screen.getByText(/is now managed/)).toBeTruthy())
    expect(screen.getByText(/inactive interface, idle counters/)).toBeTruthy()
    expect(screen.getByText(/widen access only when.*permission denial/)).toBeTruthy()
    expect(screen.queryByText(/widening the ACL is the only thing/)).toBeNull()
    expect(screen.queryByDisplayValue(key)).toBeNull()
    expect(screen.getByText(/password and private key.*not stored/i)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Adopt another device' }))
    await screen.findByText(/found no eligible local addresses/i)
    expect((screen.getByLabelText('Address') as HTMLInputElement).value).toBe('')
    expect((screen.getByLabelText('Device username') as HTMLInputElement).value).toBe('root')
    expect(
      (
        screen.getByRole('checkbox', {
          name: /^Access point\b/,
        }) as HTMLInputElement
      ).checked,
    ).toBe(true)
    expect(
      (
        screen.getByRole('checkbox', {
          name: /Install the oonfeeWRT controller access payload/i,
        }) as HTMLInputElement
      ).checked,
    ).toBe(false)
  })

  it('inspects first, recommends measured combined functions, and preserves the legacy role', async () => {
    api.devices.mockResolvedValue({ devices: [] })
    api.scanPlan.mockResolvedValue({ networks: ['192.168.1.0/24'], hosts: 1 })
    api.scan.mockResolvedValue({
      found: [
        {
          host: '192.168.1.1',
          port: 80,
          scheme: 'http',
          verdict: 'openwrt',
          signals: {
            objects: 15,
            radios: 2,
            gateway: true,
            dhcp: true,
            wireless: true,
          },
        },
      ],
      swept: 1,
      answered: 1,
      networks: ['192.168.1.0/24'],
      elapsed_ms: 5,
    })
    api.inspectDevice.mockResolvedValue({
      mac: 'aa:bb:cc:dd:ee:01',
      model: 'Linksys WRT3200ACM',
      class: 'A',
      firmware: 'OpenWrt 25.12.5',
      radio_count: 2,
      lan_ports: ['lan1', 'lan2', 'lan3', 'lan4'],
      wan_port: 'wan',
      switch_mode: 'dsa-conditional',
      functions_supported: ['gateway', 'ap', 'switch'],
      functions_recommended: ['gateway', 'ap', 'switch'],
      gateway_evidence: {
        active_wan_default_route: true,
        lan_dhcp_enabled: true,
      },
    })
    api.adopt.mockResolvedValue({
      device_id: 1,
      mac: 'aa:bb:cc:dd:ee:01',
      name: 'WRT3200ACM',
      model: 'Linksys WRT3200ACM',
      class: 'A',
      firmware: 'OpenWrt 25.12.5',
      functions: ['gateway', 'ap', 'switch'],
      features: [],
    })

    render(<Adopt onAdopted={vi.fn()} />)
    fireEvent.click(await screen.findByRole('button', { name: 'Scan' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Adopt this' }))

    expect(await screen.findByText(/Possible gateway found/i)).toBeTruthy()
    expect((screen.getByRole('checkbox', { name: /^Gateway\b/ }) as HTMLInputElement).checked).toBe(false)
    expect(
      (
        screen.getByRole('checkbox', {
          name: /^Access point\b/,
        }) as HTMLInputElement
      ).checked,
    ).toBe(true)
    expect(screen.getByRole('group', { name: 'Device functions' })).toBeTruthy()

    fireEvent.change(screen.getByLabelText('Device password (for ubus)'), {
      target: { value: 'router-password' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Inspect capabilities' }))

    expect(await screen.findByText('Linksys WRT3200ACM')).toBeTruthy()
    expect(screen.getByText('4 observed: lan1, lan2, lan3, lan4')).toBeTruthy()
    expect(screen.getAllByText(/Gateway recommendation evidence/).length).toBe(2)
    expect(screen.getByText(/DSA detected.*existing VLAN-aware LAN bridge/)).toBeTruthy()
    for (const label of ['Gateway', 'Access point', 'Switch']) {
      expect(
        (
          screen.getByRole('checkbox', {
            name: new RegExp(`^${label}\\b`),
          }) as HTMLInputElement
        ).checked,
      ).toBe(true)
    }
    const gatewayLabel = screen.getByRole('checkbox', { name: /^Gateway\b/ }).closest('label')
    expect(gatewayLabel?.textContent).toMatch(/Gateway\s*· recommended/)
    expect(gatewayLabel?.textContent).not.toMatch(/observed/i)
    expect(api.inspectDevice).toHaveBeenCalledWith({
      host: '192.168.1.1',
      username: 'root',
      password: 'router-password',
      scheme: 'http',
    })

    fireEvent.click(
      screen.getByRole('checkbox', {
        name: /Install the oonfeeWRT controller access payload/i,
      }),
    )
    fireEvent.click(screen.getByRole('button', { name: 'Adopt' }))
    await waitFor(() => expect(api.adopt).toHaveBeenCalledTimes(1))
    expect(api.adopt.mock.calls[0][0]).toMatchObject({
      functions: ['gateway', 'ap', 'switch'],
      role: 'gateway',
      acknowledge_router_changes: true,
    })
  })

  it('renders an unreadable radio inventory as unknown, never zero', async () => {
    api.devices.mockResolvedValue({ devices: [] })
    api.scanPlan.mockResolvedValue({ networks: [], hosts: 0 })
    api.inspectDevice.mockResolvedValue({
      mac: 'aa:bb:cc:dd:ee:01',
      model: 'Router',
      class: 'A',
      firmware: 'OpenWrt',
      radio_count: null,
      lan_ports: [],
      switch_mode: 'unknown',
      functions_supported: [],
      functions_recommended: [],
      functions_unknown: ['ap'],
      gateway_evidence: {
        active_wan_default_route: null,
        lan_dhcp_enabled: null,
      },
    })
    render(<Adopt onAdopted={vi.fn()} />)
    fireEvent.change(screen.getByLabelText('Address'), {
      target: { value: '192.0.2.1' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Inspect capabilities' }))
    expect(await screen.findByText('Unknown — radio inventory was not observable')).toBeTruthy()
    expect(screen.queryByText('0')).toBeNull()
  })

  it('never applies a late inspection result to a different router', async () => {
    api.devices.mockResolvedValue({ devices: [] })
    api.scanPlan.mockResolvedValue({ networks: [], hosts: 0 })
    let resolveFirst!: (value: unknown) => void
    api.inspectDevice
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveFirst = resolve
          }),
      )
      .mockResolvedValueOnce({
        mac: 'bb:bb:bb:bb:bb:bb',
        model: 'Router B',
        class: 'C',
        firmware: 'OpenWrt',
        radio_count: 1,
        lan_ports: [],
        wan_port: '',
        switch_mode: 'observe-only',
        functions_supported: ['ap'],
        functions_recommended: ['ap'],
        gateway_evidence: {
          active_wan_default_route: false,
          lan_dhcp_enabled: false,
        },
      })

    render(<Adopt onAdopted={vi.fn()} />)
    fireEvent.change(screen.getByLabelText('Address'), {
      target: { value: '192.0.2.10' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Inspect capabilities' }))
    fireEvent.change(screen.getByLabelText('Address'), {
      target: { value: '192.0.2.11' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Inspect capabilities' }))

    expect(await screen.findByText('Router B')).toBeTruthy()
    await act(async () =>
      resolveFirst({
        mac: 'aa:aa:aa:aa:aa:aa',
        model: 'Router A',
        class: 'A',
        firmware: 'OpenWrt',
        radio_count: 2,
        lan_ports: ['lan1'],
        wan_port: 'wan',
        switch_mode: 'dsa-conditional',
        functions_supported: ['gateway'],
        functions_recommended: ['gateway'],
        gateway_evidence: {
          active_wan_default_route: true,
          lan_dhcp_enabled: true,
        },
      }),
    )

    expect(screen.queryByText('Router A')).toBeNull()
    expect(screen.getByText('Router B')).toBeTruthy()
    expect(api.inspectDevice.mock.calls.map(([request]) => request.host)).toEqual(['192.0.2.10', '192.0.2.11'])
  })

  it('labels a supported C6 gateway function as available when routing and DHCP are not observed', async () => {
    api.devices.mockResolvedValue({ devices: [{ adopted: true }] })
    api.scanPlan.mockResolvedValue({ networks: [], hosts: 0 })
    api.inspectDevice.mockResolvedValue({
      mac: 'aa:bb:cc:dd:ee:02',
      model: 'TP-Link Archer C6',
      class: 'C',
      firmware: 'OpenWrt 25.12.5',
      radio_count: 2,
      lan_ports: ['lan1', 'lan2', 'lan3', 'lan4'],
      wan_port: 'wan',
      switch_mode: 'observe-only',
      functions_supported: ['gateway', 'ap', 'switch'],
      functions_recommended: ['ap', 'switch'],
      gateway_evidence: {
        active_wan_default_route: false,
        lan_dhcp_enabled: false,
      },
    })

    render(<Adopt onAdopted={vi.fn()} />)
    fireEvent.change(screen.getByLabelText('Address'), {
      target: { value: '192.168.1.2' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Inspect capabilities' }))

    expect(await screen.findByText('TP-Link Archer C6')).toBeTruthy()
    expect(screen.getByText('Not observed')).toBeTruthy()
    expect(screen.getByText('Not enabled')).toBeTruthy()
    const gateway = screen.getByRole('checkbox', {
      name: /^Gateway\b/,
    }) as HTMLInputElement
    expect(gateway.checked).toBe(false)
    expect(gateway.closest('label')?.textContent).toMatch(/Gateway\s*· available/)
    expect(gateway.closest('label')?.textContent).not.toMatch(/observed/i)
  })

  it('keeps direct AP-only adoption available when inspection is unavailable', async () => {
    api.devices.mockResolvedValue({ devices: [] })
    api.scanPlan.mockResolvedValue({ networks: [], hosts: 0 })
    api.inspectDevice.mockRejectedValue(new ApiError(404, 'inspection is unavailable'))
    api.adopt.mockResolvedValue({
      device_id: 8,
      mac: 'aa:bb:cc:dd:ee:08',
      name: 'external-gateway-ap',
      model: 'OpenWrt AP',
      class: 'C',
      firmware: 'OpenWrt',
      features: [],
    })

    render(<Adopt onAdopted={vi.fn()} />)
    fireEvent.change(screen.getByLabelText('Address'), {
      target: { value: '192.0.2.8' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Inspect capabilities' }))

    expect(await screen.findByText(/You can still adopt directly/i)).toBeTruthy()
    const adopt = screen.getByRole('button', {
      name: 'Adopt',
    }) as HTMLButtonElement
    expect(adopt.disabled).toBe(true)
    fireEvent.click(
      screen.getByRole('checkbox', {
        name: /Install the oonfeeWRT controller access payload/i,
      }),
    )
    expect(adopt.disabled).toBe(false)
    fireEvent.click(adopt)
    await waitFor(() => expect(api.adopt).toHaveBeenCalledTimes(1))
    expect(api.adopt.mock.calls[0][0]).toMatchObject({
      functions: ['ap'],
      role: 'ap',
      acknowledge_router_changes: true,
    })
  })

  it('labels observe-only switching and unknown gateway evidence without promising control', async () => {
    api.devices.mockResolvedValue({ devices: [{ adopted: true }] })
    api.scanPlan.mockResolvedValue({ networks: [], hosts: 0 })
    api.inspectDevice.mockResolvedValue({
      mac: 'aa:bb:cc:dd:ee:02',
      model: 'Archer C6',
      class: 'C',
      firmware: 'OpenWrt',
      radio_count: 2,
      lan_ports: ['lan1', 'lan2'],
      switch_mode: 'observe-only',
      functions_supported: ['ap', 'switch'],
      functions_recommended: ['ap', 'switch'],
      gateway_evidence: {
        active_wan_default_route: null,
        lan_dhcp_enabled: false,
      },
    })

    render(<Adopt onAdopted={vi.fn()} />)
    fireEvent.change(screen.getByLabelText('Address'), {
      target: { value: '192.0.2.2' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Inspect capabilities' }))

    expect(await screen.findByText('2 observed: lan1, lan2')).toBeTruthy()
    expect(screen.getByText(/Observe only.*no per-port or managed-VLAN configuration/)).toBeTruthy()
    expect(screen.getByText(/Unknown.*inspection could not determine this/)).toBeTruthy()
    expect(screen.getByText('Not enabled')).toBeTruthy()
  })

  it('requires at least one independently selected device function', async () => {
    api.devices.mockResolvedValue({ devices: [] })
    api.scanPlan.mockResolvedValue({ networks: [], hosts: 0 })
    render(<Adopt onAdopted={vi.fn()} />)
    await screen.findByText(/Starting a new device ecosystem/i)
    fireEvent.change(screen.getByLabelText('Address'), {
      target: { value: '192.0.2.9' },
    })
    fireEvent.click(screen.getByRole('checkbox', { name: /^Access point\b/ }))

    expect(screen.getByRole('alert').textContent).toMatch(/at least one device function/i)
    expect((screen.getByRole('button', { name: 'Adopt' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('refuses form submission until the router-write opt-in is checked', async () => {
    api.devices.mockResolvedValue({ devices: [] })
    api.scanPlan.mockResolvedValue({ networks: [], hosts: 0 })
    render(<Adopt onAdopted={vi.fn()} />)
    await screen.findByText(/Starting a new device ecosystem/i)
    fireEvent.change(screen.getByLabelText('Address'), {
      target: { value: '192.0.2.10' },
    })

    const form = screen.getByRole('button', { name: 'Adopt' }).closest('form')
    expect(form).toBeTruthy()
    fireEvent.submit(form!)

    expect(api.adopt).not.toHaveBeenCalled()
    expect(screen.getByText(/confirm the required, opt-in oonfeeWRT controller capability installation/i)).toBeTruthy()
  })
})

describe('Clients', () => {
  it('does not claim zero while the first query is unresolved or unavailable', async () => {
    let reject!: (error: Error) => void
    api.clients.mockReturnValue(new Promise((_, rejectPromise) => {
      reject = rejectPromise
    }))
    render(<Clients />)

    expect(screen.getByText('Client devices (…)')).toBeTruthy()
    expect(screen.queryByText('Client devices (0)')).toBeNull()
    await act(async () => reject(new Error('client inventory offline')))
    expect(await screen.findByText('Client devices (Unavailable)')).toBeTruthy()
    expect(screen.queryByText('Client devices (0)')).toBeNull()
  })

  // Page 4 of the unfiltered list is not page 4 of the filtered one. Keeping
  // the offset lands on an empty page, which reads as "no matches" — a wrong
  // answer produced by a stale number rather than by the data.
  it('resets the offset when a filter changes', async () => {
    api.clients.mockResolvedValue(
      clientPage({
        total: 900,
        facets: {
          ...emptyFacets,
          scope: [
            { value: 'local', count: 3 },
            { value: 'upstream', count: 7 },
          ],
        },
      }),
    )
    render(<Clients />)
    expectSinglePageHeading('Client Devices')
    expect(screen.getByText(/Current wired and wireless clients/)).toBeTruthy()
    await waitFor(() => expect(api.clients).toHaveBeenCalled())

    // Turn a page, then change a filter.
    fireEvent.click(screen.getByText('Next'))
    await waitFor(() => expect(api.clients.mock.calls.at(-1)?.[0].offset).toBeGreaterThan(0))
    fireEvent.click(screen.getByText('upstream'))

    await waitFor(() => {
      const last = api.clients.mock.calls.at(-1)?.[0]
      expect(last.scope).toBe('upstream')
      expect(last.offset).toBe(0)
    })
  })

  // A dropped same-query refresh must not blank the grid: "no clients" is a
  // different claim from "the refresh failed", and only one of them is true.
  it('keeps the last good page when a refresh fails', async () => {
    vi.useFakeTimers()
    api.clients.mockResolvedValueOnce(
      clientPage({
        total: 1,
        clients: [
          {
            mac: 'aa:bb:cc:dd:ee:ff',
            name: 'laptop',
            first_seen: 1,
            last_seen: 2,
            blocked: false,
            connection: 'unknown',
            online: true,
            scope: 'local',
          },
        ],
      }),
    )
    let unmount = () => {}
    try {
      ;({ unmount } = render(<Clients />))
      await act(async () => {})
      expect(screen.getByText('laptop')).toBeTruthy()

      api.clients.mockRejectedValueOnce(new Error('network down'))
      await act(async () => {
        vi.advanceTimersByTime(30_000)
      })

      expect(screen.getByText('network down')).toBeTruthy()
      expect(screen.getByRole('alert').textContent).toContain('network down')
      expect(screen.getByText('laptop')).toBeTruthy()
    } finally {
      unmount()
      vi.useRealTimers()
    }
  })

  it('never renders an out-of-order response under newer filters', async () => {
    vi.useFakeTimers()
    const initial = clientPage({
      total: 1,
      facets: {
        ...emptyFacets,
        scope: [
          { value: 'local', count: 1 },
          { value: 'upstream', count: 1 },
        ],
      },
      clients: [
        {
          mac: 'aa:bb:cc:dd:ee:01',
          name: 'initial-local',
          first_seen: 1,
          last_seen: 2,
          blocked: false,
          connection: 'unknown',
          online: true,
          scope: 'local',
        },
      ],
    })
    let resolveRefresh!: (page: typeof initial) => void
    let resolveFiltered!: (page: typeof initial) => void
    const refresh = new Promise<typeof initial>((resolve) => {
      resolveRefresh = resolve
    })
    const filtered = new Promise<typeof initial>((resolve) => {
      resolveFiltered = resolve
    })
    api.clients.mockResolvedValueOnce(initial).mockReturnValueOnce(refresh).mockReturnValueOnce(filtered)

    let unmount = () => {}
    try {
      ;({ unmount } = render(<Clients />))
      await act(async () => {})
      expect(screen.getByText('initial-local')).toBeTruthy()

      // Leave a same-query refresh unresolved, then start a newer filtered
      // request. Rows and facets from the old query are invalid immediately.
      await act(async () => {
        vi.advanceTimersByTime(30_000)
      })
      expect(api.clients).toHaveBeenCalledTimes(2)
      fireEvent.click(screen.getByText('upstream'))
      await act(async () => {})
      expect(api.clients).toHaveBeenCalledTimes(3)
      expect(screen.queryByText('initial-local')).toBeNull()
      expect(screen.getByText('Loading clients…')).toBeTruthy()

      await act(async () => {
        resolveFiltered(
          clientPage({
            ...initial,
            clients: [
              {
                mac: 'aa:bb:cc:dd:ee:02',
                name: 'current-upstream',
                first_seen: 1,
                last_seen: 2,
                blocked: false,
                connection: 'unknown',
                online: true,
                scope: 'upstream',
              },
            ],
          }),
        )
      })
      expect(screen.getByText('current-upstream')).toBeTruthy()

      await act(async () => {
        resolveRefresh(
          clientPage({
            ...initial,
            clients: [
              {
                mac: 'aa:bb:cc:dd:ee:03',
                name: 'stale-local',
                first_seen: 1,
                last_seen: 2,
                blocked: false,
                connection: 'unknown',
                online: true,
                scope: 'local',
              },
            ],
          }),
        )
      })
      expect(screen.queryByText('stale-local')).toBeNull()
      expect(screen.getByText('current-upstream')).toBeTruthy()
    } finally {
      unmount()
      vi.useRealTimers()
    }
  })

  // On a multi-AP controller, which AP a client is on is the most useful thing
  // on the row — and it was computed by the API, typed in the client, and shown
  // nowhere. The name must be resolved against the roster, and a client no
  // managed AP currently reports must say so rather than render an empty cell
  // that reads as "on no access point".
  it('names the access point, and says why when it cannot', async () => {
    api.devices.mockResolvedValue({
      devices: [
        { id: 4, name: 'hallway-ap', adopted: true },
        { id: 9, name: 'garage-ap', adopted: true },
      ],
    })
    api.clients.mockResolvedValue(
      clientPage({
        total: 2,
        clients: [
          {
            mac: 'aa:bb:cc:dd:ee:01',
            name: 'roamer',
            first_seen: 1,
            last_seen: 2,
            blocked: false,
            connection: 'wireless',
            online: true,
            scope: 'local',
            signal: -47,
            device_id: 9,
          },
          {
            mac: 'aa:bb:cc:dd:ee:02',
            name: 'unseen',
            first_seen: 1,
            last_seen: 2,
            blocked: false,
            connection: 'unknown',
            online: true,
            scope: 'local',
          },
        ],
      }),
    )
    render(<Clients />)
    await waitFor(() => expect(screen.getByText('roamer')).toBeTruthy())

    // The attributed AP is named, not numbered.
    await waitFor(() => expect(screen.getByText('garage-ap')).toBeTruthy())
    // And the AP it is NOT on must not appear anywhere on the row.
    expect(screen.queryByText('hallway-ap')).toBeNull()

    // The uncovered client gets an explanation, not a blank. Unknown renders a
    // dash carrying its reason as a title.
    const why = document.querySelectorAll('[title*="every baseline poll"]')
    expect(why.length).toBeGreaterThan(0)
  })

  it('explains unreadable subnet data when client placement is unknown', async () => {
    api.clients.mockResolvedValue(
      clientPage({
        total: 1,
        scope_note:
          'Unknown can mean network.interface.dump could not be read. Open Devices and check What the controller cannot read here.',
        clients: [
          {
            mac: 'aa:bb:cc:dd:ee:03',
            name: 'unplaced',
            first_seen: 1,
            last_seen: 2,
            blocked: false,
            connection: 'unknown',
            online: true,
            scope: 'unknown',
          },
        ],
      }),
    )
    render(<Clients />)
    await waitFor(() => expect(screen.getByText('unplaced')).toBeTruthy())

    const rowReason = document.querySelector('[title*="network.interface.dump"]')
    if (!rowReason) throw new Error('the unknown-scope cell omits unreadable subnet data')
    expect(rowReason.getAttribute('title')).toMatch(/Open Devices/)
    expect(screen.getByRole('note').textContent).toMatch(/What the controller cannot read here/)
  })

  it('uses one joined response and one cursor for client, AP, site, events and path', async () => {
    const base = 1_787_140_800_000
    const client = {
      mac: 'aa:bb:cc:dd:ee:44',
      name: 'timeline-laptop',
      first_seen: 1,
      last_seen: 2,
      blocked: false,
      connection: 'wireless' as const,
      online: true,
      scope: 'local' as const,
      signal: -58,
      device_id: 2,
    }
    api.clients.mockResolvedValue(clientPage({ total: 1, clients: [client] }))
    api.clientObservability.mockResolvedValue({
      client_mac: client.mac,
      from: base,
      to: base + 900_000,
      resolution: '5m',
      bucket_ms: 300_000,
      timestamps: [base, base + 300_000, base + 600_000],
      ap_device_at: [2, 2, 3],
      metrics: [
        {
          id: 'client:sta_rssi',
          scope: 'client',
          kind: 'sta_rssi',
          label: 'Signal',
          unit: 'dBm',
          values: [-70, -55, -58],
          mins: [-74, -57, -61],
          maxs: [-66, -53, -56],
          counts: [4, 4, 4],
          availability: {
            state: 'available',
            source: 'rollup_5m',
            observed_points: 3,
            expected_points: 3,
            gaps: [],
          },
        },
        {
          id: 'client:sta_experience_wifi_v1',
          scope: 'client',
          kind: 'sta_experience_wifi_v1',
          label: 'WiFi experience',
          unit: 'score',
          values: [null, 91, null],
          availability: {
            state: 'partial',
            source: 'derived:5m',
            observed_points: 1,
            expected_points: 3,
            gaps: [
              { from: base, to: base + 300_000 },
              { from: base + 600_000, to: base + 900_000 },
            ],
            reason: 'wifi-v1 requires all inputs',
          },
        },
        {
          id: 'ap:2:sys_load1',
          scope: 'ap',
          kind: 'sys_load1',
          label: 'AP load',
          unit: 'load',
          device_id: 2,
          device_name: 'Hall AP',
          values: [0.2, 0.4, null],
          availability: {
            state: 'partial',
            source: 'rollup_5m',
            observed_points: 2,
            expected_points: 3,
            gaps: [{ from: base + 600_000, to: base + 900_000 }],
          },
        },
        {
          id: 'ap:2:radio_utilization_pct:radio1',
          scope: 'ap',
          kind: 'radio_utilization_pct',
          label: 'Channel utilization',
          unit: '%',
          device_id: 2,
          device_name: 'Hall AP',
          key: 'radio1',
          values: [null, null, null],
          availability: {
            state: 'unavailable',
            source: 'rollup_5m',
            observed_points: 0,
            expected_points: 3,
            gaps: [{ from: base, to: base + 900_000 }],
            reason:
              'no stored stable-radio channel-utilization rollup exists for this known radio in the requested interval',
          },
        },
        {
          id: 'site:1:site_wan_latency_ms',
          scope: 'site',
          kind: 'site_wan_latency_ms',
          label: 'ICMP latency to 1.1.1.1',
          unit: 'ms',
          device_id: 1,
          device_name: 'Gateway',
          values: [8, 9, 10],
          availability: {
            state: 'available',
            source: 'rollup_5m',
            observed_points: 3,
            expected_points: 3,
            gaps: [],
          },
        },
      ],
      events: [
        {
          id: 4392,
          ts: base + 60_000,
          category: 'client',
          severity: 'info',
          event: 'client.connect',
          detail: {},
          source: 'openwrt-log',
          source_id: '4392',
          source_boot: 'boot:1',
          ingested_at: base + 61_000,
          client_mac: client.mac,
          action: 'connect',
          in_iface: 'phy0-ap0',
        },
        {
          id: 4393,
          ts: base + 60_000,
          category: 'client',
          severity: 'info',
          event: 'client.connect',
          detail: {},
          source: 'openwrt-log',
          source_id: '4393',
          source_boot: 'boot:1',
          ingested_at: base + 62_000,
          client_mac: client.mac,
          action: 'reconnect',
          in_iface: 'lan1',
        },
      ],
      paths: [
        {
          from: base,
          to: base + 300_000,
          complete: false,
          gaps: ['no observed edge medium'],
          paths: [
            {
              node_ids: ['client:x', 'device:2', 'device:1', 'synthetic:internet'],
              labels: ['timeline-laptop', 'Hall AP', 'Gateway', 'Internet'],
              mediums: null as never,
              confidence: 'measured',
            },
          ],
        },
        {
          from: base + 300_000,
          to: base + 900_000,
          complete: true,
          gaps: [],
          paths: [
            {
              node_ids: ['client:x', 'device:3', 'device:1', 'synthetic:internet'],
              labels: ['timeline-laptop', 'Other AP', 'Gateway', 'Internet'],
              mediums: ['wireless', 'wired', 'uplink'],
              confidence: 'measured',
            },
          ],
        },
      ],
      gaps: ['historical topology source coverage is unavailable'],
      experience_formula: {
        name: 'wifi-v1',
        weights: { rssi: 0.45, retry_delta: 0.35, tx_fail_delta: 0.2 },
        missing_policy: 'null when any input is missing; weights are never renormalized',
      },
      data_contract: {
        metric_source: 'rollup_5m',
        raw_samples_persisted: false,
        event_time_resolution_ms: 1000,
        events_truncated: false,
        topology_source: 'persisted validity intervals',
      },
    })

    render(<Clients />)
    const opener = await screen.findByRole('button', {
      name: 'Open observability for timeline-laptop',
    })
    opener.focus()
    fireEvent.click(opener)
    const analysis = await screen.findByRole('region', {
      name: 'Client analysis',
    })
    const eventPane = screen.getByRole('complementary', {
      name: 'Event spine',
    })
    expect(screen.getByRole('region', { name: 'Client filters' })).toBeTruthy()
    expect(screen.getByRole('region', { name: 'Client list' })).toBeTruthy()
    expect(eventPane).toBeTruthy()
    expect(analysis).toBeTruthy()
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(api.clientObservability).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(live.watch).toHaveBeenCalledWith(3))
    expect(within(analysis).getByText('timeline-laptop → Other AP → Gateway → Internet')).toBeTruthy()
    expect(within(analysis).getByText(/wifi-v1: RSSI 45%.*weights are never renormalized/i)).toBeTruthy()
    expect(within(analysis).getByText(/Raw samples are not persisted/i)).toBeTruthy()

    const slider = screen.getByRole('slider', { name: 'Investigation time' })
    // The cursor belongs to the containing half-open bucket. At +160s the
    // nearest timestamp is the future +300s bucket, but the containing bucket
    // still starts at base and carries -70 dBm.
    fireEvent.change(slider, { target: { value: base + 160_000 } })
    expect(within(analysis).getByText('timeline-laptop → Hall AP → Gateway → Internet')).toBeTruthy()
    expect(within(analysis).getByText(/no observed link/)).toBeTruthy()
    const signal = within(analysis).getByRole('region', {
      name: 'Signal metric',
    })
    expect(within(signal).getByText('Avg -70 dBm')).toBeTruthy()
    const tooltip = within(signal).getByRole('tooltip')
    expect(tooltip.textContent).toContain(new Date(base).toLocaleString())
    expect(tooltip.textContent).toContain(new Date(base + 300_000).toLocaleString())
    expect(tooltip.textContent).toMatch(/Stored range: -74 dBm – -66 dBm/)
    expect(tooltip.textContent).toMatch(/Source samples: 4/)
    expect(signal.querySelector('[data-rollup-band="true"]')).toBeTruthy()
    fireEvent.click(within(signal).getByRole('button', { name: 'Table' }))
    const table = within(signal).getByRole('table', {
      name: 'Signal rollup table',
    })
    expect(within(table).getByRole('columnheader', { name: 'Minimum' })).toBeTruthy()
    expect(within(table).getByText('-74 dBm')).toBeTruthy()
    const unavailableRadio = within(analysis).getByRole('region', {
      name: 'Channel utilization metric',
    })
    expect(within(unavailableRadio).getByText('Hall AP · radio1')).toBeTruthy()
    expect(within(unavailableRadio).getByText(/no stored stable-radio channel-utilization rollup/)).toBeTruthy()
    expect(api.clientObservability).toHaveBeenCalledTimes(1)

    // `to` is outside every [bucket,bucket+5m) interval; it must not snap back
    // to the last bucket and present that old value as current.
    fireEvent.change(slider, { target: { value: base + 900_000 } })
    expect(within(analysis).getByText('Client AP at cursor is unavailable')).toBeTruthy()
    expect(within(analysis).getByText(/No persisted topology interval contains/)).toBeTruthy()

    // Regression: equal-second events have distinct persisted identities. The
    // shared cursor still moves to their common time, but the clicked row and
    // its detail must not collapse to the first event at that timestamp.
    const firstEvent = within(eventPane).getByRole('button', {
      name: /client\.connect.*#4392/,
    })
    const secondEvent = within(eventPane).getByRole('button', {
      name: /client\.connect.*#4393/,
    })
    fireEvent.click(secondEvent)
    expect(api.clientObservability).toHaveBeenCalledTimes(1)
    expect(secondEvent.getAttribute('aria-pressed')).toBe('true')
    expect(firstEvent.getAttribute('aria-pressed')).toBe('false')
    expect((slider as HTMLInputElement).value).toBe(String(base + 60_000))
    expect(within(eventPane).getByText('reconnect')).toBeTruthy()
    expect(within(eventPane).getByText('lan1')).toBeTruthy()
    expect(within(eventPane).getByText('yes')).toBeTruthy()

    // The live API once returned `mediums: null` for this zero-hop path.
    // Selecting the tied event must also keep the analysis pane mounted.
    expect(within(analysis).getByText(/no observed link/)).toBeTruthy()

    fireEvent.keyDown(window, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Client analysis' })).toBeNull())
    expect(document.activeElement).toBe(opener)
  })

  it('follows joined AP attribution across refresh, change and null while sliding the 24h range', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const mac = 'aa:bb:cc:dd:ee:77'
    let pageLoad = 0
    api.clients.mockImplementation(async () => {
      pageLoad++
      return clientPage({
        total: 1,
        clients: [
          {
            mac,
            name: 'moving-client',
            first_seen: 1,
            last_seen: 2,
            blocked: false,
            connection: 'wireless',
            online: true,
            scope: 'local',
            // Deliberately stale/different: the joined timeline owns the lease.
            device_id: pageLoad === 1 ? 90 : 91,
          },
        ],
      })
    })
    let joinedLoad = 0
    api.clientObservability.mockImplementation(async (_mac: string, from: number, to: number) => {
      const attribution = [[2], [3], [null]][Math.min(joinedLoad++, 2)]
      return {
        client_mac: mac,
        from,
        to,
        resolution: '5m' as const,
        bucket_ms: 300_000,
        timestamps: [to - 300_000],
        ap_device_at: attribution,
        metrics: [],
        events: [],
        paths: [],
        gaps: ['historical router-log source coverage is unavailable'],
        experience_formula: {
          name: 'wifi-v1' as const,
          weights: { rssi: 0.45, retry_delta: 0.35, tx_fail_delta: 0.2 },
          missing_policy: 'null when any input is missing',
        },
        data_contract: {
          metric_source: 'rollup_5m',
          raw_samples_persisted: false as const,
          event_time_resolution_ms: 1000,
          events_truncated: false,
          topology_source: 'persisted validity intervals',
        },
      }
    })
    const release2 = vi.fn()
    const release3 = vi.fn()
    live.watch.mockImplementation((id) => (id === 2 ? release2 : release3))

    const view = render(<Clients />)
    fireEvent.click(
      await screen.findByRole('button', {
        name: 'Open observability for moving-client',
      }),
    )
    const eventPane = await screen.findByRole('complementary', {
      name: 'Event spine',
    })
    await waitFor(() => expect(live.watch.mock.calls.map(([id]) => id)).toEqual([2]))
    expect(within(eventPane).getByText(/No sourced client event was returned/)).toBeTruthy()
    const firstRange = api.clientObservability.mock.calls[0]
    expect(firstRange[2] - firstRange[1]).toBe(24 * 60 * 60 * 1000)

    await act(async () => {
      vi.advanceTimersByTime(300_000)
      await Promise.resolve()
    })
    await waitFor(() => expect(live.watch.mock.calls.map(([id]) => id)).toEqual([2, 3]))
    expect(release2).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(api.clientObservability.mock.calls.length).toBeGreaterThan(1))
    const latestRange = api.clientObservability.mock.calls.at(-1)!
    expect(latestRange[2]).toBeGreaterThan(firstRange[2])
    expect(latestRange[2] - latestRange[1]).toBe(24 * 60 * 60 * 1000)

    await act(async () => {
      vi.advanceTimersByTime(300_000)
      await Promise.resolve()
    })
    await waitFor(() => expect(release3).toHaveBeenCalledTimes(1))
    expect(live.watch.mock.calls.map(([id]) => id)).toEqual([2, 3])

    view.unmount()
    expect(release3).toHaveBeenCalledTimes(1)
    vi.useRealTimers()
  })
})

describe('Settings — mesh editor', () => {
  const site = {
    name: 'Site',
    uuid: 'abcdef01-2345-6789-abcd-ef0123456789',
    wlans: [],
    meshes: [
      {
        id: 1,
        mesh_id: 'backhaul',
        network_id: 1,
        group_id: 1,
        band: '5g' as const,
        has_key: true,
        enabled: true,
      },
    ],
    groups: [{ id: 1, name: 'all', device_ids: [] }],
    networks: [
      {
        id: 1,
        name: 'lan',
        vlan: 1,
        cidr: '192.168.1.1/24',
        zone: 'lan',
        enabled: true,
      },
    ],
    problems: [],
    overrides: [],
    overridable: [],
    override_note: '',
  }

  beforeEach(() => {
    localStorage.clear()
    api.site.mockResolvedValue(site)
  })

  // The rule this whole design rests on: editing an encrypted mesh without
  // retyping the passphrase must NOT read as "make it open". The list omits the
  // key, so a round-trip sends an empty one — and treating that as open would
  // strip encryption from a backhaul during a rename.
  it('does not warn about an open mesh when editing an encrypted one', async () => {
    render(<Settings devices={[]} />)
    expectSinglePageHeading('Settings')
    await waitFor(() => expect(screen.getByText('backhaul')).toBeTruthy())

    fireEvent.click(screen.getAllByText('Edit')[0])
    await waitFor(() => expect(screen.getByText(/Edit backhaul/)).toBeTruthy())

    expect(screen.queryByText(/this mesh is open/i)).toBeNull()
    expect(screen.queryByText(/anyone in radio range/i)).toBeNull()
  })

  // And a NEW mesh with no passphrase really will be open, so it says so before
  // the fact rather than after.
  it('warns that a new mesh with no passphrase will be open', async () => {
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('Add a mesh')).toBeTruthy())

    fireEvent.click(screen.getByText('Add a mesh'))
    await waitFor(() => expect(screen.getByText('New mesh backhaul')).toBeTruthy())

    expect(screen.getByText(/any device in radio range/i)).toBeTruthy()
  })

  // The editor must send an empty key rather than the stored one — it never
  // holds the secret, which is why a round-trip is safe in the first place.
  it('sends no passphrase when the field is untouched', async () => {
    api.saveMesh.mockResolvedValue({ mesh: site.meshes[0], problems: [] })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('backhaul')).toBeTruthy())

    fireEvent.click(screen.getAllByText('Edit')[0])
    await waitFor(() => expect(screen.getByText(/Edit backhaul/)).toBeTruthy())
    fireEvent.click(screen.getByText('Save'))

    await waitFor(() => expect(api.saveMesh).toHaveBeenCalled())
    expect(api.saveMesh.mock.calls[0][0].key).toBe('')
  })

  it('requires an explicit control to make an encrypted mesh open', async () => {
    api.saveMesh.mockResolvedValue({
      mesh: { ...site.meshes[0], has_key: false },
      problems: [],
    })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('backhaul')).toBeTruthy())

    fireEvent.click(screen.getAllByText('Edit')[0])
    const clear = await screen.findByLabelText('Remove the passphrase and make this mesh open')
    fireEvent.click(clear)
    expect(screen.getByText(/any device in radio range/i)).toBeTruthy()
    fireEvent.click(screen.getByText('Save'))

    await waitFor(() => expect(api.saveMesh).toHaveBeenCalled())
    expect(api.saveMesh.mock.calls[0][0]).toMatchObject({
      clear_key: true,
      key: '',
    })
  })

  // An open mesh has to be visible as open in the LIST, not only in the editor.
  // The list is where someone scans for what is wrong.
  it('marks an open mesh in the list', async () => {
    api.site.mockResolvedValue({
      ...site,
      meshes: [{ ...site.meshes[0], has_key: false }],
    })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText(/anyone in range can join/i)).toBeTruthy())
  })

  it('requires a non-empty mesh ID before saving', async () => {
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Add a mesh'))

    const editor = screen.getByText('New mesh backhaul').closest('section') as HTMLElement
    const save = within(editor).getByRole('button', {
      name: 'Save',
    }) as HTMLButtonElement
    expect(save.disabled).toBe(true)

    fireEvent.change(within(editor).getByLabelText('Mesh ID'), {
      target: { value: '   ' },
    })
    expect(save.disabled).toBe(true)
    fireEvent.change(within(editor).getByLabelText('Mesh ID'), {
      target: { value: 'backhaul-2' },
    })
    expect(save.disabled).toBe(false)
  })

  it('requires a named confirmation and surfaces a mesh deletion failure', async () => {
    let rejectDelete!: (error: Error) => void
    api.deleteMesh.mockReturnValue(
      new Promise((_, reject) => {
        rejectDelete = reject
      }),
    )
    render(<Settings devices={[]} />)

    const card = (await screen.findByText('Mesh backhauls')).closest('section') as HTMLElement
    fireEvent.click(within(card).getByRole('button', { name: 'Delete mesh backhaul' }))
    expect(api.deleteMesh).not.toHaveBeenCalled()

    const confirm = within(card).getByRole('button', {
      name: 'Delete “backhaul”',
    })
    fireEvent.click(confirm)
    await waitFor(() => expect(api.deleteMesh).toHaveBeenCalledWith(1))
    expect(
      (
        within(card).getByRole('button', {
          name: 'Deleting…',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true)

    await act(async () => rejectDelete(new Error('mesh deletion failed')))
    expect(await within(card).findByText('mesh deletion failed')).toBeTruthy()
    expect(
      (
        within(card).getByRole('button', {
          name: 'Delete “backhaul”',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false)
  })
})

describe('Settings — desired-state controls', () => {
  const site = {
    name: 'Site',
    uuid: 'abcdef01-2345-6789-abcd-ef0123456789',
    wlans: [
      {
        id: 1,
        ssid: 'fixture-roam',
        network_id: 1,
        group_id: 1,
        bands: ['5g'],
        security_mode: 'psk2',
        pmf: '1',
        has_key: true,
        enabled: true,
        roaming: { ft: true, ft_over_ds: true, kv: true, ft_with_psk2: true },
        hidden: false,
        isolate: false,
        max_assoc: 0,
        allow_uplink: false,
      },
    ],
    meshes: [],
    uplinks: [],
    groups: [{ id: 1, name: 'all', device_ids: [] }],
    networks: [
      {
        id: 1,
        name: 'lan',
        vlan: 1,
        cidr: '192.168.1.1/24',
        zone: 'lan',
        enabled: true,
      },
    ],
    zones: [],
    problems: [],
    overrides: [],
    overridable: [],
    override_note: '',
  }

  beforeEach(() => {
    localStorage.clear()
    api.site.mockResolvedValue(site)
  })

  it('summarizes configuration blockers and keeps their exact reasons in an alert disclosure', async () => {
    api.site.mockResolvedValue({
      ...site,
      problems: ['A managed network needs a firewall zone.', 'An AP group has no devices.'],
    })
    render(<Settings devices={[]} />)

    const alert = await screen.findByRole('alert')
    const notice = within(alert).getByRole('group', { name: 'Warning: Configuration readiness' })
    expect(within(notice).getByText('2 configuration problems block Apply.')).toBeTruthy()
    const toggle = within(notice).getByText('More information about configuration problems')
    const details = toggle.closest('details') as HTMLDetailsElement
    expect(details.open).toBe(false)
    expect(within(notice).getByText('A managed network needs a firewall zone.')).toBeTruthy()
    fireEvent.click(toggle)
    expect(details.open).toBe(true)
  })

  it('requires a named confirmation and surfaces a WLAN deletion failure', async () => {
    let rejectDelete!: (error: Error) => void
    api.deleteWLAN.mockReturnValue(
      new Promise((_, reject) => {
        rejectDelete = reject
      }),
    )
    render(<Settings devices={[]} />)

    const card = (await screen.findByText('Add a WLAN')).closest('section') as HTMLElement
    fireEvent.click(
      within(card).getByRole('button', {
        name: 'Delete wireless network fixture-roam',
      }),
    )
    expect(api.deleteWLAN).not.toHaveBeenCalled()

    fireEvent.click(within(card).getByRole('button', { name: 'Delete “fixture-roam”' }))
    await waitFor(() => expect(api.deleteWLAN).toHaveBeenCalledWith(1))
    expect(
      (
        within(card).getByRole('button', {
          name: 'Deleting…',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true)
    await act(async () => rejectDelete(new Error('WLAN deletion failed')))
    expect(await within(card).findByText('WLAN deletion failed')).toBeTruthy()
    expect(
      (
        within(card).getByRole('button', {
          name: 'Delete “fixture-roam”',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false)
  })

  it('serializes rapid membership changes from the latest desired membership', async () => {
    let finishFirst!: () => void
    api.saveGroup
      .mockReturnValueOnce(
        new Promise((resolve) => {
          finishFirst = () => resolve({})
        }),
      )
      .mockResolvedValueOnce({})
    const devices = [
      { id: 1, name: 'AP one', adopted: true },
      { id: 2, name: 'AP two', adopted: true },
    ] as never
    render(<Settings devices={devices} />)

    fireEvent.click(await screen.findByLabelText('AP one'))
    fireEvent.click(screen.getByLabelText('AP two'))
    await waitFor(() => expect(api.saveGroup).toHaveBeenCalledTimes(1))
    expect(api.saveGroup.mock.calls[0][0]).toEqual({
      id: 1,
      name: 'all',
      device_ids: [1],
    })

    await act(async () => finishFirst())
    await waitFor(() => expect(api.saveGroup).toHaveBeenCalledTimes(2))
    expect(api.saveGroup.mock.calls[1][0]).toEqual({
      id: 1,
      name: 'all',
      device_ids: [1, 2],
    })
  })

  it('keeps inline network controls from opening the network editor', async () => {
    api.deleteNetwork.mockReturnValue(new Promise(() => {}))
    render(<Settings devices={[]} />)

    const zone = await screen.findByLabelText('Firewall zone for lan')
    fireEvent.click(zone)
    expect(screen.queryByRole('dialog')).toBeNull()

    const networkCard = zone.closest('section') as HTMLElement
    fireEvent.click(within(networkCard).getByRole('button', { name: 'Delete network lan' }))
    expect(api.deleteNetwork).toHaveBeenCalledWith(1)
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('labels selected WLAN controls and the AP-group name input', async () => {
    render(<Settings devices={[]} />)
    expect(await screen.findByLabelText('New AP group name')).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Edit wireless network fixture-roam' }))
    const selected = [
      ['Bands', '5 GHz'],
      ['Security', 'WPA2 only'],
      ['Protected management frames', 'Optional'],
      ['Network', 'lan (VLAN 1)'],
      ['AP group', 'all (0 devices)'],
    ]
    for (const [groupName, buttonName] of selected) {
      const group = screen.getByRole('group', { name: groupName })
      expect(within(group).getByRole('button', { name: buttonName }).getAttribute('aria-pressed')).toBe('true')
    }
  })

  it('keeps an override toggle at the requested value while its refresh is pending', async () => {
    const overridden = {
      ...site,
      overrides: [
        {
          device_id: 1,
          wlan_id: 1,
          key: 'disabled',
          value: '1',
          describe: 'fixture-roam is not published here',
        },
      ],
    }
    const cleared = { ...site, overrides: [] }
    api.site.mockResolvedValueOnce(overridden).mockResolvedValueOnce(cleared)
    let finish!: () => void
    api.setOverride.mockReturnValueOnce(
      new Promise((resolve) => {
        finish = () => resolve({})
      }),
    )
    render(<Settings devices={[{ id: 1, name: 'AP one', adopted: true }] as never} />)

    const toggle = (await screen.findByLabelText('Do not publish here')) as HTMLInputElement
    expect(toggle.checked).toBe(true)
    fireEvent.click(toggle)
    expect(toggle.checked).toBe(false)
    expect(toggle.disabled).toBe(true)
    expect(api.setOverride).toHaveBeenCalledWith(1, 1, 'disabled', '')

    await act(async () => finish())
    await waitFor(() => {
      expect(toggle.checked).toBe(false)
      expect(toggle.disabled).toBe(false)
    })
  })
})

describe('Devices — re-probe panel', () => {
  const detail = {
    id: 1,
    mac: '02:00:00:00:00:01',
    name: 'ap-1',
    host: '192.168.1.1',
    role: 'ap',
    functions: ['ap', 'switch'],
    adopted: true,
    adopted_at: 1,
    class: 'A',
    firmware: 'OpenWrt 24.10',
    last_seen: 2,
    poll_state: 'baseline',
    status: 'online' as const,
    capabilities: null,
    interfaces: ['wan'],
    radios: [],
    stations: [],
  }

  beforeEach(() => {
    api.device.mockResolvedValue(detail)
    api.deviceSeries.mockResolvedValue({ series: {} })
    api.overhead.mockRejectedValue(new Error('none'))
    api.lldpCapability.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      state: 'not_installed',
      requested_packages: ['lldpd'],
      added_packages: [],
    })
    // Every chart on this panel renders its empty state, which is what the
    // focused-tier test below is about.
    api.stats.mockRejectedValue(new Error('none'))
  })

  it('shows assigned functions in both the fleet row and device detail', async () => {
    await openPanel()
    expectSinglePageHeading('Devices')
    expect(screen.getByText(/Managed OpenWrt inventory/)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Adopt a device' })
      .closest('.page-header-actions')).toBeTruthy()
    expect(screen.getAllByText('AP · Switch').length).toBeGreaterThanOrEqual(2)
    const panel = screen.getByRole('dialog', { name: 'ap-1' })
    expect(within(panel).getByText('AP · Switch')).toBeTruthy()
    fireEvent.click(within(panel).getByRole('button', { name: 'Rename ap-1' }))
    expect(within(panel).getByLabelText('New name for ap-1')).toBeTruthy()
  })

  it('renders an omitted live station signal as unavailable, not zero dBm', async () => {
    await openPanel()
    act(() =>
      pushLive({
        type: 'stats',
        device_id: 1,
        ts: Math.floor(Date.now() / 1000),
        tier: 'focused',
        uptime: 100,
        load1: 0.1,
        poll_ms: 5,
        clients: 1,
        degraded: 0,
        aps: [],
        stations: [
          {
            mac: '00:11:22:33:44:55',
            iface: 'phy0-ap0',
            signal: null,
            rx_kbit: null,
            tx_kbit: null,
            connected_seconds: 10,
          },
        ],
      }),
    )
    expect(screen.getByText('Associated now')).toBeTruthy()
    expect(screen.getByTitle('this station did not report signal')).toBeTruthy()
    expect(screen.queryByText('0 dBm')).toBeNull()
  })

  it('lets a fresh live poll supersede a stale offline detail response', async () => {
    api.device.mockResolvedValue({ ...detail, status: 'offline' })
    await openPanel()
    const panel = screen.getByRole('dialog', { name: 'ap-1' })
    expect(within(panel).getByText('offline')).toBeTruthy()

    act(() =>
      pushLive({
        type: 'stats',
        device_id: 1,
        ts: Math.floor(Date.now() / 1000),
        tier: 'focused',
        uptime: 100,
        load1: 0.1,
        poll_ms: 5,
        clients: 0,
        degraded: 0,
        aps: [],
        stations: [],
      }),
    )
    expect(within(panel).getByText('online')).toBeTruthy()
    expect(within(panel).queryByText('offline')).toBeNull()
  })

  it('keeps core device facts visible when the optional series catalog fails', async () => {
    api.deviceSeries.mockRejectedValue(new Error('series unavailable'))
    await openPanel()

    const panel = screen.getByRole('dialog', { name: 'ap-1' })
    expect(within(panel).getByText('192.168.1.1')).toBeTruthy()
    expect(within(panel).getByText(/Metric catalog refresh failed \(series unavailable\)/)).toBeTruthy()
  })

  it('does not mislabel explicit non-poll actions as unexpected logins', async () => {
    api.overhead.mockResolvedValue({
      overhead: {
        device_id: 1,
        tier: 'focused',
        interval_seconds: 10,
        requests: 145,
        polls_per_minute: 5.9,
        bytes_out: 1024,
        polls: 100,
        failed_polls: 0,
        since: 1,
        requests_per_minute: 6.2,
        non_poll_requests: 45,
        quiesced: false,
        cpu_ms_per_poll: 5,
        cpu_percent_of_core: 0.01,
        cpu_basis: 'measured',
      },
      packages: [],
      packages_note: 'none installed',
      poll_interval_s: 0,
      poll_interval_note: 'controller default',
    })
    await openPanel()

    expect(screen.getByText('Scheduled poll rate')).toBeTruthy()
    expect(screen.getByText('HTTP request rate')).toBeTruthy()
    expect(screen.getByText(/includes session setup and explicit actions/)).toBeTruthy()
    expect(screen.queryByText(/should only be session logins/)).toBeNull()
  })

  it('expires an old live frame instead of leaving departed stations associated now', async () => {
    await openPanel()
    act(() =>
      pushLive({
        type: 'stats',
        device_id: 1,
        ts: Math.floor(Date.now() / 1000) - 31,
        tier: 'focused',
        uptime: 100,
        load1: 0.1,
        poll_ms: 5,
        clients: 1,
        degraded: 0,
        aps: [],
        stations: [
          {
            mac: '00:11:22:33:44:55',
            iface: 'phy0-ap0',
            signal: -42,
            rx_kbit: 1,
            tx_kbit: 1,
            connected_seconds: 10,
          },
        ],
      }),
    )

    await waitFor(() => expect(screen.queryByText('Associated now')).toBeNull())
    expect(screen.queryByText('just now (live)')).toBeNull()
  })

  it('does not widen an explicit empty function record through its legacy role', async () => {
    api.device.mockResolvedValue({ ...detail, functions: [] })
    render(
      <Devices
        devices={[{ ...detail, functions: [], quiesced: false } as never]}
        onAdopt={() => {}}
        onChanged={() => {}}
      />,
    )
    fireEvent.click(screen.getByText('ap-1'))
    const panel = await screen.findByRole('dialog', { name: 'ap-1' })

    expect(screen.getAllByText('None — invalid record').length).toBeGreaterThanOrEqual(2)
    expect(within(panel).queryByText('AP · Switch')).toBeNull()
  })

  // Channel utilization is the one chart here whose series is NOT written on
  // every poll: it comes from iwinfo.survey, which is focused-tier only, so it
  // is recorded while this panel is open and not otherwise.
  //
  // The shared empty message says "telemetry is written every five minutes",
  // which told the operator to wait — and waiting means closing the panel,
  // which is exactly what stops the collection. Measured on the reference
  // device: 25 rollup buckets against 236 for a baseline series, newest an hour
  // old with the panel shut, while the card above showed a live busy percentage
  // read from the same radio seconds earlier.
  it('says why the survey chart is empty, not the generic reason', async () => {
    api.deviceSeries.mockResolvedValue({
      series: { chan_busy_pct: ['phy0-ap0'], ap_airtime_pct: ['phy0-ap0'] },
    })
    await openPanel()

    await waitFor(() => expect(screen.getByText(/recorded only while this panel is open/)).toBeTruthy())
    // And it must not repeat the advice that cannot work.
    const note = screen.getByText(/recorded only while this panel is open/)
    expect(note.textContent).toMatch(/waiting with it closed will not fill this in/i)
  })

  // BSS load runs on every poll and was charted nowhere — 189 rollup buckets
  // unused, while the card above showed a live percentage from the same field.
  // It is also what hostapd advertises to clients, so it is the figure they act
  // on when deciding whether to roam.
  it('charts the utilization series that is recorded on every poll', async () => {
    api.deviceSeries.mockResolvedValue({
      series: { chan_busy_pct: ['phy0-ap0'], ap_airtime_pct: ['phy0-ap0'] },
    })
    await openPanel()

    await waitFor(() => expect(screen.getByText(/as hostapd advertises it to clients/)).toBeTruthy())
    // Both are drawn, and each says what it measures: they agree to within 1.6
    // points on average but diverge by up to 16 in a single bucket, so neither
    // substitutes for the other and a reader must be able to tell them apart.
    expect(screen.getByText(/Channel utilization — phy0/)).toBeTruthy()
    expect(screen.getByText(/Channel occupancy \(survey\) — phy0/)).toBeTruthy()
  })

  // Both quantities belong to the RADIO, and both sources report them per
  // interface, so a radio carrying two SSIDs produced two identical series and
  // the panel drew each chart twice — four on the Archer C6, two of them
  // duplicates to the decimal.
  it('draws one chart per radio, not one per BSS', async () => {
    api.deviceSeries.mockResolvedValue({
      series: {
        ap_airtime_pct: ['phy0-ap0', 'phy0-ap1', 'phy1-ap0', 'phy1-ap1'],
        chan_busy_pct: ['phy0-ap0', 'phy0-ap1', 'phy1-ap0', 'phy1-ap1'],
      },
    })
    await openPanel()

    await waitFor(() => expect(screen.getAllByText(/Channel utilization — phy/).length).toBe(2))
    expect(screen.getAllByText(/Channel occupancy \(survey\) — phy/).length).toBe(2)
    // Named by radio, so nothing implies the reading belongs to one SSID.
    expect(screen.getByText(/Channel utilization — phy0$/)).toBeTruthy()
    expect(screen.getByText(/Channel utilization — phy1$/)).toBeTruthy()
  })

  async function openPanel() {
    const { Devices } = await import('./Devices')
    render(<Devices devices={[{ ...detail, quiesced: false } as never]} onAdopt={() => {}} onChanged={() => {}} />)
    fireEvent.click(screen.getByText('ap-1'))
    await waitFor(() => expect(screen.getByText('Re-probe capabilities')).toBeTruthy())
  }

  it('refreshes the scoped ACL with an ephemeral administrator credential', async () => {
    api.refreshACL.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      acl_updated: true,
      controller_verified: true,
      features: ['radio-freqlist', 'router-log'],
      unobservable: [],
    })
    await openPanel()
    expect(screen.getByText('/usr/share/rpcd/acl.d/oonfeewrt.json')).toBeTruthy()
    expect(
      screen.getByText(/default-off action installs the payload if missing or replaces its one rpcd ACL JSON file/i),
    ).toBeTruthy()
    expect(screen.getByText(/controller-owned network, wireless, firewall and DHCP/i)).toBeTruthy()
    expect(screen.getByText(/cannot disconnect or steer clients/i)).toBeTruthy()
    expect(screen.getByText(/installs no package, binary, daemon, service, or firmware/i)).toBeTruthy()
    expect(screen.getByText(/Leave it off or cancel to keep the router unchanged/i)).toBeTruthy()
    expect(screen.getByText(/remain explicit gaps/i)).toBeTruthy()
    fireEvent.click(screen.getByText('Review or refresh controller access payload'))
    fireEvent.change(screen.getByLabelText('Device administrator password'), {
      target: { value: 'cancelled-password' },
    })
    fireEvent.click(screen.getByText('Cancel payload review'))
    expect(api.refreshACL).not.toHaveBeenCalled()
    fireEvent.click(screen.getByText('Review or refresh controller access payload'))
    expect((screen.getByLabelText('Device administrator password') as HTMLInputElement).value).toBe('')
    fireEvent.change(screen.getByLabelText('Device administrator username'), {
      target: { value: 'admin' },
    })
    fireEvent.change(screen.getByLabelText('Device administrator password'), {
      target: { value: 'sentinel-password' },
    })
    fireEvent.change(screen.getByLabelText('SSH private key (optional)'), {
      target: { value: 'sentinel-key' },
    })
    const submit = screen.getByRole('button', {
      name: 'Install or refresh controller access payload and verify',
    }) as HTMLButtonElement
    const acknowledgement = screen.getByRole('checkbox', {
      name: /accepting installs the payload if missing or replaces the controller's single rpcd ACL JSON file/i,
    }) as HTMLInputElement
    expect(acknowledgement.checked).toBe(false)
    expect(submit.disabled).toBe(true)
    fireEvent.click(submit)
    expect(api.refreshACL).not.toHaveBeenCalled()
    fireEvent.click(acknowledgement)
    expect(submit.disabled).toBe(false)
    fireEvent.click(submit)
    await waitFor(() =>
      expect(api.refreshACL).toHaveBeenCalledWith(1, {
        username: 'admin',
        password: 'sentinel-password',
        private_key: 'sentinel-key',
        acknowledge_router_changes: true,
      }),
    )
    expect(
      await screen.findByText(/oonfeeWRT controller access payload installed or refreshed and verified/),
    ).toBeTruthy()
    expect(screen.queryByDisplayValue('sentinel-password')).toBeNull()
    expect(screen.queryByDisplayValue('sentinel-key')).toBeNull()
    expect(acknowledgement.checked).toBe(false)
    expect(submit.disabled).toBe(true)
  })

  it('shows an exact LLDP package plan before accepting a separate install acknowledgement', async () => {
    api.changeLLDPCapability
      .mockResolvedValueOnce({
        device_id: 1,
        name: 'ap-1',
        state: 'install_planned',
        package_manager: 'apk',
        requested_packages: ['lldpd'],
        added_packages: [],
        plan: '(1/1) Installing lldpd',
        plan_hash: 'plan-1',
        service_enabled: false,
        service_running: false,
      })
      .mockResolvedValueOnce({
        device_id: 1,
        name: 'ap-1',
        state: 'installed',
        package_manager: 'apk',
        requested_packages: ['lldpd'],
        added_packages: ['lldpd'],
        service_enabled: true,
        service_running: true,
    })
    await openPanel()
    expect(await screen.findByText(/Adds measured wired-neighbour discovery/)).toBeTruthy()
    const capabilityDetails = screen.getByText('What this installs and rolls back').closest('details') as HTMLDetailsElement
    const review = screen.getByText('Review LLDP installation')
    expect(capabilityDetails.open).toBe(false)
    expect(review.closest('details')).toBeNull()
    fireEvent.click(review)
    expect(capabilityDetails.open).toBe(true)
    const planButton = screen.getByRole('button', {
      name: 'Refresh index and show exact install plan',
    }) as HTMLButtonElement
    expect(planButton.disabled).toBe(true)
    const indexAcknowledgement = screen.getByRole('checkbox', {
      name: /authorize refreshing the router's package index cache/i,
    })
    expect(indexAcknowledgement.parentElement?.querySelector(':scope > span code')?.textContent).toBe('lldpd')
    fireEvent.click(indexAcknowledgement)
    fireEvent.click(planButton)
    expect(await screen.findByText('(1/1) Installing lldpd')).toBeTruthy()
    expect(api.changeLLDPCapability).toHaveBeenNthCalledWith(
      1,
      1,
      expect.objectContaining({
        action: 'plan_install',
        acknowledge_package_index_refresh: true,
      }),
    )
    const install = screen.getByRole('button', {
      name: 'Install LLDP capability',
    }) as HTMLButtonElement
    const changeAcknowledgement = screen.getByRole('checkbox', {
      name: /authorize installing.*refreshing.*once more immediately beforehand/i,
    })
    expect(screen.getByText('(1/1) Installing lldpd').closest('.notice-disclosure')).toBeNull()
    expect(changeAcknowledgement.closest('.notice-disclosure')).toBeNull()
    expect(install.closest('.notice-disclosure')).toBeNull()
    expect(install.disabled).toBe(true)
    fireEvent.click(changeAcknowledgement)
    fireEvent.click(install)
    await waitFor(() =>
      expect(api.changeLLDPCapability).toHaveBeenNthCalledWith(
        2,
        1,
        expect.objectContaining({
          action: 'install',
          plan_hash: 'plan-1',
          acknowledge_router_changes: true,
          acknowledge_package_index_refresh: true,
        }),
      ),
    )
  })

  it('clears rejected LLDP package-plan credentials before a retry', async () => {
    api.changeLLDPCapability.mockRejectedValueOnce(new Error('authentication failed')).mockResolvedValueOnce({
      device_id: 1,
      name: 'ap-1',
      state: 'install_planned',
      package_manager: 'apk',
      requested_packages: ['lldpd'],
      added_packages: [],
      plan: '(1/1) Installing lldpd',
      plan_hash: 'plan-1',
      service_enabled: false,
      service_running: false,
    })
    await openPanel()
    fireEvent.click(await screen.findByText('Review LLDP installation'))
    fireEvent.change(screen.getByLabelText('Device administrator password'), {
      target: { value: 'stale-password' },
    })
    fireEvent.change(screen.getByLabelText('SSH private key (optional)'), {
      target: { value: 'stale-key' },
    })
    fireEvent.click(
      screen.getByRole('checkbox', {
        name: /authorize refreshing the router's package index cache/i,
      }),
    )
    const showPlan = screen.getByRole('button', {
      name: 'Refresh index and show exact install plan',
    })
    fireEvent.click(showPlan)

    expect(await screen.findByText('authentication failed')).toBeTruthy()
    expect(screen.queryByDisplayValue('stale-password')).toBeNull()
    expect(screen.queryByDisplayValue('stale-key')).toBeNull()

    fireEvent.change(screen.getByLabelText('Device administrator password'), {
      target: { value: 'replacement-password' },
    })
    fireEvent.change(screen.getByLabelText('SSH private key (optional)'), {
      target: { value: 'replacement-key' },
    })
    fireEvent.click(showPlan)
    await waitFor(() =>
      expect(api.changeLLDPCapability).toHaveBeenNthCalledWith(
        2,
        1,
        expect.objectContaining({
          action: 'plan_install',
          password: 'replacement-password',
          private_key: 'replacement-key',
        }),
      ),
    )
    expect(screen.getByDisplayValue('replacement-password')).toBeTruthy()
    expect(screen.getByDisplayValue('replacement-key')).toBeTruthy()
  })

  it('requires separate consent for read-only LLDP runtime diagnostics', async () => {
    api.lldpCapability.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      state: 'installed',
      package_manager: 'apk',
      requested_packages: ['lldpd'],
      added_packages: ['lldpd'],
    })
    api.changeLLDPCapability.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      state: 'installed',
      package_manager: 'apk',
      requested_packages: ['lldpd'],
      added_packages: ['lldpd'],
      diagnostics: 'RUNTIME_INTERFACES\n{}',
    })
    await openPanel()
    fireEvent.click(await screen.findByText('Review LLDP rollback'))
    const inspect = screen.getByRole('button', {
      name: 'Inspect LLDP runtime (read only)',
    }) as HTMLButtonElement
    expect(inspect.disabled).toBe(true)
    fireEvent.click(
      screen.getByRole('checkbox', {
        name: /authorize a read-only inspection/i,
      }),
    )
    fireEvent.click(inspect)
    await waitFor(() =>
      expect(api.changeLLDPCapability).toHaveBeenCalledWith(
        1,
        expect.objectContaining({
          action: 'diagnose',
          acknowledge_read_only_diagnostics: true,
        }),
      ),
    )
    expect(await screen.findByText('LLDP runtime diagnostic')).toBeTruthy()
  })

  it('shows and separately authorizes an exact LLDP interface configuration plan', async () => {
    api.lldpCapability.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      state: 'installed',
      package_manager: 'apk',
      requested_packages: ['lldpd'],
      added_packages: ['lldpd'],
      configuration_state: 'package_default',
    })
    api.changeLLDPCapability
      .mockResolvedValueOnce({
        device_id: 1,
        name: 'ap-1',
        state: 'configure_planned',
        package_manager: 'apk',
        requested_packages: ['lldpd'],
        added_packages: ['lldpd'],
        configuration_state: 'planned',
        configured_interfaces: ['lan1', 'lan3'],
        plan: 'Replace only lldpd.config.interface with: lan1, lan3.',
        plan_hash: 'config-1',
      })
      .mockResolvedValueOnce({
        device_id: 1,
        name: 'ap-1',
        state: 'installed',
        package_manager: 'apk',
        requested_packages: ['lldpd'],
        added_packages: ['lldpd'],
        configuration_state: 'configured',
        configured_interfaces: ['lan1', 'lan3'],
      })
    await openPanel()
    fireEvent.click(await screen.findByText('Review LLDP rollback'))
    fireEvent.change(screen.getByLabelText('Device administrator password'), {
      target: { value: 'sentinel-password' },
    })
    expect(screen.getByText(/Credentials remain only in this open review/)).toBeTruthy()
    const showPlan = screen.getByRole('button', {
      name: 'Show exact LLDP interface plan',
    }) as HTMLButtonElement
    expect(showPlan.disabled).toBe(true)
    fireEvent.click(
      screen.getByRole('checkbox', {
        name: /authorize reading the current lldpd UCI export/i,
      }),
    )
    fireEvent.click(showPlan)
    expect(await screen.findByText(/Replace only lldpd.config.interface with: lan1, lan3/)).toBeTruthy()
    expect(api.changeLLDPCapability).toHaveBeenNthCalledWith(
      1,
      1,
      expect.objectContaining({
        action: 'plan_configure',
        password: 'sentinel-password',
        acknowledge_read_only_diagnostics: true,
      }),
    )
    expect(screen.getByDisplayValue('sentinel-password')).toBeTruthy()
    const apply = screen.getByRole('button', {
      name: 'Apply LLDP interface configuration',
    }) as HTMLButtonElement
    expect(apply.disabled).toBe(true)
    fireEvent.click(
      screen.getByRole('checkbox', {
        name: /authorize replacing only lldpd.config.interface/i,
      }),
    )
    fireEvent.click(apply)
    await waitFor(() =>
      expect(api.changeLLDPCapability).toHaveBeenNthCalledWith(
        2,
        1,
        expect.objectContaining({
          action: 'configure',
          password: 'sentinel-password',
          plan_hash: 'config-1',
          acknowledge_router_changes: true,
        }),
      ),
    )
    expect(screen.queryByDisplayValue('sentinel-password')).toBeNull()
  })

  it('clears rejected LLDP interface-plan credentials before a retry', async () => {
    api.lldpCapability.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      state: 'installed',
      package_manager: 'apk',
      requested_packages: ['lldpd'],
      added_packages: ['lldpd'],
      configuration_state: 'package_default',
    })
    api.changeLLDPCapability.mockRejectedValueOnce(new Error('authentication failed')).mockResolvedValueOnce({
      device_id: 1,
      name: 'ap-1',
      state: 'configure_planned',
      package_manager: 'apk',
      requested_packages: ['lldpd'],
      added_packages: ['lldpd'],
      configuration_state: 'planned',
      configured_interfaces: ['lan1'],
      plan: 'Replace only lldpd.config.interface with: lan1.',
      plan_hash: 'config-1',
    })
    await openPanel()
    fireEvent.click(await screen.findByText('Review LLDP rollback'))
    fireEvent.change(screen.getByLabelText('Device administrator password'), {
      target: { value: 'stale-password' },
    })
    fireEvent.change(screen.getByLabelText('SSH private key (optional)'), {
      target: { value: 'stale-key' },
    })
    fireEvent.click(
      screen.getByRole('checkbox', {
        name: /authorize reading the current lldpd UCI export/i,
      }),
    )
    const showPlan = screen.getByRole('button', {
      name: 'Show exact LLDP interface plan',
    })
    fireEvent.click(showPlan)

    expect(await screen.findByText('authentication failed')).toBeTruthy()
    expect(screen.queryByDisplayValue('stale-password')).toBeNull()
    expect(screen.queryByDisplayValue('stale-key')).toBeNull()

    fireEvent.change(screen.getByLabelText('Device administrator password'), {
      target: { value: 'replacement-password' },
    })
    fireEvent.change(screen.getByLabelText('SSH private key (optional)'), {
      target: { value: 'replacement-key' },
    })
    fireEvent.click(showPlan)
    await waitFor(() =>
      expect(api.changeLLDPCapability).toHaveBeenNthCalledWith(
        2,
        1,
        expect.objectContaining({
          action: 'plan_configure',
          password: 'replacement-password',
          private_key: 'replacement-key',
        }),
      ),
    )
    expect(screen.getByDisplayValue('replacement-password')).toBeTruthy()
    expect(screen.getByDisplayValue('replacement-key')).toBeTruthy()
  })

  it('clears the ACL refresh credential and consent after a failed request', async () => {
    api.refreshACL.mockRejectedValue(new Error('verification failed'))
    await openPanel()
    fireEvent.click(screen.getByText('Review or refresh controller access payload'))
    fireEvent.change(screen.getByLabelText('Device administrator password'), {
      target: { value: 'failed-password' },
    })
    fireEvent.change(screen.getByLabelText('SSH private key (optional)'), {
      target: { value: 'failed-key' },
    })
    const acknowledgement = screen.getByRole('checkbox', {
      name: /accepting installs the payload if missing or replaces the controller's single rpcd ACL JSON file/i,
    }) as HTMLInputElement
    const submit = screen.getByRole('button', {
      name: 'Install or refresh controller access payload and verify',
    }) as HTMLButtonElement
    fireEvent.click(acknowledgement)
    fireEvent.click(submit)

    expect(await screen.findByText('verification failed')).toBeTruthy()
    expect(screen.queryByDisplayValue('failed-password')).toBeNull()
    expect(screen.queryByDisplayValue('failed-key')).toBeNull()
    expect(acknowledgement.checked).toBe(false)
    expect(submit.disabled).toBe(true)
  })

  // The rule the whole capability-diff design protects: a check that stopped
  // being POSSIBLE is not a capability that stopped EXISTING. Rendering the two
  // the same way recreates, in the UI, the bug the three-state model exists to
  // prevent — and sends someone hunting a hardware fault that is not there.
  it('labels a visibility change as not a loss', async () => {
    api.reprobe.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      summary: 'class A',
      unchanged: false,
      actionable: 0,
      capabilities: null,
      note: '',
      changes: [
        {
          kind: 'feature',
          name: 'hostapd-control',
          from: 'present',
          to: 'not-observable',
          effect: 'no-longer-observable',
          detail: 'hostapd-control can no longer be checked',
        },
      ],
    })
    await openPanel()
    fireEvent.click(screen.getByText('Re-probe capabilities'))

    await waitFor(() => expect(screen.getByText(/not a loss/i)).toBeTruthy())
    // And the summary line says none of it changes what may be sent.
    expect(screen.getByText(/changes in what the controller can see/i)).toBeTruthy()
  })

  // A real loss must still read as one, or the caution above becomes a blanket
  // excuse and the panel stops reporting anything.
  it('does not soften a genuine loss', async () => {
    api.reprobe.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      summary: 'class A',
      unchanged: false,
      actionable: 1,
      capabilities: null,
      note: '',
      changes: [
        {
          kind: 'radio',
          name: 'phy1',
          from: 'a,n,ac',
          to: '',
          effect: 'lost',
          detail: 'radio phy1 is gone',
        },
      ],
    })
    await openPanel()
    fireEvent.click(screen.getByText('Re-probe capabilities'))

    await waitFor(() => expect(screen.getByText('radio phy1 is gone')).toBeTruthy())
    expect(screen.queryByText(/changes in what the controller can see/i)).toBeNull()
  })

  // "Nothing changed" is a RESULT. An empty list reads as a failure, and after
  // pressing a button that is the worse of the two readings.
  it('says so when a probe found nothing', async () => {
    api.reprobe.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      summary: 'class A Linksys WRT3200ACM',
      unchanged: true,
      actionable: 0,
      capabilities: null,
      note: '',
      changes: [],
    })
    await openPanel()
    fireEvent.click(screen.getByText('Re-probe capabilities'))

    await waitFor(() => expect(screen.getByText(/nothing changed/i)).toBeTruthy())
  })

  it('refreshes the fleet row after a successful re-probe', async () => {
    api.reprobe.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      summary: 'class C',
      unchanged: false,
      actionable: 0,
      capabilities: null,
      note: '',
      changes: [],
    })
    const onChanged = vi.fn()
    const { Devices } = await import('./Devices')
    render(<Devices devices={[{ ...detail, quiesced: false } as never]} onAdopt={() => {}} onChanged={onChanged} />)
    fireEvent.click(screen.getByText('ap-1'))
    fireEvent.click(await screen.findByText('Re-probe capabilities'))

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1))
  })

  // A role that no longer fits the hardware is a warning, shown where the probe
  // result is — it is exactly when that fact can change.
  it('surfaces a role that no longer fits', async () => {
    api.reprobe.mockResolvedValue({
      device_id: 1,
      name: 'ap-1',
      summary: 'class A',
      unchanged: true,
      actionable: 0,
      capabilities: null,
      note: '',
      changes: [],
      role_fit: ['adopted as "ap", but this device reported no radios'],
    })
    await openPanel()
    fireEvent.click(screen.getByText('Re-probe capabilities'))

    await waitFor(() => expect(screen.getByText(/reported no radios/i)).toBeTruthy())
  })
})

describe('Settings — neighbour reports', () => {
  const wlan = (over: Record<string, unknown> = {}) => ({
    id: 1,
    ssid: 'fixture-roam',
    network_id: 1,
    group_id: 1,
    bands: ['2g', '5g'],
    security_mode: 'psk2',
    pmf: '1',
    has_key: true,
    enabled: true,
    roaming: { ft: true, ft_over_ds: true, kv: true, ft_with_psk2: true },
    hidden: false,
    isolate: false,
    max_assoc: 0,
    ...over,
  })

  const siteWith = (wlans: unknown[]) => ({
    name: 'Site',
    uuid: 'abcdef01-2345-6789-abcd-ef0123456789',
    wlans,
    meshes: [],
    groups: [{ id: 1, name: 'all', device_ids: [] }],
    networks: [
      {
        id: 1,
        name: 'lan',
        vlan: 1,
        cidr: '192.168.1.1/24',
        zone: 'lan',
        enabled: true,
      },
    ],
    problems: [],
    overrides: [],
    overridable: [],
    override_note: '',
  })

  // A WLAN with 802.11k switched off must not offer to distribute anything. The
  // renderer writes no rrm_neighbor_report for it, so the AP will not answer a
  // client's request — a button that fills a list nobody reads is a feature that
  // is not there.
  it('offers nothing when no network asked for 802.11k', async () => {
    api.site.mockResolvedValue(siteWith([wlan({ roaming: { ft: true, kv: false } })]))
    render(<Settings devices={[]} />)

    await waitFor(() => expect(screen.getByText(/Neighbour reports/)).toBeTruthy())
    expect(screen.getByText(/No wireless network has neighbour reports/)).toBeTruthy()
    expect((screen.getByText('Distribute now') as HTMLButtonElement).disabled).toBe(true)
  })

  it('names the networks it would distribute across', async () => {
    api.site.mockResolvedValue(siteWith([wlan()]))
    render(<Settings devices={[]} />)

    await waitFor(() => expect(screen.getByText(/Neighbour reports/)).toBeTruthy())
    // Scoped to the card: the SSID also appears in the WLAN list above, and a
    // document-wide match would pass even if the card named nothing.
    const card = screen.getByText(/Neighbour reports/).closest('section') as HTMLElement
    expect(within(card).getByText('fixture-roam')).toBeTruthy()
    expect((screen.getByText('Distribute now') as HTMLButtonElement).disabled).toBe(false)
  })

  it('keeps the automatic purpose visible while neighbour mechanics stay collapsed', async () => {
    api.site.mockResolvedValue(siteWith([wlan()]))
    render(<Settings devices={[]} />)

    const notice = await screen.findByRole('group', {
      name: 'Information: 802.11k neighbour reports',
    })
    expect(within(notice).getByText(/every 15 minutes and after every Apply/)).toBeTruthy()
    const toggle = within(notice).getByText('More information about neighbour reports')
    const details = toggle.closest('details') as HTMLDetailsElement
    expect(details.open).toBe(false)
    expect(screen.getByRole('button', { name: 'Distribute now' }).closest('details')).toBeNull()
    fireEvent.click(toggle)
    expect(details.open).toBe(true)
    expect(within(notice).getByText(/An AP that restarts comes back with an empty list/)).toBeTruthy()
  })

  // The distinction the whole capability model exists to protect, at the UI
  // layer. "Could not reach this device" is something to go and fix now; "this
  // device was adopted before the controller could ask" is a standing fact that
  // will not change until it is re-adopted. Rendering both as an error teaches
  // people to ignore errors.
  it('separates a device that failed from one that was skipped', async () => {
    api.site.mockResolvedValue(siteWith([wlan()]))
    api.distributeNeighbours.mockResolvedValue({
      ssids: ['fixture-roam'],
      updated: 2,
      unchanged: 0,
      devices: [
        {
          device_id: 1,
          name: 'ap-one',
          updated: 2,
          unchanged: 0,
          bsses: [
            {
              iface: 'phy0-ap1',
              ssid: 'fixture-roam',
              bssid: '02:00:00:ab:51:43',
              neighbours: 3,
              changed: true,
            },
          ],
        },
        {
          device_id: 2,
          name: 'ap-old',
          updated: 0,
          unchanged: 0,
          skipped: 'this device has not been shown to accept neighbour lists — re-adopt it',
        },
        {
          device_id: 3,
          name: 'ap-gone',
          updated: 0,
          unchanged: 0,
          error: 'could not reach this device',
        },
      ],
    })

    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText(/Neighbour reports/)).toBeTruthy())
    fireEvent.click(screen.getByText('Distribute now'))

    await waitFor(() => expect(screen.getByText(/Updated 2 access point/)).toBeTruthy())
    expect(screen.getByText(/knows 3 neighbours/)).toBeTruthy()

    const skipped = screen.getByText(/re-adopt it/)
    const failed = screen.getByText(/could not reach this device/)
    expect(skipped.getAttribute('style')).not.toEqual(failed.getAttribute('style'))
  })

  // Zero updates is a success, not an empty screen. A run that says nothing is
  // indistinguishable from a broken feature.
  it('says so plainly when everything was already correct', async () => {
    api.site.mockResolvedValue(siteWith([wlan()]))
    api.distributeNeighbours.mockResolvedValue({
      ssids: ['fixture-roam'],
      updated: 0,
      unchanged: 4,
      devices: [],
    })

    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText(/Neighbour reports/)).toBeTruthy())
    fireEvent.click(screen.getByText('Distribute now'))

    await waitFor(() => expect(screen.getByText(/already up to date/)).toBeTruthy())
    expect(screen.getByText(/4 already correct/)).toBeTruthy()
  })
})

describe('Devices — the unmeasured class', () => {
  // "?" is not a failure and not unclassifiable hardware. It means this SoC
  // family has never been measured — which is most old routers, and precisely
  // the hardware this project exists to support. A bare "?" sends an operator
  // looking for a fault; naming the target says what the controller is looking
  // at, which is the only thing that would let anyone close the gap.
  it('explains an unmeasured class and names the board target', () => {
    render(<DeviceClass cls="?" target="ath79/generic" />)
    expect(screen.getByText(/ath79\/generic/)).toBeTruthy()
    expect(screen.getByText(/has not been measured/)).toBeTruthy()
  })

  it('says nothing extra for a class that WAS measured', () => {
    render(<DeviceClass cls="A" target="mvebu/cortexa9" />)
    expect(screen.getByText('A')).toBeTruthy()
    expect(screen.queryByText(/has not been measured/)).toBeNull()
  })

  // A device with no class at all is a different thing again: the probe never
  // ran or its record could not be read. That is "we do not know", not "we know
  // it is unmeasured".
  it('distinguishes no class from an unmeasured one', () => {
    const { container } = render(<DeviceClass cls={null} />)
    expect(screen.queryByText(/has not been measured/)).toBeNull()
    // Unknown carries its reason in a title attribute, not in text — the whole
    // point being that the em dash is never mistaken for a value.
    expect(container.querySelector('[title]')?.getAttribute('title')).toMatch(/not classified this device/)
  })
})

describe('Devices — column preferences', () => {
  // The screen an operator looks at most was the one grid with no column
  // customisation. Its headers were not draggable, there was no picker, and
  // nothing said why — so trying to reorder read as a broken feature rather
  // than an absent one.
  const device = {
    id: 1,
    mac: '02:00:00:ab:51:40',
    name: 'ap-one',
    host: '192.0.2.1',
    class: 'A',
    firmware: 'OpenWrt 25.12.5',
    online: true,
    adopted: true,
    role: 'ap',
    last_seen: 1,
  }

  it('distinguishes loading, first-load failure, and retained last-good inventory', () => {
    const { rerender } = render(<Devices devices={[]} devicesLoaded={false} />)
    expect(screen.getByText('Managed devices (…)')).toBeTruthy()
    expect(screen.getByText('Loading devices…')).toBeTruthy()

    rerender(<Devices devices={[]} devicesLoaded={false} devicesError="inventory offline" />)
    expect(screen.getByText('Managed devices (Unavailable)')).toBeTruthy()
    expect(screen.getByText(/Device inventory is unavailable\. Retry/)).toBeTruthy()

    rerender(<Devices devices={[device] as never} devicesLoaded devicesError="refresh failed" />)
    expect(screen.getByText('Managed devices (1)')).toBeTruthy()
    expect(screen.getByText('ap-one')).toBeTruthy()
  })

  it('makes the device grid reorderable like every other grid', () => {
    // A row is required: an empty grid renders its empty state instead of a
    // header, and there is nothing to customise when there are no columns on
    // screen.
    render(<Devices devices={[device] as never} />)
    expect(screen.getByText(/Customize columns/)).toBeTruthy()
    for (const th of screen.getAllByRole('columnheader')) {
      expect(th.getAttribute('draggable')).toBe('true')
    }
    // Legacy AP rows historically also carried the switching/VLAN plumbing.
    expect(screen.getByText('AP · Switch')).toBeTruthy()
  })
})

describe('Settings — wireless uplinks', () => {
  const base = {
    name: 'Site',
    uuid: 'abcdef01-2345-6789-abcd-ef0123456789',
    meshes: [],
    uplinks: [],
    groups: [{ id: 1, name: 'all', device_ids: [] }],
    networks: [
      {
        id: 1,
        name: 'lan',
        vlan: 1,
        cidr: '192.168.1.1/24',
        zone: 'lan',
        enabled: true,
      },
    ],
    problems: [],
    overrides: [],
    overridable: [],
    override_note: '',
  }

  const wlan = (over: Record<string, unknown> = {}) => ({
    id: 1,
    ssid: 'fixture-roam',
    network_id: 1,
    group_id: 1,
    bands: ['5g'],
    security_mode: 'psk2',
    pmf: '1',
    has_key: true,
    enabled: true,
    roaming: { ft: true, ft_over_ds: true, kv: true, ft_with_psk2: true },
    hidden: false,
    isolate: false,
    max_assoc: 0,
    allow_uplink: false,
    ...over,
  })

  // PMF was carried by the model, written by the renderer onto every WLAN, and
  // exposed nowhere — hardcoded to "1" at creation. That made the driver-defect
  // warning unactionable: it told an operator to turn PMF off on hardware that
  // cannot do it, with nowhere to do so.
  it('lets PMF be changed, and hides it where WPA3 mandates it', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [wlan({ security_mode: 'psk2' })],
    })
    api.saveWLAN.mockResolvedValue({})
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getAllByText('fixture-roam').length).toBeGreaterThan(0))

    // The row's own Edit button; the SSID text is not the control.
    fireEvent.click(screen.getAllByText('Edit')[0])
    await waitFor(() => expect(screen.getByText('Protected management frames')).toBeTruthy())
    expect(screen.getByText('Isolate clients on this access point')).toBeTruthy()
    expect(screen.getByText(/Verify client behavior after applying/i)).toBeTruthy()
    expect(screen.getByText(/different APs still need additional switch or bridge policy/i)).toBeTruthy()
    // All three states reachable — "Disabled" is the one the warning asks for.
    expect(screen.getByText('Disabled')).toBeTruthy()
    expect(screen.getByText('Required')).toBeTruthy()

    // WPA3 mandates PMF and the renderer forces it back on regardless, so
    // offering a choice it would override is worse than offering none.
    fireEvent.click(screen.getByText('WPA3 only'))
    await waitFor(() => expect(screen.queryByText('Protected management frames')).toBeNull())

    // Enhanced open mandates PMF exactly as WPA3 does, so the control is
    // hidden for the same reason. Untested, this guard could be reverted
    // silently and an "owe" WLAN would store pmf="0" while the device got 2.
    fireEvent.click(screen.getByText('Enhanced open'))
    await waitFor(() => expect(screen.queryByText('Protected management frames')).toBeNull())

    // Open has no RSN at all, so there is nothing to protect. This is the case
    // that used to leave a WLAN carrying the pmf="1" every draft is created
    // with, rendered onto the device where nobody could see or clear it.
    fireEvent.click(screen.getByText('Open'))
    await waitFor(() => expect(screen.queryByText('Protected management frames')).toBeNull())

    // Back to WPA2 and choose Disabled — the state the lab device is in, and
    // the one that makes the next step interesting.
    fireEvent.click(screen.getByText('WPA2 only'))
    await waitFor(() => expect(screen.getByText('Disabled')).toBeTruthy())
    fireEvent.click(screen.getByText('Disabled'))

    // Transitional WPA2/WPA3 keeps the control but must not offer Disabled:
    // that silently removes the WPA3 half of a network still advertising it.
    fireEvent.click(screen.getByText('WPA2/WPA3'))
    await waitFor(() => expect(screen.getByText('Protected management frames')).toBeTruthy())
    expect(screen.queryByText('Disabled')).toBeNull()
    expect(screen.getByText('Required')).toBeTruthy()

    // And the draft must not still hold the value that mode does not offer.
    // Found by looking: coming from WPA2-with-PMF-disabled left neither option
    // selected, and saving stored a setting the form never showed.
    fireEvent.click(screen.getByText('Save'))
    await waitFor(() => expect(api.saveWLAN).toHaveBeenCalled())
    const saved = api.saveWLAN.mock.calls.at(-1)?.[0] as { pmf?: string }
    if (saved.pmf === '0') {
      throw new Error(
        'saved pmf="0" for a WPA2/WPA3 network, which the picker does not ' + 'offer and the renderer would override',
      )
    }
  })

  // A detour through a mode that hides PMF must not silently rewrite a
  // deliberate choice. Required -> Open -> WPA2 used to come back Disabled:
  // the clamp read the already-clamped draft rather than what was picked, and
  // the apply preview names a section and an option count, never a value, so
  // the downgrade appeared nowhere.
  it('keeps a deliberate PMF choice across a mode detour', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [wlan({ security_mode: 'psk2' })],
    })
    api.saveWLAN.mockResolvedValue({})
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getAllByText('fixture-roam').length).toBeGreaterThan(0))
    fireEvent.click(screen.getAllByText('Edit')[0])
    await waitFor(() => expect(screen.getByText('Protected management frames')).toBeTruthy())

    fireEvent.click(screen.getByText('Required'))
    fireEvent.click(screen.getByText('Open'))
    await waitFor(() => expect(screen.queryByText('Protected management frames')).toBeNull())
    fireEvent.click(screen.getByText('WPA2 only'))
    await waitFor(() => expect(screen.getByText('Disabled')).toBeTruthy())

    fireEvent.click(screen.getByText('Save'))
    await waitFor(() => expect(api.saveWLAN).toHaveBeenCalled())
    const saved = api.saveWLAN.mock.calls.at(-1)?.[0] as { pmf?: string }
    if (saved.pmf !== '2') {
      throw new Error(`a detour through Open turned a deliberate "Required" into pmf="${saved.pmf}"`)
    }
  })

  // A WLAN already persisted with a PMF its mode does not offer must be
  // normalised on the way IN, not just on a mode change.
  //
  // sae-mixed + pmf="0" is writable by an earlier build, by the mode-switch
  // hole, or by a plain POST to the API. It opened with two buttons and
  // neither selected, then re-saved the value it had never shown. Guarding one
  // door and not the other is not guarding.
  it('normalises a stored PMF the picker cannot show', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [wlan({ security_mode: 'sae-mixed', pmf: '0' })],
    })
    api.saveWLAN.mockResolvedValue({})
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getAllByText('fixture-roam').length).toBeGreaterThan(0))
    fireEvent.click(screen.getAllByText('Edit')[0])
    await waitFor(() => expect(screen.getByText('Protected management frames')).toBeTruthy())

    // Saving without touching PMF must not write back the unshowable value.
    fireEvent.click(screen.getByText('Save'))
    await waitFor(() => expect(api.saveWLAN).toHaveBeenCalled())
    const saved = api.saveWLAN.mock.calls.at(-1)?.[0] as { pmf?: string }
    if (saved.pmf === '0') {
      throw new Error(
        'the editor re-saved pmf="0" for a sae-mixed WLAN — a value its own ' +
          'picker does not offer and never displayed',
      )
    }
  })

  // The 802.11k card must SHOW why a device failed, not guess.
  //
  // "could not be reached" was asserted for any failure, including an ACL
  // narrowed by a sysupgrade — a device that is powered, on the network,
  // answering, and refusing one ubus call. That sends an operator to check
  // cabling when the remedy is to re-adopt so the ACL is rewritten. The global
  // mock returns {ran:false}, so no test rendered this branch at all.
  it('shows the reason a neighbour push failed, rather than guessing', async () => {
    api.site.mockResolvedValue({ ...base, wlans: [wlan()] })
    api.lastNeighbours.mockResolvedValue({
      ran: true,
      at: Math.floor(Date.now() / 1000) - 180,
      devices_failed: 1,
      result: {
        updated: 1,
        unchanged: 1,
        devices: [
          {
            device_id: 1,
            name: 'ap-192-168-1-1',
            error: 'could not list wireless interfaces: ubus iwinfo.devices: access denied',
          },
        ],
      },
    })
    render(<Settings devices={[]} />)

    await waitFor(() => expect(screen.getByText(/reported an error/)).toBeTruthy())
    expect(screen.getByText(/access denied/)).toBeTruthy()
    // And it must not assert a cause it has no evidence for.
    expect(screen.queryByText(/could not be reached/)).toBeNull()
  })

  // A network that does not accept bridges must not be offered as somewhere to
  // join. Offering it would let someone build the one configuration whose
  // failure mode is indistinguishable from a driver refusing 4-address frames:
  // the station associates as an ordinary client and everything behind the
  // device stays dark.
  it('offers nothing to join until a network accepts bridges', async () => {
    api.site.mockResolvedValue({ ...base, wlans: [wlan()] })
    render(<Settings devices={[]} />)

    expect(await screen.findByRole('group', { name: 'Information: Wireless uplinks' })).toBeTruthy()
    expect(screen.getByText(/No network accepts wireless bridges yet/)).toBeTruthy()
    expect(screen.queryByText('Add uplink')).toBeNull()
  })

  it('offers the add form once a network accepts bridges', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [wlan({ allow_uplink: true })],
    })
    render(<Settings devices={[{ id: 7, name: 'no-cable' } as never]} />)

    expect(await screen.findByRole('group', { name: 'Information: Wireless uplinks' })).toBeTruthy()
    expect(screen.getByText('Add uplink')).toBeTruthy()
    expect(screen.getByText(/No device is using a wireless uplink/)).toBeTruthy()
  })

  it('keeps the uplink purpose visible while 4-address mechanics stay collapsed', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [wlan({ allow_uplink: true })],
    })
    render(<Settings devices={[{ id: 7, name: 'no-cable' } as never]} />)

    const notice = await screen.findByRole('group', { name: 'Information: Wireless uplinks' })
    expect(within(notice).getByText(/network must accept wireless bridges first/)).toBeTruthy()
    const toggle = within(notice).getByText('More information about wireless uplinks')
    const details = toggle.closest('details') as HTMLDetailsElement
    expect(details.open).toBe(false)
    expect(screen.getByRole('button', { name: 'Add uplink' }).closest('details')).toBeNull()
    fireEvent.click(toggle)
    expect(details.open).toBe(true)
    expect(within(notice).getByText(/joins as a 4-address bridge/)).toBeTruthy()
  })

  // One per device, and the reason is said rather than merely enforced: a
  // router with two bridges the same network to itself twice.
  it('will not offer a device that already has an uplink', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [wlan({ allow_uplink: true })],
      uplinks: [{ id: 1, device_id: 7, wlan_id: 1, band: '5g', enabled: true }],
    })
    render(<Settings devices={[{ id: 7, name: 'no-cable' } as never]} />)

    expect(await screen.findByRole('group', { name: 'Information: Wireless uplinks' })).toBeTruthy()
    expect(screen.getByText(/joins fixture-roam on 5g/)).toBeTruthy()
    expect(screen.getByText(/loop rather than redundancy/)).toBeTruthy()
    expect(screen.queryByText('Add uplink')).toBeNull()
  })

  // The hazard wording comes from the server rather than being restated in the
  // UI, so there is one wording of it rather than two that can drift apart.
  it('shows the server’s own warning after a change', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [wlan({ allow_uplink: true })],
      uplinks: [{ id: 1, device_id: 7, wlan_id: 1, band: '5g', enabled: true }],
    })
    api.deleteUplink.mockResolvedValue({
      deleted: 1,
      note: 'applying this removes the station interface — acknowledge it',
    })
    render(<Settings devices={[{ id: 7, name: 'no-cable' } as never]} />)

    await waitFor(() => expect(screen.getByText('Remove')).toBeTruthy())
    fireEvent.click(screen.getByText('Remove'))

    await waitFor(() => expect(screen.getByText(/removes the station interface/)).toBeTruthy())
  })
  // There was no control for a network's firewall zone at all, and
  // store.SaveNetwork used to default it to "lan" — so every network the
  // product could create asked for a second zone named lan beside the device's
  // own, and nothing in the UI could change it. Found on the operator's own
  // testvlan, stuck that way.
  it('lets a firewall zone be edited, and warns about one the device owns', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 1,
          name: 'lan',
          vlan: 1,
          cidr: '192.168.1.1/24',
          zone: 'lan',
          enabled: true,
        },
        {
          id: 2,
          name: 'testvlan',
          vlan: 2,
          cidr: '192.168.2.1/24',
          zone: 'lan',
          enabled: true,
        },
      ],
    })
    api.saveNetwork.mockResolvedValue({})
    render(<Settings devices={[]} />)

    const field = await screen.findByLabelText('Firewall zone for testvlan')
    expect((field as HTMLInputElement).value).toBe('lan')

    // Flagged where it can be fixed, and the flag says WHAT TO DO. The first
    // version named the problem only, and the operator's reply was "I'm not
    // actually sure what to do".
    const note = screen.getByRole('note')
    const advice = note.getAttribute('title') ?? ''
    expect(advice).toMatch(/belongs to the device/)
    expect(advice).toMatch(/To fix it: type a different name/)
    expect(advice).toMatch(/for example "testvlan"/)

    // And VLAN 1 is NOT flagged: a network on VLAN 0 or 1 renders no firewall
    // zone at all, so its zone is inert and a warning there is noise on a row
    // nobody can act on. Only testvlan carries one.
    expect(screen.getAllByRole('note')).toHaveLength(1)

    fireEvent.change(field, { target: { value: 'iot' } })
    fireEvent.keyDown(field, { key: 'Enter' })

    await waitFor(() => expect(api.saveNetwork).toHaveBeenCalled())
    const sent = api.saveNetwork.mock.calls[0][0]
    expect(sent.zone).toBe('iot')
    // Only the intended field is sent. The server merges it under one mutation
    // gate, so a stale editor cannot overwrite an unrelated DHCP/address edit.
    expect(sent.id).toBe(2)
    expect(sent).toEqual({ id: 2, zone: 'iot' })
  })

  it('states the legacy WAN-only policy before moving a network into an unconfigured zone', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      zones: [
        { name: 'iot', forward_to: ['wan'], explicit: true },
        { name: 'guest', forward_to: ['wan'], explicit: false },
        { name: 'trusted', forward_to: ['guest', 'wan'], explicit: true },
      ],
      networks: [
        {
          id: 2,
          name: 'iot',
          vlan: 20,
          cidr: '10.0.20.1/24',
          zone: 'iot',
          enabled: true,
        },
      ],
    })
    render(<Settings devices={[]} />)

    const field = await screen.findByLabelText('Firewall zone for iot')
    fireEvent.change(field, { target: { value: 'guest' } })
    const warning = await screen.findByText(/uses the legacy policy/i)
    expect(warning.textContent ?? '').toMatch(/Internet\/WAN is allowed/i)
    expect(warning.textContent ?? '').toMatch(/every other managed zone is blocked/i)
    expect(warning.textContent ?? '').toMatch(/Review Policy Engine, then Preview before Apply/i)

    fireEvent.change(field, { target: { value: 'trusted' } })
    expect(screen.queryByText(/uses the legacy policy/i)).toBeNull()
  })

  it('warns using the exact fw4 zone identifier before save', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 2,
          name: 'iot',
          vlan: 20,
          cidr: '10.0.20.1/24',
          zone: 'wan!',
          enabled: true,
        },
        {
          id: 3,
          name: '20_guest',
          vlan: 30,
          cidr: '10.0.30.1/24',
          zone: '20_guest',
          enabled: true,
        },
      ],
    })
    render(<Settings devices={[]} />)

    const notes = await screen.findAllByRole('note')
    expect(notes.map((note) => note.getAttribute('title')).join('\n')).toMatch(
      /renders as "wan".*belongs to the device/i,
    )
    expect(notes.map((note) => note.getAttribute('title')).join('\n')).toMatch(
      /starts with a digit.*rejected by current OpenWrt fw4.*for example "net_20_gues"/i,
    )
  })

  // A network could be created and deleted and nothing else. A typo in a VLAN
  // or an address meant deleting the row and starting again, and the zone —
  // once it had defaulted to "lan" — could not be corrected at all.
  it('opens an editor for an existing network and saves every field', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 2,
          name: 'testvlan',
          vlan: 2,
          cidr: '192.168.2.1/24',
          zone: 'iot',
          enabled: true,
        },
      ],
    })
    api.saveNetwork.mockResolvedValue({})
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('testvlan'))
    await waitFor(() => expect(screen.getByRole('dialog')).toBeTruthy())

    // The facts UniFi shows, derived from the address rather than stored.
    expect(screen.getByText('192.168.2.255')).toBeTruthy() // broadcast
    expect(screen.getByText('255.255.255.0')).toBeTruthy() // netmask
    expect(screen.getByText('192.168.2.100 – 192.168.2.249')).toBeTruthy()

    const dialog = screen.getByRole('dialog')
    const vlan = within(dialog).getByLabelText('VLAN')
    fireEvent.change(vlan, { target: { value: '7' } })
    fireEvent.click(within(dialog).getByText('Save'))

    await waitFor(() => expect(api.saveNetwork).toHaveBeenCalled())
    const sent = api.saveNetwork.mock.calls[0][0]
    expect(sent.id).toBe(2)
    expect(sent.vlan).toBe(7)
    // Everything else rides along: handleSaveNetwork rebuilds the record from
    // what it is sent, so a field left out is a field blanked.
    expect(sent.name).toBe('testvlan')
    expect(sent.cidr).toBe('192.168.2.1/24')
    expect(sent.zone).toBe('iot')
    expect(sent.enabled).toBe(true)
  })

  it('edits DHCP policy and shows the exact derived pool before saving', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 2,
          name: 'iot',
          vlan: 20,
          cidr: '10.0.20.1/24',
          zone: 'iot',
          dhcp: { enabled: true, start: 20, limit: 80, leasetime: '30m' },
          enabled: true,
        },
      ],
    })
    api.saveNetwork.mockResolvedValue({})
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('iot'))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('10.0.20.20 – 10.0.20.99')).toBeTruthy()

    fireEvent.change(within(dialog).getByLabelText('Pool start'), {
      target: { value: '30' },
    })
    fireEvent.change(within(dialog).getByLabelText('Pool limit'), {
      target: { value: '50' },
    })
    fireEvent.change(within(dialog).getByLabelText('Lease time'), {
      target: { value: '2h' },
    })
    expect(within(dialog).getByText('10.0.20.30 – 10.0.20.79')).toBeTruthy()
    fireEvent.click(within(dialog).getByText('Save'))

    await waitFor(() => expect(api.saveNetwork).toHaveBeenCalled())
    expect(api.saveNetwork.mock.calls[0][0].dhcp).toEqual({
      enabled: true,
      start: 30,
      limit: 50,
      leasetime: '2h',
    })
  })

  it('does not offer DHCP controls for the management LAN it never owns', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 1,
          name: 'lan',
          vlan: 1,
          cidr: '192.168.1.1/24',
          zone: 'lan',
          dhcp: { enabled: true, start: 100, limit: 150, leasetime: '12h' },
          enabled: true,
        },
      ],
    })
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('lan', { selector: 'strong' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).queryByLabelText('Pool start')).toBeNull()
    expect(within(dialog).queryByText('192.168.1.100 – 192.168.1.249')).toBeNull()
    expect(within(dialog).getByRole('note').textContent ?? '').toMatch(
      /leaves their addressing and DHCP server untouched/i,
    )
  })

  it('invalidates a fleet preview when DHCP is edited', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 2,
          name: 'iot',
          vlan: 20,
          cidr: '10.0.20.1/24',
          zone: 'iot',
          dhcp: { enabled: true, start: 20, limit: 80, leasetime: '30m' },
          enabled: true,
        },
      ],
    })
    api.preview.mockResolvedValue({
      devices: [
        {
          device_id: 1,
          name: 'gateway',
          role: 'gateway',
          blocked: false,
          touches_traversal: false,
          driver_defects: [],
          changes: [{ config: 'dhcp', section: 'oowrt_dhcp_iot', action: 'update' }],
        },
      ],
      site_errors: [],
    })
    api.saveNetwork.mockResolvedValue({})
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalled())
    const apply = () => screen.getAllByText(/^Apply/)[0].closest('button')!
    await waitFor(() => {
      if (apply().disabled) throw new Error('precondition: preview did not enable Apply')
    })

    fireEvent.click(screen.getByText('iot'))
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByText('DHCP server'))
    fireEvent.click(within(dialog).getByText('Save'))
    await waitFor(() => expect(api.saveNetwork).toHaveBeenCalled())

    if (!apply().disabled) {
      throw new Error('Apply remained enabled for a DHCP edit the preview never saw')
    }
    expect(screen.queryByText(/oowrt_dhcp_iot/)).toBeNull()
  })

  it('warns when a DHCP pool falls outside its subnet', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 2,
          name: 'small',
          vlan: 20,
          cidr: '10.0.20.1/25',
          zone: 'small',
          dhcp: { enabled: true, start: 100, limit: 30, leasetime: '12h' },
          enabled: true,
        },
      ],
    })
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('small'))
    const warning = await within(screen.getByRole('dialog')).findByRole('alert')
    expect(warning.textContent ?? '').toMatch(/do not fit this \/25/i)
    expect(warning.textContent ?? '').toMatch(/Applying is blocked/i)
    expect((within(screen.getByRole('dialog')).getByText('Save').closest('button') as HTMLButtonElement).disabled).toBe(
      true,
    )
  })

  it('requires an explicit customize-or-disable choice for an invalid legacy DHCP pool', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 2,
          name: 'legacy-small',
          vlan: 20,
          cidr: '10.0.20.1/25',
          zone: 'legacy-small',
          dhcp: {
            enabled: true,
            start: 100,
            limit: 150,
            leasetime: '12h',
            legacy_default: true,
          },
          enabled: true,
        },
      ],
    })
    api.saveNetwork.mockResolvedValue({})
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('legacy-small'))
    const dialog = await screen.findByRole('dialog')
    const warning = await within(dialog).findByRole('alert')
    expect(warning.textContent ?? '').toMatch(/upgraded network still inherits/i)
    expect(warning.textContent ?? '').toMatch(/customize Pool start and Pool limit/i)
    expect(warning.textContent ?? '').toMatch(/turn DHCP server off/i)
    expect(warning.textContent ?? '').toMatch(/No device will be changed/i)

    fireEvent.click(within(dialog).getByText('DHCP server'))
    fireEvent.click(within(dialog).getByText('Save'))
    await waitFor(() => expect(api.saveNetwork).toHaveBeenCalled())
    expect(api.saveNetwork.mock.calls[0][0].dhcp).toEqual({
      enabled: false,
      start: 100,
      limit: 150,
      leasetime: '12h',
    })
  })

  it('blocks a subnet or broadcast address from being used as a gateway', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 2,
          name: 'iot',
          vlan: 20,
          cidr: '10.0.20.1/24',
          zone: 'iot',
          dhcp: { enabled: true, start: 20, limit: 80, leasetime: '30m' },
          enabled: true,
        },
      ],
    })
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('iot'))
    const dialog = await screen.findByRole('dialog')
    fireEvent.change(within(dialog).getByLabelText('Address'), {
      target: { value: '10.0.20.0/24' },
    })
    expect((await within(dialog).findByRole('alert')).textContent ?? '').toMatch(
      /subnet address, not a usable gateway/i,
    )
    expect((within(dialog).getByText('Save').closest('button') as HTMLButtonElement).disabled).toBe(true)

    fireEvent.change(within(dialog).getByLabelText('Address'), {
      target: { value: '10.0.20.255/24' },
    })
    expect((await within(dialog).findByRole('alert')).textContent ?? '').toMatch(
      /broadcast address, not a usable gateway/i,
    )
  })

  // An address that is not a CIDR gets a VLAN and no addressing, which is a
  // silent half-network. Say so where it is typed.
  it('says what is wrong with an address that is not in CIDR form', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 2,
          name: 'iot',
          vlan: 4,
          cidr: '10.0.4.1/24',
          zone: 'iot',
          enabled: true,
        },
      ],
    })
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('iot'))
    const dialog = await screen.findByRole('dialog')
    fireEvent.change(within(dialog).getByLabelText('Address'), {
      target: { value: '10.0.4.1' },
    })
    await waitFor(() => expect(dialog.textContent ?? '').toMatch(/not an IPv4 network in CIDR form/i))
    // And it says what to type, not just what is wrong.
    expect(dialog.textContent ?? '').toMatch(/Saving is blocked/)
    expect(dialog.textContent ?? '').toMatch(/Write it as address\/prefix/)
    expect(dialog.textContent ?? '').toMatch(/10\.0\.4\.1\/24/)
    expect((within(dialog).getByText('Save').closest('button') as HTMLButtonElement).disabled).toBe(true)
  })

  // An unchanged zone must not fire a write on every blur.
  it('does not save a zone that was not changed', async () => {
    api.site.mockResolvedValue({
      ...base,
      wlans: [],
      networks: [
        {
          id: 2,
          name: 'iot',
          vlan: 2,
          cidr: '10.0.2.1/24',
          zone: 'iot',
          enabled: true,
        },
      ],
    })
    render(<Settings devices={[]} />)
    const field = await screen.findByLabelText('Firewall zone for iot')
    fireEvent.blur(field)
    await waitFor(() => expect(screen.getByLabelText('Firewall zone for iot')).toBeTruthy())
    expect(api.saveNetwork).not.toHaveBeenCalled()
  })
})

describe('Devices — BSS provenance', () => {
  beforeEach(() => {
    api.stats.mockRejectedValue(new Error('none'))
  })

  // A BSS the detail response does not mention must not render as one we
  // manage. The list is painted from the live frame while provenance comes
  // from the REST detail refreshed every 30s, so a missing entry is reachable
  // with no device quirk at all: on a freshly adopted device, or for 30s after
  // a daemon restart, every foreign SSID read as managed.
  it('treats a BSS with no provenance entry as unknown, not as ours', async () => {
    const dev = {
      id: 1,
      mac: 'aa:bb:cc:dd:ee:01',
      name: 'ap-1',
      host: '192.168.1.1',
      role: 'ap',
      adopted: true,
      online: true,
      class: 'A',
    }
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: true,
      // phy1-ap0 is deliberately ABSENT here while the live frame carries it.
      broadcasting: [
        {
          ssid: 'fixture-roam',
          iface: 'phy0-ap1',
          section: 'oowrt_wlan1_radio0',
          origin: 'ours',
        },
      ],
    })
    api.deviceSeries.mockResolvedValue({ series: {} })
    api.overhead.mockRejectedValue(new Error('none'))

    const { Devices } = await import('./Devices')
    render(<Devices devices={[{ ...dev, quiesced: false } as never]} onAdopt={() => {}} onChanged={() => {}} />)
    fireEvent.click(screen.getByText('ap-1'))
    await waitFor(() => expect(screen.getByText('Re-probe capabilities')).toBeTruthy())

    // The live frame carries BOTH BSSes; the detail response knows only one.
    await act(async () => {
      pushLive({
        type: 'stats',
        device_id: 1,
        ts: Math.floor(Date.now() / 1000),
        tier: 'focused',
        uptime: 3600,
        load1: 0.1,
        poll_ms: 120,
        clients: 0,
        degraded: 0,
        aps: [
          {
            iface: 'phy0-ap1',
            ssid: 'fixture-roam',
            channel: 36,
            freq: 5180,
            clients: 0,
          },
          {
            iface: 'phy1-ap0',
            ssid: 'somebody-elses',
            channel: 1,
            freq: 2412,
            clients: 0,
          },
        ],
        stations: [],
      })
    })

    await waitFor(() => expect(screen.getByText('somebody-elses')).toBeTruthy())
    expect(screen.getByText('Reported enabled BSSs')).toBeTruthy()
    expect(screen.getByText(/not an independent on-air scan/i)).toBeTruthy()

    // The unknown one must carry the marker. Without it the row renders exactly
    // like the managed BSS above it, which is the bug.
    const marks = document.querySelectorAll('[title*="not established"]')
    if (marks.length === 0) {
      throw new Error('a BSS with no provenance entry rendered bare, identically to one ' + 'oonfeeWRT manages')
    }
  })

  it('labels the note that records why a foreign network stays unmanaged', async () => {
    const dev = {
      id: 1,
      mac: 'aa:bb:cc:dd:ee:01',
      name: 'ap-1',
      host: '192.168.1.1',
      role: 'ap',
      adopted: true,
      online: true,
      class: 'A',
    }
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: true,
      broadcasting: [
        {
          ssid: 'guest-old',
          iface: 'phy0-ap0',
          section: 'guest_old',
          origin: 'foreign',
          brief: {
            section: 'guest_old',
            ssid: 'guest-old',
            iface: 'phy0-ap0',
            safe_to_disable: false,
            refusal: 'Leave this operator-owned network unchanged.',
          },
        },
      ],
    })
    api.deviceSeries.mockResolvedValue({ series: {} })
    api.overhead.mockRejectedValue(new Error('none'))
    render(<Devices devices={[{ ...dev, quiesced: false } as never]} />)
    fireEvent.click(screen.getByText('ap-1'))
    await waitFor(() => expect(screen.getByText('Re-probe capabilities')).toBeTruthy())
    act(() =>
      pushLive({
        type: 'stats',
        device_id: 1,
        ts: Math.floor(Date.now() / 1000),
        tier: 'focused',
        uptime: 1,
        load1: 0,
        poll_ms: 1,
        clients: 0,
        degraded: 0,
        aps: [
          {
            iface: 'phy0-ap0',
            ssid: 'guest-old',
            channel: 1,
            freq: 2412,
            clients: 0,
          },
        ],
        stations: [],
      }),
    )

    fireEvent.click(await screen.findByText('What would it take to manage this?'))
    expect(screen.getByLabelText('Reason for leaving guest-old unmanaged')).toBeTruthy()
  })
})

describe('Devices — poll degradation', () => {
  const dev = {
    id: 5,
    mac: 'aa:bb:cc:dd:ee:05',
    name: 'degraded-ap',
    host: '192.168.1.5',
    role: 'ap',
    adopted: true,
    online: true,
    class: 'A',
  }

  function openWith(degraded: unknown[]) {
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: false,
      owned_sections_known: true,
      degraded,
    })
    api.deviceSeries.mockResolvedValue({ series: {} })
    api.overhead.mockRejectedValue(new Error('none'))
    render(<Devices devices={[{ ...dev, quiesced: false } as never]} onAdopt={() => {}} onChanged={() => {}} />)
    fireEvent.click(screen.getByText('degraded-ap'))
  }

  it('separates standing permission limits from a transient transport failure', async () => {
    openWith([
      {
        call: 'luci-rpc.getWirelessDevices',
        error: 'Permission denied',
        cause: 'permission',
        status: { code: 6, name: 'PERMISSION_DENIED' },
        permanent: true,
        costs: 'mesh peers cannot be separated from clients',
      },
      {
        call: 'iwinfo.survey',
        error: 'timed out',
        cause: 'transport',
        status: { code: 7, name: 'TIMEOUT' },
        permanent: false,
        costs: 'channel utilization is unavailable',
      },
    ])

    expect(await screen.findByText('Permission or device limits')).toBeTruthy()
    expect(screen.getByText('Current poll failures')).toBeTruthy()
    expect(screen.getByText(/ubus PERMISSION_DENIED \(6\)/)).toBeTruthy()
    expect(screen.getByText(/ubus TIMEOUT \(7\)/)).toBeTruthy()
    expect(screen.getByText(/These are standing limits/)).toBeTruthy()
    expect(screen.getByRole('note').textContent).toMatch(/latest poll, not a confirmed/)
  })

  it('does not call a non-retryable protocol failure a device limitation', async () => {
    openWith([
      {
        call: 'iwinfo.survey',
        error: 'malformed response',
        cause: 'protocol',
        permanent: true,
      },
    ])

    expect(await screen.findByText('Current poll failures')).toBeTruthy()
    expect(screen.queryByText('Permission or device limits')).toBeNull()
    expect(screen.queryByText(/These are standing limits/)).toBeNull()
    expect(screen.getByText(/Cause: protocol/)).toBeTruthy()
  })
})

describe('Unadopt', () => {
  const dev = {
    id: 4,
    mac: 'aa:bb:cc:dd:ee:04',
    name: 'ap-c6',
    host: '192.168.1.2',
    role: 'ap',
    adopted: true,
    online: true,
  }

  // Un-adopt is the most destructive thing the controller does and the one
  // operation with NO rollback armed — yet it reported a count, after the
  // fact, while the safer apply path got a full preview and a confirmation.
  it('names the sections it will revert, before anything is done', async () => {
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: false,
      owned_sections_known: true,
      owned_sections: ['wireless.oowrt_wlan1_radio0', 'wireless.oowrt_wlan1_radio1'],
    })
    const { Unadopt } = await import('./Unadopt')
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={() => {}} onCancel={() => {}} />)

    await waitFor(() => expect(screen.getByText('wireless.oowrt_wlan1_radio0')).toBeTruthy())
    expect(screen.getAllByText('wireless.oowrt_wlan1_radio1').length).toBeGreaterThan(0)
  })

  it('distinguishes a known-empty ownership ledger from an unreadable one', async () => {
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: false,
      owned_sections_known: true,
    })
    const { Unadopt } = await import('./Unadopt')
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={() => {}} onCancel={() => {}} />)

    await waitFor(() => expect(screen.getByText(/has no recorded sections/)).toBeTruthy())
    fireEvent.click(screen.getByText(/reverts the sections above/))
    for (const label of ['Remove completely', 'Revert config only']) {
      const button = screen.getByText(label).closest('button')!
      if (button.disabled) {
        throw new Error(`${label} stayed disabled for a known-empty ledger`)
      }
    }
  })

  // A list that could not be read must not render as an empty one — that would
  // say "this controller wrote nothing here" about a device it may own plenty
  // of, immediately before an irreversible step.
  it('says when the section list could not be read', async () => {
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: false,
      owned_sections_known: false,
    })
    const { Unadopt } = await import('./Unadopt')
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={() => {}} onCancel={() => {}} />)

    await waitFor(() => expect(screen.getByText(/could not be read/)).toBeTruthy())
    expect(screen.getByText(/not the same as owning none/)).toBeTruthy()

    fireEvent.click(screen.getByText(/reverts the sections above/))
    for (const label of ['Remove completely', 'Revert config only']) {
      const button = screen.getByText(label).closest('button')!
      if (!button.disabled) {
        throw new Error(`${label} was enabled with an unreadable ownership ledger`)
      }
    }
  })

  // The same speed bump the apply path has, on the operation that needs it
  // more. Both destructive buttons stay disabled until it is ticked.
  it('will not remove anything until the operator confirms', async () => {
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: false,
      owned_sections_known: true,
      owned_sections: ['wireless.oowrt_wlan1_radio0'],
    })
    const { Unadopt } = await import('./Unadopt')
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={() => {}} onCancel={() => {}} />)
    await waitFor(() => expect(screen.getByText('wireless.oowrt_wlan1_radio0')).toBeTruthy())

    const remove = screen.getByText('Remove completely').closest('button')!
    const revert = screen.getByText('Revert config only').closest('button')!
    if (!remove.disabled || !revert.disabled) {
      throw new Error('a destructive button was enabled before any confirmation')
    }

    fireEvent.click(screen.getByText(/I understand/))
    await waitFor(() => {
      const r = screen.getByText('Remove completely').closest('button')!
      if (r.disabled) throw new Error('still disabled after confirming')
    })
  })

  it('passes an optional SSH key to complete removal and clears it afterwards', async () => {
    const key = '-----BEGIN OPENSSH PRIVATE KEY-----\nunadopt-key-sentinel\n-----END OPENSSH PRIVATE KEY-----'
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: false,
      owned_sections_known: true,
      owned_sections: ['wireless.oowrt_wlan1_radio0'],
    })
    api.unadopt.mockResolvedValue({
      removed_from_inventory: true,
      footprint_remains: false,
      reverted_sections: 1,
      login_removed: true,
      acl_removed: true,
      needs_operator_credential: false,
      errors: [],
    })
    const { Unadopt } = await import('./Unadopt')
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={() => {}} onCancel={() => {}} />)

    await waitFor(() => expect(screen.getByText('wireless.oowrt_wlan1_radio0')).toBeTruthy())
    fireEvent.change(screen.getByLabelText('Password'), {
      target: { value: 'router-password' },
    })
    fireEvent.change(screen.getByLabelText('SSH private key (optional)'), {
      target: { value: key },
    })
    fireEvent.click(screen.getByText(/reverts the sections above/))
    fireEvent.click(screen.getByText('Remove completely'))

    await waitFor(() => expect(api.unadopt).toHaveBeenCalledTimes(1))
    expect(api.unadopt.mock.calls[0][1]).toMatchObject({
      username: 'root',
      password: 'router-password',
      private_key: key,
    })
    await waitFor(() => expect(screen.getByText(/was removed/)).toBeTruthy())
    expect(screen.queryByDisplayValue(key)).toBeNull()
  })

  // Reaches the form, confirms, and presses the destructive button.
  async function attemptRemoval() {
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: false,
      owned_sections_known: true,
      owned_sections: ['wireless.oowrt_wlan1_radio0'],
    })
    const { Unadopt } = await import('./Unadopt')
    const onDone = vi.fn()
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={onDone} onCancel={() => {}} />)
    await waitFor(() => expect(screen.getByText('wireless.oowrt_wlan1_radio0')).toBeTruthy())
    fireEvent.click(screen.getByText(/reverts the sections above/))
    await waitFor(() => {
      const r = screen.getByText('Remove completely').closest('button')!
      if (r.disabled) throw new Error('still disabled after confirming')
    })
    await act(async () => {
      fireEvent.click(screen.getByText('Remove completely'))
    })
    return onDone
  }

  // The API has taken `force` since un-adopt was written, and no screen ever
  // sent it. So a device that cannot be reached — dead hardware, a reflash, a
  // lost administrator password, and now a refused host key — could never leave
  // the inventory: it stayed listed, polled and counted forever, and the only
  // way out was a hand-written API call.
  it('offers a way out when the device cannot be reached at all', async () => {
    api.unadopt.mockRejectedValueOnce(new Error("the device's SSH host key changed"))
    await attemptRemoval()

    await waitFor(() => expect(screen.getByText(/SSH host key changed/)).toBeTruthy())
    const forceBtn = screen.getByText('Remove from the inventory anyway').closest('button')!
    if (!forceBtn.disabled) {
      throw new Error('forced removal was available without its own confirmation')
    }

    api.unadopt.mockResolvedValueOnce({
      removed_from_inventory: true,
      footprint_remains: true,
      reverted_sections: 0,
      login_removed: false,
      acl_removed: false,
      residue: ['/usr/share/rpcd/acl.d/oonfeewrt.json'],
      errors: ['removal was forced, so the footprint was NOT removed'],
    })
    fireEvent.click(screen.getByText(/footprint stays on the device/))
    await waitFor(() => {
      const b = screen.getByText('Remove from the inventory anyway').closest('button')!
      if (b.disabled) throw new Error('still disabled after confirming')
    })
    fireEvent.click(screen.getByText('Remove from the inventory anyway'))

    await waitFor(() => expect(api.unadopt).toHaveBeenCalledTimes(2))
    const [id, req] = api.unadopt.mock.calls[1]
    expect(id).toBe(4)
    expect(req.force).toBe(true)
    // The credential goes with it. The daemon still TRIES phase 2 and only
    // skips it when the connection fails, so a device that turns out to be
    // reachable is cleaned properly rather than abandoned on our say-so.
    expect(req.username).toBe('root')
  })

  it('never calls a forced removal clean when managed config remains', async () => {
    api.unadopt.mockResolvedValueOnce({
      removed_from_inventory: true,
      reverted_sections: 1,
      config_revert_complete: false,
      config_remains: ['wireless.oowrt_wlan1_radio1'],
      login_removed: true,
      acl_removed: true,
      footprint_remains: false,
      needs_operator_credential: false,
      residue: ['config section wireless.oowrt_wlan1_radio1'],
      cleanup_commands: ['uci -q delete wireless.oowrt_wlan1_radio1', 'uci commit wireless'],
    })
    await attemptRemoval()

    await waitFor(() => expect(screen.getByText(/configuration or the controller footprint remains/i)).toBeTruthy())
    expect(screen.queryByText(/Nothing of ours is left/)).toBeNull()
    expect(screen.getByText('wireless.oowrt_wlan1_radio1')).toBeTruthy()
    expect(screen.getByText(/Configuration hand-back was not proved complete/)).toBeTruthy()
  })

  // The speed bump has to be re-earned. Someone who ticks the forced-removal
  // confirmation, thinks better of it, and retries with a corrected password was
  // one click from a forced removal the instant the second attempt failed —
  // which is the confirmation gone at exactly the point it exists for.
  it('makes every attempt re-confirm a forced removal', async () => {
    api.unadopt.mockRejectedValueOnce(new Error('first failure'))
    await attemptRemoval()

    await waitFor(() => expect(screen.getByText('Remove from the inventory anyway')).toBeTruthy())
    fireEvent.click(screen.getByText(/footprint stays on the device/))
    await waitFor(() => {
      const b = screen.getByText('Remove from the inventory anyway').closest('button')!
      if (b.disabled) throw new Error('still disabled after confirming')
    })

    // Second thoughts: retry the ordinary path instead. It fails too.
    api.unadopt.mockRejectedValueOnce(new Error('second failure'))
    fireEvent.click(screen.getByText('Remove completely'))
    await waitFor(() => expect(screen.getByText(/second failure/)).toBeTruthy())

    const b = screen.getByText('Remove from the inventory anyway').closest('button')!
    if (!b.disabled) {
      throw new Error(
        'the forced-removal confirmation survived a second attempt, so the ' +
          'destructive action is one click away without being re-confirmed',
      )
    }
  })

  // A 502 can carry the whole report, and the worst case is the forced removal
  // whose phase 2 connected and then could not commit: the row is already gone
  // and the residue list is the only surviving record of what is installed. The
  // panel accepted a body only on 409, so that case fell through to a bare
  // error banner and the list was destroyed.
  it('renders a report that arrives with an error status', async () => {
    const err = Object.assign(new ApiError(502, 'stub'), {
      status: 502,
      message: 'adoption: un-adopt completed with 1 error(s)',
      body: {
        removed_from_inventory: true,
        footprint_remains: true,
        reverted_sections: 1,
        login_removed: false,
        acl_removed: false,
        needs_operator_credential: false,
        residue: ['/usr/share/rpcd/acl.d/oonfeewrt.json'],
        cleanup_commands: [
          'uci -q delete rpcd.oonfeewrt',
          'uci commit rpcd',
          "uci -q get rpcd.oonfeewrt >/dev/null 2>&1 && echo 'ERROR: login still present' || echo 'login gone'",
          "rm -f '/usr/share/rpcd/acl.d/oonfeewrt.json'",
          "[ ! -e '/usr/share/rpcd/acl.d/oonfeewrt.json' ] && echo 'ACL gone' || echo 'ERROR: ACL still present'",
        ],
        errors: ['uci commit rpcd: Read-only file system'],
        error: 'adoption: un-adopt completed with 1 error(s)',
      },
    })
    api.unadopt.mockRejectedValueOnce(err)
    await attemptRemoval()

    await waitFor(() => expect(screen.getByText('/usr/share/rpcd/acl.d/oonfeewrt.json')).toBeTruthy())
    expect(screen.getByText(/Read-only file system/)).toBeTruthy()
    expect(screen.getByText(/uci -q delete rpcd\.oonfeewrt/)).toBeTruthy()
    expect(screen.getByText(/rm -f '.*oonfeewrt\.json'/)).toBeTruthy()
    expect(screen.getByText(/ERROR: login still present/)).toBeTruthy()
    expect(screen.getByText(/ERROR: ACL still present/)).toBeTruthy()
  })

  // The fleet list has to learn about the removal however this panel is left.
  //
  // onDone refreshes the fleet AND closes; it used to fire the instant the
  // request returned, which is what threw the report away. Moving it to Close
  // made the slide-over's own × a second exit that refreshes nothing, so a
  // removed device stayed in the table — a controller listing a router it had
  // just deleted. The × lives outside this component, so unmounting is the only
  // place it can be caught.
  it('tells the fleet list about a removal even when dismissed by unmounting', async () => {
    api.unadopt.mockResolvedValueOnce({
      removed_from_inventory: true,
      footprint_remains: false,
      reverted_sections: 1,
      login_removed: true,
      acl_removed: true,
      needs_operator_credential: false,
      errors: [],
    })
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: false,
      owned_sections_known: true,
      owned_sections: ['wireless.oowrt_wlan1_radio0'],
    })
    const { Unadopt } = await import('./Unadopt')
    const onDone = vi.fn()
    const { unmount } = render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={onDone} onCancel={() => {}} />)
    await waitFor(() => expect(screen.getByText('wireless.oowrt_wlan1_radio0')).toBeTruthy())
    fireEvent.click(screen.getByText(/reverts the sections above/))
    await waitFor(() => {
      const r = screen.getByText('Remove completely').closest('button')!
      if (r.disabled) throw new Error('still disabled after confirming')
    })
    fireEvent.click(screen.getByText('Remove completely'))
    await waitFor(() => expect(screen.getByText(/was removed/)).toBeTruthy())
    expect(onDone).not.toHaveBeenCalled()

    // Dismissed from outside — the slide-over's ×, not our Close button.
    unmount()
    expect(onDone).toHaveBeenCalledTimes(1)
  })

  // And exactly once when the button is used, rather than again on unmount.
  //
  // onDone must actually unmount here, the way it does in the app: it calls
  // setOpenID(null), which tears the slide-over down and runs the cleanup. A
  // spy that only records leaves the component mounted, so the cleanup never
  // fires and the double-refresh this asserts against cannot happen — the test
  // would pass whether or not the flag is cleared.
  it('refreshes the fleet once, not twice, when Close is used', async () => {
    api.unadopt.mockResolvedValueOnce({
      removed_from_inventory: true,
      footprint_remains: false,
      reverted_sections: 1,
      login_removed: true,
      acl_removed: true,
      needs_operator_credential: false,
      errors: [],
    })
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null,
      interfaces: [],
      radios: [],
      stations: [],
      broadcast_known: false,
      owned_sections_known: true,
      owned_sections: ['wireless.oowrt_wlan1_radio0'],
    })
    const { Unadopt } = await import('./Unadopt')
    let teardown = () => {}
    const onDone = vi.fn(() => teardown())
    const r = render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={onDone} onCancel={() => {}} />)
    teardown = r.unmount

    await waitFor(() => expect(screen.getByText('wireless.oowrt_wlan1_radio0')).toBeTruthy())
    fireEvent.click(screen.getByText(/reverts the sections above/))
    await waitFor(() => {
      const b = screen.getByText('Remove completely').closest('button')!
      if (b.disabled) throw new Error('still disabled after confirming')
    })
    fireEvent.click(screen.getByText('Remove completely'))
    await waitFor(() => expect(screen.getByText(/was removed/)).toBeTruthy())

    fireEvent.click(screen.getByText('Close'))
    expect(onDone).toHaveBeenCalledTimes(1)
  })

  // A report with NO residue is still a report.
  //
  // Real case: phase 1 fails to revert one section, phase 2 cleans up fine, so
  // the row is removed and nothing is left on the device — an error, an empty
  // residue, and the only record of which section was not handed back. Keying
  // the recognition on an omittable field would read this as a plain error and
  // discard it, which is why it is keyed on the one field the Go type always
  // emits.
  it('recognises a report that carries no residue', async () => {
    const err = Object.assign(new ApiError(502, 'stub'), {
      status: 502,
      message: 'adoption: un-adopt completed with 1 error(s)',
      body: {
        removed_from_inventory: true,
        footprint_remains: false,
        reverted_sections: 1,
        login_removed: true,
        acl_removed: true,
        needs_operator_credential: false,
        errors: ['revert wireless.oowrt_wlan1_radio1: Permission denied'],
        error: 'adoption: un-adopt completed with 1 error(s)',
      },
    })
    api.unadopt.mockRejectedValueOnce(err)
    await attemptRemoval()

    await waitFor(() => expect(screen.getByText(/Permission denied/)).toBeTruthy())
    expect(screen.getByText(/Nothing of ours is left on it/)).toBeTruthy()
  })

  // "Still in the inventory" and "needs the administrator credential" are not
  // the same statement, and one banner made both. A phase-2 failure WITH a
  // credential supplied was described as needing one, sending the operator off
  // to re-type a password that was already correct.
  it('does not blame a missing credential for a phase-2 failure', async () => {
    const err = Object.assign(new ApiError(502, 'stub'), {
      status: 502,
      message: 'boom',
      body: {
        removed_from_inventory: false,
        footprint_remains: true,
        reverted_sections: 2,
        login_removed: false,
        acl_removed: false,
        needs_operator_credential: false,
        residue: ['/usr/share/rpcd/acl.d/oonfeewrt.json'],
        errors: ['uci commit rpcd: Read-only file system'],
        error: 'the removal did not complete',
      },
    })
    api.unadopt.mockRejectedValueOnce(err)
    await attemptRemoval()

    await waitFor(() => expect(screen.getByText(/did not finish/)).toBeTruthy())
    expect(screen.queryByText(/needs the device's administrator credential/)).toBeNull()
  })

  // The report is the LAST copy of what is still installed on a device whose
  // row has just been deleted. It was rendered and discarded in the same tick:
  // onDone ran the moment the request returned, and it unmounts the panel.
  it('keeps the report on screen after the row is gone', async () => {
    api.unadopt.mockResolvedValueOnce({
      removed_from_inventory: true,
      footprint_remains: true,
      reverted_sections: 2,
      login_removed: false,
      acl_removed: false,
      residue: ['/usr/share/rpcd/acl.d/oonfeewrt.json'],
      errors: [],
    })
    const onDone = await attemptRemoval()

    await waitFor(() => expect(screen.getByText('/usr/share/rpcd/acl.d/oonfeewrt.json')).toBeTruthy())
    expect(onDone).not.toHaveBeenCalled()

    // "Supply the credential and try again" is advice only while there is
    // still a row to try against. There is not, and sending someone back to a
    // device the controller has forgotten is how a footprint stays behind.
    expect(screen.getByText(/last time the controller can tell you/)).toBeTruthy()
    expect(screen.queryByText(/try again/)).toBeNull()

    // Forcing is not offered once the row is actually gone — there is nothing
    // left to force.
    expect(screen.queryByText('Remove from the inventory anyway')).toBeNull()

    fireEvent.click(screen.getByText('Close'))
    expect(onDone).toHaveBeenCalled()
  })
})

describe('Logs', () => {
  it('distinguishes observed-empty router logs from missing coverage', async () => {
    api.devices.mockResolvedValue({ devices: [] })
    api.events.mockResolvedValueOnce({
      events: [],
      total: 0,
      limit: 100,
      scope: 'general',
      next_before: null,
      facets: { category: [], severity: [] },
      coverage: {
        complete: false,
        expected_devices: 1,
        observed_devices: 0,
        gaps: ['router log coverage has not been observed on AP one'],
      },
    })
    const { unmount } = render(<Logs />)
    expectSinglePageHeading('Logs')
    expect(screen.getByText(/Controller, router, and audit events/)).toBeTruthy()
    const coverageNotice = await screen.findByRole('group', { name: 'Warning: Router log coverage' })
    expect(within(coverageNotice).getByText(/Router log coverage is incomplete/)).toBeTruthy()
    expect(within(coverageNotice).getByText(/an empty result is not proven/)).toBeTruthy()
    const coverageToggle = within(coverageNotice).getByText('More information about log coverage')
    const coverageDetails = coverageToggle.closest('details') as HTMLDetailsElement
    expect(coverageDetails.open).toBe(false)
    fireEvent.click(coverageToggle)
    expect(coverageDetails.open).toBe(true)
    expect(within(coverageNotice).getByText(/has not been observed on AP one/)).toBeTruthy()
    expect(screen.queryByText('No general events were observed.')).toBeNull()
    unmount()

    api.events.mockResolvedValueOnce({
      events: [],
      total: 0,
      limit: 100,
      scope: 'general',
      next_before: null,
      facets: { category: [], severity: [] },
      coverage: {
        complete: true,
        expected_devices: 1,
        observed_devices: 1,
        gaps: [],
      },
    })
    render(<Logs />)
    expect(await screen.findByText('No general events were observed.')).toBeTruthy()
    expect(screen.queryByText(/Router log coverage is incomplete/)).toBeNull()
  })

  const ev = (over: Record<string, unknown> = {}) => ({
    ID: 1,
    TS: 1755400000,
    DeviceID: null,
    Category: 'device',
    Severity: 'info',
    Event: 'device.reachable',
    Detail: {},
    Source: 'controller',
    SourceID: '',
    SourceBoot: '',
    IngestedAt: 1755400000000,
    ClientMAC: '',
    Action: '',
    Direction: '',
    InIface: '',
    OutIface: '',
    SrcIP: '',
    DstIP: '',
    SrcPort: null,
    DstPort: null,
    ZoneIn: '',
    ZoneOut: '',
    PolicyID: null,
    ...over,
  })

  it('discloses router clock skew instead of silently presenting misleading event times', async () => {
    api.devices.mockResolvedValue({ devices: [] })
    api.events.mockResolvedValue({
      events: [ev({ Source: 'openwrt-logd', IngestedAt: 1755486400000 })],
      total: 1,
      limit: 100,
      scope: 'general',
      next_before: null,
      facets: { category: [], severity: [] },
      coverage: {
        complete: true,
        expected_devices: 1,
        observed_devices: 1,
        gaps: [],
      },
    })

    render(<Logs />)

    const clockNotice = await screen.findByRole('group', { name: 'Warning: Router clock' })
    expect(within(clockNotice).getByRole('status').textContent ?? '').toMatch(
      /Router event time differs.*24 hours.*ordered by their router source time/i,
    )
    const toggle = within(clockNotice).getByText('More information about event time')
    const details = toggle.closest('details') as HTMLDetailsElement
    expect(details.open).toBe(false)
    fireEvent.click(toggle)
    expect(details.open).toBe(true)
    expect(within(clockNotice).getByText(/Check the router clock and NTP/)).toBeTruthy()
  })

  // Every device event carries a device_id, the API has always returned it, and
  // the grid had no column for it — not hidden, absent. So "device.unreachable"
  // never said which device. On a two-device lab you can guess; on a fleet the
  // row is useless, and answering "what happened to what" is the entire job of
  // an event log.
  it('names the device an event is about', async () => {
    api.devices.mockResolvedValue({
      devices: [{ id: 7, name: 'hallway-ap', adopted: true }],
    })
    api.events.mockResolvedValue({
      events: [
        ev({
          ID: 1,
          DeviceID: 7,
          Event: 'device.unreachable',
          Severity: 'warning',
        }),
        ev({ ID: 2, DeviceID: null, Event: 'auth.login', Category: 'audit' }),
      ],
      total: 2,
      limit: 100,
      offset: 0,
      facets: { category: [], severity: [] },
    })
    render(<Logs />)
    await waitFor(() => expect(screen.getByText('device.unreachable')).toBeTruthy())
    expect(screen.getByText('hallway-ap')).toBeTruthy()
  })

  // A whole serialised array of apply omissions used to land in one cell, each
  // with a full sentence of prose. It ran off the screen and forced the table
  // into a horizontal scrollbar. Counting is the honest summary: it says there
  // is something to look at without pretending the cell can hold it.
  it('summarises a list by counting it, never by dumping it', async () => {
    api.devices.mockResolvedValue({ devices: [] })
    api.events.mockResolvedValue({
      events: [
        ev({
          Event: 'config.apply',
          Detail: {
            omissions: [
              {
                WLAN: 'lan',
                Reason:
                  'VLAN 1 and untagged traffic are the device’s existing LAN, which oonfeeWRT does not own and will not rewrite.',
              },
              {
                WLAN: 'lan',
                Reason: 'another long sentence that has no business being in a table cell at all',
              },
            ],
          },
        }),
      ],
      total: 1,
      limit: 100,
      offset: 0,
      facets: { category: [], severity: [] },
    })
    render(<Logs />)
    await waitFor(() => expect(screen.getByText('config.apply')).toBeTruthy())

    expect(screen.getByText(/omissions=2 items/)).toBeTruthy()
    // The prose must not be in the cell.
    expect(screen.queryByText(/will not rewrite/)).toBeNull()
  })

  it('explains and condenses the known IPv6 router-advertisement condition', async () => {
	api.devices.mockResolvedValue({ devices: [] })
	api.events.mockResolvedValue({
	  events: [ev({
		Event: 'openwrt.ipv6_ra_no_default_route',
		Severity: 'warning',
		Source: 'openwrt-logd',
		Detail: {
		  message: 'odhcpd[81]: No default route present, setting ra_lifetime to 0!',
		  priority: 28,
		  occurrences: 37,
		},
	  })],
	  total: 1,
	  limit: 100,
	  scope: 'general',
	  next_before: null,
	  facets: { category: [], severity: [] },
	  coverage: { complete: true, expected_devices: 1, observed_devices: 1, gaps: [] },
	})

	render(<Logs />)

	expect(await screen.findByText(/IPv6 router advertisements have no usable default route · 37 occurrences/)).toBeTruthy()
	const notice = screen.getByRole('group', { name: 'Warning: IPv6 router advertisements' })
	expect(within(notice).getByRole('status').textContent ?? '').toMatch(
	  /does not indicate an IPv4 outage/i,
	)
	const toggle = within(notice).getByText('More information about this IPv6 warning')
	const details = toggle.closest('details') as HTMLDetailsElement
	expect(details.open).toBe(false)
	fireEvent.click(toggle)
	expect(within(notice).getByText(/37 reported occurrences.*1 condition record/i)).toBeTruthy()
  })

  it('never renders an out-of-order response under newer filters', async () => {
    vi.useFakeTimers()
    api.devices.mockResolvedValue({ devices: [] })
    const initial = {
      events: [
        ev({ ID: 11, Event: 'routine.info', Severity: 'info' }),
        ev({ ID: 12, Event: 'current.error', Severity: 'error' }),
      ],
      total: 2,
      limit: 100,
      offset: 0,
      facets: {
        category: [{ value: 'device', count: 2 }],
        severity: [
          { value: 'info', count: 1 },
          { value: 'error', count: 1 },
        ],
      },
    }
    let resolveRefresh!: (page: typeof initial) => void
    let resolveError!: (page: typeof initial) => void
    const refresh = new Promise<typeof initial>((resolve) => {
      resolveRefresh = resolve
    })
    const error = new Promise<typeof initial>((resolve) => {
      resolveError = resolve
    })
    api.events.mockResolvedValueOnce(initial).mockReturnValueOnce(refresh).mockReturnValueOnce(error)

    let unmount = () => {}
    try {
      ;({ unmount } = render(<Logs />))
      await act(async () => {})
      expect(screen.getByText('routine.info')).toBeTruthy()

      // Start a same-query periodic refresh, then change the filter while that
      // older request is unresolved.
      await act(async () => {
        vi.advanceTimersByTime(10_000)
      })
      expect(api.events).toHaveBeenCalledTimes(2)

      fireEvent.click(screen.getByRole('button', { name: /^error\s+1$/i }))
      await act(async () => {})
      expect(api.events).toHaveBeenCalledTimes(3)
      // Neither rows nor facet counts from "All" are valid evidence for the
      // selected filter while its request is pending.
      expect(screen.queryByText('routine.info')).toBeNull()
      expect(screen.queryByRole('button', { name: /^device\s+2$/i })).toBeNull()
      expect(screen.getByText('Loading events…')).toBeTruthy()

      await act(async () => {
        resolveError({
          ...initial,
          events: [ev({ ID: 12, Event: 'current.error', Severity: 'error' })],
          total: 1,
          facets: {
            category: [{ value: 'device', count: 1 }],
            severity: initial.facets.severity,
          },
        })
      })
      expect(screen.getByText('current.error')).toBeTruthy()

      // This older response deliberately has the exact inconsistent shape seen
      // live: its count says one while its stale rows would draw two. It must not
      // be allowed to replace the completed error query.
      await act(async () => {
        resolveRefresh({
          ...initial,
          events: [
            ev({ ID: 11, Event: 'stale.info', Severity: 'info' }),
            ev({ ID: 13, Event: 'stale.extra', Severity: 'info' }),
          ],
          total: 1,
        })
      })
      expect(screen.queryByText('stale.info')).toBeNull()
      expect(screen.queryByText('stale.extra')).toBeNull()
      expect(screen.getByText('Events (1)')).toBeTruthy()
      expect(screen.getAllByRole('row').filter((row) => row.hasAttribute('data-row'))).toHaveLength(1)
    } finally {
      unmount()
      vi.useRealTimers()
    }
  })

  it('lets one slow refresh finish instead of invalidating it every 10 seconds', async () => {
    vi.useFakeTimers()
    api.devices.mockResolvedValue({ devices: [] })
    const page = {
      events: [ev({ ID: 21, Event: 'slow.but.valid' })],
      total: 1,
      limit: 100,
      offset: 0,
      facets: { category: [], severity: [] },
    }
    let resolvePage!: (value: typeof page) => void
    api.events.mockReturnValue(
      new Promise<typeof page>((resolve) => {
        resolvePage = resolve
      }),
    )

    let unmount = () => {}
    try {
      ;({ unmount } = render(<Logs />))
      await act(async () => {})
      expect(api.events).toHaveBeenCalledTimes(1)

      await act(async () => {
        vi.advanceTimersByTime(30_000)
      })
      expect(api.events).toHaveBeenCalledTimes(1)

      await act(async () => {
        resolvePage(page)
      })
      expect(screen.getByText('slow.but.valid')).toBeTruthy()
    } finally {
      unmount()
      vi.useRealTimers()
    }
  })

  it('uses scoped keyset pages and opens exact enriched event detail', async () => {
    api.devices.mockResolvedValue({
      devices: [{ id: 7, name: 'hallway-ap', adopted: true }],
    })
    const newest = ev({
      ID: 31,
      DeviceID: 7,
      Event: 'client.connect',
      Category: 'client',
      Source: 'openwrt-logd',
      SourceID: '87',
      SourceBoot: 'boot:123',
      ClientMAC: 'aa:bb:cc:dd:ee:ff',
      Action: 'connect',
      InIface: 'phy0-ap0',
      SrcIP: '192.168.2.100',
      SrcPort: 5353,
      DstIP: '192.168.2.1',
      DstPort: 53,
      ZoneIn: 'guest',
      PolicyID: 4,
      Detail: { hostname: 'laptop', raw: 'sanitized' },
    })
    const older = ev({ ID: 30, TS: 1755399999, Event: 'older.general' })
    api.events
      .mockResolvedValueOnce({
        events: [newest],
        total: 2,
        limit: 100,
        scope: 'general',
        next_before: { ts: newest.TS, id: newest.ID },
        facets: { category: [], severity: [] },
      })
      .mockResolvedValueOnce({
        events: [older],
        total: 2,
        limit: 100,
        scope: 'general',
        next_before: null,
        facets: { category: [], severity: [] },
      })
      .mockResolvedValueOnce({
        events: [ev({ ID: 40, Category: 'audit', Event: 'auth.login' })],
        total: 1,
        limit: 100,
        scope: 'audit',
        next_before: null,
        facets: { category: [], severity: [] },
      })
    api.eventDetail.mockResolvedValue(newest)

    render(<Logs />)
    await waitFor(() => expect(screen.getByText('client.connect')).toBeTruthy())
    expect(api.events).toHaveBeenLastCalledWith(
      expect.objectContaining({
        scope: 'general',
        before: null,
      }),
    )

    fireEvent.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() => expect(screen.getByText('older.general')).toBeTruthy())
    expect(api.events).toHaveBeenLastCalledWith(
      expect.objectContaining({
        scope: 'general',
        before: { ts: newest.TS, id: newest.ID },
      }),
    )

    fireEvent.click(screen.getByRole('button', { name: 'Audit' }))
    await waitFor(() => expect(screen.getByText('auth.login')).toBeTruthy())
    expect(api.events).toHaveBeenLastCalledWith(
      expect.objectContaining({
        scope: 'audit',
        before: null,
      }),
    )

    api.events.mockResolvedValueOnce({
      events: [newest],
      total: 1,
      limit: 100,
      scope: 'general',
      next_before: null,
      facets: { category: [], severity: [] },
    })
    fireEvent.click(screen.getByRole('button', { name: 'General' }))
    await waitFor(() => expect(screen.getByText('client.connect')).toBeTruthy())
    const view = screen.getByRole('button', {
      name: /View event 31: client\.connect/,
    })
    fireEvent.click(view)
    await waitFor(() => expect(api.eventDetail).toHaveBeenCalledWith(31))
    const detail = await screen.findByRole('dialog', {
      name: /client.connect · event 31/,
    })
    await waitFor(() => expect(detail.contains(document.activeElement)).toBe(true))
    expect(screen.getAllByText('hallway-ap')).toHaveLength(2)
    expect(screen.getByText('openwrt-logd')).toBeTruthy()
    expect(screen.getByText('aa:bb:cc:dd:ee:ff')).toBeTruthy()
    expect(screen.getByText(/192\.168\.2\.100:5353.*192\.168\.2\.1:53/)).toBeTruthy()
    expect(screen.getByText(/"hostname": "laptop"/)).toBeTruthy()
    fireEvent.keyDown(window, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(document.activeElement).toBe(view)
  })
})

describe('Dashboard', () => {
  const speedPlanID = `sha256:${'a'.repeat(64)}`
  const data = {
    devices: { total: 2, online: 2, offline: 0, pending: 0, unknown: 0 },
    wireless_clients: 0,
    wireless_clients_complete: true,
    known_devices: 5,
    active_devices: 5,
    upstream_devices: 4,
    unscoped_devices: 3,
    gateway_uplinks: [{ device_id: 1, name: 'Gateway', state: 'up' }],
    focused_devices: 0,
    quiesced_devices: 0,
    series_count: 70,
    recent_events: [],
    recent_alert_events: [],
  }

  const topology = {
    at: Date.now() - 60_000,
    complete: true,
    truncated: false,
    gaps: [],
    nodes: [
      { id: 'synthetic:internet', kind: 'synthetic', name: 'Internet', synthetic: true },
      { id: 'device:02:00:00:00:00:01', kind: 'device', name: 'Gateway', device_id: 1, online: true, synthetic: false },
      { id: 'device:02:00:00:00:00:02', kind: 'device', name: 'Hall AP', device_id: 2, online: true, synthetic: false },
      { id: 'client:02:00:00:00:01:01', kind: 'client', name: 'Phone', online: true, synthetic: false },
    ],
    edges: [
      {
        id: 1, child_id: 'device:02:00:00:00:00:01', parent_id: 'synthetic:internet',
        parent_port: 'wan', medium: 'uplink', confidence: 'measured', valid_from: 1, last_seen: 1,
        evidence: [], ambiguities: [],
      },
      {
        id: 2, child_id: 'device:02:00:00:00:00:02', parent_id: 'device:02:00:00:00:00:01',
        parent_port: 'lan2', medium: 'wired', confidence: 'measured', valid_from: 1, last_seen: 1,
        evidence: [], ambiguities: [],
      },
      {
        id: 3, child_id: 'client:02:00:00:00:01:01', parent_id: 'device:02:00:00:00:00:02',
        parent_port: 'phy0-ap0', medium: 'wireless', confidence: 'inferred', valid_from: 1, last_seen: 1,
        evidence: [], ambiguities: [],
      },
    ],
    last_known_edges: [],
  }

  it('summarizes current topology evidence without turning placed clients into a fleet total', async () => {
    api.topology.mockResolvedValueOnce(topology)
    const onOpenTopology = vi.fn()
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} onOpenTopology={onOpenTopology} />)

    const summary = await screen.findByRole('region', { name: 'Network topology' })
    expect(within(summary).getByText('Complete coverage')).toBeTruthy()
    expect(within(summary).getByText('Managed devices').parentElement?.textContent).toContain('2')
    expect(within(summary).getByText('Active links').parentElement?.textContent).toContain('3')
    expect(within(summary).queryByText('Observed links')).toBeNull()
    expect(within(summary).getByText('Placed clients').parentElement?.textContent).toContain('1')
    expect(within(summary).getByLabelText('Internet to Gateway, uplink, wan, measured')).toBeTruthy()
    expect(within(summary).getByLabelText('Gateway to Hall AP, wired, lan2, measured')).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Open topology' }))
    expect(onOpenTopology).toHaveBeenCalledTimes(1)
  })

  it('bounds the compact infrastructure preview and names the omitted remainder', async () => {
    const extraNodes = [
      { id: 'device:02:00:00:00:00:03', kind: 'device', name: 'Office AP', device_id: 3, online: true, synthetic: false },
      { id: 'device:02:00:00:00:00:04', kind: 'device', name: 'Patio AP', device_id: 4, online: true, synthetic: false },
    ]
    api.topology.mockResolvedValueOnce({
      ...topology,
      nodes: [...topology.nodes, ...extraNodes],
      edges: [
        ...topology.edges,
        {
          ...topology.edges[1], id: 4, child_id: extraNodes[0].id,
          parent_port: 'lan3',
        },
        {
          ...topology.edges[1], id: 5, child_id: extraNodes[1].id,
          parent_port: 'lan4',
        },
      ],
    })
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)

    const summary = await screen.findByRole('region', { name: 'Network topology' })
    const links = within(summary).getByRole('list', { name: 'Active infrastructure links' })
    expect(within(links).getAllByRole('listitem')).toHaveLength(3)
    expect(within(summary).getByText(/Showing 3 of 4 infrastructure links/)).toBeTruthy()
  })

  it('does not count an unresolved device reference as managed inventory', async () => {
    api.topology.mockResolvedValueOnce({
      ...topology,
      nodes: [
        ...topology.nodes,
        { id: 'device:02:00:00:00:00:99', kind: 'device', name: 'Unresolved device', synthetic: false },
      ],
      edges: [
        ...topology.edges,
        {
          ...topology.edges[1], id: 9, child_id: 'device:02:00:00:00:00:99',
          parent_port: 'lan4', confidence: 'inferred',
        },
      ],
    })
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)

    const summary = await screen.findByRole('region', { name: 'Network topology' })
    expect(within(summary).getByText('Managed devices').parentElement?.textContent).toContain('2')
    expect(within(summary).getByText('Active links').parentElement?.textContent).toContain('4')
  })

  it('keeps partial, ambiguous and last-known topology evidence explicit', async () => {
    api.topology.mockResolvedValueOnce({
      ...topology,
      complete: false,
      gaps: ['device:2/private-source: unavailable', 'edge:2: parent is ambiguous'],
      edges: [{ ...topology.edges[1], confidence: 'ambiguous' }],
      last_known_edges: [{ ...topology.edges[1], id: 4, valid_to: topology.at - 1 }],
    })
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)

    const summary = await screen.findByRole('region', { name: 'Network topology' })
    expect(within(summary).getByText('Partial · 2 coverage issues')).toBeTruthy()
    expect(within(summary).getByText('Active links').parentElement?.textContent).toContain('1')
    expect(within(summary).getByText(/1 active link has ambiguous evidence/)).toBeTruthy()
    expect(within(summary).getByText(/1 last-known placement is excluded from active links/)).toBeTruthy()
    expect(within(summary).queryByText(/private-source/)).toBeNull()
  })

  it('isolates a topology failure and retries without hiding the rest of the Dashboard', async () => {
    api.topology
      .mockRejectedValueOnce(new Error('topology store unavailable'))
      .mockResolvedValueOnce(topology)
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)

    const topologyError = await screen.findByRole('group', { name: 'Warning: Topology summary' })
    expect(within(topologyError).getByRole('alert')).toBeTruthy()
    expect(screen.getByText('Fleet overview')).toBeTruthy()
    expect(screen.queryByText('Active links')).toBeNull()

    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('Complete coverage')).toBeTruthy()
    expect(screen.queryByRole('group', { name: 'Warning: Topology summary' })).toBeNull()
    expect(api.topology).toHaveBeenCalledTimes(2)
  })

  it('renders only the bounded warning/error feed and keeps legacy activity out', async () => {
    const rows = Array.from({ length: 9 }, (_, index) => ({
      ID: index + 1,
      TS: Date.now() / 1000 - index,
      Severity: index === 0 ? 'error' : 'warning',
      Event: `alert.${index}`,
    }))
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={{
      ...data,
      recent_events: [{ ID: 90, TS: 1, Severity: 'info', Event: 'routine.activity' }],
      recent_alert_events: [...rows, { ID: 91, TS: 1, Severity: 'info', Event: 'invalid.alert' }],
    } as never} />)

    expect(screen.getByText('Recent warnings and errors')).toBeTruthy()
    expect(screen.getByText('alert.0').parentElement?.textContent).toContain('error')
    expect(screen.getByText('alert.7')).toBeTruthy()
    expect(screen.queryByText('alert.8')).toBeNull()
    expect(screen.queryByText('routine.activity')).toBeNull()
    expect(screen.queryByText('invalid.alert')).toBeNull()
    expect(screen.getByText(/1 alert row had an unrecognized severity/)).toBeTruthy()
  })

  it('renders the coalesced IPv6 condition without hiding its warning severity', async () => {
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={{
      ...data,
      recent_alert_events: [{
        ID: 44,
        TS: Date.now() / 1000,
        Severity: 'warning',
        Event: 'openwrt.ipv6_ra_no_default_route',
        Detail: { occurrences: 220 },
      }],
    } as never} />)

    const row = screen.getByText(/IPv6 router advertisements have no usable default route/).parentElement
    expect(row?.textContent).toMatch(/warning.*220 occurrences/i)
  })

  it('distinguishes a confirmed-empty alert feed from unavailable evidence', async () => {
    const { Dashboard } = await import('./Dashboard')
    const { rerender } = render(<Dashboard data={data as never} />)
    expect(screen.getByText('No retained warning or error events.')).toBeTruthy()

    rerender(<Dashboard data={{ ...data, recent_alert_events: null } as never} />)
    expect(screen.getByRole('alert').textContent ?? '').toMatch(/feed is unavailable/i)
    expect(screen.queryByText('No retained warning or error events.')).toBeNull()
  })

  // A number under another number's label. This screen's own sibling code
  // states the rule — showing one thing labelled as another is how a dashboard
  // gets quietly distrusted — and this stat broke it: focused_devices, a count
  // of DEVICES, sat under "Focused polls".
  it('labels the focus stat for what it counts', async () => {
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)
    expectSinglePageHeading('Dashboard')

    expect(screen.getByText('Devices in focus')).toBeTruthy()
    expect(screen.queryByText('Focused polls')).toBeNull()
  })

  // Zero here is the normal, correct reading — focus is held by an open device
  // panel, and nobody reading the dashboard has one open. Without saying so, a
  // permanently-zero counter reads as broken.
  it('explains why the focus count is normally zero', async () => {
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)

    expect(screen.getByText(/no panel is open/)).toBeTruthy()
    expect(screen.getByText(/normally zero, and that is the honest answer/)).toBeTruthy()
  })

  // And it must still show a real count when there is one. 7 rather than 2:
  // the fleet counts on this screen are 2s and 5s, and an assertion that passes
  // by matching another stat's number is not an assertion about this one.
  it('shows the count when devices are in focus', async () => {
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={{ ...data, focused_devices: 7 } as never} />)

    expect(screen.getByText('7')).toBeTruthy()
    expect(screen.queryByText(/no panel is open/)).toBeNull()
  })

  it('defines wireless clients as the same scoped rows as Client Devices', async () => {
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={{ ...data, wireless_clients: 1 } as never} />)

    expect(screen.getByText('Wireless clients').parentElement?.textContent).toContain('1')
    expect(screen.getByText(/same count as Client Devices/)).toBeTruthy()
    expect(screen.getByText(/private MACs.*managed VLAN/)).toBeTruthy()
    expect(screen.getByText(/uplink-side and unplaced rows do not/)).toBeTruthy()
  })

  it('keeps count methodology compact until requested', async () => {
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)

    expect(screen.getByText(/Counts use current, scoped evidence/)).toBeTruthy()
    const summary = screen.getByText('How these counts are calculated').closest('summary') as HTMLElement
    const disclosure = summary.closest('details') as HTMLDetailsElement
    expect(disclosure.open).toBe(false)

    fireEvent.click(summary)
    expect(disclosure.open).toBe(true)
    expect(screen.getByText('Hide count definitions')).toBeTruthy()
  })

  it('withholds a fleet total when any device station set is unknown', async () => {
    const { Dashboard } = await import('./Dashboard')
    render(
      <Dashboard
        data={
          {
            ...data,
            wireless_clients: 1,
            wireless_clients_complete: false,
            wireless_clients_unknown_on: ['upstairs-ap'],
          } as never
        }
      />,
    )

    const card = screen.getByText('Wireless clients').parentElement
    expect(card?.children[1]?.textContent).toBe('—')
    expect(card?.textContent).toContain('1 matching row identified; full total unavailable')
    expect(screen.getByText('upstairs-ap')).toBeTruthy()
    expect(screen.getByText(/false zero or dip/)).toBeTruthy()
  })

  it('warns only when an online gateway freshly reports no WAN route', async () => {
    const { Dashboard } = await import('./Dashboard')
    const { rerender } = render(
      <Dashboard
        data={
          {
            ...data,
            gateway_uplinks: [{ device_id: 1, name: 'Gateway', state: 'missing' }],
          } as never
        }
      />,
    )

    expect(screen.getByRole('alert').textContent).toContain('No active WAN/default route was observed on Gateway')
    rerender(
      <Dashboard
        data={
          {
            ...data,
            gateway_uplinks: [{ device_id: 1, name: 'Gateway', state: 'unknown' }],
          } as never
        }
      />,
    )
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('shows WAN provenance and never turns unavailable metrics into zero', async () => {
    const now = Date.now() - 120_000
    const metric = (kind: string, value: number | null, status = 'fresh') => ({
      kind,
      unit: kind.includes('bps') ? 'B/s' : kind.includes('loss') ? 'percent' : 'ms',
      meaning: `${kind} from the selected gateway`,
      status,
      value,
      as_of: value == null ? null : now,
      points: value == null ? [] : [{ ts: now - 300_000, value: value / 2 }, { ts: now, value }],
    })
    const { Dashboard } = await import('./Dashboard')
    render(
      <Dashboard data={{
        ...data,
        wan: {
          target: '1.1.1.1',
          probe: 'icmp',
          freshness: 'fresh',
          as_of: now,
          gateway: { device_id: 1, name: 'Gateway', route_interface: 'wan', series_key: null },
          resolution: '5m',
          bucket_ms: 300_000,
          from: now - 21_600_000,
          to: now,
          metrics: {
            download_bps: metric('download_bps', null, 'unavailable'),
            upload_bps: metric('upload_bps', 1_000_000, 'last_observed'),
            latency_ms: metric('latency_ms', 12.4),
            loss_pct: metric('loss_pct', 0),
            reachable: metric('reachable', 1),
          },
        },
      } as never} />,
    )

    const health = screen.getByRole('region', { name: 'Internet health details' })
    const path = within(health).getByRole('group', { name: 'Observed gateway path' })
    expect(within(path).getByText('Default route')).toBeTruthy()
    expect(within(path).getByText('wan')).toBeTruthy()
    expect(within(path).getByText('External ICMP target · from gateway')).toBeTruthy()
    expect(within(path).getByText('1.1.1.1 · Reachable')).toBeTruthy()
    expect(within(health).getByText('8.0 Mbps')).toBeTruthy()
    expect(within(health).getByText(/Last observed 2m ago/)).toBeTruthy()
    expect(within(health).getByText('12.4 ms')).toBeTruthy()
    expect(within(health).getByText('0.0%')).toBeTruthy()
    expect(within(health).getByText('Download traffic').closest('.dashboard-metric')?.textContent).not.toContain('0 bps')
    expect(within(health).getAllByRole('img')).toHaveLength(4)
    expect(within(health).getByText(/target reachability from the gateway, not gateway or ISP uptime/)).toBeTruthy()

    const view = within(health).getByRole('group', { name: 'Internet health history view' })
    fireEvent.click(within(view).getByRole('button', { name: 'Table' }))
    expect(within(view).getByRole('button', { name: 'Table' }).getAttribute('aria-pressed')).toBe('true')
    const tableRegion = within(health).getByRole('region', { name: 'Internet health history table' })
    expect(within(tableRegion).getAllByRole('row')).toHaveLength(3)
    expect(within(tableRegion).getAllByText('Unavailable')).toHaveLength(2)
    expect(within(tableRegion).getAllByText('0.0%')).toHaveLength(2)
    expect(within(tableRegion).getByText('8.0 Mbps')).toBeTruthy()
  })

  it('aligns sparse WAN history by timestamp and distinguishes unavailable samples from zero', async () => {
    const now = Date.now() - 120_000
    const timestamps = [now - 600_000, now - 300_000, now]
    const metric = (kind: string, unit: string, points: Array<{ ts: number; value: number | null }>) => ({
      kind,
      unit,
      meaning: `${kind} from the selected gateway`,
      status: 'fresh',
      value: points.at(-1)?.value ?? null,
      as_of: now,
      points,
    })
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={{
      ...data,
      wan: {
        target: '1.1.1.1', probe: 'icmp', freshness: 'fresh', as_of: now,
        gateway: { device_id: 1, name: 'Gateway', route_interface: 'wan', series_key: 'wan' },
        resolution: '5m', bucket_ms: 300_000, from: timestamps[0], to: timestamps[2],
        metrics: {
          download_bps: metric('download_bps', 'B/s', [
            { ts: timestamps[0], value: 0 },
            { ts: timestamps[2], value: null },
          ]),
          upload_bps: metric('upload_bps', 'B/s', [
            { ts: timestamps[1], value: 125_000 },
            { ts: timestamps[2], value: 0 },
          ]),
          latency_ms: metric('latency_ms', 'ms', [
            { ts: timestamps[0], value: null },
            { ts: timestamps[1], value: 0 },
          ]),
          loss_pct: metric('loss_pct', 'percent', [{ ts: timestamps[2], value: 0 }]),
          reachable: metric('reachable', 'boolean', [{ ts: timestamps[2], value: 1 }]),
        },
      },
    } as never} />)

    const health = screen.getByRole('region', { name: 'Internet health details' })
    const view = within(health).getByRole('group', { name: 'Internet health history view' })
    fireEvent.click(within(view).getByRole('button', { name: 'Table' }))
    const table = within(health).getByRole('region', { name: 'Internet health history table' })
    const rows = within(table).getAllByRole('row')
    expect(within(rows[0]).getAllByRole('columnheader').map((cell) => cell.textContent)).toEqual([
      'Time', 'Download', 'Upload', 'Latency', 'Loss',
    ])
    expect(rows.slice(1).map((row) => within(row).getAllByRole('cell').slice(1).map((cell) => cell.textContent))).toEqual([
      ['0 bps', 'Unavailable', 'Unavailable', 'Unavailable'],
      ['Unavailable', '1.0 Mbps', '0.0 ms', 'Unavailable'],
      ['Unavailable', '0 bps', 'Unavailable', '0.0%'],
    ])
  })

  it('keeps the selected active gateway healthy when another gateway has no route', async () => {
    const metric = { kind: '', unit: '', meaning: '', status: 'unavailable', value: null, as_of: null, points: [] }
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={{
      ...data,
      gateway_uplinks: [
        { device_id: 1, name: 'Backup', state: 'missing' },
        { device_id: 2, name: 'Primary', state: 'up' },
      ],
      wan: {
        target: '1.1.1.1', probe: 'icmp', freshness: 'fresh', as_of: null,
        gateway: { device_id: 2, name: 'Primary', route_interface: 'wan', series_key: null },
        resolution: '5m', bucket_ms: 300_000, from: 0, to: 0,
        metrics: { download_bps: metric, upload_bps: metric, latency_ms: metric, loss_pct: metric, reachable: metric },
      },
    } as never} />)

    expect(screen.getByText('Route active')).toBeTruthy()
    expect(screen.getByRole('alert').textContent).toContain('Backup')
  })

  it('requires explicit data-use consent before starting a controller speed test', async () => {
    const { Dashboard } = await import('./Dashboard')
    api.speedTests.mockResolvedValueOnce({
      jobs: [], active: null,
      limits: { max_history: 20 },
      test: {
        plan_id: speedPlanID,
        provider: 'librespeed', method: 'embedded HTTP', provenance: 'controller-host',
        endpoint: 'https://speed.example', download_endpoint: 'https://speed.example/down',
        upload_endpoint: 'https://speed.example/up', estimated_bytes: 500_000_000, max_duration_seconds: 90,
      },
      disclosure: {
        vantage_point: 'controller-host', router_management_calls: false, router_changes: false,
        saturation_warning: 'May temporarily saturate the gateway/WAN.',
        privacy: "Test requests and the controller host's public IP are visible to the provider; measurements remain in the controller database.",
      },
    })
    api.startSpeedTest.mockResolvedValue({
      id: '0123456789abcdef0123456789abcdef', state: 'queued', phase: 'queued', progress_percent: 0,
      provider: 'ookla', method: 'cli', provenance: 'controller-host', endpoint: '',
      estimated_bytes: 500_000_000, created_at: 1, bytes_downloaded: 0, bytes_uploaded: 0,
    })
    render(<Dashboard data={data as never} />)
    await waitFor(() => expect(api.speedTests).toHaveBeenCalledWith(20))

    fireEvent.click(screen.getByRole('button', { name: 'Review speed test' }))
    expect(screen.getByText('librespeed')).toBeTruthy()
    expect(screen.getByText('https://speed.example')).toBeTruthy()
    expect(screen.getByText('https://speed.example/down')).toBeTruthy()
    expect(screen.getByText('https://speed.example/up')).toBeTruthy()
    expect(screen.getByText('embedded HTTP')).toBeTruthy()
    expect(screen.getByText(/public IP are visible to the provider/)).toBeTruthy()
    expect(screen.getByText(/approximately 500 MB plus protocol overhead/)).toBeTruthy()
    expect(screen.getAllByText('false')).toHaveLength(2)
    const consent = screen.getByRole('checkbox', {
      name: /may use data and temporarily saturate the connection/i,
    }) as HTMLInputElement
    const start = screen.getByRole('button', { name: 'Start speed test' }) as HTMLButtonElement
    expect(start.disabled).toBe(true)
    fireEvent.click(consent)
    expect(start.disabled).toBe(false)
    fireEvent.click(start)
    await waitFor(() => expect(api.startSpeedTest).toHaveBeenCalledWith(speedPlanID, true))
  })

  it('never invents absent safety disclosures or enables Start for a partial plan', async () => {
    const { Dashboard } = await import('./Dashboard')
    api.speedTests.mockResolvedValueOnce({
      jobs: [], active: null, limits: { max_history: 20 },
      test: {
        plan_id: speedPlanID,
        provider: 'librespeed', method: 'embedded HTTP', provenance: 'controller-host',
        endpoint: 'https://speed.example', download_endpoint: 'https://speed.example/down',
        upload_endpoint: 'https://speed.example/up', estimated_bytes: 15_000_000, max_duration_seconds: 30,
      },
      disclosure: {
        vantage_point: 'controller-host',
        router_management_calls: undefined,
        router_changes: undefined,
        saturation_warning: 'May saturate the WAN.',
        privacy: '',
      },
    } as never)
    render(<Dashboard data={data as never} />)

    fireEvent.click(screen.getByRole('button', { name: 'Review speed test' }))
    await screen.findByText(/safety disclosures are unavailable or incomplete/)
    const review = screen.getByRole('group', { name: 'Information: Controller speed test' })
    expect(within(review).getAllByText('Unavailable')).toHaveLength(2)
    expect(within(review).getByText('Privacy disclosure unavailable.')).toBeTruthy()
    fireEvent.click(screen.getByRole('checkbox', { name: /may use data/i }))
    expect((screen.getByRole('button', { name: 'Start speed test' }) as HTMLButtonElement).disabled).toBe(true)
    expect(api.startSpeedTest).not.toHaveBeenCalled()
  })

  it('refreshes server truth when another tab wins the start race', async () => {
    const plan = {
      test: {
        plan_id: speedPlanID,
        provider: 'librespeed', method: 'embedded', provenance: 'controller-host',
        endpoint: 'https://speed.example', download_endpoint: 'https://speed.example/down',
        upload_endpoint: 'https://speed.example/up', estimated_bytes: 400_000_000, max_duration_seconds: 90,
      },
      limits: { max_history: 20 },
      disclosure: {
        vantage_point: 'controller-host', router_management_calls: false, router_changes: false,
        saturation_warning: 'May saturate the WAN.', privacy: 'Public IP is visible to the provider.',
      },
    }
    const active = {
      id: '44444444444444444444444444444444', state: 'running', phase: 'latency', progress_percent: 15,
      provider: 'librespeed', method: 'embedded', provenance: 'controller-host', endpoint: 'speed.example',
      estimated_bytes: 400_000_000, created_at: Date.now(), bytes_downloaded: 0, bytes_uploaded: 0,
    }
    const completed = {
      ...active,
      state: 'completed', phase: 'complete', progress_percent: 100, finished_at: Date.now(),
    }
    api.speedTests
      .mockResolvedValueOnce({ ...plan, jobs: [], active: null })
      .mockResolvedValueOnce({ ...plan, jobs: [active], active })
      .mockResolvedValueOnce({ ...plan, jobs: [completed], active: null })
    api.startSpeedTest.mockRejectedValue(new Error('a speed test is already active'))
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)

    fireEvent.click(screen.getByRole('button', { name: 'Review speed test' }))
    await screen.findByText('librespeed')
    fireEvent.click(screen.getByRole('checkbox', { name: /may use data/i }))
    fireEvent.click(screen.getByRole('button', { name: 'Start speed test' }))

    const progress = await screen.findByRole('progressbar', { name: 'Controller speed test progress' })
    expect(progress.getAttribute('aria-valuetext')).toContain('latency')
    expect(api.speedTests).toHaveBeenCalledTimes(2)
    const retry = await screen.findByRole('button', { name: 'Start speed test' }, { timeout: 2_500 }) as HTMLButtonElement
    const freshConsent = screen.getByRole('checkbox', { name: /may use data/i }) as HTMLInputElement
    expect(freshConsent.checked).toBe(false)
    expect(retry.disabled).toBe(true)
  })

  it('shows controller provenance, progress, cancellation and result history', async () => {
    const active = {
      id: '11111111111111111111111111111111', state: 'running', phase: 'download', progress_percent: 42,
      provider: 'librespeed', method: 'embedded', provenance: 'controller-host', endpoint: 'speed.example',
      estimated_bytes: 400_000_000, created_at: 10, started_at: 11,
      download_mbps: 321.4, bytes_downloaded: 30_000_000, bytes_uploaded: 0,
    }
    const completed = {
      id: '22222222222222222222222222222222', state: 'completed', phase: 'complete', progress_percent: 100,
      provider: 'librespeed', method: 'embedded', provenance: 'controller-host', endpoint: 'speed.example',
      estimated_bytes: 400_000_000, created_at: 1, started_at: 2, finished_at: 3,
      download_mbps: 200, upload_mbps: 40, idle_latency_ms: 9, idle_jitter_ms: 1.5,
      loaded_latency_ms: null, loaded_jitter_ms: null,
      bytes_downloaded: 200_000_000, bytes_uploaded: 40_000_000,
    }
    api.speedTests.mockResolvedValueOnce({
      jobs: [active, completed], active,
      limits: { max_history: 20 },
    })
    api.cancelSpeedTest.mockResolvedValue({ ...active, state: 'cancelling', phase: 'cancelling' })
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)

    const progress = await screen.findByRole('progressbar', { name: 'Controller speed test progress' })
    expect(progress.getAttribute('aria-valuenow')).toBe('42')
    expect(screen.getByText(/librespeed.*embedded.*controller-host.*download.*speed\.example/)).toBeTruthy()
    expect(screen.getByText('Loaded latency')).toBeTruthy()
    expect(screen.getByRole('meter', { name: 'Download throughput' }).getAttribute('aria-valuetext')).toBe('200.0 Mbps')
    expect(screen.getByRole('meter', { name: 'Upload throughput' }).getAttribute('aria-valuetext')).toBe('40.0 Mbps')
    expect(screen.getByText(/Idle 9\.0 ms latency · 1\.5 ms jitter/)).toBeTruthy()
    expect(screen.getByText('Loaded unavailable')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(api.cancelSpeedTest).toHaveBeenCalledWith(active.id))
  })

  it('does not let an older poll erase a newer cancellation state', async () => {
    const active = {
      id: '33333333333333333333333333333333', state: 'running', phase: 'upload', progress_percent: 70,
      provider: 'librespeed', method: 'embedded', provenance: 'controller-host', endpoint: 'speed.example',
      estimated_bytes: 400_000_000, created_at: 10, started_at: 11,
      bytes_downloaded: 30_000_000, bytes_uploaded: 10_000_000,
    }
    const collection = {
      jobs: [active], active, limits: { max_history: 20 },
      test: {
        plan_id: speedPlanID,
        provider: 'librespeed', method: 'embedded', provenance: 'controller-host',
        endpoint: 'https://speed.example', download_endpoint: 'https://speed.example/down',
        upload_endpoint: 'https://speed.example/up', estimated_bytes: 400_000_000, max_duration_seconds: 90,
      },
      disclosure: {
        vantage_point: 'controller-host', router_management_calls: false, router_changes: false,
        saturation_warning: '', privacy: '',
      },
    }
    let resolveLate!: (value: unknown) => void
    const late = new Promise((resolve) => { resolveLate = resolve })
    api.speedTests.mockResolvedValueOnce(collection).mockReturnValueOnce(late)
    api.cancelSpeedTest.mockResolvedValue({ ...active, state: 'cancelling', phase: 'cancelling' })
    const { Dashboard } = await import('./Dashboard')
    render(<Dashboard data={data as never} />)
    await screen.findByRole('progressbar', { name: 'Controller speed test progress' })
    await waitFor(() => expect(api.speedTests).toHaveBeenCalledTimes(2), { timeout: 1_500 })

    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(api.cancelSpeedTest).toHaveBeenCalledWith(active.id))
    await act(async () => {
      resolveLate({ ...collection, jobs: [], active: null })
      await Promise.resolve()
    })

    const progress = screen.getByRole('progressbar', { name: 'Controller speed test progress' })
    expect(progress.getAttribute('aria-valuetext')).toContain('cancelling')
  })
})

describe('Settings — the hazard a rollback cannot undo', () => {
  const site = {
    name: 'Site',
    uuid: 'abcdef01-2345-6789-abcd-ef0123456789',
    wlans: [],
    meshes: [],
    groups: [{ id: 1, name: 'all', device_ids: [] }],
    networks: [
      {
        id: 1,
        name: 'lan',
        vlan: 1,
        cidr: '192.168.1.1/24',
        zone: 'lan',
        enabled: true,
      },
    ],
    problems: [],
    overrides: [],
    overridable: [],
    override_note: '',
  }

  const defect = (over: Record<string, unknown> = {}) => ({
    wlan: 'fixture-roam',
    defect_id: 'mwlwifi-80211w-unsupported',
    summary: '802.11w is not properly supported by this radio driver',
    detail: 'measured: the firmware stops answering 85s after a fast transition',
    confidence: 'measured',
    severity: 'radio-death',
    mitigation: 'set PMF to disabled on this WLAN',
    source: 'STATUS.md §5an',
    ...over,
  })

  it('does not call an empty fleet converged', async () => {
    api.site.mockResolvedValue(site)
    api.preview.mockResolvedValue({ devices: [], site_errors: [] })
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('Preview changes'))

    expect(await screen.findByText(/no adopted devices to compare or apply/)).toBeTruthy()
    expect(screen.queryByText(/every device already matches/)).toBeNull()
  })

  const previewWith = (defects: unknown[]) => ({
    preview_token: 'pv-current',
    devices: [
      {
        device_id: 1,
        name: 'ap-wrt',
        role: 'ap',
        functions: ['ap', 'switch'],
        changes: [
          {
            config: 'wireless',
            section: 's',
            action: 'update',
            options: ['x'],
          },
        ],
        blocked: false,
        touches_traversal: false,
        cautions: [] as string[],
        driver_defects: defects,
      },
    ],
    site_errors: [],
  })

  beforeEach(() => {
    localStorage.clear()
    api.site.mockResolvedValue(site)
  })

  it('keeps durable Apply truth visible while rollout mechanics stay collapsed', async () => {
    render(<Settings devices={[]} />)

    const notice = await screen.findByRole('group', { name: 'Information: Apply behavior' })
    expect(within(notice).getByText(/Nothing above has touched a device/)).toBeTruthy()
    const toggle = within(notice).getByText('More information about Apply')
    const details = toggle.closest('details') as HTMLDetailsElement
    expect(details.open).toBe(false)
    fireEvent.click(toggle)
    expect(details.open).toBe(true)
    expect(within(notice).getByText(/stops at the first device that fails/)).toBeTruthy()
    expect(within(notice).getByText(/rollback armed/)).toBeTruthy()
  })

  async function previewThen(defects: unknown[]) {
    api.preview.mockResolvedValue(previewWith(defects))
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalled())
  }

  it('clears the old plan while re-previewing and keeps it cleared on failure', async () => {
    api.preview.mockResolvedValueOnce(previewWith([])).mockRejectedValueOnce(new Error('preview unavailable'))
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('Preview changes'))
    expect(await screen.findByText('ap-wrt')).toBeTruthy()
    if (applyBtn().disabled) throw new Error('first preview did not enable Apply')

    fireEvent.click(screen.getByText('Preview changes'))
    expect(screen.queryByText('ap-wrt')).toBeNull()
    expect(await screen.findByText('preview unavailable')).toBeTruthy()
    if (!applyBtn().disabled) throw new Error('failed refresh left the old plan applicable')
  })

  it('ignores a preview response that finishes after the model changes', async () => {
    let finishPreview!: (result: ReturnType<typeof previewWith>) => void
    api.preview.mockReturnValue(
      new Promise((resolve) => {
        finishPreview = resolve
      }),
    )
    api.saveNetwork.mockResolvedValue({})
    render(<Settings devices={[]} />)

    fireEvent.click(await screen.findByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalledTimes(1))
    const zone = screen.getByLabelText('Firewall zone for lan')
    fireEvent.change(zone, { target: { value: 'managed_lan' } })
    fireEvent.keyDown(zone, { key: 'Enter' })
    await waitFor(() => expect(api.saveNetwork).toHaveBeenCalled())

    await act(async () => finishPreview(previewWith([])))
    expect(screen.queryByText('ap-wrt')).toBeNull()
    if (!applyBtn().disabled) throw new Error('a stale response re-enabled Apply')
    expect((screen.getByText('Preview changes').closest('button') as HTMLButtonElement).disabled).toBe(false)
  })

  it('shows additive functions in the per-device apply preview', async () => {
    await previewThen([])
    expect(await screen.findByText(/AP · Switch/)).toBeTruthy()
  })

  it('preserves bundled behavior for a legacy preview with only a role', async () => {
    api.preview.mockResolvedValue({
      devices: [
        {
          device_id: 1,
          name: 'old-gateway',
          role: 'gateway',
          changes: [],
          blocked: false,
          touches_traversal: false,
          driver_defects: [],
        },
      ],
      site_errors: [],
    })
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Preview changes'))
    expect(await screen.findByText(/Gateway · AP · Switch/)).toBeTruthy()
  })

  it('does not widen an explicitly empty preview through its legacy role', async () => {
    api.preview.mockResolvedValue({
      devices: [
        {
          device_id: 1,
          name: 'invalid-gateway',
          role: 'gateway',
          functions: [],
          changes: [],
          blocked: false,
          touches_traversal: false,
          driver_defects: [],
        },
      ],
      site_errors: [],
    })
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Preview changes'))
    expect(await screen.findByText(/None — invalid record/)).toBeTruthy()
    expect(screen.queryByText(/Gateway · AP · Switch/)).toBeNull()
  })

  // The preview showed every omission under one heading: "Left out on this
  // device (not an error — the hardware or firmware cannot take it)".
  //
  // Two of the things in that list describe a network that stops working — an
  // unencrypted mesh anyone in range can join, and a wireless bridge that is a
  // layer-2 loop if the device is also cabled — and both were rendered in muted
  // grey directly under the reassurance. A third kind says a section was KEPT
  // because the device could not be read, which is the reverse of "left out".
  it('does not file a pre-apply hazard under "not an error"', async () => {
    api.preview.mockResolvedValue({
      devices: [
        {
          device_id: 1,
          name: 'ap-1',
          role: 'ap',
          changes: [],
          blocked: false,
          touches_traversal: false,
          driver_defects: [],
          omitted: ['home: device has no 6g radio'],
          cautions: [
            'roam: this device will join roam as a wireless bridge. If it is ' +
              'ALSO connected by ethernet to the same network, that is a layer-2 loop',
          ],
          undetermined: ['oowrt_up1_radio1: the existing wireless uplink section is left exactly as it is'],
        },
      ],
      site_errors: [],
    })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalled())

    const plan = screen.getByRole('group', { name: 'Warning: Apply preview · ap-1' })
    expect(within(plan).getByText(/0 planned changes · 1 caution · 1 omission · 1 undetermined item/)).toBeTruthy()
    const technicalToggle = within(plan).getByText('Show technical details for ap-1')
    const technicalDetails = technicalToggle.closest('details') as HTMLDetailsElement
    expect(technicalDetails.open).toBe(false)

    // The hazard is present, and is NOT under the heading that calls it fine.
    await waitFor(() => expect(screen.getAllByText(/layer-2 loop/).length).toBeGreaterThan(0))
    expect(screen.getByText(/worth a look first/)).toBeTruthy()
    const visibleCautions = screen.getAllByText(/ALSO connected by ethernet/)
    expect(visibleCautions.length).toBeGreaterThanOrEqual(2)
    expect(visibleCautions.every((item) => item.closest('.notice-disclosure') == null)).toBe(true)
    expect(screen.queryByText(/hardware or firmware cannot take it/)).toBeNull()

    // A section kept in place is not described as left out.
    expect(screen.getByText(/Could not be determined/)).toBeTruthy()
    expect(screen.getByText(/left exactly as it is/)).toBeTruthy()

    // And a genuine omission still shows.
    expect(screen.getByText(/no 6g radio/)).toBeTruthy()
    fireEvent.click(technicalToggle)
    expect(technicalDetails.open).toBe(true)
  })

  it('keeps blocking, management-path and drift-overwrite consequences outside device details', async () => {
    api.preview.mockResolvedValue({
      ...previewWith([]),
      devices: [{
        ...previewWith([]).devices[0],
        blocked: true,
        error: 'device planning failed',
        conflicts: ['wireless.foreign is owned by the operator'],
        touches_traversal: true,
        drift: ['wireless.s.mode differs from desired state'],
      }],
    })
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Preview changes'))

    const plan = await screen.findByRole('group', { name: 'Critical: Apply preview · ap-wrt' })
    const details = within(plan).getByText('Show technical details for ap-wrt').closest('details')
    expect(details?.open).toBe(false)
    for (const text of [
      'device planning failed',
      /Nothing will be applied to this device/,
      /Edits this device's network or firewall configuration/,
      /Applying will put it back to the site model/,
    ]) {
      expect(screen.getByText(text).closest('.notice-disclosure')).toBeNull()
    }
    expect(within(plan).getByText(/1 planned change · 1 drift item · management path affected/)).toBeTruthy()
  })

  // Two changes can name the SAME section: an option-to-list repair clears the
  // option and then writes the section back. The list was keyed by
  // `config.section`, so React got duplicate keys — and worse, the clear
  // rendered as a bare "remove wireless.oowrt_bv20" in the colour used for
  // destruction, which reads as the bridge-VLAN being deleted.
  //
  // Found by a pre-flight audit agent that crashed before reporting, and left
  // the probe behind in a scratch file.
  it('does not paint clearing one option as removing the section', async () => {
    const err = vi.spyOn(console, 'error').mockImplementation(() => {})
    api.preview.mockResolvedValue({
      devices: [
        {
          device_id: 1,
          name: 'ap-wrt',
          role: 'ap',
          blocked: false,
          touches_traversal: false,
          driver_defects: [],
          changes: [
            {
              config: 'network',
              section: 'oowrt_bv20',
              action: 'remove',
              option: 'ports',
            },
            {
              config: 'network',
              section: 'oowrt_bv20',
              action: 'update',
              options: ['device', 'vlan'],
            },
          ],
        },
      ],
      site_errors: [],
    })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalled())

    // The option is named, and the word is not "remove".
    await waitFor(() => expect(screen.getByText(/oowrt_bv20\.ports/)).toBeTruthy())
    expect(screen.getByText('clear')).toBeTruthy()

    // And React was not handed two identical keys.
    const dupes = err.mock.calls.filter((c) => String(c[0]).includes('same key'))
    expect(dupes.length).toBe(0)
    err.mockRestore()
  })

  // A whole-section removal still reads as one.
  it('still calls a whole-section removal a removal', async () => {
    api.preview.mockResolvedValue({
      devices: [
        {
          device_id: 1,
          name: 'ap-wrt',
          role: 'ap',
          blocked: false,
          touches_traversal: false,
          driver_defects: [],
          changes: [
            {
              config: 'wireless',
              section: 'oowrt_wlan9_radio0',
              action: 'remove',
            },
          ],
        },
      ],
      site_errors: [],
    })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalled())
    await waitFor(() => expect(screen.getByText('remove')).toBeTruthy())
  })

  const applyBtn = () =>
    screen
      .getAllByText(/^Apply/)
      .map((n) => n.closest('button')!)
      .find(Boolean)!

  // The screen already stops for touches_traversal — editing the path the
  // controller reaches a device through — and tells the reader a rollback
  // restores it within 90 seconds. That is true there and false here: a radio
  // that stops answering cannot be reached to revert, and stays down until
  // somebody physically power-cycles the box. The lesser, recoverable hazard
  // demanded an acknowledgement; the greater, unrecoverable one demanded none.
  it('will not apply a change that is known to kill the radio until acknowledged', async () => {
    await previewThen([defect()])

    await waitFor(() => expect(screen.getByText(/take the radio down until someone power-cycles it/)).toBeTruthy())
    const risk = screen.getByRole('group', { name: 'Critical: Driver risk' })
    expect((within(risk).getByText('Hide driver risk information').closest('details') as HTMLDetailsElement).open).toBe(true)
    expect(risk.textContent ?? '').toMatch(/rollback does not cover this/i)
    expect(risk.textContent ?? '').toMatch(/physically power-cycles the device/i)
    expect(screen.getByText(/take the radio down until someone power-cycles it/).closest('.notice-disclosure')).toBeNull()
    if (!applyBtn().disabled) {
      throw new Error('Apply was enabled for a change measured to kill the radio')
    }
    // And it says the rollback does not cover it, because the line above the
    // button promises the opposite.
    expect(screen.getByText(/rollback does not cover this/i)).toBeTruthy()

    fireEvent.click(screen.getByText(/take the radio down until someone power-cycles it/))
    await waitFor(() => {
      if (applyBtn().disabled) throw new Error('still disabled after acknowledging')
    })
  })

  it('keeps a silently ignored setting visible without gating Apply', async () => {
    const summary = 'the driver accepts PMF configuration but silently discards it'
    const detail = 'the requested PMF value is absent from the active radio state'
    await previewThen([defect({
      defect_id: 'mwlwifi-pmf-silently-ignored',
      summary,
      detail,
      severity: 'silently-ignored',
    })])

    const consequence = screen.getByText(/Silently ignored: Apply will write this setting/)
    expect(consequence.textContent ?? '').toContain(`fixture-roam: ${summary}`)
    expect(consequence.closest('.notice-disclosure')).toBeNull()
    expect(screen.getByText(detail).closest('.notice-disclosure')).not.toBeNull()
    expect(screen.queryByRole('group', { name: 'Critical: Driver risk' })).toBeNull()
    if (applyBtn().disabled) throw new Error('Apply was gated by a nonfatal driver defect')
  })

  it('sends the bound preview and both acknowledgements, then requires a new preview after rejection', async () => {
    const hazardous = previewWith([defect()])
    hazardous.devices[0].touches_traversal = true
    hazardous.devices[0].cautions = ['wireless uplink may form a layer-2 loop']
    api.preview.mockResolvedValue(hazardous)
    api.applySite.mockRejectedValue(
      new ApiError(409, 'the preview is stale; Preview again before applying; nothing was written', {
        write_state: 'none',
      }),
    )
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Preview changes'))

    fireEvent.click(await screen.findByText(/apply the network changes/))
    fireEvent.click(await screen.findByText(/reviewed these cautions/))
    fireEvent.click(await screen.findByText(/take the radio down until someone power-cycles it/))
    await waitFor(() => {
      if (applyBtn().disabled) throw new Error('still disabled after both acknowledgements')
    })
    fireEvent.click(applyBtn())

    await waitFor(() =>
      expect(api.applySite).toHaveBeenCalledWith({
        operation_id: expect.stringMatching(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/),
        preview_token: 'pv-current',
        acknowledge_traversal: true,
        acknowledge_driver_risk: true,
        acknowledge_cautions: true,
      }),
    )
    expect(await screen.findByText(/preview is stale/i)).toBeTruthy()
    if (!applyBtn().disabled) throw new Error('rejected preview remained applicable')

    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalledTimes(2))
    if (!applyBtn().disabled) {
      throw new Error('acknowledgements survived a rejected apply')
    }
  })

  it('disables Apply when any selected device could not be planned', async () => {
    api.preview.mockResolvedValue({
      ...previewWith([]),
      devices: [
        {
          ...previewWith([]).devices[0],
          error: 'could not reach this device',
        },
      ],
    })
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Preview changes'))
    expect(await screen.findByText(/could not reach this device/)).toBeTruthy()
    if (!applyBtn().disabled) throw new Error('Apply was enabled for an unplanned device')
  })

  it('recovers a completed Apply after its POST response is lost', async () => {
    api.preview.mockResolvedValue(previewWith([]))
    api.applySite.mockRejectedValue(new TypeError('Failed to fetch'))
    api.applyOperation.mockResolvedValue({
      operation_id: '01962c09-7d62-7cd7-a1c2-450eba830892',
      state: 'completed',
      created_at: 1,
      started_at: 2,
      finished_at: 3,
      write_state: 'possible',
      result: {
        operation_id: '01962c09-7d62-7cd7-a1c2-450eba830892',
        devices: [{ device_id: 1, name: 'ap-wrt', outcome: 'applied', changes: 1 }],
        aborted: false,
      },
    })
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Preview changes'))
    await waitFor(() => {
      if (applyBtn().disabled) throw new Error('preview did not enable Apply')
    })
    fireEvent.click(applyBtn())

    expect(await screen.findByText('Previous result: 1 change applied to 1 device.')).toBeTruthy()
    expect(api.applyOperation).toHaveBeenCalledTimes(1)
    const sent = api.applySite.mock.calls[0][0].operation_id
    expect(api.applyOperation).toHaveBeenCalledWith(sent)
    expect(localStorage.getItem('oonfee_last_apply_operation')).toBeNull()
    expect(screen.queryByText(/result unknown/i)).toBeNull()
    expect(screen.queryByText(/Nothing above has touched a device/)).toBeNull()
    expect(screen.getByText(/Durable write state: possible/)).toBeTruthy()
  })

  it('recovers a partial Apply when the POST response body is truncated', async () => {
    api.preview.mockResolvedValue(previewWith([]))
    api.applySite.mockRejectedValue(new SyntaxError('Unexpected end of JSON input'))
    api.applyOperation.mockImplementation(async (id: string) => ({
      operation_id: id,
      state: 'failed',
      created_at: 1,
      started_at: 2,
      finished_at: 3,
      write_state: 'possible',
      devices: [],
      error: 'apply stopped after ap-wrt',
      result: {
        operation_id: id,
        devices: [
          {
            device_id: 1,
            name: 'ap-wrt',
            outcome: 'reverted',
            router_outcome: 'reverted',
            changes: 1,
            reason: 'health check failed',
          },
        ],
        aborted: true,
        aborted_after: 'ap-wrt',
      },
    }))
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Preview changes'))
    await waitFor(() => {
      if (applyBtn().disabled) throw new Error('preview did not enable Apply')
    })
    fireEvent.click(applyBtn())

    expect(await screen.findByText(/Stopped after ap-wrt: health check failed/)).toBeTruthy()
    expect(api.applyOperation).toHaveBeenCalledTimes(1)
    expect(screen.queryByText(/result unknown/i)).toBeNull()
  })

  it('recovers a retained interrupted operation on reload and announces its ID', async () => {
    const id = '01962c09-7d62-7cd7-a1c2-450eba830892'
    localStorage.setItem('oonfee_last_apply_operation', id)
    api.applyOperation.mockResolvedValue({
      operation_id: id,
      state: 'unknown',
      created_at: 1,
      started_at: 2,
      finished_at: 3,
      write_state: 'possible',
      error: 'controller restarted while this Apply was running',
      devices: [
        {
          ordinal: 0,
          device_id: 1,
          device_mac: '02:00:00:00:00:01',
          device_name: 'ap-wrt',
          state: 'unknown',
          write_state: 'possible',
          router_outcome: 'unknown',
          outcome: 'unknown',
          changes: 1,
        },
      ],
    })

    render(<Settings devices={[]} />)

    expect(await screen.findByText(id)).toBeTruthy()
    const status = screen.getByRole('status')
    expect(status.getAttribute('aria-live')).toBe('polite')
    await waitFor(() => expect(status.textContent ?? '').toMatch(/unknown/i))
    expect(await screen.findByText(/outcome of operation .* is unknown/i)).toBeTruthy()
    expect(screen.getByText(/router: unknown/i)).toBeTruthy()
    expect(screen.queryByText(/Nothing above has touched a device/)).toBeNull()
    expect(screen.getByText(/Durable write state: possible/)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Check status' })).toBeTruthy()
  })

  it('clears a retained Apply reference that belongs to a different controller database', async () => {
    const id = '01962c09-7d62-7cd7-a1c2-450eba830892'
    localStorage.setItem('oonfee_last_apply_operation', id)
    api.applyOperation.mockRejectedValue(new ApiError(404, 'apply operation not found'))

    render(<Settings devices={[]} />)

    expect(await screen.findByText(/Cleared a saved Apply reference/)).toBeTruthy()
    expect(localStorage.getItem('oonfee_last_apply_operation')).toBeNull()
    expect(screen.queryByText(id)).toBeNull()
    expect(screen.getByText(/Nothing above has touched a device/)).toBeTruthy()
  })

  it('polls a retained running operation until its terminal result is durable', async () => {
    const id = '01962c09-7d62-7cd7-a1c2-450eba830892'
    localStorage.setItem('oonfee_last_apply_operation', id)
    const terminal = {
      operation_id: id,
      state: 'completed' as const,
      created_at: 1,
      started_at: 2,
      finished_at: 3,
      write_state: 'possible' as const,
      result: {
        operation_id: id,
        devices: [{ device_id: 1, name: 'ap-wrt', outcome: 'applied', changes: 1 }],
        aborted: false,
      },
      devices: [],
    }
    let finish!: () => void
    const terminalResponse = new Promise<typeof terminal>((resolve) => {
      finish = () => resolve(terminal)
    })
    api.applyOperation
      .mockResolvedValueOnce({
        operation_id: id,
        state: 'running',
        created_at: 1,
        started_at: 2,
      })
      .mockReturnValueOnce(terminalResponse)

    render(<Settings devices={[]} />)

    expect(await screen.findByText(/request was accepted and its durable status above is authoritative/)).toBeTruthy()
    expect(screen.queryByText(/Nothing above has touched a device/)).toBeNull()
    await waitFor(() => expect(api.applyOperation).toHaveBeenCalledTimes(2), {
      timeout: 2500,
    })
    finish()
    expect(await screen.findByText('Previous result: 1 change applied to 1 device.')).toBeTruthy()
    expect(localStorage.getItem('oonfee_last_apply_operation')).toBeNull()
    expect(screen.getByRole('status').textContent ?? '').toMatch(/completed/i)
  })

  it('hides a completed previous Apply when a new Preview is loaded', async () => {
    const id = '01962c09-7d62-7cd7-a1c2-450eba830892'
    localStorage.setItem('oonfee_last_apply_operation', id)
    api.applyOperation.mockResolvedValue({
      operation_id: id,
      state: 'completed',
      created_at: 1,
      finished_at: 2,
      write_state: 'possible',
      devices: [],
      result: {
        operation_id: id,
        devices: [{ device_id: 1, name: 'ap-wrt', outcome: 'applied', changes: 1 }],
        aborted: false,
      },
    })
    api.preview.mockResolvedValue(previewWith([]))

    render(<Settings devices={[]} />)

    expect(await screen.findByText('Previous Apply operation')).toBeTruthy()
    expect(localStorage.getItem('oonfee_last_apply_operation')).toBeNull()
    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalledTimes(1))
    expect(screen.queryByText(id)).toBeNull()
    expect(screen.getByText(/Nothing above has touched a device/)).toBeTruthy()
  })

  it('does not call an interrupted queued operation a possible router write', async () => {
    const id = '01962c09-7d62-7cd7-a1c2-450eba830892'
    localStorage.setItem('oonfee_last_apply_operation', id)
    api.applyOperation.mockResolvedValue({
      operation_id: id,
      state: 'unknown',
      created_at: 1,
      finished_at: 2,
      write_state: 'none',
      devices: [],
      error: 'controller restarted before this queued Apply started',
    })

    render(<Settings devices={[]} />)

    expect((await screen.findAllByText(/no device write began/i)).length).toBeGreaterThan(0)
    expect(screen.getByText(/Durable write state: none/)).toBeTruthy()
    expect(screen.queryByText(/outcome of operation .* is unknown/i)).toBeNull()
  })

  it('preserves a completed Apply result when only the automatic refresh fails', async () => {
    api.preview.mockResolvedValueOnce(previewWith([])).mockRejectedValueOnce(new Error('refresh offline'))
    api.applySite.mockResolvedValue({
      devices: [{ device_id: 1, name: 'ap-wrt', outcome: 'applied', changes: 1 }],
      aborted: false,
    })
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Preview changes'))
    await waitFor(() => {
      if (applyBtn().disabled) throw new Error('preview did not enable Apply')
    })
    fireEvent.click(applyBtn())

    expect(await screen.findByText('1 change applied to 1 device.')).toBeTruthy()
    const warning = await screen.findByText(/Refresh failed: refresh offline/i)
    expect(warning.textContent ?? '').not.toMatch(/nothing was written|result unknown/i)
  })

  it('distinguishes changed devices from devices that already matched', async () => {
    api.preview.mockResolvedValue(previewWith([]))
    api.applySite.mockResolvedValue({
      devices: [
        { device_id: 1, name: 'ap-wrt', outcome: 'applied', changes: 2 },
        { device_id: 2, name: 'ap-c6', outcome: 'applied', changes: 0 },
      ],
      aborted: false,
    })
    render(<Settings devices={[]} />)
    fireEvent.click(await screen.findByText('Preview changes'))
    await waitFor(() => {
      if (applyBtn().disabled) throw new Error('preview did not enable Apply')
    })
    fireEvent.click(applyBtn())

    expect(await screen.findByText('2 changes applied to 1 device; 1 device already matched.')).toBeTruthy()
  })

  // The same reset, for the acknowledgement that already existed. Covered
  // separately because a mutation that reset only the new one passed every
  // other test here — the two halves are one line apart and independently
  // wrong-able.
  it('makes every preview re-earn the traversal acknowledgement too', async () => {
    api.preview.mockResolvedValue({
      devices: [
        {
          device_id: 1,
          name: 'ap-gw',
          role: 'gateway',
          changes: [
            {
              config: 'network',
              section: 's',
              action: 'update',
              options: ['x'],
            },
          ],
          blocked: false,
          touches_traversal: true,
          driver_defects: [],
        },
      ],
      site_errors: [],
    })
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalled())

    await waitFor(() => expect(screen.getByText(/apply the network changes/)).toBeTruthy())
    const managementPath = screen.getByRole('group', { name: 'Warning: Management path' })
    const managementDetails = within(managementPath)
      .getByText('More information about management-path rollback')
      .closest('details') as HTMLDetailsElement
    expect(managementDetails.open).toBe(false)
    const unreachable = within(managementPath).getByText(/may become temporarily unreachable/)
    expect(unreachable.closest('.notice-disclosure')).toBeNull()
    expect(within(managementPath).getByText(/armed rollback restores it to its prior configuration/)
      .closest('.notice-disclosure')).toBeNull()
    expect(screen.getByText(/apply the network changes/).closest('.notice-disclosure')).toBeNull()
    expect(within(managementPath).getByText(/restores itself within 90 seconds/)).toBeTruthy()
    fireEvent.click(screen.getByText(/apply the network changes/))
    await waitFor(() => {
      if (applyBtn().disabled) throw new Error('still disabled after acknowledging')
    })

    fireEvent.click(screen.getByText('Preview changes'))
    await waitFor(() => expect(api.preview).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    if (!applyBtn().disabled) {
      throw new Error('a stale traversal acknowledgement carried to a fresh preview')
    }
  })

  // A defect of the HARDWARE that no configuration causes and none can avoid
  // has no wlan attached. Gating on those would demand a tick before every
  // apply to that device forever — the cry-wolf failure that makes a warning
  // worth ignoring on the day it matters.
  it('does not gate on a hardware defect no configuration asked for', async () => {
    await previewThen([defect({ wlan: undefined })])

    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    expect(screen.queryByText(/take the radio down until someone power-cycles it/)).toBeNull()
    expect(applyBtn().disabled).toBe(false)
  })

  // Consent to one plan must not carry to the next. Acknowledgements used to
  // persist across previews, so ticking once and then editing the site left
  // Apply enabled for a different set of changes nobody had agreed to.
  it('makes every preview re-earn the acknowledgement', async () => {
    await previewThen([defect()])
    fireEvent.click(screen.getByText(/take the radio down until someone power-cycles it/))
    await waitFor(() => {
      if (applyBtn().disabled) throw new Error('still disabled after acknowledging')
    })

    fireEvent.click(screen.getByText('Preview changes'))
    // Wait for the preview to FINISH first. Apply is also disabled while
    // busy==='preview', so asserting straight after the click passes on the
    // transient state and says nothing about the acknowledgement — which is
    // exactly how the first version of this test passed with the reset removed.
    await waitFor(() => expect(api.preview).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(screen.getByText('Preview changes')).toBeTruthy())
    if (!applyBtn().disabled) {
      throw new Error('a stale acknowledgement carried to a fresh preview')
    }
  })
})
