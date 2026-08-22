package capability

import (
	"sort"
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
	// Nil means the defect is not caused by anything this controller writes.
	Triggers func(vals map[string]string) bool

	// TriggersRadio reports whether a radio's CURRENT state hits this defect,
	// independently of anything we render.
	//
	// Needed because not every dangerous setting is one we own. The renderer
	// emits no wifi-device sections at all — channel and width are not
	// controller-managed — so a defect about the channel a radio is on cannot
	// be found by inspecting our own output. An earlier version tried, reading
	// "channel" from a wifi-iface that never carries one, and the guard could
	// therefore never fire.
	TriggersRadio func(Radio) bool
}

// Configured reports whether this defect is triggered by configuration at all,
// from either direction.
func (d Defect) Configured() bool { return d.Triggers != nil || d.TriggersRadio != nil }

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
			"and it is off by default there for that reason. Measured here rather " +
			"than merely repeated: with PMF on, the FIRST 802.11r fast-transition " +
			"roam onto the 5 GHz radio logged \"kernel reports: key addition " +
			"failed\", and 85 seconds later the radio's firmware stopped answering " +
			"— \"cmd 0x801d=MEMAddrAccess timed out\", every 20 seconds thereafter, " +
			"then hostapd blocked and every other radio on the device went with it. " +
			"Recovery needed a power cycle. The same device, same boot, had run " +
			"14h50m carrying clients on that radio with PMF off. Turning PMF back " +
			"on and forcing one roam was enough.",
		Source: "measured on a WRT3200ACM (88W8964), 2026-08-17; " +
			"https://openwrt.org/toh/linksys/wrt3200acm",
		Confidence: ConfMeasuredHere,
		Severity:   SevRadioDeath,
		Mitigation: "Either set PMF to disabled on this WLAN, or stop publishing " +
			"it on this device (the per-device \"disabled\" override). PMF cannot " +
			"be varied per device: APs in one mobility domain must agree on their " +
			"RSN capabilities or 802.11r fast transition fails intermittently " +
			"rather than cleanly, which is why security settings are deliberately " +
			"not overridable. So turning it off here turns it off for every AP " +
			"carrying this WLAN — if the others are healthy, not publishing it on " +
			"this one keeps them protected. Note also that WPA3/SAE requires PMF, " +
			"so this hardware cannot run WPA3 at all. " +
			"KEEP 802.11r: fast roaming is what exposes this, but it is not what " +
			"breaks it. The same device ran 14h50m with 802.11r on and PMF off, " +
			"through fast-transition roams that logged the identical \"key " +
			"addition failed\" line, and stayed up — the firmware only died once " +
			"PMF was added. Turning off fast roaming instead would cost you " +
			"seamless handover and would not be the fix. Whether PMF also kills " +
			"this radio with fast roaming disabled is untested and not worth " +
			"testing: the hardware's own documentation says not to enable " +
			"802.11w at all, and the failure it produces needs somebody to " +
			"physically power-cycle the device. Leave PMF off here.",
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
		Mitigation: "Use channel 36, or auto, on the 5 GHz radio. The controller " +
			"does not manage channels, so this one has to be changed on the device.",
		TriggersRadio: func(r Radio) bool { return isDFSChannel(r.Channel) },
	},
	{
		ID:       "mwlwifi-firmware-hang",
		Hardware: "marvell",
		Summary:  "this radio's firmware can stop responding, taking every radio on the device with it",
		Detail: "The driver has no firmware recovery path at all. In " +
			"hif/pcie/pcie.c (kaloz/mwlwifi db97edf2, the commit OpenWrt pins), a " +
			"host command that times out logs, sets cmd_timeout and returns -EIO; " +
			"nothing resets or re-initialises the chip, and the firmware is " +
			"re-downloaded only on PCI probe. So neither a `wifi` restart nor " +
			"re-applying config can recover it. That much is driver-wide, shared " +
			"across the 88W8864/8997/8964; the hang itself is what the 88W8964 " +
			"does in the field. Measured on the reference WRT3200ACM: the 5 GHz " +
			"firmware stops " +
			"answering (the repeating \"cmd 0x801d=MEMAddrAccess timed out\" is the " +
			"driver's own heartbeat probe failing, so it confirms the firmware is " +
			"already dead rather than causing it), hostapd blocks in " +
			"uninterruptible sleep, and because nl80211 operations serialise, every " +
			"other radio on the device stops answering too — including a healthy one. " +
			"Recovery needs a power cycle. Seen at 17, 28 and 50 minutes after boot on " +
			"one device; it is not known how widely it affects this model.",
		Source:     "STATUS.md §5aa; hif/pcie/pcie.c in kaloz/mwlwifi db97edf2",
		Confidence: ConfMeasuredHere,
		Severity:   SevRadioDeath,
		Mitigation: "Power cycle. Do NOT try `rmmod mwlwifi; modprobe mwlwifi` — it " +
			"is the most commonly repeated remedy and it was measured here to make " +
			"things worse, leaving modprobe hung with no radios at all and still " +
			"needing the reboot. Nothing preventive is proven. Both observed wedges " +
			"were preceded by a key-install failure on a client during an 802.11r " +
			"association, so disabling PMF and fast transition on this hardware is " +
			"worth trying, but it is untested and the correlation is two samples.",
		// No Triggers: nothing in the configuration causes it, so it is reported
		// against the device rather than against a WLAN.
	},
}

// isDFSChannel reports a 5 GHz channel in a band that requires radar detection.
//
// Channels 52–64 and 100–144 are DFS in both ETSI and FCC regions. 149+ and
// 36–48 are not. Zero means the channel is not known — a radio with no
// interface reports none — and is deliberately not treated as DFS: "we could
// not tell" must not be rendered as a specific accusation.
func isDFSChannel(ch int) bool {
	return (ch >= 52 && ch <= 64) || (ch >= 100 && ch <= 144)
}

// HardwareIdentified reports whether ANY radio told us what it is.
//
// This gates every claim this file makes, and it has to, because the hardware
// name comes from `iwinfo.info` and iwinfo only answers for a radio that has an
// interface. Stock OpenWrt ships with its default wifi-iface disabled, so
// nothing on a freshly flashed router is broadcasting and a freshly adopted
// router reports no hardware at all — and matching nothing would otherwise be
// indistinguishable from "this hardware has no known defects".
//
// That is this package's cardinal error reaching the defect registry, and fresh
// adoption is the worst possible moment for it: it is exactly when an operator
// is choosing the security settings the registry exists to warn about.
func HardwareIdentified(r *Registry) bool {
	if r == nil {
		return false
	}
	for _, radio := range r.Radios {
		if strings.TrimSpace(radio.Hardware) != "" {
			return true
		}
	}
	return false
}

// DefectsFor returns the known defects of the radios this device actually has.
//
// An empty result is not a clean bill of health. It means either that nothing
// in the registry matched — the registry covers only what somebody has written
// down with a source — or that no radio said what it was, which callers must
// distinguish with HardwareIdentified before reporting anything reassuring.
func DefectsFor(r *Registry) []Defect {
	if r == nil {
		return nil
	}
	hardware := make([]string, 0, len(r.Radios)+1)
	for _, radio := range r.Radios {
		hardware = append(hardware, radio.Hardware)
	}
	// Factory-default WRT3200ACM radios have no wifi-iface, so iwinfo cannot
	// name their 88W8964 hardware until after the first WLAN is enabled. The
	// board identity is already authoritative; use it so Preview warns before
	// that first Apply rather than discovering the defect after exposure.
	board := strings.ToLower(r.Board.Model + " " + r.Board.BoardName)
	if strings.Contains(board, "wrt3200acm") {
		hardware = append(hardware, "Marvell 88W8964")
	}
	var out []Defect
	seen := map[string]bool{}
	for _, name := range hardware {
		hw := strings.ToLower(name)
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

// MayAffect reports whether a defect can apply to one radio.
//
// Three-state in effect, collapsed to the only safe boolean: a radio whose
// hardware matches, and a radio that has not said what it is, both count. Only
// a radio KNOWN to be something else is excluded.
//
// That asymmetry is deliberate and was checked before it was written. A plain
// per-radio filter looks obviously right and is a trap: on a homogeneous
// Marvell board whose second radio has no interface, that radio's Hardware is
// "" — the §5ab case HardwareIdentified exists for — and filtering it out
// silences a real warning on the reference device. Going quiet about a defect
// because a radio did not identify itself is the cardinal error, reached by the
// same road §5ab already documents. Warning about a radio that is genuinely a
// different chip is merely noise, and only that case is removed here.
func (d Defect) MayAffect(r Radio) bool {
	hw := strings.ToLower(strings.TrimSpace(r.Hardware))
	// iwinfo answers with a placeholder when it cannot name the part, and a
	// placeholder is not an identification. "Generic MAC80211" is what the
	// lab's own Archer C6 reports for one of its two radios — so treating it
	// as "a different chip" dropped every Marvell defect for that radio on a
	// device the probe had already established was Marvell.
	if hw == "" || strings.HasPrefix(hw, "generic") || hw == "unknown" {
		return true // unidentified: cannot be ruled out
	}
	return strings.Contains(hw, strings.ToLower(d.Hardware))
}

// TriggeredBy returns the defects a rendered wifi-iface's values would hit.
//
// Called at render time, so the operator is told before an apply lands rather
// than after their radio has stopped. Unconditional hardware defects are
// excluded here — they belong against the device, and repeating them on every
// WLAN would bury the ones the operator can actually act on.
func TriggeredBy(r *Registry, vals map[string]string) []Defect {
	return TriggeredByOn(r, vals, nil)
}

// TriggeredByOn is TriggeredBy scoped to the radio the section is written to.
//
// A nil radio means "we do not know which radio this lands on", and then every
// defect of the device is considered — the old behaviour, kept because silence
// is the worse error. Passing the radio removes the false positive that
// otherwise accuses a WLAN on an Atheros radio of a Marvell driver's defects,
// which is exactly what a mixed-silicon device produced.
func TriggeredByOn(r *Registry, vals map[string]string, radio *Radio) []Defect {
	var out []Defect
	for _, d := range DefectsFor(r) {
		if d.Triggers == nil || !d.Triggers(vals) {
			continue
		}
		if radio != nil && !d.MayAffect(*radio) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// TriggeredByRadios returns the defects the device's CURRENT radio state hits,
// whether or not this controller put it in that state.
func TriggeredByRadios(r *Registry) []Defect {
	if r == nil {
		return nil
	}
	var out []Defect
	seen := map[string]bool{}
	for _, d := range DefectsFor(r) {
		if d.TriggersRadio == nil {
			continue
		}
		for _, radio := range r.Radios {
			// Only radios the defect can apply to. A 2.4 GHz Marvell radio
			// cannot be on a DFS channel at any value, and the warning used to
			// fire anyway — sourced from the 5 GHz Atheros radio beside it.
			if !d.MayAffect(radio) {
				continue
			}
			if !seen[d.ID] && d.TriggersRadio(radio) {
				seen[d.ID] = true
				out = append(out, d)
			}
		}
	}
	return out
}
