package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
)

// Plan converts a rendered document into staged operations for the apply
// engine.
//
// Everything is an add of a NAMED section. Named sections make the operation
// idempotent — a re-render targets the same section rather than appending a
// duplicate — and make /etc/config readable to a human wondering what wrote
// this.
//
// Nothing here commits. uci.apply is what commits, together with the rollback
// snapshot; a commit beforehand would leave nothing staged and silently disarm
// the protection.
func (d Doc) Plan(existing Existing) applyengine.Plan {
	ops := make([]applyengine.Op, 0, len(d.Sections))
	for _, s := range sortedSections(d.Sections) {
		current, present := existing.WifiIfaces[s.Name]
		if !present {
			ops = append(ops, applyengine.Op{
				Kind: applyengine.OpAdd, Config: s.Config, Type: s.Type,
				Name: s.Name, Section: s.Name, Values: s.Values,
			})
			continue
		}
		if matches(s, current) {
			// Already exactly what we would write. Emitting a set anyway makes
			// every preview report changes that change nothing — "2 changes
			// pending" on a device that already matches — and that is how an
			// operator learns to stop reading the preview. It also means
			// DevicePlan.Empty() could never be true, so a no-op apply would
			// still stage, apply and confirm against a device for no reason.
			continue
		}
		// Present and ours but different: set rather than add, so options a
		// previous version of us wrote and this one no longer manages are left
		// alone rather than being silently dropped.
		ops = append(ops, applyengine.Op{
			Kind: applyengine.OpSet, Config: s.Config, Type: s.Type,
			Name: s.Name, Section: s.Name, Values: s.Values,
		})
	}
	return applyengine.Plan{Ops: ops}
}

// matches reports whether the device already holds every value this section
// would write.
//
// Only the keys WE write are compared. The device adds defaults of its own and
// hostapd writes state back into these sections, so comparing whole sections
// would find a difference every time and never converge.
func matches(s Section, current map[string]string) bool {
	for k, v := range s.Values {
		if current[k] != v {
			return false
		}
	}
	return true
}

// Prune returns operations removing sections we own that the render no longer
// produces — a WLAN deleted from the site model, or one that stopped applying
// to this device.
//
// Only ever sections carrying our marker. A section without it was written by a
// human and is not ours to delete, however much it looks like ours.
func (d Doc) Prune(existing Existing) []applyengine.Op {
	wanted := map[string]bool{}
	for _, s := range d.Sections {
		wanted[s.Name] = true
	}
	var stale []string
	for name := range existing.WifiIfaces {
		if wanted[name] || !existing.Owned(name) {
			continue
		}
		stale = append(stale, name)
	}
	sort.Strings(stale) // deterministic diffs
	ops := make([]applyengine.Op, 0, len(stale))
	for _, name := range stale {
		ops = append(ops, applyengine.Op{
			Kind: applyengine.OpDelete, Config: "wireless", Section: name,
		})
	}
	return ops
}

// Hash is the canonical fingerprint of one rendered section, stored in
// owned_sections so the reconciler can detect drift without re-deriving intent.
//
// Canonical means key-sorted: Go map iteration is randomised, and a hash that
// changed between runs would report drift on every poll.
func (s Section) Hash() string {
	keys := make([]string, 0, len(s.Values))
	for k := range s.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", s.Config, s.Type, s.Name)
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\x00", k, s.Values[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedSections(in []Section) []Section {
	out := append([]Section(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Config != out[j].Config {
			return out[i].Config < out[j].Config
		}
		return out[i].Name < out[j].Name
	})
	return out
}
