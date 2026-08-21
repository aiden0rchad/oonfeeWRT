package collector

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestWANProbeParsesBusyBoxSuccessAndPartialLoss(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		loss    float64
		latency float64
	}{
		{
			name: "busybox",
			raw:  `{"code":0,"stdout":"PING 1.1.1.1 (1.1.1.1): 56 data bytes\n64 bytes from 1.1.1.1: seq=0 ttl=58 time=8.100 ms\n64 bytes from 1.1.1.1: seq=1 ttl=58 time=8.900 ms\n64 bytes from 1.1.1.1: seq=2 ttl=58 time=9.300 ms\n\n--- 1.1.1.1 ping statistics ---\n3 packets transmitted, 3 packets received, 0% packet loss\nround-trip min/avg/max = 8.100/8.767/9.300 ms\n","stderr":""}`,
			loss: 0, latency: 8.767,
		},
		{
			name: "iputils-compatible",
			raw:  `{"code":0,"stdout":"3 packets transmitted, 2 received, 33% packet loss, time 2002ms\nrtt min/avg/max/mdev = 9.000/10.250/11.500/1.250 ms\n","stderr":""}`,
			loss: 100.0 / 3, latency: 10.25,
		},
		{
			name: "busybox duplicate reply",
			raw:  `{"code":0,"stdout":"3 packets transmitted, 3 packets received, 1 duplicates, 0% packet loss\nround-trip min/avg/max = 8.000/9.000/10.000 ms\n","stderr":""}`,
			loss: 0, latency: 9,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWANProbe(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if !got.Up || got.LatencyMS == nil || math.Abs(got.LossPct-tc.loss) > 0.001 ||
				math.Abs(*got.LatencyMS-tc.latency) > 0.001 {
				t.Fatalf("probe = %+v, want up loss=%.3f latency=%.3f", got, tc.loss, tc.latency)
			}
		})
	}
}

func TestWANProbeZeroRepliesIsMeasuredDownNotZeroLatency(t *testing.T) {
	raw := json.RawMessage(`{"code":1,"stdout":"PING 1.1.1.1 (1.1.1.1): 56 data bytes\n\n--- 1.1.1.1 ping statistics ---\n3 packets transmitted, 0 packets received, 100% packet loss\n","stderr":""}`)
	got, err := parseWANProbe(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Up || got.LossPct != 100 || got.LatencyMS != nil {
		t.Fatalf("probe = %+v, want down, 100%% loss, unknown latency", got)
	}
}

func TestWANProbeUnavailableAndMalformedNeverBecomeZero(t *testing.T) {
	cases := map[string]string{
		"missing code":        `{"stdout":"3 packets transmitted, 0 packets received, 100% packet loss\n"}`,
		"command unavailable": `{"code":127,"stdout":"","stderr":"not found"}`,
		"incomplete":          `{"code":1,"stdout":"PING 1.1.1.1\n","stderr":""}`,
		"wrong count":         `{"code":0,"stdout":"1 packets transmitted, 1 packets received, 0% packet loss\nround-trip min/avg/max = 1/1/1 ms\n","stderr":""}`,
		"mismatched loss":     `{"code":0,"stdout":"3 packets transmitted, 2 packets received, 0% packet loss\nround-trip min/avg/max = 1/1/1 ms\n","stderr":""}`,
		"missing latency":     `{"code":0,"stdout":"3 packets transmitted, 3 packets received, 0% packet loss\n","stderr":""}`,
		"bad ordering":        `{"code":0,"stdout":"3 packets transmitted, 3 packets received, 0% packet loss\nround-trip min/avg/max = 3/2/4 ms\n","stderr":""}`,
		"extra field":         `{"code":1,"stdout":"3 packets transmitted, 0 packets received, 100% packet loss\n","stderr":"","secret":"no"}`,
		"duplicate code":      `{"code":0,"code":1,"stdout":"3 packets transmitted, 0 packets received, 100% packet loss\n","stderr":""}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := parseWANProbe(json.RawMessage(raw)); err == nil {
				t.Fatalf("malformed probe decoded as %+v", got)
			}
		})
	}
	tooLarge := `{"code":1,"stdout":"` + strings.Repeat("x", maxWANProbeOutput+1) + `","stderr":""}`
	if _, err := parseWANProbe(json.RawMessage(tooLarge)); err == nil {
		t.Fatal("oversized output was accepted")
	}
}

func TestWANProbeDecodeClearsPriorValueOnFailure(t *testing.T) {
	latency := 7.0
	snap := Snapshot{WAN: &WANProbe{Up: true, LatencyMS: &latency}}
	err := decodeWANProbe(json.RawMessage(`{"code":2,"stdout":"","stderr":"bad option"}`), &snap)
	if err == nil || snap.WAN != nil {
		t.Fatalf("failure left stale probe: err=%v probe=%+v", err, snap.WAN)
	}
}
