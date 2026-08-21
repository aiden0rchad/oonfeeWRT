package radio

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeWirelessDevicesUsesStableKeyAndDropsSecrets(t *testing.T) {
	raw := []byte(`{
  "radio1":{"up":true,"config":{"band":"2g","channel":6,"htmode":"HT20"},"interfaces":[
    {"ifname":"phy1-ap1","config":{"mode":"ap","key":"never-retain-me"},"iwinfo":{"frequency":2437,"channel":6}},
    {"ifname":"phy1-ap0","config":{"mode":"ap","key":"also-secret"},"iwinfo":{"frequency":2437,"channel":6}}]},
  "radio0":{"up":false,"disabled":true,"config":{"band":"5g","channel":"auto"},"interfaces":[]}}`)
	got, err := DecodeWirelessDevices(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "radio0" || got[1].Key != "radio1" {
		t.Fatalf("stable inventory = %+v", got)
	}
	if got[1].CurrentMHz == nil || *got[1].CurrentMHz != 2437 ||
		got[1].ConfiguredChannel != "6" || got[1].Interfaces[0].Name != "phy1-ap0" {
		t.Fatalf("radio1 = %+v", got[1])
	}
	encoded, _ := json.Marshal(got)
	for _, secret := range []string{"never-retain-me", "also-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("decoded inventory retained %q: %s", secret, encoded)
		}
	}
}

func TestDecodeWirelessDevicesRefusesAmbiguousCurrentFrequency(t *testing.T) {
	got, err := DecodeWirelessDevices([]byte(`{"radio0":{"interfaces":[
 {"ifname":"wlan0","iwinfo":{"frequency":5180,"channel":36}},
 {"ifname":"wlan0-1","iwinfo":{"frequency":5200,"channel":40}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].CurrentAmbiguous || got[0].CurrentMHz != nil || got[0].CurrentChannel != nil {
		t.Fatalf("ambiguous radio became a current-channel claim: %+v", got[0])
	}
}

func TestFrequencyListDoesNotInventDFSFromRestricted(t *testing.T) {
	got, err := DecodeFrequencyList([]byte(`{"results":[
 {"band":"5g","channel":36,"mhz":5180,"restricted":false,"active":true,"flags":[]},
 {"band":"5g","channel":52,"mhz":5260,"restricted":true,"flags":["NO-IR"]},
 {"channel":149,"mhz":5745}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].Restricted == nil || !*got[1].Restricted || got[2].Restricted != nil {
		t.Fatalf("frequency truth was collapsed: %+v", got)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(strings.ToLower(string(encoded)), "dfs") {
		t.Fatalf("restricted was relabelled as DFS: %s", encoded)
	}
}

func TestFrequencyListAcceptsObservedBandShapesWithoutInterpretingNumericEnums(t *testing.T) {
	got, err := DecodeFrequencyList([]byte(`{"results":[
 {"band":0,"channel":1,"mhz":2412,"restricted":false,"active":true,"flags":[]},
 {"band":"5g","channel":36,"mhz":5180,"restricted":false,"flags":[]},
 {"channel":5,"mhz":5975,"flags":[]},
 {"band":99,"channel":7,"mhz":3000,"flags":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	wantBands := map[int]string{2412: "2g", 3000: "", 5180: "5g", 5975: "6g"}
	if len(got) != len(wantBands) {
		t.Fatalf("frequency rows = %+v", got)
	}
	for _, row := range got {
		if row.Band != wantBands[row.MHz] {
			t.Fatalf("frequency %d band = %q, want %q", row.MHz, row.Band, wantBands[row.MHz])
		}
	}
	if got[0].Restricted == nil || *got[0].Restricted || got[0].Active == nil || !*got[0].Active {
		t.Fatalf("numeric band changed independent row facts: %+v", got[0])
	}
}

func TestFrequencyListRejectsMalformedBandScalarsAndRowBounds(t *testing.T) {
	for _, raw := range []string{
		`{"results":[{"band":true,"channel":1,"mhz":2412}]}`,
		`{"results":[{"band":1.5,"channel":1,"mhz":2412}]}`,
		`{"results":[{"channel":0,"mhz":2412}]}`,
		`{"results":[{"channel":1,"mhz":1999}]}`,
		`{"results":[{"channel":1,"mhz":8001}]}`,
	} {
		if _, err := DecodeFrequencyList([]byte(raw)); err == nil {
			t.Fatalf("malformed freqlist accepted: %s", raw)
		}
	}
	overLimit := `{"results":[` + strings.Repeat(`{"channel":1,"mhz":2412},`, maxFrequencies) +
		`{"channel":1,"mhz":2412}]}`
	if _, err := DecodeFrequencyList([]byte(overLimit)); err == nil {
		t.Fatal("oversized freqlist accepted")
	}
}

func TestDecodeScanBoundsValidatesAndDeduplicates(t *testing.T) {
	got, err := DecodeScan([]byte(`{"results":[
 {"bssid":"AA:BB:CC:DD:EE:FF","ssid":"one","mhz":2412,"channel":1,"signal":-70},
 {"bssid":"aa:bb:cc:dd:ee:ff","ssid":"one","mhz":2412,"channel":1,"signal":-50},
 {"bssid":"00:11:22:33:44:55","ssid":"","mhz":2437,"channel":6}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].BSSID != "aa:bb:cc:dd:ee:ff" || got[0].Signal == nil || *got[0].Signal != -50 {
		t.Fatalf("scan rows = %+v", got)
	}
	if _, err := DecodeScan([]byte(`{"results":[{"bssid":"bad","mhz":2412,"channel":1}]}`)); err == nil {
		t.Fatal("invalid BSSID accepted")
	}
}

func TestScoreChannelsOnlyUsesProvedEnabledChannels(t *testing.T) {
	no, yes := false, true
	minus50 := -50
	freqs := []Frequency{
		{Band: "2g", Channel: 1, MHz: 2412, Restricted: &no},
		{Band: "2g", Channel: 6, MHz: 2437, Restricted: &no},
		{Band: "2g", Channel: 11, MHz: 2462},
		{Band: "2g", Channel: 13, MHz: 2472, Restricted: &yes},
	}
	got := ScoreChannels(freqs, []ScanBSS{{MHz: 2412, Signal: &minus50}})
	if len(got) != 2 || got[0].Channel != 1 || got[1].Channel != 6 || got[1].Score <= got[0].Score {
		t.Fatalf("scores = %+v", got)
	}
}

func TestScoreChannelsUsesObservedSpectralWidthOnFiveGHz(t *testing.T) {
	no := false
	minus50, width80 := -50, 80
	got := ScoreChannels([]Frequency{
		{Band: "5g", Channel: 36, MHz: 5180, Restricted: &no},
		{Band: "5g", Channel: 60, MHz: 5300, Restricted: &no},
	}, []ScanBSS{{MHz: 5210, Signal: &minus50, Width: &width80}})
	if len(got) != 2 || got[0].Score >= got[1].Score {
		t.Fatalf("80 MHz BSS centered at 5210 did not overlap 5180: %+v", got)
	}
}

func TestScoreChannelsUsesConservativeWidthWhenScanOmitsIt(t *testing.T) {
	no := false
	minus50 := -50
	got := ScoreChannels([]Frequency{
		{Band: "5g", Channel: 36, MHz: 5180, Restricted: &no},
		{Band: "5g", Channel: 52, MHz: 5260, Restricted: &no},
	}, []ScanBSS{{MHz: 5200, Signal: &minus50}})
	if len(got) != 2 || got[0].Score >= got[1].Score || got[1].Score != 100 {
		t.Fatalf("unknown-width BSS became zero-width evidence: %+v", got)
	}
}
