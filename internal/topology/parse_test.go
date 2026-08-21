package topology

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseNeighborsBusyBoxIPv4AndIPv6(t *testing.T) {
	ipv4 := []byte(`192.168.1.37 dev br-lan lladdr AA:BB:CC:DD:EE:01 ref 1 used 7/5/3 probes 2 REACHABLE
192.168.1.38 dev br-lan FAILED
192.168.1.39 dev br-lan lladdr aa:bb:cc:dd:ee:03 STALE router
`)
	got, err := ParseNeighbors(4, ipv4)
	if err != nil {
		t.Fatal(err)
	}
	used, confirmed, updated := int64(7), int64(5), int64(3)
	want := []Neighbor{
		{Family: 4, Address: "192.168.1.37", Interface: "br-lan", MAC: "aa:bb:cc:dd:ee:01", State: "reachable",
			UsedSeconds: &used, ConfirmedSeconds: &confirmed, UpdatedSeconds: &updated},
		{Family: 4, Address: "192.168.1.38", Interface: "br-lan", State: "failed"},
		{Family: 4, Address: "192.168.1.39", Interface: "br-lan", MAC: "aa:bb:cc:dd:ee:03", State: "stale", Flags: []string{"router"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IPv4 rows = %#v, want %#v", got, want)
	}

	got, err = ParseNeighbors(6, []byte("fe80::1 dev br-lan lladdr 02:00:00:00:00:01 DELAY\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Address != "fe80::1" || got[0].Family != 6 {
		t.Fatalf("IPv6 rows = %#v", got)
	}
}

func TestDecodeExecOutputRequiresAnExplicitSuccessfulExit(t *testing.T) {
	got, err := DecodeExecOutput([]byte(`{"code":0,"stdout":"one\ntwo\n","stderr":""}`))
	if err != nil || string(got.Stdout) != "one\ntwo\n" || got.Code != 0 {
		t.Fatalf("output=%+v err=%v", got, err)
	}
	for _, raw := range []string{
		`{"stdout":"silent ambiguity"}`,
		`{"code":1,"stdout":"","stderr":"not supported"}`,
		`[]`,
	} {
		if _, err := DecodeExecOutput([]byte(raw)); err == nil {
			t.Fatalf("failed/malformed exec accepted: %s", raw)
		}
	}
}

func TestParseNeighborsRejectsAmbiguousOrMalformedRows(t *testing.T) {
	tests := []struct {
		name   string
		family int
		row    string
	}{
		{"wrong family", 4, "fe80::1 dev br-lan FAILED"},
		{"missing device", 4, "192.0.2.1 lladdr aa:bb:cc:dd:ee:ff STALE"},
		{"missing state", 4, "192.0.2.1 dev br-lan lladdr aa:bb:cc:dd:ee:ff"},
		{"bad mac", 4, "192.0.2.1 dev br-lan lladdr not-a-mac STALE"},
		{"unknown token", 4, "192.0.2.1 dev br-lan lladdr aa:bb:cc:dd:ee:ff MAYBE"},
		{"duplicate state", 4, "192.0.2.1 dev br-lan STALE REACHABLE"},
		{"bad used tuple", 4, "192.0.2.1 dev br-lan used 1/2 STALE"},
		{"negative used age", 4, "192.0.2.1 dev br-lan used 1/-2/3 STALE"},
		{"duplicate used", 4, "192.0.2.1 dev br-lan used 1/2/3 used 4/5/6 STALE"},
		{"bad ref", 4, "192.0.2.1 dev br-lan ref many STALE"},
		{"bad probes", 4, "192.0.2.1 dev br-lan probes -1 STALE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseNeighbors(tc.family, []byte(tc.row)); err == nil {
				t.Fatal("malformed row accepted")
			}
		})
	}
}

func TestParseShowMACsBusyBoxCapture(t *testing.T) {
	out := []byte(`port no mac addr                is local?       ageing timer
  1     aa:bb:cc:dd:ee:01       no                 12.34
  2     AA:BB:CC:DD:EE:02       yes                 0.00
`)
	got, err := ParseShowMACs(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []FDBEntry{
		{Port: 1, MAC: "aa:bb:cc:dd:ee:01", AgeSeconds: 12.34},
		{Port: 2, MAC: "aa:bb:cc:dd:ee:02", Local: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %#v, want %#v", got, want)
	}
}

func TestParseShowMACsRequiresRealHeaderAndFields(t *testing.T) {
	for _, out := range []string{
		"",
		"port mac local age\n1 aa:bb:cc:dd:ee:ff no 0.00\n",
		"port no mac addr is local? ageing timer\n1 aa:bb:cc:dd:ee:ff perhaps 0.00\n",
		"port no mac addr is local? ageing timer\n1 aa:bb:cc:dd:ee:ff no never\n",
	} {
		if _, err := ParseShowMACs([]byte(out)); err == nil {
			t.Fatalf("invalid showmacs output accepted: %q", out)
		}
	}
}

func TestParseShowSTPMapsPortNumbersAndStates(t *testing.T) {
	out := []byte(`br-lan
 bridge id              7fff.001122334455
 designated root        7fff.001122334455
 root port                 0                    path cost                  0

lan1 (1)
 port id                8001                    state                forwarding
 designated root        7fff.001122334455       path cost                100

wlan0-1 (3)
 port id                8003                    state                  blocking
 designated root        7fff.001122334455       path cost                100
`)
	got, err := ParseShowSTP(out)
	if err != nil {
		t.Fatal(err)
	}
	want := STPState{Bridge: "br-lan", Ports: []STPPort{
		{Name: "lan1", Port: 1, State: "forwarding"},
		{Name: "wlan0-1", Port: 3, State: "blocking"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

func TestParseShowSTPDoesNotInventMissingPortState(t *testing.T) {
	if _, err := ParseShowSTP([]byte("br-lan\nlan1 (1)\n port id 8001\n")); err == nil {
		t.Fatal("port without an observed state was accepted")
	}
	if _, err := ParseShowSTP([]byte("br-lan\nlan1 (1)\n port id 8001 state mysterious\n")); err == nil {
		t.Fatal("unknown STP state was accepted")
	}
}

func TestDecodeNetworkDevicesIsSelectiveAndStable(t *testing.T) {
	raw := []byte(`{
	  "wan":{"devtype":"dsa","parent":"eth0","macaddr":"AA:BB:CC:DD:EE:01","up":true,"secret_future":"discard"},
	  "br-lan":{"devtype":"bridge","ports":["lan2","lan1"]}
	}`)
	got, err := DecodeNetworkDevices(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "br-lan" || got[1].Name != "wan" {
		t.Fatalf("devices not deterministically ordered: %#v", got)
	}
	if !reflect.DeepEqual(got[0].BridgeOf, []string{"lan1", "lan2"}) {
		t.Fatalf("bridge ports = %#v", got[0].BridgeOf)
	}
	if got[1].MAC != "aa:bb:cc:dd:ee:01" || got[1].Up == nil || !*got[1].Up {
		t.Fatalf("wan = %#v", got[1])
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "secret_future") || strings.Contains(string(encoded), "discard") {
		t.Fatalf("unknown payload retained: %s", encoded)
	}
}

func TestDecodeWirelessDevicesNeverRetainsPlaintextKey(t *testing.T) {
	const sentinel = "placeholder-wireless-key"
	raw := []byte(`{
	  "radio1":{"up":false,"config":{"band":"2g","channel":"auto"},"interfaces":[]},
	  "radio0":{"up":true,"config":{"band":"5g","channel":36,"key":"radio-secret"},"interfaces":[
	    {"ifname":"phy0-ap0","section":"default_radio0","iwinfo":{"bssid":"AA:BB:CC:DD:EE:99"},"config":{"mode":"ap","network":["guest","lan"],"ssid":"Home","key":"` + sentinel + `"}}
	  ]}
	}`)
	got, err := DecodeWirelessDevices(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "radio0" || got[0].Channel != "36" || got[1].Key != "radio1" {
		t.Fatalf("radios = %#v", got)
	}
	iface := got[0].Interfaces[0]
	if iface.IfName != "phy0-ap0" || iface.Mode != "ap" || iface.BSSID != "aa:bb:cc:dd:ee:99" ||
		!reflect.DeepEqual(iface.Networks, []string{"guest", "lan"}) {
		t.Fatalf("interface = %#v", iface)
	}
	encoded, _ := json.Marshal(got)
	for _, forbidden := range []string{sentinel, "radio-secret", "\"ssid\"", "Home"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("sensitive/unneeded wireless payload %q retained: %s", forbidden, encoded)
		}
	}
}

func TestDecodeWirelessDevicesAcceptsScalarNetworkAndMissingRuntimeIface(t *testing.T) {
	got, err := DecodeWirelessDevices([]byte(`{"radio0":{"interfaces":[
	  {"section":"configured_but_absent","config":{"mode":"mesh","network":"lan"}}
	]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Interfaces) != 1 || got[0].Interfaces[0].IfName != "" ||
		!reflect.DeepEqual(got[0].Interfaces[0].Networks, []string{"lan"}) {
		t.Fatalf("radios = %#v", got)
	}
}

func TestCompositeDecodersRejectWrongShapes(t *testing.T) {
	for _, raw := range []string{"", "null", "[]", `{"bad name":{}}`} {
		if _, err := DecodeNetworkDevices([]byte(raw)); err == nil {
			t.Errorf("getNetworkDevices accepted %q", raw)
		}
	}
	for _, raw := range []string{"", "null", "[]", `{"radio0":{"config":{"channel":36.5}}}`} {
		if _, err := DecodeWirelessDevices([]byte(raw)); err == nil {
			t.Errorf("getWirelessDevices accepted %q", raw)
		}
	}
}
