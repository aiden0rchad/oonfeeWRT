package render

import (
	"fmt"
	"sort"
	"strconv"
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
// server and a firewall zone with its forwarding rule. An AP renders the
// bridge-VLAN plus an unmanaged interface so hostapd can attach to it; the
// gateway remains the only device that addresses or serves the network. An AP
// that also ran a DHCP server on the same VLAN would produce two servers
// answering the same broadcast, which fails intermittently and is miserable to
// diagnose — so the subsetting is by role and tested, not an if-cascade that
// happens to work on one topology.

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

func singleInterfaceVLANLimitation(caps *capability.Registry, device string) string {
	if caps.State(capability.FeatDSA) == capability.Absent &&
		caps.State(capability.FeatSwitchPorts) == capability.Present {
		return fmt.Sprintf("this board presents its LAN as a single interface (%s) "+
			"rather than switch ports that can be tagged individually, so oonfeeWRT "+
			"cannot add a tagged VLAN to it. This measured legacy layout uses "+
			"swconfig, which oonfeeWRT observes but does not manage", device)
	}
	return fmt.Sprintf("this board presents its LAN as a single interface (%s) "+
		"rather than switch ports that can be tagged individually. oonfeeWRT "+
		"does not yet create tagged VLAN attachments on single-interface LANs, "+
		"so it leaves this board's existing LAN and VLAN configuration untouched. "+
		"The layout was read successfully; this is a management limit, not missing "+
		"hardware", device)
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
	zone        string // as the operator typed it
	iface       string // our interface section name
	dhcp4       bool   // this interface needs router-local IPv4 DHCP and DNS access
	ipv6        bool   // this interface needs DHCPv6, IPv6 DNS, and ICMPv6 access
	dhcpSection bool   // the desired document contains an owned DHCP section
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

	// Two different answers that used to share one sentence.
	//
	// probePorts fails by leaving the bridge EMPTY. It sets Bridge from
	// lan.Device — with no LAN ports — for a board whose LAN is a single
	// interface rather than a set of individually taggable switch ports, which
	// is a successful read of a real layout. Saying "did not report its wired
	// port layout" about that board is false, and it sends an operator to
	// widen an ACL and re-probe over a device that answered the first time.
	//
	// Measured on the reference Archer C6: bridge eth0.1, no LAN ports, DSA
	// Absent — a swconfig board, where VLANs live in a config oonfeeWRT does
	// not manage.
	ports := caps.Ports
	switch {
	case ports.Bridge == "":
		omissions = append(omissions, Omission{
			WLAN: n.Name, Kind: KindUndetermined,
			Reason: "this device did not report its wired port layout, so a VLAN " +
				"cannot be tagged onto physical ports here. The check reads the " +
				"board description and got nothing back — that is a gap in what " +
				"the controller can see, not a statement about the hardware. " +
				readoptFix,
		})
		return nil, omissions, none
	case len(ports.LAN) == 0:
		omissions = append(omissions, Omission{
			WLAN: n.Name, Reason: singleInterfaceVLANLimitation(caps, ports.Bridge),
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
			"device": ports.Bridge,
			"vlan":   itoa(n.VLAN),
			// The br-lan.<VID> interface and hostapd attachment both require
			// membership on the bridge itself. netifd defaults this on, but an
			// owned stale `local 0` would otherwise compare equal and strand the
			// BSS from the CPU side of the VLAN.
			"local":      "1",
			OwnershipTag: "1",
		},
		// A list, not a joined string — see Section.Lists.
		Lists: map[string][]string{"ports": tagged},
	})

	functions := dev.EffectiveFunctions()
	ifaceName := netIfaceName(n.Name)
	if !functions.Routes() {
		// An AP still needs a UCI network interface for hostapd to attach its BSS
		// to the VLAN. A bridge-vlan alone carries tagged wired frames, but a
		// wifi-iface referencing a network name that does not exist comes up
		// isolated from that bridge. It is deliberately unmanaged: addressing,
		// DHCP and firewalling remain the gateway's job.
		if functions.Wireless() {
			out = append(out, Section{
				Config: "network", Type: "interface", Name: ifaceName,
				Values: map[string]string{
					"proto":      "none",
					"device":     fmt.Sprintf("%s.%d", ports.Bridge, n.VLAN),
					OwnershipTag: "1",
				},
				Manages: []string{"ipaddr", "netmask"},
			})
		}
		return out, omissions, none
	}

	// The addressed interface.
	ipaddr, netmask, ok := splitCIDR(n.CIDR)
	if !ok {
		omissions = append(omissions, Omission{
			WLAN: n.Name,
			Reason: fmt.Sprintf("no usable address: %q is not an IPv4 network in "+
				"CIDR form, so this network gets its VLAN and no gateway address "+
				"and no DHCP — clients on it will associate and get no lease. To "+
				"fix it: open this network and write the address with a prefix "+
				"length, for example %q. A prefix shorter than /8 is refused as a "+
				"typo rather than accepted as an enormous subnet.",
				n.CIDR, exampleCIDR(n.VLAN)),
		})
		return out, omissions, none
	}
	ifaceValues := map[string]string{
		"proto":      "static",
		"device":     fmt.Sprintf("%s.%d", ports.Bridge, n.VLAN),
		"ipaddr":     ipaddr,
		"netmask":    netmask,
		OwnershipTag: "1",
	}
	ifaceManages := []string{"ipaddr", "netmask"}
	ipv6 := n.EffectiveIPv6()
	switch ipv6.Mode {
	case model.IPv6PrefixDelegation:
		ifaceValues["ip6assign"] = itoa(ipv6.AssignmentLength)
		ifaceManages = append(ifaceManages, "ip6assign")
	case model.IPv6Disabled:
		ifaceManages = append(ifaceManages, "ip6assign", "ip6hint", "ip6class")
	}
	out = append(out, Section{
		Config: "network", Type: "interface", Name: ifaceName,
		Values: ifaceValues, Manages: ifaceManages,
	})

	// DHCP is part of the site model rather than a renderer constant. Legacy
	// rows still resolve to the historical 100/150/12h defaults through
	// EffectiveDHCP, so merely upgrading does not create a device diff.
	dhcp := n.EffectiveDHCP()
	preservedState, preservedServer := managedIPv6State(n, existing)
	preservedState = ipv6.Mode == model.IPv6Preserve && preservedState
	preservedServer = ipv6.Mode == model.IPv6Preserve && preservedServer
	if dhcp.Enabled || ipv6.Mode != model.IPv6Preserve || preservedState {
		values := map[string]string{
			"interface":  ifaceName,
			"networkid":  ifaceName,
			OwnershipTag: "1",
		}
		if dhcp.Enabled {
			values["start"] = itoa(dhcp.Start)
			values["limit"] = itoa(dhcp.Limit)
			values["leasetime"] = strings.TrimSpace(dhcp.LeaseTime)
			values["dhcpv4"] = "server"
		} else {
			// Keep dnsmasq from serving IPv4 merely because an odhcpd policy
			// needs a DHCP section of its own.
			values["ignore"] = "1"
			values["dhcpv4"] = "disabled"
		}
		manages := []string{"instance", "ignore", "networkid", "netmask", "force", "options", "dhcp_option", "dhcp_option_force",
			"dynamicdhcp", "dynamicdhcpv4", "dhcpv4", "tag"}
		switch ipv6.Mode {
		case model.IPv6PrefixDelegation:
			values["dhcpv6"] = "server"
			values["ra"] = "server"
			values["ndp"] = "disabled"
			manages = append(manages, "dynamicdhcpv6", "dhcpv6", "ra", "ra_management", "ndp", "ra_default")
		case model.IPv6Disabled:
			values["dhcpv6"] = "disabled"
			values["ra"] = "disabled"
			values["ndp"] = "disabled"
			manages = append(manages, "dynamicdhcpv6", "dhcpv6", "ra", "ra_management", "ndp", "ra_default")
		}
		out = append(out, Section{
			Config: "dhcp", Type: "dhcp", Name: fmt.Sprintf("%s_dhcp_%s", NamePrefix, safe(n.Name)),
			Values: values,
			// These optional inputs can disable, re-scope, or change the generated
			// IPv4 range and must not survive on an exact-owned pool.
			Manages: manages,
		})
	}

	zone := n.Zone
	if zone == "" {
		zone = n.Name
	}
	return out, omissions, zoneMember{
		zone: zone, iface: ifaceName, dhcp4: dhcp.Enabled,
		ipv6:        ipv6.Mode == model.IPv6PrefixDelegation || preservedServer,
		dhcpSection: dhcp.Enabled || ipv6.Mode != model.IPv6Preserve || preservedState,
	}
}

// managedIPv6State finds IPv6 configuration in an already-owned DHCP section.
// Preserve carries that state forward without rewriting its IPv6 options; the
// second result recognizes the exact server state whose owned firewall rules
// must also remain present.
func managedIPv6State(n model.Network, existing Existing) (configured, server bool) {
	name := fmt.Sprintf("%s_dhcp_%s", NamePrefix, safe(n.Name))
	values, present := existing.In("dhcp")[name]
	if !present || values[OwnershipTag] != "1" {
		return false, false
	}
	for _, option := range []string{"dynamicdhcpv6", "dhcpv6", "ra", "ra_management", "ndp", "ra_default"} {
		if _, present := values[option]; present {
			configured = true
			break
		}
	}
	return configured, values["ra"] == "server" && values["dhcpv6"] == "server"
}

// renderManagementIPv6 builds option-only patches for OpenWrt's existing LAN
// and conventional WAN sections. These sections remain operator-owned: a
// missing or ambiguous LAN target is a conflict, never permission to create or
// claim a replacement.
func renderManagementIPv6(n model.Network, existing Existing) ([]Patch, []Conflict) {
	policy := n.EffectiveIPv6()
	if policy.Mode == model.IPv6Preserve {
		return nil, nil
	}

	var (
		patches   []Patch
		conflicts []Conflict
	)
	lan, found := existing.In("network")[n.Name]
	if !found || lan[".type"] != "interface" {
		reason := fmt.Sprintf("IPv6 policy targets the existing management interface network.%s, "+
			"but that exact interface section was not found. oonfeeWRT will not guess, "+
			"create a replacement, or claim a foreign section", n.Name)
		if found {
			reason = fmt.Sprintf("IPv6 policy targets network.%s, but it is a %q section rather "+
				"than an interface. oonfeeWRT will not rewrite or replace it", n.Name, lan[".type"])
		}
		conflicts = append(conflicts, Conflict{Config: "network", Section: n.Name, Reason: reason})
	} else {
		if policy.Mode == model.IPv6Disabled {
			static := staticIPv6Options(lan)
			if len(static) > 0 {
				conflicts = append(conflicts, Conflict{
					Config: "network", Section: n.Name,
					Reason: fmt.Sprintf("network.%s contains explicit static IPv6 option(s) %s. "+
						"Automatic disable only turns off delegated prefix assignment and "+
						"RA/DHCPv6/NDP; oonfeeWRT will not silently delete operator-assigned "+
						"IPv6 addresses, prefixes, or gateways. Remove those values deliberately "+
						"in OpenWrt, then preview again", n.Name, strings.Join(static, ", "))})
			}
		}
		patch := Patch{Config: "network", Section: n.Name}
		switch policy.Mode {
		case model.IPv6PrefixDelegation:
			patch.Values = map[string]string{"ip6assign": itoa(policy.AssignmentLength)}
		case model.IPv6Disabled:
			patch.Deletes = []string{"ip6assign", "ip6hint", "ip6class"}
		}
		patches = append(patches, patch)
	}

	var dhcpSections []string
	for name, values := range existing.In("dhcp") {
		if values[".type"] == "dhcp" && values["interface"] == n.Name {
			dhcpSections = append(dhcpSections, name)
		}
	}
	sort.Strings(dhcpSections)
	if len(dhcpSections) != 1 {
		reason := fmt.Sprintf("IPv6 policy needs exactly one existing DHCP section for management "+
			"interface %s, but found %d. oonfeeWRT will not guess, create, claim, or "+
			"rewrite a whole foreign section", n.Name, len(dhcpSections))
		conflicts = append(conflicts, Conflict{Config: "dhcp", Section: n.Name, Reason: reason})
	} else {
		values := map[string]string{"ndp": "disabled"}
		switch policy.Mode {
		case model.IPv6PrefixDelegation:
			values["ra"] = "server"
			values["dhcpv6"] = "server"
		case model.IPv6Disabled:
			values["ra"] = "disabled"
			values["dhcpv6"] = "disabled"
		}
		patches = append(patches, Patch{
			Config: "dhcp", Section: dhcpSections[0], Values: values,
			Deletes: []string{"ra_default"},
		})
	}

	// The conventional WAN knobs are safe to update only when those exact
	// existing interface sections are present. Their absence is valid on
	// PPPoE, multi-WAN and vendor layouts, so absence is not a conflict and no
	// replacement is created.
	wan6, hasWAN6 := existing.In("network")["wan6"]
	if hasWAN6 && wan6[".type"] != "interface" {
		conflicts = append(conflicts, Conflict{
			Config: "network", Section: "wan6",
			Reason: fmt.Sprintf("the conventional network.wan6 IPv6 control exists as a %q "+
				"section rather than an interface, so oonfeeWRT will not modify it", wan6[".type"]),
		})
		hasWAN6 = false
	}
	// A conventional wan6 is a DHCPv6 client. The section name alone is not
	// enough evidence: operators and vendor firmware also use `wan6` for static,
	// relay, tunnel, or custom protocol sections. Those remain router-managed,
	// exactly as the settings UI promises.
	conventionalWAN6 := hasWAN6 &&
		strings.EqualFold(strings.TrimSpace(wan6["proto"]), "dhcpv6")
	if conventionalWAN6 {
		if policy.Mode == model.IPv6Disabled {
			if static := staticIPv6Options(wan6); len(static) > 0 {
				conflicts = append(conflicts, Conflict{
					Config: "network", Section: "wan6",
					Reason: fmt.Sprintf("network.wan6 contains explicit static IPv6 option(s) %s. "+
						"Automatic disable will not silently delete operator-assigned IPv6 "+
						"addresses, prefixes, or gateways. Remove those values deliberately "+
						"in OpenWrt, then preview again", strings.Join(static, ", "))})
			}
		}
		auto := "1"
		if policy.Mode == model.IPv6Disabled {
			auto = "0"
		}
		patches = append(patches, Patch{Config: "network", Section: "wan6",
			Values: map[string]string{"auto": auto}})
	}
	if wan, present := existing.In("network")["wan"]; present {
		switch {
		case wan[".type"] != "interface":
			conflicts = append(conflicts, Conflict{
				Config: "network", Section: "wan",
				Reason: fmt.Sprintf("the conventional network.wan IPv6 control exists as a %q "+
					"section rather than an interface, so oonfeeWRT will not modify it", wan[".type"]),
			})
		case policy.Mode == model.IPv6Disabled:
			if static := staticIPv6Options(wan); len(static) > 0 {
				conflicts = append(conflicts, Conflict{
					Config: "network", Section: "wan",
					Reason: fmt.Sprintf("network.wan contains explicit static IPv6 option(s) %s. "+
						"Automatic disable will not silently delete operator-assigned IPv6 "+
						"addresses, prefixes, or gateways. Remove those values deliberately "+
						"in OpenWrt, then preview again", strings.Join(static, ", "))})
			}
			patches = append(patches, Patch{Config: "network", Section: "wan",
				Values: map[string]string{"ipv6": "0"}})
		case pppProtocol(wan["proto"]):
			value := "auto"
			if hasWAN6 {
				// `auto` would spawn a dynamic wan_6 client in addition to the
				// explicit wan6 client. Enable IPv6 on the PPP parent without
				// asking it to create that second client.
				value = "1"
			}
			patches = append(patches, Patch{Config: "network", Section: "wan",
				Values: map[string]string{"ipv6": value}})
		}
	}
	return patches, conflicts
}

func staticIPv6Options(values map[string]string) []string {
	var options []string
	for _, option := range []string{"ip6addr", "ip6prefix", "ip6gw"} {
		if strings.TrimSpace(values[option]) != "" {
			options = append(options, option)
		}
	}
	return options
}

func pppProtocol(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "ppp", "pppoe", "pppoa", "pptp", "l2tp":
		return true
	default:
		return false
	}
}

// foreignBridgeVLANConflicts catches two differently named UCI sections that
// claim the same netifd object. A bridge VLAN is identified by bridge and VID,
// not by its section name; writing a second one would make port and local
// membership depend on config iteration order.
func foreignBridgeVLANConflicts(want Section, existing Existing) []Conflict {
	wantVID, err := strconv.Atoi(strings.TrimSpace(want.Values["vlan"]))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(existing.In("network")))
	for name := range existing.In("network") {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Conflict
	for _, name := range names {
		vals := existing.In("network")[name]
		if name == want.Name || vals[OwnershipTag] == "1" ||
			vals[".type"] != "bridge-vlan" || vals["device"] != want.Values["device"] {
			continue
		}
		vid, err := strconv.Atoi(strings.TrimSpace(vals["vlan"]))
		if err != nil || vid != wantVID {
			continue
		}
		out = append(out, Conflict{
			Config: "network", Section: name,
			Reason: fmt.Sprintf("bridge VLAN section %s already claims %s VLAN %d without "+
				"oonfeeWRT's ownership marker. OpenWrt identifies this object by bridge and "+
				"VLAN, not by UCI section name, so adding %s would create two competing port "+
				"and local-membership definitions. Remove the duplicate, choose another VLAN, "+
				"or add `option %s '1'` only if that section really belongs to this controller",
				name, want.Values["device"], wantVID, want.Name, OwnershipTag),
		})
	}
	return out
}

// foreignDHCPConflicts finds semantic collisions, not just section-name
// collisions. Two differently named DHCP sections targeting one interface are
// still two claims on the same pool; when DHCP is disabled, the foreign section
// also means deleting ours cannot guarantee the requested off state.
func foreignDHCPConflicts(n model.Network, iface string, wantsOwnedPool bool, existing Existing) []Conflict {
	ours := fmt.Sprintf("%s_dhcp_%s", NamePrefix, safe(n.Name))
	var names []string
	for name, vals := range existing.In("dhcp") {
		if vals[".type"] != "dhcp" || vals["interface"] != iface || vals[OwnershipTag] == "1" {
			continue
		}
		// addOwned reports this exact-name collision when the section is wanted.
		if wantsOwnedPool && name == ours {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Conflict, 0, len(names))
	for _, name := range names {
		out = append(out, Conflict{
			Config: "dhcp", Section: name,
			Reason: fmt.Sprintf("DHCP section %s already targets managed interface %s without "+
				"oonfeeWRT's ownership marker, so the requested DHCP state cannot be guaranteed "+
				"without overwriting human-owned config. Remove or retarget that section, or add "+
				"`option %s '1'` if it really belongs to this controller", name, iface, OwnershipTag),
		})
	}
	return out
}

// renderZones turns every network's zone claim into firewall sections.
//
// One section per zone rather than per network, because that is what a zone
// is. The networks in it are a UCI *list* — see Section.Lists — which is also
// the only way two networks can share one.
func renderZones(members []zoneMember, policies []model.ZonePolicy, existing Existing) ([]Section, []Conflict) {
	policyBySource := make(map[string][]string, len(policies))
	for _, p := range policies {
		policyBySource[p.Name] = model.CanonicalZoneDestinations(p.ForwardTo)
	}
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
		source := distinct[0]
		needsDHCP, needsIPv6 := false, false
		for _, member := range group {
			needsDHCP = needsDHCP || member.dhcp4
			needsIPv6 = needsIPv6 || member.ipv6
		}
		forwardTo, ok := policyBySource[source]
		if !ok {
			// renderZones is kept safe for direct package callers too. The site
			// contract normally supplies this through EffectiveZonePolicies.
			forwardTo = []string{"wan"}
		}
		conflicts = append(conflicts,
			foreignFirewallPolicyConflicts(existing, source, forwardTo)...)
		if needsDHCP {
			conflicts = append(conflicts,
				foreignRouterServiceConflicts(existing, source)...)
		}
		if needsIPv6 {
			conflicts = append(conflicts,
				foreignIPv6RouterServiceConflicts(existing, source)...)
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

		section := safe(source)
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
			Lists:   map[string][]string{"network": ifaces},
			Manages: []string{"enabled", "family"},
		})
		if needsDHCP {
			// The zone rejects router-local input by default. Without these two
			// narrow exceptions dnsmasq can be running with the exact desired
			// pool while clients receive neither a lease nor the DNS service that
			// lease advertises. They are owned and disappear when the last pool in
			// this zone is disabled.
			out = append(out,
				Section{
					Config: "firewall", Type: "rule",
					Name: fmt.Sprintf("%s_in_%s_dhcp", NamePrefix, section),
					Values: map[string]string{
						"name":       fmt.Sprintf("oonfeeWRT %s DHCP", source),
						"src":        fw,
						"proto":      "udp",
						"src_port":   "68",
						"dest_port":  "67",
						"target":     "ACCEPT",
						"family":     "ipv4",
						OwnershipTag: "1",
					},
					Manages: []string{"enabled"},
				},
				Section{
					Config: "firewall", Type: "rule",
					Name: fmt.Sprintf("%s_in_%s_dns", NamePrefix, section),
					Values: map[string]string{
						"name":       fmt.Sprintf("oonfeeWRT %s DNS", source),
						"src":        fw,
						"dest_port":  "53",
						"target":     "ACCEPT",
						"family":     "ipv4",
						OwnershipTag: "1",
					},
					Lists:   map[string][]string{"proto": {"tcp", "udp"}},
					Manages: []string{"enabled"},
				},
			)
		}
		if needsIPv6 {
			// The zone itself rejects router-local input. IPv6 cannot function
			// through that default without DHCPv6, DNS and the ICMPv6 control
			// traffic used for router and neighbour discovery.
			out = append(out,
				Section{
					Config: "firewall", Type: "rule",
					Name: fmt.Sprintf("%s_in_%s_dhcpv6", NamePrefix, section),
					Values: map[string]string{
						"name": fmt.Sprintf("oonfeeWRT %s DHCPv6", source), "src": fw,
						"proto": "udp", "src_port": "546", "dest_port": "547",
						"target": "ACCEPT", "family": "ipv6", OwnershipTag: "1",
					},
					Manages: []string{"enabled"},
				},
				Section{
					Config: "firewall", Type: "rule",
					Name: fmt.Sprintf("%s_in_%s_dnsv6", NamePrefix, section),
					Values: map[string]string{
						"name": fmt.Sprintf("oonfeeWRT %s IPv6 DNS", source), "src": fw,
						"dest_port": "53", "target": "ACCEPT", "family": "ipv6",
						OwnershipTag: "1",
					},
					Lists: map[string][]string{"proto": {"tcp", "udp"}}, Manages: []string{"enabled"},
				},
				Section{
					Config: "firewall", Type: "rule",
					Name: fmt.Sprintf("%s_in_%s_icmpv6", NamePrefix, section),
					Values: map[string]string{
						"name": fmt.Sprintf("oonfeeWRT %s ICMPv6", source), "src": fw,
						"proto": "icmp", "target": "ACCEPT", "family": "ipv6", "limit": "1000/sec",
						OwnershipTag: "1",
					},
					Lists: map[string][]string{"icmp_type": {
						"echo-request", "echo-reply", "destination-unreachable", "packet-too-big",
						"time-exceeded", "bad-header", "unknown-header-type", "router-solicitation",
						"neighbour-solicitation", "router-advertisement", "neighbour-advertisement",
					}},
					Manages: []string{"enabled"},
				},
			)
		}
		for _, dest := range forwardTo {
			out = append(out, Section{
				Config: "firewall", Type: "forwarding",
				Name: fmt.Sprintf("%s_fwd_%s_%s", NamePrefix, section, safe(dest)),
				Values: map[string]string{
					"src":        fw,
					"dest":       fwZoneName(dest),
					OwnershipTag: "1",
				},
				Manages: []string{"enabled", "family"},
			})
		}
	}
	return out, conflicts
}

// foreignRouterServiceConflicts reports human-owned input rules that can reject
// the DHCP/DNS traffic opened below. fw4 uses terminating verdicts in rule
// order; because the renderer neither owns nor reorders foreign sections,
// coexistence cannot prove that clients can reach the service.
func foreignRouterServiceConflicts(existing Existing, source string) []Conflict {
	src := fwZoneName(source)
	sections := existing.In("firewall")
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Conflict
	for _, name := range names {
		vals := sections[name]
		if vals[".type"] != "rule" || vals[OwnershipTag] == "1" ||
			uciOptionFalse(vals["enabled"]) || vals["dest"] != "" ||
			!foreignSourceCouldMatch(vals["src"], src) ||
			!familyCouldMatchIPv4(vals["family"]) {
			continue
		}
		target := strings.ToUpper(strings.TrimSpace(vals["target"]))
		if target != "DROP" && target != "REJECT" {
			continue
		}
		var services []string
		if protocolCouldMatch(vals["proto"], "udp") &&
			portCouldMatch(vals["src_port"], 68) && portCouldMatch(vals["dest_port"], 67) {
			services = append(services, "DHCP")
		}
		if (protocolCouldMatch(vals["proto"], "tcp") ||
			protocolCouldMatch(vals["proto"], "udp")) && portCouldMatch(vals["dest_port"], 53) {
			services = append(services, "DNS")
		}
		if len(services) == 0 {
			continue
		}
		out = append(out, Conflict{
			Config: "firewall", Section: name,
			Reason: fmt.Sprintf("foreign firewall rule %s can %s %s client traffic from "+
				"zone %q before oonfeeWRT's router-service allow rules, so DHCP/DNS client "+
				"access cannot be guaranteed. oonfeeWRT will not reorder or delete a "+
				"human-owned rule; disable, remove, or narrow it before applying",
				name, strings.ToLower(target), strings.Join(services, " and "), src),
		})
	}
	return out
}

// foreignIPv6RouterServiceConflicts is the IPv6 counterpart to
// foreignRouterServiceConflicts. An earlier terminating foreign rule would
// make the owned allow rules below decorative while the preview claimed a
// working delegated network.
func foreignIPv6RouterServiceConflicts(existing Existing, source string) []Conflict {
	src := fwZoneName(source)
	sections := existing.In("firewall")
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Conflict
	for _, name := range names {
		vals := sections[name]
		if vals[".type"] != "rule" || vals[OwnershipTag] == "1" ||
			uciOptionFalse(vals["enabled"]) || vals["dest"] != "" ||
			!foreignSourceCouldMatch(vals["src"], src) ||
			!familyCouldMatchIPv6(vals["family"]) {
			continue
		}
		target := strings.ToUpper(strings.TrimSpace(vals["target"]))
		if target != "DROP" && target != "REJECT" {
			continue
		}
		var services []string
		if protocolCouldMatch(vals["proto"], "udp") &&
			portCouldMatch(vals["src_port"], 546) && portCouldMatch(vals["dest_port"], 547) {
			services = append(services, "DHCPv6")
		}
		if (protocolCouldMatch(vals["proto"], "tcp") ||
			protocolCouldMatch(vals["proto"], "udp")) && portCouldMatch(vals["dest_port"], 53) {
			services = append(services, "DNS")
		}
		if protocolCouldMatch(vals["proto"], "icmpv6") || protocolCouldMatch(vals["proto"], "icmp") {
			services = append(services, "ICMPv6")
		}
		if len(services) == 0 {
			continue
		}
		out = append(out, Conflict{
			Config: "firewall", Section: name,
			Reason: fmt.Sprintf("foreign firewall rule %s can %s %s client traffic from "+
				"zone %q before oonfeeWRT's IPv6 router-service allow rules, so delegated "+
				"IPv6 cannot be guaranteed. oonfeeWRT will not reorder or delete a "+
				"human-owned rule; disable, remove, or narrow it before applying",
				name, strings.ToLower(target), strings.Join(services, " and "), src),
		})
	}
	return out
}

func familyCouldMatchIPv4(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "any" || value == "all" || value == "*" || value == "inet" {
		return true
	}
	if strings.Contains(value, "4") {
		return true
	}
	if strings.Contains(value, "6") {
		return false
	}
	return true // unknown syntax is not evidence that the rule is disjoint
}

func familyCouldMatchIPv6(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "any" || value == "all" || value == "*" || value == "inet" {
		return true
	}
	if strings.Contains(value, "6") {
		return true
	}
	if strings.Contains(value, "4") {
		return false
	}
	return true
}

func protocolCouldMatch(value, want string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	if len(fields) == 0 {
		fields = []string{"tcpudp"} // fw4's rule default
	}
	positive, matched, excluded := false, false, false
	for _, field := range fields {
		invert := strings.HasPrefix(field, "!")
		field = strings.TrimPrefix(field, "!")
		var matches, known bool
		switch field {
		case "all", "any", "*":
			matches, known = true, true
		case "tcpudp":
			matches, known = want == "tcp" || want == "udp", true
		case "6":
			matches, known = want == "tcp", true
		case "17":
			matches, known = want == "udp", true
		case "tcp", "udp", "icmp", "icmpv6", "esp", "ah", "gre":
			matches, known = field == want, true
		default:
			return true // unknown syntax is not evidence of disjointness
		}
		if !known {
			return true
		}
		if invert {
			excluded = excluded || matches
		} else {
			positive = true
			matched = matched || matches
		}
	}
	return !excluded && (!positive || matched)
}

func portCouldMatch(value string, want int) bool {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return true
	}
	positive, matched, excluded := false, false, false
	for _, field := range fields {
		invert := strings.HasPrefix(field, "!")
		field = strings.TrimPrefix(field, "!")
		parts := strings.FieldsFunc(field, func(r rune) bool { return r == '-' || r == ':' })
		if len(parts) < 1 || len(parts) > 2 {
			return true
		}
		lo, err := strconv.Atoi(parts[0])
		if err != nil || lo < 0 || lo > 65535 {
			return true
		}
		hi := lo
		if len(parts) == 2 {
			hi, err = strconv.Atoi(parts[1])
			if err != nil || hi < lo || hi > 65535 {
				return true
			}
		}
		contains := want >= lo && want <= hi
		if invert {
			excluded = excluded || contains
		} else {
			positive = true
			matched = matched || contains
		}
	}
	return !excluded && (!positive || matched)
}

func uciOptionFalse(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "off", "false", "no":
		return true
	default:
		return false
	}
}

func uciOptionTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "on", "true", "yes":
		return true
	default:
		return false
	}
}

func foreignSourceCouldMatch(value, want string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false // fw4 compiles a source-less config rule into an output chain
	}
	if value == "*" || value == want {
		return true
	}
	if strings.ContainsAny(value, "!{},;\"'") || len(strings.Fields(value)) != 1 {
		return true
	}
	return false
}

// foreignFirewallPolicyConflicts reports human-owned UCI that defeats a matrix
// claim. fw4 evaluates rules and DNAT accepts in a source's forward chain, so
// checking only config forwarding would let the UI say Block All while a
// foreign ACCEPT still passes traffic. Conversely, a REJECT/DROP rule can make
// an owned forwarding fall short of Allow All.
//
// This can only reason about UCI sections. Custom nftables includes remain an
// observability limitation and must not be described as verified policy.
func foreignFirewallPolicyConflicts(existing Existing, source string, forwardTo []string) []Conflict {
	allowed := map[string]bool{}
	for _, dest := range forwardTo {
		allowed[fwZoneName(dest)] = true
	}
	src := fwZoneName(source)
	sections := existing.In("firewall")
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Conflict
	for _, name := range names {
		vals := sections[name]
		if vals[OwnershipTag] == "1" || uciOptionFalse(vals["enabled"]) {
			continue
		}
		kind := vals[".type"]
		ruleSource, dest := vals["src"], vals["dest"]
		if !foreignSourceCouldMatch(ruleSource, src) {
			continue
		}
		target := strings.ToUpper(strings.TrimSpace(vals["target"]))
		// OpenWrt ignores redirect.dest for DNAT. With dest_ip, fw4 accepts the
		// translated flow in forward_<src> before the zone verdict, and it can
		// land in a foreign or otherwise unmodeled zone. No selection in this
		// matrix can therefore prove that human-owned DNAT harmless. An explicit
		// REDIRECT, or DNAT without dest_ip, is router-local and does not
		// contradict an inter-zone forwarding claim.
		if kind == "redirect" && (target == "" || target == "DNAT") &&
			strings.TrimSpace(vals["dest_ip"]) != "" {
			out = append(out, Conflict{
				Config: "firewall", Section: name,
				Reason: fmt.Sprintf("foreign firewall redirect %s DNAT-accepts forwarded traffic from source %q before the zone verdict; OpenWrt ignores its `dest` option for DNAT, so the zone-matrix claim cannot be guaranteed. oonfeeWRT will not edit human-owned firewall policy; remove or narrow that section, or mark it with `option %s '1'` only if it belongs to this controller",
					name, ruleSource, OwnershipTag),
			})
			continue
		}
		if dest == "" {
			continue
		}
		destAllowed := allowed[dest] || (dest == "*" && len(allowed) > 0)
		destOmitted := !allowed[dest] || dest == "*"
		var claim string
		switch {
		case kind == "forwarding" && destOmitted:
			claim = "allows an omitted destination"
		case kind == "rule" && target == "ACCEPT" && destOmitted:
			claim = "has target ACCEPT for an omitted destination"
		case kind == "rule" && (target == "REJECT" || target == "DROP") && destAllowed:
			claim = "can reject traffic to an allowed destination"
		default:
			continue
		}
		out = append(out, Conflict{
			Config: "firewall", Section: name,
			Reason: fmt.Sprintf("foreign firewall %s %s for source %q and destination %q, so the zone-matrix claim cannot be guaranteed. oonfeeWRT will not edit human-owned firewall policy; remove or narrow that section, change the matrix edge to match it, or mark the section with `option %s '1'` only if it belongs to this controller",
				name, claim, ruleSource, dest, OwnershipTag),
		})
	}
	return out
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

// networkAttachmentName is the UCI network hostapd/netifd must join. VLAN 1
// is the operator's existing network; routed/tagged VLANs use our owned
// interface section on both gateways and APs.
func networkAttachmentName(n model.Network) string {
	if n.VLAN <= 1 {
		return n.Name
	}
	return netIfaceName(n.Name)
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
const maxZoneName = model.FirewallZoneMaxLen

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
	return model.FirewallZoneName(s)
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
