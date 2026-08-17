package capability

import (
	"sort"
	"strconv"
	"strings"
)

// Known defects in the wireless drivers OpenWrt devices ship with.
//
// This exists because of a failure mode the rest of this package cannot reach.
// The three-state model answers "can this device do X?" by asking the device,
// and a driver that is broken in a particular way answers **yes**: it accepts
// the configuration, reports success, and then does not work. `Quirk` covers
// the narrow version of that — one field that is present and wrong — but the
// defects here are wider. They are properties of a driver that no amount of
// probing will reveal, because the device does not know it is broken.
//
// The reference case, and the reason this file exists: the OpenWrt device page
// for the WRT3200ACM says plainly not to enable 802.11w, because mwlwifi does
// not support it properly. oonfeeWRT rendered `ieee80211w=1` onto that board
// anyway, and nothing anywhere would have told the operator. The device took
// the setting without complaint.
//
// Two rules govern everything here.
//
// **Warn, never rewrite.** A controller that silently downgrades the security
// settings a user asked for is worse than one that says "this will not work on
// this hardware, here is why, here is the source". Auto-remediation would also
// make the defect invisible, and an invisible workaround becomes folklore the
// moment the driver is fixed.
//
// **Say how well it is known.** Wireless folklore is repeated far more often
// than it is verified, and half of what circulates about this hardware is
// years out of date. A finding from a device's own documentation and a finding
// from a forum post must not be presented with the same authority, so every
// entry carries its Confidence and its Source, and the UI shows both.
type Confidence string

const (
	// ConfDeviceDoc is the device's own OpenWrt page, the driver's
	// documentation, or a maintainer's statement. The strongest available.
	ConfDeviceDoc Confidence = "documented"
	// ConfMeasuredHere is a defect this project reproduced on hardware, with
	// the evidence written down in STATUS.md.
	ConfMeasuredHere Confidence = "measured"
	// ConfBugTracker is a filed, accepted issue.
	ConfBugTracker Confidence = "reported"
	// ConfAnecdote is repeated in forums with no primary source found. Shown,
	// because a user debugging at 1am deserves the lead, but never as fact.
	ConfAnecdote Confidence = "anecdotal"
)

// Severity is what the defect does when it bites.
type Severity string

const (
	// SevRadioDeath means the radio or the whole wireless stack stops. The
	// operator needs to know before applying, not after.
	SevRadioDeath Severity = "radio-death"
	// SevSilentlyIgnored means the setting is accepted and does nothing, so the
	// feature the operator thinks they enabled is not running.
	SevSilentlyIgnored Severity = "silently-ignored"
	// SevDegraded means it works worse than it should.
	SevDegraded Severity = "degraded"
)

// Defect is one known flaw, and the configuration that triggers it.
type Defect struct {
	// ID is a stable slug, so a UI can dismiss or link one without matching on
	// prose that may be reworded.
	ID string
	// Hardware is matched case-insensitively as a substring of a radio's
	// reported hardware name — "marvell 88w8964", "qualcomm atheros qca9880".
	//
	// Matched on the hardware string rather than the driver name because the
	// driver name lives in /sys, which rpcd canonicalises out of reach. That is
	// the same constraint marvellRadio() already works within.
	Hardware string
	// Summary is one line an operator reads in a warning.
	Summary string
	// Detail says what actually happens, in plain terms.
	Detail     string
	Source     string
	Confidence Confidence
	Severity   Severity
	// Mitigation is what the operator can do instead. Never applied
	// automatically — see the file comment.
	Mitigation string

	// Triggers reports whether a rendered wifi-iface would hit this defect.
	//
	// Nil means the defect is a property of the hardware itself and applies
	// whatever the configuration — those are reported once against the device
	// rather than against each WLAN.
	Triggers func(vals map[string]string) bool
}

// Configured reports whether this defect is triggered by configuration at all.
func (d Defect) Configured() bool { return d.Triggers != nil }

// knownDefects is the registry.
//
// Deliberately a small, sourced list rather than an attempt at completeness. An
// entry here makes the controller tell a user something about their hardware,
// and a wrong entry makes them disable a feature that worked. Every line needs
// a Source that a sceptical reader can check.
var knownDefects = []Defect{
	{
		ID:       "mwlwifi-80211w-unsupported",
		Hardware: "marvell",
		Summary:  "802.11w (protected management frames) is not properly supported by this radio's driver",
		Detail: "The mwlwifi driver accepts ieee80211w and does not implement it " +
			"correctly. OpenWrt's own page for this hardware says not to enable it, " +
			"and it is off by default there for that reason.",
		Source:     "https://openwrt.org/toh/linksys/wrt3200acm",
		Confidence: ConfDeviceDoc,
		Severity:   SevRadioDeath,
		Mitigation: "Either set PMF to disabled on this WLAN, or stop publishing " +
			"it on this device (the per-device \"disabled\" override). PMF cannot " +
			"be varied per device: APs in one mobility domain must agree on their " +
			"RSN capabilities or 802.11r fast transition fails intermittently " +
			"rather than cleanly, which is why security settings are deliberately " +
			"not overridable. So turning it off here turns it off for every AP " +
			"carrying this WLAN — if the others are healthy, not publishing it on " +
			"this one keeps them protected. Note also that WPA3/SAE requires PMF, " +
			"so this hardware cannot run WPA3 at all.",
		Triggers: func(v map[string]string) bool {
			return v["ieee80211w"] != "" && v["ieee80211w"] != "0"
		},
	},
	{
		ID:       "mwlwifi-wpa3-unsupported",
		Hardware: "marvell",
		Summary:  "WPA3/SAE does not work reliably on this radio and can crash the router",
		Detail: "OpenWrt's page for this hardware states WPA3 will not reliably work " +
			"because of mwlwifi driver issues, that it can crash the router outright, " +
			"and that the driver is unlikely ever to support it.",
		Source:     "https://openwrt.org/toh/linksys/wrt3200acm",
		Confidence: ConfDeviceDoc,
		Severity:   SevRadioDeath,
		Mitigation: "Use WPA2-PSK (psk2) on WLANs carried by this radio.",
		Triggers: func(v map[string]string) bool {
			return strings.Contains(strings.ToLower(v["encryption"]), "sae")
		},
	},
	{
		ID:       "mwlwifi-dfs-channels",
		Hardware: "marvell",
		Summary:  "the 5 GHz radio can fail to start on DFS channels",
		Detail: "OpenWrt's page for this hardware strongly recommends channel 36 or " +
			"auto on the 5 GHz radio to avoid crashing the wifi chipset.",
		Source:     "https://openwrt.org/toh/linksys/wrt3200acm",
		Confidence: ConfDeviceDoc,
		Severity:   SevRadioDeath,
		Mitigation: "Use channel 36, or auto, on the 5 GHz radio.",
		Triggers: func(v map[string]string) bool {
			return isDFSChannel(v["channel"])
		},
	},
	{
		ID:       "mwlwifi-firmware-hang",
		Hardware: "marvell",
		Summary:  "this radio's firmware can stop responding, taking every radio on the device with it",
		Detail: "Measured on the reference WRT3200ACM: the 5 GHz firmware stops " +
			"answering (the repeating \"cmd 0x801d=MEMAddrAccess timed out\" is the " +
			"driver's own heartbeat probe failing, so it confirms the firmware is " +
			"already dead rather than causing it), hostapd blocks in " +
			"uninterruptible sleep, and because nl80211 operations serialise, every " +
			"other radio on the device stops answering too — including a healthy one. " +
			"Recovery needs a power cycle. Seen at 17, 28 and 50 minutes after boot on " +
			"one device; it is not known how widely it affects this model.",
		Source:     "STATUS.md §5aa",
		Confidence: ConfMeasuredHere,
		Severity:   SevRadioDeath,
		Mitigation: "None proven. The controller cannot recover it — that is below " +
			"the level any management protocol reaches. Both observed wedges were " +
			"preceded by a key-install failure on a client during an 802.11r " +
			"association, so disabling PMF and fast transition on this hardware is " +
			"worth trying, but it is untested and the correlation is two samples.",
		// No Triggers: nothing in the configuration causes it, so it is reported
		// against the device rather than against a WLAN.
	},
}

// isDFSChannel reports a 5 GHz channel in a band that requires radar detection.
//
// Channels 52–64 and 100–144 are DFS in both ETSI and FCC regions. 149+ and
// 36–48 are not. Deliberately conservative: an unparseable or absent channel is
// not reported as DFS, because "auto" is a real and common answer and warning
// about it would be noise.
func isDFSChannel(ch string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(ch))
	if err != nil {
		return false
	}
	return (n >= 52 && n <= 64) || (n >= 100 && n <= 144)
}

// DefectsFor returns the known defects of the radios this device actually has.
//
// An empty result is not a clean bill of health — it means nothing in the
// registry matched, and the registry covers only what somebody has written down
// with a source. A device whose radios could not be listed at all matches
// nothing here for the same reason it matches nothing anywhere else.
func DefectsFor(r *Registry) []Defect {
	if r == nil {
		return nil
	}
	var out []Defect
	seen := map[string]bool{}
	for _, radio := range r.Radios {
		hw := strings.ToLower(radio.Hardware)
		if hw == "" {
			continue
		}
		for _, d := range knownDefects {
			if seen[d.ID] || !strings.Contains(hw, strings.ToLower(d.Hardware)) {
				continue
			}
			seen[d.ID] = true
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// TriggeredBy returns the defects a rendered wifi-iface's values would hit.
//
// Called at render time, so the operator is told before an apply lands rather
// than after their radio has stopped. Unconditional hardware defects are
// excluded here — they belong against the device, and repeating them on every
// WLAN would bury the ones the operator can actually act on.
func TriggeredBy(r *Registry, vals map[string]string) []Defect {
	var out []Defect
	for _, d := range DefectsFor(r) {
		if d.Configured() && d.Triggers(vals) {
			out = append(out, d)
		}
	}
	return out
}
