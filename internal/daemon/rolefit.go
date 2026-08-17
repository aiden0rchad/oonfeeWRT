package daemon

import (
	"fmt"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// Does the role an operator chose match the hardware the probe found?
//
// # Why this warns and never refuses
//
// The role is the operator's statement of intent and the probe is a snapshot.
// They can disagree for entirely good reasons — radios switched off today and
// wanted tomorrow, a board file that under-reports, an old router being
// prepared before it is cabled. Refusing adoption over a mismatch would turn a
// note into a wall, and the operator is the one who knows which of the two is
// wrong.
//
// What it must not do is stay quiet. Adopting an old router as an access point
// and getting silence — no WLANs, no error, a preview that renders nothing —
// is the failure this exists to prevent, and it is the likeliest failure when
// the whole point is repurposing hardware nobody has catalogued.
//
// # Why "no radios" is not one condition
//
// An empty radio list means either "this device has none" or "we could not
// ask": `probeRadios` returns early with the wireless features NotObservable
// when `iwinfo.devices` is refused, and the list stays empty either way. Those
// are different messages — one says the role is wrong, the other says the ACL
// is narrow — and telling an operator to change the role when the real problem
// is a refused call sends them to fix the wrong thing.
func roleFit(role model.Role, caps *capability.Registry) []string {
	if caps == nil {
		return nil
	}
	out := hardwareDefectWarnings(caps)
	radios := len(caps.Radios)
	// FeatSurvey is the tell. probeRadios sets it NotObservable on the
	// early-return path and resolves it from evidence otherwise, so it
	// separates "asked, none" from "could not ask" without a second call.
	wirelessObservable := caps.State(capability.FeatSurvey) != capability.NotObservable

	switch {
	case role.Wireless() && radios == 0 && wirelessObservable:
		out = append(out, fmt.Sprintf(
			"adopted as %q, but this device reported no radios. No WLAN will "+
				"render on it. If that is a surprise, its radios may be disabled "+
				"— enable one and re-probe from the device screen. If it is not, "+
				"%q is the role that matches what it can do",
			role, model.RoleSwitch))

	case role.Wireless() && radios == 0:
		out = append(out, fmt.Sprintf(
			"adopted as %q, but its radios could not be listed — the check was "+
				"refused rather than answered, so whether it has any is unknown. "+
				"This is an access-control gap on the device, not a wrong role",
			role))

	case !role.Wireless() && radios > 0:
		// Not a problem — it is the intended use of the role — but worth
		// confirming, because "I adopted it and it never broadcast" is the
		// question this pre-empts.
		out = append(out, fmt.Sprintf(
			"adopted as %q with %d radio(s) present. No WLAN will be sent to it: "+
				"that is what this role means. Adopt it as %q if you want it to "+
				"broadcast", role, radios, model.RoleAP))
	}

	if role.Routes() {
		portsObservable := caps.Ports.Bridge != "" || len(caps.Ports.LAN) > 0
		if caps.Ports.WAN == "" && portsObservable {
			out = append(out, "adopted as a gateway, but its board declares no "+
				"WAN port. That is normal for a device bridged onto an existing "+
				"network and wrong for one meant to route to an uplink — worth "+
				"checking which this is before applying a firewall zone to it")
		}
	}
	return out
}

// hardwareDefectWarnings names the known defects of this device's radios, at
// adoption rather than at apply.
//
// Adoption is the right moment for it. Someone repurposing an old router is
// deciding whether to build a network on it, and "this radio's driver does not
// support 802.11w, so this board cannot run WPA3" is a fact worth having before
// that decision rather than three screens later when a preview warns. It also
// reaches the case a preview never does: a device adopted and left alone still
// has the defect, and reprobe surfaces it again.
//
// Only defects no configuration triggers are listed here. A defect that fires
// on a specific setting belongs against that setting, in the preview, where the
// operator is choosing it — saying "802.11w is broken" to someone who has not
// enabled it is noise, and noise is how real warnings get ignored.
func hardwareDefectWarnings(caps *capability.Registry) []string {
	var out []string
	// Adoption is the likeliest moment for this to be unanswerable, and the
	// worst one for it to pass silently: stock OpenWrt ships its default
	// wifi-iface disabled so nothing is broadcasting,
	// iwinfo only names a radio that has an interface, so a device adopted
	// before it carries a WLAN reports no hardware at all. Matching nothing
	// would then read exactly like "no known defects".
	if len(caps.Radios) > 0 && !capability.HardwareIdentified(caps) {
		out = append(out, "this device's radios could not be checked against "+
			"the known-defect list: none reported a hardware name, which is "+
			"what happens when a radio has no interface yet. This is not a "+
			"clean bill of health. Re-probe once a WLAN is applied.")
	}
	for _, d := range capability.DefectsFor(caps) {
		if d.Configured() {
			continue
		}
		out = append(out, fmt.Sprintf(
			"this device's wireless hardware has a known defect (%s): %s. %s "+
				"Mitigation: %s Source: %s",
			d.Confidence, d.Summary, d.Detail, d.Mitigation, d.Source))
	}
	return out
}
