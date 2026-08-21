import { afterEach, describe, expect, it, vi } from 'vitest'
import { Live } from './live'

class FakeWebSocket {
  static OPEN = 1
  static instances: FakeWebSocket[] = []

  readyState = 0
  sent: string[] = []
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null

  constructor(_url: string) { FakeWebSocket.instances.push(this) }
  send(value: string) { this.sent.push(value) }
  close() {}
}

afterEach(() => {
  FakeWebSocket.instances = []
	vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('Live device focus references', () => {
  it('subscribes on 0→1 and unsubscribes only on 1→0', () => {
    vi.stubGlobal('WebSocket', FakeWebSocket)
    const live = new Live()
    const first = live.watch(7)
    const second = live.watch(7)
    const socket = FakeWebSocket.instances[0]
    socket.readyState = FakeWebSocket.OPEN
    socket.onopen?.()

    expect(socket.sent.map((value) => JSON.parse(value))).toEqual([
      { type: 'subscribe', topic: 'device.stats', device_id: 7 },
    ])
    first()
    expect(socket.sent).toHaveLength(1)
    second()
    expect(socket.sent.map((value) => JSON.parse(value))).toEqual([
      { type: 'subscribe', topic: 'device.stats', device_id: 7 },
      { type: 'unsubscribe', topic: 'device.stats', device_id: 7 },
    ])
  })

	it('clears a cancelled retry so a later socket can reconnect', () => {
	  vi.useFakeTimers()
	  vi.stubGlobal('WebSocket', FakeWebSocket)
	  const live = new Live()
	  live.connect()
	  const first = FakeWebSocket.instances[0]
	  first.onclose?.()
	  live.close()
	  live.watch(7)
	  const second = FakeWebSocket.instances[1]
	  second.onclose?.()
	  vi.advanceTimersByTime(500)
	  expect(FakeWebSocket.instances).toHaveLength(3)
	})

	it('ignores an old socket close after a replacement is installed', () => {
	  vi.useFakeTimers()
	  vi.stubGlobal('WebSocket', FakeWebSocket)
	  const live = new Live()
	  live.connect()
	  const first = FakeWebSocket.instances[0]
	  live.close()
	  live.connect()
	  const second = FakeWebSocket.instances[1]
	  second.readyState = FakeWebSocket.OPEN
	  second.onopen?.()
	  first.onclose?.()
	  live.watch(8)
	  expect(second.sent.map((value) => JSON.parse(value))).toContainEqual(
	    { type: 'subscribe', topic: 'device.stats', device_id: 8 },
	  )
	  vi.advanceTimersByTime(1_000)
	  expect(FakeWebSocket.instances).toHaveLength(2)
	})
})
