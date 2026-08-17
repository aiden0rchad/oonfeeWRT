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
  devices: vi.fn(),
  saveWLAN: vi.fn(),
  noteForeign: vi.fn(),
  lastNeighbours: vi.fn(),
  events: vi.fn(),
  site: vi.fn(),
  saveMesh: vi.fn(),
  deleteMesh: vi.fn(),
  preview: vi.fn(),
  applySite: vi.fn(),
  device: vi.fn(),
  deviceSeries: vi.fn(),
  overhead: vi.fn(),
  reprobe: vi.fn(),
  distributeNeighbours: vi.fn(),
  meshHealth: vi.fn(),
  saveUplink: vi.fn(),
  deleteUplink: vi.fn(),
  unadopt: vi.fn(),
}

vi.mock('../lib/api', () => ({
  api,
  ApiError: class extends Error {},
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
vi.mock('../lib/live', () => ({
  live: {
    watch: () => () => {},
    on: (h: (msg: unknown) => void) => {
      liveHandlers.push(h)
      return () => {
        const i = liveHandlers.indexOf(h)
        if (i >= 0) liveHandlers.splice(i, 1)
      }
    },
    connect: () => {},
  },
}))

// The Clients grid resolves its "Access point" column against the fleet roster.
// Defaulted here rather than in each test: a screen that throws because an
// auxiliary fetch was not stubbed fails for a reason unrelated to what the test
// is about, which is how two unrelated Clients tests broke when the column
// landed.
api.devices.mockResolvedValue({ devices: [] })
// The 802.11k card asks what the last automatic cycle did, on mount.
api.lastNeighbours.mockResolvedValue({ ran: false })

const { Clients } = await import('./Clients')
const { Settings } = await import('./Settings')
const { DeviceClass, Devices } = await import('./Devices')
// Dynamic, like the others: a static import evaluates the module before the
// api mock is registered, and the factory then reads `api` in its TDZ.
const { Logs } = await import('./Logs')

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
})

describe('Clients', () => {
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
    await waitFor(() => expect(api.clients).toHaveBeenCalled())

    // Turn a page, then change a filter.
    fireEvent.click(screen.getByText('Next'))
    await waitFor(() =>
      expect(api.clients.mock.calls.at(-1)?.[0].offset).toBeGreaterThan(0),
    )
    fireEvent.click(screen.getByText('upstream'))

    await waitFor(() => {
      const last = api.clients.mock.calls.at(-1)?.[0]
      expect(last.scope).toBe('upstream')
      expect(last.offset).toBe(0)
    })
  })

  // A dropped request must not blank the grid: "no clients" is a different
  // claim from "the last fetch failed", and only one of them is true.
  it('keeps the last good page when a refresh fails', async () => {
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
    render(<Clients />)
    await waitFor(() => expect(screen.getByText('laptop')).toBeTruthy())

    api.clients.mockRejectedValueOnce(new Error('network down'))
    // Any filter change refetches. "All" in the first rail group, rather than a
    // named option: "online" appears both as a rail option and as a row's
    // status pill, so matching it by text is ambiguous.
    fireEvent.click(screen.getAllByText('All')[0])

    await waitFor(() => expect(screen.getByText('network down')).toBeTruthy())
    // Still there.
    expect(screen.getByText('laptop')).toBeTruthy()
  })

  // On a multi-AP controller, which AP a client is on is the most useful thing
  // on the row — and it was computed by the API, typed in the client, and shown
  // nowhere. The name must be resolved against the roster, and a client no
  // focused poll has covered must say so rather than render an empty cell that
  // reads as "on no access point".
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
    const why = document.querySelectorAll('[title*="focused poll tier"]')
    expect(why.length).toBeGreaterThan(0)
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
      { id: 1, name: 'lan', vlan: 1, cidr: '192.168.1.1/24', zone: 'lan', enabled: true },
    ],
    problems: [],
    overrides: [],
    overridable: [],
    override_note: '',
  }

  beforeEach(() => {
    api.site.mockResolvedValue(site)
  })

  // The rule this whole design rests on: editing an encrypted mesh without
  // retyping the passphrase must NOT read as "make it open". The list omits the
  // key, so a round-trip sends an empty one — and treating that as open would
  // strip encryption from a backhaul during a rename.
  it('does not warn about an open mesh when editing an encrypted one', async () => {
    render(<Settings devices={[]} />)
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
})

describe('Devices — re-probe panel', () => {
  const detail = {
    id: 1,
    mac: '60:38:e0:00:00:01',
    name: 'ap-1',
    host: '192.168.1.1',
    role: 'ap',
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
  })

  async function openPanel() {
    const { Devices } = await import('./Devices')
    render(
      <Devices
        devices={[{ ...detail, quiesced: false } as never]}
        onAdopt={() => {}}
        onChanged={() => {}}
      />,
    )
    fireEvent.click(screen.getByText('ap-1'))
    await waitFor(() => expect(screen.getByText('Re-probe capabilities')).toBeTruthy())
  }

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

    await waitFor(() =>
      expect(screen.getByText(/reported no radios/i)).toBeTruthy(),
    )
  })
})

describe('Settings — neighbour reports', () => {
  const wlan = (over: Record<string, unknown> = {}) => ({
    id: 1,
    ssid: 'oonfee-roam',
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
      { id: 1, name: 'lan', vlan: 1, cidr: '192.168.1.1/24', zone: 'lan', enabled: true },
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
    expect(within(card).getByText('oonfee-roam')).toBeTruthy()
    expect((screen.getByText('Distribute now') as HTMLButtonElement).disabled).toBe(false)
  })

  // The distinction the whole capability model exists to protect, at the UI
  // layer. "Could not reach this device" is something to go and fix now; "this
  // device was adopted before the controller could ask" is a standing fact that
  // will not change until it is re-adopted. Rendering both as an error teaches
  // people to ignore errors.
  it('separates a device that failed from one that was skipped', async () => {
    api.site.mockResolvedValue(siteWith([wlan()]))
    api.distributeNeighbours.mockResolvedValue({
      ssids: ['oonfee-roam'],
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
              ssid: 'oonfee-roam',
              bssid: '32:23:03:db:be:43',
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
        { device_id: 3, name: 'ap-gone', updated: 0, unchanged: 0, error: 'could not reach this device' },
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
      ssids: ['oonfee-roam'],
      updated: 0,
      unchanged: 4,
      devices: [],
    })

    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getByText(/Neighbour reports/)).toBeTruthy())
    fireEvent.click(screen.getByText('Distribute now'))

    await waitFor(() =>
      expect(screen.getByText(/already up to date/)).toBeTruthy(),
    )
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
    expect(container.querySelector('[title]')?.getAttribute('title')).toMatch(
      /not classified this device/,
    )
  })
})

describe('Devices — column preferences', () => {
  // The screen an operator looks at most was the one grid with no column
  // customisation. Its headers were not draggable, there was no picker, and
  // nothing said why — so trying to reorder read as a broken feature rather
  // than an absent one.
  const device = {
    id: 1,
    mac: '30:23:03:db:be:40',
    name: 'ap-one',
    host: '192.168.1.1',
    class: 'A',
    firmware: 'OpenWrt 25.12.5',
    online: true,
    adopted: true,
    role: 'ap',
    last_seen: 1,
  }

  it('makes the device grid reorderable like every other grid', () => {
    // A row is required: an empty grid renders its empty state instead of a
    // header, and there is nothing to customise when there are no columns on
    // screen.
    render(<Devices devices={[device] as never} />)
    expect(screen.getByText(/Customize columns/)).toBeTruthy()
    for (const th of screen.getAllByRole('columnheader')) {
      expect(th.getAttribute('draggable')).toBe('true')
    }
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
      { id: 1, name: 'lan', vlan: 1, cidr: '192.168.1.1/24', zone: 'lan', enabled: true },
    ],
    problems: [],
    overrides: [],
    overridable: [],
    override_note: '',
  }

  const wlan = (over: Record<string, unknown> = {}) => ({
    id: 1, ssid: 'oonfee-roam', network_id: 1, group_id: 1,
    bands: ['5g'], security_mode: 'psk2', pmf: '1', has_key: true, enabled: true,
    roaming: { ft: true, ft_over_ds: true, kv: true, ft_with_psk2: true },
    hidden: false, isolate: false, max_assoc: 0, allow_uplink: false,
    ...over,
  })

  // PMF was carried by the model, written by the renderer onto every WLAN, and
  // exposed nowhere — hardcoded to "1" at creation. That made the driver-defect
  // warning unactionable: it told an operator to turn PMF off on hardware that
  // cannot do it, with nowhere to do so.
  it('lets PMF be changed, and hides it where WPA3 mandates it', async () => {
    api.site.mockResolvedValue({ ...base, wlans: [wlan({ security_mode: 'psk2' })] })
    api.saveWLAN.mockResolvedValue({})
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getAllByText('oonfee-roam').length).toBeGreaterThan(0))

    // The row's own Edit button; the SSID text is not the control.
    fireEvent.click(screen.getAllByText('Edit')[0])
    await waitFor(() =>
      expect(screen.getByText('Protected management frames')).toBeTruthy(),
    )
    // All three states reachable — "Disabled" is the one the warning asks for.
    expect(screen.getByText('Disabled')).toBeTruthy()
    expect(screen.getByText('Required')).toBeTruthy()

    // WPA3 mandates PMF and the renderer forces it back on regardless, so
    // offering a choice it would override is worse than offering none.
    fireEvent.click(screen.getByText('WPA3 only'))
    await waitFor(() =>
      expect(screen.queryByText('Protected management frames')).toBeNull(),
    )

    // Enhanced open mandates PMF exactly as WPA3 does, so the control is
    // hidden for the same reason. Untested, this guard could be reverted
    // silently and an "owe" WLAN would store pmf="0" while the device got 2.
    fireEvent.click(screen.getByText('Enhanced open'))
    await waitFor(() =>
      expect(screen.queryByText('Protected management frames')).toBeNull(),
    )

    // Open has no RSN at all, so there is nothing to protect. This is the case
    // that used to leave a WLAN carrying the pmf="1" every draft is created
    // with, rendered onto the device where nobody could see or clear it.
    fireEvent.click(screen.getByText('Open'))
    await waitFor(() =>
      expect(screen.queryByText('Protected management frames')).toBeNull(),
    )

    // Back to WPA2 and choose Disabled — the state the lab device is in, and
    // the one that makes the next step interesting.
    fireEvent.click(screen.getByText('WPA2 only'))
    await waitFor(() => expect(screen.getByText('Disabled')).toBeTruthy())
    fireEvent.click(screen.getByText('Disabled'))

    // Transitional WPA2/WPA3 keeps the control but must not offer Disabled:
    // that silently removes the WPA3 half of a network still advertising it.
    fireEvent.click(screen.getByText('WPA2/WPA3'))
    await waitFor(() =>
      expect(screen.getByText('Protected management frames')).toBeTruthy(),
    )
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
        'saved pmf="0" for a WPA2/WPA3 network, which the picker does not ' +
          'offer and the renderer would override',
      )
    }
  })

  // A detour through a mode that hides PMF must not silently rewrite a
  // deliberate choice. Required -> Open -> WPA2 used to come back Disabled:
  // the clamp read the already-clamped draft rather than what was picked, and
  // the apply preview names a section and an option count, never a value, so
  // the downgrade appeared nowhere.
  it('keeps a deliberate PMF choice across a mode detour', async () => {
    api.site.mockResolvedValue({ ...base, wlans: [wlan({ security_mode: 'psk2' })] })
    api.saveWLAN.mockResolvedValue({})
    render(<Settings devices={[]} />)
    await waitFor(() => expect(screen.getAllByText('oonfee-roam').length).toBeGreaterThan(0))
    fireEvent.click(screen.getAllByText('Edit')[0])
    await waitFor(() =>
      expect(screen.getByText('Protected management frames')).toBeTruthy(),
    )

    fireEvent.click(screen.getByText('Required'))
    fireEvent.click(screen.getByText('Open'))
    await waitFor(() =>
      expect(screen.queryByText('Protected management frames')).toBeNull(),
    )
    fireEvent.click(screen.getByText('WPA2 only'))
    await waitFor(() => expect(screen.getByText('Disabled')).toBeTruthy())

    fireEvent.click(screen.getByText('Save'))
    await waitFor(() => expect(api.saveWLAN).toHaveBeenCalled())
    const saved = api.saveWLAN.mock.calls.at(-1)?.[0] as { pmf?: string }
    if (saved.pmf !== '2') {
      throw new Error(
        `a detour through Open turned a deliberate "Required" into pmf="${saved.pmf}"`,
      )
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
    await waitFor(() => expect(screen.getAllByText('oonfee-roam').length).toBeGreaterThan(0))
    fireEvent.click(screen.getAllByText('Edit')[0])
    await waitFor(() =>
      expect(screen.getByText('Protected management frames')).toBeTruthy(),
    )

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
            error:
              'could not list wireless interfaces: ubus iwinfo.devices: access denied',
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

    await waitFor(() => expect(screen.getByText('Wireless uplinks')).toBeTruthy())
    expect(screen.getByText(/No network accepts wireless bridges yet/)).toBeTruthy()
    expect(screen.queryByText('Add uplink')).toBeNull()
  })

  it('offers the add form once a network accepts bridges', async () => {
    api.site.mockResolvedValue({ ...base, wlans: [wlan({ allow_uplink: true })] })
    render(<Settings devices={[{ id: 7, name: 'no-cable' } as never]} />)

    await waitFor(() => expect(screen.getByText('Wireless uplinks')).toBeTruthy())
    expect(screen.getByText('Add uplink')).toBeTruthy()
    expect(screen.getByText(/No device is using a wireless uplink/)).toBeTruthy()
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

    await waitFor(() => expect(screen.getByText('Wireless uplinks')).toBeTruthy())
    expect(screen.getByText(/joins oonfee-roam on 5g/)).toBeTruthy()
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

    await waitFor(() =>
      expect(screen.getByText(/removes the station interface/)).toBeTruthy(),
    )
  })
})

describe('Devices — BSS provenance', () => {
  // A BSS the detail response does not mention must not render as one we
  // manage. The list is painted from the live frame while provenance comes
  // from the REST detail refreshed every 30s, so a missing entry is reachable
  // with no device quirk at all: on a freshly adopted device, or for 30s after
  // a daemon restart, every foreign SSID read as managed.
  it('treats a BSS with no provenance entry as unknown, not as ours', async () => {
    const dev = {
      id: 1, mac: 'aa:bb:cc:dd:ee:01', name: 'ap-1', host: '192.168.1.1',
      role: 'ap', adopted: true, online: true, class: 'A',
    }
    api.device.mockResolvedValue({
      ...dev,
      capabilities: null, interfaces: [], radios: [], stations: [],
      broadcast_known: true,
      // phy1-ap0 is deliberately ABSENT here while the live frame carries it.
      broadcasting: [
        {
          ssid: 'oonfee-roam', iface: 'phy0-ap1',
          section: 'oowrt_wlan1_radio0', origin: 'ours',
        },
      ],
    })
    api.deviceSeries.mockResolvedValue({ series: {} })
    api.overhead.mockRejectedValue(new Error('none'))

    const { Devices } = await import('./Devices')
    render(
      <Devices
        devices={[{ ...dev, quiesced: false } as never]}
        onAdopt={() => {}}
        onChanged={() => {}}
      />,
    )
    fireEvent.click(screen.getByText('ap-1'))
    await waitFor(() =>
      expect(screen.getByText('Re-probe capabilities')).toBeTruthy(),
    )

    // The live frame carries BOTH BSSes; the detail response knows only one.
    await act(async () => {
      pushLive({
        type: 'stats',
        device_id: 1,
        ts: 1755400000,
        tier: 'focused',
        uptime: 3600,
        load1: 0.1,
        poll_ms: 120,
        clients: 0,
        degraded: 0,
        aps: [
          { iface: 'phy0-ap1', ssid: 'oonfee-roam', channel: 36, freq: 5180, clients: 0 },
          { iface: 'phy1-ap0', ssid: 'somebody-elses', channel: 1, freq: 2412, clients: 0 },
        ],
        stations: [],
      })
    })

    await waitFor(() => expect(screen.getByText('somebody-elses')).toBeTruthy())

    // The unknown one must carry the marker. Without it the row renders exactly
    // like the managed BSS above it, which is the bug.
    const marks = document.querySelectorAll('[title*="not established"]')
    if (marks.length === 0) {
      throw new Error(
        'a BSS with no provenance entry rendered bare, identically to one ' +
          'oonfeeWRT manages',
      )
    }
  })
})

describe('Unadopt', () => {
  const dev = {
    id: 4, mac: 'aa:bb:cc:dd:ee:04', name: 'ap-c6', host: '192.168.1.2',
    role: 'ap', adopted: true, online: true,
  }

  // Un-adopt is the most destructive thing the controller does and the one
  // operation with NO rollback armed — yet it reported a count, after the
  // fact, while the safer apply path got a full preview and a confirmation.
  it('names the sections it will revert, before anything is done', async () => {
    api.device.mockResolvedValue({
      ...dev, capabilities: null, interfaces: [], radios: [], stations: [],
      broadcast_known: false,
      owned_sections: ['wireless.oowrt_wlan1_radio0', 'wireless.oowrt_wlan1_radio1'],
    })
    const { Unadopt } = await import('./Unadopt')
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={() => {}} onCancel={() => {}} />)

    await waitFor(() =>
      expect(screen.getByText('wireless.oowrt_wlan1_radio0')).toBeTruthy(),
    )
    expect(screen.getByText('wireless.oowrt_wlan1_radio1')).toBeTruthy()
  })

  // A list that could not be read must not render as an empty one — that would
  // say "this controller wrote nothing here" about a device it may own plenty
  // of, immediately before an irreversible step.
  it('says when the section list could not be read', async () => {
    api.device.mockRejectedValue(new Error('nope'))
    const { Unadopt } = await import('./Unadopt')
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={() => {}} onCancel={() => {}} />)

    await waitFor(() =>
      expect(screen.getByText(/could not be read/)).toBeTruthy(),
    )
    expect(screen.getByText(/not the same as owning none/)).toBeTruthy()
  })

  // The same speed bump the apply path has, on the operation that needs it
  // more. Both destructive buttons stay disabled until it is ticked.
  it('will not remove anything until the operator confirms', async () => {
    api.device.mockResolvedValue({
      ...dev, capabilities: null, interfaces: [], radios: [], stations: [],
      broadcast_known: false, owned_sections: ['wireless.oowrt_wlan1_radio0'],
    })
    const { Unadopt } = await import('./Unadopt')
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={() => {}} onCancel={() => {}} />)
    await waitFor(() =>
      expect(screen.getByText('wireless.oowrt_wlan1_radio0')).toBeTruthy(),
    )

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

  // Reaches the form, confirms, and presses the destructive button.
  async function attemptRemoval() {
    api.device.mockResolvedValue({
      ...dev, capabilities: null, interfaces: [], radios: [], stations: [],
      broadcast_known: false, owned_sections: ['wireless.oowrt_wlan1_radio0'],
    })
    const { Unadopt } = await import('./Unadopt')
    const onDone = vi.fn()
    render(<Unadopt deviceID={4} deviceName="ap-c6" onDone={onDone} onCancel={() => {}} />)
    await waitFor(() =>
      expect(screen.getByText('wireless.oowrt_wlan1_radio0')).toBeTruthy(),
    )
    fireEvent.click(screen.getByText(/reverts the sections above/))
    await waitFor(() => {
      const r = screen.getByText('Remove completely').closest('button')!
      if (r.disabled) throw new Error('still disabled after confirming')
    })
    fireEvent.click(screen.getByText('Remove completely'))
    return onDone
  }

  // The API has taken `force` since un-adopt was written, and no screen ever
  // sent it. So a device that cannot be reached — dead hardware, a reflash, a
  // lost administrator password, and now a refused host key — could never leave
  // the inventory: it stayed listed, polled and counted forever, and the only
  // way out was a hand-written API call.
  it('offers a way out when the device cannot be reached at all', async () => {
    api.unadopt.mockRejectedValueOnce(
      new Error("the device's SSH host key changed"),
    )
    await attemptRemoval()

    await waitFor(() =>
      expect(screen.getByText(/SSH host key changed/)).toBeTruthy(),
    )
    const forceBtn = screen
      .getByText('Remove from the inventory anyway')
      .closest('button')!
    if (!forceBtn.disabled) {
      throw new Error('forced removal was available without its own confirmation')
    }

    api.unadopt.mockResolvedValueOnce({
      removed_from_inventory: true, footprint_remains: true,
      reverted_sections: 0, login_removed: false, acl_removed: false,
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

  // The report is the LAST copy of what is still installed on a device whose
  // row has just been deleted. It was rendered and discarded in the same tick:
  // onDone ran the moment the request returned, and it unmounts the panel.
  it('keeps the report on screen after the row is gone', async () => {
    api.unadopt.mockResolvedValueOnce({
      removed_from_inventory: true, footprint_remains: true,
      reverted_sections: 2, login_removed: false, acl_removed: false,
      residue: ['/usr/share/rpcd/acl.d/oonfeewrt.json'],
      errors: [],
    })
    const onDone = await attemptRemoval()

    await waitFor(() =>
      expect(screen.getByText('/usr/share/rpcd/acl.d/oonfeewrt.json')).toBeTruthy(),
    )
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
  const ev = (over: Record<string, unknown> = {}) => ({
    TS: 1755400000, DeviceID: null, Category: 'device', Severity: 'info',
    Event: 'device.reachable', Detail: {}, ...over,
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
        ev({ DeviceID: 7, Event: 'device.unreachable', Severity: 'warning' }),
        ev({ DeviceID: null, Event: 'auth.login', Category: 'audit' }),
      ],
      total: 2, limit: 100, offset: 0,
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
              { WLAN: 'lan', Reason: 'VLAN 1 and untagged traffic are the device’s existing LAN, which oonfeeWRT does not own and will not rewrite.' },
              { WLAN: 'lan', Reason: 'another long sentence that has no business being in a table cell at all' },
            ],
          },
        }),
      ],
      total: 1, limit: 100, offset: 0,
      facets: { category: [], severity: [] },
    })
    render(<Logs />)
    await waitFor(() => expect(screen.getByText('config.apply')).toBeTruthy())

    expect(screen.getByText(/omissions=2 items/)).toBeTruthy()
    // The prose must not be in the cell.
    expect(screen.queryByText(/will not rewrite/)).toBeNull()
  })
})
