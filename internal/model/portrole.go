package model

import (
	"fmt"
	"unicode"
)

// PortRoleMode is how one LAN port carries the site's VLANs.
//
// Trunk is the historical render: the port stays an untagged member of the
// device's existing VLAN 1, and every VLAN this controller adds is tagged.
// Access puts the port untagged on exactly one site network whose VLAN is
// greater than 1, and takes it out of the others — including VLAN 1 — so a
// switch that does not understand tags sees only that network.
type PortRoleMode string

const (
	PortRoleTrunk  PortRoleMode = "trunk"
	PortRoleAccess PortRoleMode = "access"
)

// PortRole is one LAN port on one device. A missing row means trunk, and the
// renderer treats that absence as "do not edit VLAN 1".
type PortRole struct {
	DeviceID  int64
	Port      string
	Mode      PortRoleMode
	NetworkID int // access target; zero for trunk
}

// ValidLANPortName reports whether port can be a UCI bridge-vlan member name.
//
// The suffix after ':' is netifd's tagged/PVID marker, so a name that itself
// contains ':' would be parsed as flags. Spaces would split one list element
// into two. WAN is not rejected here — the caller knows which name is the
// uplink, and this check is only the shape.
func ValidLANPortName(port string) bool {
	if port == "" || len(port) > 32 {
		return false
	}
	for i, r := range port {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
		case i > 0 && (r == '_' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// ValidateShape checks the row by itself. It does not look up the network or
// the device's switch ports; those need the site and the capability record.
func (r PortRole) ValidateShape() error {
	if r.DeviceID <= 0 {
		return errf("a LAN port role needs a device")
	}
	if !ValidLANPortName(r.Port) {
		return errf("port %q is not a switch port name", r.Port)
	}
	switch r.Mode {
	case PortRoleTrunk:
		if r.NetworkID != 0 {
			return errf("port %q is a trunk and cannot also name a network", r.Port)
		}
	case PortRoleAccess:
		if r.NetworkID <= 0 {
			return errf("port %q is an access port and needs a network", r.Port)
		}
	default:
		return errf("port %q role %q is neither trunk nor access", r.Port, r.Mode)
	}
	return nil
}

// ValidatePortRoles checks the roles against each other and against the
// networks they name.
//
// A target whose VLAN is 0 or 1 is not a structural error. The network can be
// edited after the role is saved, and failing the whole site here would block
// every other change. The renderer leaves that port a trunk and says so.
func (s Site) ValidatePortRoles() []error {
	var errs []error
	seen := map[string]struct{}{}
	for _, role := range s.PortRoles {
		if err := role.ValidateShape(); err != nil {
			errs = append(errs, err)
			continue
		}
		key := fmt.Sprintf("%d\x00%s", role.DeviceID, role.Port)
		if _, dup := seen[key]; dup {
			errs = append(errs, errf("device %d port %q has more than one LAN role", role.DeviceID, role.Port))
		}
		seen[key] = struct{}{}
		if role.Mode != PortRoleAccess {
			continue
		}
		if _, ok := s.NetworkByID(role.NetworkID); !ok {
			errs = append(errs, errf("device %d port %q is an access port of unknown network %d",
				role.DeviceID, role.Port, role.NetworkID))
		}
	}
	return errs
}

// PortRoleFor returns the saved role for one port. The bool is false when the
// port has no row, which callers must treat as trunk rather than as an error.
func (s Site) PortRoleFor(deviceID int64, port string) (PortRole, bool) {
	for _, role := range s.PortRoles {
		if role.DeviceID == deviceID && role.Port == port {
			return role, true
		}
	}
	return PortRole{}, false
}
