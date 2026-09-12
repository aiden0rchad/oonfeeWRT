export interface Position {
  x: number
  y: number
}

export type NodePlacement = Position & { unplaced: boolean }
export type SavedPositions = Record<string, NodePlacement>

const maxCoordinate = 20_000

/** Origin is the controller boundary; a site UUID additionally distinguishes
 * controllers restored or replaced at the same address. Never persist without
 * an authenticated account key. Current and historical arrangements are separate. */
export function topologyLayoutKey(userKey?: string, controllerKey?: string, mode = 'current') {
  return userKey
    ? `oonfeewrt:topology-layout:v1:${JSON.stringify([window.location.origin, controllerKey ?? '', userKey, mode])}`
    : null
}

export function parseTopologyPositions(raw: string | null): SavedPositions {
  if (!raw || raw.length > 1_000_000) return {}
  try {
    const value: unknown = JSON.parse(raw)
    if (!value || typeof value !== 'object' || Array.isArray(value)) return {}
    const entries = Object.entries(value)
    if (entries.length > 2_000) return {}
    return Object.fromEntries(entries.filter(([id, position]) => {
      if (!id || id.length > 512 || !position || typeof position !== 'object' || Array.isArray(position)) return false
      const { x, y, unplaced } = position as NodePlacement
      return typeof x === 'number' && Number.isFinite(x) && x >= 0 && x <= maxCoordinate
        && typeof y === 'number' && Number.isFinite(y) && y >= 0 && y <= maxCoordinate
        && typeof unplaced === 'boolean'
    }).map(([id, position]) => [id, {
      x: position.x, y: position.y, unplaced: position.unplaced,
    }]))
  } catch {
    return {}
  }
}

export function readTopologyPositions(key: string | null): SavedPositions {
  if (!key) return {}
  try {
    return parseTopologyPositions(localStorage.getItem(key))
  } catch {
    return {}
  }
}

export interface LayoutBounds {
  width: number
  height: number
  unplacedStartX: number | null
}

/** Unplaced nodes cannot be dragged into the observed-link area. Moving a
 * drawing never changes what the evidence does (or does not) establish. */
export function constrainTopologyPosition(position: Position, unplaced: boolean, bounds: LayoutBounds): Position {
  const left = unplaced && bounds.unplacedStartX != null ? bounds.unplacedStartX + 132 : 132
  const right = !unplaced && bounds.unplacedStartX != null ? bounds.unplacedStartX - 132 : bounds.width - 132
  return {
    x: Math.max(left, Math.min(right, position.x)),
    y: Math.max(unplaced ? 100 : 66, Math.min(bounds.height - 66, position.y)),
  }
}
