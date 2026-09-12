export interface AdGuardConfig {
  configured: boolean; url: string; username: string; has_password: boolean; tls_fingerprint: string
}
export interface AdGuardResult {
  state: 'observed' | 'partial' | 'unavailable'; checked_at: number; source_url: string
  version?: string; running: boolean | null; protection_enabled: boolean | null
  dns_queries: number | null; blocked_filtering: number | null; avg_processing_ms: number | null; notes: string[]
}
export interface WireGuardResult {
  device_id: number; state: 'observed' | 'partial' | 'unavailable'; checked_at: number
  interfaces: Array<{ name: string; peers: Array<{
    public_key: string; last_handshake: number | null; handshake_state: 'observed' | 'never' | 'unavailable'
    rx_bytes: number | null; tx_bytes: number | null
  }> }>; notes: string[]
}
