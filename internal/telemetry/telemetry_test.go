package telemetry

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
)

func testStore() *Store {
	return New(Options{Window: 60 * time.Second, Capacity: 16, IdleWindows: 2})
}

func at(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func TestFlushAggregatesCompletedWindowsOnly(t *testing.T) {
	s := testStore()
	k := SeriesKey{DeviceID: 1, Kind: KindLoad1}

	// Window [0,60): 1, 3, 2. Window [60,120): still filling.
	s.Gauge(k, 10, 1)
	s.Gauge(k, 20, 3)
	s.Gauge(k, 30, 2)
	s.Gauge(k, 70, 99)

	rows := s.Flush(at(90))
	if len(rows) != 1 {
		t.Fatalf("got %d rollups, want 1 (the window in progress must not be emitted)", len(rows))
	}
	r := rows[0]
	if r.TS != 0 {
		t.Errorf("window start = %d, want 0", r.TS)
	}
	if r.Cnt != 3 || r.Min != 1 || r.Max != 3 || math.Abs(r.Avg-2) > 1e-9 {
		t.Errorf("got cnt=%d min=%v max=%v avg=%v, want 3/1/3/2", r.Cnt, r.Min, r.Max, r.Avg)
	}

	// The still-filling sample survived the drain and emerges once its window
	// closes — losing it would notch every series at every flush boundary.
	rows = s.Flush(at(150))
	if len(rows) != 1 || rows[0].TS != 60 || rows[0].Cnt != 1 || rows[0].Avg != 99 {
		t.Fatalf("second flush = %+v, want the [60,120) window with the retained sample", rows)
	}
}

func TestFlushIsSortedAndAttributed(t *testing.T) {
	s := testStore()
	s.Gauge(SeriesKey{DeviceID: 2, Kind: KindLoad1}, 10, 1)
	s.Gauge(SeriesKey{DeviceID: 1, Kind: KindStaRSSI, Key: "bb"}, 10, -60)
	s.Gauge(SeriesKey{DeviceID: 1, Kind: KindStaRSSI, Key: "aa"}, 10, -50)

	rows := s.Flush(at(120))
	if len(rows) != 3 {
		t.Fatalf("got %d rollups, want 3", len(rows))
	}
	// Deterministic order keeps the write path reproducible, which is what makes
	// a failing flush debuggable.
	want := []SeriesKey{
		{DeviceID: 1, Kind: KindStaRSSI, Key: "aa"},
		{DeviceID: 1, Kind: KindStaRSSI, Key: "bb"},
		{DeviceID: 2, Kind: KindLoad1},
	}
	for i, w := range want {
		if rows[i].SeriesKey != w {
			t.Errorf("row %d = %+v, want %+v", i, rows[i].SeriesKey, w)
		}
	}
}

// A stalled flush must not grow memory without bound. Losing the oldest samples
// is the intended failure, and the capacity is chosen so it cannot happen in
// normal operation.
func TestRingOverwritesRatherThanGrows(t *testing.T) {
	s := New(Options{Window: 60 * time.Second, Capacity: 4})
	k := SeriesKey{DeviceID: 1, Kind: KindLoad1}
	for i := range int64(10) {
		s.Gauge(k, i, float64(i))
	}
	rows := s.Flush(at(600))
	if len(rows) != 1 {
		t.Fatalf("got %d rollups, want 1", len(rows))
	}
	if rows[0].Cnt != 4 {
		t.Errorf("cnt = %d, want 4 (the ring holds four samples)", rows[0].Cnt)
	}
	if rows[0].Min != 6 || rows[0].Max != 9 {
		t.Errorf("kept samples %v..%v, want the newest four (6..9)", rows[0].Min, rows[0].Max)
	}
}

// A client that disconnects must not keep its ring forever: the key is a MAC,
// which is the one identifier guaranteed to churn.
func TestIdleSeriesAreRetired(t *testing.T) {
	s := testStore() // IdleWindows: 2
	k := SeriesKey{DeviceID: 1, Kind: KindStaRSSI, Key: "aa"}
	s.Gauge(k, 10, -50)

	if rows := s.Flush(at(120)); len(rows) != 1 {
		t.Fatalf("got %d rollups, want 1", len(rows))
	}
	if s.Len() != 1 {
		t.Fatalf("series count = %d right after a flush, want 1", s.Len())
	}
	s.Flush(at(180))
	s.Flush(at(240))
	if s.Len() != 0 {
		t.Fatalf("series count = %d after two empty windows, want 0", s.Len())
	}
}

// ---- counters ----

func rateOf(t *testing.T, s *Store, k SeriesKey, ts int64, c uint64) (float64, bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rate(k, ts, c, false, false)
}

func TestRateNeedsTwoReadings(t *testing.T) {
	s := testStore()
	k := SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "eth0"}

	if _, ok := rateOf(t, s, k, 100, 1000); ok {
		t.Fatal("the first reading of a counter produced a rate")
	}
	v, ok := rateOf(t, s, k, 110, 2000)
	if !ok {
		t.Fatal("the second reading produced no rate")
	}
	if v != 100 {
		t.Errorf("rate = %v, want 100 B/s (1000 bytes over 10 s)", v)
	}
}

// A gap is not a reset. The rate over the longer interval is the correct average
// for the time that actually passed.
func TestRateAcrossAGapAveragesTheGap(t *testing.T) {
	s := testStore()
	k := SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "eth0"}
	rateOf(t, s, k, 100, 0)
	v, ok := rateOf(t, s, k, 400, 30000)
	if !ok {
		t.Fatal("no rate after a gap")
	}
	if v != 100 {
		t.Errorf("rate = %v, want 100 B/s (30000 bytes over 300 s)", v)
	}
}

// At 1 Gbit/s a 32-bit byte counter wraps every 34 seconds. Discarding wraps
// would destroy the throughput series on exactly the links anyone cares about.
func TestThirtyTwoBitWrapIsRecovered(t *testing.T) {
	s := testStore()
	k := SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "eth0"}

	const nearMax = uint64(1<<32) - 1000
	rateOf(t, s, k, 100, nearMax)
	v, ok := rateOf(t, s, k, 110, 500) // wrapped: 1000 + 500 bytes
	if !ok {
		t.Fatal("a 32-bit wrap was discarded")
	}
	if v != 150 {
		t.Errorf("rate = %v, want 150 B/s (1500 bytes over 10 s)", v)
	}
}

// Once a counter has been seen above 2^32 it is provably 64-bit, so a decrease
// cannot be a wrap. Treating it as one would invent a 4 GB burst.
func TestWideCounterDecreaseIsAResetNotAWrap(t *testing.T) {
	s := testStore()
	k := SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "eth0"}

	rateOf(t, s, k, 100, 1<<33) // proves 64-bit
	if _, ok := rateOf(t, s, k, 110, 5000); ok {
		t.Fatal("a decrease on a proven 64-bit counter was reported as a wrap")
	}
	// ...and the baseline rebased, so the next reading measures from there.
	v, ok := rateOf(t, s, k, 120, 6000)
	if !ok || v != 100 {
		t.Errorf("after the reset: rate = %v ok=%v, want 100 B/s", v, ok)
	}
}

// A reboot resets every counter on the device. Uptime says so directly, which
// beats deducing it from a number that moved the wrong way.
func TestRebootIsDetectedFromUptime(t *testing.T) {
	s := testStore()
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.observeUptime(1, 1000, 5000) {
		t.Fatal("the first uptime reading was reported as a reboot")
	}
	if s.observeUptime(1, 1060, 5060) {
		t.Fatal("a normal 60 s advance was reported as a reboot")
	}
	// Uptime advanced only 10 s while 60 s of wall clock passed.
	if !s.observeUptime(1, 1070, 5120) {
		t.Fatal("a restart mid-interval was missed")
	}
	// A device that rebooted long ago and came back with a LARGER uptime than
	// last seen is still a reboot, because it did not advance enough.
	s.observeUptime(1, 100, 6000)
	if !s.observeUptime(1, 200, 10000) {
		t.Fatal("a reboot during a long gap was missed (uptime rose, but not enough)")
	}
}

func TestRebootDropsEveryCounterOnTheDevice(t *testing.T) {
	s := testStore()
	rx := SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "eth0"}
	tx := SeriesKey{DeviceID: 1, Kind: KindIfaceTx, Key: "eth0"}
	other := SeriesKey{DeviceID: 2, Kind: KindIfaceRx, Key: "eth0"}
	rateOf(t, s, rx, 100, 1000)
	rateOf(t, s, tx, 100, 2000)
	rateOf(t, s, other, 100, 3000)

	s.mu.Lock()
	s.forgetCounters(1)
	s.mu.Unlock()

	if _, ok := rateOf(t, s, rx, 110, 1100); ok {
		t.Error("rx kept its baseline across a reboot")
	}
	if _, ok := rateOf(t, s, tx, 110, 2100); ok {
		t.Error("tx kept its baseline across a reboot")
	}
	if _, ok := rateOf(t, s, other, 110, 3100); !ok {
		t.Error("another device's baseline was dropped")
	}
}

func TestImplausibleRateIsRefused(t *testing.T) {
	s := testStore()
	k := SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "eth0"}
	rateOf(t, s, k, 100, 1<<40)
	// 2^50 bytes in one second is not a measurement, whatever produced it.
	if v, ok := rateOf(t, s, k, 101, 1<<50); ok {
		t.Errorf("an impossible rate of %v B/s was accepted", v)
	}
}

func TestSameTimestampProducesNoRate(t *testing.T) {
	s := testStore()
	k := SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "eth0"}
	rateOf(t, s, k, 100, 1000)
	if _, ok := rateOf(t, s, k, 100, 5000); ok {
		t.Fatal("two readings at one timestamp produced a rate")
	}
}

// ---- observing a poll ----

func snapshot(devID int64, ts int64, uptime int64) collector.Snapshot {
	return collector.Snapshot{
		DeviceID: devID, MAC: "aa:bb:cc:dd:ee:ff", Tier: collector.Baseline,
		At: at(ts), Uptime: uptime,
		Load:   [3]float64{0.5, 0.4, 0.3},
		Memory: collector.Memory{Total: 1000, Available: 400},
	}
}

func TestObserveSkipsFailedPollsWithoutLosingBaselines(t *testing.T) {
	s := testStore()
	ctx := context.Background()

	good := snapshot(1, 100, 1000)
	good.Interfaces = map[string]collector.Interface{"eth0": ifaceWith(1000)}
	s.Observe(ctx, good)

	// A failed poll must contribute nothing AND leave the counter baseline
	// alone: rebasing on every hiccup silently drops a sample each time the
	// network stutters.
	bad := snapshot(1, 160, 1060)
	bad.Err = context.DeadlineExceeded
	s.Observe(ctx, bad)

	next := snapshot(1, 220, 1120)
	next.Interfaces = map[string]collector.Interface{"eth0": ifaceWith(13000)}
	s.Observe(ctx, next)

	rows := s.Flush(at(600))
	var rate *Rollup
	for i := range rows {
		if rows[i].Kind == KindIfaceRx {
			rate = &rows[i]
		}
	}
	if rate == nil {
		t.Fatal("no interface rate series after the gap")
	}
	// 12000 bytes over the full 120 s, not over the 60 s since the last good poll.
	if rate.Avg != 100 {
		t.Errorf("rate = %v B/s, want 100 (12000 bytes over 120 s spanning the gap)", rate.Avg)
	}
}

func ifaceWith(rx int64) collector.Interface {
	var i collector.Interface
	i.Up, i.Carrier = true, true
	i.Stats.RxBytes = rx
	i.Stats.TxBytes = rx * 2
	return i
}

func TestObserveMapsAPoll(t *testing.T) {
	s := testStore()
	ctx := context.Background()
	n := 3
	snap := snapshot(1, 100, 1000)
	snap.Tier = collector.Focused
	snap.Interfaces = map[string]collector.Interface{
		"eth0": ifaceWith(1000),
		"lo":   ifaceWith(500), // must be skipped: nobody graphs loopback
	}
	snap.APs = []collector.AP{{
		Iface: "wlan0", Clients: &n,
		Airtime: &collector.Airtime{Utilization: 172},
	}, {
		Iface: "wlan1", // no client count and no airtime: both unknown
	}}
	snap.Surveys = []collector.Survey{{Iface: "wlan0", ActiveTime: 1000, BusyTime: 250}}
	snap.Stations = []collector.Station{{
		Iface: "wlan0", MAC: "AA:BB:CC:11:22:33", Signal: -52,
		TX: collector.Rate{Bytes: 5000, Packets: 90, Retries: 10},
	}}
	s.Observe(ctx, snap)

	rows := s.Flush(at(600))
	got := map[SeriesKey]float64{}
	for _, r := range rows {
		got[r.SeriesKey] = r.Avg
	}

	want := map[SeriesKey]float64{
		{DeviceID: 1, Kind: KindLoad1}:                              0.5,
		{DeviceID: 1, Kind: KindMemUsed}:                            600,
		{DeviceID: 1, Kind: KindMemPct}:                             60,
		{DeviceID: 1, Kind: KindAPClients, Key: "wlan0"}:            3,
		{DeviceID: 1, Kind: KindStaRSSI, Key: "aa:bb:cc:11:22:33"}:  -52,
		{DeviceID: 1, Kind: KindStaRetry, Key: "aa:bb:cc:11:22:33"}: 10,
	}
	for k, w := range want {
		v, ok := got[k]
		if !ok {
			t.Errorf("missing series %+v", k)
			continue
		}
		if math.Abs(v-w) > 1e-6 {
			t.Errorf("%+v = %v, want %v", k, v, w)
		}
	}
	// 172 on the 0-255 BSS-Load scale is ~67%, not 172%.
	if v := got[SeriesKey{DeviceID: 1, Kind: KindAPAirtime, Key: "wlan0"}]; v < 67 || v > 68 {
		t.Errorf("airtime = %v, want ~67.5 (the 0-255 scale is not a percentage)", v)
	}
	// An AP whose count could not be read must produce NO sample. A zero here
	// would draw a dip that means "one radio did not reply".
	if _, ok := got[SeriesKey{DeviceID: 1, Kind: KindAPClients, Key: "wlan1"}]; ok {
		t.Error("an unknown client count was recorded as a value")
	}
	if _, ok := got[SeriesKey{DeviceID: 1, Kind: KindIfaceRx, Key: "lo"}]; ok {
		t.Error("loopback was sampled")
	}
	// The noise floor is deliberately not a series: it is gated per radio and
	// swings 40+ dB on the reference device, so a rollup of it would be a
	// smooth, stable, meaningless line.
	for k := range got {
		if k.Kind == "chan_noise" || k.Kind == "sta_noise" {
			t.Errorf("a noise series was recorded: %+v", k)
		}
	}
}

func TestObserveDetectsRebootThroughTheFullPath(t *testing.T) {
	s := testStore()
	ctx := context.Background()

	a := snapshot(1, 100, 5000)
	a.Interfaces = map[string]collector.Interface{"eth0": ifaceWith(1_000_000)}
	s.Observe(ctx, a)

	b := snapshot(1, 160, 5060)
	b.Interfaces = map[string]collector.Interface{"eth0": ifaceWith(1_006_000)}
	s.Observe(ctx, b)

	// Rebooted: uptime restarted and the counters with it.
	c := snapshot(1, 220, 30)
	c.Interfaces = map[string]collector.Interface{"eth0": ifaceWith(2_000)}
	s.Observe(ctx, c)

	rows := s.Flush(at(600))
	for _, r := range rows {
		if r.Kind != KindIfaceRx {
			continue
		}
		if r.Max > 1000 {
			t.Fatalf("a reboot produced a %v B/s spike; the counter restart was "+
				"treated as traffic", r.Max)
		}
	}
}

func TestObserveTreatsAnInterfaceComingBackAsARestart(t *testing.T) {
	s := testStore()
	ctx := context.Background()

	up := snapshot(1, 100, 1000)
	up.Interfaces = map[string]collector.Interface{"eth0": ifaceWith(1_000_000)}
	s.Observe(ctx, up)

	down := snapshot(1, 160, 1060)
	iface := ifaceWith(1_000_000)
	iface.Up = false
	down.Interfaces = map[string]collector.Interface{"eth0": iface}
	s.Observe(ctx, down)

	// netifd rebuilt it; counters start over. Without this the difference reads
	// as a negative, and on a 32-bit counter it would be "recovered" as a wrap.
	back := snapshot(1, 220, 1120)
	back.Interfaces = map[string]collector.Interface{"eth0": ifaceWith(500)}
	s.Observe(ctx, back)

	after := snapshot(1, 280, 1180)
	after.Interfaces = map[string]collector.Interface{"eth0": ifaceWith(6500)}
	s.Observe(ctx, after)

	rows := s.Flush(at(600))
	for _, r := range rows {
		if r.Kind == KindIfaceRx && r.Max > 1000 {
			t.Fatalf("an interface restart produced a %v B/s spike", r.Max)
		}
	}
}

func TestKindIsRate(t *testing.T) {
	for _, k := range []Kind{KindIfaceRx, KindIfaceTx, KindStaRx, KindStaTx} {
		if !k.IsRate() {
			t.Errorf("%s should be a rate series", k)
		}
	}
	for _, k := range []Kind{KindLoad1, KindStaRSSI, KindAPClients, KindChanBusy} {
		if k.IsRate() {
			t.Errorf("%s should not be a rate series", k)
		}
	}
}

// Channel utilization is the ratio of two counter DELTAS. busy_time and
// active_time do not share an epoch: measured on the reference device, the
// 5 GHz radio read active=24427 against busy=922104 while both advanced
// correctly, so the absolute ratio said 1354% where the truth was 1.7%.
func TestChannelUtilizationUsesDeltasNotAbsolutes(t *testing.T) {
	s := testStore()
	ctx := context.Background()

	// The dangerous shape: absolutes that look plausible and are wrong. Here the
	// absolute ratio is 900000/1000000 = 90%, while the deltas say 25%.
	a := snapshot(1, 100, 1000)
	a.Surveys = []collector.Survey{{Iface: "wlan0", ActiveTime: 1_000_000, BusyTime: 900_000}}
	s.Observe(ctx, a)

	// A single reading produces nothing, exactly like any other counter.
	if rows := s.Flush(at(120)); hasKind(rows, KindChanBusy) {
		t.Fatal("one survey reading produced a utilization sample")
	}

	b := snapshot(1, 160, 1060)
	b.Surveys = []collector.Survey{{Iface: "wlan0", ActiveTime: 1_004_000, BusyTime: 901_000}}
	s.Observe(ctx, b)

	rows := s.Flush(at(600))
	got, ok := findKind(rows, KindChanBusy)
	if !ok {
		t.Fatal("no utilization sample after two readings")
	}
	// 1000ms busy over 4000ms active = 25%, not the 89.7% the absolutes suggest.
	if math.Abs(got.Avg-25) > 0.01 {
		t.Fatalf("utilization = %v%%, want 25%% from the deltas (the absolute "+
			"ratio would give ~89.7%%)", got.Avg)
	}
}

func TestChannelUtilizationRefusesTheImpossible(t *testing.T) {
	s := testStore()
	ctx := context.Background()

	a := snapshot(1, 100, 1000)
	a.Surveys = []collector.Survey{{Iface: "wlan0", ActiveTime: 1000, BusyTime: 1000}}
	s.Observe(ctx, a)

	// busy advancing far faster than active is the counters disagreeing, not a
	// channel that is 500% busy.
	b := snapshot(1, 160, 1060)
	b.Surveys = []collector.Survey{{Iface: "wlan0", ActiveTime: 2000, BusyTime: 6000}}
	s.Observe(ctx, b)

	if rows := s.Flush(at(600)); hasKind(rows, KindChanBusy) {
		v, _ := findKind(rows, KindChanBusy)
		t.Fatalf("an impossible utilization of %v%% was recorded", v.Avg)
	}
}

// A fully saturated channel can read a fraction over 100% because the driver
// samples the two counters a moment apart. Clamping keeps a real reading;
// discarding it would put a hole in the series exactly when it matters most.
func TestChannelUtilizationClampsJitter(t *testing.T) {
	s := testStore()
	ctx := context.Background()

	a := snapshot(1, 100, 1000)
	a.Surveys = []collector.Survey{{Iface: "wlan0", ActiveTime: 1000, BusyTime: 1000}}
	s.Observe(ctx, a)
	b := snapshot(1, 160, 1060)
	b.Surveys = []collector.Survey{{Iface: "wlan0", ActiveTime: 2000, BusyTime: 2030}}
	s.Observe(ctx, b)

	rows := s.Flush(at(600))
	got, ok := findKind(rows, KindChanBusy)
	if !ok {
		t.Fatal("a 103% reading was discarded rather than clamped")
	}
	if got.Avg != 100 {
		t.Fatalf("utilization = %v, want it clamped to 100", got.Avg)
	}
}

func TestChannelUtilizationResetsOnReboot(t *testing.T) {
	s := testStore()
	ctx := context.Background()

	a := snapshot(1, 100, 5000)
	a.Surveys = []collector.Survey{{Iface: "wlan0", ActiveTime: 1_000_000, BusyTime: 500_000}}
	s.Observe(ctx, a)

	// Rebooted: the survey counters restarted with everything else.
	b := snapshot(1, 160, 30)
	b.Surveys = []collector.Survey{{Iface: "wlan0", ActiveTime: 4000, BusyTime: 1000}}
	s.Observe(ctx, b)

	if rows := s.Flush(at(600)); hasKind(rows, KindChanBusy) {
		v, _ := findKind(rows, KindChanBusy)
		t.Fatalf("a reboot produced a utilization sample of %v%%", v.Avg)
	}
}

func hasKind(rows []Rollup, k Kind) bool {
	_, ok := findKind(rows, k)
	return ok
}

func findKind(rows []Rollup, k Kind) (Rollup, bool) {
	for _, r := range rows {
		if r.Kind == k {
			return r, true
		}
	}
	return Rollup{}, false
}
