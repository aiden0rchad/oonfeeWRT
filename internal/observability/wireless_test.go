package observability

import (
	"strings"
	"testing"
)

func TestParseWirelessLog(t *testing.T) {
	tests := []struct {
		message string
		action  WirelessAction
		iface   string
	}{
		{"phy0-ap1: AP-STA-CONNECTED AA:BB:CC:11:22:33", WirelessConnect, "phy0-ap1"},
		{"phy0-ap1: STA aa:bb:cc:11:22:33 IEEE 802.11: disassociated", WirelessDisconnect, "phy0-ap1"},
		{"phy1-ap1: FT authentication completed for STA aa:bb:cc:11:22:33", WirelessFT, "phy1-ap1"},
	}
	for _, tc := range tests {
		got, ok := ParseWirelessLog(tc.message)
		if !ok || got.Action != tc.action || got.Iface != tc.iface || got.MAC != "aa:bb:cc:11:22:33" {
			t.Fatalf("%q => %+v,%v", tc.message, got, ok)
		}
	}
	if _, ok := ParseWirelessLog("hostapd: driver initialized"); ok {
		t.Fatal("unrelated line parsed as a station event")
	}
}

func TestAssociationCorrelatorConnectDisconnectRoam(t *testing.T) {
	c := NewAssociationCorrelator()
	mac := "aa:bb:cc:11:22:33"
	connect, ok := c.Observe(1, 1_000, WirelessLog{Action: WirelessConnect, MAC: mac, Iface: "phy0-ap0"})
	if !ok || connect.Action != WirelessConnect || connect.To == nil {
		t.Fatalf("connect=%+v,%v", connect, ok)
	}
	disconnect, ok := c.Observe(1, 2_000, WirelessLog{Action: WirelessDisconnect, MAC: mac, Iface: "phy0-ap0"})
	if !ok || disconnect.Action != WirelessDisconnect || disconnect.From == nil {
		t.Fatalf("disconnect=%+v,%v", disconnect, ok)
	}
	c.Observe(2, 2_500, WirelessLog{Action: WirelessFT, MAC: mac, Iface: "phy1-ap0"})
	roam, ok := c.Observe(2, 3_000, WirelessLog{Action: WirelessConnect, MAC: mac, Iface: "phy1-ap0"})
	if !ok || roam.Action != WirelessRoam || roam.From == nil || roam.To == nil ||
		roam.From.DeviceID != 1 || roam.To.DeviceID != 2 || !roam.To.FastTransition {
		t.Fatalf("roam=%+v,%v", roam, ok)
	}
}

func TestAssociationCorrelatorDoesNotInventARoamAfterLongGap(t *testing.T) {
	c := NewAssociationCorrelator()
	mac := "aa:bb:cc:11:22:33"
	c.Restore(mac, Association{DeviceID: 1, Iface: "phy0-ap0", AtMS: 1, Connected: true})
	got, ok := c.Observe(2, c.RoamWindowMS+2, WirelessLog{Action: WirelessConnect, MAC: mac, Iface: "phy1-ap0"})
	if !ok || got.Action != WirelessConnect || got.From != nil {
		t.Fatalf("got=%+v,%v", got, ok)
	}
}

func TestAssociationCorrelatorCloneIsIndependent(t *testing.T) {
	original := NewAssociationCorrelator()
	original.Restore("aa:bb:cc:11:22:33", Association{DeviceID: 1, Connected: true})
	clone := original.Clone()
	clone.Restore("aa:bb:cc:11:22:33", Association{DeviceID: 2, Connected: true})
	if original.current["aa:bb:cc:11:22:33"].DeviceID != 1 {
		t.Fatal("clone mutated original")
	}
}

func TestAssociationCorrelatorIgnoresLateRowsWithoutRewindingState(t *testing.T) {
	c := NewAssociationCorrelator()
	mac := "aa:bb:cc:11:22:33"
	if _, ok := c.Observe(2, 110, WirelessLog{Action: WirelessConnect, MAC: mac, Iface: "phy1-ap0"}); !ok {
		t.Fatal("newest connect was not emitted")
	}
	for _, log := range []WirelessLog{
		{Action: WirelessDisconnect, MAC: mac, Iface: "phy0-ap0"},
		{Action: WirelessFT, MAC: mac, Iface: "phy0-ap0"},
		{Action: WirelessConnect, MAC: mac, Iface: "phy0-ap0"},
	} {
		if event, ok := c.Observe(1, 105, log); ok || event.IgnoredReason == "" {
			t.Fatalf("late row emitted %+v", event)
		}
	}
	got := c.current[mac]
	if got.DeviceID != 2 || got.Iface != "phy1-ap0" || got.AtMS != 110 || !got.Connected {
		t.Fatalf("state rewound to %+v", got)
	}
	if _, ok := c.pendingFT[mac]; ok {
		t.Fatal("late FT marker was retained")
	}
}

func TestAssociationCorrelatorDoesNotBreakEqualTimestampTiesByArrival(t *testing.T) {
	c := NewAssociationCorrelator()
	mac := "aa:bb:cc:11:22:33"
	c.Restore(mac, Association{DeviceID: 2, Iface: "phy1-ap0", AtMS: 110_500, Connected: true})
	event, ok := c.Observe(1, 110_500, WirelessLog{Action: WirelessConnect, MAC: mac, Iface: "phy0-ap0"})
	if ok || event.IgnoredReason == "" {
		t.Fatalf("equal-timestamp conflict=%+v,%v", event, ok)
	}
	if got := c.current[mac]; got.DeviceID != 2 || got.Iface != "phy1-ap0" {
		t.Fatalf("tie rewound state to %+v", got)
	}
}

func TestAssociationCorrelatorIgnoresLateOldAPDisconnectAfterRoam(t *testing.T) {
	c := NewAssociationCorrelator()
	mac := "aa:bb:cc:11:22:33"
	c.Restore(mac, Association{DeviceID: 2, Iface: "phy1-ap0", AtMS: 110, Connected: true})
	event, ok := c.Observe(1, 120, WirelessLog{Action: WirelessDisconnect, MAC: mac, Iface: "phy0-ap0"})
	if ok || event.IgnoredReason == "" {
		t.Fatalf("old-AP disconnect=%+v,%v", event, ok)
	}
	if got := c.current[mac]; got.DeviceID != 2 || got.Iface != "phy1-ap0" || !got.Connected {
		t.Fatalf("old-AP disconnect rewound state to %+v", got)
	}
}

func TestSanitizeLogMessage(t *testing.T) {
	got := SanitizeLogMessage(`rpc password=hunter2 token:abc Authorization="Bearer secret"`)
	if got != `rpc password=[redacted] token:[redacted] Authorization=[redacted]` {
		t.Fatalf("got %q", got)
	}
	for _, input := range []string{
		`{"password":"sentinel-one","token":"sentinel-two","authorization":"sentinel-three"}`,
		`hostapd: wpa_passphrase=sentinel-four secret='sentinel-five'`,
		"-----BEGIN OPEN" + "SSH PRIVATE KEY-----",
		"-----BEGIN R" + "SA PRIVATE KEY-----",
	} {
		got := SanitizeLogMessage(input)
		if strings.Contains(got, "sentinel") || strings.Contains(got, "PRIVATE KEY") {
			t.Fatalf("sensitive input survived: %q", got)
		}
	}
	plain := "hostapd: key installation failed"
	if got := SanitizeLogMessage(plain); got != plain {
		t.Fatalf("diagnostic changed to %q", got)
	}
	if got := SanitizeLogMessage("-----BEGIN PRIVATE KEY-----"); got != "[redacted sensitive log line]" {
		t.Fatalf("private key line=%q", got)
	}
}
