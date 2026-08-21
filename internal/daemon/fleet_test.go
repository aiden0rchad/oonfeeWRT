package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/observability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestLegacyOverlongPollIntervalIsClampedToFreshnessContract(t *testing.T) {
	d := &Daemon{}
	target := d.target(&store.Device{PollInterval: 3600})
	if target.Baseline != 15*time.Minute {
		t.Fatalf("baseline=%v, want 15m", target.Baseline)
	}
}

func TestLogOnlySnapshotDurablyAdvancesCoverageWithoutFullPollState(t *testing.T) {
	ctx := context.Background()
	d := openDaemon(t)
	adopted := int64(1)
	dev := &store.Device{MAC: "02:00:00:00:00:01", Host: "192.0.2.1",
		Name: "ap", Role: "ap", AdoptedAt: &adopted}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(100_000)
	d.sink().Observe(ctx, collector.Snapshot{
		DeviceID: dev.ID, MAC: dev.MAC, Name: dev.Name, Tier: collector.Baseline,
		At: at, LogOnly: true, LogsFresh: true,
		LogEpoch: observability.LogEpoch{
			BootID: "11111111-2222-4333-8444-555555555555", PID: 81,
		},
	})
	cursor, err := d.Store.LoadIngestCursor(ctx, dev.ID, openWRTLogSource)
	if err != nil || cursor.UpdatedAt != at.UnixMilli() {
		t.Fatalf("auxiliary log cursor=%+v err=%v", cursor, err)
	}
	fresh, err := d.Store.DeviceByID(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.LastSeen != nil {
		t.Fatalf("log-only attempt changed full-poll last_seen to %v", *fresh.LastSeen)
	}
	d.sinkMu.Lock()
	_, reachabilityKnown := d.sinkKnown[dev.ID]
	d.sinkMu.Unlock()
	if reachabilityKnown {
		t.Fatal("log-only attempt changed reachability state")
	}
}

func TestDeviceConnectionKeyTracksOnlyConnectionIdentity(t *testing.T) {
	base := store.Device{
		MAC: "aa:bb:cc:dd:ee:ff", Host: "192.0.2.1", Port: 443,
		Scheme: "https", CertFP: "cert", HostKeyFP: "ssh", CredEnc: []byte("sealed"),
		Name: "router", Class: "A", PollInterval: 60,
	}
	want := deviceConnectionKey(&base)
	nonConnection := base
	nonConnection.Name = "renamed"
	nonConnection.Class = "B"
	nonConnection.PollInterval = 120
	nonConnection.HostKeyFP = "new-ssh-pin"
	if got := deviceConnectionKey(&nonConnection); got != want {
		t.Fatalf("non-polling metadata changed connection key: %q != %q", got, want)
	}

	tests := []struct {
		name   string
		change func(*store.Device)
	}{
		{"mac", func(d *store.Device) { d.MAC = "11:22:33:44:55:66" }},
		{"host", func(d *store.Device) { d.Host = "192.0.2.2" }},
		{"port", func(d *store.Device) { d.Port = 80 }},
		{"scheme", func(d *store.Device) { d.Scheme = "http" }},
		{"certificate", func(d *store.Device) { d.CertFP = "other-cert" }},
		{"credential", func(d *store.Device) { d.CredEnc = []byte("other-sealed") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			tt.change(&changed)
			if got := deviceConnectionKey(&changed); got == want {
				t.Fatalf("connection identity change retained key %q", got)
			}
		})
	}
}
