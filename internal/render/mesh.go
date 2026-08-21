package render

import (
	"fmt"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// Rendering an 802.11s mesh point.
//
// A `wifi-iface` like any other, with `mode 'mesh'` instead of `'ap'` and a
// `mesh_id` instead of an `ssid`. It renders alongside the AP interfaces on the
// same device rather than instead of them: a node carrying a mesh backhaul
// while still serving clients is the intended arrangement, not an edge case.

// renderMesh produces the wifi-iface for one mesh on one device.
func renderMesh(m model.Mesh, net model.Network, radio string,
	caps *capability.Registry) (Section, []Omission) {

	// The capability gate, and the reason it is three-state.
	//
	// FeatMesh comes from which wpad build is installed, because nothing else
	// on the device answers the question. A build that is not named for its
	// feature set records NotObservable — and rendering into that would send an
	// interface hostapd may refuse at startup, which surfaces as a radio that
	// silently does not come up. So only Present renders, and the other two
	// states say which one they are: "your device cannot" and "we could not
	// find out" send an operator to completely different places.
	if ok, reason := MeshGate(caps); !ok {
		// Classified here rather than by changing MeshGate's signature, which
		// two screens share. A gate that could not establish the answer is an
		// undetermined omission, not a hardware limit.
		return Section{}, []Omission{{WLAN: m.MeshID, Reason: reason,
			Kind: gateKind(caps, capability.FeatMesh)}}
	}

	v := map[string]string{
		"device":  radio,
		"mode":    "mesh",
		"mesh_id": m.MeshID,
		"network": networkAttachmentName(net),
		// Forwarding between peers is what makes a mesh a mesh rather than a
		// set of nodes that can each see the gateway. Off by default in some
		// builds, so it is set explicitly.
		"mesh_fwding": "1",
		OwnershipTag:  "1",
	}

	var omissions []Omission
	if m.Open() {
		v["encryption"] = "none"
		// Not a refusal — an open mesh is a legitimate choice on a wired-equivalent
		// trusted segment — but it is worth saying once, on the screen where the
		// decision is visible, because the consequence is not obvious: joining a
		// mesh is joining the network behind it.
		omissions = append(omissions, Omission{
			WLAN: m.MeshID, Kind: KindCaution,
			Reason: "this mesh is unencrypted: any device in radio range can peer " +
				"with it and reach the network behind it. Set a passphrase unless " +
				"that is deliberate",
		})
	} else {
		// 802.11s uses SAE, and SAE mandates protected management frames.
		// Rendering one without the other produces peers that refuse each other
		// for reasons nobody enjoys debugging.
		v["encryption"] = "sae"
		v["key"] = m.Key
		v["ieee80211w"] = string(model.PMFRequired)
	}

	return Section{
		Config: "wireless", Type: "wifi-iface",
		Name:   meshIfaceName(m.ID, radio),
		Values: v,
	}, omissions
}

// meshIfaceName is deterministic, so a re-render targets the same section
// rather than accumulating a new one per apply. Same shape as ifaceName, which
// already receives the resolved radio section name.
func meshIfaceName(meshID int, radio string) string {
	return fmt.Sprintf("%s_mesh%d_%s", NamePrefix, meshID, radio)
}

// MeshGate decides whether an 802.11s interface may be rendered for a device,
// and says why when it may not.
//
// Exported and shared, because two different screens answer this question and
// they must not answer it differently. The apply preview reports it as an
// omission ("this WLAN was not rendered because…"); the mesh health readout
// reports it as a standing state ("this device cannot carry the mesh you have
// assigned to it"). Same fact, same sentence, one place.
//
// # Why it is three-state
//
// FeatMesh comes from which wpad build is installed, because nothing else on
// the device answers the question. A build that is not named for its feature
// set records NotObservable — and rendering into that would send an interface
// hostapd may refuse at startup, which surfaces as a radio that silently does
// not come up. So only Present passes, and the other two states say which one
// they are: "your device cannot" and "we could not find out" send an operator
// to completely different places.
//
// # Absent has two causes and they send an operator to opposite places
//
// The package cause is fixable: install wpad-mesh-*. The driver cause is not —
// the daemon already supports mesh and the radio will not run one, so the
// answer is different hardware. Telling someone to install a package they
// already have is worse than saying nothing, and that is exactly what this
// message did until a real apply exposed it (§5q).
func MeshGate(caps *capability.Registry) (bool, string) {
	st := caps.State(capability.FeatMesh)
	switch {
	case st == capability.Present:
		return true, ""
	case st == capability.NotObservable:
		return false, "802.11s support could not be established on this device, " +
			"so no mesh interface is rendered. The check reads which wpad build " +
			"is installed and could not; that is a gap in what the controller " +
			"can see, not a statement that the device lacks mesh. " + readoptFix
	case !st.Decided():
		// No entry at all, which is not the same as a refused one and not
		// remotely the same as "no". A record written before this controller
		// knew to ask has no key for the feature, so this is what every device
		// adopted before mesh support existed reports.
		return false, "this device's capability record has no answer about " +
			"802.11s: the check never ran here, most often because the record " +
			"predates it. Re-probe the device from its screen, and re-adopt it " +
			"if that does not fill the answer in. Nothing about this says the " +
			"device lacks mesh"
	}
	if caps.HasQuirk("mac80211", "mesh-point") {
		return false, "this device's wireless driver will not run an 802.11s " +
			"mesh point. It advertises the mode and accepts the config, and " +
			"then refuses to bring the interface up — so a mesh here would " +
			"apply cleanly and never carry traffic. The 802.11s daemon is " +
			"installed and is not the problem; this needs a different radio"
	}
	return false, "this device's wpad build does not carry 802.11s. Installing " +
		"a wpad-mesh-* package would provide it — which the controller does " +
		"not do, because it installs nothing on your devices"
}
