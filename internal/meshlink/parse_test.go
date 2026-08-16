package meshlink

import "testing"

// A verbatim capture from an Archer C6 running OpenWrt 25.12.5, 2026-08-16,
// with one real client associated. The MAC is redacted; nothing else is
// touched, including the whitespace, which is the part that matters.
//
// Kept whole rather than trimmed to the interesting lines, because the fields
// this parser must IGNORE are as much a part of the format as the ones it
// reads, and a fixture pared down to the useful lines stops proving that.
const realAPDump = `Station aa:bb:cc:dd:ee:ff (on phy0-ap1)
	authorized:	yes
	authenticated:	yes
	associated:	yes
	preamble:	short
	WMM/WME:	yes
	MFP:		yes
	TDLS peer:	no
	inactive time:	7700 ms
	rx bytes:	38727
	rx packets:	257
	tx bytes:	3205
	tx packets:	28
	tx retries:	0
	tx failed:	0
	rx drop misc:	0
	signal:  	-37 [-37, -47, -77, -77] dBm
	signal avg:	-40 [-35, -45, -77, -77] dBm
	tx bitrate:	650.0 MBit/s VHT-MCS 7 80MHz short GI VHT-NSS 2
	tx duration:	7181 us
	rx bitrate:	650.0 MBit/s VHT-MCS 7 80MHz short GI VHT-NSS 2
	rx duration:	0 us
	airtime weight: 256
	expected throughput:	526.31Mbps
	DTIM period:	2
	beacon interval:100
	short preamble:	yes
	short slot time:yes
	connected time:	119 seconds
	associated at [boottime]:	14130.603s
	associated at:	1786864729622 ms
	current time:	1786864848933 ms`

func TestParsesTheRealCapture(t *testing.T) {
	peers := ParseStationDump(realAPDump)

	if len(peers) != 1 {
		t.Fatalf("want 1 station, got %d", len(peers))
	}
	p := peers[0]
	if p.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("MAC = %q", p.MAC)
	}
	// The headline figure, not one of the per-chain values in brackets.
	if p.SignalDBm == nil || *p.SignalDBm != -37 {
		t.Errorf("signal = %v, want -37 (the first number, before the brackets)",
			p.SignalDBm)
	}
	if p.InactiveMS == nil || *p.InactiveMS != 7700 {
		t.Errorf("inactive = %v, want 7700", p.InactiveMS)
	}
	// An AP client has no peer-link state, and inventing one is the single
	// thing this parser must never do: plink is what the healthy/unhealthy
	// split turns on.
	if p.Plink != "" {
		t.Errorf("invented a plink state for an AP client: %q", p.Plink)
	}
	if p.Established() {
		t.Error("an AP client with no plink was reported as established")
	}
}

// The trap in the real format: some fields have NO whitespace after the colon.
// A parser splitting on ":\t" drops them silently, and would drop `mesh
// plink:ESTAB` on any device that formats it that way.
func TestParsesFieldsWithNoSpaceAfterTheColon(t *testing.T) {
	dump := "Station aa:bb:cc:dd:ee:01 (on phy0-mesh0)\n" +
		"\tmesh plink:ESTAB\n" +
		"\tinactive time:120 ms\n"

	peers := ParseStationDump(dump)

	if len(peers) != 1 {
		t.Fatalf("want 1 peer, got %d", len(peers))
	}
	if peers[0].Plink != "ESTAB" {
		t.Errorf("plink = %q — a field with no space after the colon was dropped",
			peers[0].Plink)
	}
	if peers[0].InactiveMS == nil || *peers[0].InactiveMS != 120 {
		t.Errorf("inactive = %v", peers[0].InactiveMS)
	}
}

// A mesh dump: several peers, mixed link states. This is the shape the readout
// exists to interpret, and it is the one shape this project's hardware cannot
// produce — mesh is Present only on the C6 and there is no second node — so it
// is constructed from the captured field syntax rather than captured whole.
func TestParsesSeveralMeshPeersWithMixedLinkStates(t *testing.T) {
	dump := `Station aa:bb:cc:dd:ee:01 (on phy0-mesh0)
	inactive time:	40 ms
	signal:  	-52 [-55, -58] dBm
	mesh plink:	ESTAB
	mesh llid:	1
Station aa:bb:cc:dd:ee:02 (on phy0-mesh0)
	inactive time:	3200 ms
	signal:  	-81 [-84] dBm
	mesh plink:	OPN_SNT`

	peers := ParseStationDump(dump)

	if len(peers) != 2 {
		t.Fatalf("want 2 peers, got %d", len(peers))
	}
	if peers[0].Plink != "ESTAB" || !peers[0].Established() {
		t.Errorf("first peer: %+v", peers[0])
	}
	if peers[1].Plink != "OPN_SNT" || peers[1].Established() {
		t.Errorf("second peer should not be established: %+v", peers[1])
	}
	if *peers[0].SignalDBm != -52 || *peers[1].SignalDBm != -81 {
		t.Errorf("signals: %v %v", *peers[0].SignalDBm, *peers[1].SignalDBm)
	}

	// And the whole point: one established peer carries the backhaul, the
	// half-open one does not make it unhealthy.
	o := healthy()
	o.Peers = peers
	if l := Evaluate(o, true); l.State != StatePeered || *l.Established != 1 {
		t.Errorf("want peered with 1 established, got %s / %v", l.State, l.Established)
	}
}

// Empty output is the normal case for a mesh with no peers, and it must be an
// empty result rather than anything resembling a failure — the caller decides
// what zero peers means, and it has a state for it.
func TestEmptyDumpIsZeroPeersAndNotAnError(t *testing.T) {
	for _, in := range []string{"", "\n", "   \n\t\n"} {
		if got := ParseStationDump(in); len(got) != 0 {
			t.Errorf("%q gave %d peers", in, len(got))
		}
	}
}

// Garbage must not become a peer with invented fields. A signal of 0 dBm is a
// plausible-looking number and a lie, so an unreadable value stays nil.
func TestUnreadableValuesStayUnknown(t *testing.T) {
	dump := "Station aa:bb:cc:dd:ee:01 (on phy0-mesh0)\n" +
		"\tsignal:  \tunknown\n" +
		"\tinactive time:\t- ms\n" +
		"\tsome future field:\twhatever\n"

	peers := ParseStationDump(dump)

	if len(peers) != 1 {
		t.Fatalf("a station with unreadable fields is still a station: %d", len(peers))
	}
	if peers[0].SignalDBm != nil {
		t.Errorf("invented a signal from unreadable text: %v", *peers[0].SignalDBm)
	}
	if peers[0].InactiveMS != nil {
		t.Errorf("invented an inactive time: %v", *peers[0].InactiveMS)
	}
}

// Lines before any Station header belong to nothing and must not crash or be
// attributed to the first peer that comes along.
func TestFieldsBeforeAnyStationAreIgnored(t *testing.T) {
	dump := "\tmesh plink:\tESTAB\nStation aa:bb:cc:dd:ee:01 (on phy0-mesh0)\n"

	peers := ParseStationDump(dump)

	if len(peers) != 1 {
		t.Fatalf("want 1 peer, got %d", len(peers))
	}
	if peers[0].Plink != "" {
		t.Error("a field with no station was attributed to the next one")
	}
}
