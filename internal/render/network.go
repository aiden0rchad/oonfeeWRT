package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// Rendering a network: VLAN, addressing, DHCP and firewall zone
// (IMPLEMENTATION §5, worked example 2).
//
// # What this deliberately does not do
//
// It only ever ADDS sections, all of them ours and all of them named. It never
// modifies the device's existing `lan` interface, its `br-lan` device, or any
// other section a human or the firmware wrote. That is the ownership rule, and
// here it has a consequence worth stating plainly rather than discovering:
//
//	**oonfeeWRT cannot re-address your LAN or move your management interface.**
//
// Those live in sections we do not own. A controller that rewrote them would be
// editing the config it reaches the device through, on a device it might then
// be unable to reach. Creating an additional tagged VLAN alongside the existing
// LAN is safe, useful, and the actual requirement most of the time — a guest or
// IoT network. Re-addressing an existing LAN is a job for LuCI or SSH, once.
//
// # Role-aware subsetting
//
// A gateway renders the whole stack: the VLAN, an addressed interface, a DHCP
// server and a firewall zone with its forwarding rule. An AP renders only the
// bridge-VLAN, so tagged frames traverse it to the gateway that actually routes
// them. An AP that also ran a DHCP server on the same VLAN would produce two
// servers answering the same broadcast, which fails intermittently and is
// miserable to diagnose — so the subsetting is by role and tested, not an
// if-cascade that happens to work on one topology.

// bridgeIsVLANAware reports whether the device's bridge already has VLAN
// filtering configured by its operator.
//
// # Why oonfeeWRT will not turn VLAN filtering on by itself
//
// This is the sharpest limit the ownership rule imposes, and it was found the
// hard way. A stock `br-lan` runs unfiltered: one flat domain, and
// `config interface 'lan'` points at `br-lan` directly. Adding ANY bridge-vlan
// section switches filtering on — and at that moment `br-lan` stops being the
// untagged view of the LAN. The address stays, the interface stays up, and all
// layer-2 traffic stops.
//
// Measured on the reference device 2026-08-14, three times. `vlan_filtering`
// went 0 -> 1, `br-lan` kept `192.168.1.1/24` and reported UP, and
// `ip neigh show dev br-lan` was empty: not one neighbour. The apply's health
// check passed — it asks whether the lan interface is up, and it was — the
// confirm landed, and the device was then unreachable until a pre-armed restore
// ran. A confirmed, "healthy", network-severing change.
//
// The fix is not something we may do. Connectivity survives only if the
// existing lan interface moves from `br-lan` to `br-lan.1`, verified in the
// same way: with that one edit, filtering on, `br-lan.1` held the address and
// the controller's own host stayed REACHABLE in the neighbour table. But
// `config interface 'lan'` is the operator's section, and rewriting the
// interface we reach the device through — on a device we might then be unable
// to reach — is exactly what ARCHITECTURE §0 forbids.
//
// So a device whose bridge is not already VLAN-aware gets no VLAN, and an
// explanation of the one-time change that would let it have one. IMPLEMENTATION
// §5's worked example 2 shows the bridge-vlan being added with none of this
// mentioned; rendering it as specified breaks the LAN.
func bridgeIsVLANAware(caps *capability.Registry, existing Existing) bool {
	if caps == nil {
		return false
	}
	bridge := caps.Ports.Bridge
	if bridge == "" {
		return false
	}
	for _, vals := range existing.In("network") {
		if vals[".type"] == "bridge-vlan" && vals["device"] == bridge {
			return true
		}
	}
	return false
}

// vlanPrerequisite is the message shown when a device cannot take a VLAN.
func vlanPrerequisite(bridge string) string {
	return fmt.Sprintf("this device's %s is not VLAN-aware yet, and oonfeeWRT "+
		"will not make it so: enabling VLAN filtering requires moving the "+
		"existing 'lan' interface from %s to %s.1, which is configuration "+
		"oonfeeWRT does not own — and getting it wrong takes the LAN down "+
		"(measured). Make that one change in LuCI or over SSH (set the LAN "+
		"interface's device to %s.1 and give VLAN 1 the untagged ports), after "+
		"which additional VLANs can be managed from here",
		bridge, bridge, bridge, bridge)
}

// zoneMember is one network's claim on a firewall zone: the zone name the
// operator asked for, and the interface section that has to end up inside it.
//
// Returned rather than rendered on the spot because a zone is not a per-network
// thing. Two networks may share one, and fw4 identifies zones by name — so
// rendering one zone section per network produced two sections with the same
// name, of which the device keeps the last. The first network then belonged to
// no zone at all, which in fw4 means every packet on it is dropped, with
// nothing said anywhere.
type zoneMember struct {
	zone  string // as the operator typed it
	iface string // our interface section name
}

// renderNetwork produces the sections for one network on one device.
//
// Zones are not among them — see zoneMember and renderZones.
func renderNetwork(n model.Network, dev model.Device, caps *capability.Registry,
	existing Existing) ([]Section, []Omission, zoneMember) {
	var (
		out       []Section
		omissions []Omission
		none      zoneMember
	)
	if !n.Enabled {
		return nil, nil, none
	}
	// VLAN 0 and 1 are the untagged/default LAN. We do not render those: VLAN 1
	// is the device's existing lan, which we do not own.
	if n.VLAN <= 1 {
		omissions = append(omissions, Omission{
			WLAN: n.Name,
			Reason: "VLAN 1 and untagged traffic are the device's existing LAN, " +
				"which oonfeeWRT does not own and will not rewrite. Wireless can " +
				"attach to it; the wired VLAN is left as the device has it",
		})
		return nil, omissions, none
	}

	// A nil registry is "nothing is known about this device", which every other
	// helper here already treats as such — radiosByBand, radioBySection and
	// withLiveChannels all check it. These two did not, so a render with no
	// capability record panicked on the first VLAN network instead. Not
	// reachable from the daemon, which never produces a nil record without an
	// error, but a contract half the package honours is not a contract.
	if caps == nil {
		omissions = append(omissions, Omission{
			WLAN: n.Name, Kind: KindUndetermined,
			Reason: "nothing is known about this device's hardware, so no wired " +
				"configuration is rendered for it. This is not a statement that " +
				"the device cannot carry a VLAN — it means there is no " +
				"capability record to decide from",
		})
		return nil, omissions, none
	}

	ports := caps.Ports
	if ports.Bridge == "" || len(ports.LAN) == 0 {
		omissions = append(omissions, Omission{
			WLAN: n.Name, Kind: KindUndetermined,
			Reason: "this device did not report its wired port layout, so a VLAN " +
				"cannot be tagged onto physical ports here",
		})
		return nil, omissions, none
	}

	if !bridgeIsVLANAware(caps, existing) {
		omissions = append(omissions, Omission{
			WLAN: n.Name, Reason: vlanPrerequisite(ports.Bridge),
		})
		return nil, omissions, none
	}

	// The bridge-VLAN. Every LAN port carries it tagged: an untagged member
	// would change what an existing port already does, which is the device's
	// configuration and not ours to repurpose.
	tagged := make([]string, 0, len(ports.LAN))
	for _, p := range ports.LAN {
		tagged = append(tagged, p+":t")
	}
	out = append(out, Section{
		Config: "network", Type: "bridge-vlan",
		Name: fmt.Sprintf("%s_bv%d", NamePrefix, n.VLAN),
		Values: map[string]string{
			"device":     ports.Bridge,
			"vlan":       itoa(n.VLAN),
			OwnershipTag: "1",
		},
		// A list, not a joined string — see Section.Lists.
		Lists: map[string][]string{"ports": tagged},
	})

	if !dev.Role.Routes() {
		// An AP stops here. The VLAN exists on its bridge so tagged frames pass
		// through to the gateway; addressing, DHCP and firewalling are the
		// gateway's job and doing them twice is worse than not doing them.
		return out, omissions, none
	}

	// The addressed interface.
	ifaceName := netIfaceName(n.Name)
	ipaddr, netmask, ok := splitCIDR(n.CIDR)
	if !ok {
		omissions = append(omissions, Omission{
			WLAN: n.Name,
			Reason: fmt.Sprintf("no usable address: %q is not an IPv4 network in "+
				"CIDR form, so this network gets a VLAN but no addressing", n.CIDR),
		})
		return out, omissions, none
	}
	out = append(out, Section{
		Config: "network", Type: "interface", Name: ifaceName,
		Values: map[string]string{
			"proto":      "static",
			"device":     fmt.Sprintf("%s.%d", ports.Bridge, n.VLAN),
			"ipaddr":     ipaddr,
			"netmask":    netmask,
			OwnershipTag: "1",
		},
	})

	// DHCP. The range is derived from the prefix rather than configured,
	// because a range that does not fit the subnet is a mistake nobody catches
	// until a client fails to get a lease.
	out = append(out, Section{
		Config: "dhcp", Type: "dhcp", Name: fmt.Sprintf("%s_dhcp_%s", NamePrefix, safe(n.Name)),
		Values: map[string]string{
			"interface":  ifaceName,
			"start":      "100",
			"limit":      "150",
			"leasetime":  "12h",
			OwnershipTag: "1",
		},
	})

	zone := n.Zone
	if zone == "" {
		zone = n.Name
	}
	return out, omissions, zoneMember{zone: zone, iface: ifaceName}
}

// renderZones turns every network's zone claim into firewall sections.
//
// One section per zone rather than per network, because that is what a zone
// is. The networks in it are a UCI *list* — see Section.Lists — which is also
// the only way two networks can share one.
func renderZones(members []zoneMember, existing Existing) ([]Section, []Conflict) {
	var order []string
	byName := map[string][]zoneMember{}
	for _, m := range members {
		// Keyed by the name the FIREWALL will see, not the one the operator
		// typed. Those differ past fw4's cap, and two zones the firewall
		// cannot tell apart are one zone whichever way we name the section.
		k := fwZoneName(m.zone)
		if _, seen := byName[k]; !seen {
			order = append(order, k)
		}
		byName[k] = append(byName[k], m)
	}

	var (
		out       []Section
		conflicts []Conflict
	)
	for _, fw := range order {
		group := byName[fw]

		// Two DIFFERENT zone names that collapse to the same one. Silently
		// merging two networks the operator deliberately separated is a
		// firewall policy nobody chose, so it is refused and named.
		var distinct []string
		for _, m := range group {
			if !hasString(distinct, m.zone) {
				distinct = append(distinct, m.zone)
			}
		}
		if len(distinct) > 1 {
			sort.Strings(distinct)
			conflicts = append(conflicts, Conflict{
				Config: "firewall", Section: fw,
				Reason: fmt.Sprintf("the firewall zones %s are different names "+
					"that both become %q once fw4's %d-character limit is "+
					"applied, so the device cannot tell them apart and one "+
					"network's rules would silently become the other's. Give "+
					"them names that differ within the first %d characters",
					strings.Join(quoteAll(distinct), " and "), fw,
					maxZoneName, maxZoneName),
			})
			continue
		}

		// A zone the device already has and we did not write. Same ownership
		// rule as everywhere else, applied to the namespace fw4 actually keys
		// on: our section name would not collide, and the ZONE would.
		//
		// This is the default path, not a corner: store.SaveNetwork used to
		// stamp every network with zone "lan", so every VLAN rendered a second
		// zone named lan beside the device's own, carrying input REJECT and
		// forward REJECT.
		if other, clash := foreignZone(existing, fw); clash {
			conflicts = append(conflicts, Conflict{
				Config: "firewall", Section: other,
				Reason: fmt.Sprintf("this device already has a firewall zone "+
					"named %q that oonfeeWRT did not write. Adding a network to "+
					"it means editing that zone, which is the operator's "+
					"configuration and not ours to change — and writing a "+
					"second zone with the same name gives the device two, which "+
					"is not what anyone means. Give this network a zone name of "+
					"its own", fw),
			})
			continue
		}

		ifaces := make([]string, 0, len(group))
		for _, m := range group {
			ifaces = append(ifaces, m.iface)
		}
		sort.Strings(ifaces) // deterministic diffs

		section := safe(distinct[0])
		out = append(out, Section{
			Config: "firewall", Type: "zone",
			Name: fmt.Sprintf("%s_zone_%s", NamePrefix, section),
			Values: map[string]string{
				"name": fw,
				// A new network defaults to "can reach out, cannot reach in".
				// That is the safe direction to be wrong in: an operator who
				// wanted a guest network and got an isolated one notices
				// immediately, and one who wanted isolation and got an open
				// zone may never notice.
				"input":      "REJECT",
				"output":     "ACCEPT",
				"forward":    "REJECT",
				OwnershipTag: "1",
			},
			// A list, not a joined string — see Section.Lists. It is also what
			// lets one zone hold more than one network.
			Lists: map[string][]string{"network": ifaces},
		})
		out = append(out, Section{
			Config: "firewall", Type: "forwarding",
			Name: fmt.Sprintf("%s_fwd_%s_wan", NamePrefix, section),
			Values: map[string]string{
				"src":        fw,
				"dest":       "wan",
				OwnershipTag: "1",
			},
		})
	}
	return out, conflicts
}

// foreignZone finds a firewall zone with this name that we do not own.
func foreignZone(e Existing, name string) (string, bool) {
	sections := e.In("firewall")
	names := make([]string, 0, len(sections))
	for n := range sections {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic conflict reporting
	for _, n := range names {
		vals := sections[n]
		if vals[OwnershipTag] == "1" || vals[".type"] != "zone" {
			continue
		}
		if vals["name"] == name {
			return n, true
		}
	}
	return "", false
}

func hasString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return out
}

// netIfaceName is the UCI interface name for a network. Deterministic, so a
// re-render targets the same section.
func netIfaceName(name string) string {
	return fmt.Sprintf("%s_net_%s", NamePrefix, safe(name))
}

// maxZoneName is fw4's limit on a firewall zone name. It applies to the zone's
// NAME, which is what fw4 reads — not to UCI section names, which have no such
// limit.
//
// Conflating the two is what made this worth separating. safe() capped every
// name it produced at 11 characters, so two networks called "Guest Network A"
// and "Guest Network B" both rendered oowrt_net_guest_netwo,
// oowrt_dhcp_guest_netwo and oowrt_zone_guest_netwo. Four sections, each
// rendered twice, and UCI keeps the last: one network got its VLAN tagged onto
// the bridge and nothing else — no address, no DHCP, no firewall — while the
// preview reported no omission and no conflict. The cap was there to STOP two
// zones colliding past it, and applied to section names it produced exactly
// the collision it was guarding against.
const maxZoneName = 11

// safe reduces a human's name to something UCI accepts as a section name:
// letters, digits and underscores.
//
// UCI section names are not free text. A network called "Guest WiFi (2.4)"
// would produce a config file the device rejects, and the failure arrives as an
// opaque parse error at apply time rather than as a message about the name.
//
// Not truncated. A section name has no length limit, and shortening one can
// only ever merge two things the operator kept apart.
func safe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == ' ':
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "net"
	}
	return out
}

// fwZoneName is the name fw4 will actually see: safe(), capped.
//
// A longer one is silently truncated on the device, so the cap is applied here
// rather than discovered there — and because it can make two distinct names
// one, renderZones checks for that and refuses rather than merging.
func fwZoneName(s string) string {
	out := safe(s)
	if len(out) > maxZoneName {
		out = out[:maxZoneName]
	}
	return out
}

// splitCIDR turns "10.7.45.1/24" into an address and a dotted netmask, which is
// what UCI's static proto wants.
func splitCIDR(cidr string) (ipaddr, netmask string, ok bool) {
	addr, prefix, found := strings.Cut(strings.TrimSpace(cidr), "/")
	if !found {
		return "", "", false
	}
	if !validIPv4(addr) {
		return "", "", false
	}
	bits := 0
	for _, r := range prefix {
		if r < '0' || r > '9' {
			return "", "", false
		}
		bits = bits*10 + int(r-'0')
		if bits > 32 {
			return "", "", false
		}
	}
	if prefix == "" || bits < 8 {
		// Below /8 is not a network anyone means to configure on a router, and
		// treating a typo as a valid enormous subnet is worse than refusing it.
		return "", "", false
	}
	mask := ^uint32(0) << (32 - bits)
	return addr, fmt.Sprintf("%d.%d.%d.%d",
		mask>>24&0xff, mask>>16&0xff, mask>>8&0xff, mask&0xff), true
}

func validIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
			n = n*10 + int(r-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
