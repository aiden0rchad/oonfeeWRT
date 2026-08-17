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

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// OwnershipTag marks every section we create.
const OwnershipTag = "oonfeewrt"

// NamePrefix distinguishes our sections at a glance in /etc/config, which
// matters when a human is reading the file over SSH wondering what touched it.
const NamePrefix = "oowrt"

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

// Doc is everything we intend to exist on one device.
type Doc struct {
	DeviceID int64
	Sections []Section
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

// Omission records something the operator asked for that this device cannot
// do. These are shown in the diff preview: silently dropping a requested SSID
// is how a controller loses trust.
type Omission struct {
	WLAN   string
	Reason string
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

	// Networks first: a WLAN attaches to one, and a config file that declares
	// the interface before the wireless section referencing it reads the way a
	// human would write it.
	for _, n := range site.Networks {
		secs, oms := renderNetwork(n, dev, caps, existing)
		for _, sec := range secs {
			// Same ownership rule as wireless: a section with our name that is
			// not ours aborts rather than being overwritten.
			if vals, exists := existing.In(sec.Config)[sec.Name]; exists && vals[OwnershipTag] != "1" {
				rep.Conflicts = append(rep.Conflicts, Conflict{
					Config: sec.Config, Section: sec.Name,
					Reason: "a section with our name exists but is not ours; " +
						"refusing to overwrite config we did not write",
				})
				continue
			}
			doc.Sections = append(doc.Sections, sec)
		}
		rep.Omissions = append(rep.Omissions, oms...)
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

	radios := radiosByBand(caps)
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
					WLAN:   w.SSID,
					Reason: fmt.Sprintf("device has no %s radio", band),
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
			for _, d := range capability.TriggeredBy(caps, sec.Values) {
				rep.Warnings = append(rep.Warnings, warn(w.SSID, d))
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
				if sec.Name != "" {
					doc.Sections = append(doc.Sections, sec)
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
		if sec.Name != "" {
			doc.Sections = append(doc.Sections, sec)
		}
	}

	// Defects of the hardware itself, reported once against the device. Kept
	// separate from the per-WLAN ones because nothing the operator changes will
	// make them go away, and repeating them on every SSID would bury the
	// warnings they can actually act on.
	for _, d := range capability.DefectsFor(caps) {
		if !d.Configured() {
			rep.Warnings = append(rep.Warnings, warn("", d))
		}
	}
	// Defects triggered by the radio's CURRENT state rather than by anything we
	// write — a DFS channel, say. The controller does not manage channels, so
	// these can only be found by looking at the device.
	for _, d := range capability.TriggeredByRadios(caps) {
		rep.Warnings = append(rep.Warnings, warn("", d))
	}
	// And when nothing could be checked at all, say so. Silence here would be a
	// clean bill of health from a check that never ran: the hardware name comes
	// from iwinfo, iwinfo only answers for a radio with an interface, and stock
	// OpenWrt ships its radios disabled. A freshly adopted router is therefore
	// the case most likely to look defect-free and the case where the operator
	// is choosing the settings this exists to warn about.
	if caps != nil && len(caps.Radios) > 0 && !capability.HardwareIdentified(caps) {
		rep.Warnings = append(rep.Warnings, Warning{
			DefectID: "hardware-unidentified",
			Summary: "this device's radios could not be checked against the " +
				"known-defect list",
			Detail: "No radio reported a hardware name. That comes from iwinfo, " +
				"which only answers for a radio that has an interface, so a device " +
				"whose radios are still disabled cannot be identified. This is not " +
				"a clean bill of health — it means the check did not run.",
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
	if w.Security.PMF != "" {
		v["ieee80211w"] = string(w.Security.PMF)
	}
	// WPA3 mandates protected management frames; rendering sae without it
	// produces an AP that clients reject for reasons nobody enjoys debugging.
	if w.Security.Mode == model.SecSAE && v["ieee80211w"] != string(model.PMFRequired) {
		v["ieee80211w"] = string(model.PMFRequired)
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
