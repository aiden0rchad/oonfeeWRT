export interface FirmwareDevice {
  device_id: number; name: string; status: string; management_mode: string
  identity: { board_name: string; target: string; rootfs_type: string; release: string }
  identity_source: 'stored_capability_probe'
}
export interface FirmwareInventory {
  devices: FirmwareDevice[]
  checking: { source_url: string; scope: string; note: string }
  agent: { available: boolean; installed_state: string; package_path: string; note: string }
  installation: { enabled: boolean; required_checks: string[] }
}
export interface FirmwareResult {
  state: 'available' | 'current' | 'ahead' | 'unsupported' | 'error'
  current_version: string; latest_version?: string; branch?: string
  /** Unix milliseconds, unlike alert-manager timestamps. */
  checked_at: number; source_url: string; message: string
  image?: { name: string; url: string; sha256: string; size: number; filesystem: string }
  limitations: string[]
}
