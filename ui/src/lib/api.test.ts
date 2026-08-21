import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from './api'

function ok(body: unknown) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn())
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('response trust boundary', () => {
  it.each([
    ['empty', '', 503],
    ['plain text', 'upstream unavailable', 502],
    ['HTML', '<html><title>Bad Gateway</title></html>', 504],
  ])('preserves an %s error response as an HTTP ApiError', async (_kind, body, status) => {
    vi.mocked(fetch).mockResolvedValue(new Response(body, { status }))

    await expect(api.dashboard()).rejects.toMatchObject({
      status,
      message: `request failed (${status})`,
    } satisfies Partial<ApiError>)
  })

  it('wraps malformed JSON from a successful response instead of leaking SyntaxError', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response('{"devices":', {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }))

    let caught: unknown
    try {
      await api.dashboard()
    } catch (error) {
      caught = error
    }
    expect(caught).toBeInstanceOf(ApiError)
    expect(caught).not.toBeInstanceOf(SyntaxError)
    expect(caught).toMatchObject({
      status: 200,
      message: 'server returned an invalid response (200)',
    } satisfies Partial<ApiError>)
  })
})

describe('zone policy API contract', () => {
  it('POSTs the complete forward_to list to the encoded source path', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({
      name: 'Guest IoT', forward_to: ['Office', 'wan'], explicit: true,
    }))

    await api.saveZonePolicy('Guest IoT', ['Office', 'wan'])

    expect(fetch).toHaveBeenCalledTimes(1)
    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/site/zones/Guest%20IoT')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({ forward_to: ['Office', 'wan'] })
  })

  it('DELETEs the encoded source path to restore its legacy default', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({
      name: 'Guest IoT', forward_to: ['wan'], explicit: false,
    }))

    await api.resetZonePolicy('Guest IoT')

    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/site/zones/Guest%20IoT')
    expect(init?.method).toBe('DELETE')
    expect(init?.body).toBeUndefined()
  })
})

describe('event API contract', () => {
  it('sends paired keyset cursors and fetches an exact detail row', async () => {
    vi.mocked(fetch)
      .mockResolvedValueOnce(ok({ events: [], total: 0, limit: 50, scope: 'general', next_before: null, facets: { category: [], severity: [] }, coverage: { complete: true, expected_devices: 0, observed_devices: 0, gaps: [] } }))
      .mockResolvedValueOnce(ok({ ID: 9, TS: 1, Event: 'client.connect' }))

    await api.events({
      scope: 'general', limit: 50, before: { ts: 123, id: 9 },
      category: 'client', severity: 'warning',
    })
    await api.eventDetail(9)

    expect(vi.mocked(fetch).mock.calls[0][0]).toBe(
      '/api/v1/events?limit=50&scope=general&before_ts=123&before_id=9&category=client&severity=warning',
    )
    expect(vi.mocked(fetch).mock.calls[1][0]).toBe('/api/v1/events/9')
  })
})

describe('device ACL refresh API contract', () => {
  it('sends the administrator credential only in the CSRF-protected POST body', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({
      device_id: 7, name: 'AP', acl_updated: true, controller_verified: true, features: [],
    }))
    await api.refreshACL(7, {
      username: 'root', password: 'sentinel-password', private_key: 'sentinel-key',
      acknowledge_router_changes: true,
    })
    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/devices/7/refresh-acl')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({
      username: 'root', password: 'sentinel-password', private_key: 'sentinel-key',
      acknowledge_router_changes: true,
    })
  })
})

describe('device adoption API contract', () => {
  it('includes the explicit router-change acknowledgement in the POST body', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({ device_id: 9 }))
    await api.adopt({
      host: '192.0.2.9', username: 'root', password: 'sentinel-password',
      functions: ['ap'], role: 'ap', acknowledge_router_changes: true,
    })
    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/devices/adopt')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toMatchObject({
      acknowledge_router_changes: true,
    })
  })
})

describe('unified policy API contract', () => {
  it('creates and updates concrete policy records without sending an id in the body', async () => {
    const policy = {
      order: 200,
      name: 'Guest HTTPS',
      kind: 'firewall_rule' as const,
      origin: 'manual' as const,
      enabled: true,
      firewall: {
        action: 'accept' as const,
        source_zone: 'Guest',
        destination_zone: 'wan',
        protocols: ['tcp' as const],
        destination_port: '443',
      },
    }
    vi.mocked(fetch).mockImplementation(async () => ok({ id: 8, ...policy }))

    await api.savePolicy(policy)
    await api.savePolicy({ id: 8, ...policy, enabled: false })

    const [createPath, createInit] = vi.mocked(fetch).mock.calls[0]
    expect(createPath).toBe('/api/v1/site/policies')
    expect(createInit?.method).toBe('POST')
    expect(JSON.parse(String(createInit?.body))).toEqual(policy)
    expect(JSON.parse(String(createInit?.body)).firewall).not.toHaveProperty('family')

    const [updatePath, updateInit] = vi.mocked(fetch).mock.calls[1]
    expect(updatePath).toBe('/api/v1/site/policies/8')
    expect(JSON.parse(String(updateInit?.body))).toEqual({ ...policy, enabled: false })
    expect(JSON.parse(String(updateInit?.body))).not.toHaveProperty('id')
  })

  it('sends Object Manager scope and outcomes only to the draft compiler', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({
      drafts: [], gates: [], persisted: false, applied: false, note: 'draft only',
    }))

    await api.compilePolicyObjects(
      [{ kind: 'network', id: '7' }],
      [
        { kind: 'secure', destination_zone: 'wan' },
        { kind: 'qos', rate_kbps: 10000 },
      ],
    )

    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/site/object-manager/compile')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({
      objects: [{ kind: 'network', id: '7' }],
      outcomes: [
        { kind: 'secure', destination_zone: 'wan' },
        { kind: 'qos', rate_kbps: 10000 },
      ],
    })
  })

  it('encodes a client MAC and sends only explicit desired-state fields', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({
      client: { mac: 'aa:bb:cc:dd:ee:ff', blocked: true, group: 'Kids' },
      note: 'desired state saved',
    }))

    await api.saveClientPolicy('aa:bb:cc:dd:ee:ff', {
      blocked: true,
      fixed_ip: '',
      group: 'Kids',
    })

    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/clients/aa%3Abb%3Acc%3Add%3Aee%3Aff/policy')
    expect(JSON.parse(String(init?.body))).toEqual({
      blocked: true,
      fixed_ip: '',
      group: 'Kids',
    })
  })
})

describe('apply preview binding', () => {
  it('sends the opaque token and both risk acknowledgements', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({ devices: [], aborted: false }))

    await api.applySite({
      operation_id: '01962c09-7d62-7cd7-a1c2-450eba830892',
      preview_token: 'pv1_opaque',
      device_ids: [7],
      acknowledge_traversal: true,
      acknowledge_driver_risk: true,
      acknowledge_cautions: true,
    })

    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/site/apply')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({
      operation_id: '01962c09-7d62-7cd7-a1c2-450eba830892',
      preview_token: 'pv1_opaque',
      device_ids: [7],
      acknowledge_traversal: true,
      acknowledge_driver_risk: true,
      acknowledge_cautions: true,
    })
  })

  it('carries the server write verdict as structured error metadata', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(JSON.stringify({
      error: 'the preview is stale', write_state: 'none',
    }), {
      status: 409,
      headers: { 'Content-Type': 'application/json' },
    }))

    await expect(api.applySite({
      operation_id: '01962c09-7d62-7cd7-a1c2-450eba830892',
      preview_token: 'pv1_old',
    })).rejects.toMatchObject({
      status: 409,
      writeState: 'none',
      message: 'the preview is stale',
      body: { error: 'the preview is stale', write_state: 'none' },
    } satisfies Partial<ApiError>)
  })

  it('reads a retained apply operation without a CSRF mutation header', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({
      operation_id: '01962c09-7d62-7cd7-a1c2-450eba830892',
      state: 'running',
      created_at: 1,
      started_at: 2,
    }))

    await api.applyOperation('01962c09-7d62-7cd7-a1c2-450eba830892')

    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/site/apply/01962c09-7d62-7cd7-a1c2-450eba830892')
    expect(init?.method ?? 'GET').toBe('GET')
    expect(new Headers(init?.headers).has('X-Oonfee-CSRF')).toBe(false)
  })
})

describe('topology API contract', () => {
  it('uses Unix-millisecond snapshots and a bounded history range', async () => {
    vi.mocked(fetch).mockImplementation(async () => ok({
      at: 1787140800000, complete: true, nodes: [], edges: [], gaps: [],
    }))

    await api.topology(1787140800123.9)
    await api.topologyHistory(1787054400000.8, 1787140800000.9)

    expect(vi.mocked(fetch).mock.calls[0][0]).toBe('/api/v1/topology?at=1787140800123')
    expect(vi.mocked(fetch).mock.calls[1][0]).toBe(
      '/api/v1/topology/history?from=1787054400000&to=1787140800000',
    )
    for (const [, init] of vi.mocked(fetch).mock.calls) {
      expect(init?.method ?? 'GET').toBe('GET')
      expect(new Headers(init?.headers).has('X-Oonfee-CSRF')).toBe(false)
    }
  })
})

describe('radio scan API contract', () => {
  it('serializes the caller acknowledgement instead of inventing consent', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({
      scan: { id: 3, status: 'completed' }, observations: [],
    }))

    await api.scanRadio(7, 'radio/0', false)

    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/devices/7/radios/radio%2F0/scan')
    expect(JSON.parse(String(init?.body))).toEqual({ acknowledge_disruption: false })
  })
})

describe('client observability API contract', () => {
  it('loads the whole aligned investigation window in one authenticated GET', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({
      client_mac: 'aa:bb:cc:dd:ee:ff', from: 1787054400000, to: 1787140800000,
      resolution: '5m', bucket_ms: 300000, timestamps: [], ap_device_at: [],
      metrics: [{
        id: 'client:sta_rssi', scope: 'client', kind: 'sta_rssi', label: 'Signal', unit: 'dBm',
        values: [-60], mins: [-64], maxs: [-56], counts: [4],
        availability: { state: 'available', source: 'rollup_5m', observed_points: 1, expected_points: 1, gaps: [] },
      }], events: [], paths: [], gaps: [],
      experience_formula: {
        name: 'wifi-v1', weights: { rssi: .45, retry_delta: .35, tx_fail_delta: .2 },
        missing_policy: 'null when an input is missing',
      },
      data_contract: {
        metric_source: 'rollup_5m', raw_samples_persisted: false,
        event_time_resolution_ms: 1000, events_truncated: false,
        topology_source: 'persisted validity intervals',
      },
    }))

    const result = await api.clientObservability(
      'aa:bb:cc:dd:ee:ff', 1787054400000.9, 1787140800000.8,
    )

    expect(fetch).toHaveBeenCalledTimes(1)
    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe(
      '/api/v1/clients/aa%3Abb%3Acc%3Add%3Aee%3Aff/observability' +
      '?from=1787054400000&to=1787140800000',
    )
    expect(init?.method ?? 'GET').toBe('GET')
    expect(new Headers(init?.headers).has('X-Oonfee-CSRF')).toBe(false)
    expect(result.metrics[0]).toMatchObject({
      values: [-60], mins: [-64], maxs: [-56], counts: [4],
    })
  })
})
