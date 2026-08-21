// Package telemetry turns polls into series: a raw sample ring in RAM, drained
// every five minutes into SQLite rollups.
//
// The write shape is decision D4, and it is not negotiable. Raw samples never
// reach the database; one transaction per five-minute flush does. That was
// designed for NAND survival on a router and kept when the controller moved to
// a container, because it is simply the right shape — it keeps SQLite happy and
// makes the flush path testable. Do not "improve" it into per-sample inserts.
//
// Two rules the rest of the package exists to enforce:
//
//   - The device computes nothing. Rates, percentages and derived metrics are
//     all produced here, from raw readings, on hardware that has cycles.
//   - Counters are never stored. An interface byte counter is meaningless
//     without its predecessor, and storing raw counters would push the
//     wrap-and-reset problem onto every future reader of the data. Rates are
//     computed at ingest, once, where the previous reading and the device's
//     uptime are both in hand.
package telemetry

import (
	"sort"
	"sync"
	"time"
)

// Kind identifies what a series measures. The set is closed: a screen that
// needs a new number needs a new kind here and a mapping in observe.go, not an
// ad-hoc query somewhere.
type Kind string

const (
	// Device-level, always collected (baseline tier).
	KindLoad1     Kind = "sys_load1"      // 1-minute load average
	KindMemUsed   Kind = "sys_mem_used"   // bytes in use, kernel's own "available" preferred
	KindMemPct    Kind = "sys_mem_pct"    // percent of total in use
	KindIfaceRx   Kind = "iface_rx_bps"   // bytes/sec, keyed by interface
	KindIfaceTx   Kind = "iface_tx_bps"   // bytes/sec, keyed by interface
	KindAPClients Kind = "ap_clients"     // associated stations, keyed by interface
	KindAPAirtime Kind = "ap_airtime_pct" // BSS load as a percentage, keyed by interface

	// Radio and per-station, focused tier only.
	KindChanBusy Kind = "chan_busy_pct" // channel utilization, keyed by interface
	KindStaRSSI  Kind = "sta_rssi"      // dBm, keyed by station MAC
	KindStaRx    Kind = "sta_rx_bps"    // bytes/sec, keyed by station MAC
	KindStaTx    Kind = "sta_tx_bps"    // bytes/sec, keyed by station MAC
	KindStaRetry Kind = "sta_retry_pct" // TX retries as a percentage of TX packets

	// Phase 4 quality series. Counter-derived ratios are deltas over one
	// observation interval; wifi-v1 is a fixed-arity score and is absent unless
	// every required input is known. It is never reweighted around a missing
	// component, which keeps scores comparable across devices and time.
	KindStaRetryDelta       Kind = "sta_retry_delta_pct"
	KindStaTXFailDelta      Kind = "sta_tx_fail_delta_pct"
	KindStaExperienceWiFiV1 Kind = "sta_experience_wifi_v1"

	// Radio series are keyed by the stable UCI wifi-device section (radio0,
	// radio1, ...), never a runtime phy/interface name.
	KindRadioUtilization  Kind = "radio_utilization_pct"
	KindRadioInterference Kind = "radio_interference_pct"
	KindRadioRXAirtime    Kind = "radio_rx_airtime_pct"
	KindRadioTXAirtime    Kind = "radio_tx_airtime_pct"
	KindRadioNoise        Kind = "radio_noise_dbm"
	KindRadioRetryDelta   Kind = "radio_retry_delta_pct"
	KindRadioTXFailDelta  Kind = "radio_tx_fail_delta_pct"
	KindRadioSignalAvg    Kind = "radio_signal_avg_dbm"

	// Site-wide WAN health. Key is empty; the selected gateway is provenance in
	// the sample/query response rather than an interface-name identity.
	KindSiteWANLatency Kind = "site_wan_latency_ms"
	KindSiteWANLoss    Kind = "site_wan_loss_pct"
	KindSiteWANUp      Kind = "site_wan_up"
)

// IsRate reports a series derived from a counter difference rather than read
// directly. Rate series have no meaningful value for a single poll, which is
// why the first sample after a gap or a reboot produces nothing at all.
func (k Kind) IsRate() bool {
	switch k {
	case KindIfaceRx, KindIfaceTx, KindStaRx, KindStaTx:
		return true
	}
	return false
}

// SeriesKey identifies one series. Key is empty for device-wide series and
// carries an interface name or a station MAC otherwise.
type SeriesKey struct {
	DeviceID int64
	Kind     Kind
	Key      string
}

// Sample is one raw reading. float32 halves the ring's memory for a precision
// that no metric here comes close to needing — an RSSI of −52 dBm and a rate of
// 118 MB/s both survive it exactly.
type Sample struct {
	TS int64
	V  float32
}

// Rollup is one aggregated window, ready for the database.
type Rollup struct {
	SeriesKey
	TS  int64 // window start, aligned
	Avg float64
	Min float64
	Max float64
	Cnt int
}

// Options tune the store.
type Options struct {
	// Window is the rollup period. IMPLEMENTATION §3 fixes this at 5 minutes;
	// it is settable so tests do not have to.
	Window time.Duration

	// Capacity is the per-series ring depth. It must exceed one window's worth
	// of samples at the fastest poll rate, or a flush that runs late silently
	// loses the oldest readings.
	Capacity int

	// IdleWindows is how many consecutive empty windows retire a series. A
	// station that disconnects would otherwise keep its ring forever, and a busy
	// network churns through client MACs.
	IdleWindows int
}

// Defaults sized against the poll rates in DEVICE-BUDGET §4.2: a 5-minute
// window at the 5-second focused rate is 60 samples, so 128 leaves room for a
// flush that runs late without dropping the oldest reading.
const (
	DefaultWindow      = 5 * time.Minute
	DefaultCapacity    = 128
	DefaultIdleWindows = 3
)

func (o Options) withDefaults() Options {
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.Capacity <= 0 {
		o.Capacity = DefaultCapacity
	}
	if o.IdleWindows <= 0 {
		o.IdleWindows = DefaultIdleWindows
	}
	return o
}

// Store is the in-RAM sample ring for every live series.
//
// Safe for concurrent use: one goroutine per device writes into it and the
// maintenance tick drains it.
type Store struct {
	opts Options

	mu       sync.Mutex
	series   map[SeriesKey]*ring
	counters map[SeriesKey]*counterState
	ratios   map[SeriesKey]*ratioState
	quality  map[stationQualityKey]stationQualityState
	reboots  map[int64]*rebootState
}

// New returns an empty Store.
func New(opts Options) *Store {
	return &Store{
		opts:     opts.withDefaults(),
		series:   map[SeriesKey]*ring{},
		counters: map[SeriesKey]*counterState{},
		ratios:   map[SeriesKey]*ratioState{},
		quality:  map[stationQualityKey]stationQualityState{},
		reboots:  map[int64]*rebootState{},
	}
}

// Gauge records a value that was read directly.
func (s *Store) Gauge(k SeriesKey, ts int64, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLocked(k, ts, v)
}

func (s *Store) appendLocked(k SeriesKey, ts int64, v float64) {
	r := s.series[k]
	if r == nil {
		r = newRing(s.opts.Capacity)
		s.series[k] = r
	}
	r.append(Sample{TS: ts, V: float32(v)})
}

// Window is the rollup period this store aggregates into. The maintainer reads
// it rather than keeping its own copy: a tick period and a window that disagree
// produce a flush that silently drains nothing, which looks exactly like a
// working system with no data.
func (s *Store) Window() time.Duration { return s.opts.Window }

// Len reports how many live series the store holds, for the Management
// Overhead readout and for tests.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.series)
}

// ForgetDevice removes every in-memory sample and derivation baseline owned by
// a device. SQLite may reuse an INTEGER PRIMARY KEY after un-adoption; keeping
// any state under that number would attach the removed router's history to the
// next router that receives it.
func (s *Store) ForgetDevice(deviceID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.series {
		if k.DeviceID == deviceID {
			delete(s.series, k)
		}
	}
	s.forgetCounters(deviceID)
	delete(s.reboots, deviceID)
}

// Flush drains every window that has completely elapsed and returns the
// rollups, sorted for a deterministic write order.
//
// Only completed windows: the one `now` falls inside is still filling, and
// emitting it would write a partial average that a later flush could not
// correct without a read-modify-write on every tick.
func (s *Store) Flush(now time.Time) []Rollup {
	cutoff := now.Truncate(s.opts.Window).Unix()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Age out baselines that no series retirement can reach: a counter whose
	// first reading never produced a sample has state but no ring, and the
	// iface_up pseudo-key is never a series at all.
	s.expireStale(now.Unix(), now.Add(-time.Duration(s.opts.IdleWindows)*s.opts.Window).Unix())

	var out []Rollup
	for k, r := range s.series {
		rolled := r.drain(cutoff, int64(s.opts.Window.Seconds()))
		for _, w := range rolled {
			w.SeriesKey = k
			out = append(out, w)
		}
		if len(rolled) > 0 {
			r.idle = 0
			continue
		}
		// Retire a series that has produced nothing for several windows, along
		// with any counter baseline it left behind. Keeping them is a slow leak
		// keyed by client MAC, which is the one key guaranteed to churn.
		if r.empty() {
			r.idle++
			if r.idle >= s.opts.IdleWindows {
				delete(s.series, k)
				delete(s.counters, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS < out[j].TS
		}
		if out[i].DeviceID != out[j].DeviceID {
			return out[i].DeviceID < out[j].DeviceID
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ring is a fixed-size circular buffer of samples for one series.
type ring struct {
	buf  []Sample
	head int // next write position
	n    int // live samples, <= len(buf)
	idle int // consecutive flushes that produced nothing
}

func newRing(capacity int) *ring { return &ring{buf: make([]Sample, capacity)} }

func (r *ring) empty() bool { return r.n == 0 }

func (r *ring) append(s Sample) {
	r.buf[r.head] = s
	r.head = (r.head + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
	// At capacity the oldest sample is overwritten. That is the intended
	// behaviour — a stalled flush must not grow memory without bound — and the
	// capacity is chosen so it cannot happen during normal operation.
}

// samples returns the live samples in chronological order.
func (r *ring) samples() []Sample {
	out := make([]Sample, 0, r.n)
	start := (r.head - r.n + len(r.buf)) % len(r.buf)
	for i := range r.n {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

// drain aggregates and removes every sample older than cutoff, grouped into
// windows of windowSec seconds.
func (r *ring) drain(cutoff, windowSec int64) []Rollup {
	if r.n == 0 || windowSec <= 0 {
		return nil
	}
	all := r.samples()
	byWindow := map[int64][]Sample{}
	var kept []Sample
	for _, smp := range all {
		start := smp.TS - smp.TS%windowSec
		if start >= cutoff {
			kept = append(kept, smp)
			continue
		}
		byWindow[start] = append(byWindow[start], smp)
	}
	if len(byWindow) == 0 {
		return nil
	}

	starts := make([]int64, 0, len(byWindow))
	for start := range byWindow {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	out := make([]Rollup, 0, len(starts))
	for _, start := range starts {
		smps := byWindow[start]
		w := Rollup{TS: start, Cnt: len(smps)}
		sum := 0.0
		for i, smp := range smps {
			v := float64(smp.V)
			sum += v
			if i == 0 || v < w.Min {
				w.Min = v
			}
			if i == 0 || v > w.Max {
				w.Max = v
			}
		}
		w.Avg = sum / float64(len(smps))
		out = append(out, w)
	}

	// Rewrite the ring with only what is still accumulating.
	r.head, r.n = 0, 0
	for _, smp := range kept {
		r.append(smp)
	}
	return out
}
