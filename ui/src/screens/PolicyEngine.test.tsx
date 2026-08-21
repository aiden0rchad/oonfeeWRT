import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'

const api = {
  site: vi.fn(),
  saveZonePolicy: vi.fn(),
  resetZonePolicy: vi.fn(),
  policies: vi.fn(),
  savePolicy: vi.fn(),
  deletePolicy: vi.fn(),
  clients: vi.fn(),
  saveClientPolicy: vi.fn(),
  compilePolicyObjects: vi.fn(),
}

vi.mock('../lib/api', () => ({ api }))

const { PolicyEngine } = await import('./PolicyEngine')

function zone(name: string, forwardTo: string[], explicit = true) {
  return { name, forward_to: forwardTo, explicit }
}

function site(zones: ReturnType<typeof zone>[]) {
  return {
    name: 'Lab', uuid: '12345678-0000-0000-0000-000000000000',
    wlans: [], meshes: [], uplinks: [], groups: [], networks: [], zones,
    problems: [], overrides: [], overridable: [], override_note: '',
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  api.policies.mockResolvedValue({ rows: [], capabilities: [] })
  api.clients.mockResolvedValue({
    clients: [], total: 0, limit: 5000, offset: 0,
    facets: { presence: [], connection: [], scope: [] }, note: '', scope_note: '',
  })
})

describe('Policy Engine', () => {
  it('moves roving tab focus with arrows and Home/End while keeping one tab stop', async () => {
    api.site.mockResolvedValue(site([zone('Office', ['wan'], false)]))
    render(<PolicyEngine />)

    expect(await screen.findByRole('heading', { level: 1, name: 'Policy Engine' })).toBeTruthy()
    const objects = await screen.findByRole('tab', { name: 'Objects' })
    const master = screen.getByRole('tab', { name: 'Master Table' })
    const zones = screen.getByRole('tab', { name: 'Zone Matrix' })
    expect(zones.getAttribute('aria-selected')).toBe('true')
    expect(zones.tabIndex).toBe(0)
    expect(objects.tabIndex).toBe(-1)
    expect(master.tabIndex).toBe(-1)

    zones.focus()
    fireEvent.keyDown(zones, { key: 'ArrowRight' })
    expect(objects.getAttribute('aria-selected')).toBe('true')
    expect(document.activeElement).toBe(objects)
    expect(objects.tabIndex).toBe(0)
    expect(zones.tabIndex).toBe(-1)

    fireEvent.keyDown(objects, { key: 'ArrowRight' })
    expect(master.getAttribute('aria-selected')).toBe('true')
    expect(document.activeElement).toBe(master)

    fireEvent.keyDown(master, { key: 'End' })
    expect(zones.getAttribute('aria-selected')).toBe('true')
    expect(document.activeElement).toBe(zones)

    fireEvent.keyDown(zones, { key: 'Home' })
    expect(objects.getAttribute('aria-selected')).toBe('true')
    expect(document.activeElement).toBe(objects)

    fireEvent.keyDown(objects, { key: 'ArrowLeft' })
    expect(zones.getAttribute('aria-selected')).toBe('true')
    expect(document.activeElement).toBe(zones)
  })

  it('renders asymmetric forwarding as allow-all in one direction and return-only in the other', async () => {
    api.site.mockResolvedValue(site([
      zone('Office', ['Guest']),
      zone('Guest', []),
    ]))
    render(<PolicyEngine />)

    expect(await screen.findByRole('button', {
      name: 'Office to Guest: Allow All. Edit Office policy',
    })).toBeTruthy()
    expect(screen.getByRole('button', {
      name: 'Guest to Office: Allow Return Traffic. Edit Guest policy',
    })).toBeTruthy()
    const diagonal = screen.getByLabelText('Office to Office: Same zone')
    expect(diagonal.closest('button')).toBeNull()
    expect(diagonal.getAttribute('title')).toMatch(/Not firewall-controlled/i)
    expect(screen.getByText(/Same-zone traffic is not controlled here/i)).toBeTruthy()
    expect(screen.getAllByText('Allow Return Traffic').length).toBeGreaterThan(0)
    expect(screen.getByText(/Forwarded firewall and NAT changes govern new flows; existing tracked sessions and NAT mappings may persist until conntrack expiry/i)).toBeTruthy()
  })

  it('keeps an explicit empty forwarding list distinct from the legacy default', async () => {
    api.site.mockResolvedValue(site([zone('Guest', [])]))
    render(<PolicyEngine />)

    const outbound = await screen.findByRole('button', {
      name: 'Guest to Internet / WAN: Block All. Edit Guest policy',
    })
    expect(screen.getByLabelText(
      'Internet / WAN to Guest: Block All. Read only',
    )).toBeTruthy()
    fireEvent.click(outbound)
    const dialog = screen.getByRole('dialog', { name: 'Traffic from Guest' })
    expect((within(dialog).getByRole('checkbox', { name: 'Internet / WAN' }) as HTMLInputElement).checked).toBe(false)
    expect(within(dialog).getByRole('button', { name: 'Reset policy' })).toBeTruthy()
  })

  it('shows the effective legacy WAN default and does not offer a meaningless reset', async () => {
    api.site.mockResolvedValue(site([zone('Office', ['wan'], false)]))
    render(<PolicyEngine />)

    const outbound = await screen.findByRole('button', {
      name: 'Office to Internet / WAN: Allow All. Edit Office policy',
    })
    expect(screen.getByLabelText(
      'Internet / WAN to Office: Allow Return Traffic. Read only',
    )).toBeTruthy()
    expect(screen.getAllByText(/legacy default/i).length).toBeGreaterThan(0)
    fireEvent.click(outbound)
    const dialog = screen.getByRole('dialog', { name: 'Traffic from Office' })
    expect(within(dialog).queryByRole('button', { name: 'Reset policy' })).toBeNull()
    expect(within(dialog).getByText(/already uses the legacy default/i)).toBeTruthy()
  })

  it('keeps every WAN-source matrix cell read-only and points to explicit rules', async () => {
    api.site.mockResolvedValue(site([
      zone('Office', ['wan'], false),
      zone('Guest', []),
    ]))
    render(<PolicyEngine />)

    const inbound = await screen.findByLabelText(
      'Internet / WAN to Office: Allow Return Traffic. Read only',
    )
    expect(inbound.closest('button')).toBeNull()
    expect(inbound.getAttribute('title')).toMatch(/created as an explicit firewall rule or port forward/i)
    expect(screen.queryByRole('button', { name: /Edit Internet \/ WAN policy/i })).toBeNull()
    expect(document.body.textContent).toMatch(
      /WAN-initiated allow rules, port forwards, per-client or per-port rules, application filtering, QoS, and DPI are not implemented by this Zone Matrix editor/i,
    )
    expect(document.body.textContent).toMatch(
      /explicit gateway policies it also reads active nftables transit hooks and reachable rules/i,
    )
    expect(document.body.textContent).toMatch(
      /terse runtime view cannot prove include-file provenance or inspect set contents/i,
    )
    expect(document.body.textContent).toMatch(
      /!fw4: attribution comments can be imitated by direct custom rules/i,
    )
  })

  it('saves the complete source forwarding list from the slide-over', async () => {
    api.site.mockResolvedValue(site([
      zone('Office', ['wan'], false),
      zone('Guest', []),
    ]))
    api.saveZonePolicy.mockResolvedValue(zone('Office', ['Guest', 'wan']))
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('button', {
      name: 'Office to Guest: Block All. Edit Office policy',
    }))
    const dialog = screen.getByRole('dialog', { name: 'Traffic from Office' })
    expect((within(dialog).getByRole('checkbox', { name: 'Internet / WAN' }) as HTMLInputElement).checked).toBe(true)
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Guest' }))
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save source policy' }))

    await waitFor(() => expect(api.saveZonePolicy).toHaveBeenCalledWith(
      'Office', ['Guest', 'wan'],
    ))
    expect(await screen.findByText(/Office policy saved as desired state/i)).toBeTruthy()
    expect(screen.getByText(/Preview and Apply are still required/i)).toBeTruthy()
  })

  it('warns that reset restores source-to-WAN, then renders the returned legacy state', async () => {
    api.site.mockResolvedValue(site([
      zone('Office', ['Guest']),
      zone('Guest', []),
    ]))
    api.resetZonePolicy.mockResolvedValue(zone('Office', ['wan'], false))
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('button', {
      name: 'Office to Internet / WAN: Block All. Edit Office policy',
    }))
    const dialog = screen.getByRole('dialog', { name: 'Traffic from Office' })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Reset policy' }))
    expect(within(dialog).getByText(/Office → Internet \/ WAN is allowed/i)).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Restore legacy default' }))

    await waitFor(() => expect(api.resetZonePolicy).toHaveBeenCalledWith('Office'))
    expect(await screen.findByRole('button', {
      name: 'Office to Internet / WAN: Allow All. Edit Office policy',
    })).toBeTruthy()
  })

  it('shows load errors instead of an empty policy verdict', async () => {
    api.site.mockRejectedValue(new Error('controller unavailable'))
    render(<PolicyEngine />)

    expect((await screen.findByRole('alert')).textContent).toContain('controller unavailable')
    expect(screen.queryByRole('table', { name: 'Zone policy matrix' })).toBeNull()
  })

  it('surfaces site validation problems instead of presenting the matrix as deployable', async () => {
    const invalid = {
      ...site([zone('Office', ['wan'], false)]),
      problems: ['zone policy "Guest" is not an active managed zone'],
    }
    api.site.mockResolvedValue(invalid)
    render(<PolicyEngine />)

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain('Preview and Apply will refuse it')
    expect(alert.textContent).toContain('zone policy "Guest" is not an active managed zone')
    expect(screen.getByRole('table', { name: 'Zone policy matrix' })).toBeTruthy()
  })

  it('keeps the editor and last effective matrix visible when save fails', async () => {
    api.site.mockResolvedValue(site([zone('Office', ['wan'], false)]))
    api.saveZonePolicy.mockRejectedValue(new Error('policy conflict'))
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('button', {
      name: 'Office to Internet / WAN: Allow All. Edit Office policy',
    }))
    const dialog = screen.getByRole('dialog', { name: 'Traffic from Office' })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save source policy' }))

    expect((await within(dialog).findByRole('alert')).textContent).toContain('policy conflict')
    expect(screen.getByRole('table', { name: 'Zone policy matrix' })).toBeTruthy()
    expect(screen.getByRole('dialog', { name: 'Traffic from Office' })).toBeTruthy()
  })

  it('shows a unified gated master table and creates strict desired rules without claiming precedence', async () => {
    const master = {
      ...site([zone('Guest', [])]),
      networks: [{ id: 7, name: 'Guest', vlan: 20, cidr: '192.168.20.1/24', zone: 'Guest', enabled: true }],
      policies: [{
        id: 'policy:4', record_id: 4, origin: 'manual', kind: 'port_forward',
        name: 'Camera HTTPS', enabled: true, order: 200, order_scope: 'firewall',
        effective_scope: { source_zone: 'wan', destination_zone: 'Guest', destination_ip: '192.168.20.10' },
        mutable: true, renderable: false,
        gated_reason: 'Gateway WRT reports firewall4 unknown; re-probe after restoring the nft read grant',
        rule: {
          source_zone: 'wan', destination_zone: 'Guest', protocols: ['tcp'],
          external_port: 443, destination_ip: '192.168.20.10', destination_port: 443,
        },
      }],
      policy_capabilities: [{ kind: 'firewall', available: false, reason: 'firewall backend unknown' }],
    }
    api.site.mockResolvedValue(master)
    api.savePolicy.mockResolvedValue({
      id: 9, order: 0, name: 'Block WAN input', kind: 'firewall_rule',
      origin: 'manual', enabled: true,
      firewall: { action: 'reject', source_zone: 'wan', destination_zone: '', protocols: ['all'] },
    })
    api.policies.mockResolvedValue({ rows: master.policies, capabilities: master.policy_capabilities })
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('tab', { name: 'Master Table' }))
    expect(await screen.findByText('Camera HTTPS')).toBeTruthy()
    expect(screen.getByText(/Gateway WRT reports firewall4 unknown/i)).toBeTruthy()
    expect(screen.getByText(/address families: ipv4/i)).toBeTruthy()
    expect(screen.getByText(/Display order groups records for inspection; it does not claim UCI evaluation precedence/i)).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Create rule' }))
    const dialog = screen.getByRole('dialog', { name: 'Create policy rule' })
    expect(within(dialog).getByText(/explicit firewall rule is IPv4-only. IPv6 traffic is unaffected/i)).toBeTruthy()
    fireEvent.change(within(dialog).getByLabelText('Name'), { target: { value: 'Block WAN input' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save desired rule' }))

    await waitFor(() => expect(api.savePolicy).toHaveBeenCalledWith(expect.objectContaining({
      id: undefined,
      name: 'Block WAN input',
      kind: 'firewall_rule',
      enabled: true,
      firewall: expect.objectContaining({
        action: 'reject', source_zone: 'wan', destination_zone: '', protocols: ['all'],
      }),
    })))
    expect(await screen.findByText(/Block WAN input saved as desired state/i)).toBeTruthy()
  })

  it('edits master rows at their source and confirms desired-rule deletion', async () => {
    const row = {
      id: 'policy:4', record_id: 4, origin: 'manual', kind: 'static_route',
      name: 'Lab route', enabled: true, order: 300, order_scope: 'network_route',
      effective_scope: { target: '10.40.0.0/16', via: 'wan', gateway: '192.168.1.2' },
      mutable: true, renderable: true,
      rule: { network_id: 0, target: '10.40.0.0/16', gateway: '192.168.1.2', metric: 10 },
    }
    api.site.mockResolvedValue({ ...site([zone('Guest', [])]), policies: [row], policy_capabilities: [] })
    api.deletePolicy.mockResolvedValue({ deleted: 4, note: 'desired state removed' })
    api.policies.mockResolvedValue({ rows: [], capabilities: [] })
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('tab', { name: 'Master Table' }))
    const edit = await screen.findByRole('button', { name: 'Edit Lab route' })
    edit.focus()
    fireEvent.keyDown(edit, { key: 'Enter' })
    const dialog = screen.getByRole('dialog', { name: 'Edit Lab route' })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete rule' }))
    expect(within(dialog).getByText(/Existing router state remains unchanged until Preview and Apply/i)).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete desired rule' }))

    await waitFor(() => expect(api.deletePolicy).toHaveBeenCalledWith(4))
    expect(await screen.findByText(/Lab route removed from desired state/i)).toBeTruthy()
  })

  it('compiles Object Manager drafts and verbatim QoS gates without persisting, then saves only by a separate action', async () => {
    const objectSite = {
      ...site([zone('Guest', [])]),
      networks: [{ id: 7, name: 'Guest', vlan: 20, cidr: '192.168.20.1/24', zone: 'Guest', enabled: true }],
      policies: [],
      policy_capabilities: [
        { kind: 'firewall', available: true },
        { kind: 'qos', available: false, reason: 'unavailable: no observed SQM/tc backend' },
        { kind: 'application', available: false, reason: 'unavailable: DPI capability is not observed' },
      ],
    }
    const compiled = {
      id: 0, order: 0, name: 'Secure network 7', kind: 'firewall_rule',
      origin: 'object_manager', enabled: true,
      firewall: { action: 'reject', source_zone: 'Guest', destination_zone: 'wan', protocols: ['all'] },
    }
    api.site.mockResolvedValue(objectSite)
    api.compilePolicyObjects.mockResolvedValue({
      drafts: [compiled],
      gates: [{
        object: { kind: 'network', id: '7' }, outcome: 'qos',
        reason: 'QoS is unavailable: this build does not observe or own an SQM/tc backend; no rate-limit rule was invented',
      }],
      persisted: false, applied: false,
      note: 'Object Manager only compiled inspectable drafts.',
    })
    api.savePolicy.mockResolvedValue({ ...compiled, id: 11, order: 100 })
    api.policies.mockResolvedValue({ rows: [], capabilities: objectSite.policy_capabilities })
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('tab', { name: 'Objects' }))
    const objectSelect = await screen.findByLabelText('Object')
    fireEvent.change(objectSelect, { target: { value: '7' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add object' }))
    expect(screen.getByRole('checkbox', {
      name: /Secure.*affects new routed flows; existing conntrack sessions may continue until expiry/i,
    })).toBeTruthy()
    fireEvent.click(screen.getByRole('checkbox', { name: /QoS/i }))
    fireEvent.click(screen.getByRole('button', { name: 'Compile visible drafts' }))

    await waitFor(() => expect(api.compilePolicyObjects).toHaveBeenCalledWith(
      [{ kind: 'network', id: '7' }],
      [
        { kind: 'secure', destination_zone: 'wan' },
        { kind: 'qos', rate_kbps: 10000 },
      ],
    ))
    expect(await screen.findByText(/Not persisted · Not applied/i)).toBeTruthy()
    expect(screen.getByText(/QoS is unavailable: this build does not observe or own an SQM\/tc backend; no rate-limit rule was invented/i)).toBeTruthy()
    expect(screen.getByLabelText('Concrete rule for Secure network 7').textContent).toContain('"destination_zone": "wan"')
    expect(screen.getByText(/Concrete scope:/).textContent).toMatch(/IPv4 only.*Guest.*wan/i)
    expect(api.savePolicy).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: 'Save this draft' }))
    await waitFor(() => expect(api.savePolicy).toHaveBeenCalledWith(expect.objectContaining({
      id: undefined, name: 'Secure network 7', origin: 'object_manager',
    })))
    expect(await screen.findByRole('button', { name: 'Saved to desired state' })).toBeTruthy()
  })

  it('removes compiled drafts after source, outcome, or parameter changes so stale output cannot be saved', async () => {
    const objectSite = {
      ...site([zone('Guest', [])]),
      networks: [{ id: 7, name: 'Guest', vlan: 20, cidr: '192.168.20.1/24', zone: 'Guest', enabled: true }],
      policies: [], policy_capabilities: [],
    }
    const compiled = {
      id: 0, order: 0, name: 'Secure network 7', kind: 'firewall_rule',
      origin: 'object_manager', enabled: true,
      firewall: { action: 'reject', source_zone: 'Guest', destination_zone: 'wan', protocols: ['all'] },
    }
    api.site.mockResolvedValue(objectSite)
    api.compilePolicyObjects.mockResolvedValue({
      drafts: [compiled], gates: [], persisted: false, applied: false, note: 'Inspectable draft.',
    })
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('tab', { name: 'Objects' }))
    fireEvent.change(await screen.findByLabelText('Object'), { target: { value: '7' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add object' }))
    fireEvent.click(screen.getByRole('button', { name: 'Compile visible drafts' }))
    expect(await screen.findByRole('button', { name: 'Save this draft' })).toBeTruthy()

    fireEvent.change(screen.getByLabelText('Secure against destination'), { target: { value: 'Guest' } })
    expect(screen.queryByRole('button', { name: 'Save this draft' })).toBeNull()

    fireEvent.click(screen.getByRole('button', { name: 'Compile visible drafts' }))
    expect(await screen.findByRole('button', { name: 'Save this draft' })).toBeTruthy()
    fireEvent.click(screen.getByRole('checkbox', { name: /Secure/i }))
    expect(screen.queryByRole('button', { name: 'Save this draft' })).toBeNull()

    fireEvent.click(screen.getByRole('checkbox', { name: /Secure/i }))
    fireEvent.click(screen.getByRole('button', { name: 'Compile visible drafts' }))
    expect(await screen.findByRole('button', { name: 'Save this draft' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Remove network 7' }))
    expect(screen.queryByRole('button', { name: 'Save this draft' })).toBeNull()
    expect(api.savePolicy).not.toHaveBeenCalled()
  })

  it('marks capped client and group choices as an incomplete subset', async () => {
    const client = {
      mac: 'aa:bb:cc:dd:ee:ff', name: 'Tablet', ipv4: '192.168.20.22', group: 'Kids',
      first_seen: 1, last_seen: 2, blocked: false, connection: 'wireless',
      online: true, scope: 'local',
    }
    api.site.mockResolvedValue(site([zone('Guest', [])]))
    api.clients.mockResolvedValue({
      clients: [client], total: 6001, limit: 5000, offset: 0,
      facets: { presence: [], connection: [], scope: [] }, note: '', scope_note: '',
    })
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('tab', { name: 'Objects' }))
    const warning = await screen.findByRole('alert')
    expect(warning.textContent).toMatch(/incomplete: 1 of 6001 clients were returned/i)
    expect(warning.textContent).toMatch(/absence from these lists does not mean a client or group does not exist/i)
    fireEvent.change(screen.getByLabelText('Object type'), { target: { value: 'device' } })
    expect(await screen.findByLabelText('Object (partial client inventory)')).toBeTruthy()
    expect(screen.getByLabelText('Client (partial client inventory)')).toBeTruthy()
  })

  it('replaces and then clears site validation problems on policy refresh', async () => {
    const initial = { ...site([zone('Guest', [])]), problems: ['stale validation problem'] }
    const updated = { ...initial, problems: ['fresh validation problem'] }
    const clean = { ...initial, problems: [] }
    api.site
      .mockResolvedValueOnce(initial)
      .mockResolvedValueOnce(updated)
      .mockResolvedValueOnce(clean)
    api.savePolicy.mockImplementation(async (policy) => ({ ...policy, id: 9 }))
    render(<PolicyEngine />)

    expect(await screen.findByText('stale validation problem')).toBeTruthy()
    fireEvent.click(screen.getByRole('tab', { name: 'Master Table' }))
    fireEvent.click(screen.getByRole('button', { name: 'Create rule' }))
    let dialog = screen.getByRole('dialog', { name: 'Create policy rule' })
    fireEvent.change(within(dialog).getByLabelText('Name'), { target: { value: 'First rule' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save desired rule' }))

    expect(await screen.findByText('fresh validation problem')).toBeTruthy()
    expect(screen.queryByText('stale validation problem')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Create rule' }))
    dialog = screen.getByRole('dialog', { name: 'Create policy rule' })
    fireEvent.change(within(dialog).getByLabelText('Name'), { target: { value: 'Second rule' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save desired rule' }))

    await waitFor(() => expect(screen.queryByText('fresh validation problem')).toBeNull())
    expect(api.site).toHaveBeenCalledTimes(3)
  })

  it('keeps a saved rule editor open, reports a failed parent refresh, and retries as an update', async () => {
    const currentSite = site([zone('Guest', [])])
    const saved = {
      id: 12, order: 0, name: 'Resilient rule', kind: 'firewall_rule',
      origin: 'manual', enabled: true,
      firewall: { action: 'reject', source_zone: 'wan', destination_zone: '', protocols: ['all'] },
    }
    api.site.mockResolvedValue(currentSite)
    api.savePolicy.mockResolvedValue(saved)
    api.policies.mockRejectedValueOnce(new Error('policy refresh unavailable'))
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('tab', { name: 'Master Table' }))
    fireEvent.click(screen.getByRole('button', { name: 'Create rule' }))
    let dialog = screen.getByRole('dialog', { name: 'Create policy rule' })
    fireEvent.change(within(dialog).getByLabelText('Name'), { target: { value: saved.name } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save desired rule' }))

    dialog = await screen.findByRole('dialog', { name: `Edit ${saved.name}` })
    expect((await within(dialog).findByRole('alert')).textContent).toContain(
      'Rule saved, but the table could not refresh: policy refresh unavailable',
    )

    fireEvent.click(within(dialog).getByRole('button', { name: 'Save desired rule' }))
    await waitFor(() => expect(api.savePolicy).toHaveBeenCalledTimes(2))
    expect(api.savePolicy).toHaveBeenLastCalledWith(expect.objectContaining({ id: 12 }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: `Edit ${saved.name}` })).toBeNull())
  })

  it('labels Client Block scope honestly and saves block, fixed IP and group as desired state', async () => {
    const client = {
      mac: 'aa:bb:cc:dd:ee:ff', name: 'Tablet', ipv4: '192.168.20.22',
      first_seen: 1, last_seen: 2, blocked: false, connection: 'wireless',
      online: true, scope: 'local',
    }
    api.site.mockResolvedValue({
      ...site([zone('Guest', [])]),
      policies: [{
        id: `client:block:${client.mac}`, origin: 'client', kind: 'client_block',
        name: `Block ${client.mac}`, enabled: true, order: 1000000,
        order_scope: 'display_only', mutable: true, renderable: true,
        effective_scope: {
          client_mac: client.mac, source_zones: ['Guest'], traffic: 'routed_forwarding',
          destination_zones: 'any', address_families: ['ipv4', 'ipv6'],
          excludes: ['router_input', 'same_l2'],
        },
        rule: { blocked: true },
      }],
      policy_capabilities: [],
    })
    api.clients.mockResolvedValue({
      clients: [client], total: 1, limit: 5000, offset: 0,
      facets: { presence: [], connection: [], scope: [] }, note: '', scope_note: '',
    })
    api.saveClientPolicy.mockResolvedValue({
      client: { mac: client.mac, blocked: true, fixed_ip: '192.168.20.50', group: 'Kids' },
      note: 'desired state saved',
    })
    render(<PolicyEngine />)

    fireEvent.click(await screen.findByRole('tab', { name: 'Master Table' }))
    expect(await screen.findByText(/including foreign management LAN; excludes DHCP\/DNS\/router input and same-L2; existing conntrack sessions may continue until expiry/i)).toBeTruthy()
    fireEvent.click(await screen.findByRole('tab', { name: 'Objects' }))
    expect(await screen.findByText(/Client Block covers IPv4 and IPv6 traffic routed from managed zones to every destination/i)).toBeTruthy()
    fireEvent.click(await screen.findByRole('button', { name: 'Edit selected client' }))
    const dialog = screen.getByRole('dialog', { name: 'Client policy · Tablet' })
    expect(within(dialog).getByText(/Covers every routed destination, including a foreign management LAN.*Router input such as DHCP\/DNS and same-L2 traffic are excluded/i)).toBeTruthy()
    expect(within(dialog).getByText(/Blocking affects new routed flows; existing conntrack sessions may continue until expiry/i)).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Block IPv4 and IPv6 managed-zone forwarded traffic' }))
    fireEvent.change(within(dialog).getByLabelText('Fixed IPv4 (blank clears)'), { target: { value: '192.168.20.50' } })
    fireEvent.change(within(dialog).getByLabelText('Client group (blank clears)'), { target: { value: 'Kids' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save client policy' }))

    await waitFor(() => expect(api.saveClientPolicy).toHaveBeenCalledWith(client.mac, {
      blocked: true, fixed_ip: '192.168.20.50', group: 'Kids',
    }))
    expect(await screen.findByText(/client policy saved as desired state/i)).toBeTruthy()
  })
})
