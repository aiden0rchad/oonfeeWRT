import { expect, test, type Page } from '@playwright/test'

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
  jobs: [],
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
      '/api/v1/clients': {
        clients: [],
        total: 0,
        limit: 500,
        offset: 0,
        facets: { presence: [], connection: [], scope: [] },
        note: 'No current RF evidence is available',
        scope_note: '',
      },
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
    }
    const body = responses[path]
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
