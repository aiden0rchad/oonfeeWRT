package collector

import "testing"

func apWith(iface string, clients *int) AP {
	return AP{Iface: iface, Clients: clients}
}

func intp(n int) *int { return &n }

// A device with no AP interfaces has zero wireless clients, and that is a fact
// rather than an absence of one.
//
// ClientCount gated on `len(s.APs) > 0`, so a device with its radios off — or a
// switch, or an AP whose WLAN has not been applied yet — was reported as
// unknown. That suppressed the dashboard's whole fleet total and named the
// device as one that "did not report a client count", which it had: it reported
// that it has none. Seen for real on an adopted device between un-adopt and the
// next apply.
func TestNoAPInterfacesIsAKnownZero(t *testing.T) {
	s := Snapshot{APsFresh: true}
	n, ok := s.ClientCount()
	if !ok {
		t.Error("a device that answered and has no AP interfaces was reported as unknown")
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// And the other direction, which is the dangerous one. A device where only SOME
// hostapd get_status calls answered has entries for just those radios. Summing
// them and calling the total known draws exactly the dip the dashboard says it
// refuses to draw — "adding up the rest would show a dip that looks like
// clients leaving" — reached inside the function that message trusts.
func TestAPartialRadioAnswerIsNotATrustworthyTotal(t *testing.T) {
	s := Snapshot{
		APsFresh: false, // one radio never answered, so the set is incomplete
		APs:      []AP{apWith("phy0-ap0", intp(3))},
	}
	if _, ok := s.ClientCount(); ok {
		t.Error("a partial radio answer was reported as a trustworthy total")
	}
}

// get_status answered and get_clients did not: the radio exists and its
// population is unknown. Unknown, not zero.
func TestARadioWithNoClientReadingIsUnknown(t *testing.T) {
	s := Snapshot{
		APsFresh: true,
		APs:      []AP{apWith("phy0-ap0", intp(2)), apWith("phy1-ap0", nil)},
	}
	if _, ok := s.ClientCount(); ok {
		t.Error("a radio whose client count could not be read was counted as zero")
	}
}

// The ordinary case still adds up.
func TestEveryRadioAnsweringGivesTheTotal(t *testing.T) {
	s := Snapshot{
		APsFresh: true,
		APs:      []AP{apWith("phy0-ap0", intp(2)), apWith("phy1-ap0", intp(5))},
	}
	n, ok := s.ClientCount()
	if !ok || n != 7 {
		t.Errorf("ClientCount = %d, %v; want 7, true", n, ok)
	}
}
