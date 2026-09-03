package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

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
	ops := make([]applyengine.Op, 0, len(d.Sections)+len(d.Patches))
	for _, s := range sortedSections(d.Sections) {
		current, present := existing.In(s.Config)[s.Name]
		if !present {
			ops = append(ops, applyengine.Op{
				Kind: applyengine.OpAdd, Config: s.Config, Type: s.Type,
				Name: s.Name, Section: s.Name, Values: s.Values, Lists: s.Lists,
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
		// An option that has to become a LIST is deleted first.
		//
		// Measured 2026-08-17 (IMPLEMENTATION §14): rpcd's uci.set DOES convert
		// a string option into a list when handed a JSON array, so this delete
		// is not required for the conversion on the reference firmware.
		//
		// It stays. One firmware is measured, the failure if another does not
		// convert is the silent one — accepted, stored in a form netifd
		// ignores, apply confirms healthy — and this costs a single staged call
		// in the only case that reaches it, a section an older version of us
		// wrote wrong.
		//
		// Staged before the set: applyengine preserves op order through
		// ubus.Batch, and nothing commits until uci.apply.
		for _, k := range sortedKeys(s.Lists) {
			// Only when the option is actually THERE. Deleting an option the
			// device does not have is a call that can fail, and stage() aborts
			// the whole batch on any failed op — so an unnecessary delete would
			// turn a good apply into no apply at all.
			if _, onDevice := current[k]; !onDevice {
				continue
			}
			if isList, known := StoredAsList(current, k); known && !isList {
				ops = append(ops, applyengine.Op{
					Kind: applyengine.OpDelete, Config: s.Config,
					Section: s.Name, Option: k,
				})
			}
		}
		// Options this section manages, that the device still holds, and that
		// this render did not produce.
		//
		// Without this an option written under a condition and then no longer
		// written is never compared by matches and never cleared: the operator
		// turns it off, the preview reports no changes, and the device keeps
		// it. See Section.Manages — measured, on both reference devices.
		for _, k := range managedButUnwritten(s, current) {
			ops = append(ops, applyengine.Op{
				Kind: applyengine.OpDelete, Config: s.Config,
				Section: s.Name, Option: k,
			})
		}
		// Present and ours but different: set rather than add, so options a
		// previous version of us wrote and this one no longer manages are left
		// alone rather than being silently dropped.
		ops = append(ops, applyengine.Op{
			Kind: applyengine.OpSet, Config: s.Config, Type: s.Type,
			Name: s.Name, Section: s.Name, Values: s.Values, Lists: s.Lists,
		})
	}
	for _, p := range sortedPatches(d.Patches) {
		current, present := existing.In(p.Config)[p.Section]
		if !present {
			// A patch never creates a foreign section. Render normally turns this
			// into a conflict; this guard keeps a manually constructed Doc safe.
			continue
		}
		deletes := append([]string(nil), p.Deletes...)
		sort.Strings(deletes)
		for _, option := range deletes {
			if forbiddenPatchOption(option) {
				continue
			}
			if _, present := current[option]; !present {
				continue
			}
			ops = append(ops, applyengine.Op{
				Kind: applyengine.OpDelete, Config: p.Config,
				Section: p.Section, Option: option,
			})
		}
		changed := map[string]string{}
		for option, value := range p.Values {
			if forbiddenPatchOption(option) || current[option] == value {
				continue
			}
			changed[option] = value
		}
		if len(changed) > 0 {
			ops = append(ops, applyengine.Op{
				Kind: applyengine.OpSet, Config: p.Config,
				Section: p.Section, Values: changed, Patch: true,
			})
		}
	}
	return applyengine.Plan{Ops: ops}
}

func forbiddenPatchOption(option string) bool {
	return option == OwnershipTag || option == ".type" || option == ".name"
}

func sortedPatches(in []Patch) []Patch {
	out := append([]Patch(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Config != out[j].Config {
			return out[i].Config < out[j].Config
		}
		return out[i].Section < out[j].Section
	})
	return out
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
	// An option we manage, are not writing, and the device still has, is a
	// difference — otherwise "already matches" is reported for a section that
	// still carries the setting the operator just turned off.
	if len(managedButUnwritten(s, current)) > 0 {
		return false
	}
	// Lists come back from the device space-joined (see reconcile.flatten), so
	// compare in that form rather than round-tripping through a parser.
	for k, v := range s.Lists {
		if current[k] != strings.Join(v, " ") {
			return false
		}
		// Equal text, wrong SHAPE.
		//
		// `option ports 'lan1:t lan2:t'` flattens to exactly what our list
		// joins to, so the comparison above passes while the device holds a
		// form netifd stores and ignores — VLAN filtering on with no untagged
		// membership, and the LAN down after a confirmed, healthy apply
		// (Section.Lists). Reporting "already matches" there means the
		// controller can never repair a config it wrote.
		//
		// Only when the shape is actually known: an Existing with no marker
		// leaves this alone rather than rewriting every list on every plan.
		if isList, known := StoredAsList(current, k); known && !isList {
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
//
// And only ever sections the render actually DECIDED about. A document that
// could not read the device's radios produces no wireless sections, which
// reaches here indistinguishable from a device the operator emptied — so the
// document carries the distinction itself, in Retain and Blind, and this
// honours it. Without that, a refused capability call deleted every interface
// we own on the device and the apply reported success.
func (d Doc) Prune(existing Existing) []applyengine.Op {
	// Keyed by config as well as name: two configs can hold sections with the
	// same name, and pruning "everything called oowrt_net_iot" would reach
	// across from network into dhcp.
	type ref struct{ config, section string }
	wanted := map[ref]bool{}
	for _, s := range d.Sections {
		wanted[ref{s.Config, s.Name}] = true
	}
	var stale []ref
	for config, sections := range existing.Configs {
		// A config this render could not see into is not a config it decided
		// anything about. See Doc.Retain.
		if d.blind(config) {
			continue
		}
		for name := range sections {
			if wanted[ref{config, name}] || !existing.OwnedIn(config, name) {
				continue
			}
			if d.retained(config, name) {
				continue
			}
			stale = append(stale, ref{config, name})
		}
	}
	sort.Slice(stale, func(i, j int) bool { // deterministic diffs
		if stale[i].config != stale[j].config {
			return stale[i].config < stale[j].config
		}
		return stale[i].section < stale[j].section
	})
	ops := make([]applyengine.Op, 0, len(stale))
	for _, r := range stale {
		ops = append(ops, applyengine.Op{
			Kind: applyengine.OpDelete, Config: r.config, Section: r.section,
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
	// Lists are part of the section's identity too: a bridge-VLAN whose port
	// membership changed but whose options did not is a real change.
	lkeys := make([]string, 0, len(s.Lists))
	for k := range s.Lists {
		lkeys = append(lkeys, k)
	}
	sort.Strings(lkeys)
	for _, k := range lkeys {
		fmt.Fprintf(h, "%s\x00%s\x00", k, strings.Join(s.Lists[k], "\x1f"))
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

// Preserved reports an owned section this render deliberately left in place
// rather than pruning — because it could not decide about it. See Doc.Retain.
//
// The counterpart to Prune, and it exists because the ownership RECORD has to
// agree with what the apply actually did. ReplaceOwned replaces rather than
// merges, on the premise that an apply prunes everything absent from the
// document; Retain and Blind made that premise false. A claim dropped for a
// section still sitting on the device is not a bookkeeping detail: un-adopt
// removes exactly the sections in that record, so the controller would lose
// the ability to clean up its own config, and the fleet detail would report
// our own BSS as somebody else's work.
func (d Doc) Preserved(existing Existing, config, name string) bool {
	for _, s := range d.Sections {
		if s.Config == config && s.Name == name {
			return false // rendered, so already claimed on its own account
		}
	}
	if !existing.OwnedIn(config, name) {
		return false // not ours on the device, so not ours to claim
	}
	return d.blind(config) || d.retained(config, name)
}

// sortedKeys is the deterministic iteration Go maps do not give us.
func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// managedButUnwritten lists the options this section owns that the device still
// holds and this render did not produce, in a stable order.
func managedButUnwritten(s Section, current map[string]string) []string {
	var out []string
	for _, k := range s.Manages {
		if _, writing := s.Values[k]; writing {
			continue
		}
		if _, writingList := s.Lists[k]; writingList {
			continue
		}
		if _, onDevice := current[k]; !onDevice {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
