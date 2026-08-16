// Package onair answers a question no other check in this controller can:
// is a configured BSS actually being transmitted?
//
// # Why this exists
//
// Measured on 2026-08-16, and it is the most expensive lesson this project has
// paid for. A WRT3200ACM beaconed `wrt-cleanroom` — an SSID present in NO
// configuration anywhere on the device — for roughly fourteen hours, while
// `/etc/config/wireless`, hostapd's running configuration, `iwinfo`, ubus AND
// the kernel's own `iw dev info` all reported `oonfee-roam`. A `wifi reload`
// did not clear it; only a hard reset did.
//
// Every verification this controller had was on the wrong side of the driver.
// "Hardware-verified" meant "hostapd agreed with us", and hostapd agreed with
// us the entire time the radio was transmitting something else.
//
// # The only source that can answer it
//
// A second radio. One AP's scan can hear another AP's beacons, and a beacon is
// the physical thing — it cannot be produced by a stale config or a confused
// daemon. So this cross-checks the fleet against itself: each device scans, and
// what it hears is compared with what the others claim to be broadcasting.
//
// # Why "not heard" is never "not transmitting"
//
// This is the whole design, and getting it wrong would make the feature worse
// than useless — it would report healthy APs as dead. A scan can miss a live
// BSS for at least four reasons measured or known here:
//
//   - **The scanning radio was serving an AP and could not go off-channel.**
//     Measured on the reference C6: its 2.4 GHz radio returned 20 BSSes while
//     its 5 GHz radio, also serving an AP, returned zero. Not a quiet band —
//     a scan that never happened.
//   - **Distance.** Two APs deliberately placed for coverage may not hear each
//     other at all. That is a correct deployment, not a fault.
//   - **The bands do not overlap.** A 2.4 GHz radio cannot hear a 5 GHz BSS.
//   - **Timing.** A scan is a sample; a beacon is every 100 ms, but a scan that
//     dwells briefly on each channel can still miss one.
//
// So the verdict is three-state like everything else here, and the negative
// state is `Unheard` rather than `Absent`. Only one combination is ever
// reported as a fault: a BSS that another radio DID hear, on the right band,
// **broadcasting a different SSID than its own device claims**. That is not
// ambiguous — it is the exact signature of the fourteen-hour failure, and no
// distance or timing explanation covers it.
package onair

import (
	"sort"
	"strings"
)

// Verdict is what could be established about one configured BSS.
type Verdict string

const (
	// Confirmed — another radio heard this BSSID with the expected SSID. The
	// only state that proves a BSS is genuinely on the air.
	Confirmed Verdict = "confirmed"
	// Mismatched — another radio heard this BSSID broadcasting a DIFFERENT
	// SSID than its own device reports. A fault, and the reason this package
	// exists.
	Mismatched Verdict = "mismatched"
	// Unheard — no radio reported this BSSID. NOT a fault: distance, a
	// non-overlapping band, an AP-busy radio that could not scan, or a scan
	// that simply missed it all produce this.
	Unheard Verdict = "unheard"
	// NotChecked — nothing was in a position to hear it. A single-AP site, or
	// every other radio on a different band, or no scan succeeded at all.
	NotChecked Verdict = "not-checked"
)

// BSS is one broadcasting interface as its OWN device describes it.
type BSS struct {
	DeviceID int64
	Name     string // device name, for the report
	Iface    string
	BSSID    string
	SSID     string
	// Band is used to decide which scanners could plausibly have heard it. A
	// 2.4 GHz radio hearing nothing on 5 GHz is not evidence of anything.
	Band string
}

// Heard is one BSS observed in a scan.
type Heard struct {
	BSSID string
	SSID  string
	// ScannerID is the device whose radio heard it, so a report can say who
	// the witness was — "nobody heard it" and "only the device next to it
	// heard it" are different levels of confidence.
	ScannerID int64
	Band      string
}

// Scan is one device's scan result, and whether it worked at all.
type Scan struct {
	DeviceID int64
	Name     string
	// Bands the scan actually covered. A radio that could not scan contributes
	// no bands, which is what stops its silence being read as evidence.
	BandsCovered []string
	Heard        []Heard
	// Err is why a scan produced nothing, when that is known.
	Err string
}

// Result is one BSS's verdict.
type Result struct {
	BSS     BSS
	Verdict Verdict
	// HeardSSID is what was actually on the air, when that differs.
	HeardSSID string
	// Witnesses are the devices that heard this BSSID.
	Witnesses []int64
	Reason    string
}

// Fault reports whether this result is something to act on.
//
// Only Mismatched qualifies. Unheard and NotChecked are absences of evidence,
// and rendering an absence of evidence as a fault is how a screen trains
// somebody to ignore it.
func (r Result) Fault() bool { return r.Verdict == Mismatched }

// Check compares what the fleet claims to broadcast against what it heard.
func Check(claimed []BSS, scans []Scan) []Result {
	// Index every observation by BSSID, lower-cased: a MAC is not
	// case-sensitive and two sources of one address need not agree on case.
	heardBy := map[string][]Heard{}
	bandsCovered := map[string]bool{}
	for _, s := range scans {
		for _, b := range s.BandsCovered {
			bandsCovered[b] = true
		}
		for _, h := range s.Heard {
			key := strings.ToLower(h.BSSID)
			h.ScannerID = s.DeviceID
			heardBy[key] = append(heardBy[key], h)
		}
	}

	out := make([]Result, 0, len(claimed))
	for _, c := range claimed {
		r := Result{BSS: c}
		obs := heardBy[strings.ToLower(c.BSSID)]

		switch {
		case len(obs) > 0:
			// Heard. The only question left is whether it is broadcasting what
			// its device says it is.
			var mismatch string
			for _, o := range obs {
				if !strings.EqualFold(o.SSID, c.SSID) {
					mismatch = o.SSID
				}
				r.Witnesses = append(r.Witnesses, o.ScannerID)
			}
			sort.Slice(r.Witnesses, func(i, j int) bool { return r.Witnesses[i] < r.Witnesses[j] })
			if mismatch != "" {
				r.Verdict, r.HeardSSID = Mismatched, mismatch
				r.Reason = c.Iface + " reports it is broadcasting " + c.SSID +
					" and another radio hears " + mismatch + " from that same " +
					"BSSID. The device's configuration, its daemon and its " +
					"kernel can all agree and still be wrong about what the " +
					"radio is transmitting — this is the one check that can " +
					"tell. A restart of the wireless stack usually does not " +
					"clear it; a reboot does"
				break
			}
			r.Verdict = Confirmed
			r.Reason = c.SSID + " confirmed on the air by " +
				plural(len(obs), "other radio", "other radios")

		case !bandsCovered[c.Band]:
			// Nothing scanned this band, so silence says nothing at all.
			r.Verdict = NotChecked
			r.Reason = "no other radio scanned the " + c.Band + " band, so " +
				"whether this is on the air was not established. A radio that " +
				"is serving an access point often cannot scan"

		default:
			r.Verdict = Unheard
			r.Reason = "no other radio heard this BSS, which is not the same as " +
				"it being off the air: access points placed for coverage " +
				"routinely cannot hear each other, and a scan is a sample. " +
				"Treat this as unverified rather than broken"
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BSS.DeviceID != out[j].BSS.DeviceID {
			return out[i].BSS.DeviceID < out[j].BSS.DeviceID
		}
		return out[i].BSS.Iface < out[j].BSS.Iface
	})
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return itoa(n) + " " + many
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
