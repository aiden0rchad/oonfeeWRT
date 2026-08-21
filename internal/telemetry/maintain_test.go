package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	_ "modernc.org/sqlite"
)

func openMaintainerDB(t *testing.T) (*store.DB, int64) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oonfee.db")
	keyring := path + ".keyring"
	keeper, err := secrets.Create(keyring, []byte("maintainer test passphrase"),
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if errors.Is(err, os.ErrExist) {
		t.Fatal("unexpected existing test keyring")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { keeper.Close() })
	db, err := store.Open(ctx, "sqlite", path, keeper)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	device := &store.Device{MAC: "02:00:00:00:00:01", Host: "192.0.2.1", Name: "test"}
	if err := db.UpsertDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	return db, device.ID
}

func TestFinalFlushPersistsCompletedBucketsWithoutWritingCurrentPartial(t *testing.T) {
	ctx := context.Background()
	db, deviceID := openMaintainerDB(t)
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Process A has one completed bucket and one sample in the current bucket.
	first := New(Options{Window: 5 * time.Minute})
	key := SeriesKey{DeviceID: deviceID, Kind: KindLoad1}
	first.Gauge(key, base.Add(-2*time.Minute).Unix(), 10)
	first.Gauge(key, base.Add(2*time.Minute).Unix(), 20)
	m := NewMaintainer(db, first, quiet)
	m.Now = func() time.Time { return base.Add(150 * time.Second) }
	m.tick(ctx, true)

	got, err := db.QuerySeries(ctx, deviceID, string(KindLoad1), "",
		base.Add(-5*time.Minute), base.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 1 || got.Points[0].TS != base.Add(-5*time.Minute).Unix() ||
		got.Points[0].Avg != 10 {
		t.Fatalf("shutdown rows=%+v, want only the completed bucket", got.Points)
	}

	// Process B starts in the same in-progress bucket. Its shutdown must not
	// replace a partial row from A: no process writes that canonical row until
	// the bucket is complete. The unavoidable tradeoff is at most the current
	// process's one incomplete window, never a persisted bucket's integrity.
	second := New(Options{Window: 5 * time.Minute})
	second.Gauge(key, base.Add(3*time.Minute).Unix(), 30)
	m = NewMaintainer(db, second, quiet)
	m.Now = func() time.Time { return base.Add(4 * time.Minute) }
	m.tick(ctx, true)
	got, err = db.QuerySeries(ctx, deviceID, string(KindLoad1), "",
		base.Add(-5*time.Minute), base.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 1 || got.Points[0].Avg != 10 {
		t.Fatalf("second shutdown changed persisted buckets: %+v", got.Points)
	}

	m.Now = func() time.Time { return base.Add(5 * time.Minute) }
	m.Tick(ctx)
	got, err = db.QuerySeries(ctx, deviceID, string(KindLoad1), "",
		base.Add(-5*time.Minute), base.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 2 || got.Points[0].Avg != 10 || got.Points[1].Avg != 30 {
		t.Fatalf("completed buckets=%+v, want process A then process B", got.Points)
	}
}
