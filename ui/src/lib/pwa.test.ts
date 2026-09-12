import { readFileSync } from 'node:fs'
import { runInNewContext } from 'node:vm'
import { describe, expect, it, vi } from 'vitest'

const worker = readFileSync('public/sw.js', 'utf8')

function workerEvents(fetch: ReturnType<typeof vi.fn>) {
  const handlers: Record<string, (event: Record<string, unknown>) => void> = {}
  runInNewContext(worker, { URL, Response, fetch, self: {
    location: { origin: 'https://controller.example' },
    addEventListener: (name: string, handler: (event: Record<string, unknown>) => void) => { handlers[name] = handler },
  } })
  return handlers
}

describe('installed app safety', () => {
  it('does not intercept API calls, mutations, assets or other origins', () => {
    const fetch = vi.fn()
    const handlers = workerEvents(fetch)
    const respondWith = vi.fn()
    for (const request of [
      { url: 'https://controller.example/api/v1/devices', method: 'GET', mode: 'navigate' },
      { url: 'https://controller.example/api/v1/site/apply', method: 'POST', mode: 'navigate' },
      { url: 'https://controller.example/assets/main.js', method: 'GET', mode: 'cors' },
      { url: 'https://other.example/', method: 'GET', mode: 'navigate' },
    ]) handlers.fetch({ request, respondWith })
    expect(respondWith).not.toHaveBeenCalled()
    expect(fetch).not.toHaveBeenCalled()
  })
  it('returns a static no-store offline page, without cached network data', async () => {
    const handlers = workerEvents(vi.fn().mockRejectedValue(new Error('offline')))
    let response!: Promise<Response>
    handlers.fetch({ request: { url: 'https://controller.example/devices', method: 'GET', mode: 'navigate' },
      respondWith: (value: Promise<Response>) => { response = value } })
    const result = await response
    expect(result.status).toBe(503)
    expect(result.headers.get('Cache-Control')).toBe('no-store')
    expect(await result.text()).toContain('Nothing has been changed or queued while offline')
  })
  it('does not replace real authorization or server failures with offline success', async () => {
    const expected = new Response('Forbidden', { status: 403 })
    const handlers = workerEvents(vi.fn().mockResolvedValue(expected))
    let response!: Promise<Response>
    handlers.fetch({ request: { url: 'https://controller.example/', method: 'GET', mode: 'navigate' },
      respondWith: (value: Promise<Response>) => { response = value } })
    expect(await response).toBe(expected)
  })
})
