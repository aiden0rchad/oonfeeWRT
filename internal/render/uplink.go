package render

import (
	"fmt"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// Rendering a wireless uplink: the station half of a 4-address (WDS) bridge.
//
// A `wifi-iface` with `mode 'sta'` and `wds '1'`, joining a WLAN this site
// already publishes, bridged into the same network its wired ports are on. The
// AP half is a single `wds '1'` on the access points publishing that WLAN, and
// lives in renderWifiIface — see WLANOptions.AllowUplink.
//
// # Why the credentials are not repeated here
//
// The station joins by SSID and passphrase like any client, and both are taken
// from the WLAN rather than restated on the uplink. That is the same rule
// override.go enforces for APs and for the same reason: two copies of a
// passphrase drift, and a bridge whose key no longer matches the network does
// not fail cleanly — it fails the way a client with a stale password fails,
// intermittently and at the worst moment.

// renderUplink produces the station wifi-iface for one device.
func renderUplink(u model.Uplink, w model.WLAN, net model.Network, radio string,
	caps *capability.Registry) (Section, []Omission) {

	if ok, reason := UplinkGate(caps); !ok {
		return Section{}, []Omission{{WLAN: w.SSID, Reason: reason,
			Kind: gateKind(caps, capability.FeatWirelessUplink)}}
	}

	v := map[string]string{
		"device":  radio,
		"mode":    "sta",
		"ssid":    w.SSID,
		"network": net.Name,
		// The whole feature. Without 4-address framing this is an ordinary
		// client: the device itself gets on the network and nothing behind it
		// does, so the wired ports an operator put it there for stay dark.
		"wds":              "1",
		OwnershipTag:       "1",
		"encryption":       string(w.Security.Mode),
		"disassoc_low_ack": "0",
	}
	if w.Security.Mode.NeedsKey() {
		v["key"] = w.Security.Key
	}
	if w.Security.PMF != "" {
		v["ieee80211w"] = string(w.Security.PMF)
	}

	// The loop, said before the fact.
	//
	// This is the §5g shape: a change that applies cleanly, reports healthy,
	// and can take a network down. Bridging a station into the same bridge the
	// device's wired ports are on is a layer-2 loop the moment that device is
	// ALSO cabled, and OpenWrt's br-lan ships with STP off, so nothing breaks
	// it. The controller cannot see the far end of a cable and must not pretend
	// to — so it does not refuse, it states the condition and leaves the
	// operator, who can see the room, to decide.
	omissions := []Omission{{
		WLAN: w.SSID, Kind: KindCaution,
		Reason: "this device will join " + w.SSID + " as a wireless bridge. If it " +
			"is ALSO connected by ethernet to the same network, that is a " +
			"layer-2 loop — OpenWrt bridges ship with STP off, so nothing will " +
			"break it and the symptom is a network that stops working rather " +
			"than an error. Unplug the cable, or enable STP, before applying",
	}}

	return Section{
		Config: "wireless", Type: "wifi-iface",
		Name:   uplinkIfaceName(u.ID, radio),
		Values: v,
	}, omissions
}

// uplinkIfaceName is deterministic, so a re-render targets the same section
// rather than accumulating one per apply. Same shape as ifaceName and
// meshIfaceName.
func uplinkIfaceName(uplinkID int, radio string) string {
	return fmt.Sprintf("%s_up%d_%s", NamePrefix, uplinkID, radio)
}

// UplinkGate decides whether a device may be given a wireless uplink, and says
// why when it may not.
//
// Exported for the same reason MeshGate is: the apply preview and any health
// readout must say the same sentence about the same device.
//
// # What Present does and does not mean
//
// FeatWirelessUplink is decided from whether a supplicant is installed, which
// is the half a package list can settle. Whether the RADIO will carry a
// 4-address station is not settled by anything the ACL can reach — `iw phy
// info` would answer it and is not grantable (§5m). So this gate lets a
// configuration through that the driver may still refuse, exactly as the mesh
// gate did before §5q taught it otherwise.
//
// That is a deliberate difference from MeshGate, not an oversight. Mesh has a
// measured quirk to gate on; this has none yet, and inventing one would be a
// guess wearing a measurement's clothes. What it does instead is warn — the
// renderer's omission says a station that never associates is the expected
// shape of that failure, so the first person to hit it knows what they are
// looking at rather than rediscovering §5q from scratch.
func UplinkGate(caps *capability.Registry) (bool, string) {
	st := caps.State(capability.FeatWirelessUplink)
	switch {
	case st == capability.Present:
		return true, ""
	case st == capability.NotObservable:
		return false, "whether this device can join a network over the air could " +
			"not be established — the installed-package list, which is the only " +
			"source that says whether a supplicant is present, could not be " +
			"read. That is a gap in what the controller can see, not a " +
			"statement about the device. " + readoptFix
	case !st.Decided():
		return false, "this device's capability record has no answer about " +
			"joining a network over the air: the check never ran here, most " +
			"often because the record predates it. Re-probe the device from its " +
			"screen, and re-adopt it if that does not fill the answer in"
	}
	return false, "this device has no wireless supplicant installed, so it can " +
		"serve a network and cannot join one. Installing a wpad-* package would " +
		"provide it — which the controller does not do, because it installs " +
		"nothing on your devices"
}
