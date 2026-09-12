export type AlertCondition = 'device_offline' | 'wan_latency' | 'wan_loss'
export interface AlertRuleInput {
  name: string; condition: AlertCondition; device_id: number; threshold: number
  hold_seconds: number; cooldown_seconds: number; enabled: boolean
}
export interface AlertRule extends AlertRuleInput {
  id: number; state: 'disabled' | 'unknown' | 'pending' | 'firing' | 'clear'
  since: number | null; value: number | null; observed_at: number | null; reason: string
}
export interface AlertIncident {
  id: number; rule_id: number; rule_name: string; device_id: number; device_name: string
  condition: AlertCondition; state: 'firing' | 'resolved'; started_at: number; resolved_at: number | null
  value: number | null; delivery_state: 'not_configured' | 'pending' | 'sent' | 'failed' | 'cooldown' | 'cancelled'; delivery_error: string
}
export interface AlertDelivery {
  configured: boolean; host: string; enabled: boolean
  last_attempt_at: number | null; last_success_at: number | null; last_error: string
}
export interface AlertResponse {
  rules: AlertRule[]; incidents: AlertIncident[]; delivery: AlertDelivery; evaluated_at: number | null
  counts?: { open_incidents: number }
}
