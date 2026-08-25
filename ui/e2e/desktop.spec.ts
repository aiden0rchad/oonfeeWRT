import { expect, test, type Locator, type Page } from '@playwright/test'

const dashboardObservedAt = Date.now() - 240_000
const dashboardTimes = Array.from(
  { length: 72 },
  (_, index) => dashboardObservedAt - (71 - index) * 300_000,
)

function wanMetric(
  kind: string,
  unit: string,
  value: number | null,
  samples: Array<number | null>,
  status: 'fresh' | 'last_observed' | 'unavailable' = 'fresh',
) {
  return {
    kind,
    unit,
    meaning: `${kind} from the selected default-route interface`,
    status,
    value,
    as_of: value == null ? null : dashboardObservedAt,
    points: samples.map((sample, index) => ({ ts: dashboardTimes[index], value: sample })),
  }
}

const dashboard = {
  devices: { total: 2, online: 2, offline: 0, pending: 0, unknown: 0 },
  wireless_clients: 1,
  wireless_clients_complete: true,
  known_devices: 3,
  active_devices: 3,
  upstream_devices: 0,
  unscoped_devices: 0,
  gateway_uplinks: [{ device_id: 1, name: 'Gateway', state: 'up' }],
  focused_devices: 0,
  quiesced_devices: 0,
  series_count: 24,
  recent_events: [],
  recent_alert_events: [{
    ID: 4,
    TS: 1_788_000_000,
    Severity: 'warning',
    Event: 'fixture.warning',
  }],
  wan: {
    target: '1.1.1.1',
    probe: 'icmp',
    freshness: 'fresh',
    as_of: dashboardObservedAt,
    gateway: { device_id: 1, name: 'Gateway', route_interface: 'wan', series_key: 'wan' },
    resolution: '5m',
    bucket_ms: 300_000,
    from: dashboardTimes[0],
    to: dashboardTimes.at(-1),
    metrics: {
      download_bps: wanMetric('site_wan_download_bps', 'B/s', 13_125,
        dashboardTimes.map((_, index) => index % 17 === 0
          ? null
          : index % 23 === 0 ? 0 : 12_000 + (index % 9) * 450)),
      upload_bps: wanMetric('site_wan_upload_bps', 'B/s', 66.75,
        dashboardTimes.map((_, index) => index % 19 === 7
          ? null
          : index % 29 === 0 ? 0 : 62 + (index % 8) * 1.25)),
      latency_ms: wanMetric('site_wan_latency_ms', 'ms', 22,
        dashboardTimes.map((_, index) => index % 31 === 4 ? null : 21 + (index % 8) * .2)),
      loss_pct: wanMetric('site_wan_loss_pct', 'percent', 0,
        dashboardTimes.map((_, index) => index % 31 === 5 ? null : index === 20 ? 1.2 : 0)),
      reachable: wanMetric('site_wan_reachable', 'boolean', 1, []),
    },
  },
}

const topology = {
  at: 1_788_000_000_000,
  complete: true,
  truncated: false,
  gaps: [],
  nodes: [
    { id: 'synthetic:internet', kind: 'synthetic', name: 'Internet', synthetic: true },
    { id: 'device:1', kind: 'device', name: 'Gateway', device_id: 1, synthetic: false },
    { id: 'device:2', kind: 'device', name: 'Access point', device_id: 2, synthetic: false },
    { id: 'client:1', kind: 'client', name: 'Client', synthetic: false },
  ],
  edges: [{
    id: 1,
    child_id: 'device:2',
    parent_id: 'device:1',
    parent_port: 'lan2',
    medium: 'wired',
    confidence: 'measured',
    valid_from: 1_788_000_000_000,
    last_seen: 1_788_000_000_000,
    evidence: [],
    ambiguities: [],
  }],
  last_known_edges: [],
}

const speedTests = {
  jobs: [
    {
      id: '11111111111111111111111111111111', plan_id: `sha256:${'a'.repeat(64)}`,
      state: 'completed', phase: 'complete', progress_percent: 100,
      provider: 'Cloudflare', method: 'single stream', provenance: 'controller-host', endpoint: 'speed.cloudflare.com',
      estimated_bytes: 15_000_000, created_at: Date.now() - 86_430_000, finished_at: Date.now() - 86_400_000,
      download_mbps: 125.3, upload_mbps: 107.4, idle_latency_ms: 18.2, idle_jitter_ms: 2.4,
      loaded_latency_ms: null, loaded_jitter_ms: null, bytes_downloaded: 12_000_000, bytes_uploaded: 3_000_000,
    },
    {
      id: '22222222222222222222222222222222', plan_id: `sha256:${'a'.repeat(64)}`,
      state: 'completed', phase: 'complete', progress_percent: 100,
      provider: 'Cloudflare', method: 'single stream', provenance: 'controller-host', endpoint: 'speed.cloudflare.com',
      estimated_bytes: 15_000_000, created_at: Date.now() - 172_830_000, finished_at: Date.now() - 172_800_000,
      download_mbps: 216.3, upload_mbps: 412.4, idle_latency_ms: 12.1, idle_jitter_ms: 1.8,
      loaded_latency_ms: null, loaded_jitter_ms: null, bytes_downloaded: 12_000_000, bytes_uploaded: 3_000_000,
    },
  ],
  active: null,
  test: {
    plan_id: `sha256:${'a'.repeat(64)}`,
    provider: 'Cloudflare',
    method: 'controller-host HTTPS transfer',
    provenance: 'controller-host',
    endpoint: 'speed.cloudflare.com',
    download_endpoint: 'https://speed.cloudflare.com/__down',
    upload_endpoint: 'https://speed.cloudflare.com/__up',
    estimated_bytes: 15_000_000,
    max_duration_seconds: 30,
  },
  limits: { max_history: 20 },
  disclosure: {
    vantage_point: 'controller-host',
    router_management_calls: false,
    router_changes: false,
    saturation_warning: 'The test may saturate the WAN while it runs.',
    privacy: 'The provider observes the controller public address and transfer metadata.',
  },
}

const clientPage = {
  clients: [{
    mac: '02:00:00:00:00:01',
    name: 'Fixture phone',
    ipv4: '192.168.1.20',
    first_seen: 1_787_999_000,
    last_seen: 1_788_000_000,
    blocked: false,
    connection: 'wireless',
    online: true,
    signal: -55,
    device_id: 1,
    scope: 'local',
  }],
  total: 1,
  limit: 500,
  offset: 0,
  facets: {
    presence: [{ value: 'online', count: 1 }],
    connection: [{ value: 'wireless', count: 1 }],
    scope: [{ value: 'local', count: 1 }],
  },
  note: 'Current fixture evidence is available',
  scope_note: '',
}

const site = {
  name: 'Fixture site',
  uuid: 'f8a258d7-3bf1-4099-a534-ce1f0a6cdd7c',
  wlans: [],
  meshes: [],
  uplinks: [],
  groups: [],
  networks: [{ id: 1, name: 'lan', vlan: 1, cidr: '192.168.1.1/24', zone: 'lan', enabled: true }],
  zones: [{ name: 'lan', forward_to: ['wan'], explicit: true }],
  policies: [],
  policy_capabilities: [
    { kind: 'firewall', available: true },
    { kind: 'route', available: true },
    { kind: 'fixed_ip', available: true },
  ],
  problems: [],
  overrides: [],
  overridable: [],
  override_note: '',
}

const radios = {
  generated_at: 1_788_000_000_000,
  gaps: [],
  devices: [{
    device_id: 7,
    name: 'Fixture AP',
    status: { last_poll_ok: true, consecutive_failures: 0, stale: false },
    radios: [{
      radio_key: 'radio0',
      up: true,
      band: '5g',
      configured_channel: 'auto',
      htmode: 'VHT80',
      current_mhz: 5180,
      current_channel: 36,
      inventory_observed_at: 1_788_000_000_000,
      channels_observed_at: 1_788_000_000_000,
      stale: false,
      interfaces: [{ name: 'phy0-ap0', mode: 'ap' }],
      channels_known: true,
      channels: [
        { band: '5g', channel: 36, mhz: 5180, state: 'in-use', availability: 'enabled', in_use: true, restricted: false, dfs: null, excluded: null, flags: [] },
        { band: '5g', channel: 44, mhz: 5220, state: 'enabled', availability: 'enabled', in_use: false, restricted: false, dfs: null, excluded: null, flags: [] },
        { band: '5g', channel: 52, mhz: 5260, state: 'restricted', availability: 'restricted', in_use: false, restricted: true, dfs: null, excluded: null, flags: ['NO-IR'] },
        { band: '5g', channel: 60, mhz: 5300, state: 'unknown', availability: 'unknown', in_use: false, restricted: false, dfs: null, excluded: null, flags: [] },
      ],
      scan_capability: 'absent',
      latest_observations: [],
    }],
  }],
}

const accounts = {
  accounts: [{
    id: 1,
    username: 'operator',
    role: 'owner',
    role_label: 'Owner',
    enabled: true,
    created_at: 1_787_000_000,
    last_login_at: 1_788_000_000,
    active_session_count: 1,
  }],
  roles: [
    { value: 'owner', label: 'Owner', description: 'Full controller and account administration.' },
    { value: 'admin', label: 'Administrator', description: 'Full network administration.' },
    { value: 'operator', label: 'Operator', description: 'Operate the network.' },
    { value: 'viewer', label: 'Read only', description: 'View controller state.' },
  ],
}

async function installControllerFixture(page: Page, topologyResponse: unknown = topology) {
  const unexpectedRequests: string[] = []
  await page.addInitScript(() => {
    class FixtureWebSocket {
      static readonly OPEN = 1
      readyState = 0
      onopen: ((event: Event) => void) | null = null
      onmessage: ((event: MessageEvent) => void) | null = null
      onclose: ((event: Event) => void) | null = null
      onerror: ((event: Event) => void) | null = null

      constructor() {
        queueMicrotask(() => {
          this.readyState = FixtureWebSocket.OPEN
          this.onopen?.(new Event('open'))
        })
      }

      send() {}

      close() {
        this.readyState = 3
        this.onclose?.(new Event('close'))
      }
    }
    Object.defineProperty(window, 'WebSocket', { value: FixtureWebSocket })
  })

  await page.route('**/*', async (route) => {
    const url = new URL(route.request().url())
    if (url.origin !== 'http://127.0.0.1:4173') {
      unexpectedRequests.push(`${route.request().method()} ${url.href}`)
      await route.abort('blockedbyclient')
      return
    }
    if (!url.pathname.startsWith('/api/')) {
      await route.continue()
      return
    }
    const path = url.pathname
    const responses: Record<string, unknown> = {
      '/api/v1/setup': { needs_setup: false },
      '/api/v1/session': {
        admin_id: 1,
        username: 'operator',
        role: 'owner',
        role_label: 'Owner',
        csrf: 'fixture',
        reauthenticated_until: null,
      },
      '/api/v1/dashboard': dashboard,
      '/api/v1/devices': { devices: [] },
      '/api/v1/clients': clientPage,
      '/api/v1/events': {
        events: [],
        total: 0,
        limit: 100,
        scope: 'general',
        next_before: null,
        facets: { category: [], severity: [] },
        coverage: { complete: true, expected_devices: 0, observed_devices: 0, gaps: [] },
      },
      '/api/v1/speedtests': speedTests,
      '/api/v1/topology': topologyResponse,
      '/api/v1/site': site,
      '/api/v1/accounts': accounts,
      '/api/v1/radios': radios,
      '/api/v1/roaming/neighbours': { ran: false },
      '/api/v1/site/mesh-health': { links: [], note: 'No configured mesh links.' },
    }
    const body = path.startsWith('/api/v1/stats/')
      ? { device_id: 7, kind: path.split('/').at(-1), key: 'radio0', resolution: '5m', points: [] }
      : responses[path]
    if (route.request().method() !== 'GET' || body === undefined) {
      unexpectedRequests.push(`${route.request().method()} ${path}`)
      await route.fulfill({ status: 500, json: { error: `unmocked fixture route: ${path}` } })
      return
    }
    await route.fulfill({
      status: 200,
      json: body,
      headers: { 'X-OonfeeWRT-Instance': 'desktop-layout-fixture' },
    })
  })
  return unexpectedRequests
}

async function readOverflow(page: Page) {
  return page.evaluate(() => {
    const main = document.querySelector<HTMLElement>('#main-content')
    return {
      document: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      main: main ? main.scrollWidth - main.clientWidth : Number.POSITIVE_INFINITY,
    }
  })
}

async function expectWithinMain(page: Page, locator: Locator) {
  await locator.scrollIntoViewIfNeeded()
  const [main, element] = await Promise.all([
    page.locator('#main-content').boundingBox(),
    locator.boundingBox(),
  ])
  expect(main).not.toBeNull()
  expect(element).not.toBeNull()
  expect(element!.x).toBeGreaterThanOrEqual(main!.x - 1)
  expect(element!.x + element!.width).toBeLessThanOrEqual(main!.x + main!.width + 1)
}

async function contrastRatio(locator: Locator) {
  return locator.evaluate((element) => {
    const rgb = (value: string) => (value.match(/[\d.]+/g) ?? []).slice(0, 3).map(Number)
    const luminance = (value: string) => {
      const [red, green, blue] = rgb(value).map((channel) => {
        const normalized = channel / 255
        return normalized <= .04045 ? normalized / 12.92 : ((normalized + .055) / 1.055) ** 2.4
      })
      return .2126 * red + .7152 * green + .0722 * blue
    }
    const style = getComputedStyle(element)
    const foreground = luminance(style.color)
    const background = luminance(style.backgroundColor)
    return (Math.max(foreground, background) + .05) / (Math.min(foreground, background) + .05)
  })
}

for (const viewport of [{ width: 1280, height: 720 }, { width: 1440, height: 900 }]) {
  for (const theme of ['dark', 'light'] as const) {
    test(`${viewport.width}x${viewport.height} ${theme} Dashboard fits and discloses by keyboard`, async ({ page }) => {
      await page.setViewportSize(viewport)
      const unexpectedRequests = await installControllerFixture(page)
      await page.goto('/')

      await expect(page.getByRole('heading', { level: 1, name: 'Dashboard' })).toBeVisible()
      await page.getByRole('button', { name: 'Expand navigation' }).click()
      if (theme === 'light') await page.getByRole('button', { name: /switch to light theme/i }).click()
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
      await expect(page.getByText('fixture.warning')).toBeVisible()
      await expect(page.getByText('Complete coverage')).toBeVisible()

      const health = page.getByRole('region', { name: 'Internet health details' })
      const healthCharts = health.locator('.dashboard-wan-metrics')
      await expect(healthCharts.getByRole('img')).toHaveCount(4)
      await expect(page.getByLabel(/Throughput history for 2 completed tests/)).toBeVisible()
      expect(await healthCharts.evaluate((element) =>
        getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(2)
      const operationCards = page.locator('.dashboard-operations-grid > section')
      await expect(operationCards).toHaveCount(2)
      const cardHeights = await operationCards.evaluateAll((cards) =>
        cards.map((card) => card.getBoundingClientRect().height))
      expect(Math.abs(cardHeights[0] - cardHeights[1])).toBeLessThanOrEqual(1)

      const healthView = health.getByRole('group', { name: 'Internet health history view' })
      await healthView.getByRole('button', { name: 'Table' }).click()
      const healthTable = health.getByRole('region', { name: 'Internet health history table' })
      await expect(healthTable.getByRole('row')).toHaveCount(73)
      await expectWithinMain(page, healthTable)
      await healthView.getByRole('button', { name: 'Chart' }).click()

      const overflow = await readOverflow(page)
      expect(overflow.document).toBeLessThanOrEqual(1)
      expect(overflow.main).toBeLessThanOrEqual(1)

      const noticeSummary = page.locator('.notice-summary', {
        hasText: 'Counts use current, scoped evidence',
      })
      const lines = await noticeSummary.evaluate((element) => {
        const style = getComputedStyle(element)
        return element.getBoundingClientRect().height / Number.parseFloat(style.lineHeight)
      })
      expect(lines).toBeLessThanOrEqual(2.1)

      const disclosure = page
        .getByRole('group', { name: 'Information: Dashboard metrics' })
        .locator('summary')
      const details = page.getByText(/“Wireless clients” is the same count/)
      await expect(disclosure).toHaveAttribute('aria-expanded', 'false')
      await expect(details).toBeHidden()
      await disclosure.focus()
      await disclosure.press('Enter')
      await expect(disclosure).toHaveAttribute('aria-expanded', 'true')
      await expect(disclosure).toBeFocused()
      await expect(details).toBeVisible()
      const expandedOverflow = await readOverflow(page)
      expect(expandedOverflow.document).toBeLessThanOrEqual(1)
      expect(expandedOverflow.main).toBeLessThanOrEqual(1)
      expect(unexpectedRequests).toEqual([])
    })
  }
}

test('Topology keeps review actions visible while technical detail is collapsed', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 })
  const partialTopology = {
    ...topology,
    complete: false,
    gaps: ['device:1/ip-4-neigh: source call failure: access/permission denied'],
  }
  const unexpectedRequests = await installControllerFixture(page, partialTopology)
  await page.goto('/topology')

  await expect(page.getByRole('heading', { level: 1, name: 'Topology' })).toBeVisible()
  await page.getByRole('button', { name: 'Expand navigation' }).click()
  const notice = page.getByRole('group', { name: 'Information: Bridge and neighbor sources' })
  const disclosure = notice.locator('summary')
  const detail = notice.getByText(/Optional controller access may restore bridge and neighbor evidence/)
  const action = notice.getByRole('button', { name: 'Review optional capability' })
  await expect(disclosure).toHaveAttribute('aria-expanded', 'false')
  await expect(detail).toBeHidden()
  await expect(action).toBeVisible()
  expect(await action.evaluate((element) => element.closest('.notice-disclosure') == null)).toBe(true)
  await action.focus()
  await expect(action).toBeFocused()
  await disclosure.focus()
  await disclosure.press('Enter')
  await expect(disclosure).toHaveAttribute('aria-expanded', 'true')
  await expect(detail).toBeVisible()
  const overflow = await readOverflow(page)
  expect(overflow.document).toBeLessThanOrEqual(1)
  expect(overflow.main).toBeLessThanOrEqual(1)
  expect(unexpectedRequests).toEqual([])
})

for (const viewport of [{ width: 1280, height: 720 }, { width: 1440, height: 900 }]) {
  test(`${viewport.width}x${viewport.height} list page headers keep controls in view`, async ({ page }) => {
    await page.setViewportSize(viewport)
    const unexpectedRequests = await installControllerFixture(page)
    await page.goto('/devices')
    await page.getByRole('button', { name: 'Expand navigation' }).click()

    await expect(page.getByRole('heading', { level: 1, name: 'Devices' })).toHaveCount(1)
    await expect(page.locator('#main-content').getByRole('button', { name: 'Adopt a device' })).toBeVisible()
    let overflow = await readOverflow(page)
    expect(overflow.document).toBeLessThanOrEqual(1)
    expect(overflow.main).toBeLessThanOrEqual(1)

    await page.getByRole('button', { name: 'Client Devices' }).click()
    await expect(page.getByRole('heading', { level: 1, name: 'Client Devices' })).toHaveCount(1)
    await expect(page.getByRole('region', { name: 'Client filters' })).toBeVisible()
    overflow = await readOverflow(page)
    expect(overflow.document).toBeLessThanOrEqual(1)
    expect(overflow.main).toBeLessThanOrEqual(1)

    await page.getByRole('button', { name: 'Logs' }).click()
    await expect(page.getByRole('heading', { level: 1, name: 'Logs' })).toHaveCount(1)
    const eventView = page.getByRole('group', { name: 'Event view' })
    await expect(eventView).toBeVisible()
    await expect(eventView.getByRole('button', { name: 'General' })).toHaveAttribute('aria-pressed', 'true')
    overflow = await readOverflow(page)
    expect(overflow.document).toBeLessThanOrEqual(1)
    expect(overflow.main).toBeLessThanOrEqual(1)
    expect(unexpectedRequests).toEqual([])
  })
}

test('390x844 responsive routes keep controls and state in view', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const unexpectedRequests = await installControllerFixture(page)

  await page.goto('/logs')
  await expect(page.getByRole('heading', { level: 1, name: 'Logs' })).toBeVisible()
  expect(await page.locator('.logs-page').evaluate((element) =>
    getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(1)
  await expectWithinMain(page, page.getByRole('group', { name: 'Event view' }))
  await expectWithinMain(page, page.locator('.logs-page .pager').getByRole('button', { name: 'Next' }))

  await page.goto('/settings')
  await expect(page.getByRole('heading', { level: 1, name: 'Settings' })).toBeVisible()
  await page.getByRole('tab', { name: 'Accounts' }).click()
  for (const field of ['Username', 'Password', 'Repeat password']) {
    await expectWithinMain(page, page.getByLabel(field, { exact: true }).first())
  }
  await expectWithinMain(page, page.getByRole('combobox', { name: /^Role/ }))

  await page.goto('/radios')
  await expect(page.getByRole('heading', { level: 1, name: 'Radios & Channel Plan' })).toBeVisible()
  expect(await page.locator('.radio-stat-grid').evaluate((element) =>
    getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(1)
  expect(await page.locator('.radio-plan-row').evaluate((element) =>
    getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(1)
  await expectWithinMain(page, page.getByLabel('Filter by access point'))
  await expectWithinMain(page, page.getByLabel('Filter by band'))
  await expectWithinMain(page, page.getByLabel('Channel Plan legend'))

  await page.goto('/topology')
  await expect(page.getByRole('heading', { level: 1, name: 'Topology' })).toBeVisible()
  expect(await page.locator('.topology-stat-grid').evaluate((element) =>
    getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(1)
  await expectWithinMain(page, page.getByText('Evidence coverage').locator('..'))
  await expectWithinMain(page, page.getByLabel('Topology confidence legend'))

  await page.goto('/policy')
  const policyTabs = page.getByRole('tablist', { name: 'Policy Engine views' })
  await expect(policyTabs).toBeVisible()
  await expectWithinMain(page, policyTabs)
  await page.getByRole('tab', { name: 'Objects' }).click()
  expect(await page.locator('.policy-object-picker').evaluate((element) =>
    getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(1)
  await page.getByRole('checkbox', { name: /^Route\b/ }).check()
  expect(await page.locator('.policy-route-fields').evaluate((element) =>
    getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(1)

  await page.goto('/clients')
  await expect(page.getByRole('heading', { level: 1, name: 'Client Devices' })).toBeVisible()
  await expectWithinMain(page, page.locator('.pager').getByRole('button', { name: 'Next' }))

  const overflow = await readOverflow(page)
  expect(overflow.document).toBeLessThanOrEqual(1)
  expect(overflow.main).toBeLessThanOrEqual(1)
  expect(unexpectedRequests).toEqual([])
})

test('320px narrow dashboard, Logs pager, and Channel Plan do not clip', async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 568 })
  const unexpectedRequests = await installControllerFixture(page)

  await page.goto('/')
  await expect(page.getByRole('heading', { level: 1, name: 'Dashboard' })).toBeVisible()
  const health = page.getByRole('region', { name: 'Internet health details' })
  await expect(health.getByRole('img')).toHaveCount(4)
  await expectWithinMain(page, health)
  const healthView = health.getByRole('group', { name: 'Internet health history view' })
  await healthView.getByRole('button', { name: 'Table' }).click()
  await expectWithinMain(page, health.getByRole('region', { name: 'Internet health history table' }))
  await healthView.getByRole('button', { name: 'Chart' }).click()
  const speedChart = page.getByLabel(/Throughput history for 2 completed tests/)
  await expect(speedChart).toBeVisible()
  await expectWithinMain(page, speedChart)
  const downloadBar = page.getByRole('meter', { name: 'Download throughput' }).first()
  await downloadBar.focus()
  await expect(page.getByRole('tooltip').filter({ hasText: 'Download 125.3 Mbps' })).toBeVisible()
  const [trackBox, scaleBox] = await Promise.all([
    downloadBar.boundingBox(),
    page.locator('.speedtest-chart-scale').boundingBox(),
  ])
  expect(Math.abs(trackBox!.x - scaleBox!.x)).toBeLessThanOrEqual(1)
  expect(Math.abs(trackBox!.x + trackBox!.width - scaleBox!.x - scaleBox!.width)).toBeLessThanOrEqual(1)
  const lastUpload = page.getByRole('meter', { name: 'Upload throughput' }).last()
  await lastUpload.focus()
  const lastTooltip = page.getByRole('tooltip').filter({ hasText: 'Upload 412.4 Mbps' })
  await expect(lastTooltip).toBeVisible()
  await expectWithinMain(page, lastTooltip)
  const speedView = page.getByRole('group', { name: 'Speed test history view' })
  await speedView.getByRole('button', { name: 'Table' }).click()
  await expectWithinMain(page, page.getByRole('region', { name: 'Speed test result table' }))
  await speedView.getByRole('button', { name: 'Chart' }).click()
  let overflow = await readOverflow(page)
  expect(overflow.document).toBeLessThanOrEqual(1)
  expect(overflow.main).toBeLessThanOrEqual(1)

  await page.goto('/logs')
  const logsPager = page.locator('.logs-page .pager')
  await expect(logsPager).toBeVisible()
  await expectWithinMain(page, logsPager.getByRole('button', { name: 'Previous' }))
  await expectWithinMain(page, logsPager.getByRole('button', { name: 'Next' }))

  await page.goto('/radios')
  const channel = page.locator('.radio-channel[data-state="in-use"]')
  await expect(channel).toBeVisible()
  await expectWithinMain(page, channel)
  expect(await page.locator('.radio-plan-row').evaluate((element) =>
    getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(1)
  overflow = await readOverflow(page)
  expect(overflow.document).toBeLessThanOrEqual(1)
  expect(overflow.main).toBeLessThanOrEqual(1)
  expect(unexpectedRequests).toEqual([])
})

for (const theme of ['dark', 'light'] as const) {
  test(`${theme} selected controls and channel numerals meet text contrast`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    const unexpectedRequests = await installControllerFixture(page)

    await page.goto('/logs')
    if (theme === 'light') await page.getByRole('button', { name: /switch to light theme/i }).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    expect(await contrastRatio(page.getByRole('button', { name: 'General' }))).toBeGreaterThanOrEqual(4.5)

    await page.goto('/policy')
    const objects = page.getByRole('tab', { name: 'Objects' })
    await objects.click()
    expect(await contrastRatio(objects)).toBeGreaterThanOrEqual(4.5)

    await page.goto('/radios')
    await expect(page.locator('.radio-channel')).toHaveCount(4)
    for (const state of ['in-use', 'enabled', 'restricted', 'unknown']) {
      expect(await contrastRatio(page.locator(`.radio-channel[data-state="${state}"]`))).toBeGreaterThanOrEqual(4.5)
    }

    await page.goto('/')
    const health = page.getByRole('region', { name: 'Internet health details' })
    await expect(health.getByRole('img')).toHaveCount(4)
    await expectWithinMain(page, health)
    const overflow = await readOverflow(page)
    expect(overflow.document).toBeLessThanOrEqual(1)
    expect(overflow.main).toBeLessThanOrEqual(1)
    expect(unexpectedRequests).toEqual([])
  })
}
