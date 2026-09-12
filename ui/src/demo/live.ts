import type { LiveStats } from '../lib/live'
export type { LiveStats } from '../lib/live'

/** Matches the live-channel surface but never opens a socket or starts timers. */
export class Live {
  onState: ((up: boolean) => void) | null = null
  connect() { this.onState?.(false) }
  watch(_deviceID: number): () => void { return () => {} }
  on(_handler: (message: LiveStats | Record<string, unknown>) => void): () => void { return () => {} }
  close() { this.onState?.(false) }
}

export const live = new Live()
