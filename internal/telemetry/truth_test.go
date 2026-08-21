package telemetry

import (
	"context"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
)

func TestOmittedDirectionalCountersDoNotCreateZeroBaselines(t *testing.T) {
	s := testStore()
	for _, sample := range []struct{ ts, rx int64 }{{10, 100}, {20, 300}} {
		snap := snapshot(1, sample.ts, 100+sample.ts)
		snap.NetDevsFresh = true
		iface := collector.Interface{Up: true}
		iface.Stats.RxBytes = sample.rx
		iface.Stats.RxBytesKnown = true
		snap.Interfaces = map[string]collector.Interface{"eth0": iface}
		snap.Stations = []collector.Station{{
			Iface: "phy0-ap0", MAC: "00:11:22:33:44:55", PresenceKnown: true,
			RX: collector.Rate{Bytes: sample.rx, BytesKnown: true},
		}}
		s.Observe(context.Background(), snap)
	}
	rows := s.Flush(at(120))
	for _, kind := range []Kind{KindIfaceRx, KindStaRx} {
		if row, ok := findKind(rows, kind); !ok || row.Cnt != 1 {
			t.Errorf("known %s = %+v, present=%v", kind, row, ok)
		}
	}
	for _, kind := range []Kind{KindIfaceTx, KindStaTx, KindStaRSSI} {
		if row, ok := findKind(rows, kind); ok {
			t.Errorf("omitted %s became numeric data: %+v", kind, row)
		}
	}
}
