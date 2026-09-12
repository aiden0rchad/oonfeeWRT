// Package alerts evaluates controller-local rules using existing observations.
// It never polls, configures, restarts, or installs anything on a router.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const MaxRules = 50

var ErrNotFound = errors.New("alert rule not found")
var ErrConflict = errors.New("alert rule cannot be changed in its current state")

type Config struct {
	Name            string  `json:"name"`
	Condition       string  `json:"condition"`
	DeviceID        int64   `json:"device_id"`
	Threshold       float64 `json:"threshold"`
	HoldSeconds     int64   `json:"hold_seconds"`
	CooldownSeconds int64   `json:"cooldown_seconds"`
	Enabled         bool    `json:"enabled"`
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Name) == "" || utf8.RuneCountInString(c.Name) > 80 || strings.ContainsAny(c.Name, "\r\n\x00") {
		return errors.New("name must contain 1–80 characters without line breaks")
	}
	if c.DeviceID <= 0 {
		return errors.New("device_id must be positive")
	}
	if c.HoldSeconds < 60 || c.HoldSeconds > 86400 {
		return errors.New("hold_seconds must be between 60 and 86400")
	}
	if c.CooldownSeconds < 60 || c.CooldownSeconds > 604800 {
		return errors.New("cooldown_seconds must be between 60 and 604800")
	}
	if math.IsNaN(c.Threshold) || math.IsInf(c.Threshold, 0) {
		return errors.New("threshold must be finite")
	}
	switch c.Condition {
	case "device_offline":
		if c.Threshold != 0 {
			return errors.New("device_offline threshold must be zero")
		}
	case "wan_latency":
		if c.Threshold <= 0 || c.Threshold > 60000 {
			return errors.New("WAN latency threshold must be greater than zero and at most 60000 ms")
		}
	case "wan_loss":
		if c.Threshold <= 0 || c.Threshold > 100 {
			return errors.New("WAN loss threshold must be greater than zero and at most 100 percent")
		}
	default:
		return errors.New("condition must be device_offline, wan_latency, or wan_loss")
	}
	return nil
}

type Rule struct {
	Config
	ID         int64    `json:"id"`
	State      string   `json:"state"`
	Since      *int64   `json:"since"`
	Value      *float64 `json:"value"`
	ObservedAt *int64   `json:"observed_at"`
	Reason     string   `json:"reason"`
}

// RuleRecord retains continuity and cooldown separately from the public view.
type RuleRecord struct {
	Rule
	TargetIdentity   string `json:"target_identity"`
	PendingSince     int64  `json:"pending_since"`
	LastEvidence     int64  `json:"last_evidence"`
	LastEvaluation   int64  `json:"last_evaluation"`
	LastNotification int64  `json:"last_notification"`
	IncidentID       int64  `json:"incident_id"`
}

type Incident struct {
	ID            int64    `json:"id"`
	RuleID        int64    `json:"rule_id"`
	RuleName      string   `json:"rule_name"`
	DeviceID      int64    `json:"device_id"`
	DeviceName    string   `json:"device_name"`
	Condition     string   `json:"condition"`
	State         string   `json:"state"`
	StartedAt     int64    `json:"started_at"`
	ResolvedAt    *int64   `json:"resolved_at"`
	Value         *float64 `json:"value"`
	DeliveryState string   `json:"delivery_state"`
	DeliveryError string   `json:"delivery_error"`
}

type Delivery struct {
	Configured    bool   `json:"configured"`
	Host          string `json:"host"`
	Enabled       bool   `json:"enabled"`
	LastAttemptAt *int64 `json:"last_attempt_at"`
	LastSuccessAt *int64 `json:"last_success_at"`
	LastError     string `json:"last_error"`
}

type Notification struct {
	EventID  string   `json:"event_id"`
	Event    string   `json:"event"`
	At       int64    `json:"at"`
	Incident Incident `json:"incident"`
}

type QueuedDelivery struct {
	Notification Notification `json:"notification"`
	Attempts     int          `json:"attempts"`
	NextAttempt  int64        `json:"next_attempt"`
	ExpiresAt    int64        `json:"expires_at"`
}

// PersistentState is bounded and committed atomically with its notification
// outbox. Neither URLs nor bearer tokens are stored in plaintext.
type PersistentState struct {
	Rules                 []RuleRecord     `json:"rules"`
	Incidents             []Incident       `json:"incidents"`
	Queue                 []QueuedDelivery `json:"queue"`
	Delivery              Delivery         `json:"delivery"`
	DestinationCiphertext []byte           `json:"destination_ciphertext"`
	EvaluatedAt           *int64           `json:"evaluated_at"`
	NextRuleID            int64            `json:"next_rule_id"`
	NextIncidentID        int64            `json:"next_incident_id"`
}

type Collection struct {
	Counts struct {
		OpenIncidents int `json:"open_incidents"`
	} `json:"counts"`
	Rules       []Rule     `json:"rules"`
	Incidents   []Incident `json:"incidents"`
	Delivery    Delivery   `json:"delivery"`
	EvaluatedAt *int64     `json:"evaluated_at"`
}

type Repository interface {
	LoadAlertState(context.Context) (PersistentState, error)
	SaveAlertState(context.Context, PersistentState) error
}

type Keeper interface {
	Seal([]byte, []byte) ([]byte, error)
	Unseal([]byte, []byte) ([]byte, error)
}

type Evidence struct {
	Known      bool
	Value      float64
	ObservedAt int64
	MaxGap     int64
	DeviceName string
	Reason     string
}

type Source func(context.Context, []RuleRecord, time.Time) (map[int64]Evidence, error)
type Sender func(context.Context, string, string, Notification) error

type Manager struct {
	mu     sync.Mutex
	Store  Repository
	Keys   func() Keeper
	Source Source
	Send   Sender
	Now    func() time.Time
}

func New(repository Repository, source Source, keys func() Keeper) *Manager {
	return &Manager{Store: repository, Source: source, Keys: keys, Send: SendWebhook, Now: time.Now}
}

func (m *Manager) List(ctx context.Context) (Collection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.Store.LoadAlertState(ctx)
	if err != nil {
		return Collection{}, err
	}
	out := Collection{Rules: []Rule{}, Incidents: []Incident{}, Delivery: s.Delivery, EvaluatedAt: s.EvaluatedAt}
	for _, incident := range s.Incidents {
		if incident.State == "firing" {
			out.Counts.OpenIncidents++
		}
	}
	for _, rule := range s.Rules {
		out.Rules = append(out.Rules, rule.Rule)
	}
	for i := len(s.Incidents) - 1; i >= 0 && len(out.Incidents) < 100; i-- {
		out.Incidents = append(out.Incidents, s.Incidents[i])
	}
	return out, nil
}

func (m *Manager) SaveRule(ctx context.Context, id int64, config Config, identity string) (Rule, error) {
	if err := config.Validate(); err != nil {
		return Rule{}, err
	}
	if identity == "" {
		return Rule{}, errors.New("device identity is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.Store.LoadAlertState(ctx)
	if err != nil {
		return Rule{}, err
	}
	index := -1
	for i := range s.Rules {
		if s.Rules[i].ID == id {
			index = i
			break
		}
	}
	if id != 0 && index < 0 {
		return Rule{}, ErrNotFound
	}
	if id == 0 {
		if len(s.Rules) >= MaxRules {
			return Rule{}, fmt.Errorf("%w: a maximum of 50 alert rules is supported", ErrConflict)
		}
		s.NextRuleID++
		id = s.NextRuleID
		index = len(s.Rules)
		s.Rules = append(s.Rules, RuleRecord{})
	} else if s.Rules[index].IncidentID != 0 {
		previous := s.Rules[index].Config
		previous.Enabled = config.Enabled
		if previous != config || s.Rules[index].TargetIdentity != identity {
			return Rule{}, fmt.Errorf("%w: an active rule can be disabled, but its condition cannot be edited until recovery", ErrConflict)
		}
		s.Rules[index].Enabled = config.Enabled
		s.Rules[index].State, s.Rules[index].Reason = "disabled", "Rule is disabled; the incident has not been marked recovered"
		if config.Enabled {
			s.Rules[index].State, s.Rules[index].Reason = "unknown", "Waiting for fresh evidence"
		} else {
			cancelQueuedDeliveries(&s, id)
		}
		if err := m.Store.SaveAlertState(ctx, s); err != nil {
			return Rule{}, err
		}
		return s.Rules[index].Rule, nil
	}
	state := "unknown"
	if !config.Enabled {
		state = "disabled"
	}
	rule := Rule{Config: config, ID: id, State: state, Reason: "Waiting for the next evaluation"}
	lastNotification := s.Rules[index].LastNotification
	s.Rules[index] = RuleRecord{Rule: rule, TargetIdentity: identity, LastNotification: lastNotification}
	if !config.Enabled {
		cancelQueuedDeliveries(&s, id)
	}
	if err := m.Store.SaveAlertState(ctx, s); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

func (m *Manager) DeleteRule(ctx context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.Store.LoadAlertState(ctx)
	if err != nil {
		return err
	}
	for i, rule := range s.Rules {
		if rule.ID != id {
			continue
		}
		s.Rules = append(s.Rules[:i], s.Rules[i+1:]...)
		// Deletion stops evaluation and queued sends. It never invents recovery.
		cancelQueuedDeliveries(&s, id)
		return m.Store.SaveAlertState(ctx, s)
	}
	return ErrNotFound
}

type DeliveryRequest struct {
	Enabled     bool    `json:"enabled"`
	URL         *string `json:"url"`
	BearerToken *string `json:"bearer_token"`
}

type destination struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

const DestinationContext = "oonfeewrt:alert-webhook:v1"

func (m *Manager) destination(s PersistentState) (destination, error) {
	if len(s.DestinationCiphertext) == 0 {
		return destination{}, nil
	}
	if m.Keys == nil || m.Keys() == nil {
		return destination{}, errors.New("notification keyring is unavailable")
	}
	plain, err := m.Keys().Unseal(s.DestinationCiphertext, []byte(DestinationContext))
	if err != nil {
		return destination{}, errors.New("notification destination could not be decrypted")
	}
	defer clear(plain)
	var d destination
	if json.Unmarshal(plain, &d) != nil {
		return destination{}, errors.New("notification destination is invalid")
	}
	return d, nil
}

func (m *Manager) SetDelivery(ctx context.Context, request DeliveryRequest) (Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.Store.LoadAlertState(ctx)
	if err != nil {
		return Delivery{}, err
	}
	// Disabling or clearing delivery must still work when its keyring cannot
	// be opened. Neither action needs to recover the saved secret.
	clearURL := request.URL != nil && strings.TrimSpace(*request.URL) == ""
	if !request.Enabled && (clearURL || request.URL == nil && request.BearerToken == nil) {
		s.Delivery.Enabled = false
		if clearURL {
			s.DestinationCiphertext = nil
			s.Delivery = Delivery{}
		}
		cancelQueuedDeliveries(&s, 0)
		if err := m.Store.SaveAlertState(ctx, s); err != nil {
			return Delivery{}, err
		}
		return s.Delivery, nil
	}
	d, err := m.destination(s)
	if err != nil {
		return Delivery{}, err
	}
	if request.URL != nil {
		d.URL = strings.TrimSpace(*request.URL)
	}
	if request.BearerToken != nil {
		d.Token = *request.BearerToken
	}
	if len(d.Token) > 4096 || strings.ContainsAny(d.Token, "\r\n\x00") {
		return Delivery{}, errors.New("bearer token must be at most 4096 characters without line breaks")
	}
	if d.URL == "" && request.Enabled {
		return Delivery{}, errors.New("configure an HTTPS webhook URL before enabling delivery")
	}
	s.DestinationCiphertext = nil
	s.Delivery = Delivery{Enabled: request.Enabled, Configured: d.URL != ""}
	if d.URL != "" {
		u, err := ValidateWebhookURL(d.URL)
		if err != nil {
			return Delivery{}, err
		}
		s.Delivery.Host = u.Hostname()
		if m.Keys == nil || m.Keys() == nil {
			return Delivery{}, errors.New("notification keyring is unavailable")
		}
		plain, _ := json.Marshal(d)
		s.DestinationCiphertext, err = m.Keys().Seal(plain, []byte(DestinationContext))
		clear(plain)
		if err != nil {
			return Delivery{}, errors.New("notification destination could not be encrypted")
		}
	}
	// Explicit config changes never replay old events to a new destination.
	cancelQueuedDeliveries(&s, 0)
	if err := m.Store.SaveAlertState(ctx, s); err != nil {
		return Delivery{}, err
	}
	return s.Delivery, nil
}

// A zero rule ID cancels the entire outbox. Failed attempts that are still
// queued are cancelled too; unrelated completed delivery history is preserved.
func cancelQueuedDeliveries(s *PersistentState, ruleID int64) {
	keep := s.Queue[:0]
	for _, queued := range s.Queue {
		if ruleID != 0 && queued.Notification.Incident.RuleID != ruleID {
			keep = append(keep, queued)
		} else {
			finishDelivery(s, queued, "cancelled", "Pending delivery cancelled by a configuration change; no historical replay was sent")
		}
	}
	s.Queue = keep
}

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// Waiting one cadence prevents startup from replaying a burst of old data.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tickCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_ = m.Tick(tickCtx)
			cancel()
		}
	}
}

func (m *Manager) Tick(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.Store.LoadAlertState(ctx)
	if err != nil {
		return err
	}
	now := m.Now().Unix()
	evidence, err := m.Source(ctx, s.Rules, time.Unix(now, 0))
	if err != nil {
		return err
	}
	for i := range s.Rules {
		evaluate(&s, &s.Rules[i], evidence[s.Rules[i].ID], now)
	}
	s.EvaluatedAt = &now
	prune(&s)
	// Persist the condition transition before its corresponding side effect.
	if err := m.Store.SaveAlertState(ctx, s); err != nil {
		return err
	}
	for sent := 0; sent < 2 && len(s.Queue) > 0; sent++ {
		q := s.Queue[0]
		if q.NextAttempt > now {
			break
		}
		if !s.Delivery.Enabled || q.ExpiresAt < now || q.Attempts >= 3 {
			finishDelivery(&s, q, "failed", "Delivery expired or was disabled; no historical replay was sent")
			s.Queue = s.Queue[1:]
			if err := m.Store.SaveAlertState(ctx, s); err != nil {
				return err
			}
			continue
		}
		q.Attempts++
		q.NextAttempt = now + 300
		s.Queue[0] = q
		s.Delivery.LastAttemptAt = &now
		// Record an attempt before the network call. A crash may retry, so the
		// stable event_id is provided for receiver-side deduplication.
		if err := m.Store.SaveAlertState(ctx, s); err != nil {
			return err
		}
		destination, sendErr := m.destination(s)
		if sendErr == nil {
			sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			sendErr = m.Send(sendCtx, destination.URL, destination.Token, q.Notification)
			cancel()
		}
		if sendErr != nil {
			// Error text from transports is untrusted and often includes URLs.
			s.Delivery.LastError = "Webhook delivery failed. Check the public HTTPS endpoint, TLS certificate, authentication and response status."
			finishDelivery(&s, q, "failed", s.Delivery.LastError)
			if q.Attempts >= 3 {
				s.Queue = s.Queue[1:]
			}
		} else {
			s.Delivery.LastSuccessAt = &now
			s.Delivery.LastError = ""
			finishDelivery(&s, q, "sent", "")
			s.Queue = s.Queue[1:]
		}
		if err := m.Store.SaveAlertState(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func evaluate(s *PersistentState, r *RuleRecord, e Evidence, now int64) {
	if !r.Enabled {
		r.State, r.Reason = "disabled", "Rule is disabled"
		return
	}
	if r.Condition == "device_offline" && e.Value != 0 && e.Value != 1 || r.Condition == "wan_loss" && (e.Value < 0 || e.Value > 100) {
		e.Known, e.Reason = false, "The observed value is outside the condition's valid range"
	}
	gap := e.MaxGap
	if gap <= 0 {
		gap = 120
	}
	continuous := r.LastEvaluation > 0 && now-r.LastEvaluation <= 120 && now >= r.LastEvaluation
	r.LastEvaluation = now
	if !e.Known || math.IsInf(e.Value, 0) || math.IsNaN(e.Value) || e.ObservedAt <= 0 || e.ObservedAt > now || now-e.ObservedAt > gap {
		r.State, r.Reason, r.Value, r.ObservedAt = "unknown", e.Reason, nil, nil
		if r.Reason == "" {
			r.Reason = "Fresh observation is unavailable; existing incidents are not resolved"
		}
		r.PendingSince = 0
		if r.IncidentID == 0 {
			r.LastEvidence, r.Since = 0, nil
		}
		return
	}
	r.Value, r.ObservedAt = &e.Value, &e.ObservedAt
	breached := e.Value > r.Threshold
	if r.Condition == "device_offline" {
		breached = e.Value == 1
	}
	if !breached {
		if r.IncidentID != 0 && e.ObservedAt <= r.LastEvidence {
			r.State, r.Reason = "unknown", "A newer observation is required to confirm recovery"
			return
		}
		r.PendingSince, r.LastEvidence = 0, e.ObservedAt
		r.State, r.Reason, r.Since = "clear", "Latest observation is within the rule threshold", nil
		if r.IncidentID != 0 {
			for i := range s.Incidents {
				if s.Incidents[i].ID != r.IncidentID {
					continue
				}
				s.Incidents[i].State, s.Incidents[i].ResolvedAt, s.Incidents[i].Value = "resolved", &now, &e.Value
				queue(s, &s.Incidents[i], "resolved", now)
			}
			r.IncidentID = 0
		}
		return
	}
	if r.IncidentID != 0 {
		r.LastEvidence = max(r.LastEvidence, e.ObservedAt)
		r.State, r.Reason = "firing", "Condition remains above the threshold"
		return
	}
	newEvidence := e.ObservedAt > r.LastEvidence
	if !continuous || r.PendingSince == 0 || e.ObservedAt-r.LastEvidence > gap || e.ObservedAt < r.LastEvidence {
		r.PendingSince = e.ObservedAt
	}
	r.LastEvidence = e.ObservedAt
	r.State, r.Reason, r.Since = "pending", "Waiting for sustained, fresh evidence", &r.PendingSince
	if !newEvidence || e.ObservedAt-r.PendingSince < r.HoldSeconds {
		return
	}
	r.State, r.Reason = "firing", "Condition persisted for the configured duration"
	s.NextIncidentID++
	r.IncidentID = s.NextIncidentID
	incident := Incident{ID: r.IncidentID, RuleID: r.ID, RuleName: r.Name, DeviceID: r.DeviceID,
		DeviceName: e.DeviceName, Condition: r.Condition, State: "firing", StartedAt: now,
		Value: &e.Value, DeliveryState: "not_configured"}
	if r.LastNotification == 0 || now-r.LastNotification >= r.CooldownSeconds {
		queue(s, &incident, "firing", now)
		if incident.DeliveryState == "pending" {
			r.LastNotification = now
		}
	} else {
		incident.DeliveryState = "cooldown"
	}
	s.Incidents = append(s.Incidents, incident)
}

func queue(s *PersistentState, incident *Incident, event string, now int64) {
	if !s.Delivery.Enabled || !s.Delivery.Configured {
		incident.DeliveryState = "not_configured"
		return
	}
	if len(s.Queue) >= 100 {
		incident.DeliveryState, incident.DeliveryError = "failed", "Notification queue is full"
		return
	}
	incident.DeliveryState, incident.DeliveryError = "pending", ""
	s.Queue = append(s.Queue, QueuedDelivery{Notification: Notification{
		EventID: fmt.Sprintf("oonfeewrt-alert-%d-%s", incident.ID, event), Event: event, At: now, Incident: *incident,
	}, NextAttempt: now, ExpiresAt: now + 1800})
}

func finishDelivery(s *PersistentState, q QueuedDelivery, state, detail string) {
	for i := range s.Incidents {
		if s.Incidents[i].ID == q.Notification.Incident.ID && s.Incidents[i].State == q.Notification.Event {
			s.Incidents[i].DeliveryState, s.Incidents[i].DeliveryError = state, detail
		}
	}
}

func prune(s *PersistentState) {
	if len(s.Incidents) <= 500 {
		return
	}
	active := make(map[int64]bool)
	for _, r := range s.Rules {
		if r.IncidentID != 0 {
			active[r.IncidentID] = true
		}
	}
	for _, q := range s.Queue {
		active[q.Notification.Incident.ID] = true
	}
	remove := len(s.Incidents) - 500
	keep := s.Incidents[:0]
	for _, incident := range s.Incidents {
		if remove > 0 && !active[incident.ID] {
			remove--
			continue
		}
		keep = append(keep, incident)
	}
	s.Incidents = keep
}
