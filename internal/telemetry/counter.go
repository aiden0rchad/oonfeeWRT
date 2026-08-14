package telemetry

import "math"

// counterState remembers what a counter last read, so the next reading can
// become a rate.
type counterState struct {
	last   uint64
	lastTS int64
	valid  bool

	// wide records that this counter has been observed above 2^32, which proves
	// it is 64-bit. See rate() — it is the one fact that removes the guesswork
	// from a decrease, and it is self-evidencing: no configuration, no probe,
	// just something we either have seen or have not.
	wide bool
}

// rebootState tracks a device's uptime so a restart can be recognised.
type rebootState struct {
	uptime int64
	ts     int64
	valid  bool
}

// maxPlausibleBps bounds a believable interface rate at 20 Gbit/s. Nothing in
// the supported hardware classes comes within an order of magnitude; this exists
// only to reject an arithmetic result that cannot be a real measurement.
const maxPlausibleBps = 2.5e9

// maxWrapBps is the ceiling the WRAP branch is judged against, and it has to be
// much tighter than maxPlausibleBps to do anything at all.
//
// A 32-bit wrap delta is by construction below 2^32, so testing it against
// 2.5e9 B/s can only reject when the two readings are under 1.72 s apart — and
// the fastest configured poll is the focused tier at 5 s. The guard was
// therefore unreachable at every rate the controller actually uses, while its
// comment claimed the opposite ("it bites at the focused rate"). That was wrong
// and this constant is the correction.
//
// 1.25e8 B/s is a saturated gigabit link, the fastest port on any supported
// device. A wrap implying more than that did not happen: at the 5 s focused
// rate a full wrap implies 859 MB/s and is now rejected, and the branch only
// accepts a wrap once the interval is long enough (>34 s) for the traffic to
// have physically fit. Above gigabit hardware this needs raising — with the
// rest of DEVICE-BUDGET §1, which is where link speeds belong.
const maxWrapBps = 1.25e8

// observeUptime records a device's uptime and reports whether it restarted.
//
// This is the primary reset signal, and it is worth preferring to anything
// inferred from the counters themselves: a reboot is exactly what makes every
// counter on the device start again from zero, and uptime says so directly
// instead of being deduced from a number that moved the wrong way.
//
// "Went backwards" is not the only case. A device that rebooted between two
// polls comes back with a small uptime, which may still exceed the previous
// reading if the gap was long — so the test is that uptime advanced by less
// than the wall-clock time between polls, with a tolerance for the fact that
// neither clock is exact.
func (s *Store) observeUptime(deviceID, uptime, ts int64) bool {
	st := s.reboots[deviceID]
	if st == nil {
		st = &rebootState{}
		s.reboots[deviceID] = st
	}
	prev, prevTS, had := st.uptime, st.ts, st.valid
	st.uptime, st.ts, st.valid = uptime, ts, true
	if !had {
		return false
	}
	elapsed := ts - prevTS
	if elapsed < 0 {
		elapsed = 0
	}
	// A 10% slop plus 5 seconds absorbs the difference between the controller's
	// clock and the router's tick without letting a real reboot through: a
	// restart drops uptime by far more than a rounding error.
	tolerance := elapsed/10 + 5
	return uptime < prev+elapsed-tolerance
}

// rate converts a raw counter reading into a per-second rate.
//
// It returns ok=false whenever the honest answer is "no measurement": the first
// reading of a counter, a reading after the device rebooted, and any decrease
// that cannot be explained. A gap in a graph is a fact. A fabricated spike is a
// lie that looks like a fact, and once it is in a rollup nothing downstream can
// tell the difference.
//
// The awkward case is a counter that decreased. Two things cause it — a 32-bit
// counter wrapping, or the counter resetting — and they call for opposite
// handling. At 1 Gbit/s a 32-bit byte counter wraps every 34 seconds, so
// discarding wraps would destroy the throughput series on exactly the links
// anyone cares about; treating a reset as a wrap invents a 4 GB burst.
//
// They are separated with evidence rather than a guess, in this order:
//
//  1. If the device rebooted, it is a reset. Uptime said so.
//  2. If this counter has ever exceeded 2^32, it is 64-bit and cannot have
//     wrapped — a 64-bit byte counter needs centuries at 10 Gbit/s. Reset.
//  3. If the interface was down and came back, netifd recreated it. Reset.
//  4. Otherwise assume a 32-bit wrap, unless the implied rate is impossible.
//
// Step 4 is the residual guess and it is stated plainly. The wrap is accepted
// only if the traffic it implies could physically have crossed a gigabit link
// in the elapsed time (maxWrapBps), so a wrap is rejected outright below ~34 s
// between readings — which covers the whole focused tier. At the 60 s baseline
// a full wrap implies 572 Mbit/s, which a gigabit link genuinely could carry,
// so a reset there is still indistinguishable from a wrap. That is the residue,
// and it is one interval wide.
//
// The counter width on the reference WRT3200ACM could not be determined without
// pushing 3 GB through it, so this is written to be correct either way rather
// than tuned to one.
func (s *Store) rate(k SeriesKey, ts int64, counter uint64, rebooted, recreated bool) (float64, bool) {
	st := s.counters[k]
	if st == nil {
		st = &counterState{}
		s.counters[k] = st
	}
	if counter > math.MaxUint32 {
		st.wide = true
	}

	// Rebase without emitting. The next poll produces the first real rate.
	rebase := func() (float64, bool) {
		st.last, st.lastTS, st.valid = counter, ts, true
		return 0, false
	}
	if rebooted || recreated || !st.valid {
		return rebase()
	}

	dt := ts - st.lastTS
	if dt <= 0 {
		// Two readings with the same timestamp say nothing about a rate, and
		// dividing by the gap would be a division by zero dressed up as data.
		return rebase()
	}

	var delta uint64
	switch {
	case counter >= st.last:
		delta = counter - st.last
	case st.wide:
		// A 64-bit counter that went backwards was reset by something we did not
		// observe — an interface torn down and rebuilt, a driver reload.
		return rebase()
	default:
		delta = (1 << 32) - st.last + counter
		// Judged against a real link rate, not the 20 Gbit/s sanity ceiling: a
		// wrap delta is always below 2^32, so the loose bound could never
		// reject one at any poll interval the controller uses.
		if float64(delta)/float64(dt) > maxWrapBps {
			return rebase()
		}
	}

	st.last, st.lastTS = counter, ts
	r := float64(delta) / float64(dt)
	if r > maxPlausibleBps {
		// Reachable on a genuine 64-bit counter if the device swapped an
		// interface underneath the same name. Refuse it rather than record it.
		return 0, false
	}
	return r, true
}

// expireStale drops counter and ratio baselines that have not been updated for
// `before`.
//
// They cannot ride on series retirement, which is what an earlier version
// assumed. rate() creates a counterState on a counter's FIRST reading, and that
// reading produces no sample and therefore no ring — so a series whose rate
// never became valid has a baseline and no ring, and the retirement sweep walks
// rings. The `iface_up` pseudo-key is never a series at all.
//
// The leak is not only memory. A station that appears once, leaves, and returns
// an hour later with its counters reset would be measured against the stale
// baseline: the decrease takes the wrap branch and emits traffic that never
// happened.
func (s *Store) expireStale(now, before int64) {
	for k, st := range s.counters {
		// iface_up carries no timestamp; it is refreshed on every poll of a
		// live interface, so age it from the device's other counters instead of
		// keeping it forever.
		if st.lastTS == 0 || st.lastTS < before {
			delete(s.counters, k)
		}
	}
	for k, st := range s.ratios {
		if st.lastTS < before {
			delete(s.ratios, k)
		}
	}
	_ = now
}

// forgetCounters drops every counter baseline for a device, so the next poll
// starts clean. Called when the device restarted: every counter on it did too.
func (s *Store) forgetCounters(deviceID int64) {
	for k := range s.counters {
		if k.DeviceID == deviceID {
			delete(s.counters, k)
		}
	}
	for k := range s.ratios {
		if k.DeviceID == deviceID {
			delete(s.ratios, k)
		}
	}
}

// ratioState remembers the previous reading of a counter PAIR.
type ratioState struct {
	num, den uint64
	lastTS   int64
	valid    bool
}

// ratio converts two monotonic counters into a percentage over the interval
// between readings.
//
// This exists because channel utilization is not a gauge, however much it looks
// like one. iwinfo.survey reports busy_time and active_time as counters that do
// not share an epoch: measured on the reference device, the 5 GHz radio read
// active=24427 against busy=922104 while both advanced correctly. Dividing the
// absolutes gave 1354%. Dividing the deltas gave 1.7%.
//
// The 2.4 GHz radio is why this matters more than the obvious case: there the
// absolute ratio produced 25.9% — entirely plausible — against a true 73.3%,
// confirmed by hostapd's independent BSS-load reading of 70% on the same radio
// at the same moment.
//
// Like every other counter path here, the first reading and any reading after a
// reset produce nothing rather than a fabricated value.
func (s *Store) ratio(k SeriesKey, ts int64, num, den uint64, rebooted bool) (float64, bool) {
	st := s.ratios[k]
	if st == nil {
		st = &ratioState{}
		s.ratios[k] = st
	}
	prevNum, prevDen, had := st.num, st.den, st.valid
	st.num, st.den, st.valid, st.lastTS = num, den, true, ts

	if rebooted || !had {
		return 0, false
	}
	// Either counter going backwards means the pair was reset; a denominator
	// that did not advance means no time passed to measure over.
	if num < prevNum || den <= prevDen {
		return 0, false
	}
	pct := float64(num-prevNum) * 100 / float64(den-prevDen)
	switch {
	case pct < 0:
		return 0, false
	case pct <= 100:
		return pct, true
	case pct <= 110:
		// The two counters are sampled a moment apart inside the driver, so a
		// fully saturated channel can read slightly over. Clamp rather than
		// discard a real reading of "busy the whole time".
		return 100, true
	default:
		// Further than jitter explains. Refuse: a utilization above 110% is the
		// counters disagreeing, and recording it would put an impossible value
		// in a series someone will later average.
		return 0, false
	}
}
