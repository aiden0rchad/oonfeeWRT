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
    /** null when the driver omitted the focused assoclist field. */
    signal: number | null
    rx_kbit: number | null
    tx_kbit: number | null
    connected_seconds: number
  }[]
}

type Handler = (msg: LiveStats | Record<string, unknown>) => void

export class Live {
  private ws: WebSocket | null = null
  private handlers = new Set<Handler>()
  private devices = new Map<number, number>()
  private retry = 0
  private timer: number | null = null
  private closed = false

  /** Fires with true when connected, false when not. */
  onState: ((up: boolean) => void) | null = null

  connect() {
    if (this.ws) return
	if (this.timer !== null) {
	  window.clearTimeout(this.timer)
	  this.timer = null
	}
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
	  if (this.ws !== ws) return
      this.retry = 0
      this.onState?.(true)
      // Re-subscribe on reconnect. The server treats a repeat subscribe as a
      // no-op, so this cannot stack focus a client could never release.
      for (const id of this.devices.keys()) this.sendRaw({ type: 'subscribe', topic: 'device.stats', device_id: id })
    }
    ws.onmessage = (e) => {
	  if (this.ws !== ws) return
      try {
        const msg = JSON.parse(e.data)
        this.handlers.forEach((h) => h(msg))
      } catch {
        /* a frame we cannot parse is not worth tearing the connection down for */
      }
    }
    ws.onclose = () => {
	  if (this.ws !== ws) return
      this.ws = null
      this.onState?.(false)
      this.scheduleReconnect()
    }
	ws.onerror = () => {
	  if (this.ws === ws) ws.close()
	}
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
    const previous = this.devices.get(deviceID) ?? 0
    this.devices.set(deviceID, previous + 1)
    if (previous === 0) {
      this.sendRaw({ type: 'subscribe', topic: 'device.stats', device_id: deviceID })
    }
    let released = false
    return () => {
      if (released) return
      released = true
      const remaining = (this.devices.get(deviceID) ?? 1) - 1
      if (remaining > 0) {
        this.devices.set(deviceID, remaining)
      } else {
        this.devices.delete(deviceID)
        this.sendRaw({ type: 'unsubscribe', topic: 'device.stats', device_id: deviceID })
      }
    }
  }

  on(h: Handler): () => void {
    this.handlers.add(h)
    return () => this.handlers.delete(h)
  }

  close() {
    this.closed = true
	if (this.timer !== null) {
	  window.clearTimeout(this.timer)
	  this.timer = null
	}
	const ws = this.ws
    this.ws = null
	ws?.close()
	this.onState?.(false)
  }
}

export const live = new Live()
