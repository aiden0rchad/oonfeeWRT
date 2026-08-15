/**
 * Column ordering and preference parsing.
 *
 * Separate from components/ui.tsx and free of React on purpose: these are the
 * parts with rules rather than rendering, and in a file with no JSX they can be
 * compiled with the repo's own tsc and exercised directly. The UI has no test
 * runner, so "can this be run at all" is the difference between verified and
 * merely reviewed — see columns.check.ts.
 */

/** A grid's column preferences: what is hidden, and in what order. */
export interface ColumnPrefs {
  hidden: string[]
  /** Column keys in display order. Keys not listed keep their built-in order
   *  and follow the listed ones — see orderColumns. */
  order: string[]
}

/** The shape orderColumns needs. Structural, so components/ui.tsx's Column<T>
 *  satisfies it without this file importing React. */
export interface Keyed {
  key: string
}

/**
 * Apply a saved column order.
 *
 * The rule for a key that is not in the saved order is the interesting part: a
 * later build that adds a column must still show it. It goes after the ordered
 * ones, in its built-in position relative to other unsaved columns. Predictable
 * and visible beats clever — the alternative, guessing where it "should" sit
 * among columns the operator has already arranged, is a guess that looks like a
 * bug when it lands somewhere unexpected.
 *
 * Unknown keys in the saved order — a column a later build REMOVED — are
 * skipped rather than treated as an error. Storage outlives any one build.
 */
export function orderColumns<C extends Keyed>(columns: C[], order: string[]): C[] {
  if (order.length === 0) return columns
  const byKey = new Map(columns.map((c) => [c.key, c]))
  const out: C[] = []
  const placed = new Set<string>()
  for (const key of order) {
    const col = byKey.get(key)
    if (col && !placed.has(key)) {
      out.push(col)
      placed.add(key)
    }
  }
  for (const col of columns) {
    if (!placed.has(col.key)) out.push(col)
  }
  return out
}

/**
 * Move one column to another's position.
 *
 * Takes and returns the FULL key list, including hidden columns. Reordering
 * only the visible ones would silently drop the hidden ones' positions, so
 * unhiding a column later would put it somewhere the operator never chose.
 */
export function moveColumn(keys: string[], from: string, to: string): string[] {
  if (from === to) return keys
  const fromAt = keys.indexOf(from)
  const toAt = keys.indexOf(to)
  if (fromAt < 0 || toAt < 0) return keys
  const without = keys.filter((k) => k !== from)
  const at = without.indexOf(to)
  // Dropping onto a column to the RIGHT of the dragged one puts it after that
  // column; onto one to the left, before it. Without this the two directions
  // are asymmetric — dragging one place to the right appears to do nothing,
  // because removing the column first shifts the target into its own slot.
  const insert = fromAt < toAt ? at + 1 : at
  return [...without.slice(0, insert), from, ...without.slice(insert)]
}

/**
 * Parse stored preferences, tolerating both formats and anything else.
 *
 * Anything could be in storage — another version's format, a hand-edit. A grid
 * that throws on load is worse than one that forgets a preference.
 */
export function parsePrefs(raw: string | null): ColumnPrefs {
  const empty: ColumnPrefs = { hidden: [], order: [] }
  if (!raw) return empty
  try {
    const parsed: unknown = JSON.parse(raw)
    // The original format was a bare array of hidden keys. Migrate rather than
    // discard: someone who hid four columns should not silently get them all
    // back because a later build started storing an order alongside.
    if (Array.isArray(parsed)) return { hidden: strings(parsed), order: [] }
    if (parsed && typeof parsed === 'object') {
      const o = parsed as Record<string, unknown>
      return { hidden: strings(o.hidden), order: strings(o.order) }
    }
    return empty
  } catch {
    return empty
  }
}

function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
}
