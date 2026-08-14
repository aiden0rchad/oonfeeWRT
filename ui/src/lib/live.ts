/**
 * The live channel.
 *
 * Replaces polling the REST API every ten seconds, which showed data up to ten
 * seconds old and defeated the point of a focused tier that polls every five.
 *
 * It also replaces the focus lease. Focus is reference-counted by the server's
 * hub, acquired on subscribe and released on unsubscribe or disconnect — so a
 * closed tab releases it exactly, with no renewal timer and no grace period to
 * get wrong. The subscription IS the focus.
 */

export interface LiveStats {
  type: 'stats'
  device_id: number
  ts: number
  tier: string
  uptime: number
  load1: number
  mem_pct?: number
  poll_ms: number
  /** null when an AP's count could not be read — never zero for "unknown". */
  clients: number | null
  degraded: number
  aps: {
    iface: string
    ssid: string
    channel: number
    freq: number
    clients: number | null
    airtime_pct?: number
  }[]
  stations: {
    mac: string
    iface: string
    signal: number
    rx_kbit: number
    tx_kbit: number
    connected_seconds: number
  }[]
}

type Handler = (msg: LiveStats | Record<string, unknown>) => void

export class Live {
  private ws: WebSocket | null = null
  private handlers = new Set<Handler>()
  private devices = new Set<number>()
  private events = false
  private retry = 0
  private timer: number | null = null
  private closed = false

  /** Fires with true when connected, false when not. */
  onState: ((up: boolean) => void) | null = null

  connect() {
    if (this.ws) return
    // Connecting is an explicit intent to be open, so it clears a previous
    // close. Without this, the App's "close when signed out" effect — which
    // runs once on mount, before the session has loaded — latched the channel
    // shut permanently and every later connect() silently returned.
    this.closed = false
    this.retry = 0
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(`${proto}//${location.host}/api/v1/live`)
    this.ws = ws

    ws.onopen = () => {
      this.retry = 0
      this.onState?.(true)
      // Re-subscribe on reconnect. The server treats a repeat subscribe as a
      // no-op, so this cannot stack focus a client could never release.
      for (const id of this.devices) this.sendRaw({ type: 'subscribe', topic: 'device.stats', device_id: id })
      if (this.events) this.sendRaw({ type: 'subscribe', topic: 'events' })
    }
    ws.onmessage = (e) => {
      try {
        const msg = JSON.parse(e.data)
        this.handlers.forEach((h) => h(msg))
      } catch {
        /* a frame we cannot parse is not worth tearing the connection down for */
      }
    }
    ws.onclose = () => {
      this.ws = null
      this.onState?.(false)
      this.scheduleReconnect()
    }
    ws.onerror = () => ws.close()
  }

  private scheduleReconnect() {
    if (this.closed || this.timer !== null) return
    // Exponential with a ceiling: a controller that is restarting should not be
    // hammered, and a browser left open overnight should still reconnect.
    const delay = Math.min(30_000, 500 * 2 ** this.retry++)
    this.timer = window.setTimeout(() => {
      this.timer = null
      this.connect()
    }, delay)
  }

  private sendRaw(v: unknown) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(v))
  }

  /** Watch a device. Returns the unsubscribe, which also releases its focus. */
  watch(deviceID: number): () => void {
    // Connect if nobody has yet. A subscriber should not depend on some other
    // component having opened the channel first: that coupling is invisible,
    // and when it broke the symptom was a panel that silently never updated
    // while the server was pushing correctly.
    this.connect()
    this.devices.add(deviceID)
    this.sendRaw({ type: 'subscribe', topic: 'device.stats', device_id: deviceID })
    let released = false
    return () => {
      if (released) return
      released = true
      this.devices.delete(deviceID)
      this.sendRaw({ type: 'unsubscribe', topic: 'device.stats', device_id: deviceID })
    }
  }

  watchEvents(): () => void {
    this.connect()
    this.events = true
    this.sendRaw({ type: 'subscribe', topic: 'events' })
    return () => {
      this.events = false
      this.sendRaw({ type: 'unsubscribe', topic: 'events' })
    }
  }

  on(h: Handler): () => void {
    this.handlers.add(h)
    return () => this.handlers.delete(h)
  }

  close() {
    this.closed = true
    if (this.timer !== null) window.clearTimeout(this.timer)
    this.ws?.close()
    this.ws = null
  }
}

export const live = new Live()
