package collector

import "testing"

// The baseline poll keeps WHICH clients an AP has, not just how many, and
// lower-cases their MACs on the way in.
//
// hostapd.get_clients already carries every MAC and its RSSI on every poll;
// the decoder kept len() and dropped the rest, so the clients grid reported
// "unknown" for connection, access point and signal while two devices were
// associated and hostapd was reporting both.
//
// Case matters because the sources disagree. Measured on the reference
// WRT3200ACM in the same minute: iwinfo.assoclist returns
// "F6:97:77:EB:8E:C9" and hostapd.get_clients returns "f6:97:77:eb:8e:c9"
// for the same station, and the clients table stores lower case. A join that
// does not normalise misses every row and looks like an empty result.
func TestAPClientsKeepsMACsAndLowerCasesThem(t *testing.T) {
	var s Snapshot
	raw := []byte(`{"clients":{"F6:97:77:EB:8E:C9":{"signal":-46},
	                            "04:2e:c1:6d:f4:0d":{}}}`)
	if err := decodeAPClients("phy0-ap0")(raw, &s); err != nil {
		t.Fatal(err)
	}
	ap := s.ap("phy0-ap0")
	if ap.Clients == nil || *ap.Clients != 2 {
		t.Fatalf("client count = %v, want 2", ap.Clients)
	}
	st, ok := ap.Stations["f6:97:77:eb:8e:c9"]
	if !ok {
		t.Fatalf("upper-case MAC was not normalised: %v", keysOf(ap.Stations))
	}
	if st.Signal == nil || *st.Signal != -46 {
		t.Errorf("signal = %v, want -46", st.Signal)
	}
	if st.Iface != "phy0-ap0" {
		t.Errorf("iface = %q", st.Iface)
	}
	// A station hostapd lists without an RSSI is associated and unmeasured.
	quiet, ok := ap.Stations["04:2e:c1:6d:f4:0d"]
	if !ok {
		t.Fatal("a station with no signal field was dropped")
	}
	if quiet.Signal != nil {
		t.Errorf("signal = %v; nothing reported one", *quiet.Signal)
	}
}

func keysOf(m map[string]LiveStation) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
