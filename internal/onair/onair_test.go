package onair

import "testing"

// Stable locally administered BSSIDs preserve the measured failure shape
// without retaining lab identifiers.
var (
	wrt24 = BSS{DeviceID: 5, Name: "wrt", Iface: "phy1-ap0",
		BSSID: "02:00:00:ab:24:41", SSID: "fixture-roam", Band: "2g"}
	wrt5 = BSS{DeviceID: 5, Name: "wrt", Iface: "phy0-ap0",
		BSSID: "02:00:00:ab:24:42", SSID: "fixture-roam", Band: "5g"}
	c624 = BSS{DeviceID: 4, Name: "c6", Iface: "phy1-ap1",
		BSSID: "02:00:00:cd:52:35", SSID: "fixture-roam", Band: "2g"}
)

// The failure this package was built for, in the exact shape it occurred.
func TestTheFourteenHourLie(t *testing.T) {
	scans := []Scan{{
		DeviceID: 4, Name: "c6", BandsCovered: []string{"2g"},
		Heard: []Heard{
			// The BSSID is the WRT's. The SSID is one that existed in no
			// configuration on that device.
			{BSSID: "02:00:00:ab:24:41", SSID: "fixture-mismatch", Band: "2g"},
		},
	}}

	got := Check([]BSS{wrt24}, scans)

	if len(got) != 1 {
		t.Fatalf("want 1 result, got %d", len(got))
	}
	r := got[0]
	if r.Verdict != Mismatched {
		t.Fatalf("verdict = %s, want mismatched — this is the one thing no "+
			"other check in the controller can see", r.Verdict)
	}
	if r.HeardSSID != "fixture-mismatch" {
		t.Errorf("HeardSSID = %q; the report has to name what is actually on "+
			"the air or nobody can act on it", r.HeardSSID)
	}
	if !r.Fault() {
		t.Error("a BSS broadcasting the wrong SSID is a fault")
	}
	if len(r.Witnesses) != 1 || r.Witnesses[0] != 4 {
		t.Errorf("witnesses = %v, want the scanning device", r.Witnesses)
	}
}

func TestHeardWithTheRightSSIDIsConfirmed(t *testing.T) {
	scans := []Scan{{
		DeviceID: 4, BandsCovered: []string{"2g"},
		Heard: []Heard{{BSSID: "02:00:00:AB:24:41", SSID: "fixture-roam", Band: "2g"}},
	}}

	got := Check([]BSS{wrt24}, scans)

	if got[0].Verdict != Confirmed {
		t.Fatalf("verdict = %s (%s)", got[0].Verdict, got[0].Reason)
	}
	if got[0].Fault() {
		t.Error("a confirmed BSS is not a fault")
	}
}

// A MAC is not case sensitive and two sources of one address need not agree on
// case. Getting this wrong turns every confirmation into an "unheard".
func TestBSSIDMatchingIgnoresCase(t *testing.T) {
	upper := wrt24
	upper.BSSID = "02:00:00:AB:24:41"
	scans := []Scan{{DeviceID: 4, BandsCovered: []string{"2g"},
		Heard: []Heard{{BSSID: "02:00:00:ab:24:41", SSID: "fixture-roam"}}}}

	if got := Check([]BSS{upper}, scans); got[0].Verdict != Confirmed {
		t.Errorf("case difference broke the match: %s", got[0].Verdict)
	}
}

// The rule that makes this feature safe to ship. Access points placed for
// coverage routinely cannot hear each other; reporting that as a fault would
// mean every correctly-deployed fleet lights up red.
func TestUnheardIsNotAFault(t *testing.T) {
	scans := []Scan{{
		DeviceID: 4, BandsCovered: []string{"2g"},
		Heard: []Heard{{BSSID: "aa:bb:cc:dd:ee:ff", SSID: "someone-else"}},
	}}

	got := Check([]BSS{wrt24}, scans)

	if got[0].Verdict != Unheard {
		t.Fatalf("verdict = %s, want unheard", got[0].Verdict)
	}
	if got[0].Fault() {
		t.Error("a BSS nobody heard is unverified, not broken — reporting it " +
			"as a fault is how a screen trains people to ignore it")
	}
	if !contains(got[0].Reason, "not the same as") {
		t.Errorf("the reason does not distinguish unheard from off-air: %q",
			got[0].Reason)
	}
}

// Measured on the reference C6: its 2.4 GHz radio returned 20 BSSes while its
// 5 GHz radio, serving an AP, returned zero. A band nobody could scan must
// produce NotChecked, not Unheard — the second implies somebody listened.
func TestABandNobodyScannedIsNotChecked(t *testing.T) {
	scans := []Scan{{
		DeviceID: 4, BandsCovered: []string{"2g"}, // 5 GHz scan returned nothing
		Heard: []Heard{{BSSID: "02:00:00:ab:24:41", SSID: "fixture-roam"}},
	}}

	got := Check([]BSS{wrt24, wrt5}, scans)

	byIface := map[string]Result{}
	for _, r := range got {
		byIface[r.BSS.Iface] = r
	}
	if byIface["phy1-ap0"].Verdict != Confirmed {
		t.Errorf("2.4 GHz should be confirmed, got %s", byIface["phy1-ap0"].Verdict)
	}
	if v := byIface["phy0-ap0"].Verdict; v != NotChecked {
		t.Fatalf("5 GHz verdict = %s, want not-checked: nobody scanned that "+
			"band, so silence is not evidence", v)
	}
	if !contains(byIface["phy0-ap0"].Reason, "cannot scan") {
		t.Errorf("the reason does not explain why the band went unscanned: %q",
			byIface["phy0-ap0"].Reason)
	}
}

// A site with one AP cannot verify itself: a radio does not hear its own
// beacons. That must read as "not checked", never as a confirmation.
func TestASingleAPCannotConfirmItself(t *testing.T) {
	got := Check([]BSS{c624}, nil)

	if got[0].Verdict != NotChecked {
		t.Fatalf("verdict = %s, want not-checked — with no other radio there "+
			"is no witness", got[0].Verdict)
	}
	if got[0].Fault() {
		t.Error("a single-AP site is not a fault")
	}
}

// Every result carries a sentence. A verdict with no reason is a code an
// operator has to look up, and nobody looks it up.
func TestEveryVerdictExplainsItself(t *testing.T) {
	scans := []Scan{{DeviceID: 4, BandsCovered: []string{"2g", "5g"},
		Heard: []Heard{
			{BSSID: "02:00:00:ab:24:41", SSID: "fixture-mismatch"},
			{BSSID: "02:00:00:cd:52:35", SSID: "fixture-roam"},
		}}}

	for _, r := range Check([]BSS{wrt24, wrt5, c624}, scans) {
		if r.Reason == "" {
			t.Errorf("%s on %s has no reason", r.Verdict, r.BSS.Iface)
		}
	}
}

func contains(s, want string) bool {
	for i := 0; i+len(want) <= len(s); i++ {
		if s[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
