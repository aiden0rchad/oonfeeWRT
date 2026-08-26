import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api, onControllerRestart, onUnauthorized } from './api'

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
  it('requests the three retained speed-test attempts by default', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({ jobs: [], active: null }))

    await api.speedTests()

    expect(vi.mocked(fetch).mock.calls[0][0]).toBe('/api/v1/speedtests?limit=3')
  })

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

  it('rejects stale data and announces a changed controller process', async () => {
    vi.mocked(fetch)
      .mockResolvedValueOnce(new Response('{"needs_setup":false}', {
        status: 200, headers: { 'X-OonfeeWRT-Instance': 'process-a' },
      }))
      .mockResolvedValueOnce(new Response('{"needs_setup":false}', {
        status: 200, headers: { 'X-OonfeeWRT-Instance': 'process-b' },
      }))
    const restarted = vi.fn()
    onControllerRestart.add(restarted)
    try {
      await expect(api.setupState()).resolves.toEqual({ needs_setup: false })
      await expect(api.setupState()).rejects.toMatchObject({
        status: 409, message: 'controller restarted',
      } satisfies Partial<ApiError>)
      expect(restarted).toHaveBeenCalledTimes(1)
    } finally {
      onControllerRestart.delete(restarted)
    }
  })
})

describe('account management API contract', () => {
  it('uses the controller account and session resources without putting secrets in URLs', async () => {
    vi.mocked(fetch)
      .mockResolvedValueOnce(ok({ account: { id: 4, username: 'owner', role: 'owner' } }))
      .mockResolvedValueOnce(ok({ sessions: [] }))
      .mockResolvedValueOnce(ok({ reauthenticated_until: 123 }))

    await api.account()
    await api.accountSessions()
    await api.reauthenticate('current-password-sentinel')

    expect(vi.mocked(fetch).mock.calls[0][0]).toBe('/api/v1/account')
    expect(vi.mocked(fetch).mock.calls[1][0]).toBe('/api/v1/account/sessions')
    const [path, init] = vi.mocked(fetch).mock.calls[2]
    expect(path).toBe('/api/v1/session/reauth')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({ password: 'current-password-sentinel' })
    expect(String(path)).not.toContain('sentinel')
  })

  it('sends owner mutations to the named PATCH, password, and session endpoints', async () => {
    vi.mocked(fetch).mockImplementation(async () => ok({ ok: true, account: {} }))

    await api.setAccountRole(7, 'operator')
    await api.setAccountEnabled(7, false)
    await api.resetAccountPassword(7, 'replacement-password')
    await api.revokeManagedAccountSession(7, 'session/with space')
    await api.revokeManagedAccountSessions(7)

    expect(vi.mocked(fetch).mock.calls.map(([path, init]) => [path, init?.method, init?.body && JSON.parse(String(init.body))])).toEqual([
      ['/api/v1/accounts/7/role', 'PATCH', { role: 'operator' }],
      ['/api/v1/accounts/7/enabled', 'PATCH', { enabled: false }],
      ['/api/v1/accounts/7/password', 'POST', { new_password: 'replacement-password' }],
      ['/api/v1/accounts/7/sessions/session%2Fwith%20space', 'DELETE', undefined],
      ['/api/v1/accounts/7/sessions', 'DELETE', undefined],
    ])
  })

  it('preserves a 428 reauthentication challenge for an explicit UI retry', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(JSON.stringify({
      error: 'reauthentication required', code: 'reauth_required',
    }), { status: 428, headers: { 'Content-Type': 'application/json' } }))

    await expect(api.deleteAccount(9)).rejects.toMatchObject({
      status: 428,
      message: 'reauthentication required',
      body: { error: 'reauthentication required', code: 'reauth_required' },
    } satisfies Partial<ApiError>)
  })

  it('does not sign out a live session for a rejected password confirmation', async () => {
    const unauthorized = vi.fn()
    onUnauthorized.add(unauthorized)
    try {
      vi.mocked(fetch)
        .mockResolvedValueOnce(new Response(JSON.stringify({
          error: 'password is incorrect', code: 'incorrect_password',
        }), { status: 401 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({
          error: 'session expired', code: 'not_signed_in',
        }), { status: 401 }))

      await expect(api.reauthenticate('wrong password')).rejects.toMatchObject({ status: 401 })
      expect(unauthorized).not.toHaveBeenCalled()
      await expect(api.reauthenticate('right but expired')).rejects.toMatchObject({ status: 401 })
      expect(unauthorized).toHaveBeenCalledOnce()
    } finally {
      onUnauthorized.delete(unauthorized)
    }
  })
})

describe('diagnostics API contract', () => {
  it('uses the stored-bundle resources and sends no invented request body', async () => {
    vi.mocked(fetch).mockImplementation(async () => ok({ jobs: [], job: {} }))

    await api.diagnostics()
    await api.startDiagnostics()
    await api.diagnostic('job/with space')
    await api.cancelDiagnostics('job/with space')

    expect(vi.mocked(fetch).mock.calls.map(([path, init]) => [path, init?.method, init?.body])).toEqual([
      ['/api/v1/diagnostics', undefined, undefined],
      ['/api/v1/diagnostics', 'POST', undefined],
      ['/api/v1/diagnostics/job%2Fwith%20space', undefined, undefined],
      ['/api/v1/diagnostics/job%2Fwith%20space/cancel', 'POST', undefined],
    ])
  })

  it('accepts only a completed ZIP response and preserves its server filename', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(new Blob(['zip']), {
      status: 200,
      headers: {
        'Content-Type': 'application/zip',
        'Content-Disposition': 'attachment; filename="oonfeewrt-diagnostics-123.zip"',
        'Content-Length': '3',
      },
    }))

    const result = await api.downloadDiagnostics('job/with space', 1024, 3)

    expect(vi.mocked(fetch).mock.calls[0][0]).toBe('/api/v1/diagnostics/job%2Fwith%20space/download')
    expect(result.filename).toBe('oonfeewrt-diagnostics-123.zip')
    expect(result.blob.type).toBe('application/zip')
  })

  it('rejects a successful non-ZIP response instead of downloading partial output', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response('{}', {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }))

    await expect(api.downloadDiagnostics('job', 1024, 2)).rejects.toMatchObject({
      status: 200,
      message: 'server returned a non-ZIP diagnostic download',
    } satisfies Partial<ApiError>)
  })

  it.each([
    ['missing length', undefined, '3'],
    ['larger than descriptor limit', '2048', '3'],
    ['different from the completed job', '4', '3'],
  ])('rejects a ZIP with %s', async (_name, contentLength, expected) => {
    const headers: Record<string, string> = { 'Content-Type': 'application/zip' }
    if (contentLength) headers['Content-Length'] = contentLength
    vi.mocked(fetch).mockResolvedValue(new Response(new Blob(['zip']), { status: 200, headers }))

    await expect(api.downloadDiagnostics('job', 1024, Number(expected))).rejects.toMatchObject({
      status: 200,
      message: 'server returned an invalid diagnostic download size',
    } satisfies Partial<ApiError>)
  })

  it('rejects a ZIP whose body size differs from its trusted headers', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(new Blob(['truncated']), {
      status: 200,
      headers: { 'Content-Type': 'application/zip', 'Content-Length': '3' },
    }))

    await expect(api.downloadDiagnostics('job', 1024, 3)).rejects.toMatchObject({
      status: 200,
      message: 'server returned an invalid diagnostic download size',
    } satisfies Partial<ApiError>)
  })
})

describe('backup export API contract', () => {
  it('binds sensitive-content consent and both passphrase entries to the reviewed plan', async () => {
    vi.mocked(fetch).mockImplementation(async () => ok({ jobs: [], job: {} }))

    await api.backups()
    await api.startBackup('controller-backup-export-v1', true, 'export passphrase', 'export passphrase')
    await api.backup('job/with space')
    await api.cancelBackup('job/with space')

    const calls = vi.mocked(fetch).mock.calls
    expect([calls[0][0], calls[0][1]?.method, calls[0][1]?.body]).toEqual([
      '/api/v1/backups', undefined, undefined,
    ])
    expect(calls[1][0]).toBe('/api/v1/backups')
    expect(calls[1][1]?.method).toBe('POST')
    expect(JSON.parse(String(calls[1][1]?.body))).toEqual({
      plan_id: 'controller-backup-export-v1',
      acknowledge_sensitive_content: true,
      export_passphrase: 'export passphrase',
      confirm_export_passphrase: 'export passphrase',
    })
    expect([calls[2][0], calls[2][1]?.method]).toEqual([
      '/api/v1/backups/job%2Fwith%20space', undefined,
    ])
    expect([calls[3][0], calls[3][1]?.method, calls[3][1]?.body]).toEqual([
      '/api/v1/backups/job%2Fwith%20space/cancel', 'POST', undefined,
    ])
    expect(api.backupDownloadURL('job/with space')).toBe('/api/v1/backups/job%2Fwith%20space/download')
  })
})

describe('controller restore API contract', () => {
  it('uploads the native artifact body and binds preview, cancellation, and confirmation to encoded IDs', async () => {
    document.cookie = 'oonfee_csrf=restore-csrf-sentinel'
    vi.mocked(fetch).mockImplementation(async () => ok({
      upload: {}, preview: {}, intent: { id: 'a'.repeat(32), state: 'accepted', accepted_at: 1 },
    }))
    const artifact = new File(['encrypted-backup'], 'controller.oowrtbak', {
      type: 'application/vnd.oonfeewrt.backup',
    })

    await api.restores()
    await api.uploadRestore(artifact)
    await api.startRestorePreview('upload/with space', 'export-passphrase-sentinel')
    await api.restorePreview('preview/with space')
    await api.cancelRestorePreview('preview/with space')
    await api.confirmRestore('preview/with space', {
      plan_id: 'controller-restore-confirm-v1.plan-sentinel',
      export_passphrase: 'export-passphrase-sentinel',
      destination_runtime_passphrase: 'runtime-passphrase-sentinel',
      typed_confirmation: 'RESTORE CONTROLLER',
      acknowledge_restart: true,
      acknowledge_session_revocation: true,
      acknowledge_router_writes_suppressed: true,
      acknowledge_no_automatic_router_apply: true,
    })

    const calls = vi.mocked(fetch).mock.calls
    expect([calls[0][0], calls[0][1]?.method]).toEqual(['/api/v1/restores', undefined])
    expect([calls[1][0], calls[1][1]?.method, calls[1][1]?.body]).toEqual([
      '/api/v1/restores/uploads', 'POST', artifact,
    ])
    expect(new Headers(calls[1][1]?.headers).get('Content-Type')).toBe('application/vnd.oonfeewrt.backup')
    expect(new Headers(calls[1][1]?.headers).get('X-Oonfee-CSRF')).toBe('restore-csrf-sentinel')
    expect(calls[1][1]?.credentials).toBe('same-origin')
    expect(JSON.parse(String(calls[2][1]?.body))).toEqual({
      upload_id: 'upload/with space', export_passphrase: 'export-passphrase-sentinel',
    })
    expect([calls[3][0], calls[3][1]?.method]).toEqual([
      '/api/v1/restores/previews/preview%2Fwith%20space', undefined,
    ])
    expect([calls[4][0], calls[4][1]?.method, calls[4][1]?.body]).toEqual([
      '/api/v1/restores/previews/preview%2Fwith%20space/cancel', 'POST', undefined,
    ])
    expect(calls[5][0]).toBe('/api/v1/restores/previews/preview%2Fwith%20space/confirm')
    expect(JSON.parse(String(calls[5][1]?.body))).toEqual({
      plan_id: 'controller-restore-confirm-v1.plan-sentinel',
      export_passphrase: 'export-passphrase-sentinel',
      destination_runtime_passphrase: 'runtime-passphrase-sentinel',
      typed_confirmation: 'RESTORE CONTROLLER',
      acknowledge_restart: true,
      acknowledge_session_revocation: true,
      acknowledge_router_writes_suppressed: true,
      acknowledge_no_automatic_router_apply: true,
    })
    expect(String(calls[2][0]) + String(calls[5][0])).not.toContain('passphrase-sentinel')
  })

  it('reads suppression and requires the active restore id plus exact resume phrase', async () => {
    vi.mocked(fetch).mockImplementation(async () => ok({ suppression: { active: false } }))

    await api.restoreSuppression()
    await api.resumeRouterWrites('restore/id', 'RESUME ROUTER WRITES')

    const calls = vi.mocked(fetch).mock.calls
    expect([calls[0][0], calls[0][1]?.method]).toEqual(['/api/v1/restores/suppression', undefined])
    expect([calls[1][0], calls[1][1]?.method, JSON.parse(String(calls[1][1]?.body))]).toEqual([
      '/api/v1/restores/suppression/resume', 'POST', {
        restore_id: 'restore/id', typed_confirmation: 'RESUME ROUTER WRITES',
      },
    ])
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

describe('speed test API contract', () => {
  it('binds the data-use acknowledgement to the exact current plan', async () => {
    vi.mocked(fetch).mockResolvedValue(ok({ id: 'job', state: 'queued' }))
    const planID = `sha256:${'a'.repeat(64)}`

    await api.startSpeedTest(planID, true)

    const [path, init] = vi.mocked(fetch).mock.calls[0]
    expect(path).toBe('/api/v1/speedtests')
    expect(JSON.parse(String(init?.body))).toEqual({
      acknowledge_data_use: true,
      plan_id: planID,
    })
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
