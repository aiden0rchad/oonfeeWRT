package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// The gate exists because a probe is a burst against a budget that allows one
// request a minute. Two at once do not merely cost double — they interleave on
// one rpcd, and the second gets answers to the first's questions.
func TestReprobeGateSerialisesPerDevice(t *testing.T) {
	var g reprobeGate
	now := time.Now()

	if !g.enter(1, false, now, time.Minute) {
		t.Fatal("the first probe was refused")
	}
	if g.enter(1, false, now, time.Minute) {
		t.Error("a second probe of the same device was admitted while one was running")
	}
	// A different device is unaffected: the constraint is per-rpcd.
	if !g.enter(2, false, now, time.Minute) {
		t.Error("a probe of a different device was refused")
	}
	g.leave(1)
	if !g.enter(1, false, now.Add(time.Second), time.Minute) {
		t.Error("the device stayed blocked after its probe finished")
	}
}

// Automatic probes are rate limited; operator-initiated ones are not.
//
// A device whose board call returns an unstable string would otherwise trigger
// a probe on every poll. But someone watching a screen and pressing a button
// has a reason, and refusing them because a background probe ran two minutes
// ago makes the button look broken.
func TestReprobeGateRateLimitsOnlyAutomaticProbes(t *testing.T) {
	var g reprobeGate
	start := time.Now()

	if !g.enter(1, true, start, 10*time.Minute) {
		t.Fatal("the first automatic probe was refused")
	}
	g.leave(1)

	if g.enter(1, true, start.Add(time.Minute), 10*time.Minute) {
		t.Error("an automatic probe ran a minute after the last one")
	}
	if !g.enter(1, false, start.Add(time.Minute), 10*time.Minute) {
		t.Error("an operator-initiated probe was refused by the automatic floor")
	}
	g.leave(1)

	if !g.enter(1, true, start.Add(11*time.Minute), 10*time.Minute) {
		t.Error("an automatic probe was still blocked after the floor elapsed")
	}
}

// A device that cannot be reached must not silently keep a record that is known
// to describe a different firmware.
//
// The record is left in place — clearing it on one refused connection would
// make a device unplannable for a transient fault — but the failure is logged
// as an event that names the consequence, because the alternative is a device
// that is quietly misdescribed with nothing anywhere saying so.
func TestReprobeFailureIsRecordedWithItsConsequence(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Adopted, with a credential that cannot open a session: the host does not
	// exist. That is the shape of "upgraded and has not come back yet".
	at := int64(1)
	blob, err := d.Keys.SealCredential("60:38:e0:00:00:09", "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	old := capability.NewRegistry()
	old.Board.Release = "OpenWrt 23.05.0"
	old.Set(capability.FeatDSA, capability.Present)
	caps, _ := json.Marshal(old)

	dev := &store.Device{
		MAC: "60:38:e0:00:00:09", Host: "127.0.0.1:1", Name: "gone",
		Scheme: "http", AdoptedAt: &at, CredEnc: blob,
		CapsJSON: string(caps), FWRelease: "OpenWrt 23.05.0",
	}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Reprobe(ctx, dev.ID); err == nil {
		t.Fatal("probing an unreachable device reported success")
	}

	// The stored record must be untouched: a failed probe learned nothing, and
	// replacing a good record with an empty one would make the device
	// unplannable for a reason that has nothing to do with the device.
	after, err := d.Store.DeviceByID(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CapsJSON != string(caps) {
		t.Errorf("a failed probe modified the capability record:\n%s", after.CapsJSON)
	}
}

// The automatic path must not report "not now" as a device failure.
func TestReprobeBusyIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	at := int64(1)
	blob, err := d.Keys.SealCredential("60:38:e0:00:00:0a", "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	dev := &store.Device{
		MAC: "60:38:e0:00:00:0a", Host: "127.0.0.1:1", Name: "busy",
		Scheme: "http", AdoptedAt: &at, CredEnc: blob,
	}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	// Hold the gate as though a probe were running.
	if !d.reprobes.enter(dev.ID, false, time.Now(), reprobeMinInterval) {
		t.Fatal("could not take the gate")
	}
	defer d.reprobes.leave(dev.ID)

	_, err = d.Reprobe(ctx, dev.ID)
	if !errors.Is(err, errReprobeBusy) {
		t.Fatalf("err = %v, want the busy sentinel — the API turns anything "+
			"else into a 502 that says the device failed", err)
	}
}

// An un-adopted device has no controller credential, so the error must say
// that rather than surfacing a login failure.
func TestReprobeRefusesAnUnadoptedDevice(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	dev := &store.Device{MAC: "60:38:e0:00:00:0b", Host: "192.0.2.1", Name: "pending"}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	_, err = d.Reprobe(ctx, dev.ID)
	if err == nil {
		t.Fatal("probing a pending device reported success")
	}
	if !strings.Contains(err.Error(), "not adopted") {
		t.Errorf("err = %q, want it to name the actual reason", err)
	}
}

// The preview's probable-cause lookup: what qualifies, and what does not.
//
// It answers a question the preview screen could not. An operator reads "this
// WLAN was not rendered: device has no 5 GHz radio" and cannot tell their own
// misconfiguration from a radio that failed on Tuesday — one sentence describes
// both, and they call for opposite responses.
func TestRecentCapabilityLossOffersOnlyActionableChanges(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	dev := &store.Device{MAC: "60:38:e0:00:00:0c", Host: "192.0.2.1", Name: "ap"}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	id := dev.ID

	// A probe that only lost VISIBILITY. Nothing about the device changed, so
	// it must never be offered as the reason a WLAN did not render — that
	// sends someone to widen an ACL that is not the problem.
	if err := d.Store.LogEvent(ctx, store.Event{
		TS: 1000, DeviceID: &id, Category: "device", Severity: "info",
		Event: EventCapabilitiesChanged,
		Detail: map[string]any{
			"actionable": 0,
			"changes": []map[string]any{{
				"effect": string(capability.EffectHidden),
				"detail": "hostapd-control can no longer be checked",
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := d.recentCapabilityLoss(ctx, id); got != nil {
		t.Errorf("a visibility-only change was offered as a cause: %+v", got)
	}

	// A real loss, and a visibility change recorded alongside it. Only the
	// real one is offered.
	if err := d.Store.LogEvent(ctx, store.Event{
		TS: 2000, DeviceID: &id, Category: "device", Severity: "warning",
		Event: EventCapabilitiesChanged,
		Detail: map[string]any{
			"actionable": 1,
			"changes": []map[string]any{
				{"effect": string(capability.EffectLost),
					"detail": "radio phy1 is gone"},
				{"effect": string(capability.EffectHidden),
					"detail": "iwinfo-survey can no longer be checked"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := d.recentCapabilityLoss(ctx, id)
	if got == nil {
		t.Fatal("a real capability loss was not offered as a cause")
	}
	if got.At != 2000 {
		t.Errorf("At = %d, want the newest actionable event (2000)", got.At)
	}
	if len(got.Changes) != 1 || got.Changes[0] != "radio phy1 is gone" {
		t.Errorf("changes = %v; only the actionable one belongs here", got.Changes)
	}
}

// A device with no probe history has no cause, and asking must not error.
func TestRecentCapabilityLossIsNilWithNoHistory(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	dev := &store.Device{MAC: "60:38:e0:00:00:0d", Host: "192.0.2.1", Name: "fresh"}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if got := d.recentCapabilityLoss(ctx, dev.ID); got != nil {
		t.Errorf("a device that has never been re-probed reported %+v", got)
	}
}

// The cause is scoped to its own device. Offering one device's radio failure as
// the reason a different device omitted a WLAN would be worse than silence.
func TestRecentCapabilityLossDoesNotLeakBetweenDevices(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	a := &store.Device{MAC: "60:38:e0:00:00:0e", Host: "192.0.2.1", Name: "a"}
	b := &store.Device{MAC: "60:38:e0:00:00:0f", Host: "192.0.2.2", Name: "b"}
	for _, dev := range []*store.Device{a, b} {
		if err := d.Store.UpsertDevice(ctx, dev); err != nil {
			t.Fatal(err)
		}
	}
	aid := a.ID
	if err := d.Store.LogEvent(ctx, store.Event{
		TS: 3000, DeviceID: &aid, Category: "device", Severity: "warning",
		Event: EventCapabilitiesChanged,
		Detail: map[string]any{
			"actionable": 1,
			"changes": []map[string]any{{
				"effect": string(capability.EffectLost), "detail": "radio phy1 is gone",
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := d.recentCapabilityLoss(ctx, b.ID); got != nil {
		t.Errorf("device b was offered device a's capability loss: %+v", got)
	}
	if got := d.recentCapabilityLoss(ctx, a.ID); got == nil {
		t.Error("device a lost its own cause")
	}
}

// A probable cause is offered only where there is something to explain.
//
// Always attaching the last capability change is the tempting simplification,
// and it is wrong: a device whose plan is clean does not need to be told its
// radio list changed last week. The preview is the screen someone reads
// immediately before writing to their network, so it is the worst place to add
// a standing warning that is usually irrelevant.
func TestOnlyRowsWithSomethingUnexplainedGetACause(t *testing.T) {
	clean := api.DevicePreview{Name: "ap", Changes: []api.Change{
		{Action: "create", Config: "wireless", Section: "oowrt_wlan1_radio0"},
	}}
	if needsExplanation(clean) {
		t.Error("a device that plans cleanly was given a capability explanation")
	}
	if needsExplanation(api.DevicePreview{Name: "ap"}) {
		t.Error("a device with nothing to do was given one")
	}

	for _, tc := range []struct {
		name string
		p    api.DevicePreview
	}{
		{"omitted", api.DevicePreview{Omitted: []string{"guest: device has no 5 GHz radio"}}},
		{"blocked", api.DevicePreview{Blocked: true}},
		{"conflict", api.DevicePreview{Conflicts: []string{"wireless.human_wlan: not ours"}}},
	} {
		if !needsExplanation(tc.p) {
			t.Errorf("%s: a row with something unexplained got no cause lookup", tc.name)
		}
	}
}

// A clean re-probe must supersede an earlier loss.
//
// This scanned back through five events and returned the first ACTIONABLE
// one, skipping anything newer that was not — so a loss stayed pinned to the
// apply preview until five further capability events pushed it out. A
// successful re-probe did not clear it, which makes "re-probe to settle it"
// advice that cannot work: the newest probe says the device is fine and the
// screen keeps quoting an older one.
//
// Observed on the reference WRT3200ACM: a 39-hour-old event claiming "radio
// radio0 is gone" was still being offered as the probable cause of two VLAN
// omissions, about a radio that was up and carrying the SSID.
func TestACleanReprobeClearsAnEarlierLoss(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dev := &store.Device{MAC: "60:38:e0:00:00:0d", Host: "192.0.2.2", Name: "ap2"}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	id := dev.ID

	if err := d.Store.LogEvent(ctx, store.Event{
		TS: 1000, DeviceID: &id, Category: "device", Severity: "warning",
		Event: EventCapabilitiesChanged,
		Detail: map[string]any{
			"actionable": 1,
			"changes": []map[string]any{{
				"effect": string(capability.EffectLost),
				"detail": "radio phy1 is gone",
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := d.recentCapabilityLoss(ctx, id); got == nil {
		t.Fatal("a real loss should be offered while it is the latest word")
	}

	// A later probe that found NOTHING AT ALL. This is the case that matters
	// and the one the first version of this fix missed: an unchanged probe
	// writes no capabilities_changed event, so reading only that stream leaves
	// the stale loss newest for ever. The operator re-probed exactly as advised
	// and the screen did not move.
	if err := d.Store.LogEvent(ctx, store.Event{
		TS: 2000, DeviceID: &id, Category: "device", Severity: "info",
		Event:  EventCapabilitiesProbed,
		Detail: map[string]any{"automatic": false, "unchanged": true},
	}); err != nil {
		t.Fatal(err)
	}
	if got := d.recentCapabilityLoss(ctx, id); got != nil {
		t.Errorf("a superseded loss is still offered as the probable cause of "+
			"what is missing, so re-probing can never clear it: %+v", got)
	}
}

// An unchanged probe must leave a record that it ran.
//
// This is the half the first fix missed, and the operator found it: told to
// re-probe to clear a stale capability panel, they did, and nothing moved.
// logReprobe returned early on res.Unchanged without writing anything, so
// there was no evidence the device had been looked at since — and the panel,
// which reads the newest capabilities_changed event, had nothing newer to
// compare against.
func TestAnUnchangedProbeStillRecordsThatItRan(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dev := &store.Device{MAC: "60:38:e0:00:00:0e", Host: "192.0.2.3", Name: "ap3"}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	d.logReprobe(ctx, dev, &api.ReprobeResult{Unchanged: true}, false)

	got, err := d.Store.DeviceEvents(ctx, dev.ID, EventCapabilitiesProbed, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("a probe that found nothing left no record that it ran, so " +
			"nothing can ever say the device has been looked at since")
	}

	// And it must not be filed as a CHANGE, which would put an empty change
	// list in front of the operator.
	changed, err := d.Store.DeviceEvents(ctx, dev.ID, EventCapabilitiesChanged, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("an unchanged probe wrote a capabilities_changed event: %+v", changed)
	}
}
