package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
