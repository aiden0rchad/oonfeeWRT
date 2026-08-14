package collector

// Attributable device CPU (DEVICE-BUDGET §7's "CPU percent attributable to
// oonfeeWRT").
//
// # Why this is a measured constant and not a live sample
//
// The obvious implementation — read the device's CPU counters and report our
// share — cannot work, and it took measuring to see why. On the reference
// device a baseline poll costs about 5 ms of CPU once a minute, which is
// 0.009% of one core. The device's own idle busy time is 0.38–0.43%, so what
// we are trying to measure is roughly fifty times smaller than the floor it
// would have to be measured against, and thousands of times smaller than the
// jitter in that floor between one minute and the next. A live sample would be
// reporting noise with a decimal point on it.
//
// So the number comes from a control experiment instead: measure the device's
// CPU over a window with nothing polling, measure it again over a window with
// a known number of polls, and attribute the difference. That is a real
// measurement of a real quantity — it is simply not one the controller can take
// while doing its normal job.
//
// # What was measured
//
// Reference device, class A (Linksys WRT3200ACM, mvebu, dual-core ARMv7,
// OpenWrt 25.12.5), 2026-08-14, 60-second windows, `/proc/stat` read over SSH:
//
//	control (nothing polling)          0.38–0.43% busy
//	baseline set, 8 invocations        5.33 ms CPU per poll
//	focused set, 12 invocations        6.65 ms CPU per poll
//
// Checked for linearity rather than assumed: at 6,049 polls/minute the cost
// came out 4.56 ms/poll and at 372 polls/minute 4.38 ms/poll — within 4%, so
// the figure is not an artefact of saturating the device and extrapolates down
// to the shipped rate honestly.
//
// The surprise worth recording: the focused poll costs only 1.25× the baseline
// poll in CPU, even though DEVICE-BUDGET §4 measures iwinfo as ~92% of a
// focused poll. That 92% is *latency* — `iwinfo.survey` and `iwinfo.assoclist`
// block on the wireless driver rather than burning cycles. **The call that
// dominates a poll's wall time is not the call that dominates its CPU cost**,
// and the two must not be used interchangeably when reasoning about device
// load.
//
// # Why it is per class
//
// These are class-A numbers and are reported only for class-A devices. Class C
// (MT7621) is the class that sets the budget and has never been measured; a
// class-A figure shown against a class-C device would be a guess wearing a
// measurement's clothes. Unmeasured classes report no figure and say why —
// the same rule as everywhere else, that a value nobody has established is
// absent rather than approximated.

// cpuPerPoll is device CPU milliseconds per poll, by device class and tier.
// Absent means never measured on that class.
var cpuPerPoll = map[string]map[Tier]float64{
	"A": {
		Baseline: 5.33,
		Focused:  6.65,
	},
}

// CPUBasis describes where an attributable-CPU figure came from, so a derived
// number is never mistaken for a live reading.
const CPUBasis = "derived from a control measurement on class-A hardware " +
	"(5.33 ms of device CPU per baseline poll, 6.65 ms per focused poll, " +
	"2026-08-14) applied to this device's actual poll rate — not sampled live, " +
	"because the quantity is ~50x below the device's own idle CPU and could " +
	"not be distinguished from noise"

// CPUUnmeasured is the reason given when the device's class has no measurement.
const CPUUnmeasured = "the attributable-CPU cost of a poll has only been " +
	"measured on class A; reporting that figure for another class would be a " +
	"guess presented as a measurement"

// cpuCost returns the per-poll CPU cost for a class and tier, and whether it
// has been measured at all.
func cpuCost(class string, tier Tier) (float64, bool) {
	byTier, ok := cpuPerPoll[class]
	if !ok {
		return 0, false
	}
	ms, ok := byTier[tier]
	return ms, ok
}
