package telemetry

import (
	"context"
	"log/slog"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// Maintainer runs the five-minute tick: drain the ring, fold, prune.
//
// One goroutine, one transaction per tick. The ordering inside a tick is not
// arbitrary — flush before fold so the newest window can be folded, fold before
// prune so nothing is deleted at 5-minute resolution before it exists at hourly.
type Maintainer struct {
	Store     *store.DB
	Samples   *Store
	Retention store.Retention
	Interval  time.Duration
	Log       *slog.Logger

	// ClientTTL is how long a client is remembered after it was last seen.
	// Zero uses DefaultClientTTL.
	ClientTTL time.Duration

	// AfterTick runs at the end of every cycle. The daemon uses it to expire
	// idle API sessions and lapsed login lockouts — housekeeping that wants the
	// same cadence and does not deserve a timer of its own.
	AfterTick func()

	// Now is injectable so tests do not have to wait five minutes.
	Now func() time.Time
}

// NewMaintainer wires one up with the shipped retention policy.
func NewMaintainer(db *store.DB, samples *Store, log *slog.Logger) *Maintainer {
	if log == nil {
		log = slog.Default()
	}
	return &Maintainer{
		Store: db, Samples: samples,
		Retention: store.DefaultRetention(),
		Interval:  DefaultWindow,
		Log:       log,
		Now:       time.Now,
	}
}

// Run ticks until the context is cancelled, then performs one final flush.
//
// The final flush matters: without it, up to five minutes of samples are lost
// every time the controller restarts, and a fleet that gets updated weekly would
// have a visible notch in every graph.
func (m *Maintainer) Run(ctx context.Context) {
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// A cancelled context cannot be used to write, so the final flush
			// gets its own bounded one.
			fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			m.tick(fctx, true)
			cancel()
			return
		case <-t.C:
			m.tick(ctx, false)
		}
	}
}

// Tick runs one maintenance cycle. Exported for the daemon's shutdown path and
// for tests.
func (m *Maintainer) Tick(ctx context.Context) { m.tick(ctx, false) }

func (m *Maintainer) tick(ctx context.Context, final bool) {
	now := m.now()

	// On shutdown, flush the window that is still filling. Normally a partial
	// window is left alone so a later tick can complete it, but there is no
	// later tick — and a partial average with an honest sample count beats a
	// hole in the series.
	at := now
	if final {
		at = now.Add(m.window())
	}

	rows := m.Samples.Flush(at)
	if len(rows) > 0 {
		if err := m.Store.WriteRollups(ctx, toStoreRows(rows)); err != nil {
			// Do not retry here. The samples are already drained from the ring,
			// so a retry would have nothing to write; what matters is that the
			// loss is visible rather than silent.
			m.Log.Error("could not write telemetry rollups; this window is lost",
				"rows", len(rows), "err", err)
		}
	}
	if final {
		m.Log.Info("final telemetry flush", "rows", len(rows))
		return
	}

	if err := m.Store.FoldHourly(ctx, now); err != nil {
		m.Log.Error("could not fold hourly rollups", "err", err)
		return // pruning now would delete 5-minute rows that never got folded
	}
	res, err := m.Store.Prune(ctx, now, m.Retention)
	if err != nil {
		m.Log.Error("could not prune telemetry", "err", err)
		return
	}
	// Clients age out on their own schedule. Randomised MACs mean one phone can
	// produce a new "client" per SSID per reconnect, so the table grows without
	// this even on a small network.
	if n, err := m.Store.PruneClients(ctx, now.Add(-m.ClientRetention())); err != nil {
		m.Log.Error("could not prune clients", "err", err)
	} else if n > 0 {
		m.Log.Debug("forgot inactive clients", "count", n)
	}
	if res.FiveMinute+res.Hourly+res.Events > 0 {
		m.Log.Debug("pruned telemetry", "rollup_5m", res.FiveMinute,
			"rollup_1h", res.Hourly, "events", res.Events)
	}
	if m.AfterTick != nil {
		m.AfterTick()
	}
}

// window is the ROLLUP period, which belongs to the sample store — not the tick
// interval, which belongs here. Deriving it means the two cannot drift apart:
// a tick shorter than the window is harmless (most ticks flush nothing), but a
// final flush offset by the wrong amount would drop the last window entirely.
// DefaultClientTTL keeps a client for 30 days after it was last seen, which is
// long enough to recognise a laptop returning from a holiday and short enough
// that randomised MACs do not accumulate forever.
const DefaultClientTTL = 30 * 24 * time.Hour

// ClientRetention is how long an unseen client is kept.
func (m *Maintainer) ClientRetention() time.Duration {
	if m.ClientTTL > 0 {
		return m.ClientTTL
	}
	return DefaultClientTTL
}

func (m *Maintainer) window() time.Duration {
	if m.Samples != nil {
		return m.Samples.Window()
	}
	return DefaultWindow
}

func (m *Maintainer) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func toStoreRows(rows []Rollup) []store.RollupRow {
	out := make([]store.RollupRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.RollupRow{
			DeviceID: r.DeviceID, Kind: string(r.Kind), Key: r.Key,
			TS: r.TS, Avg: r.Avg, Min: r.Min, Max: r.Max, Cnt: r.Cnt,
		})
	}
	return out
}
