import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

/**
 * Test environment setup.
 *
 * # localStorage
 *
 * happy-dom exposes a `localStorage` object with none of the Storage methods on
 * it, so any component that persists a preference throws on first render. This
 * installs a real in-memory Storage instead of stubbing the call sites, because
 * the persistence behaviour is worth testing and a no-op stub would make every
 * such test vacuously pass.
 *
 * Finding this was the runner's first useful act: the same gap showed that
 * useColumnPrefs read localStorage outside a try/catch, which in a browser with
 * site data blocked would throw inside a useState initialiser and blank the
 * screen rather than lose a preference.
 *
 * # Hook order
 *
 * Vitest runs afterEach hooks in REVERSE registration order, so a hook that
 * throws prevents the ones registered before it from running at all. That is
 * how a broken localStorage teardown stopped `cleanup` from unmounting, and
 * every test after the first then saw the previous test's DOM still on screen —
 * failures that pointed everywhere except at the cause.
 */
class MemoryStorage implements Storage {
  private data = new Map<string, string>()

  get length() {
    return this.data.size
  }
  clear() {
    this.data.clear()
  }
  getItem(key: string) {
    return this.data.has(key) ? this.data.get(key)! : null
  }
  key(index: number) {
    return [...this.data.keys()][index] ?? null
  }
  removeItem(key: string) {
    this.data.delete(key)
  }
  setItem(key: string, value: string) {
    // Storage stringifies everything, and a test that stores a number and gets
    // a number back would not match the browser.
    this.data.set(String(key), String(value))
  }
  [name: string]: unknown
}

const storage = new MemoryStorage()
Object.defineProperty(globalThis, 'localStorage', {
  value: storage,
  configurable: true,
})

// Registered FIRST so it runs LAST — see the hook-order note above. Clearing
// state before unmounting would tear the store out from under a component
// mid-teardown.
afterEach(() => storage.clear())

// Unmount between tests. Without it a component that registers a window
// listener or an interval keeps running under the next test, and the failure
// shows up somewhere unrelated.
afterEach(cleanup)
