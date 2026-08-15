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
	switch caps.State(capability.FeatMesh) {
	case capability.Present:
	case capability.NotObservable:
		return Section{}, []Omission{{
			WLAN: m.MeshID,
			Reason: "802.11s support could not be established on this device, so " +
				"no mesh interface is rendered. The check reads which wpad build " +
				"is installed and could not; that is a gap in what the controller " +
				"can see, not a statement that the device lacks mesh",
		}}
	default:
		return Section{}, []Omission{{
			WLAN: m.MeshID,
			Reason: "this device's wpad build does not carry 802.11s. Installing " +
				"a wpad-mesh-* package would provide it — which the controller " +
				"does not do, because it installs nothing on your devices",
		}}
	}

	v := map[string]string{
		"device":  radio,
		"mode":    "mesh",
		"mesh_id": m.MeshID,
		"network": net.Name,
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
			WLAN: m.MeshID,
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
