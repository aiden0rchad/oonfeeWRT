package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/radio"
)

func TestRadioFrequencyCallsUseStableKeyDedupeAPPreferenceAndSlowCadence(t *testing.T) {
	now := time.Unix(100, 0)
	c := New(newRecorder(), Options{Now: func() time.Time { return now }})
	p := &poller{c: c, target: Target{DeviceID: 1}, radiosKnown: true,
		radios: []radio.LiveState{
			{InventoryRadio: radio.InventoryRadio{Key: "radio1", Interfaces: []radio.Interface{
				{Name: "phy1-mesh0", Mode: "mesh"},
			}}},
			{InventoryRadio: radio.InventoryRadio{Key: "radio0", Interfaces: []radio.Interface{
				{Name: "phy0-ap2", Mode: "ap"},
				{Name: "phy0-mesh0", Mode: "mesh"},
				{Name: "phy0-ap0", Mode: "ap"},
			}}},
		}}

	assert := func(want map[string]string) {
		t.Helper()
		got := map[string]string{}
		for _, spec := range p.buildCalls(Baseline, nil, nil) {
			if spec.inv.Method == "scan" {
				t.Fatal("a periodic poll scheduled disruptive iwinfo.scan")
			}
			if spec.inv.Object == "iwinfo" && spec.inv.Method == "freqlist" {
				if _, duplicate := got[spec.radioKey]; duplicate {
					t.Fatalf("stable radio %q scheduled more than once", spec.radioKey)
				}
				got[spec.radioKey] = argDevice(spec.inv.Args)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("freqlist targets=%v, want %v", got, want)
		}
		for key, iface := range want {
			if got[key] != iface {
				t.Fatalf("freqlist[%s]=%q, want %q", key, got[key], iface)
			}
		}
	}

	assert(map[string]string{"radio0": "phy0-ap0", "radio1": "phy1-mesh0"})
	assert(map[string]string{})
	now = now.Add(rediscoverInterval - time.Second)
	assert(map[string]string{})
	now = now.Add(time.Second)
	assert(map[string]string{"radio0": "phy0-ap0", "radio1": "phy1-mesh0"})
}

func TestReplacingTargetUpdatesAirtimeCapabilityWithoutRestart(t *testing.T) {
	c := New(newRecorder(), fastOptions())
	c.Add(Target{DeviceID: 7})
	p := c.pollers[7]
	c.Add(Target{DeviceID: 7, AirtimeSplit: true})

	p.mu.Lock()
	proved := p.target.AirtimeSplit
	p.mu.Unlock()
	if c.pollers[7] != p || !proved {
		t.Fatalf("target replacement restarted=%v airtime_split=%v", c.pollers[7] != p, proved)
	}
}

func TestRadioCacheRetainsOfflineLastKnownValuesAndMarksThemStale(t *testing.T) {
	now := time.Unix(100, 0)
	c := New(newRecorder(), Options{Now: func() time.Time { return now }})
	c.Add(Target{DeviceID: 9})
	p := c.pollers[9]
	snap := Snapshot{At: now, radioInventory: []radio.InventoryRadio{{Key: "radio0", Band: "5g"}}}
	p.reconcileRadios(&snap)
	p.succeed(snap)
	status, known := c.RadioStatus(9)
	if !known || status.Stale || !status.LastPollOK || status.ObservedAt != now.UnixMilli() {
		t.Fatalf("fresh status=%+v known=%v", status, known)
	}

	now = now.Add(time.Minute)
	p.fail(context.Background(), Snapshot{DeviceID: 9, At: now, Err: errors.New("offline")})
	status, known = c.RadioStatus(9)
	states, statesKnown := c.Radios(9)
	if !known || !status.Stale || status.LastPollOK || status.ConsecutiveFailures != 1 ||
		!statesKnown || len(states) != 1 || states[0].Key != "radio0" ||
		states[0].InventoryObservedAt != time.Unix(100, 0).UnixMilli() {
		t.Fatalf("offline status=%+v states=%+v known=%v/%v", status, states, known, statesKnown)
	}
}

func TestDeniedRadioInventoryRefreshIsStaleDespiteHealthyRequiredPoll(t *testing.T) {
	now := time.Unix(100, 0)
	c := New(newRecorder(), Options{Now: func() time.Time { return now }})
	c.Add(Target{DeviceID: 10})
	p := c.pollers[10]
	initial := Snapshot{At: now, radioInventoryAsked: true, radioInventoryOK: true,
		radioInventory: []radio.InventoryRadio{{Key: "radio0", Band: "5g"}}}
	p.reconcileRadios(&initial)
	p.succeed(initial)

	now = now.Add(time.Minute)
	denied := Snapshot{At: now, radioInventoryAsked: true}
	p.reconcileRadios(&denied)
	p.succeed(denied)
	if !denied.RadiosStale {
		t.Fatal("snapshot carrying the failed optional refresh claimed current radio mapping")
	}

	status, known := c.RadioStatus(10)
	states, statesKnown := c.Radios(10)
	if !known || !statesKnown || len(states) != 1 || states[0].Key != "radio0" {
		t.Fatalf("last-known inventory was not retained: status=%+v states=%+v", status, states)
	}
	if !status.LastPollOK || status.ConsecutiveFailures != 0 || !status.Stale ||
		status.LastSourceAttemptOK || status.LastSourceAttemptAt != now.UnixMilli() ||
		status.ObservedAt != time.Unix(100, 0).UnixMilli() {
		t.Fatalf("optional source failure collapsed into required poll status: %+v", status)
	}
}

func TestSnapshotMarksOverdueRadioInventoryStale(t *testing.T) {
	now := time.Unix(100, 0)
	c := New(newRecorder(), Options{Now: func() time.Time { return now }})
	p := &poller{c: c}
	fresh := Snapshot{At: now, radioInventory: []radio.InventoryRadio{{Key: "radio0"}}}
	p.reconcileRadios(&fresh)
	if fresh.RadiosStale {
		t.Fatal("fresh inventory marked stale")
	}
	now = now.Add(rediscoverInterval)
	overdue := Snapshot{At: now}
	p.reconcileRadios(&overdue)
	if !overdue.RadiosStale || len(overdue.Radios) != 1 {
		t.Fatalf("overdue snapshot stale=%v radios=%+v", overdue.RadiosStale, overdue.Radios)
	}
}

func TestRadioCachePreservesSuccessBetweenCadencesAndFailureBecomesUnknown(t *testing.T) {
	c := New(newRecorder(), fastOptions())
	c.Add(Target{DeviceID: 7})
	p := c.pollers[7]

	first := Snapshot{radioInventory: []radio.InventoryRadio{{
		Key: "radio0", Band: "5g", Interfaces: []radio.Interface{{Name: "phy0-ap0", Mode: "ap"}},
	}}}
	p.reconcileRadios(&first)
	states, known := c.Radios(7)
	if !known || len(states) != 1 || states[0].Key != "radio0" || states[0].FrequenciesKnown {
		t.Fatalf("initial cache=%+v known=%v; want stable radio with unknown channels", states, known)
	}

	unrestricted := false
	success := Snapshot{
		radioFrequencyAsked: map[string]bool{"radio0": true},
		radioFrequencies: map[string][]radio.Frequency{"radio0": {{
			Band: "5g", Channel: 36, MHz: 5180, Restricted: &unrestricted,
		}}},
	}
	p.reconcileRadios(&success)
	states, _ = c.Radios(7)
	if !states[0].FrequenciesKnown || len(states[0].Frequencies) != 1 {
		t.Fatalf("successful frequency cache=%+v", states[0])
	}

	// A normal poll with no slow radio work carries the last proved answer.
	var ordinary Snapshot
	p.reconcileRadios(&ordinary)
	states, _ = c.Radios(7)
	if !states[0].FrequenciesKnown || len(states[0].Frequencies) != 1 {
		t.Fatalf("ordinary poll discarded cached frequencies: %+v", states[0])
	}

	// API callers get a deep copy, not aliases into the poller's cache.
	*states[0].Frequencies[0].Restricted = true
	states[0].Interfaces[0].Name = "mutated"
	states, _ = c.Radios(7)
	if *states[0].Frequencies[0].Restricted || states[0].Interfaces[0].Name != "phy0-ap0" {
		t.Fatalf("caller mutated radio cache: %+v", states[0])
	}

	// The next slow attempt was unavailable: absence is unknown, not an empty
	// list and not a stale enabled-channel verdict.
	failed := Snapshot{radioFrequencyAsked: map[string]bool{"radio0": true}}
	p.reconcileRadios(&failed)
	states, _ = c.Radios(7)
	if states[0].FrequenciesKnown || len(states[0].Frequencies) != 0 {
		t.Fatalf("unavailable freqlist did not become unknown: %+v", states[0])
	}

	empty := Snapshot{radioInventory: []radio.InventoryRadio{}}
	p.reconcileRadios(&empty)
	if states, known = c.Radios(7); !known || len(states) != 0 {
		t.Fatalf("proved empty inventory=%+v known=%v", states, known)
	}
}
