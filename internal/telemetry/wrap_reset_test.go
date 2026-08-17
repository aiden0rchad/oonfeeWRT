package telemetry

import "testing"

// An unobserved reset must not be reconstructed as a wrap.
//
// A wrap and a reset are the same shape in the numbers — both are a decrease —
// and at the 60 s baseline the physical bound cannot separate them: a gigabit
// link genuinely could carry the 4.29 GB a full wrap implies, so every wrap
// "fits" and none is rejected. Measured on the previous code, a counter falling
// from 100 MB to 100 kB over 60 s emitted 69,917,788 B/s: 559 Mbit/s of traffic
// that never happened.
//
// It is not hypothetical. An apply reloads wifi, which destroys and recreates
// the AP interfaces and zeroes their counters, and `recreated` catches that only
// when a poll happened to see the interface down. Both reference devices sit in
// the tens of megabytes, well below 2^32, so `wide` is false for both.
//
// What separates the two is the interface's own history, which is the one thing
// the arithmetic does not look at.
func TestAnUnobservedResetIsNotReconstructedAsAWrap(t *testing.T) {
	s := testStore()
	k := SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "phy0-ap0"}

	// A quiet access point: 100 B/s, which is what this link does.
	rateOf(t, s, k, 0, 100_000_000)
	if v, ok := rateOf(t, s, k, 60, 100_006_000); !ok || v != 100 {
		t.Fatalf("baseline rate = %v, ok=%v; want 100 B/s", v, ok)
	}

	// The apply lands: the interface is recreated and its counter starts again.
	// Nothing observed it go down, so this arrives as a bare decrease.
	if v, ok := rateOf(t, s, k, 120, 100_000); ok {
		t.Errorf("a reset was reconstructed as a wrap: %v B/s (%.0f Mbit/s) on a "+
			"link that had never exceeded 100 B/s", v, v*8/1e6)
	}
	// And it rebased, so the interface measures again from its new zero.
	if v, ok := rateOf(t, s, k, 180, 106_000); !ok || v != 100 {
		t.Errorf("after the reset: %v, ok=%v; want 100 B/s", v, ok)
	}
}

// The guard must not cost a genuine wrap on a busy link, which is the whole
// reason the wrap branch exists. A real wrap starts from a counter near 2^32 and
// produces a SMALL delta, so its implied rate sits in the interface's normal
// range and nothing fires.
func TestARealWrapOnABusyLinkIsStillRecovered(t *testing.T) {
	s := testStore()
	k := SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "eth0"}

	const nearMax = uint64(1<<32) - 12_000_000
	// History at 200 kB/s, so the link is known to be busy.
	rateOf(t, s, k, 0, nearMax-12_000_000)
	if v, ok := rateOf(t, s, k, 60, nearMax); !ok || v != 200_000 {
		t.Fatalf("baseline rate = %v, ok=%v; want 200000 B/s", v, ok)
	}
	// It wraps: 12 MB to reach 2^32, plus 100 kB past it, over 60 s.
	v, ok := rateOf(t, s, k, 120, 100_000)
	if !ok {
		t.Fatal("a genuine wrap on a busy link was discarded")
	}
	if v < 190_000 || v > 210_000 {
		t.Errorf("rate = %v B/s, want ~201667", v)
	}
}

// The interface up/down state must age with its device, not vanish every flush.
//
// It is the recreation detector: `recreated` is what turns an interface being
// rebuilt into a rebase instead of a reconstructed delta, which is the failure
// rate() was corrected for. expireStale carried a comment saying the iface_up
// pseudo-key "carries no timestamp", which was never true — ifaceCameBack sets
// one on every observation. Anyone making the code match that description would
// have had every interface's state deleted on every flush and the detector
// silently disabled.
func TestInterfaceUpStateAgesWithItsDeviceNotEveryFlush(t *testing.T) {
	s := New(Options{})
	k := SeriesKey{DeviceID: 1, Kind: "iface_up", Key: "phy0-ap0"}

	s.ifaceCameBack(1, 1000, "phy0-ap0", true)
	if st := s.counters[k]; st == nil || st.lastTS != 1000 {
		t.Fatalf("no timestamp recorded for the up/down state: %+v", st)
	}

	// A live entry survives an expiry whose cutoff is behind it.
	s.expireStale(2000, 500)
	if s.counters[k] == nil {
		t.Error("the recreation detector was dropped while still current")
	}
	// And it still detects the transition it exists for.
	if !s.ifaceCameBack(1, 2000, "phy0-ap0", true) == false {
		// up -> up is not a recreation
		t.Error("an interface that stayed up was reported as recreated")
	}
	s.ifaceCameBack(1, 2060, "phy0-ap0", false)
	if !s.ifaceCameBack(1, 2120, "phy0-ap0", true) {
		t.Error("an interface that went down and came back was not detected")
	}

	// A device that stopped being polled ages out normally.
	s.expireStale(9000, 8000)
	if s.counters[k] != nil {
		t.Error("a stale entry was kept forever")
	}
}
