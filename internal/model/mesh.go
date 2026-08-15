package model

import "strings"

// An 802.11s mesh backhaul.
//
// # Why this is not a role
//
// The obvious modelling — a "mesh" device role alongside gateway/ap/switch — is
// wrong, and wrong in the way that matters for the hardware this is aimed at.
// On OpenWrt a mesh point is a `wifi-iface` with `mode 'mesh'`, and a device can
// carry one *at the same time* as an AP serving clients, on the same radio or a
// different one. That combination is the entire point of "AP bridge mesh with
// switch support": an old router extending the network over the air while still
// serving clients and its wired ports.
//
// Making mesh a role would make those mutually exclusive, and the operator
// would have to choose between the two things they want simultaneously.
//
// # Why one band and not a list
//
// A WLAN publishes on several bands because a client picks one and roams. A
// mesh does not: nodes peer only with nodes on the same band, so "the same
// mesh" on 2.4 and 5 GHz is two disjoint backhauls that cannot see each other.
// Rendering both from one record would silently produce a split network whose
// halves each look healthy.
type Mesh struct {
	ID int
	// MeshID is the 802.11s mesh identifier. Not an SSID: it is not beaconed
	// for clients, and it is what nodes match on to peer.
	MeshID    string
	NetworkID int
	GroupID   int
	Band      Band
	// Key is the SAE passphrase. Empty means an OPEN mesh — every neighbour
	// within range can join and reach the network behind it, which is why
	// Validate says so rather than treating it as an ordinary default.
	Key     string
	Enabled bool
}

// meshIDMax is the 802.11s limit, the same 32 octets as an SSID. A longer one
// is silently truncated by the driver, so two nodes that differ only past the
// cap would peer when the operator intended two meshes.
const meshIDMax = 32

// saeKeyMin is the WPA3-SAE minimum. Below it hostapd refuses the interface at
// startup, which surfaces as a radio that simply does not come up.
const saeKeyMin = 8

// Validate reports what is wrong with this mesh, in operator terms.
func (m Mesh) Validate() []error {
	var errs []error
	id := strings.TrimSpace(m.MeshID)
	switch {
	case id == "":
		errs = append(errs, errf("mesh %d needs a mesh ID — it is what nodes "+
			"match on to peer, and an empty one peers with nothing", m.ID))
	case len(id) > meshIDMax:
		errs = append(errs, errf("mesh %q has an ID longer than 32 bytes; the "+
			"driver truncates it, so two nodes differing only past the cap "+
			"would peer when you meant two separate meshes", id))
	}
	if m.Key != "" && len(m.Key) < saeKeyMin {
		errs = append(errs, errf("mesh %q has a passphrase shorter than 8 "+
			"characters; hostapd refuses the interface and the radio simply "+
			"does not come up", id))
	}
	if !m.Band.Valid() {
		errs = append(errs, errf("mesh %q needs a band — nodes peer only with "+
			"nodes on the same one", id))
	}
	return errs
}

// Open reports an unencrypted mesh, which is worth naming rather than inferring
// from an empty string at each call site.
func (m Mesh) Open() bool { return m.Key == "" }

// MeshesFor returns the enabled meshes assigned to a device via its groups, in
// stable ID order so a render is reproducible.
func (s Site) MeshesFor(deviceID int64) []Mesh {
	var out []Mesh
	for _, m := range s.Meshes {
		if !m.Enabled {
			continue
		}
		g, ok := s.GroupByID(m.GroupID)
		if !ok || !g.Contains(deviceID) {
			continue
		}
		out = append(out, m)
	}
	return out
}
