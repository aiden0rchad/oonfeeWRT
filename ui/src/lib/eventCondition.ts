import type { EventRow } from './api'

export const IPV6_RA_NO_DEFAULT_ROUTE = 'openwrt.ipv6_ra_no_default_route'

export function ipv6RACondition(event: EventRow): { occurrences: number } | null {
  if (event.Event !== IPV6_RA_NO_DEFAULT_ROUTE) return null
  const detail = event.Detail
  if (!detail || typeof detail !== 'object' || Array.isArray(detail)) return { occurrences: 1 }
  const value = (detail as Record<string, unknown>).occurrences
  return {
    occurrences: Number.isSafeInteger(value) && Number(value) > 0 ? Number(value) : 1,
  }
}

export function eventLabel(event: EventRow): string {
  return ipv6RACondition(event)
    ? 'IPv6 router advertisements have no usable default route'
    : event.Event
}
