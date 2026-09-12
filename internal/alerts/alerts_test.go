package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
)

type memoryRepository struct {
	raw      []byte
	failSave bool
}

func (r *memoryRepository) LoadAlertState(context.Context) (PersistentState, error) {
	var state PersistentState
	if len(r.raw) != 0 {
		return state, json.Unmarshal(r.raw, &state)
	}
	return state, nil
}
func (r *memoryRepository) SaveAlertState(_ context.Context, state PersistentState) error {
	if r.failSave {
		return errors.New("fixture persistence failure")
	}
	var err error
	r.raw, err = json.Marshal(state)
	return err
}

func testManager(t *testing.T) (*Manager, *memoryRepository, *Evidence, *int64) {
	t.Helper()
	keeper, err := secrets.Create(filepath.Join(t.TempDir(), "keyring"), []byte("fixture passphrase"), secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { keeper.Close() })
	repository := &memoryRepository{}
	now := int64(10000)
	evidence := Evidence{Known: true, Value: 1, ObservedAt: now, MaxGap: 120, DeviceName: "Fixture gateway"}
	manager := New(repository, func(_ context.Context, rules []RuleRecord, _ time.Time) (map[int64]Evidence, error) {
		out := map[int64]Evidence{}
		for _, rule := range rules {
			out[rule.ID] = evidence
		}
		return out, nil
	}, func() Keeper { return keeper })
	manager.Now = func() time.Time { return time.Unix(now, 0) }
	manager.Send = func(context.Context, string, string, Notification) error {
		t.Fatal("unexpected network delivery")
		return nil
	}
	return manager, repository, &evidence, &now
}

func ruleConfig() Config {
	return Config{Name: "Gateway offline", Condition: "device_offline", DeviceID: 1, HoldSeconds: 60, CooldownSeconds: 600, Enabled: true}
}

func mustTick(t *testing.T, manager *Manager) {
	t.Helper()
	if err := manager.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSustainedConditionUnknownAndRecovery(t *testing.T) {
	m, _, e, now := testManager(t)
	if _, err := m.SaveRule(context.Background(), 0, ruleConfig(), "fixture-identity"); err != nil {
		t.Fatal(err)
	}
	mustTick(t, m)
	list, _ := m.List(context.Background())
	if list.Rules[0].State != "pending" || len(list.Incidents) != 0 {
		t.Fatalf("first observation: %+v", list)
	}
	*now += 60
	e.ObservedAt = *now
	mustTick(t, m)
	list, _ = m.List(context.Background())
	if list.Rules[0].State != "firing" || len(list.Incidents) != 1 {
		t.Fatalf("sustained evidence: %+v", list)
	}
	e.Known = false
	*now += 60
	mustTick(t, m)
	list, _ = m.List(context.Background())
	if list.Rules[0].State != "unknown" || list.Incidents[0].State != "firing" || list.Incidents[0].ResolvedAt != nil {
		t.Fatalf("unknown falsely recovered: %+v", list)
	}
	e.Known, e.Value = true, 0
	*now += 60
	e.ObservedAt = *now
	mustTick(t, m)
	list, _ = m.List(context.Background())
	if list.Rules[0].State != "clear" || list.Incidents[0].State != "resolved" || *list.Incidents[0].ResolvedAt != *now {
		t.Fatalf("fresh recovery: %+v", list)
	}
}

func TestRetainedWANPointCannotSatisfyHoldAndMissingBucketResets(t *testing.T) {
	m, _, e, now := testManager(t)
	config := ruleConfig()
	config.Condition, config.Threshold, config.HoldSeconds = "wan_latency", 100, 300
	e.Value, e.MaxGap = 200, 300
	if _, err := m.SaveRule(context.Background(), 0, config, "fixture-identity"); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		mustTick(t, m)
		*now += 60
	}
	list, _ := m.List(context.Background())
	if len(list.Incidents) != 0 {
		t.Fatal("one retained point became a sustained alarm")
	}
	mustTick(t, m) // original bucket is now too old
	*now += 240
	e.ObservedAt = *now
	mustTick(t, m)
	list, _ = m.List(context.Background())
	if len(list.Incidents) != 0 || list.Rules[0].State != "pending" {
		t.Fatal("missing bucket did not reset continuity")
	}
	for range 4 {
		*now += 60
		mustTick(t, m)
	}
	*now += 60
	e.ObservedAt = *now
	mustTick(t, m)
	list, _ = m.List(context.Background())
	if len(list.Incidents) != 1 {
		t.Fatalf("two contiguous fresh buckets did not trigger: %+v", list)
	}
}

func TestCooldownSurvivesManagerRestartAndStopsRepeatFlood(t *testing.T) {
	m, repository, e, now := testManager(t)
	url, token := "https://hooks.example.com/private-key", "fixture-bearer-secret"
	if _, err := m.SetDelivery(context.Background(), DeliveryRequest{Enabled: true, URL: &url, BearerToken: &token}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(repository.raw), "private-key") || strings.Contains(string(repository.raw), token) {
		t.Fatal("plaintext destination persisted")
	}
	deliveries := []Notification{}
	sender := func(_ context.Context, gotURL, gotToken string, notification Notification) error {
		if gotURL != url || gotToken != token {
			t.Fatal("encrypted destination did not round-trip")
		}
		deliveries = append(deliveries, notification)
		return nil
	}
	m.Send = sender
	if _, err := m.SaveRule(context.Background(), 0, ruleConfig(), "fixture-identity"); err != nil {
		t.Fatal(err)
	}
	mustTick(t, m)
	*now += 60
	e.ObservedAt = *now
	mustTick(t, m)
	if len(deliveries) != 1 || deliveries[0].Event != "firing" {
		t.Fatal("initial notification missing")
	}
	restarted := New(repository, m.Source, m.Keys)
	restarted.Now, restarted.Send = m.Now, sender
	for range 3 {
		*now += 60
		e.ObservedAt = *now
		mustTick(t, restarted)
	}
	if len(deliveries) != 1 {
		t.Fatal("firing incident repeated after restart")
	}
	e.Value = 0
	*now += 60
	e.ObservedAt = *now
	mustTick(t, restarted)
	e.Value = 1
	*now += 60
	e.ObservedAt = *now
	mustTick(t, restarted)
	*now += 60
	e.ObservedAt = *now
	mustTick(t, restarted)
	list, _ := restarted.List(context.Background())
	if len(deliveries) != 2 || deliveries[1].Event != "resolved" || list.Incidents[0].DeliveryState != "cooldown" {
		t.Fatalf("cooldown failed: deliveries=%+v list=%+v", deliveries, list)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "private-key") || strings.Contains(string(raw), token) || strings.Contains(string(raw), "destination_ciphertext") || strings.Contains(string(raw), "fixture-identity") {
		t.Fatal("collection exposed private configuration")
	}
}

func TestPersistenceFailurePreventsSendAndTransportFailureIsRedacted(t *testing.T) {
	m, repository, e, now := testManager(t)
	url := "https://hooks.example.com/secret-token"
	if _, err := m.SetDelivery(context.Background(), DeliveryRequest{Enabled: true, URL: &url}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveRule(context.Background(), 0, ruleConfig(), "fixture"); err != nil {
		t.Fatal(err)
	}
	mustTick(t, m)
	*now += 60
	e.ObservedAt = *now
	repository.failSave = true
	if err := m.Tick(context.Background()); err == nil {
		t.Fatal("persistence failure was ignored")
	}
	repository.failSave = false
	sends := 0
	m.Send = func(context.Context, string, string, Notification) error {
		sends++
		return errors.New("POST " + url + ": fixture upstream details")
	}
	mustTick(t, m)
	list, _ := m.List(context.Background())
	if sends != 1 || list.Incidents[0].DeliveryState != "failed" || strings.Contains(list.Delivery.LastError, "secret-token") {
		t.Fatalf("delivery failure was not safely exposed: %+v", list)
	}
	if strings.Contains(string(repository.raw), "fixture upstream") {
		t.Fatal("transport error leaked into durable state")
	}
	if _, err := m.SetDelivery(context.Background(), DeliveryRequest{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	*now += 300
	e.ObservedAt = *now
	mustTick(t, m)
	if sends != 1 {
		t.Fatal("disabled delivery retried an old event")
	}
}

func TestActiveRuleCanDisableWithoutClaimingRecovery(t *testing.T) {
	m, _, e, now := testManager(t)
	config := ruleConfig()
	rule, err := m.SaveRule(context.Background(), 0, config, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	mustTick(t, m)
	*now += 60
	e.ObservedAt = *now
	mustTick(t, m)
	config.Enabled = false
	if _, err := m.SaveRule(context.Background(), rule.ID, config, "fixture"); err != nil {
		t.Fatal(err)
	}
	list, _ := m.List(context.Background())
	if list.Rules[0].State != "disabled" || list.Incidents[0].State != "firing" {
		t.Fatal("disabling a rule implied recovery")
	}
	config.DeviceID = 2
	if _, err := m.SaveRule(context.Background(), rule.ID, config, "different"); !errors.Is(err, ErrConflict) {
		t.Fatal("active rule was retargeted")
	}
}

func TestDisablingRuleCancelsOnlyItsPendingRetryWithoutRecoveryOrReplay(t *testing.T) {
	m, repository, e, now := testManager(t)
	ctx := context.Background()
	url := "https://hooks.example.com/alerts"
	if _, err := m.SetDelivery(ctx, DeliveryRequest{Enabled: true, URL: &url}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := m.SaveRule(ctx, 0, ruleConfig(), "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	var sends []int64
	m.Send = func(_ context.Context, _, _ string, notification Notification) error {
		sends = append(sends, notification.Incident.RuleID)
		if notification.Incident.RuleID == 1 {
			return errors.New("fixture retry")
		}
		return nil
	}
	mustTick(t, m)
	*now += 60
	e.ObservedAt = *now
	mustTick(t, m)
	state, _ := repository.LoadAlertState(ctx)
	if len(state.Queue) != 2 || state.Queue[0].Attempts != 1 {
		t.Fatal("fixture did not produce a retry and an unrelated pending send")
	}
	config := ruleConfig()
	config.Enabled = false
	if _, err := m.SaveRule(ctx, 1, config, "fixture"); err != nil {
		t.Fatal(err)
	}
	state, _ = repository.LoadAlertState(ctx)
	if len(state.Queue) != 1 || state.Queue[0].Notification.Incident.RuleID != 2 || state.Incidents[0].State != "firing" || state.Incidents[0].DeliveryState != "cancelled" || state.Incidents[1].DeliveryState != "pending" {
		t.Fatal("disable lost unrelated delivery, claimed recovery, or retained a retry")
	}
	restarted := New(repository, m.Source, m.Keys)
	restarted.Now, restarted.Send = m.Now, m.Send
	*now += 300
	e.ObservedAt = *now
	mustTick(t, restarted)
	if len(sends) != 2 || sends[0] != 1 || sends[1] != 2 {
		t.Fatalf("disabled rule retried after restart: %v", sends)
	}
	config.Enabled = true
	if _, err := restarted.SaveRule(ctx, 1, config, "fixture"); err != nil {
		t.Fatal(err)
	}
	*now += 60
	e.ObservedAt = *now
	mustTick(t, restarted)
	if len(sends) != 2 {
		t.Fatal("reenabling replayed the cancelled notification")
	}
}

func TestDisablingRecoveredRuleCancelsPendingRecoveryDelivery(t *testing.T) {
	m, repository, e, now := testManager(t)
	ctx := context.Background()
	url := "https://hooks.example.com/alerts"
	if _, err := m.SetDelivery(ctx, DeliveryRequest{Enabled: true, URL: &url}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveRule(ctx, 0, ruleConfig(), "fixture"); err != nil {
		t.Fatal(err)
	}
	m.Send = func(_ context.Context, _, _ string, notification Notification) error {
		if notification.Event == "resolved" {
			return errors.New("fixture retry")
		}
		return nil
	}
	mustTick(t, m)
	*now += 60
	e.ObservedAt = *now
	mustTick(t, m)
	*now += 60
	e.ObservedAt, e.Value = *now, 0
	mustTick(t, m)
	config := ruleConfig()
	config.Enabled = false
	if _, err := m.SaveRule(ctx, 1, config, "fixture"); err != nil {
		t.Fatal(err)
	}
	state, _ := repository.LoadAlertState(ctx)
	if len(state.Queue) != 0 || state.Incidents[0].State != "resolved" || state.Incidents[0].DeliveryState != "cancelled" {
		t.Fatal("disable retained a recovery retry or rewrote incident history")
	}
}

type staticResolver []netip.Addr

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r, nil
}

func TestWebhookRejectsSSRFAndMixedDNSWithoutContactingNetwork(t *testing.T) {
	for _, raw := range []string{
		"http://example.com", "https://user:secret@example.com/", "https://example.com/#secret",
		"https://127.0.0.1/", "https://169.254.169.254/latest/meta-data/", "https://192.168.1.1/",
		"https://10.0.0.1/", "https://100.64.0.1/", "https://[::1]/", "https://[::ffff:127.0.0.1]/",
		"https://[fc00::1]/", "https://[64:ff9b::a00:1]/", "https://localhost/", "https://router.local/",
		"https://example.com:99999/", "https://example.com/%zz",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ValidateWebhookURL(raw); err == nil {
				t.Fatal("unsafe URL accepted")
			}
		})
	}
	if _, err := ValidateWebhookURL("https://hooks.example.com/path?token=fixture"); err != nil {
		t.Fatal(err)
	}
	_, err := publicAddresses(context.Background(), staticResolver{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("10.0.0.1")}, "rebinding.example")
	if err == nil {
		t.Fatal("mixed public/private DNS accepted")
	}
	ips, err := publicAddresses(context.Background(), staticResolver{netip.MustParseAddr("1.1.1.1")}, "public.example")
	if err != nil || len(ips) != 1 {
		t.Fatalf("public resolution rejected: %v", err)
	}
	oversized := make(staticResolver, 65)
	for i := range oversized {
		oversized[i] = netip.MustParseAddr("1.1.1.1")
	}
	if _, err := publicAddresses(context.Background(), oversized, "oversized.example"); err == nil {
		t.Fatal("unbounded DNS answer list accepted")
	}
}

func TestWebhookFallsBackAcrossValidatedAddressesWithinDeadline(t *testing.T) {
	ips := []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("1.1.1.1")}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	var attempts []string
	connection, err := dialValidated(ctx, ips, "443", func(attempt context.Context, network, address string) (net.Conn, error) {
		attempts = append(attempts, address)
		if network != "tcp" {
			t.Fatal("unexpected network")
		}
		if len(attempts) == 1 {
			<-attempt.Done()
			return nil, attempt.Err()
		}
		return client, nil
	})
	if err != nil || connection != client || len(attempts) != 2 || attempts[0] != "[2606:4700:4700::1111]:443" || attempts[1] != "1.1.1.1:443" {
		t.Fatalf("validated fallback failed: %v %v", attempts, err)
	}
	if ctx.Err() != nil {
		t.Fatal("first address consumed the complete request budget")
	}
}

func TestWebhookFallbackHonorsCancellationAndRedactsDialErrors(t *testing.T) {
	ips := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, err := dialValidated(ctx, ips, "443", func(context.Context, string, string) (net.Conn, error) {
		attempts++
		cancel()
		return nil, errors.New("private diagnostic details")
	})
	if attempts != 1 || err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("cancellation or redaction failed: attempts=%d error=%v", attempts, err)
	}
}

func TestRuleValidationBounds(t *testing.T) {
	for _, edit := range []func(*Config){
		func(c *Config) { c.Name = "" }, func(c *Config) { c.DeviceID = 0 }, func(c *Config) { c.HoldSeconds = 0 },
		func(c *Config) { c.CooldownSeconds = 604801 }, func(c *Config) { c.Condition = "unknown" },
		func(c *Config) { c.Condition = "wan_loss"; c.Threshold = 101 }, func(c *Config) { c.Threshold = 1 },
	} {
		c := ruleConfig()
		edit(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
}

func TestDeliveryCanBeDisabledAndClearedWithoutDecrypting(t *testing.T) {
	m, _, _, _ := testManager(t)
	url := "https://hooks.example.com/fixture-secret"
	if _, err := m.SetDelivery(context.Background(), DeliveryRequest{Enabled: true, URL: &url}); err != nil {
		t.Fatal(err)
	}
	m.Keys = func() Keeper { return nil }
	delivery, err := m.SetDelivery(context.Background(), DeliveryRequest{Enabled: false})
	if err != nil || delivery.Enabled || !delivery.Configured {
		t.Fatalf("disable without key: %+v %v", delivery, err)
	}
	empty := ""
	delivery, err = m.SetDelivery(context.Background(), DeliveryRequest{Enabled: false, URL: &empty})
	if err != nil || delivery.Configured {
		t.Fatalf("clear without key: %+v %v", delivery, err)
	}
}

func TestOpenIncidentCountIncludesRowsOutsideNewestHundred(t *testing.T) {
	m, repository, _, _ := testManager(t)
	state := PersistentState{Incidents: make([]Incident, 150)}
	for i := range state.Incidents {
		state.Incidents[i] = Incident{ID: int64(i + 1), State: "resolved"}
	}
	state.Incidents[0].State = "firing"
	if err := repository.SaveAlertState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	list, err := m.List(context.Background())
	if err != nil || len(list.Incidents) != 100 || list.Counts.OpenIncidents != 1 {
		t.Fatalf("bounded page lost full count: %+v %v", list, err)
	}
}
