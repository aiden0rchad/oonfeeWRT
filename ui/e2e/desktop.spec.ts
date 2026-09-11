import { expect, test, type Locator, type Page } from '@playwright/test'

const pageErrors = new WeakMap<Page, string[]>()
test.beforeEach(async ({ page }) => {
  const errors: string[] = []
  pageErrors.set(page, errors)
  page.on('pageerror', (error) => errors.push(error.message))
})
test.afterEach(async ({ page }) => {
  expect(pageErrors.get(page)).toEqual([])
})

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
    {
      id: '33333333333333333333333333333333', plan_id: `sha256:${'a'.repeat(64)}`,
      state: 'completed', phase: 'complete', progress_percent: 100,
      provider: 'Cloudflare', method: 'single stream', provenance: 'controller-host', endpoint: 'speed.cloudflare.com',
      estimated_bytes: 15_000_000, created_at: Date.now() - 259_230_000, finished_at: Date.now() - 259_200_000,
      download_mbps: 117.8, upload_mbps: 105.5, idle_latency_ms: 15.4, idle_jitter_ms: 2.1,
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
  limits: { max_history: 3 },
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
  policy_sets: [{ id: 4, name: 'Trusted devices', members: ['02:00:00:00:00:01'] }],
  policy_capabilities: [
    { kind: 'firewall', available: true },
    { kind: 'route', available: true },
    { kind: 'fixed_ip', available: true },
    { kind: 'policy_set', available: true },
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

interface ControllerFixtureOptions {
  statistics?: boolean
}

const statisticsDevices = {
  devices: [{
    id: 1,
    mac: '02:00:00:00:01:01',
    name: 'Gateway',
    host: '192.168.1.1',
    role: 'gateway',
    functions: ['gateway'],
    management_mode: 'managed',
    adopted: true,
    adopted_at: 1_787_000_000,
    class: 'router',
    firmware: 'OpenWrt 24.10.2',
    last_seen: 1_788_000_000,
    poll_state: 'healthy',
    status: 'online',
  }],
}

const statisticsCatalog = {
  series: {
    sys_load1: [''],
    sys_mem_pct: [''],
    iface_rx_bps: ['wan'],
    iface_tx_bps: ['wan'],
    radio_utilization_pct: ['radio0'],
  },
}

function statisticsSeries(url: URL) {
  const kind = url.pathname.split('/').at(-1) ?? ''
  const key = url.searchParams.get('key') ?? ''
  const deviceID = Number(url.searchParams.get('device_id'))
  const from = Number(url.searchParams.get('from'))
  const to = Number(url.searchParams.get('to'))
  const hourly = to - from > 7 * 24 * 60 * 60
  const resolution = hourly ? '1h' : '5m'
  const bucketSeconds = hourly ? 60 * 60 : 5 * 60
  const first = Math.ceil(from / bucketSeconds) * bucketSeconds
  const points = []
  let index = 0

  for (let ts = first; ts + bucketSeconds <= to; ts += bucketSeconds, index++) {
    // One deliberate hole proves the page distinguishes missing evidence from zero.
    if (index === 2) continue
    const avg = kind === 'iface_rx_bps' ? 1_250_000 + (index % 12) * 22_000
      : kind === 'iface_tx_bps' ? 340_000 + (index % 9) * 11_000
        : kind === 'site_wan_latency_ms' ? 18 + (index % 7) * 0.4
          : kind === 'site_wan_loss_pct' ? (index % 97 === 0 ? 1.5 : 0)
            : kind === 'site_wan_up' ? (index === 5 ? 0 : 1)
              : kind === 'sys_load1' ? 0.24 + (index % 8) * 0.03
                : kind === 'sys_mem_pct' ? 61 + (index % 5) * 0.25
                  : kind === 'radio_utilization_pct' ? 32 + (index % 11)
                    : 0
    const spread = kind === 'site_wan_up' ? 0 : Math.max(Math.abs(avg) * 0.04, 0.05)
    points.push({ ts, avg, min: Math.max(0, avg - spread), max: avg + spread, cnt: hourly ? 12 : 1 })
  }

  return { device_id: deviceID, kind, key, resolution, points }
}

async function installControllerFixture(
  page: Page,
  topologyResponse: unknown = topology,
  options: ControllerFixtureOptions = {},
) {
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
      '/api/v1/devices': options.statistics ? statisticsDevices : { devices: [] },
      ...(options.statistics ? { '/api/v1/devices/1/series': statisticsCatalog } : {}),
      '/api/v1/clients': clientPage,
      '/api/v1/events': {
        events: [{
          ID: 1,
          TS: 1_788_000_000,
          DeviceID: 1,
          Category: 'system',
          Severity: 'warning',
          Event: 'openwrt.ipv6_ra_no_default_route',
          Detail: {
            message: 'odhcpd[81]: No default route present, setting ra_lifetime to 0!',
            priority: 28,
            occurrences: 37,
          },
          Source: 'openwrt-logd',
          SourceID: '81',
          SourceBoot: 'fixture',
          IngestedAt: 1_788_000_000_000,
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
        }],
        total: 1,
        limit: 100,
        scope: 'general',
        next_before: null,
        facets: { category: [], severity: [] },
        conditions: [{
          event_id: 1,
          device_id: 1,
          state: 'recent',
          occurrences: 37,
          last_observed_at: 1_788_000_000_000,
        }],
        coverage: { complete: true, expected_devices: 0, observed_devices: 0, gaps: [] },
      },
      '/api/v1/speedtests': speedTests,
      '/api/v1/topology': topologyResponse,
      '/api/v1/site': site,
      '/api/v1/accounts': accounts,
      '/api/v1/radios': radios,
      '/api/v1/discovery': { networks: [], skipped: [], hosts: 0 },
      '/api/v1/roaming/neighbours': { ran: false },
      '/api/v1/site/mesh-health': { links: [], note: 'No configured mesh links.' },
    }
    const body = path.startsWith('/api/v1/stats/')
      ? options.statistics
        ? statisticsSeries(url)
        : { device_id: 7, kind: path.split('/').at(-1), key: 'radio0', resolution: '5m', points: [] }
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
      await expect(page.getByLabel(/Throughput history for 3 completed tests/)).toBeVisible()
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
        hasText: 'Current scoped evidence',
      })
      const lines = await noticeSummary.evaluate((element) => {
        const style = getComputedStyle(element)
        return element.getBoundingClientRect().height / Number.parseFloat(style.lineHeight)
      })
      expect(lines).toBeLessThanOrEqual(2.1)

      const disclosure = page
        .getByRole('group', { name: 'Information: Dashboard metrics' })
        .locator('button[aria-haspopup="dialog"]')
      await expect(disclosure).toHaveAccessibleName('How these counts are calculated')
      const details = page.getByText(/“Wireless clients” is the same count/)
      await expect(disclosure).toHaveAttribute('aria-expanded', 'false')
      await expect(details).toBeHidden()
      await disclosure.focus()
      await disclosure.press('Enter')
      await expect(disclosure).toHaveAttribute('aria-expanded', 'true')
      const metricDialog = page.getByRole('dialog', { name: 'Information: Dashboard metrics' })
      await expect(metricDialog).toBeVisible()
      await expect(metricDialog.getByRole('button', {
        name: 'Close Information: Dashboard metrics',
      })).toBeFocused()
      await expectWithinMain(page, metricDialog)
      await expect(metricDialog.getByText(/“Wireless clients” is the same count/)).toBeVisible()
      const expandedOverflow = await readOverflow(page)
      expect(expandedOverflow.document).toBeLessThanOrEqual(1)
      expect(expandedOverflow.main).toBeLessThanOrEqual(1)
      await page.keyboard.press('Escape')
      await expect(metricDialog).toBeHidden()
      await expect(disclosure).toBeFocused()
      expect(unexpectedRequests).toEqual([])
    })
  }
}

for (const viewport of [
  { width: 1280, height: 720 },
  { width: 390, height: 844 },
  { width: 320, height: 568 },
]) {
  test(`${viewport.width}px routine help and speed-test impact stay compact`, async ({ page }) => {
    await page.setViewportSize(viewport)
    const unexpectedRequests = await installControllerFixture(page)

    await page.goto('/')
    const speed = page.getByRole('group', { name: 'Controller speed test' })
    const metrics = page.getByRole('group', { name: 'Information: Dashboard metrics' })
    await expectWithinMain(page, speed)
    await expect(metrics).toHaveAttribute('data-compact', 'true')
    await expectWithinMain(page, metrics)
    const run = speed.getByRole('button', { name: 'Run speed test' })
    const impact = speed.getByRole('button', { name: 'Impact & consent' })
    await expect(run).toBeVisible()
    await expect(run).toHaveAttribute('aria-describedby', /.+/)
    await expect(speed).toContainText(/15 MB \+ overhead.*30 seconds.*no router calls or changes.*public IP/i)
    await expect(speed.getByRole('checkbox')).toHaveCount(0)
    await impact.click()
    const impactDialog = page.getByRole('dialog', { name: 'Speed test impact & consent' })
    await expect(impactDialog).toBeVisible()
    await expectWithinMain(page, impactDialog)
    await page.mouse.move(2, viewport.height - 2)
    await expect(impactDialog).toBeHidden()
    await impact.focus()
    await impact.press('Enter')
    await expect(impactDialog).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(impactDialog).toBeHidden()
    await expect(impact).toBeFocused()
    if (viewport.width >= 1000) {
      expect((await speed.boundingBox())!.height).toBeLessThanOrEqual(90)
      expect((await metrics.boundingBox())!.height).toBeLessThanOrEqual(64)
    }

    const metricDisclosure = metrics.locator('button[aria-haspopup="dialog"]')
    await expect(metricDisclosure).toHaveAccessibleName('How these counts are calculated')
    await expect(metricDisclosure).toHaveAttribute('aria-expanded', 'false')
    expect((await metricDisclosure.boundingBox())!.height).toBeGreaterThanOrEqual(24)
    await metricDisclosure.focus()
    await metricDisclosure.press('Enter')
    await expect(metricDisclosure).toHaveAttribute('aria-expanded', 'true')
    const metricDialog = page.getByRole('dialog', { name: 'Information: Dashboard metrics' })
    await expect(metricDialog).toBeVisible()
    await expectWithinMain(page, metricDialog)
    await page.keyboard.press('Escape')
    await expect(metricDisclosure).toHaveAttribute('aria-expanded', 'false')
    await expect(metricDisclosure).toBeFocused()

    await page.goto('/logs')
    const sources = page.getByRole('group', { name: 'Information: General event sources' })
    const ipv6 = page.getByRole('group', { name: 'Warning: IPv6 router advertisements' })
    for (const notice of [sources, ipv6]) {
      await expect(notice).toHaveAttribute('data-compact', 'true')
      await expectWithinMain(page, notice)
    }
    await expect(ipv6.getByText('Warning', { exact: true })).toBeVisible()
    await expect(ipv6.getByText(
      /LAN IPv6 advertisements have no usable upstream route.*IPv4 is unaffected/,
    )).toBeVisible()
    const sourceDisclosure = sources.getByRole('button', {
      name: 'More information about event sources',
    })
    await sourceDisclosure.focus()
    await sourceDisclosure.press('Enter')
    const sourceDialog = page.getByRole('dialog', { name: 'Information: General event sources' })
    await expect(sourceDialog).toBeVisible()
    await expectWithinMain(page, sourceDialog)
    await expect(sourceDialog).toContainText(/Packet-flow\/NFLOG/)
    await page.keyboard.press('Escape')
    await expect(sourceDialog).toBeHidden()
    await expect(sourceDisclosure).toBeFocused()
    await expect(ipv6.locator('details')).toHaveCount(0)
    await expect(ipv6.locator('.details-popover')).toHaveCount(1)
    const ipv6Disclosure = ipv6.getByRole('button', {
      name: 'More information about this IPv6 warning',
    })
    await expect(ipv6Disclosure).toHaveAttribute('aria-expanded', 'false')
    await ipv6Disclosure.focus()
    await ipv6Disclosure.press('Enter')
    const ipv6Dialog = page.getByRole('dialog', { name: 'Warning: IPv6 router advertisements' })
    await expect(ipv6Dialog).toBeVisible()
    await expectWithinMain(page, ipv6Dialog)
    await expect(ipv6Dialog).toContainText(/37 reported occurrences.*1 current condition record/)
    await expect(ipv6Dialog.getByText('Keep IPv6')).toBeVisible()
    await expect(ipv6Dialog.getByText('Do not use IPv6 on this LAN')).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(ipv6Dialog).toBeHidden()
    await expect(ipv6Disclosure).toBeFocused()
    if (viewport.width >= 1000) {
      expect((await sources.boundingBox())!.height).toBeLessThanOrEqual(64)
      expect((await ipv6.boundingBox())!.height).toBeLessThanOrEqual(72)
      const borders = await sources.evaluate((element) => {
        const style = getComputedStyle(element)
        return { perimeter: style.borderTopColor, rail: style.borderLeftColor }
      })
      expect(borders.perimeter).not.toBe(borders.rail)
    }

    await page.goto('/settings')
    const uplinks = page.getByRole('group', { name: 'Information: Wireless uplinks' })
    const neighbours = page.getByRole('group', { name: 'Information: 802.11k neighbour reports' })
    for (const notice of [uplinks, neighbours]) {
      await expect(notice).toHaveAttribute('data-compact', 'true')
      await expectWithinMain(page, notice)
    }
    const uplinkDisclosure = uplinks.getByRole('button', {
      name: 'More information about wireless uplinks',
    })
    await uplinkDisclosure.focus()
    await uplinkDisclosure.press('Enter')
    const uplinkDialog = page.getByRole('dialog', { name: 'Information: Wireless uplinks' })
    await expect(uplinkDialog).toBeVisible()
    await expectWithinMain(page, uplinkDialog)
    await page.keyboard.press('Escape')
    await expect(uplinkDisclosure).toBeFocused()

    const overflow = await readOverflow(page)
    expect(overflow.document).toBeLessThanOrEqual(1)
    expect(overflow.main).toBeLessThanOrEqual(1)
    expect(unexpectedRequests).toEqual([])
  })
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

test('Controller tools sit at the sidebar foot and remain reachable when navigation scrolls', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 })
  const unexpectedRequests = await installControllerFixture(page)
  await page.goto('/')
  await page.getByRole('button', { name: 'Expand navigation' }).click()

  const navigation = page.getByRole('navigation', { name: 'Main navigation' })
  const divider = navigation.getByRole('separator', { name: 'Controller tools' })
  const adopt = navigation.getByRole('button', { name: 'Adopt a device' })
  const logs = navigation.getByRole('button', { name: 'Logs' })
  await expect(divider).toBeVisible()
  await expect(navigation.getByRole('button', { name: 'Settings' })).toBeVisible()
  await expect(adopt).toBeVisible()
  await expect(logs).toBeVisible()
  expect(await adopt.evaluate((element) =>
    element.nextElementSibling?.getAttribute('aria-label'))).toBe('Controller tools')
  expect(await divider.evaluate((element) =>
    element.nextElementSibling?.getAttribute('aria-label'))).toBe('Settings')
  expect(await divider.evaluate((element) =>
    element.nextElementSibling?.nextElementSibling?.getAttribute('aria-label'))).toBe('Logs')

  const [navigationBox, logsBox] = await Promise.all([
    navigation.boundingBox(),
    logs.boundingBox(),
  ])
  expect(navigationBox).not.toBeNull()
  expect(logsBox).not.toBeNull()
  expect(navigationBox!.y + navigationBox!.height - logsBox!.y - logsBox!.height)
    .toBeLessThanOrEqual(12)

  await page.setViewportSize({ width: 1280, height: 420 })
  expect(await navigation.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true)
  await logs.scrollIntoViewIfNeeded()
  await expect(logs).toBeVisible()
  await logs.click()
  await expect(page.getByRole('heading', { level: 1, name: 'Logs' })).toBeVisible()
  expect(unexpectedRequests).toEqual([])
})

test('Statistics renders gap-aware stored history in dark and light themes and switches 30 days to hourly', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 })
  const unexpectedRequests = await installControllerFixture(page, topology, { statistics: true })
  const statisticRequests: URL[] = []
  page.on('request', (request) => {
    const url = new URL(request.url())
    if (url.pathname.startsWith('/api/v1/stats/')) statisticRequests.push(url)
  })

  await page.goto('/statistics')

  await expect(page.getByRole('heading', { level: 1, name: 'Statistics' })).toBeVisible()
  await expect(page.getByRole('group', { name: 'Statistics time range' })).toBeVisible()
  await expect(page.getByLabel('Device detail')).toHaveValue('1')
  await expect(page.getByLabel('Network interface')).toHaveValue('wan')
  await expect(page.getByLabel('Stable radio')).toHaveValue('radio0')
  await expect(page.locator('.statistics-chart-card')).toHaveCount(9)
  await expect(page.getByText('History gaps', { exact: true }).first()).toBeVisible()
  await expect(page.getByRole('button', { name: '6 hours', exact: true })).toHaveAttribute('aria-pressed', 'true')
  const coverageDetail = page.getByLabel('Download traffic history coverage')
  await coverageDetail.click()
  await expect(coverageDetail.locator('..')).toContainText('intervals are unobserved')
  await expectWithinMain(page, coverageDetail.locator('..'))
  await coverageDetail.click()
  await expect(page.getByRole('img', {
    name: /ICMP reachability: \d+ all-reply buckets, 0 mixed-reply buckets, 1 no-reply bucket, and 1 missing bucket/,
  })).toBeVisible()
  await expect(page.getByText(/^All replied · \d+$/)).toBeVisible()
  await expect(page.getByText('No replies · 1', { exact: true })).toBeVisible()
  await expect(page.getByText('Missing · 1', { exact: true })).toBeVisible()
  expect(statisticRequests.some((url) =>
    url.pathname.endsWith('/iface_rx_bps') &&
    url.searchParams.get('device_id') === '1' &&
    url.searchParams.get('key') === 'wan')).toBe(true)

  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark')
  let overflow = await readOverflow(page)
  expect(overflow.document).toBeLessThanOrEqual(1)
  expect(overflow.main).toBeLessThanOrEqual(1)
  await page.getByRole('button', { name: /switch to light theme/i }).click()
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')
  overflow = await readOverflow(page)
  expect(overflow.document).toBeLessThanOrEqual(1)
  expect(overflow.main).toBeLessThanOrEqual(1)

  const thirtyDayRequest = page.waitForRequest((request) => {
    const url = new URL(request.url())
    return url.pathname.startsWith('/api/v1/stats/') &&
      Number(url.searchParams.get('to')) - Number(url.searchParams.get('from')) === 30 * 24 * 60 * 60
  })
  await page.getByRole('button', { name: '30 days' }).click()
  const request = new URL((await thirtyDayRequest).url())
  expect(Number(request.searchParams.get('to')) - Number(request.searchParams.get('from')))
    .toBe(30 * 24 * 60 * 60)
  await expect(page.getByRole('button', { name: '30 days' })).toHaveAttribute('aria-pressed', 'true')
  await expect(page.getByText(/hourly rollup/).first()).toBeVisible()
  await expect(page.getByText('History gaps', { exact: true }).first()).toBeVisible()
  expect(unexpectedRequests).toEqual([])
})

test('sparse Statistics keeps recent trends readable in both themes', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  const unexpectedRequests = await installControllerFixture(page, topology, { statistics: true })
  await page.route('**/api/v1/stats/**', async (route) => {
    const series = statisticsSeries(new URL(route.request().url()))
    await route.fulfill({ json: { ...series, points: series.points.slice(-17) } })
  })
  await page.goto('/statistics')
  await expect(page.getByRole('button', { name: '6 hours', exact: true })).toHaveAttribute('aria-pressed', 'true')
  await expect(page.getByRole('img', { name: /^Download traffic: 17 observed rollup buckets/ })).toBeVisible()
  const grid = page.locator('.statistics-chart-grid').first()
  for (const theme of ['dark', 'light']) {
    if (theme === 'light') await page.getByRole('button', { name: /switch to light theme/ }).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    await expectWithinMain(page, grid)
    await grid.screenshot({ path: testInfo.outputPath(`recent-history-${theme}.png`) })
  }
  expect(unexpectedRequests).toEqual([])
})

test('320px Statistics controls and charts stay within the content viewport', async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 568 })
  const unexpectedRequests = await installControllerFixture(page, topology, { statistics: true })

  await page.goto('/statistics')
  await expect(page.getByRole('heading', { level: 1, name: 'Statistics' })).toBeVisible()
  await expect(page.getByText('History gaps', { exact: true }).first()).toBeVisible()
  const coverageDetail = page.getByLabel('Download traffic history coverage')
  await coverageDetail.click()
  await expectWithinMain(page, coverageDetail.locator('..'))
  await page.getByText('About history and missing samples', { exact: true }).click()
  await expectWithinMain(page, page.locator('.statistics-history-help'))
  await expectWithinMain(page, page.getByRole('group', { name: 'Statistics time range' }))
  await expectWithinMain(page, page.getByLabel('Device detail'))
  await expectWithinMain(page, page.locator('.statistics-chart-card').first())
  expect(await page.locator('.statistics-chart-grid').first().evaluate((element) =>
    getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(1)

  const overflow = await readOverflow(page)
  expect(overflow.document).toBeLessThanOrEqual(1)
  expect(overflow.main).toBeLessThanOrEqual(1)
  expect(unexpectedRequests).toEqual([])
})

for (const viewport of [{ width: 1280, height: 720 }, { width: 1440, height: 900 }]) {
  for (const theme of ['dark', 'light'] as const) {
    test(`${viewport.width}x${viewport.height} ${theme} route page headers keep controls in view`, async ({ page }) => {
      await page.setViewportSize(viewport)
      const unexpectedRequests = await installControllerFixture(page)
      await page.goto('/devices')
      await page.getByRole('button', { name: 'Expand navigation' }).click()
      if (theme === 'light') await page.getByRole('button', { name: /switch to light theme/i }).click()
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme)

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

      for (const route of [
        { name: 'Dashboard', control: 'Live controller view' },
        { name: 'Statistics', control: '24h' },
        { name: 'Topology', control: 'Current' },
        { name: 'Radios', heading: 'Radios & Channel Plan', control: 'Refresh' },
        { name: 'Policy Engine', control: 'Zone Matrix' },
        { name: 'Settings', control: 'Network' },
        { name: 'Adopt a device', control: 'Inspect capabilities' },
      ]) {
        await page.getByRole('button', { name: route.name, exact: true }).click()
        const heading = page.getByRole('heading', {
          level: 1,
          name: route.heading ?? route.name,
          exact: true,
        })
        await expect(heading).toHaveCount(1)
        await expect(heading.locator('..').locator('..')).toHaveClass(/page-header/)
        await expect(page.locator('#main-content').getByText(route.control, { exact: true }).first()).toBeVisible()
        overflow = await readOverflow(page)
        expect(overflow.document).toBeLessThanOrEqual(1)
        expect(overflow.main).toBeLessThanOrEqual(1)
      }
      expect(unexpectedRequests).toEqual([])
    })
  }
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
  const namedSet = page.getByRole('region', { name: 'Named client set Trusted devices' })
  await expect(namedSet).toBeVisible()
  await expectWithinMain(page, namedSet)
  expect(await page.locator('.policy-object-picker').evaluate((element) =>
    getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)).toBe(1)
  await page.getByLabel('Object type').selectOption('policy_set')
  const policyObject = page.getByRole('combobox', { name: 'Object', exact: true })
  await expect(policyObject).toHaveValue('4')
  await expectWithinMain(page, policyObject)
  await page.getByRole('button', { name: 'Create named set' }).click()
  const setDialog = page.getByRole('dialog', { name: 'Create named client set' })
  await expect(setDialog.getByRole('checkbox', { name: /Fixture phone.*online/i })).toBeVisible()
  await expect(setDialog.getByLabel('Set name')).toBeVisible()
  const setDialogBox = await setDialog.boundingBox()
  expect(setDialogBox?.x ?? -1).toBeGreaterThanOrEqual(0)
  expect((setDialogBox?.x ?? 0) + (setDialogBox?.width ?? 391)).toBeLessThanOrEqual(390)
  await setDialog.getByRole('button', { name: 'Cancel' }).click()
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
  const speedChart = page.getByLabel(/Throughput history for 3 completed tests/)
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
  const lastTooltip = page.getByRole('tooltip').filter({ hasText: 'Upload 105.5 Mbps' })
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
  const classification = page.getByRole('group', { name: 'Information: Channel classification' })
  const classificationDetails = classification.locator('details')
  const classificationToggle = classificationDetails.locator('summary')
  await expectWithinMain(page, classification)
  await expectWithinMain(page, classification.locator('.notice-context'))
  await expectWithinMain(page, classificationToggle)
  await classificationToggle.click()
  await expect(classificationDetails).toHaveAttribute('open', '')
  await expectWithinMain(page, classificationDetails)
  const expandedOverflow = await readOverflow(page)
  expect(expandedOverflow.document).toBeLessThanOrEqual(1)
  expect(expandedOverflow.main).toBeLessThanOrEqual(1)
  await classificationToggle.click()
  await expect(classificationDetails).not.toHaveAttribute('open')
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
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
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

for (const viewport of [
  { width: 1280, height: 800 },
  { width: 390, height: 844 },
  { width: 320, height: 568 },
]) {
  for (const theme of ['dark', 'light'] as const) {
    test(`${viewport.width}px ${theme} adoption keeps connection fields and consent within the page`, async ({ page }) => {
      await page.setViewportSize(viewport)
      const unexpectedRequests = await installControllerFixture(page)
      await page.goto('/adopt')
      await expect(page.getByRole('heading', { level: 1, name: 'Adopt a device' })).toBeVisible()
      if (theme === 'light') await page.getByRole('button', { name: /switch to light theme/i }).click()
      for (const label of ['Address', 'Name (optional)', 'Management port (optional)', 'Device username', 'Device password (for ubus)']) {
        await expectWithinMain(page, page.getByLabel(label, { exact: true }))
      }
      const consent = page.getByRole('checkbox', { name: /^Install the oonfeeWRT controller access payload/ })
      const access = page.getByRole('group', { name: 'Controller access' })
      const details = access.locator('details')
      const summary = details.locator('summary')
      const adopt = page.getByRole('button', { name: 'Adopt', exact: true })
      await expect(access.getByRole('checkbox')).toHaveCount(1)
      await expect(consent).toHaveAccessibleDescription('Adds a dedicated login and permissions file—no packages or firmware. Network changes still require Preview and Apply.')
      await expect(consent).not.toBeChecked()
      await expect(adopt).toBeDisabled()
      await expect(details).not.toHaveAttribute('open')
      await expect(summary).toHaveText('View access details')
      await expectWithinMain(page, access)
      await summary.focus()
      await page.keyboard.press('Enter')
      await expect(details).toHaveAttribute('open', '')
      await expect(details.getByText('/usr/share/rpcd/acl.d/oonfeewrt.json')).toBeVisible()
      await expect(details.getByText(/Rollback asks for the device administrator login again/)).toBeVisible()
      await expectWithinMain(page, details)
      await expect(consent).not.toBeChecked()
      await expect(adopt).toBeDisabled()
      await summary.focus()
      await page.keyboard.press('Enter')
      await expect(details).not.toHaveAttribute('open')
      await expect(consent).not.toBeChecked()
      await expect(adopt).toBeDisabled()
      const overflow = await readOverflow(page)
      expect(overflow.document).toBeLessThanOrEqual(1)
      expect(overflow.main).toBeLessThanOrEqual(1)
      expect(unexpectedRequests).toEqual([])
    })
  }
}

for (const width of [320, 1280, 1440, 1920]) {
  test(`${width}px client investigation keeps its list and analysis usable`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 })
    const unexpectedRequests = await installControllerFixture(page)
    await page.route('**/api/v1/clients/*/observability?*', async (route) => {
      const to = Date.now()
      await route.fulfill({ json: {
        client_mac: clientPage.clients[0].mac, from: to - 600_000, to,
        resolution: '5m', bucket_ms: 300_000, timestamps: [to - 600_000, to - 300_000],
        ap_device_at: [null, null], events: [], paths: [], gaps: [],
        metrics: [{
          id: 'client-signal', scope: 'client', kind: 'signal_dbm', label: 'Signal',
          unit: 'dBm', values: [-55, -53],
          availability: { state: 'available', source: 'rollup_5m', observed_points: 2, expected_points: 2, gaps: [] },
        }],
        experience_formula: {
          name: 'wifi-v1', weights: { rssi: 0.45, retry_delta: 0.35, tx_fail_delta: 0.2 },
          missing_policy: 'Missing inputs remain unavailable',
        },
        data_contract: {
          metric_source: 'rollup_5m', raw_samples_persisted: false,
          event_time_resolution_ms: 1000, events_truncated: false,
          topology_source: 'persisted validity intervals',
        },
      } })
    })
    await page.goto('/clients')
    if (width >= 1280) await page.getByRole('button', { name: 'Expand navigation' }).click()
    await page.getByRole('button', { name: 'Open observability for Fixture phone' }).click()
    const list = page.getByRole('region', { name: 'Client list' })
    const analysis = page.getByRole('region', { name: 'Client analysis' })
    await expect(analysis.getByRole('region', { name: 'Signal metric' })).toBeVisible()
    await expectWithinMain(page, list)
    await expectWithinMain(page, analysis)
    await expectWithinMain(page, analysis.getByRole('region', { name: 'Signal metric' }))
    const columns = await page.locator('.client-observability-workspace').evaluate((element) =>
      getComputedStyle(element).gridTemplateColumns.split(/\s+/).length)
    expect(columns).toBe(width === 320 ? 1 : width < 1800 ? 2 : 4)
    if (width >= 1280) {
      expect((await list.boundingBox())?.width ?? 0).toBeGreaterThanOrEqual(width < 1800 ? 700 : 460)
    }
    const overflow = await readOverflow(page)
    expect(overflow.document).toBeLessThanOrEqual(1)
    expect(overflow.main).toBeLessThanOrEqual(1)
    expect(unexpectedRequests).toEqual([])
  })
}
