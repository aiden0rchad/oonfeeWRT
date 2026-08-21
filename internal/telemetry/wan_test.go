package telemetry

import (
	"context"
	"math"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
)

func TestWANProbeMapsToStableSiteSeries(t *testing.T) {
	s := testStore()
	latency := 12.25
	snap := snapshot(7, 100, 1000)
	snap.WAN = &collector.WANProbe{Up: true, LossPct: 100.0 / 3, LatencyMS: &latency}
	s.Observe(context.Background(), snap)

	got := map[Kind]Rollup{}
	for _, row := range s.Flush(at(600)) {
		switch row.Kind {
		case KindSiteWANUp, KindSiteWANLoss, KindSiteWANLatency:
			if row.Key != "" {
				t.Errorf("%s key = %q, want the stable site key", row.Kind, row.Key)
			}
			got[row.Kind] = row
		}
	}
	if len(got) != 3 {
		t.Fatalf("WAN series = %+v, want up, loss and latency", got)
	}
	if got[KindSiteWANUp].Avg != 1 ||
		math.Abs(got[KindSiteWANLoss].Avg-100.0/3) > 0.001 ||
		got[KindSiteWANLatency].Avg != latency {
		t.Fatalf("WAN values = %+v", got)
	}
	for _, row := range got {
		if row.DeviceID != 7 {
			t.Errorf("%s device provenance = %d, want gateway 7", row.Kind, row.DeviceID)
		}
	}
}

func TestWANProbeDownOmitsLatencyAndUnavailableOmitsAllWANSeries(t *testing.T) {
	s := testStore()
	down := snapshot(1, 100, 1000)
	down.WAN = &collector.WANProbe{Up: false, LossPct: 100}
	s.Observe(context.Background(), down)

	unknown := snapshot(2, 100, 1000)
	unknown.WAN = nil
	s.Observe(context.Background(), unknown)

	rows := s.Flush(at(600))
	seenUp, seenLoss := false, false
	for _, row := range rows {
		if row.DeviceID == 2 && (row.Kind == KindSiteWANUp ||
			row.Kind == KindSiteWANLoss || row.Kind == KindSiteWANLatency) {
			t.Fatalf("unavailable probe became a value: %+v", row)
		}
		if row.DeviceID != 1 {
			continue
		}
		switch row.Kind {
		case KindSiteWANUp:
			seenUp = true
			if row.Avg != 0 {
				t.Errorf("measured down = %v, want 0", row.Avg)
			}
		case KindSiteWANLoss:
			seenLoss = true
			if row.Avg != 100 {
				t.Errorf("measured loss = %v, want 100", row.Avg)
			}
		case KindSiteWANLatency:
			t.Fatalf("100%% loss invented a latency: %+v", row)
		}
	}
	if !seenUp || !seenLoss {
		t.Fatalf("measured down series missing: up=%v loss=%v", seenUp, seenLoss)
	}
}

func TestWANProbeGaugeSurvivesRouterUptimeReset(t *testing.T) {
	s := testStore()
	firstLatency, afterLatency := 10.0, 20.0
	first := snapshot(1, 100, 5000)
	first.WAN = &collector.WANProbe{Up: true, LossPct: 0, LatencyMS: &firstLatency}
	s.Observe(context.Background(), first)
	after := snapshot(1, 110, 20)
	after.WAN = &collector.WANProbe{Up: true, LossPct: 0, LatencyMS: &afterLatency}
	s.Observe(context.Background(), after)

	var latency *Rollup
	rows := s.Flush(at(600))
	for i := range rows {
		if rows[i].Kind == KindSiteWANLatency {
			latency = &rows[i]
		}
	}
	if latency == nil || latency.Cnt != 2 || latency.Avg != 15 {
		t.Fatalf("latency across uptime reset = %+v, want cnt=2 avg=15", latency)
	}
}

func TestWANOnlySnapshotEmitsNoSyntheticBaselineMetrics(t *testing.T) {
	s := testStore()
	latency := 8.5
	s.Observe(context.Background(), collector.Snapshot{
		DeviceID: 3, At: at(100), WANOnly: true,
		WAN: &collector.WANProbe{Up: true, LossPct: 0, LatencyMS: &latency},
	})
	rows := s.Flush(at(600))
	if len(rows) != 3 {
		t.Fatalf("WAN-only snapshot emitted non-WAN series: %+v", rows)
	}
	for _, row := range rows {
		switch row.Kind {
		case KindSiteWANUp, KindSiteWANLoss, KindSiteWANLatency:
		default:
			t.Fatalf("WAN-only snapshot emitted %s", row.Kind)
		}
	}
}

func TestWANOnlySnapshotPreservesFullPollCounterBaselines(t *testing.T) {
	s := testStore()
	first := snapshot(4, 100, 1_000)
	firstIface := collector.Interface{Up: true}
	firstIface.Stats.RxBytes = 1_000
	first.Interfaces = map[string]collector.Interface{"eth0": firstIface}
	s.Observe(context.Background(), first)
	s.Observe(context.Background(), collector.Snapshot{
		DeviceID: 4, At: at(160), WANOnly: true,
		WAN: &collector.WANProbe{Up: true},
	})
	second := snapshot(4, 200, 1_100)
	secondIface := collector.Interface{Up: true}
	secondIface.Stats.RxBytes = 2_000
	second.Interfaces = map[string]collector.Interface{"eth0": secondIface}
	s.Observe(context.Background(), second)

	row, ok := findKind(s.Flush(at(600)), KindIfaceRx)
	if !ok || row.Key != "eth0" || row.Avg != 10 {
		t.Fatalf("counter baseline across WAN-only sample = %+v ok=%v, want 10 B/s", row, ok)
	}
}
