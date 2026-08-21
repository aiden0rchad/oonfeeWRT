package model

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// PolicyKind is one concrete rule shown in the Policy Engine master table.
// Zone forwarding and client actions are synthesized beside these records;
// they are not duplicated in fw_rules.
type PolicyKind string

const (
	PolicyFirewallRule PolicyKind = "firewall_rule"
	PolicyPortForward  PolicyKind = "port_forward"
	PolicyStaticRoute  PolicyKind = "static_route"
)

type PolicyOrigin string

const (
	PolicyOriginManual        PolicyOrigin = "manual"
	PolicyOriginObjectManager PolicyOrigin = "object_manager"
)

type FirewallAction string

const (
	FirewallAccept FirewallAction = "accept"
	FirewallDrop   FirewallAction = "drop"
	FirewallReject FirewallAction = "reject"
)

// Policy is one persisted, ordered controller rule. Exactly one payload must
// match Kind; strict store decoding makes malformed security state fail closed.
type Policy struct {
	ID      int          `json:"id"`
	Order   int          `json:"order"`
	Name    string       `json:"name"`
	Kind    PolicyKind   `json:"kind"`
	Origin  PolicyOrigin `json:"origin"`
	Enabled bool         `json:"enabled"`

	Firewall    *FirewallRule `json:"firewall,omitempty"`
	PortForward *PortForward  `json:"port_forward,omitempty"`
	StaticRoute *StaticRoute  `json:"static_route,omitempty"`
}

// FirewallRule maps directly to one firewall4 config rule. Empty DestinationZone
// means router input; a named destination means forwarded traffic.
type FirewallRule struct {
	Action          FirewallAction `json:"action"`
	SourceZone      string         `json:"source_zone"`
	DestinationZone string         `json:"destination_zone,omitempty"`
	Protocols       []string       `json:"protocols"`
	SourceCIDR      string         `json:"source_cidr,omitempty"`
	DestinationCIDR string         `json:"destination_cidr,omitempty"`
	SourcePort      string         `json:"source_port,omitempty"`
	DestinationPort string         `json:"destination_port,omitempty"`
	SourceMACs      []string       `json:"source_macs,omitempty"`
}

type PortForward struct {
	SourceZone      string   `json:"source_zone"`
	DestinationZone string   `json:"destination_zone"`
	Protocols       []string `json:"protocols"`
	ExternalPort    int      `json:"external_port"`
	DestinationIP   string   `json:"destination_ip"`
	DestinationPort int      `json:"destination_port"`
	SourceCIDR      string   `json:"source_cidr,omitempty"`
}

// NetworkID 0 means the device's foreign wan interface. Positive IDs name an
// enabled managed network, avoiding a policy orphan when its label changes.
type StaticRoute struct {
	NetworkID int    `json:"network_id"`
	Target    string `json:"target"`
	Gateway   string `json:"gateway"`
	Metric    int    `json:"metric,omitempty"`
}

// PolicyClient is the desired policy subset of a client row. Observed name,
// address and timestamps intentionally stay out of Site so polling cannot make
// a valid Preview stale.
type PolicyClient struct {
	MAC     string `json:"mac"`
	Group   string `json:"group,omitempty"`
	Blocked bool   `json:"blocked"`
	FixedIP string `json:"fixed_ip,omitempty"`
}

// HasExplicitFirewallIntent gates the stronger runtime/foreign-policy proof.
// The historical implicit zone -> wan default deliberately does not opt old
// sites into an extra device call.
func (s Site) HasExplicitFirewallIntent() bool {
	for _, zone := range s.EffectiveZonePolicies() {
		if zone.Explicit {
			return true
		}
	}
	for _, policy := range s.Policies {
		if policy.Enabled && (policy.Kind == PolicyFirewallRule || policy.Kind == PolicyPortForward) {
			return true
		}
	}
	for _, client := range s.PolicyClients {
		if client.Blocked {
			return true
		}
	}
	return false
}

func CanonicalProtocols(in []string) []string {
	seen := map[string]bool{}
	for _, value := range in {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			seen[value] = true
		}
	}
	if seen["all"] {
		return []string{"all"}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func CanonicalMACs(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, raw := range in {
		hw, err := net.ParseMAC(strings.TrimSpace(raw))
		if err != nil || len(hw) != 6 {
			return nil, fmt.Errorf("MAC address %q is invalid", raw)
		}
		seen[strings.ToLower(hw.String())] = true
	}
	out := make([]string, 0, len(seen))
	for mac := range seen {
		out = append(out, mac)
	}
	sort.Strings(out)
	return out, nil
}

// ValidatePolicies validates the cross-feature policy model without consulting
// hardware. Capability gaps are reported by render/API gates, not repaired here.
func (s Site) ValidatePolicies() []error {
	activeZones := map[string]bool{"wan": true}
	for _, name := range s.ActiveZoneNames() {
		activeZones[name] = true
	}
	seenIDs := map[int]bool{}
	var errs []error
	for i := range s.Policies {
		p := &s.Policies[i]
		if p.ID < 0 || p.Order < 0 {
			errs = append(errs, errf("policy %q has an invalid id or order", p.Name))
		}
		if p.ID > 0 {
			if seenIDs[p.ID] {
				errs = append(errs, errf("policy id %d is defined more than once", p.ID))
			}
			seenIDs[p.ID] = true
		}
		if strings.TrimSpace(p.Name) == "" || p.Name != strings.TrimSpace(p.Name) || len(p.Name) > 128 {
			errs = append(errs, errf("policy %d needs an exact nonblank name of at most 128 bytes", p.ID))
		}
		if p.Origin != PolicyOriginManual && p.Origin != PolicyOriginObjectManager {
			errs = append(errs, errf("policy %q has unknown origin %q", p.Name, p.Origin))
		}
		payloads := 0
		for _, present := range []bool{p.Firewall != nil, p.PortForward != nil, p.StaticRoute != nil} {
			if present {
				payloads++
			}
		}
		if payloads != 1 {
			errs = append(errs, errf("policy %q must contain exactly one rule payload", p.Name))
			continue
		}
		switch p.Kind {
		case PolicyFirewallRule:
			if p.Firewall == nil {
				errs = append(errs, errf("policy %q kind firewall_rule requires firewall", p.Name))
				continue
			}
			errs = append(errs, validateFirewallRule(p.Name, p.Firewall, activeZones)...)
		case PolicyPortForward:
			if p.PortForward == nil {
				errs = append(errs, errf("policy %q kind port_forward requires port_forward", p.Name))
				continue
			}
			errs = append(errs, s.validatePortForward(p.Name, p.PortForward, activeZones)...)
		case PolicyStaticRoute:
			if p.StaticRoute == nil {
				errs = append(errs, errf("policy %q kind static_route requires static_route", p.Name))
				continue
			}
			errs = append(errs, s.validateStaticRoute(p.Name, p.StaticRoute)...)
		default:
			errs = append(errs, errf("policy %q has unknown kind %q", p.Name, p.Kind))
		}
	}
	for i := range s.Policies {
		for j := i + 1; j < len(s.Policies); j++ {
			errs = append(errs, validatePolicyPair(s.Policies[i], s.Policies[j])...)
		}
	}

	seenMAC, seenIP := map[string]bool{}, map[string]string{}
	for _, client := range s.PolicyClients {
		macs, err := CanonicalMACs([]string{client.MAC})
		if err != nil {
			errs = append(errs, errf("client policy: %v", err))
			continue
		}
		mac := macs[0]
		if seenMAC[mac] {
			errs = append(errs, errf("client policy for %s is defined more than once", mac))
		}
		seenMAC[mac] = true
		if client.Group != "" && (client.Group != strings.TrimSpace(client.Group) ||
			len(client.Group) > 128 || strings.IndexFunc(client.Group, unicode.IsControl) >= 0) {
			errs = append(errs, errf("client %s group must be a trimmed value of at most 128 bytes without control characters", mac))
		}
		if client.FixedIP == "" {
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(client.FixedIP))
		if err != nil || !addr.Is4() {
			errs = append(errs, errf("client %s fixed IP %q is not IPv4", mac, client.FixedIP))
			continue
		}
		networks := s.networksContaining(addr)
		if len(networks) != 1 {
			errs = append(errs, errf("client %s fixed IP %s must belong to exactly one active managed network", mac, addr))
			continue
		}
		n := networks[0]
		if !n.EffectiveDHCP().Enabled {
			errs = append(errs, errf("client %s fixed IP %s belongs to network %q, whose DHCP server is disabled", mac, addr, n.Name))
		}
		prefix, _ := netip.ParsePrefix(n.CIDR)
		if addr == prefix.Addr() || !usableHost(prefix, addr) {
			errs = append(errs, errf("client %s fixed IP %s is not a usable host in network %q", mac, addr, n.Name))
		}
		if previous := seenIP[addr.String()]; previous != "" && previous != mac {
			errs = append(errs, errf("fixed IP %s is assigned to both %s and %s", addr, previous, mac))
		}
		seenIP[addr.String()] = mac
	}
	for _, client := range s.PolicyClients {
		if !client.Blocked {
			continue
		}
		macs, err := CanonicalMACs([]string{client.MAC})
		if err != nil {
			continue
		}
		for _, p := range s.Policies {
			if !p.Enabled || p.Kind != PolicyFirewallRule || p.Firewall == nil ||
				p.Firewall.Action != FirewallAccept || p.Firewall.DestinationZone == "" ||
				p.Firewall.SourceZone == "wan" || !activeZones[p.Firewall.SourceZone] {
				continue
			}
			if len(p.Firewall.SourceMACs) == 0 || stringIn(p.Firewall.SourceMACs, macs[0]) {
				errs = append(errs, errf("policy %q can accept forwarded traffic for blocked client %s; managed rule order is deliberately not inferred from UCI section names", p.Name, macs[0]))
			}
		}
	}
	return errs
}

func validatePolicyPair(a, b Policy) []error {
	if !a.Enabled || !b.Enabled {
		return nil
	}
	if a.Kind == PolicyFirewallRule && b.Kind == PolicyFirewallRule &&
		a.Firewall != nil && b.Firewall != nil &&
		a.Firewall.SourceZone == b.Firewall.SourceZone &&
		a.Firewall.DestinationZone == b.Firewall.DestinationZone &&
		a.Firewall.Action != b.Firewall.Action {
		return []error{errf("policies %q and %q have opposing actions in the same zone scope; managed rule order is deliberately not inferred from UCI section names", a.Name, b.Name)}
	}
	if a.Kind == PolicyPortForward && b.Kind == PolicyPortForward &&
		a.PortForward != nil && b.PortForward != nil &&
		a.PortForward.ExternalPort == b.PortForward.ExternalPort &&
		protocolListsOverlap(a.PortForward.Protocols, b.PortForward.Protocols) {
		return []error{errf("port forwards %q and %q both claim overlapping protocols on WAN port %d", a.Name, b.Name, a.PortForward.ExternalPort)}
	}
	if policyDenialOverlapsForward(a, b) || policyDenialOverlapsForward(b, a) {
		return []error{errf("policies %q and %q overlap after DNAT; a managed WAN denial would run before firewall4's port-forward acceptance, so display order cannot make both intents true", a.Name, b.Name)}
	}
	return nil
}

func policyDenialOverlapsForward(deny, forward Policy) bool {
	if deny.Kind != PolicyFirewallRule || deny.Firewall == nil ||
		forward.Kind != PolicyPortForward || forward.PortForward == nil {
		return false
	}
	rule, redirect := deny.Firewall, forward.PortForward
	if rule.Action == FirewallAccept || rule.SourceZone != "wan" ||
		rule.DestinationZone != redirect.DestinationZone ||
		!protocolListsOverlap(rule.Protocols, redirect.Protocols) {
		return false
	}
	if rule.SourceCIDR != "" && redirect.SourceCIDR != "" {
		a, errA := netip.ParsePrefix(rule.SourceCIDR)
		b, errB := netip.ParsePrefix(redirect.SourceCIDR)
		if errA == nil && errB == nil && !a.Overlaps(b) {
			return false
		}
	}
	if rule.DestinationCIDR != "" {
		prefix, err := netip.ParsePrefix(rule.DestinationCIDR)
		addr, addrErr := netip.ParseAddr(redirect.DestinationIP)
		if err == nil && addrErr == nil && !prefix.Contains(addr) {
			return false
		}
	}
	return rule.DestinationPort == "" || canonicalPortContains(rule.DestinationPort, redirect.DestinationPort)
}

func canonicalPortContains(raw string, port int) bool {
	parts := strings.Split(raw, "-")
	lo, err := strconv.Atoi(parts[0])
	if err != nil {
		return true
	}
	hi := lo
	if len(parts) == 2 {
		hi, err = strconv.Atoi(parts[1])
		if err != nil {
			return true
		}
	}
	return lo <= port && port <= hi
}

func protocolListsOverlap(a, b []string) bool {
	for _, av := range a {
		for _, bv := range b {
			if av == "all" || bv == "all" || av == bv {
				return true
			}
		}
	}
	return false
}

func validateFirewallRule(name string, rule *FirewallRule, zones map[string]bool) []error {
	var errs []error
	if !zones[rule.SourceZone] {
		errs = append(errs, errf("policy %q source zone %q is not active or wan", name, rule.SourceZone))
	}
	if rule.DestinationZone != "" && !zones[rule.DestinationZone] {
		errs = append(errs, errf("policy %q destination zone %q is not active or wan", name, rule.DestinationZone))
	}
	if rule.DestinationZone != "" && rule.SourceZone == rule.DestinationZone {
		errs = append(errs, errf("policy %q cannot use the same source and destination zone", name))
	}
	if rule.Action != FirewallAccept && rule.Action != FirewallDrop && rule.Action != FirewallReject {
		errs = append(errs, errf("policy %q firewall action %q must be accept, drop or reject", name, rule.Action))
	}
	rule.Protocols = CanonicalProtocols(rule.Protocols)
	if len(rule.Protocols) == 0 {
		rule.Protocols = []string{"all"}
	}
	for _, protocol := range rule.Protocols {
		if protocol != "all" && protocol != "tcp" && protocol != "udp" && protocol != "icmp" {
			errs = append(errs, errf("policy %q protocol %q is unsupported", name, protocol))
		}
	}
	for label, raw := range map[string]string{"source": rule.SourceCIDR, "destination": rule.DestinationCIDR} {
		if raw != "" {
			prefix, err := parseIPv4Prefix(raw)
			if err != nil {
				errs = append(errs, errf("policy %q %s CIDR: %v", name, label, err))
			} else if label == "source" {
				rule.SourceCIDR = prefix.Masked().String()
			} else {
				rule.DestinationCIDR = prefix.Masked().String()
			}
		}
	}
	for label, raw := range map[string]string{"source": rule.SourcePort, "destination": rule.DestinationPort} {
		if raw != "" {
			canonical, err := canonicalPortRange(raw)
			if err != nil {
				errs = append(errs, errf("policy %q %s port: %v", name, label, err))
			} else if label == "source" {
				rule.SourcePort = canonical
			} else {
				rule.DestinationPort = canonical
			}
		}
	}
	if (rule.SourcePort != "" || rule.DestinationPort != "") &&
		!containsProtocol(rule.Protocols, "tcp") && !containsProtocol(rule.Protocols, "udp") {
		errs = append(errs, errf("policy %q uses ports without tcp or udp", name))
	}
	macs, err := CanonicalMACs(rule.SourceMACs)
	if err != nil {
		errs = append(errs, errf("policy %q: %v", name, err))
	} else {
		rule.SourceMACs = macs
	}
	return errs
}

func (s Site) validatePortForward(name string, rule *PortForward, zones map[string]bool) []error {
	var errs []error
	if rule.SourceZone != "wan" {
		errs = append(errs, errf("policy %q port forward source must be wan", name))
	}
	if rule.DestinationZone == "wan" || !zones[rule.DestinationZone] {
		errs = append(errs, errf("policy %q port forward destination zone %q is not an active managed zone", name, rule.DestinationZone))
	}
	rule.Protocols = CanonicalProtocols(rule.Protocols)
	if len(rule.Protocols) == 0 {
		errs = append(errs, errf("policy %q port forward needs tcp and/or udp", name))
	}
	for _, protocol := range rule.Protocols {
		if protocol != "tcp" && protocol != "udp" {
			errs = append(errs, errf("policy %q port forward protocol %q must be tcp or udp", name, protocol))
		}
	}
	if rule.ExternalPort < 1 || rule.ExternalPort > 65535 || rule.DestinationPort < 1 || rule.DestinationPort > 65535 {
		errs = append(errs, errf("policy %q port-forward ports must be between 1 and 65535", name))
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(rule.DestinationIP))
	if err != nil || !addr.Is4() {
		errs = append(errs, errf("policy %q destination IP %q is not IPv4", name, rule.DestinationIP))
	} else {
		found := false
		for _, n := range s.Networks {
			zone := n.Zone
			if zone == "" {
				zone = n.Name
			}
			prefix, parseErr := netip.ParsePrefix(n.CIDR)
			if n.Enabled && n.VLAN > 1 && zone == rule.DestinationZone && parseErr == nil &&
				prefix.Contains(addr) && addr != prefix.Addr() && usableHost(prefix, addr) {
				found = true
			}
		}
		if !found {
			errs = append(errs, errf("policy %q destination IP %s is not in destination zone %q", name, addr, rule.DestinationZone))
		}
	}
	if rule.SourceCIDR != "" {
		prefix, err := parseIPv4Prefix(rule.SourceCIDR)
		if err != nil {
			errs = append(errs, errf("policy %q source CIDR: %v", name, err))
		} else {
			rule.SourceCIDR = prefix.Masked().String()
		}
	}
	if addr.IsValid() {
		rule.DestinationIP = addr.String()
	}
	return errs
}

func (s Site) validateStaticRoute(name string, route *StaticRoute) []error {
	var errs []error
	target, err := parseIPv4Prefix(route.Target)
	if err != nil || target.Bits() < 8 || target != target.Masked() {
		errs = append(errs, errf("policy %q route target %q must be a canonical IPv4 network of /8 or longer", name, route.Target))
	}
	gateway, err := netip.ParseAddr(strings.TrimSpace(route.Gateway))
	if err != nil || !gateway.Is4() {
		errs = append(errs, errf("policy %q route gateway %q is not IPv4", name, route.Gateway))
	}
	if route.Metric < 0 || route.Metric > 65535 {
		errs = append(errs, errf("policy %q route metric must be between 0 and 65535", name))
	}
	if route.NetworkID < 0 {
		errs = append(errs, errf("policy %q route network_id cannot be negative", name))
	} else if route.NetworkID > 0 {
		n, ok := s.NetworkByID(route.NetworkID)
		if !ok || !n.Enabled || n.VLAN <= 1 {
			errs = append(errs, errf("policy %q route network %d is not an active managed network", name, route.NetworkID))
		} else if gateway.IsValid() {
			prefix, parseErr := netip.ParsePrefix(n.CIDR)
			if parseErr != nil || !prefix.Contains(gateway) || !usableHost(prefix, gateway) {
				errs = append(errs, errf("policy %q route gateway %s is not a usable host in network %q", name, gateway, n.Name))
			}
		}
	}
	if target.IsValid() {
		route.Target = target.Masked().String()
		for _, n := range s.Networks {
			prefix, parseErr := netip.ParsePrefix(n.CIDR)
			if !n.Enabled || n.VLAN <= 1 || parseErr != nil {
				continue
			}
			if prefix.Masked().Overlaps(target) {
				errs = append(errs, errf("policy %q route target %s overlaps connected managed network %q (%s)", name, target, n.Name, prefix.Masked()))
			}
		}
	}
	if gateway.IsValid() {
		route.Gateway = gateway.String()
	}
	return errs
}

func (s Site) networksContaining(addr netip.Addr) []Network {
	var out []Network
	for _, n := range s.Networks {
		prefix, err := netip.ParsePrefix(n.CIDR)
		if n.Enabled && n.VLAN > 1 && err == nil && prefix.Contains(addr) {
			out = append(out, n)
		}
	}
	return out
}

func parseIPv4Prefix(raw string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
	if err != nil || !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%q is not an IPv4 CIDR", raw)
	}
	return prefix, nil
}

func canonicalPortRange(raw string) (string, error) {
	parts := strings.Split(strings.TrimSpace(raw), "-")
	if len(parts) < 1 || len(parts) > 2 {
		return "", fmt.Errorf("%q must be one port or one inclusive range", raw)
	}
	lo, err := strconv.Atoi(parts[0])
	if err != nil || lo < 1 || lo > 65535 {
		return "", fmt.Errorf("%q is outside 1-65535", raw)
	}
	hi := lo
	if len(parts) == 2 {
		hi, err = strconv.Atoi(parts[1])
		if err != nil || hi < lo || hi > 65535 {
			return "", fmt.Errorf("%q is not an ascending range inside 1-65535", raw)
		}
	}
	if hi == lo {
		return strconv.Itoa(lo), nil
	}
	return strconv.Itoa(lo) + "-" + strconv.Itoa(hi), nil
}

func stringIn(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsProtocol(in []string, want string) bool {
	for _, value := range in {
		if value == want {
			return true
		}
	}
	return false
}

func usableHost(prefix netip.Prefix, addr netip.Addr) bool {
	if !prefix.Contains(addr) || !addr.Is4() || prefix.Bits() > 30 {
		return false
	}
	a, n := addr.As4(), prefix.Masked().Addr().As4()
	value := func(v [4]byte) uint32 {
		return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
	}
	offset := value(a) - value(n)
	last := uint32(1)<<(32-prefix.Bits()) - 1
	return offset > 0 && offset < last
}
