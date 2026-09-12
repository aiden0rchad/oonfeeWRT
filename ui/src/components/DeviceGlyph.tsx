/** Functional illustrations, not manufacturer or inferred client identities. */
export function DeviceGlyph({
  kind,
  size = 32,
}: {
  kind: 'gateway' | 'ap' | 'switch' | 'client' | 'wireless' | 'unknown'
  size?: number
}) {
  return (
    <svg width={size} height={size} viewBox="0 0 64 64" fill="none" stroke="currentColor" strokeWidth="2"
      strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      {kind === 'gateway' ? <>
        <path d="M12 38V16m40 22V16M9 38h46v14H9zM17 45h1m7 0h1m7 0h1M43 44h6" />
        <path d="M23 23a14 14 0 0 1 18 0m-13 6a6 6 0 0 1 8 0" opacity=".65" />
      </> : kind === 'ap' ? <>
        <rect x="16" y="10" width="32" height="44" rx="12" />
        <path d="M22 25a15 15 0 0 1 20 0m-16 6a9 9 0 0 1 12 0M30 37a3 3 0 0 1 4 0M29 47h6" />
      </> : kind === 'switch' ? <>
        <rect x="7" y="21" width="50" height="23" rx="5" />
        <path d="M14 29h6v7h-6zm12 0h6v7h-6zm12 0h6v7h-6zM50 29v7M16 44v5m32-5v5" />
      </> : kind === 'wireless' ? <>
        <circle cx="32" cy="45" r="3" />
        <path d="M24 36a12 12 0 0 1 16 0M16 27a24 24 0 0 1 32 0M8 18a36 36 0 0 1 48 0" />
      </> : <>
        <rect x="17" y="17" width="30" height="30" rx="10" />
        <path d="M25 10v7m14-7v7M25 47v7m14-7v7M10 25h7m-7 14h7m30-14h7m-7 14h7" />
        {kind === 'unknown' ? <path d="M28 27a4 4 0 1 1 6 4c-2 1-2 2-2 3M32 38h.01" /> : <circle cx="32" cy="32" r="5" />}
      </>}
    </svg>
  )
}
