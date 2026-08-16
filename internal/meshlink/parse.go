package meshlink

import (
	"strconv"
	"strings"
)

// Parsing `iw dev <iface> station dump`.
//
// # Why this text and not a ubus object
//
// There is no ubus source for the one field that turns a peer count into a
// health reading. `iwinfo.assoclist` returns the same peers as structured JSON
// and is already granted — and it carries no `mesh plink`. A peer stuck at
// OPN_SNT has an assoclist entry indistinguishable from an established one, so
// a count built on it calls a dead backhaul healthy. That is the whole reason
// this package would rather parse text than take the easy source.
//
// # The format is a real capture, not an assumption
//
// Taken from an Archer C6 running OpenWrt 25.12.5 on 2026-08-16, because
// inventing a payload shape is how this project got a decoder that failed on
// the very first real read (§6, "a mock that is easier to write than the real
// thing"). Two things in that capture would not have been guessed:
//
//   - Most fields are `key:\tvalue`, but some are `key:value` with NO
//     whitespace at all — `short slot time:yes`, `beacon interval:100`. A
//     parser splitting on ":\t" silently drops those.
//   - `signal:  \t-37 [-37, -47, -77, -77] dBm` carries per-chain values in
//     brackets after the headline figure, and the separator is spaces AND a
//     tab. The number wanted is the first one.
//
// # What it does with what it does not understand
//
// Nothing. Unknown keys are ignored and a station block with no recognised
// fields still yields a Peer, because the MAC alone is a real observation: a
// peer that is there and undescribed is still there. What must never happen is
// the reverse — inventing a plink state, or defaulting one to ESTAB — since
// that is the single value the healthy/unhealthy split turns on.

// ParseStationDump reads `iw station dump` output into peers.
//
// Returns no error: this is a best-effort read of a human-facing format, and
// the caller's question is "what could be seen", not "was every line
// understood". Whether the call ANSWERED is decided by its exit status before
// this is ever reached — the difference between a parse that found nothing and
// a command that did not run is the caller's to carry, and Observation has
// separate fields for exactly that.
func ParseStationDump(out string) []Peer {
	var peers []Peer
	var cur *Peer

	flush := func() {
		if cur != nil {
			peers = append(peers, *cur)
			cur = nil
		}
	}

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Station ") {
			flush()
			fields := strings.Fields(trimmed)
			if len(fields) < 2 {
				continue
			}
			cur = &Peer{MAC: strings.ToLower(fields[1])}
			continue
		}
		if cur == nil {
			continue
		}
		// Split on the FIRST colon and trim both halves, which is what makes
		// `key:\tvalue` and `key:value` the same shape. Splitting on ":\t"
		// instead drops every field the device chose not to pad.
		i := strings.IndexByte(trimmed, ':')
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:i])
		val := strings.TrimSpace(trimmed[i+1:])

		switch key {
		case "mesh plink":
			cur.Plink = val
		case "signal":
			if n, ok := leadingInt(val); ok {
				cur.SignalDBm = &n
			}
		case "inactive time":
			if n, ok := leadingInt(val); ok {
				cur.InactiveMS = &n
			}
		}
	}
	flush()
	return peers
}

// leadingInt reads the first integer in a value, which is how both of the
// fields worth having are shaped: `-37 [-37, -47] dBm` and `7700 ms`. Anything
// it cannot read leaves the field nil rather than zero — a signal of 0 dBm is a
// plausible-looking number and a lie.
func leadingInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) {
		c := s[end]
		if c == '-' && end == 0 {
			end++
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		end++
	}
	if end == 0 || (end == 1 && s[0] == '-') {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}
