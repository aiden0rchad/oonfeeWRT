package collector

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

func TestLogReadCallsUseBoundedNonStreamingSlowCadence(t *testing.T) {
	now := time.Unix(100, 0)
	c := New(newRecorder(), Options{Now: func() time.Time { return now }})
	p := &poller{c: c, target: Target{DeviceID: 1}}
	assert := func(calls []call, want int) {
		t.Helper()
		got := 0
		for _, call := range calls {
			if call.inv.Object != "log" || call.inv.Method != "read" {
				continue
			}
			got++
			args, ok := call.inv.Args.(map[string]any)
			if !ok || args["lines"] != 512 || args["stream"] != false {
				t.Fatalf("log.read args=%#v", call.inv.Args)
			}
		}
		if got != want {
			t.Fatalf("log.read calls=%d, want %d", got, want)
		}
	}
	assert(p.buildCalls(Baseline, nil, nil), 1)
	assert(p.buildCalls(Focused, nil, nil), 0)
	now = now.Add(logReadInterval)
	assert(p.buildCalls(Focused, nil, nil), 1)
}

func TestLogReadDecodersRequireRowsBootAndOneRunningPID(t *testing.T) {
	var snap Snapshot
	if err := decodeLogBootID(json.RawMessage(`{"data":"01234567-89ab-cdef-0123-456789abcdef\n"}`), &snap); err != nil {
		t.Fatal(err)
	}
	if err := decodeLogService(json.RawMessage(`{"log":{"instances":{"logd":{"running":true,"pid":321}}}}`), &snap); err != nil {
		t.Fatal(err)
	}
	if err := decodeLogRows(json.RawMessage(`{"log":[{"msg":"ready","id":4,"priority":30,"source":2,"time":1787160000123}]}`), &snap); err != nil {
		t.Fatal(err)
	}
	snap.LogsFresh = snap.logReadOK && snap.logBootOK && snap.logPIDOK
	if !snap.LogsFresh || snap.LogEpoch.PID != 321 || len(snap.Logs) != 1 {
		t.Fatalf("snapshot=%+v", snap)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"data":"not-a-boot-id"}`),
		json.RawMessage(`{"log":{"instances":{}}}`),
	} {
		var bad Snapshot
		var err error
		if string(raw) == `{"data":"not-a-boot-id"}` {
			err = decodeLogBootID(raw, &bad)
		} else {
			err = decodeLogService(raw, &bad)
		}
		if err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestDecodeAssoclistPreservesQualityFieldAvailability(t *testing.T) {
	var snap Snapshot
	raw := json.RawMessage(`{"results":[
		{"mac":"aa:bb:cc:11:22:33","signal":0,"rx":{"bytes":0,"rate":0},"tx":{"bytes":0,"rate":0,"packets":0,"retries":0,"failed":0}},
		{"mac":"aa:bb:cc:11:22:44","tx":{"packets":3,"retries":0}}
	]}`)
	if err := decodeAssoclist("phy0-ap1")(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Stations) != 2 {
		t.Fatalf("stations=%d", len(snap.Stations))
	}
	complete, partial := snap.Stations[0], snap.Stations[1]
	if !complete.PresenceKnown || !complete.SignalKnown || !complete.TXQualityKnown ||
		!complete.RX.BytesKnown || !complete.RX.RateKnown ||
		!complete.TX.BytesKnown || !complete.TX.RateKnown {
		t.Fatalf("present zero fields lost: %+v", complete)
	}
	if !partial.PresenceKnown || partial.SignalKnown || partial.TXQualityKnown ||
		partial.RX.BytesKnown || partial.RX.RateKnown ||
		partial.TX.BytesKnown || partial.TX.RateKnown {
		t.Fatalf("missing fields became observed zeroes: %+v", partial)
	}
}

func TestDecodeNetDevicesPreservesPerDirectionCounterPresence(t *testing.T) {
	var snap Snapshot
	raw := json.RawMessage(`{"eth0":{"up":true,"statistics":{"rx_bytes":0}},"eth1":{"statistics":{"tx_bytes":7}}}`)
	if err := decodeNetDevices(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.NetDevsFresh || !snap.Interfaces["eth0"].Stats.RxBytesKnown ||
		snap.Interfaces["eth0"].Stats.TxBytesKnown ||
		snap.Interfaces["eth1"].Stats.RxBytesKnown ||
		!snap.Interfaces["eth1"].Stats.TxBytesKnown {
		t.Fatalf("counter presence was flattened: %+v", snap.Interfaces)
	}
}

func TestDecodeSurveyRequiresFrequencyAndCounterPresence(t *testing.T) {
	var snap Snapshot
	if err := decodeSurvey("phy0-ap0")(json.RawMessage(
		`{"results":[{"mhz":5180,"active_time":0,"busy_time":0}]}`), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Surveys) != 1 || !snap.Surveys[0].MHzKnown ||
		!snap.Surveys[0].ActiveTimeKnown || !snap.Surveys[0].BusyTimeKnown {
		t.Fatalf("survey presence=%+v", snap.Surveys)
	}
	before := len(snap.Surveys)
	if err := decodeSurvey("phy0-ap0")(json.RawMessage(
		`{"results":[{"mhz":5180,"busy_time":9}]}`), &snap); err == nil {
		t.Fatal("survey without active_time was accepted")
	}
	if len(snap.Surveys) != before {
		t.Fatal("a rejected partial survey mutated the snapshot")
	}
}

func TestDegradationCauseKeepsFailureDomainsDistinct(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status ubus.Status
		want   DegradationCause
	}{
		{"permission status", errors.New("denied"), ubus.StatusPermissionDenied, CausePermission},
		{"unsupported status", errors.New("no method"), ubus.StatusNotSupported, CauseUnsupported},
		{"timeout status", errors.New("late"), ubus.StatusTimeout, CauseTransport},
		{"device error status", errors.New("remote failure"), ubus.StatusUnknownError, CauseDevice},
		{"invalid request", errors.New("bad args"), ubus.StatusInvalidArgument, CauseProtocol},
		{"ACL denial", &ubus.DeniedError{Retried: true}, ubus.StatusOK, CausePermission},
		{"protocol response", &ubus.ProtocolError{Code: 502, Message: "bad gateway"}, ubus.StatusOK, CauseProtocol},
		{"unclassified", errors.New("busy"), ubus.StatusOK, CauseUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := degradationCause(tc.err, tc.status); got != tc.want {
				t.Fatalf("cause = %q, want %q", got, tc.want)
			}
		})
	}
}

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

func TestLiveStationsPreservesOneMACOnCompetingBSSes(t *testing.T) {
	signalA, signalB := -46, -71
	s := Snapshot{APsFresh: true, APs: []AP{
		{Iface: "phy1-ap0", Stations: map[string]LiveStation{
			"AA:BB:CC:DD:EE:FF": {Iface: "phy1-ap0", Signal: &signalB},
		}},
		{Iface: "phy0-ap0", Stations: map[string]LiveStation{
			"aa:bb:cc:dd:ee:ff": {Iface: "phy0-ap0", Signal: &signalA},
		}},
	}}
	stations, known := s.LiveStations()
	if !known {
		t.Fatal("complete AP reads reported unknown")
	}
	got := stations["aa:bb:cc:dd:ee:ff"]
	if len(got) != 2 || got[0].Iface != "phy0-ap0" || got[1].Iface != "phy1-ap0" {
		t.Fatalf("competing BSS sightings were collapsed or unstable: %+v", got)
	}
}

func keysOf(m map[string]LiveStation) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
