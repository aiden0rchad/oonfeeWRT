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
// Step 4 is the residual guess and it is stated plainly: at a 60-second poll
// interval a full 2^32 wrap implies 572 Mbit/s, which is entirely plausible, so
// the plausibility check cannot catch a reset there. It bites at the focused
// rate, where the same arithmetic implies 6.9 Gbit/s. The counter width on the
// reference WRT3200ACM could not be determined without pushing 3 GB through it,
// so this code is written to be correct either way rather than tuned to one.
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
		if float64(delta)/float64(dt) > maxPlausibleBps {
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

// forgetCounters drops every counter baseline for a device, so the next poll
// starts clean. Called when the device restarted: every counter on it did too.
func (s *Store) forgetCounters(deviceID int64) {
	for k := range s.counters {
		if k.DeviceID == deviceID {
			delete(s.counters, k)
		}
	}
}
