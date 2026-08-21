package telemetry

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/radio"
)

func TestRadioUtilizationUsesStableKeyCurrentMHzAndDeduplicatesBSSes(t *testing.T) {
	s := testStore()
	for _, sample := range []struct{ ts, active, busy int64 }{{10, 1000, 8000}, {20, 2000, 8250}} {
		snap := snapshot(1, sample.ts, 100+sample.ts)
		snap.RadiosKnown = true
		snap.IfaceRadios = map[string]string{"phy0-ap0": "radio0", "phy0-ap1": "radio0"}
		snap.APs = []collector.AP{{Iface: "phy0-ap0", Freq: 5180}, {Iface: "phy0-ap1", Freq: 5180}}
		snap.Surveys = []collector.Survey{
			{Iface: "phy0-ap0", MHz: 5200, ActiveTime: sample.active, BusyTime: sample.busy + 999999},
			{Iface: "phy0-ap0", MHz: 5180, ActiveTime: sample.active, BusyTime: sample.busy},
			{Iface: "phy0-ap1", MHz: 5180, ActiveTime: sample.active, BusyTime: sample.busy},
		}
		s.Observe(context.Background(), snap)
	}
	rows := s.Flush(at(120))
	var matches []Rollup
	for _, row := range rows {
		if row.Kind == KindRadioUtilization {
			matches = append(matches, row)
		}
	}
	if len(matches) != 1 || matches[0].Key != "radio0" || matches[0].Cnt != 1 ||
		math.Abs(matches[0].Avg-25) > 1e-9 {
		t.Fatalf("stable radio utilization = %+v", matches)
	}
}

func TestRadioUtilizationUsesSanitizedInventoryMHzWithoutAnAPBSS(t *testing.T) {
	s := testStore()
	current := 2412
	for _, sample := range []struct{ ts, active, busy int64 }{{10, 1000, 2000}, {20, 2000, 2400}} {
		snap := snapshot(1, sample.ts, 100+sample.ts)
		snap.RadiosKnown = true
		snap.IfaceRadios = map[string]string{"phy0-mesh0": "radio0"}
		snap.Radios = []radio.LiveState{{InventoryRadio: radio.InventoryRadio{
			Key: "radio0", CurrentMHz: &current,
		}}}
		snap.Surveys = []collector.Survey{
			{Iface: "phy0-mesh0", MHz: 2437, ActiveTime: sample.active, BusyTime: sample.busy + 10000},
			{Iface: "phy0-mesh0", MHz: 2412, ActiveTime: sample.active, BusyTime: sample.busy},
		}
		s.Observe(context.Background(), snap)
	}
	row, ok := findKind(s.Flush(at(120)), KindRadioUtilization)
	if !ok || row.Key != "radio0" || math.Abs(row.Avg-40) > 1e-9 {
		t.Fatalf("mesh-only current-channel utilization=%+v ok=%v", row, ok)
	}
}

func TestRadioUtilizationDoesNotOverrideAmbiguousInventoryWithOneBSS(t *testing.T) {
	s := testStore()
	for _, ts := range []int64{10, 20} {
		snap := snapshot(1, ts, 100+ts)
		snap.RadiosKnown = true
		snap.IfaceRadios = map[string]string{"phy0-ap0": "radio0"}
		snap.Radios = []radio.LiveState{{InventoryRadio: radio.InventoryRadio{
			Key: "radio0", CurrentAmbiguous: true,
		}}}
		snap.APs = []collector.AP{{Iface: "phy0-ap0", Freq: 5180}}
		snap.Surveys = []collector.Survey{{Iface: "phy0-ap0", MHz: 5180,
			ActiveTime: ts * 100, BusyTime: ts * 40}}
		s.Observe(context.Background(), snap)
	}
	if row, ok := findKind(s.Flush(at(120)), KindRadioUtilization); ok {
		t.Fatalf("ambiguous inventory became a current-frequency metric: %+v", row)
	}
}

func TestRadioUtilizationDoesNotUseStaleInventoryAsCurrentFrequency(t *testing.T) {
	s := testStore()
	current := 2412
	for _, ts := range []int64{10, 20} {
		snap := snapshot(1, ts, 100+ts)
		snap.RadiosKnown = true
		snap.IfaceRadios = map[string]string{"phy0-mesh0": "radio0"}
		snap.RadiosStale = true
		snap.Radios = []radio.LiveState{{InventoryRadio: radio.InventoryRadio{
			Key: "radio0", CurrentMHz: &current,
		}}}
		snap.Surveys = []collector.Survey{{Iface: "phy0-mesh0", MHz: 2412,
			ActiveTime: ts * 100, BusyTime: ts * 40}}
		s.Observe(context.Background(), snap)
	}
	if row, ok := findKind(s.Flush(at(120)), KindRadioUtilization); ok {
		t.Fatalf("stale inventory selected a current survey row: %+v", row)
	}
}

func TestOffChannelSurveyCannotRebaseInterfaceCounter(t *testing.T) {
	s := testStore()
	for _, sample := range []struct {
		ts, mhz, active, busy int64
	}{
		{10, 5180, 1_000, 100},
		{20, 5200, 50, 10}, // off-channel and lower: must not touch the baseline
		{30, 5180, 3_000, 500},
	} {
		snap := snapshot(1, sample.ts, 100+sample.ts)
		snap.APs = []collector.AP{{Iface: "phy0-ap0", Freq: 5180}}
		snap.Surveys = []collector.Survey{{
			Iface: "phy0-ap0", MHz: int(sample.mhz), ActiveTime: sample.active, BusyTime: sample.busy,
			PresenceKnown: true, MHzKnown: true, ActiveTimeKnown: true, BusyTimeKnown: true,
		}}
		s.Observe(context.Background(), snap)
	}
	row, ok := findKind(s.Flush(at(120)), KindChanBusy)
	if !ok || row.Key != "phy0-ap0" || row.Cnt != 1 || math.Abs(row.Avg-20) > 1e-9 {
		t.Fatalf("current-channel utilization=%+v present=%v", row, ok)
	}
}

func TestStableRadioSeriesRequireProvenMapping(t *testing.T) {
	s := testStore()
	for _, ts := range []int64{10, 20} {
		snap := snapshot(1, ts, 100+ts)
		snap.IfaceRadios = map[string]string{"phy0-ap0": "radio0"}
		snap.APs = []collector.AP{{Iface: "phy0-ap0", Freq: 5180}}
		snap.Surveys = []collector.Survey{{Iface: "phy0-ap0", MHz: 5180,
			ActiveTime: ts * 100, BusyTime: ts * 40}}
		s.Observe(context.Background(), snap)
	}
	rows := s.Flush(at(120))
	if _, ok := findKind(rows, KindChanBusy); !ok {
		t.Fatal("current AP frequency did not preserve per-interface utilization")
	}
	if row, ok := findKind(rows, KindRadioUtilization); ok {
		t.Fatalf("unproved stable mapping emitted radio series: %+v", row)
	}
}

func TestStaleRadioMappingSuppressesEveryStableRadioSeries(t *testing.T) {
	s := testStore()
	for _, ts := range []int64{10, 20} {
		snap := snapshot(1, ts, 100+ts)
		snap.RadiosKnown = true
		snap.RadiosStale = true
		snap.AirtimeSplit = true
		snap.IfaceRadios = map[string]string{"phy0-ap0": "radio0"}
		snap.APs = []collector.AP{{Iface: "phy0-ap0", Freq: 5180}}
		snap.Surveys = []collector.Survey{{Iface: "phy0-ap0", MHz: 5180,
			ActiveTime: ts * 100, BusyTime: ts * 40, RxTime: uint64(ts * 20), TxTime: uint64(ts * 10)}}
		snap.Stations = []collector.Station{{Iface: "phy0-ap0", MAC: "00:11:22:33:44:55",
			Signal: -50, SignalKnown: true, PresenceKnown: true, TXQualityKnown: true,
			ConnectedTime: ts, TX: collector.Rate{Packets: ts * 10, Retries: ts, Failed: ts / 10}}}
		s.Observe(context.Background(), snap)
	}
	rows := s.Flush(at(120))
	if _, ok := findKind(rows, KindChanBusy); !ok {
		t.Fatal("stale stable mapping suppressed the independently keyed interface survey")
	}
	for _, row := range rows {
		if strings.HasPrefix(string(row.Kind), "radio_") {
			t.Fatalf("stale interface-to-radio mapping emitted stable series: %+v", row)
		}
	}
}

func TestRadioAirtimeSplitRequiresCapabilityAndUsesCounterDeltas(t *testing.T) {
	observe := func(s *Store, deviceID int64, proved bool) {
		for _, sample := range []struct {
			ts, active, busy int64
			rx, tx           uint64
		}{
			{10, 1_000_000, 900_000, 600_000, 200_000},
			{20, 1_001_000, 900_600, 600_200, 200_100},
		} {
			snap := snapshot(deviceID, sample.ts, 100+sample.ts)
			snap.RadiosKnown = true
			snap.AirtimeSplit = proved
			snap.IfaceRadios = map[string]string{"phy0-ap0": "radio0"}
			snap.APs = []collector.AP{{Iface: "phy0-ap0", Freq: 5180}}
			snap.Surveys = []collector.Survey{{Iface: "phy0-ap0", MHz: 5180,
				ActiveTime: sample.active, BusyTime: sample.busy, RxTime: sample.rx, TxTime: sample.tx}}
			s.Observe(context.Background(), snap)
		}
	}

	proved := testStore()
	observe(proved, 1, true)
	rows := proved.Flush(at(120))
	for kind, want := range map[Kind]float64{
		KindRadioUtilization:  60,
		KindRadioRXAirtime:    20,
		KindRadioTXAirtime:    10,
		KindRadioInterference: 30,
	} {
		got, ok := findKind(rows, kind)
		if !ok || got.Key != "radio0" || math.Abs(got.Avg-want) > 1e-9 {
			t.Errorf("%s = %+v, present=%v, want %.1f%% on stable radio key", kind, got, ok, want)
		}
	}

	unproved := testStore()
	observe(unproved, 2, false)
	rows = unproved.Flush(at(120))
	if _, ok := findKind(rows, KindRadioUtilization); !ok {
		t.Fatal("portable utilization disappeared without airtime-split proof")
	}
	for _, kind := range []Kind{KindRadioRXAirtime, KindRadioTXAirtime, KindRadioInterference} {
		if got, ok := findKind(rows, kind); ok {
			t.Errorf("unproved %s emitted: %+v", kind, got)
		}
	}
}

func TestRadioQualityAggregatesEveryStationOrReportsUnavailable(t *testing.T) {
	s := testStore()
	makeSnapshot := func(ts int64, secondUnknown bool) collector.Snapshot {
		snap := snapshot(1, ts, 100+ts)
		snap.RadiosKnown = true
		snap.IfaceRadios = map[string]string{"phy0-ap0": "radio0", "phy0-ap1": "radio0"}
		snap.Stations = []collector.Station{
			{Iface: "phy0-ap0", MAC: "00:11:22:33:44:55", Signal: -50,
				ConnectedTime: ts, TXQualityKnown: true, SignalKnown: true, PresenceKnown: true,
				TX: collector.Rate{Packets: 100 + ts*10, Retries: 10 + ts, Failed: 2 + ts/10}},
			{Iface: "phy0-ap1", MAC: "00:11:22:33:44:66", Signal: -70,
				ConnectedTime: ts, TXQualityKnown: !secondUnknown, SignalKnown: true, PresenceKnown: true,
				TX: collector.Rate{Packets: 200 + ts*20, Retries: 20 + ts*2, Failed: 4 + ts/5}},
		}
		return snap
	}
	s.Observe(context.Background(), makeSnapshot(10, false))
	s.Observe(context.Background(), makeSnapshot(20, false))
	rows := s.Flush(at(120))
	if signal, ok := findKind(rows, KindRadioSignalAvg); !ok || signal.Key != "radio0" || signal.Avg != -60 {
		t.Fatalf("radio signal = %+v, ok=%v", signal, ok)
	}
	if retry, ok := findKind(rows, KindRadioRetryDelta); !ok || retry.Key != "radio0" || math.Abs(retry.Avg-9.090909) > 1e-5 {
		t.Fatalf("radio retry = %+v, ok=%v", retry, ok)
	}

	incomplete := testStore()
	incomplete.Observe(context.Background(), makeSnapshot(10, true))
	incomplete.Observe(context.Background(), makeSnapshot(20, true))
	if retry, ok := findKind(incomplete.Flush(at(120)), KindRadioRetryDelta); ok {
		t.Fatalf("partial radio denominator emitted: %+v", retry)
	}
}

func TestPartialSameRadioAssoclistKeepsClientsButSuppressesAggregate(t *testing.T) {
	s := testStore()
	for _, ts := range []int64{10, 20} {
		snap := snapshot(1, ts, 100+ts)
		snap.RadiosKnown = true
		snap.IfaceRadios = map[string]string{"phy0-ap0": "radio0", "phy0-ap1": "radio0"}
		snap.AssocAsked = map[string]bool{"phy0-ap0": true, "phy0-ap1": true}
		snap.AssocAnswered = map[string]bool{"phy0-ap0": true}
		snap.Stations = []collector.Station{{
			Iface: "phy0-ap0", MAC: "00:11:22:33:44:55", Signal: -50,
			ConnectedTime: ts, PresenceKnown: true, SignalKnown: true, TXQualityKnown: true,
			TX: collector.Rate{Packets: 100 + ts*10, Retries: 10 + ts, Failed: 2 + ts/10},
		}}
		s.Observe(context.Background(), snap)
	}
	rows := s.Flush(at(120))
	for _, kind := range []Kind{KindStaRSSI, KindStaRetryDelta, KindStaTXFailDelta} {
		if row, ok := findKind(rows, kind); !ok {
			t.Errorf("answered client %s missing: %+v", kind, row)
		}
	}
	for _, kind := range []Kind{KindRadioSignalAvg, KindRadioRetryDelta, KindRadioTXFailDelta} {
		if row, ok := findKind(rows, kind); ok {
			t.Errorf("partial radio emitted %s: %+v", kind, row)
		}
	}
}
