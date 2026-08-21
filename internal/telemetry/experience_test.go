package telemetry

import (
	"context"
	"math"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
)

func TestWiFiExperienceV1FixedArity(t *testing.T) {
	zero, hundred := 0.0, 100.0
	tests := []struct {
		name        string
		rssi        int
		retry, fail *float64
		want        float64
		ok          bool
	}{
		{"perfect", -50, &zero, &zero, 100, true},
		{"worst", -90, &hundred, &hundred, 0, true},
		{"weighted", -70, &hundred, &zero, 42.5, true},
		{"missing retry", -50, nil, &zero, 0, false},
		{"missing failure", -50, &zero, nil, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := WiFiExperienceV1(tc.rssi, tc.retry, tc.fail)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("score=(%v,%v), want (%v,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWiFiExperienceV1RejectsNonFiniteInputs(t *testing.T) {
	zero, nan, inf := 0.0, math.NaN(), math.Inf(1)
	for _, bad := range []*float64{&nan, &inf} {
		if got, ok := WiFiExperienceV1(-50, bad, &zero); ok || got != 0 {
			t.Fatalf("non-finite retry accepted: (%v,%v)", got, ok)
		}
		if got, ok := WiFiExperienceV1(-50, &zero, bad); ok || got != 0 {
			t.Fatalf("non-finite failure accepted: (%v,%v)", got, ok)
		}
	}
}

func TestWiFiExperienceV1ClampsObservedPercentages(t *testing.T) {
	low, high := -10.0, 150.0
	if got, ok := WiFiExperienceV1(-50, &low, &high); !ok || got != 80 {
		t.Fatalf("score=(%v,%v), want (80,true)", got, ok)
	}
}

func TestStationQualityDelta(t *testing.T) {
	prev := stationQualityCounters{Iface: "phy0-ap1", ConnectedTime: 10,
		Packets: 100, Retries: 10, Failed: 2}
	cur := stationQualityCounters{Iface: "phy0-ap1", ConnectedTime: 15,
		Packets: 180, Retries: 30, Failed: 4}
	retry, failed, ok := stationQualityDelta(prev, cur)
	if !ok || math.Abs(retry-20) > 1e-9 || math.Abs(failed-2.4390243902439024) > 1e-9 {
		t.Fatalf("delta=(%v,%v,%v)", retry, failed, ok)
	}
}

func TestStationQualityDeltaRebases(t *testing.T) {
	base := stationQualityCounters{Iface: "phy0-ap1", ConnectedTime: 10,
		Packets: 100, Retries: 10, Failed: 2}
	tests := []stationQualityCounters{
		{Iface: "phy1-ap1", ConnectedTime: 11, Packets: 110, Retries: 11, Failed: 2},
		{Iface: "phy0-ap1", ConnectedTime: 1, Packets: 110, Retries: 11, Failed: 2},
		{Iface: "phy0-ap1", ConnectedTime: 11, Packets: 90, Retries: 11, Failed: 2},
		{Iface: "phy0-ap1", ConnectedTime: 11, Packets: 100, Retries: 12, Failed: 2},
	}
	for _, cur := range tests {
		if retry, failed, ok := stationQualityDelta(base, cur); ok || retry != 0 || failed != 0 {
			t.Fatalf("reset accepted: cur=%+v delta=(%v,%v,%v)", cur, retry, failed, ok)
		}
	}
}

func TestObserveEmitsDeltaQualityAndFixedExperience(t *testing.T) {
	s := testStore()
	first := snapshot(1, 10, 100)
	first.Stations = []collector.Station{{
		Iface: "phy0-ap1", MAC: "AA:BB:CC:11:22:33", Signal: -70, ConnectedTime: 10,
		TX: collector.Rate{Packets: 100, Retries: 10, Failed: 2},
	}}
	second := snapshot(1, 20, 110)
	second.Stations = []collector.Station{{
		Iface: "phy0-ap1", MAC: "aa:bb:cc:11:22:33", Signal: -70, ConnectedTime: 20,
		TX: collector.Rate{Packets: 180, Retries: 30, Failed: 4},
	}}
	s.Observe(context.Background(), first)
	s.Observe(context.Background(), second)

	rows := s.Flush(at(120))
	retry, ok := findKind(rows, KindStaRetryDelta)
	if !ok || retry.Cnt != 1 || math.Abs(retry.Avg-20) > 1e-9 {
		t.Fatalf("retry rollup=%+v, ok=%v", retry, ok)
	}
	fail, ok := findKind(rows, KindStaTXFailDelta)
	if !ok || fail.Cnt != 1 || math.Abs(fail.Avg-2.4390243902439024) > 1e-6 {
		t.Fatalf("failure rollup=%+v, ok=%v", fail, ok)
	}
	want, _ := WiFiExperienceV1(-70, &retry.Avg, &fail.Avg)
	experience, ok := findKind(rows, KindStaExperienceWiFiV1)
	if !ok || experience.Cnt != 1 || math.Abs(experience.Avg-want) > 1e-5 {
		t.Fatalf("experience rollup=%+v, want=%v, ok=%v", experience, want, ok)
	}
	if legacy, ok := findKind(rows, KindStaRetry); ok {
		t.Fatalf("legacy cumulative retry series still emitted: %+v", legacy)
	}
}

func TestObserveZeroPacketIntervalIsUnavailableAndRebases(t *testing.T) {
	s := testStore()
	for _, sample := range []struct {
		ts, packets, retries, failed int64
	}{
		{10, 100, 10, 2},
		{20, 100, 10, 2}, // no TX denominator: unavailable, not zero.
		{30, 110, 12, 3},
	} {
		snap := snapshot(1, sample.ts, 100+sample.ts)
		snap.Stations = []collector.Station{{
			Iface: "phy0-ap1", MAC: "aa:bb:cc:11:22:33", Signal: -60,
			ConnectedTime: sample.ts, TX: collector.Rate{
				Packets: sample.packets, Retries: sample.retries, Failed: sample.failed,
			},
		}}
		s.Observe(context.Background(), snap)
	}
	retry, ok := findKind(s.Flush(at(120)), KindStaRetryDelta)
	if !ok || retry.Cnt != 1 || math.Abs(retry.Avg-16.666666666666668) > 1e-6 {
		t.Fatalf("retry rollup=%+v, ok=%v", retry, ok)
	}
}

func TestForgetDeviceDropsStationQualityBaseline(t *testing.T) {
	s := testStore()
	s.quality[stationQualityKey{deviceID: 7, mac: "aa:bb:cc:11:22:33"}] = stationQualityState{
		stationQualityCounters: stationQualityCounters{Iface: "phy0-ap1", Packets: 10},
		lastTS:                 10,
	}
	s.ForgetDevice(7)
	if len(s.quality) != 0 {
		t.Fatalf("quality baseline survived: %+v", s.quality)
	}
}
