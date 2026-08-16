// Package roaming computes what each access point should be told about its
// neighbours.
//
// # What this is for
//
// 802.11k lets a client ask its current AP "what else is around?" and get back
// a list of candidate BSSes with their channels and operating classes. The
// client then scans those channels instead of all of them, which is the
// difference between a roam that takes tens of milliseconds and one that takes
// long enough to drop a call.
//
// An AP cannot discover that list by itself. It knows its own BSS and nothing
// about the AP down the hall — they do not talk to each other. Something has to
// know the whole fleet and tell each member about the others, and a controller
// is the only component in the system that does.
//
// The renderer has been setting `ieee80211k=1` and `rrm_neighbor_report=1` on
// every managed AP since Phase 2, which makes each one advertise that it will
// answer the question. Measured on both reference devices, every one of those
// APs answered with an empty list. This package is what fills it.
//
// # Why the controller relays and never constructs
//
// A neighbour report element is a packed structure: BSSID, a bitfield of BSS
// capabilities, operating class, channel, PHY type, and optional subelements.
// Getting the operating class right alone means mapping a frequency and
// bandwidth through a regulatory table.
//
// hostapd already computes that for its own BSS and hands it over verbatim
// through `rrm_nr_get_own`. So the controller reads each AP's own element and
// relays the bytes untouched. It never parses or builds one. That is not
// laziness — a controller that built the element itself would be a second
// implementation of a regulatory mapping, silently disagreeing with the AP's
// own on exactly the bands where it matters.
//
// # Why this is not part of the render/apply pipeline
//
// Everything else the controller writes is UCI, and survives a reboot because
// it is on disk. The neighbour list is not: `rrm_nr_set` writes hostapd's
// *runtime* state, and there is no UCI option that carries it.
//
// That is not a defect to work around. A neighbour list is derived from where
// the other APs currently are, so a stale one written to flash would be worse
// than none — an AP would confidently send a client to a BSS that moved
// channels a month ago. Recomputing it from live observation is the correct
// shape, and it means this reconciles rather than applies: no rollback window,
// no confirm, nothing at risk if it fails, because the worst case is the empty
// list the AP already had.
package roaming

import (
	"fmt"
	"sort"
	"strings"
)

// Neighbour is one BSS as its own AP describes it.
//
// NR is opaque on purpose — see the package comment. It is whatever
// `rrm_nr_get_own` returned, relayed unmodified.
type Neighbour struct {
	DeviceID int64  // which adopted device carries this BSS
	Iface    string // hostapd object suffix, e.g. "phy0-ap1"
	BSSID    string
	SSID     string
	NR       string // hex, exactly as the device reported it
}

// Valid reports whether an observation is complete enough to hand to another
// AP.
//
// A partial entry is worse than a missing one. An element with an empty NR
// makes an AP answer a client's neighbour request with a candidate it cannot
// scan for, and the client spends the roam it was trying to avoid.
func (n Neighbour) Valid() bool {
	return n.BSSID != "" && n.SSID != "" && n.NR != ""
}

// Target identifies one BSS to push a list to.
type Target struct {
	DeviceID int64
	Iface    string
}

func (t Target) String() string { return fmt.Sprintf("%d/%s", t.DeviceID, t.Iface) }

// Distribute computes, for every observed BSS, the neighbours it should be
// told about.
//
// The rules, each of which has a test:
//
//   - Neighbours are grouped by SSID, compared byte for byte. Two APs with
//     different SSIDs are not neighbours in any useful sense: a client cannot
//     roam between them without a full reassociation to a different network,
//     so listing one under the other would send it somewhere it cannot go.
//
//   - A BSS never lists itself. 802.11k asks an AP about its *neighbours*, and
//     an AP that names itself invites a client to consider roaming to where it
//     already is.
//
//   - The other band of the same device IS a neighbour. Band steering between
//     two radios of one AP is a real and common roam, and excluding it would
//     leave a client on a congested 2.4 GHz radio unaware that the same AP
//     offers 5 GHz.
//
//   - Every observed BSS gets an entry, including one whose list comes out
//     empty. An empty list is a real instruction: it is how the last remaining
//     AP of an SSID gets told that the neighbour it used to have is gone.
//     Omitting it would leave a decommissioned AP in a live neighbour list
//     until the surviving AP happened to restart.
//
//   - Incomplete observations are dropped rather than relayed.
//
// The result is sorted by BSSID so that two runs over the same fleet produce
// byte-identical lists. That is what makes "has this changed?" answerable
// without pushing, which is the whole of the request budget for this feature.
func Distribute(observed []Neighbour) map[Target][]Neighbour {
	bySSID := map[string][]Neighbour{}
	for _, n := range observed {
		if !n.Valid() {
			continue
		}
		bySSID[n.SSID] = append(bySSID[n.SSID], n)
	}

	out := make(map[Target][]Neighbour, len(observed))
	for _, group := range bySSID {
		sort.Slice(group, func(i, j int) bool {
			return group[i].BSSID < group[j].BSSID
		})
		for _, self := range group {
			peers := make([]Neighbour, 0, len(group))
			for _, other := range group {
				if sameBSS(self, other) {
					continue
				}
				peers = append(peers, other)
			}
			out[Target{DeviceID: self.DeviceID, Iface: self.Iface}] = peers
		}
	}
	return out
}

// sameBSS decides what counts as "itself".
//
// BSSID is the identity that matters, not the interface name: `phy0-ap1` means
// nothing outside one device, and two devices both have one. Comparison is
// case-insensitive because a MAC is not case-sensitive and nothing guarantees
// two sources of one address agree on the case — getting that wrong makes an AP
// list itself, which is invisible until a client acts on it.
func sameBSS(a, b Neighbour) bool {
	return strings.EqualFold(a.BSSID, b.BSSID)
}

// SameSet reports whether two neighbour lists carry the same entries.
//
// Order-insensitive, and that is a measurement rather than a preference:
// hostapd returns `rrm_nr_list` in its own storage order, which on both
// reference devices is neither insertion order nor sorted. An
// order-sensitive comparison would therefore report every list as different on
// every cycle and push a needless `rrm_nr_set` to every AP forever — a
// reconciler that never converges, which is indistinguishable from a broken one
// except that it also costs the request budget.
//
// Entries are compared on all three fields. A BSSID whose NR element changed —
// a radio moved channel — is a different neighbour even though it is the same
// AP, and that is precisely the change a client needs to be told about.
func SameSet(a, b []Neighbour) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, n := range a {
		seen[entryKey(n)]++
	}
	for _, n := range b {
		k := entryKey(n)
		seen[k]--
		if seen[k] < 0 {
			return false
		}
	}
	return true
}

// entryKey is the wire identity of one entry: what actually reaches the AP.
// DeviceID and Iface are the controller's bookkeeping and are deliberately
// excluded — a BSS that moved between devices in our records is the same
// neighbour to a client, and re-pushing on that would be churn with no effect.
func entryKey(n Neighbour) string {
	return strings.ToLower(n.BSSID) + "\x00" + n.SSID + "\x00" + strings.ToLower(n.NR)
}
