package store

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/alerts"
)

func TestAlertStatePersistsWithSchema24Migration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alerts.db")
	keeper := testProtector(t, path)
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`DROP TABLE controller_alert_state; UPDATE schema_version SET version=23 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatalf("v23 migration: %v", err)
	}
	state, err := db.LoadAlertState(ctx)
	if err != nil || len(state.Rules) != 0 {
		t.Fatalf("empty migrated state: %+v %v", state, err)
	}
	state.NextRuleID, state.NextIncidentID = 10, 20
	state.Delivery.Host = "hooks.example.com"
	if err := db.SaveAlertState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err = db.LoadAlertState(ctx)
	if err != nil || state.NextRuleID != 10 || state.NextIncidentID != 20 || state.Delivery.Host != "hooks.example.com" {
		t.Fatalf("state lost on reopen: %+v %v", state, err)
	}
	if err := db.SaveAlertState(ctx, alerts.PersistentState{Rules: make([]alerts.RuleRecord, 51)}); err == nil {
		t.Fatal("unbounded rules accepted")
	}
}

func TestPortableRestorePausesAlertDeliveryUntilExplicitReenable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alerts.db")
	keeper := testProtector(t, path)
	db, err := Open(ctx, driver, path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := int64(10000)
	known, value := true, float64(1)
	manager := alerts.New(db, func(context.Context, []alerts.RuleRecord, time.Time) (map[int64]alerts.Evidence, error) {
		return map[int64]alerts.Evidence{1: {Known: known, Value: value, ObservedAt: now, MaxGap: 120}}, nil
	}, func() alerts.Keeper { return keeper })
	manager.Now = func() time.Time { return time.Unix(now, 0) }
	sends := 0
	manager.Send = func(context.Context, string, string, alerts.Notification) error { sends++; return nil }
	url := "https://hooks.example.com/private-secret"
	if _, err := manager.SetDelivery(ctx, alerts.DeliveryRequest{Enabled: true, URL: &url}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SaveRule(ctx, 0, alerts.Config{Name: "Gateway", Condition: "device_offline", DeviceID: 1, HoldSeconds: 60, CooldownSeconds: 60, Enabled: true}, "fixture-identity"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := db.LoadAlertState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := bytes.Clone(state.DestinationCiphertext)
	state.NextIncidentID = 1
	state.Rules[0].IncidentID = 1
	state.Rules[0].LastNotification = now
	state.Incidents = []alerts.Incident{{ID: 1, RuleID: 1, RuleName: "Gateway", DeviceID: 1, Condition: "device_offline", State: "firing", StartedAt: now, DeliveryState: "pending"}}
	state.Queue = []alerts.QueuedDelivery{{Notification: alerts.Notification{EventID: "oonfeewrt-alert-1-firing", Event: "firing", At: now, Incident: state.Incidents[0]}, NextAttempt: now, ExpiresAt: now + 1800}}
	if err := db.SaveAlertState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := db.PausePortableRestoreAlertDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	state, err = db.LoadAlertState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Delivery.Enabled || !state.Delivery.Configured || len(state.Queue) != 0 || state.EvaluatedAt != nil || !bytes.Equal(ciphertext, state.DestinationCiphertext) {
		t.Fatal("restore failed to pause delivery or preserve its encrypted destination")
	}
	rule := state.Rules[0]
	if rule.State != "unknown" || rule.LastEvaluation != 0 || rule.PendingSince != 0 || rule.LastNotification != now || rule.IncidentID != 1 || state.Incidents[0].State != "firing" || state.Incidents[0].DeliveryState != "cancelled" {
		t.Fatal("restore lost cooldown/history or retained pending continuity")
	}
	known = false
	now += 60
	if err := manager.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if sends != 0 {
		t.Fatal("restored outbox delivered without owner authorization")
	}
	list, _ := manager.List(ctx)
	if list.Incidents[0].State != "firing" {
		t.Fatal("unknown restore evidence falsely recovered")
	}
	if _, err := manager.SetDelivery(ctx, alerts.DeliveryRequest{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now += 60
	if err := manager.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if sends != 0 {
		t.Fatal("reenabling replayed cancelled history")
	}
	known, value = true, 0
	now += 60
	if err := manager.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatal("explicit reenable did not allow a new evidenced transition")
	}
}

func TestAlertSchemaBoundsAttestedAndMalformedStateFailsClosed(t *testing.T) {
	db := open(t)
	if _, err := db.SQL().Exec(`INSERT INTO controller_alert_state(id,state_json) VALUES(2,'{}')`); err == nil {
		t.Fatal("second alert snapshot admitted")
	}
	if _, err := db.SQL().Exec(`INSERT INTO controller_alert_state(id,state_json) VALUES(1,'not json')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LoadAlertState(context.Background()); err == nil {
		t.Fatal("invalid state silently reset")
	}
	if _, err := db.SQL().Exec(`DROP TABLE controller_alert_state; CREATE TABLE controller_alert_state(id INTEGER PRIMARY KEY,state_json BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := verifySchemaV24(context.Background(), db.SQL()); err == nil {
		t.Fatal("missing singleton/size bounds accepted")
	}
}

func TestAlertRecoveryAuthenticatesEveryStoredDestination(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	plain := []byte(`{"url":"https://hooks.example.com/private-secret","token":"private-token"}`)
	ciphertext, err := db.protector.Seal(plain, []byte(alerts.DestinationContext))
	if err != nil {
		t.Fatal(err)
	}
	state := alerts.PersistentState{Delivery: alerts.Delivery{Configured: true, Enabled: true, Host: "hooks.example.com"}, DestinationCiphertext: ciphertext}
	if err := db.SaveAlertState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := db.validateRecoveryAlerts(ctx, db.SQL()); err != nil {
		t.Fatal(err)
	}
	state.DestinationCiphertext[len(state.DestinationCiphertext)-1] ^= 1
	if err := db.SaveAlertState(ctx, state); err != nil {
		t.Fatal(err)
	}
	err = db.validateRecoveryAlerts(ctx, db.SQL())
	if err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatalf("corrupt destination accepted or leaked: %v", err)
	}
}
