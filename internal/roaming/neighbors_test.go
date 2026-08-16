package roaming

import (
	"testing"
)

// The fixtures are the real thing: these are the elements the two reference
// devices reported for the `oonfee-roam` WLAN, read with `rrm_nr_get_own` on
// 2026-08-15. Using measured bytes rather than invented ones keeps the tests
// honest about the shape the code actually handles — a hand-written "aabbcc"
// would not have caught a length or case assumption.
var (
	wrt5 = Neighbour{DeviceID: 1, Iface: "phy0-ap1", SSID: "oonfee-roam",
		BSSID: "32:23:03:db:be:43", NR: "322303dbbe43ef1900008024090603022a00"}
	wrt2 = Neighbour{DeviceID: 1, Iface: "phy1-ap1", SSID: "oonfee-roam",
		BSSID: "32:23:03:db:be:40", NR: "322303dbbe40ef0900005101070603000100"}
	c6_5 = Neighbour{DeviceID: 2, Iface: "phy0-ap1", SSID: "oonfee-roam",
		BSSID: "86:d8:1b:c5:19:34", NR: "86d81bc51934ef1900008024090603022a00"}
	c6_2 = Neighbour{DeviceID: 2, Iface: "phy1-ap1", SSID: "oonfee-roam",
		BSSID: "86:d8:1b:c5:19:35", NR: "86d81bc51935ef0900005101070603000100"}
	// A different SSID on the same hardware — the reference devices really do
	// carry these alongside the managed WLAN.
	probe5 = Neighbour{DeviceID: 1, Iface: "phy0-ap0", SSID: "oonfeewrt-probe-5g",
		BSSID: "30:23:03:db:be:42", NR: "302303dbbe42ef1900008024090603022a00"}
)

func bssids(ns []Neighbour) map[string]bool {
	out := map[string]bool{}
	for _, n := range ns {
		out[n.BSSID] = true
	}
	return out
}

func TestDistributeGivesEachBSSTheOtherThree(t *testing.T) {
	got := Distribute([]Neighbour{wrt5, wrt2, c6_5, c6_2})

	if len(got) != 4 {
		t.Fatalf("want a list for each of the 4 BSSes, got %d", len(got))
	}
	for _, self := range []Neighbour{wrt5, wrt2, c6_5, c6_2} {
		peers := got[Target{self.DeviceID, self.Iface}]
		if len(peers) != 3 {
			t.Errorf("%s: want 3 neighbours, got %d", self.BSSID, len(peers))
		}
		if bssids(peers)[self.BSSID] {
			t.Errorf("%s lists itself; 802.11k asks an AP about its neighbours, "+
				"and naming itself invites a client to roam where it already is",
				self.BSSID)
		}
	}
}

// The other band of the same device is a neighbour. Excluding it — which the
// obvious "skip anything on this device" implementation would do — leaves a
// client stuck on a congested 2.4 GHz radio with no idea the same AP offers
// 5 GHz.
func TestDistributeIncludesTheOtherBandOfTheSameDevice(t *testing.T) {
	got := Distribute([]Neighbour{wrt5, wrt2})

	peers := got[Target{wrt5.DeviceID, wrt5.Iface}]
	if len(peers) != 1 || peers[0].BSSID != wrt2.BSSID {
		t.Fatalf("want the same device's other band as a neighbour, got %+v", peers)
	}
}

func TestDistributeNeverCrossesSSIDs(t *testing.T) {
	got := Distribute([]Neighbour{wrt5, wrt2, probe5})

	if peers := got[Target{probe5.DeviceID, probe5.Iface}]; len(peers) != 0 {
		t.Errorf("the only BSS on its SSID must get an empty list, got %+v", peers)
	}
	for _, self := range []Neighbour{wrt5, wrt2} {
		if bssids(got[Target{self.DeviceID, self.Iface}])[probe5.BSSID] {
			t.Errorf("%s was given a neighbour on a different SSID; a client "+
				"cannot roam there without joining a different network",
				self.BSSID)
		}
	}
}

// An empty list is an instruction, not an absence. It is how the surviving AP
// of a decommissioned pair gets told the other one is gone; omit the entry and
// the dead BSS stays in a live neighbour list until something restarts hostapd.
func TestDistributeEmitsAnEntryForALoneBSS(t *testing.T) {
	got := Distribute([]Neighbour{wrt5})

	peers, ok := got[Target{wrt5.DeviceID, wrt5.Iface}]
	if !ok {
		t.Fatal("a lone BSS must still get an entry, so its stale neighbours " +
			"can be cleared")
	}
	if len(peers) != 0 {
		t.Errorf("want an empty list, got %+v", peers)
	}
}

func TestDistributeDropsIncompleteObservations(t *testing.T) {
	noNR := Neighbour{DeviceID: 3, Iface: "phy0-ap0", SSID: "oonfee-roam",
		BSSID: "aa:bb:cc:dd:ee:ff"}

	got := Distribute([]Neighbour{wrt5, wrt2, noNR})

	if bssids(got[Target{wrt5.DeviceID, wrt5.Iface}])[noNR.BSSID] {
		t.Error("relayed a neighbour with no NR element: an AP would answer a " +
			"client with a candidate it has no channel for")
	}
	if _, ok := got[Target{noNR.DeviceID, noNR.Iface}]; ok {
		t.Error("an unreadable BSS must not be given a list either; we do not " +
			"know what it is")
	}
}

// Determinism is what makes "has this changed?" answerable without asking the
// device, which is the entire request budget for this feature.
func TestDistributeIsOrderIndependent(t *testing.T) {
	a := Distribute([]Neighbour{wrt5, wrt2, c6_5, c6_2})
	b := Distribute([]Neighbour{c6_2, wrt5, c6_5, wrt2})

	for tgt, want := range a {
		got := b[tgt]
		if len(got) != len(want) {
			t.Fatalf("%s: %d vs %d neighbours", tgt, len(want), len(got))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: entry %d differs: %+v vs %+v", tgt, i, want[i], got[i])
			}
		}
	}
}

// hostapd returns rrm_nr_list in its own storage order — measured on both
// reference devices, it is neither insertion order nor sorted. An
// order-sensitive comparison makes every list look changed on every cycle, so
// the reconciler pushes forever and never converges.
func TestSameSetIgnoresOrder(t *testing.T) {
	if !SameSet([]Neighbour{wrt5, c6_5, c6_2}, []Neighbour{c6_2, c6_5, wrt5}) {
		t.Error("the same three entries in a different order compared unequal; " +
			"the reconciler would re-push on every cycle forever")
	}
}

func TestSameSetNoticesAChangedElement(t *testing.T) {
	moved := c6_5
	// Same AP, same BSSID, different channel — byte 14 of the element.
	moved.NR = "86d81bc51934ef1900008024250603022a00"

	if SameSet([]Neighbour{wrt5, c6_5}, []Neighbour{wrt5, moved}) {
		t.Error("a neighbour that changed channel compared equal; the client " +
			"would keep scanning the channel the AP left")
	}
}

func TestSameSetNoticesLengthAndMembership(t *testing.T) {
	if SameSet([]Neighbour{wrt5, c6_5}, []Neighbour{wrt5}) {
		t.Error("different lengths compared equal")
	}
	if SameSet([]Neighbour{wrt5, c6_5}, []Neighbour{wrt5, c6_2}) {
		t.Error("different members compared equal")
	}
	if !SameSet(nil, []Neighbour{}) {
		t.Error("nil and empty are the same instruction: clear the list")
	}
}

// A MAC address is not case sensitive, and nothing guarantees two sources of
// one address agree on case. Getting this wrong makes an AP list itself, which
// is invisible until a client acts on it.
func TestCaseFoldingOnBSSIDs(t *testing.T) {
	// The same BSS, reported once in upper case — as if one source of the
	// address disagreed with another about presentation.
	upper := wrt5
	upper.BSSID = "32:23:03:DB:BE:43"

	got := Distribute([]Neighbour{upper, wrt2})

	peers := got[Target{upper.DeviceID, upper.Iface}]
	for _, p := range peers {
		if p.BSSID == wrt5.BSSID || p.BSSID == upper.BSSID {
			t.Errorf("the BSS was given itself under a different case: %+v", p)
		}
	}
	if !SameSet([]Neighbour{upper}, []Neighbour{wrt5}) {
		t.Error("the same BSSID in a different case compared unequal, so the " +
			"reconciler would re-push a list that is already correct")
	}
}
