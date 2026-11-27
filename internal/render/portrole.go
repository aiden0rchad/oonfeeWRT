package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// LAN port roles, and the one foreign edit they are allowed to make.
//
// netifd's config parser (bridge_vlan_port_flags_parse) starts each member at
// BRVLAN_F_UNTAGGED. A '*' in the suffix sets BRVLAN_F_PVID. A 't' clears
// UNTAGGED. The letter 'u' is not read on the config path — only the hotplug
// parser treats it — so `lan2:u` and a bare `lan2` are both untagged, and
// `lan2:u*` is untagged with an explicit PVID. A port has one PVID: the last
// VLAN that marked it, otherwise the first VLAN where it is untagged.
//
// An access port of a VLAN above 1 is therefore written as `port:u*` on that
// VLAN and removed from every other panel VLAN. `:u` would drop the explicit
// PVID, and the next untagged membership (VLAN 1, if we failed to remove it)
// would own ingress. The token is always `:u*` so the access VLAN stays the
// PVID after we replace the list.
//
// VLAN 1 is not our section. The patch replaces only its ports list. It does
// not set vlan_filtering, does not move interface lan, and does not adopt the
// section. With no saved role on the device the function returns nothing, so
// a default trunk render is byte-for-byte the historical one.

type lanPortDecision struct {
	// Access maps a LAN port to the site network it joins untagged. Ports
	// absent from the map are trunks and are tagged on every panel VLAN.
	Access    map[string]int
	Patch     *Patch
	Omissions []Omission
}

// bridgeVLANPorts is the ports list for one of our bridge-vlan sections.
//
// WAN is never a member, even if a board description also listed it under LAN.
// A nil access map is the default trunk: every LAN port is tagged.
func bridgeVLANPorts(caps *capability.Registry, access map[string]int, networkID int) []string {
	if caps == nil {
		return nil
	}
	out := make([]string, 0, len(caps.Ports.LAN))
	for _, port := range caps.Ports.LAN {
		if port == "" || port == caps.Ports.WAN {
			continue
		}
		if target, ok := access[port]; ok {
			if target != networkID {
				continue
			}
			out = append(out, port+":u*")
			continue
		}
		out = append(out, port+":t")
	}
	return out
}

func decideLANPortRoles(site model.Site, dev model.Device, caps *capability.Registry, existing Existing) lanPortDecision {
	var decision lanPortDecision
	roles := portRolesFor(site, dev.ID)
	if len(roles) == 0 {
		return decision
	}
	if caps == nil || caps.Ports.Bridge == "" || len(lanPortsOf(caps)) == 0 || !bridgeIsVLANAware(caps, existing) {
		decision.Omissions = append(decision.Omissions, omittedPortRoles(roles, site, caps)...)
		return decision
	}
	for _, role := range roles {
		if !lanPortKnown(caps, role.Port) {
			decision.Omissions = append(decision.Omissions, Omission{
				WLAN: role.Port,
				Reason: fmt.Sprintf("port %q is not one of this device's known LAN ports, so its "+
					"role was not applied. WAN is never assigned from here. The port list is the "+
					"one the board reported (%s)", role.Port, strings.Join(lanPortsOf(caps), ", ")),
			})
		}
	}
	sections := vlan1SectionNames(existing, caps.Ports.Bridge)
	if len(sections) != 1 {
		decision.Omissions = append(decision.Omissions, vlan1Unavailable(roles, site, caps.Ports.Bridge,
			"needs exactly one VLAN 1 bridge-vlan section, and this device has "+itoa(len(sections)))...)
		return decision
	}
	name := sections[0]
	vals := existing.In("network")[name]
	current, listed := portsList(vals)
	if !listed {
		decision.Omissions = append(decision.Omissions, vlan1Unavailable(roles, site, caps.Ports.Bridge,
			"has a VLAN 1 bridge-vlan whose ports option is not a UCI list. Rewriting an "+
				"option-shaped ports value is stored and then ignored by netifd")...)
		return decision
	}

	byPort := map[string]model.PortRole{}
	for _, role := range roles {
		if lanPortKnown(caps, role.Port) {
			byPort[role.Port] = role
		}
	}
	tokens := tokenizePorts(current)
	access := map[string]int{}
	for _, port := range lanPortsOf(caps) {
		role, saved := byPort[port]
		if !saved {
			// Implicit trunk. Leave whatever token VLAN 1 already has, and do
			// not add a port the operator never put there. That is what keeps
			// a single access role from rewriting every other port.
			continue
		}
		if role.Mode == model.PortRoleTrunk {
			if !portPresent(tokens, port) {
				tokens = append(tokens, bridgePort{raw: port + ":u*", name: port, untagged: true})
			}
			continue
		}
		net, ok := site.NetworkByID(role.NetworkID)
		if !ok || !net.Enabled || net.VLAN <= 1 {
			if !portPresent(tokens, port) {
				tokens = append(tokens, bridgePort{raw: port + ":u*", name: port, untagged: true})
			}
			decision.Omissions = append(decision.Omissions, Omission{
				WLAN: portRoleLabel(net, port),
				Reason: fmt.Sprintf("port %s was not made an access port%s: the network is missing, "+
					"disabled, or VLAN %d, and VLAN 1 is the existing management LAN. The port stays "+
					"a trunk. Nothing was staged for this role",
					port, networkClause(net), vlanOrZero(net)),
			})
			continue
		}
		if portPresent(tokens, port) {
			next := withoutPort(tokens, port)
			if countUntagged(tokens) > 0 && countUntagged(next) == 0 {
				decision.Omissions = append(decision.Omissions, Omission{
					WLAN: net.Name,
					Reason: fmt.Sprintf("port %s was not made an access port of %s (VLAN %d): removing it "+
						"would leave VLAN 1 on %s with no untagged member, which is the management LAN. "+
						"The port stays a trunk and nothing was staged for this role. Leave another LAN "+
						"port untagged on VLAN 1 first",
						port, net.Name, net.VLAN, caps.Ports.Bridge),
				})
				continue
			}
			tokens = next
		}
		access[port] = net.ID
	}
	desired := portTokens(tokens)
	if len(desired) == 0 && len(current) > 0 {
		decision.Omissions = append(decision.Omissions, Omission{
			WLAN: "VLAN 1",
			Reason: fmt.Sprintf("LAN port roles on %s were not applied because they would empty the "+
				"VLAN 1 ports list. Nothing was staged", caps.Ports.Bridge),
		})
		return decision
	}
	decision.Access = access
	if strings.Join(desired, " ") != strings.Join(current, " ") {
		decision.Patch = &Patch{
			Config:  "network",
			Section: name,
			Lists:   map[string][]string{"ports": desired},
		}
	}
	return decision
}

func portRolesFor(site model.Site, deviceID int64) []model.PortRole {
	var out []model.PortRole
	for _, role := range site.PortRoles {
		if role.DeviceID == deviceID {
			out = append(out, role)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func lanPortsOf(caps *capability.Registry) []string {
	if caps == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, port := range caps.Ports.LAN {
		if port == "" || port == caps.Ports.WAN || seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	return out
}

func lanPortKnown(caps *capability.Registry, port string) bool {
	for _, lan := range lanPortsOf(caps) {
		if lan == port {
			return true
		}
	}
	return false
}

func vlan1SectionNames(existing Existing, bridge string) []string {
	var names []string
	for name, vals := range existing.In("network") {
		if vals[".type"] != "bridge-vlan" || vals["device"] != bridge {
			continue
		}
		vid, err := strconv.Atoi(strings.TrimSpace(vals["vlan"]))
		if err != nil || vid != 1 {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// portsList returns the ports list only when the device recorded it as a UCI
// list. An option-shaped value flattens to the same text and netifd ignores it.
func portsList(vals map[string]string) ([]string, bool) {
	isList, known := StoredAsList(vals, "ports")
	if !known || !isList {
		return nil, false
	}
	raw := strings.TrimSpace(vals["ports"])
	if raw == "" {
		return []string{}, true
	}
	return strings.Fields(raw), true
}

type bridgePort struct {
	raw      string
	name     string
	untagged bool
}

func tokenizePorts(raws []string) []bridgePort {
	out := make([]bridgePort, 0, len(raws))
	for _, raw := range raws {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		name, flags, _ := strings.Cut(raw, ":")
		if name == "" {
			continue
		}
		// 't' is the only config-path flag that clears untagged. Anything we
		// cannot classify stays untagged so it still counts as a management
		// member and is harder to delete by accident.
		out = append(out, bridgePort{
			raw: raw, name: name, untagged: !strings.Contains(flags, "t"),
		})
	}
	return out
}

func portPresent(tokens []bridgePort, name string) bool {
	for _, token := range tokens {
		if token.name == name {
			return true
		}
	}
	return false
}

func withoutPort(tokens []bridgePort, name string) []bridgePort {
	out := make([]bridgePort, 0, len(tokens))
	for _, token := range tokens {
		if token.name == name {
			continue
		}
		out = append(out, token)
	}
	return out
}

func countUntagged(tokens []bridgePort) int {
	n := 0
	for _, token := range tokens {
		if token.untagged {
			n++
		}
	}
	return n
}

func portTokens(tokens []bridgePort) []string {
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, token.raw)
	}
	return out
}

func omittedPortRoles(roles []model.PortRole, site model.Site, caps *capability.Registry) []Omission {
	bridge := "its bridge"
	if caps != nil && caps.Ports.Bridge != "" {
		bridge = caps.Ports.Bridge
	}
	var out []Omission
	for _, role := range roles {
		if role.Mode != model.PortRoleAccess {
			continue
		}
		net, _ := site.NetworkByID(role.NetworkID)
		out = append(out, Omission{
			WLAN: portRoleLabel(net, role.Port),
			Reason: fmt.Sprintf("port %s was not made an access port%s: this device has no VLAN-aware "+
				"LAN bridge this controller can edit without turning filtering on or inventing port names. "+
				"The port stays a trunk and nothing was staged for this role",
				role.Port, networkClause(net)),
		})
	}
	if len(out) == 0 && bridge != "" {
		return nil
	}
	return out
}

func vlan1Unavailable(roles []model.PortRole, site model.Site, bridge, why string) []Omission {
	var out []Omission
	for _, role := range roles {
		if role.Mode != model.PortRoleAccess {
			continue
		}
		net, _ := site.NetworkByID(role.NetworkID)
		out = append(out, Omission{
			WLAN: portRoleLabel(net, role.Port),
			Reason: fmt.Sprintf("port %s was not made an access port%s: %s %s. Creating a VLAN 1 section, "+
				"or enabling filtering, is the edit that takes the management LAN down. The port stays a "+
				"trunk and nothing was staged for this role",
				role.Port, networkClause(net), bridge, why),
		})
	}
	return out
}

func portRoleLabel(net model.Network, port string) string {
	if net.Name != "" {
		return net.Name
	}
	return port
}

func networkClause(net model.Network) string {
	if net.Name == "" {
		return ""
	}
	if net.VLAN > 0 {
		return fmt.Sprintf(" of %s (VLAN %d)", net.Name, net.VLAN)
	}
	return fmt.Sprintf(" of %s", net.Name)
}

func vlanOrZero(net model.Network) int {
	if net.VLAN == 0 {
		return 0
	}
	return net.VLAN
}
