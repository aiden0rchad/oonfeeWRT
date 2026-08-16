package model

import "fmt"

// A wireless uplink: a device that reaches the network over the air rather than
// over a cable.
//
// # What this is for
//
// The stated direction of this project is that an old router lying in a drawer
// can be flashed with OpenWrt, adopted, and used to extend a network. The
// awkward half of that is the router in the room with no ethernet run to it. A
// mesh solves it when both ends can carry 802.11s; this solves it when they
// cannot — which, measured, includes one of the two devices this project has
// (§5q), and will include plenty of the old hardware the goal is aimed at.
//
// # Why it is a device property and not a "bridge" object
//
// The obvious modelling is a Bridge with two ends. It is worse, for the same
// reason a mesh is not a device role (§5n): the two ends are not symmetric and
// do not belong to the same owner. The AP end is a property of a WLAN — "this
// network accepts wireless bridges" — and applies to every AP publishing it.
// The station end is a property of one device — "this one has no cable". A
// Bridge object would force an operator to describe a relationship where the
// real decision is two independent facts, and would break the moment a second
// device wanted to join the same network the same way.
//
// # The hazard the renderer must not be quiet about
//
// A station bridged into `br-lan` on a device that is ALSO cabled creates a
// layer-2 loop. OpenWrt's `br-lan` ships with STP off, so nothing breaks it.
// This is the §5g shape — a change that applies cleanly, reports healthy, and
// takes a network down — and the renderer says so on the preview rather than
// after.
type Uplink struct {
	ID int
	// DeviceID is the device that will JOIN. One per device: a router with two
	// wireless uplinks is not a supported arrangement and would loop.
	DeviceID int64
	// WLANID is the network it joins. Deliberately an existing WLAN rather than
	// a free-text SSID: the passphrase, the security mode and the band then
	// come from one place, and a bridge cannot drift out of sync with the
	// network it is bridging to — which is the failure a controller exists to
	// prevent.
	WLANID int
	// Band the station radio should use. A device joins on one band; the other
	// radio stays free to serve clients, which is the entire point of putting
	// an old router in that room.
	Band    Band
	Enabled bool
}

// Validate reports what would make this uplink unusable.
func (u Uplink) Validate(site Site) []error {
	var errs []error
	if u.DeviceID == 0 {
		errs = append(errs, fmt.Errorf("a wireless uplink needs a device"))
	}
	w, ok := site.WLANByID(u.WLANID)
	if !ok {
		errs = append(errs, fmt.Errorf("wireless uplink: no such network (%d)", u.WLANID))
		return errs
	}
	if !w.Enabled {
		errs = append(errs, fmt.Errorf(
			"wireless uplink: %q is disabled, so there would be nothing to join", w.SSID))
	}
	// The AP end has to accept it. Without this the station is configured, the
	// APs are not, and the link silently never forms — which looks exactly like
	// a driver refusing it and would send an operator to the wrong place.
	if !w.Options.AllowUplink {
		errs = append(errs, fmt.Errorf(
			"wireless uplink: %q does not accept wireless bridges. Turn that on "+
				"for the network, or the access points will not carry the "+
				"4-address frames this needs and the link will never form", w.SSID))
	}
	// A device cannot bridge to a network it publishes itself.
	//
	// Found on hardware: the station came up in Client mode on the same radio
	// that was already serving that SSID, and never associated — channel 0,
	// signal 0, silence. Nothing in the config is wrong to look at, which is
	// what makes it worth refusing rather than warning about: it is
	// indistinguishable from a driver refusing 4-address framing, and an
	// operator would spend the afternoon on the wrong question.
	//
	// The check is group membership rather than "is this SSID on the air here",
	// because the site model is what the controller controls and the air is
	// not.
	for _, g := range site.Groups {
		if g.ID == w.GroupID && g.Contains(u.DeviceID) {
			errs = append(errs, fmt.Errorf(
				"wireless uplink: this device publishes %q itself, so it cannot "+
					"also join it — a station and an access point for one network "+
					"on one device is not a bridge to anywhere. Remove the device "+
					"from that network's AP group, or bridge it to a different "+
					"network", w.SSID))
		}
	}
	if !w.PublishesOn(u.Band) {
		errs = append(errs, fmt.Errorf(
			"wireless uplink: %q is not published on %s, so there is nothing to "+
				"join on that band", w.SSID, u.Band))
	}
	return errs
}

// UplinkFor returns the uplink configured for a device, if any.
func (s Site) UplinkFor(deviceID int64) (Uplink, bool) {
	for _, u := range s.Uplinks {
		if u.DeviceID == deviceID && u.Enabled {
			return u, true
		}
	}
	return Uplink{}, false
}

// WLANByID looks up a WLAN.
func (s Site) WLANByID(id int) (WLAN, bool) {
	for _, w := range s.WLANs {
		if w.ID == id {
			return w, true
		}
	}
	return WLAN{}, false
}
