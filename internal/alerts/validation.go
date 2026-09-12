package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ValidatePersistentState is also used at the imported-backup trust boundary.
// It intentionally permits removed device targets: those remain unknown, not
// silently rebound to the next router that reuses the old inventory ID.
func ValidatePersistentState(s PersistentState) error {
	invalid := errors.New("stored alert state is invalid")
	if len(s.Rules) > MaxRules || len(s.Incidents) > 500 || len(s.Queue) > 100 || s.NextRuleID < 0 || s.NextIncidentID < 0 || s.NextRuleID == math.MaxInt64 || s.NextIncidentID == math.MaxInt64 {
		return invalid
	}
	if len(s.DestinationCiphertext) > 16384 || s.Delivery.Configured != (len(s.DestinationCiphertext) != 0) || (s.Delivery.Enabled && !s.Delivery.Configured) || len(s.Delivery.Host) > 253 || len(s.Delivery.LastError) > 512 {
		return invalid
	}
	ruleIDs := map[int64]bool{}
	for _, r := range s.Rules {
		if r.Config.Validate() != nil || r.ID <= 0 || r.ID > s.NextRuleID || ruleIDs[r.ID] || r.TargetIdentity == "" || len(r.TargetIdentity) > 128 || len(r.Reason) > 512 {
			return invalid
		}
		ruleIDs[r.ID] = true
		switch r.State {
		case "disabled", "unknown", "pending", "firing", "clear":
		default:
			return invalid
		}
		if r.LastEvidence < 0 || r.LastEvaluation < 0 || r.PendingSince < 0 || r.LastNotification < 0 || r.IncidentID < 0 || (r.Value != nil && (math.IsNaN(*r.Value) || math.IsInf(*r.Value, 0))) {
			return invalid
		}
	}
	incidentIDs := map[int64]bool{}
	for _, incident := range s.Incidents {
		if incident.ID <= 0 || incident.ID > s.NextIncidentID || incidentIDs[incident.ID] || incident.RuleID <= 0 || incident.RuleID > s.NextRuleID || incident.DeviceID <= 0 || len(incident.RuleName) > 320 || len(incident.DeviceName) > 1024 || len(incident.DeliveryError) > 512 || incident.StartedAt <= 0 {
			return invalid
		}
		incidentIDs[incident.ID] = true
		if incident.State != "firing" && incident.State != "resolved" {
			return invalid
		}
		if incident.State == "resolved" && (incident.ResolvedAt == nil || *incident.ResolvedAt < incident.StartedAt) {
			return invalid
		}
		if incident.State == "firing" && incident.ResolvedAt != nil {
			return invalid
		}
		if incident.Value != nil && (math.IsNaN(*incident.Value) || math.IsInf(*incident.Value, 0)) {
			return invalid
		}
	}
	for _, rule := range s.Rules {
		if rule.IncidentID != 0 && !incidentIDs[rule.IncidentID] {
			return invalid
		}
	}
	for _, q := range s.Queue {
		n := q.Notification
		if !incidentIDs[n.Incident.ID] || (n.Event != "firing" && n.Event != "resolved") || n.EventID != fmt.Sprintf("oonfeewrt-alert-%d-%s", n.Incident.ID, n.Event) || q.Attempts < 0 || q.Attempts > 3 || q.NextAttempt < 0 || q.ExpiresAt < n.At || n.At <= 0 {
			return invalid
		}
	}
	return nil
}

// ValidateDestination authenticates the contents after Keeper.Unseal without
// returning any secret string to recovery reports, handlers or logs.
func ValidateDestination(plain []byte, host string) error {
	var d destination
	if json.Unmarshal(plain, &d) != nil {
		return errors.New("stored webhook destination is invalid")
	}
	u, err := ValidateWebhookURL(d.URL)
	if err != nil || u.Hostname() != host || len(d.Token) > 4096 || strings.ContainsAny(d.Token, "\r\n\x00") {
		return errors.New("stored webhook destination is invalid")
	}
	return nil
}
