// Package render turns the site model into per-device UCI documents.
//
// Pure functions, no I/O: the same inputs always produce the same document,
// which is what makes the "what will change on this device" preview honest and
// the test suite exhaustive.
//
// Two rules run through everything here:
//
//   - We only ever write sections we own, named oowrt_* and carrying
//     option oonfeewrt '1'. A foreign section with a colliding name or a
//     conflicting function aborts the render for that device rather than being
//     overwritten. You are a guest on someone else's router.
//   - Capability gates are absences, not errors. A WLAN asking for 6 GHz on a
//     device with no 6 GHz radio renders nothing for that band and says so in
//     the report; it does not fail the device.
package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// OwnershipTag marks every section we create.
const OwnershipTag = "oonfeewrt"

// NamePrefix distinguishes our sections at a glance in /etc/config, which
// matters when a human is reading the file over SSH wondering what touched it.
const NamePrefix = "oowrt"

// ListsKey is where a reader of the device records which of a section's
// options arrived as UCI *lists* rather than plain strings.
//
// It exists because that distinction is otherwise destroyed on the way in.
// reconcile.flatten renders a list space-joined — which is how `uci get`
// prints one — so `list ports 'lan1:t'` + `list ports 'lan2:t'` and
// `option ports 'lan1:t lan2:t'` arrive here as the identical Go string. The
// two are not the same config: netifd honours the first and silently ignores
// the second, and Section.Lists records that rendering a bridge-VLAN the
// second way took the LAN down after an apply had already been confirmed
// healthy.
//
// Without this, plan.matches compared the joined text, found it equal, and
// reported "already matches" — so a device holding the malformed form could
// never be corrected by the thing that wrote it.
//
// Dotted, like UCI's own .type and .name metadata, so it cannot collide with a
// real option name. Absent means "nobody recorded this", which is a third
// state and not "no lists": see StoredAsList.
const ListsKey = ".oowrt_lists"

// StoredAsList reports how the device holds one option, and whether that is
// known at all.
//
// Three-state on purpose. An Existing built by hand — every test fixture, and
// any future reader that does not record it — has no marker, and guessing
// there would either mask the malformed form or rewrite every correct list on
// every plan.
func StoredAsList(current map[string]string, option string) (isList, known bool) {
	raw, ok := current[ListsKey]
	if !ok {
		return false, false
	}
	for _, name := range strings.Fields(raw) {
		if name == option {
			return true, true
		}
	}
	return false, true
}

// Section is one UCI section we intend to exist.
type Section struct {
	Config string
	Type   string
	Name   string
	Values map[string]string

	// Lists are UCI *list* options, which are not the same thing as a string
	// with spaces in it.
	//
	// Writing `option ports 'lan1:u* lan2:u*'` where UCI wants
	// `list ports 'lan1:u*'` is accepted by uci.set without complaint, stored
	// without complaint, and then ignored by netifd. Measured: rendering a
	// bridge-VLAN's ports that way produced a config the device kept and did
	// not honour, VLAN filtering came on with no untagged membership, and the
	// LAN went down after the apply had already been confirmed as healthy.
	// There is no error anywhere in that chain — which is exactly why the
	// distinction is a separate field rather than a convention.
	Lists map[string][]string
}

// SectionRef identifies one UCI section: a config and a section name.
type SectionRef struct{ Config, Name string }

// Doc is everything we intend to exist on one device.
type Doc struct {
	DeviceID int64
	Sections []Section

	// Retain and Blind are how this document says "I did not decide about
	// that", and they exist because Prune cannot tell the difference on its
	// own.
	//
	// Prune deletes every section we own that the render did not produce. That
	// is right when the absence is a DECISION — the WLAN was deleted, the
	// device left the AP group, the role changed to one that does not
	// broadcast. It is catastrophic when the absence is IGNORANCE: a device
	// whose radio list could not be read renders no wireless at all, and a bare
	// Prune then deletes every interface we own on it, including the wireless
	// uplink that is its only path to the network. The apply reports success.
	//
	// That is this project's cardinal error — NotObservable collapsed into
	// Absent — committed at the point of deletion rather than at the point of
	// probing, which is why the capability package's care about it was not
	// enough on its own. Render already knew: it emits the
	// "hardware-unidentified" warning saying in as many words that the check
	// did not run, and then produced a plan to delete on the strength of it.
	// Worse, that warning's advice is "apply a WLAN and re-probe" — and the
	// apply was the thing doing the deleting, so the stated remedy could never
	// work.
	//
	// Retain names exact sections to leave alone; Blind names whole configs the
	// render could not see into, for when we cannot even name the sections
	// because naming them needs the radio we could not read.
	Retain []SectionRef
	Blind  []string
}

// retained reports a section this render could not decide about.
func (d Doc) retained(config, name string) bool {
	for _, r := range d.Retain {
		if r.Config == config && r.Name == name {
			return true
		}
	}
	return false
}

// blind reports a config this render could not see into at all.
func (d Doc) blind(config string) bool {
	for _, c := range d.Blind {
		if c == config {
			return true
		}
	}
	return false
}

// Configs lists the distinct UCI configs the document touches.
func (d Doc) Configs() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range d.Sections {
		if !seen[s.Config] {
			seen[s.Config] = true
			out = append(out, s.Config)
		}
	}
	sort.Strings(out)
	return out
}

// OmissionKind separates the reasons a thing is not on the device, because
// they are not one reason and the screen was presenting them as one.
//
// The preview rendered every omission under "Left out on this device (not an
// error — the hardware or firmware cannot take it)". That sentence is true of
// roughly four of the nineteen. Two of the others are conditions an operator
// has to act on BEFORE applying — an unencrypted mesh anyone in range can join,
// and a wireless bridge that is a layer-2 loop if the device is also cabled —
// and both sat in muted grey under a heading calling them not an error. The
// rest are decisions (a role, an override, a VLAN we do not own) or things we
// could not determine, which is the opposite of a hardware limit.
type OmissionKind string

const (
	// KindUnclassified is the zero value, and it deliberately claims nothing.
	// An omission added without a kind gets a neutral heading rather than
	// inheriting an assertion about the hardware nobody checked.
	KindUnclassified OmissionKind = ""
	// KindCaution is rendered, and needs a human decision before the apply.
	KindCaution OmissionKind = "caution"
	// KindUndetermined is "we could not establish this", which includes every
	// section left in place because nothing could be decided about it.
	KindUndetermined OmissionKind = "undetermined"
)

// Omission records something the operator asked for that is not on the device,
// and why. These are shown in the diff preview: silently dropping a requested
// SSID is how a controller loses trust.
type Omission struct {
	WLAN   string
	Reason string
	Kind   OmissionKind
}

// Conflict is a foreign section we refuse to touch. Conflicts abort the render
// for the device — surfaced loudly, never resolved silently.
type Conflict struct {
	Config  string
	Section string
	Reason  string
}

// Warning is a setting this device WILL accept and will not honour, or will
// break on — a known defect in its wireless driver.
//
// Distinct from Omission, and the distinction is the point. An Omission is the
// controller declining to do something, so the operator knows it did not
// happen. A Warning is the controller doing exactly what was asked on hardware
// documented not to survive it. The config is rendered either way: this
// project does not silently rewrite a user's security settings, and a warning
// the operator can act on is more honest than a quiet downgrade.
type Warning struct {
	// WLAN is the SSID this applies to, or "" for a defect of the hardware
	// itself that no configuration triggers.
	WLAN     string
	DefectID string
	Summary  string
	Detail   string
	// Confidence is how well the defect is established — device documentation,
	// measured here, a filed bug, or forum anecdote. Carried through to the UI
	// so folklore is never shown with the authority of a maintainer statement.
	Confidence string
	Severity   string
	Mitigation string
	Source     string
}

// Report accompanies every render.
type Report struct {
	Omissions []Omission
	Conflicts []Conflict
	// Warnings are known driver defects this render would hit. They do NOT
	// block an apply — see Warning.
	Warnings []Warning
}

// addWarning records a warning once per defect per WLAN.
//
// Deduplicated because a WLAN fans out to every band the device has, so a
// defect triggered by one of its settings matches once per radio. Verified
// against the reference WRT3200ACM: the 802.11w warning arrived twice for one
// SSID, which reads as two problems and teaches an operator to skim.
func (r *Report) addWarning(w Warning) {
	for _, existing := range r.Warnings {
		if existing.DefectID == w.DefectID && existing.WLAN == w.WLAN {
			return
		}
	}
	r.Warnings = append(r.Warnings, w)
}

// warn converts a matched defect into an operator-facing warning.
func warn(ssid string, d capability.Defect) Warning {
	return Warning{
		WLAN: ssid, DefectID: d.ID, Summary: d.Summary, Detail: d.Detail,
		Confidence: string(d.Confidence), Severity: string(d.Severity),
		Mitigation: d.Mitigation, Source: d.Source,
	}
}

// HasConflicts reports whether the render must not proceed.
func (r Report) HasConflicts() bool { return len(r.Conflicts) > 0 }

// Existing describes what is already on the device, so the renderer can detect
// collisions with foreign config.
//
// Keyed by UCI config then section, because the render spans four of them —
// wireless, network, dhcp and firewall. It began as a bare map of wifi-ifaces
// and grew a config dimension when networks arrived; the alternative was four
// parallel maps and four nearly identical lookups.
type Existing struct {
	// Configs maps config name -> section name -> its values, including any
	// ownership tag.
	Configs map[string]map[string]map[string]string
}

// WifiIfaces is the wireless config's wifi-iface sections.
//
// Filtered by .type on purpose: a wifi-device (a radio) is not an AP interface,
// and the SSID-collision check below would otherwise be scanning sections that
// cannot hold an SSID. Callers that need every wireless section — the
// name-collision check does, since a foreign section could have our name and
// any type at all — use In("wireless") instead.
func (e Existing) WifiIfaces() map[string]map[string]string {
	out := map[string]map[string]string{}
	for name, vals := range e.In("wireless") {
		if vals[".type"] == "wifi-iface" {
			out[name] = vals
		}
	}
	return out
}

// In returns one config's sections, never nil, so callers can index freely.
func (e Existing) In(config string) map[string]map[string]string {
	if e.Configs == nil {
		return map[string]map[string]string{}
	}
	if c, ok := e.Configs[config]; ok {
		return c
	}
	return map[string]map[string]string{}
}

// WirelessOnly builds an Existing holding just the wireless config, for callers
// and tests that have read only that one.
func WirelessOnly(ifaces map[string]map[string]string) Existing {
	return Existing{Configs: map[string]map[string]map[string]string{"wireless": ifaces}}
}

// NewExisting builds an Existing from per-config section maps.
func NewExisting(configs map[string]map[string]map[string]string) Existing {
	return Existing{Configs: configs}
}

// Owned reports whether an existing wireless section carries our marker.
func (e Existing) Owned(section string) bool {
	return e.OwnedIn("wireless", section)
}

// OwnedIn reports whether a section in any config carries our marker.
//
// The marker is the whole ownership model: a section without it was written by
// a human and is not ours to change, however much its name looks like ours.
func (e Existing) OwnedIn(config, section string) bool {
	return e.In(config)[section][OwnershipTag] == "1"
}

// Render produces the UCI document for one device.
//
// caps gates what can be expressed. A nil Existing means "nothing known", which
// is only appropriate for previews — a real apply should pass the device's
// current wireless config so collisions are caught before staging.
func Render(site model.Site, dev model.Device, caps *capability.Registry, existing Existing) (Doc, Report, error) {
	var rep Report
	if errs := site.Validate(); len(errs) > 0 {
		return Doc{}, rep, fmt.Errorf("render: site model is invalid: %v", errs[0])
	}
	doc := Doc{DeviceID: dev.ID}

	// What this render could not see, recorded before anything is decided on
	// the strength of it. See Doc.Retain.
	//
	// The port layout is the wired half. A device that did not report it
	// renders no VLAN, no addressing, no DHCP and no firewall zone — which is
	// indistinguishable, to Prune, from an operator having deleted all of them.
	// Only counts while the site actually asks for a wired VLAN: with no such
	// network the render's silence is a decision about the model, not
	// ignorance about the device.
	//
	// An EMPTY bridge is the unreadable case, and it is the only one. A board
	// that reports a bridge and no switch ports has answered: probePorts sets
	// Bridge from lan.Device precisely for boards whose LAN is one interface
	// rather than a set of taggable ports. Treating that as blindness disabled
	// pruning across every such board — the safe direction, and still the same
	// conflation of "could not ask" with "asked and got an answer".
	//
	// Observed on the reference Archer C6, which reports bridge eth0.1, no LAN
	// ports, and DSA Absent: a swconfig board, read successfully.
	if wantsVLAN(site) && (caps == nil || caps.Ports.Bridge == "") {
		doc.Blind = append(doc.Blind, "network", "dhcp", "firewall")
	}

	// Networks first: a WLAN attaches to one, and a config file that declares
	// the interface before the wireless section referencing it reads the way a
	// human would write it.
	var members []zoneMember
	for _, n := range site.Networks {
		secs, oms, m := renderNetwork(n, dev, caps, existing)
		for _, sec := range secs {
			addOwned(&doc, &rep, existing, sec)
		}
		rep.Omissions = append(rep.Omissions, oms...)
		if m.iface != "" {
			members = append(members, m)
		}
	}
	// Firewall zones last, and once per zone rather than once per network. See
	// renderZones: a zone is identified by its name, so two networks sharing
	// one are two sections with the same name of which the device keeps one.
	zoneSecs, zoneConflicts := renderZones(members, existing)
	rep.Conflicts = append(rep.Conflicts, zoneConflicts...)
	for _, sec := range zoneSecs {
		addOwned(&doc, &rep, existing, sec)
	}

	// A role that does not publish WLANs gets none, even where the hardware
	// could carry them and the site model asks for them.
	//
	// This is the point of the role rather than an edge case. An old router
	// repurposed as a switch almost always still has radios, and "has radios"
	// is not "should be broadcasting". Before roles were a closed vocabulary
	// this branch did not exist: a device adopted as a switch was an access
	// point in every respect that mattered, silently.
	if !dev.Role.Wireless() {
		for _, w := range site.WLANsFor(dev.ID) {
			rep.Omissions = append(rep.Omissions, Omission{
				WLAN: w.SSID,
				Reason: fmt.Sprintf("this device's role is %q, which does not "+
					"publish WLANs (%s). Change the role, or take it out of the "+
					"AP group, depending on which one is wrong",
					dev.Role, dev.Role.Describe()),
			})
		}
		return doc, rep, nil
	}

	// An access point that no WLAN targets broadcasts nothing, and says so
	// nowhere.
	//
	// Preview reports "already matches — nothing to do", which is true and
	// useless: the device genuinely matches a model that asks nothing of it.
	// The likeliest cause is group membership, and the likeliest cause of THAT
	// is a re-adoption — un-adopt deletes the device row, its group membership
	// goes with it by cascade, and the new row has a new id that is in no
	// group. Observed for real: a device came back adopted, healthy, polling,
	// and silently off the air, with a preview that said everything was fine.
	//
	// Only when the site HAS WLANs. On a fresh install with none, "no WLAN
	// targets this device" is a description of the whole site rather than a
	// problem with the device, and saying it per device is noise.
	if len(site.WLANs) > 0 && len(site.WLANsFor(dev.ID)) == 0 {
		rep.Omissions = append(rep.Omissions, Omission{
			WLAN: "(none)",
			Reason: "no WLAN targets this device, so it will broadcast nothing. " +
				"It is not a member of any AP group that a WLAN is published to " +
				"— check its membership under AP groups. A device that was " +
				"un-adopted and adopted again is a new entry and keeps none of " +
				"its old group memberships",
		})
	}

	radios := radiosByBand(caps)
	// The wireless half. An empty radio map has two causes that look identical
	// from here and could not be further apart: a device with no radios, and a
	// device whose radio list the ACL refused. capability records the second as
	// FeatSurvey NotObservable with no radios, which is exactly the pair to
	// test for — Absent with no radios really is a device without wireless.
	radiosUnknown := caps == nil || (len(caps.Radios) == 0 &&
		!caps.State(capability.FeatSurvey).Decided())
	if radiosUnknown {
		doc.Blind = append(doc.Blind, "wireless")
	}
	// Said once, in the words that are true. "device has no 2.4 GHz radio" is
	// a claim about hardware, and making it from a refused call is the same
	// mistake tools/probe.py made about DSA.
	noRadio := func(band model.Band) string {
		if radiosUnknown {
			return fmt.Sprintf("this device's radio list could not be read, so "+
				"whether it has a %s radio is unknown and nothing is rendered "+
				"for that band. This is not a statement that the radio is "+
				"absent — it means the check did not run", band)
		}
		return fmt.Sprintf("device has no %s radio", band)
	}
	for _, base := range site.WLANsFor(dev.ID) {
		// Per-device overrides are folded in here, on a copy. Mutating the site
		// model would leak one device's overrides into the next device rendered.
		w, published := site.Overrides.Apply(dev.ID, base)
		if !published {
			rep.Omissions = append(rep.Omissions, Omission{
				WLAN:   base.SSID,
				Reason: "not published on this device (per-device override)",
			})
			continue
		}
		net, _ := site.NetworkByID(w.NetworkID)
		rendered := 0
		for _, band := range orderedBands(w.Bands) {
			radio, ok := radios[band]
			if !ok {
				rep.Omissions = append(rep.Omissions, Omission{
					WLAN: w.SSID, Reason: noRadio(band),
					Kind: radioKind(radiosUnknown),
				})
				continue
			}
			name := ifaceName(w.ID, radio)
			// Every wireless section, not only the ifaces: a foreign section
			// could carry our name with any type, and we would still collide.
			if vals, exists := existing.In("wireless")[name]; exists && vals[OwnershipTag] != "1" {
				rep.Conflicts = append(rep.Conflicts, Conflict{
					Config: "wireless", Section: name,
					Reason: "a section with our name exists but is not ours; " +
						"refusing to overwrite config we did not write",
				})
				continue
			}
			if other, clash := foreignSSIDOnRadio(existing, radio, w.SSID, name); clash {
				rep.Conflicts = append(rep.Conflicts, Conflict{
					Config: "wireless", Section: other,
					Reason: fmt.Sprintf("SSID %q is already published on %s by a "+
						"section we do not own", w.SSID, radio),
				})
				continue
			}
			sec, omissions := renderWifiIface(site, w, net, radio, caps)
			// Checked on the RENDERED values rather than on the model, so it
			// catches settings the renderer derives rather than only the ones
			// the operator typed. WPA3 forcing PMF on is exactly such a case,
			// and on this hardware it is the dangerous one.
			for _, d := range capability.TriggeredByOn(caps, sec.Values,
				radioBySection(caps, radio)) {
				rep.addWarning(warn(w.SSID, d))
			}
			// A radio switched off swallows the WLAN silently. The section we
			// write is correct, the apply succeeds, and nothing broadcasts —
			// then the health check fails looking for an SSID that was never
			// going to appear, for a reason no screen could explain.
			//
			// Read from the device's own config rather than from the capability
			// record, because the record is a snapshot from adoption and a
			// radio can be switched off in LuCI any time after it.
			if radioIsDisabled(existing, radio) {
				rep.addWarning(Warning{
					WLAN:     w.SSID,
					DefectID: "radio-disabled",
					Summary: fmt.Sprintf("radio %s is switched off on this device, "+
						"so this network will not go on the air there", radio),
					Detail: "The wireless section will be written correctly and the " +
						"apply will report success. Nothing will broadcast, and the " +
						"health check will then fail looking for an SSID that was " +
						"never going to appear. oonfeeWRT does not switch radios on: " +
						"it only changes config it wrote, and this radio is not that.",
					Confidence: string(capability.ConfMeasuredHere),
					Severity:   string(capability.SevSilentlyIgnored),
					Mitigation: fmt.Sprintf("Enable it on the device: "+
						"`uci set wireless.%s.disabled='0'; uci commit wireless; "+
						"wifi reload`", radio),
				})
			}
			doc.Sections = append(doc.Sections, sec)
			rep.Omissions = append(rep.Omissions, omissions...)
			rendered++
		}
		if rendered == 0 && len(rep.Conflicts) == 0 {
			rep.Omissions = append(rep.Omissions, Omission{
				WLAN:   w.SSID,
				Reason: "no radio on this device matches the selected bands",
			})
		}
	}

	// The station half of a wireless uplink, if this device has one.
	//
	// Rendered before the meshes and the APs for one reason worth keeping: a
	// device with a wireless uplink has no other path to the network, so if the
	// list is ever trimmed or an apply is ever made partial, the interface the
	// device NEEDS should not be the one that gets dropped.
	if u, ok := site.UplinkFor(dev.ID); ok {
		if w, found := site.WLANByID(u.WLANID); found {
			net, _ := site.NetworkByID(w.NetworkID)
			radio, hasRadio := radios[u.Band]
			switch {
			case !hasRadio:
				rep.Omissions = append(rep.Omissions, Omission{
					WLAN: w.SSID,
					Reason: fmt.Sprintf("this device has no %s radio, so it cannot "+
						"join %s over the air on that band", u.Band, w.SSID),
				})
			default:
				sec, oms := renderUplink(u, w, net, radio, caps)
				rep.Omissions = append(rep.Omissions, oms...)
				switch {
				case sec.Name != "":
					doc.Sections = append(doc.Sections, sec)
				case undetermined(caps, capability.FeatWirelessUplink):
					// The gate could not read the package list, so it does not
					// know whether this device can join a network over the air.
					// Deleting the station interface on the strength of that
					// would cut off a device whose only path to the network is
					// the very link we could not verify.
					doc.retain(&rep, existing, uplinkIfaceName(u.ID, radio),
						fmt.Sprintf("the existing wireless uplink section for %s "+
							"is left exactly as it is: this render could not "+
							"establish whether the device supports one, and "+
							"removing the link the controller reaches it through "+
							"on the strength of a check that did not run is not "+
							"something oonfeeWRT will do", w.SSID))
				}
			}
		}
	}

	// Mesh backhauls, alongside the APs rather than instead of them: a node
	// carrying a mesh while still serving clients is the intended arrangement.
	for _, m := range site.MeshesFor(dev.ID) {
		net, _ := site.NetworkByID(m.NetworkID)
		radio, ok := radios[m.Band]
		if !ok {
			rep.Omissions = append(rep.Omissions, Omission{
				WLAN: m.MeshID,
				Reason: fmt.Sprintf("device has no %s radio, and mesh nodes peer "+
					"only with nodes on the same band — so this one cannot join "+
					"that mesh", m.Band),
			})
			continue
		}
		name := meshIfaceName(m.ID, radio)
		if vals, exists := existing.In("wireless")[name]; exists && vals[OwnershipTag] != "1" {
			rep.Conflicts = append(rep.Conflicts, Conflict{
				Config: "wireless", Section: name,
				Reason: "a section with our name exists but is not ours; " +
					"refusing to overwrite config we did not write",
			})
			continue
		}
		sec, omissions := renderMesh(m, net, radio, caps)
		rep.Omissions = append(rep.Omissions, omissions...)
		switch {
		case sec.Name != "":
			doc.Sections = append(doc.Sections, sec)
		case undetermined(caps, capability.FeatMesh):
			doc.retain(&rep, existing, name,
				fmt.Sprintf("the existing mesh section for %s is left exactly as "+
					"it is: this render could not establish whether the device "+
					"supports 802.11s, and a backhaul that is carrying traffic "+
					"must not be deleted because a check did not run", m.MeshID))
		}
	}

	// Defects of the hardware itself, reported once against the device. Kept
	// separate from the per-WLAN ones because nothing the operator changes will
	// make them go away, and repeating them on every SSID would bury the
	// warnings they can actually act on.
	for _, d := range capability.DefectsFor(caps) {
		if !d.Configured() {
			rep.addWarning(warn("", d))
		}
	}
	// Defects triggered by the radio's CURRENT state rather than by anything we
	// write — a DFS channel, say. The controller does not manage channels, so
	// these can only be found by looking at the device.
	for _, d := range capability.TriggeredByRadios(withLiveChannels(caps, existing)) {
		rep.addWarning(warn("", d))
	}
	// And when nothing could be checked at all, say so. Silence here would be a
	// clean bill of health from a check that never ran: the hardware name comes
	// from iwinfo, iwinfo only answers for a radio with an interface, and stock
	// OpenWrt ships with its default wifi-iface disabled, so nothing on a fresh
	// router is broadcasting. A freshly adopted router is therefore
	// the case most likely to look defect-free and the case where the operator
	// is choosing the settings this exists to warn about.
	// Fires when the radio list is EMPTY too, not only when radios exist and
	// cannot be named.
	//
	// probeRadios returns early with no radios and the wireless features
	// NotObservable when iwinfo.devices is refused, so gating this on there
	// being radios made it silent in the case where the least is known — the
	// same collapse of "could not ask" into "nothing there" that this warning
	// exists to prevent. FeatSurvey separates the two, which is how roleFit
	// already tells them apart.
	unreadable := caps != nil &&
		((len(caps.Radios) > 0 && !capability.HardwareIdentified(caps)) ||
			(len(caps.Radios) == 0 && !caps.State(capability.FeatSurvey).Decided()))
	reportBlind(&doc, &rep, existing)
	if unreadable {
		rep.addWarning(Warning{
			DefectID: "hardware-unidentified",
			Summary: "this device's radios could not be checked against the " +
				"known-defect list",
			Detail: "Either no radio reported a hardware name, or the radio list " +
				"itself could not be read. The name comes from iwinfo, which only " +
				"answers for a radio that has an interface, and the list itself is " +
				"refused outright by some access-control files. This is not a clean " +
				"bill of health — it means the check did not run.",
			Confidence: string(capability.ConfMeasuredHere),
			Severity:   string(capability.SevSilentlyIgnored),
			Mitigation: "Apply a WLAN and re-probe; once a radio is broadcasting it " +
				"reports what it is.",
		})
	}
	return doc, rep, nil
}

// renderWifiIface builds one wifi-iface section.
func renderWifiIface(site model.Site, w model.WLAN, net model.Network,
	radio string, caps *capability.Registry) (Section, []Omission) {

	var omissions []Omission
	v := map[string]string{
		"device":  radio,
		"mode":    "ap",
		"ssid":    w.SSID,
		"network": net.Name,
	}

	// Security
	v["encryption"] = string(w.Security.Mode)
	if w.Security.Mode.NeedsKey() {
		v["key"] = w.Security.Key
	}
	// Protected management frames, constrained by what the security mode can
	// actually carry.
	//
	// The stored value is not trusted on its own. Every new WLAN is created
	// with pmf="1" and the editor hides the control for modes that cannot use
	// it, so a WLAN switched to Open keeps a PMF setting nobody chose and
	// nobody can clear — and it was rendered onto the device, where it is
	// meaningless without RSN and, on a Marvell radio, triggered a driver
	// warning the operator had no control to act on.
	switch w.Security.Mode {
	case model.SecNone:
		// No RSN, so no protected management frames. Nothing to write.
	case model.SecSAE, model.SecOWE:
		// Both mandate PMF. Rendering either without it produces an AP that
		// clients reject for reasons nobody enjoys debugging.
		v["ieee80211w"] = string(model.PMFRequired)
	case model.SecSAEMixed:
		// The WPA3 half needs it at least optional. "Disabled" here silently
		// removes SAE from a network still advertising it.
		if w.Security.PMF == model.PMFDisabled || w.Security.PMF == "" {
			v["ieee80211w"] = string(model.PMFOptional)
		} else {
			v["ieee80211w"] = string(w.Security.PMF)
		}
	default:
		if w.Security.PMF != "" {
			v["ieee80211w"] = string(w.Security.PMF)
		}
	}

	// Roaming. The mobility domain is derived, not configured — that is the
	// whole point: every AP in the group computes the same value from the site
	// UUID and WLAN id, with no coordination between them.
	if w.Roaming.FT {
		if w.Security.Mode == model.SecPSK2 && !w.Roaming.FTWithPSK2 {
			omissions = append(omissions, Omission{WLAN: w.SSID,
				Reason: "802.11r not rendered: it breaks some older clients on " +
					"WPA2-PSK and the compatibility warning was not accepted"})
		} else {
			v["ieee80211r"] = "1"
			v["mobility_domain"] = MobilityDomain(site.UUID, w.ID)
			v["reassociation_deadline"] = "20000"
			if w.Roaming.FTOverDS {
				v["ft_over_ds"] = "1"
			} else {
				v["ft_over_ds"] = "0"
			}
		}
	}
	if w.Roaming.KV {
		v["ieee80211k"] = "1"
		v["rrm_neighbor_report"] = "1"
		v["rrm_beacon_report"] = "1"
		v["bss_transition"] = "1"
		v["wnm_sleep_mode"] = "1"
	}

	// Options
	if w.Options.Hidden {
		v["hidden"] = "1"
	}
	if w.Options.Isolate {
		v["isolate"] = "1"
	}
	if w.Options.MaxAssoc > 0 {
		v["maxassoc"] = strconv.Itoa(w.Options.MaxAssoc)
	}
	// The AP half of a wireless uplink. The half people forget: configure the
	// joining device and not this, and the station associates as an ordinary
	// client while everything behind it stays dark — a failure that looks like
	// a driver refusing 4-address frames and is not.
	//
	// Written explicitly in BOTH directions, like ft_over_ds above, and for a
	// reason measured on hardware. A plan compares only the keys it writes
	// (plan.go), so omitting the option when the flag is off leaves whatever
	// was last applied sitting on the device — an access point still accepting
	// 4-address frames after an operator turned that off, with the UI showing
	// it off. This option decides what an AP accepts from the air, so a stale
	// one is a security posture nobody chose.
	if w.Options.AllowUplink {
		v["wds"] = "1"
	} else {
		v["wds"] = "0"
	}

	v[OwnershipTag] = "1"
	return Section{
		Config: "wireless", Type: "wifi-iface",
		Name: ifaceName(w.ID, radio), Values: v,
	}, omissions
}

// ifaceName is deterministic so a re-render targets the same section rather
// than accumulating duplicates.
func ifaceName(wlanID int, radio string) string {
	return fmt.Sprintf("%s_wlan%d_%s", NamePrefix, wlanID, radio)
}

// foreignSSIDOnRadio finds a section we do not own already publishing this SSID
// on this radio. Two APs answering for one SSID with different keys is a
// genuinely confusing failure, so it is a conflict rather than a silent
// duplicate.
func foreignSSIDOnRadio(e Existing, radio, ssid, ours string) (string, bool) {
	// Every wireless section, not only those typed wifi-iface. A section with a
	// device and an ssid is publishing that SSID whatever its .type says, and a
	// conflict check that can be dodged by a missing metadata key fails open on
	// exactly the case it exists to catch.
	ifaces := e.In("wireless")
	names := make([]string, 0, len(ifaces))
	for name := range ifaces {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic conflict reporting
	for _, name := range names {
		if name == ours {
			continue
		}
		vals := ifaces[name]
		if vals[OwnershipTag] == "1" {
			continue
		}
		if vals["ssid"] == ssid && vals["device"] == radio {
			return name, true
		}
	}
	return "", false
}

// radiosByBand maps each band this device can serve to its radio name.
//
// Capability reports radios with frequencies; the site model speaks in bands.
// A device with two 5 GHz radios keeps the first in stable order, so repeated
// renders do not shuffle SSIDs between radios.
func radiosByBand(caps *capability.Registry) map[model.Band]string {
	out := map[model.Band]string{}
	if caps == nil {
		return out
	}
	radios := append([]capability.Radio(nil), caps.Radios...)
	sort.Slice(radios, func(i, j int) bool { return radios[i].Device < radios[j].Device })
	for _, r := range radios {
		// Frequency first, because it is what the radio is actually doing.
		// Falling back to the CONFIGURED band matters for a radio that has no
		// interface: iwinfo reports no frequency for one, and skipping it made
		// the renderer announce "device has no 5 GHz radio" about hardware that
		// was sitting right there — which no apply could ever fix, since the
		// radio needed an interface to become visible and could not be given
		// one.
		band, ok := model.BandForFrequency(r.Frequency)
		if !ok && r.Band != "" {
			band, ok = model.Band(r.Band), true
		}
		if !ok {
			continue
		}
		if _, taken := out[band]; !taken {
			out[band] = radioSection(r)
		}
	}
	return out
}

// radioSection is the UCI wifi-device name a wifi-iface must reference.
//
// Capability reports the runtime interface (phy0-ap0); UCI wants the config
// section (radio0). The phy index is the stable link between them.
func radioSection(r capability.Radio) string {
	phy := r.Phy
	if phy == "" {
		return r.Device
	}
	// phy0 -> radio0
	if len(phy) > 3 && phy[:3] == "phy" {
		return "radio" + phy[3:]
	}
	return phy
}

// orderedBands returns the requested bands in a stable order, so section
// ordering in the diff preview does not churn between renders.
func orderedBands(bands []model.Band) []model.Band {
	order := map[model.Band]int{model.Band2G: 0, model.Band5G: 1, model.Band6G: 2}
	out := append([]model.Band(nil), bands...)
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}

// radioIsDisabled reports a radio the device has switched off.
//
// Only an explicit "1" counts. An absent option means enabled — that is UCI's
// own default and what every healthy device in the lab reports — and a radio we
// could not read anything about must not be accused of being off, because the
// warning tells an operator to go and change something.
func radioIsDisabled(existing Existing, radio string) bool {
	sec, ok := existing.In("wireless")[radio]
	if !ok {
		return false
	}
	return sec["disabled"] == "1"
}

// radioBySection finds the capability record for a UCI radio section, or nil
// when the section matches none. Nil means "we do not know which radio", which
// callers must treat as "could be any of them" rather than as "none".
func radioBySection(caps *capability.Registry, section string) *capability.Radio {
	if caps == nil {
		return nil
	}
	for i := range caps.Radios {
		if radioSection(caps.Radios[i]) == section {
			return &caps.Radios[i]
		}
	}
	return nil
}

// withLiveChannels overlays each radio's channel from the device's own wireless
// config, keeping the capability record's value as the fallback.
//
// Defects that key on the channel judge a number frozen at adoption otherwise.
// The controller does not manage channels, so that number can only change
// behind its back — and it then fails in both directions: silent when the
// operator moves a radio ONTO a DFS channel after adoption, and crying wolf
// forever after they move it off.
//
// The snapshot is kept as the fallback rather than replaced. A radio set to
// `channel auto` has no numeric value in UCI at all, and the probe's iwinfo
// reading is then the only evidence of what the radio actually picked —
// dropping it would go silent on an ACS-selected DFS channel, which is the
// case the warning is most for.
func withLiveChannels(caps *capability.Registry, existing Existing) *capability.Registry {
	if caps == nil {
		return nil
	}
	wireless := existing.In("wireless")
	if len(wireless) == 0 {
		return caps
	}
	out := *caps
	out.Radios = append([]capability.Radio(nil), caps.Radios...)
	for i := range out.Radios {
		sec, ok := wireless[radioSection(out.Radios[i])]
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(sec["channel"])); err == nil && n > 0 {
			out.Radios[i].Channel = n
		}
	}
	return &out
}

// undetermined reports a feature whose state could not be established.
//
// The whole reason Retain exists: Absent and NotObservable both render
// nothing, and only one of them is a decision.
func undetermined(caps *capability.Registry, f capability.Feature) bool {
	return caps != nil && !caps.State(f).Decided()
}

// retain marks a section Prune must not delete, and tells the operator when
// that actually protected something.
//
// The message is emitted only when the section is really there and really
// ours. A note saying "the existing X was left alone" about a device that has
// no X is the kind of reassurance that teaches people to stop reading these.
func (d *Doc) retain(rep *Report, existing Existing, name, reason string) {
	d.Retain = append(d.Retain, SectionRef{Config: "wireless", Name: name})
	if existing.OwnedIn("wireless", name) {
		rep.Omissions = append(rep.Omissions, Omission{
			WLAN: name, Reason: reason, Kind: KindUndetermined})
	}
}

// reportBlind says which of our sections survive only because this render
// could not see well enough to decide about them.
//
// Without it the preview reads "no changes" for a device whose config we can no
// longer account for, which is a clean bill of health from a check that did not
// run — the same silence the hardware-unidentified warning exists to break.
func reportBlind(doc *Doc, rep *Report, existing Existing) {
	wanted := map[SectionRef]bool{}
	for _, s := range doc.Sections {
		wanted[SectionRef{s.Config, s.Name}] = true
	}
	for _, config := range doc.Blind {
		var kept []string
		for name := range existing.In(config) {
			if !wanted[SectionRef{config, name}] && existing.OwnedIn(config, name) {
				kept = append(kept, name)
			}
		}
		if len(kept) == 0 {
			continue
		}
		sort.Strings(kept)
		rep.Omissions = append(rep.Omissions, Omission{
			WLAN: config, Kind: KindUndetermined,
			Reason: fmt.Sprintf("%s configuration on this device could not be "+
				"determined, so these sections we own are left exactly as they "+
				"are rather than removed: %s. Nothing here is a statement that "+
				"they are unwanted — it means the check that would have decided "+
				"never ran", config, strings.Join(kept, ", ")),
		})
	}
}

// wantsVLAN reports whether the site asks for any wired VLAN at all.
//
// Blindness about the port layout only matters when something needed it. With
// no VLAN network in the model, rendering nothing into network/dhcp/firewall
// is a decision about the model rather than ignorance about the device, and
// pruning our stale sections there stays correct.
func wantsVLAN(site model.Site) bool {
	for _, n := range site.Networks {
		if n.Enabled && n.VLAN > 1 {
			return true
		}
	}
	return false
}

// addOwned appends a section unless the device already has one with that name
// that we did not write.
//
// The ownership rule, in the one place all the wired sections pass through: a
// section with our name that is not ours aborts rather than being overwritten.
func addOwned(doc *Doc, rep *Report, existing Existing, sec Section) {
	if vals, exists := existing.In(sec.Config)[sec.Name]; exists && vals[OwnershipTag] != "1" {
		rep.Conflicts = append(rep.Conflicts, Conflict{
			Config: sec.Config, Section: sec.Name,
			Reason: "a section with our name exists but is not ours; " +
				"refusing to overwrite config we did not write",
		})
		return
	}
	doc.Sections = append(doc.Sections, sec)
}

// gateKind classifies a refused gate by whether it decided anything.
func gateKind(caps *capability.Registry, f capability.Feature) OmissionKind {
	if undetermined(caps, f) {
		return KindUndetermined
	}
	return KindUnclassified
}

// radioKind classifies a missing band by whether we know it is missing.
func radioKind(radiosUnknown bool) OmissionKind {
	if radiosUnknown {
		return KindUndetermined
	}
	return KindUnclassified
}
