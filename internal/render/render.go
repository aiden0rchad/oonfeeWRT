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

// Report accompanies every render.
type Report struct {
	Omissions []Omission
	Conflicts []Conflict
}

// HasConflicts reports whether the render must not proceed.
func (r Report) HasConflicts() bool { return len(r.Conflicts) > 0 }

// Existing describes what is already on the device, so the renderer can detect
// collisions with foreign config. Supply what a `uci get wireless` returned.
type Existing struct {
	// WifiIfaces maps section name -> its values, including any ownership tag.
	WifiIfaces map[string]map[string]string
}

// Owned reports whether an existing section carries our marker.
func (e Existing) Owned(section string) bool {
	return e.WifiIfaces[section][OwnershipTag] == "1"
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

	radios := radiosByBand(caps)
	for _, w := range site.WLANsFor(dev.ID) {
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
			if vals, exists := existing.WifiIfaces[name]; exists && vals[OwnershipTag] != "1" {
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
	names := make([]string, 0, len(e.WifiIfaces))
	for name := range e.WifiIfaces {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic conflict reporting
	for _, name := range names {
		if name == ours {
			continue
		}
		vals := e.WifiIfaces[name]
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
		band, ok := model.BandForFrequency(r.Frequency)
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
