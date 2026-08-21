package model

import (
	"fmt"
	"sort"
	"strings"
)

// Role is the legacy primary label for a device.
//
// # Why this is a closed vocabulary and not a string
//
// It used to be a string, stored exactly as the API received it and compared
// with `dev.Role != "gateway"`. Three things followed from that, all silent:
//
//   - "Gateway" — the obvious capitalisation — is not "gateway", so the device
//     rendered as an access point. On a gateway that means no address, no DHCP
//     server, no firewall zone and no forwarding: a VLAN with nothing on it,
//     and no error anywhere saying why.
//   - A typo produced the same outcome, and the operator's only clue was a
//     preview that did less than expected.
//   - "switch" was accepted and then never consulted, so a device adopted as a
//     switch was an access point in every respect that mattered.
//
// New devices use DeviceFunctions as the rendering authority; Role stays on
// the wire and in the database for older clients. Its closed vocabulary still
// matters because it is also the deterministic primary label for a function
// set, and legacy rows are expanded from it.
type Role string

const (
	// RoleGateway is the primary label for a set containing gateway.
	RoleGateway Role = "gateway"
	// RoleAP is the primary label for a set containing AP but not gateway.
	RoleAP Role = "ap"
	// RoleSwitch is the primary label for a switch-only set.
	RoleSwitch Role = "switch"
)

// Roles is every valid role, in the order a picker should offer them.
var Roles = []Role{RoleGateway, RoleAP, RoleSwitch}

// or resolves the zero value.
//
// Go has no way to make a named string type default to anything but "", so
// `model.Device{ID: 7}` carries an empty role — and every method has to agree
// with ParseRole that empty means RoleAP, or the same input means different
// things depending on whether it arrived through the API or a struct literal.
// Caught by the render tests, which build devices exactly that way: without
// this, every one of them silently became a non-wireless device and rendered
// nothing.
//
// Anything that is neither empty nor a known role has already been refused —
// ParseRole rejects it at the API and RoleOf normalises it on the way out of
// the database — so the residual behaviour here is for values that cannot
// occur, and it is the conservative one: send nothing.
func (r Role) or() Role {
	if r == "" {
		return RoleAP
	}
	return r
}

// Wireless reports the bundled behavior of a legacy role. New code must ask
// Device.EffectiveFunctions instead, because gateway no longer implies AP.
func (r Role) Wireless() bool {
	v := r.or()
	return v == RoleAP || v == RoleGateway
}

// Routes reports the bundled behavior of a legacy role. New code must ask
// Device.EffectiveFunctions instead.
func (r Role) Routes() bool { return r.or() == RoleGateway }

func (r Role) String() string { return string(r.or()) }

// Describe is the one-line explanation a picker shows beside the name.
func (r Role) Describe() string {
	switch r.or() {
	case RoleGateway:
		return "routes between networks and to the internet; gets addressing, " +
			"DHCP and firewall rules"
	case RoleAP:
		return "publishes WLANs and passes tagged traffic through; does not " +
			"route or serve DHCP"
	case RoleSwitch:
		return "passes tagged traffic only; no WLANs are sent to it even if it " +
			"has radios"
	}
	return ""
}

// ParseRole normalises and validates a role from the API or the database.
//
// Empty means RoleAP, which is the safe default: an access point is the role
// that changes least about a device. Guessing "gateway" for an unlabelled
// device would hand it addressing and firewall rules nobody asked for.
func ParseRole(s string) (Role, error) {
	v := Role(strings.ToLower(strings.TrimSpace(s)))
	if v == "" {
		return RoleAP, nil
	}
	for _, r := range Roles {
		if v == r {
			return r, nil
		}
	}
	names := make([]string, 0, len(Roles))
	for _, r := range Roles {
		names = append(names, string(r))
	}
	sort.Strings(names)
	return "", fmt.Errorf("%q is not a device role; valid roles are %s",
		s, strings.Join(names, ", "))
}

// RoleOf reads a stored role, falling back to RoleAP.
//
// Separate from ParseRole because a row already in the database cannot be
// rejected — refusing to load a device because its role string is unrecognised
// would make a bad write unrecoverable without hand-editing SQLite. It falls
// back to the least-privileged role instead, which is the same direction to be
// wrong in that ParseRole's empty case chooses.
func RoleOf(s string) Role {
	if r, err := ParseRole(s); err == nil {
		return r
	}
	return RoleAP
}
