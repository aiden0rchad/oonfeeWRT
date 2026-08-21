package render

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

var policyRuleManagedOptions = []string{
	"_name", "enabled", "family", "proto", "src", "src_ip", "src_port", "src_mac",
	"dest", "dest_ip", "dest_port", "dest_mac", "device", "direction", "target",
	"icmp_type", "weekdays", "monthdays", "start_date", "stop_date", "start_time", "stop_time", "utc_time",
	"limit", "limit_burst", "log", "log_limit", "ipset", "mark", "set_mark", "set_xmark",
	"dscp", "set_dscp", "helper", "set_helper", "extra", "counter",
}

var policyRedirectManagedOptions = []string{
	"_name", "enabled", "family", "proto", "src", "src_ip", "src_dip", "src_port", "src_dport", "src_mac",
	"dest", "dest_ip", "dest_port", "dest_mac", "device", "direction", "target",
	"reflection", "reflection_src", "reflection_zone", "limit", "limit_burst", "log", "log_limit",
	"utc_time", "start_date", "stop_date", "start_time", "stop_time", "weekdays", "monthdays",
	"ipset", "mark", "helper", "set_helper", "extra", "counter", "snat_ip", "snat_port",
}

func renderPolicies(site model.Site, dev model.Device, caps *capability.Registry,
	existing Existing, desired Doc) ([]Section, []Omission, []Conflict) {
	if !dev.EffectiveFunctions().Routes() {
		return nil, nil, nil
	}

	zones := map[string]bool{"wan": true}
	dhcpNetworks := map[string]bool{}
	for _, section := range desired.Sections {
		if section.Config == "firewall" && section.Type == "zone" {
			zones[section.Values["name"]] = true
		}
		if section.Config == "dhcp" && section.Type == "dhcp" {
			dhcpNetworks[section.Values["interface"]] = true
		}
	}

	var out []Section
	var omissions []Omission
	var conflicts []Conflict
	needsFW4 := false
	for _, p := range site.Policies {
		if p.Enabled && (p.Kind == model.PolicyFirewallRule || p.Kind == model.PolicyPortForward) {
			needsFW4 = true
		}
	}
	for _, client := range site.PolicyClients {
		needsFW4 = needsFW4 || client.Blocked
	}
	if needsFW4 && (caps == nil || caps.State(capability.FeatFirewall4) != capability.Present) {
		state := capability.Unknown
		if caps != nil {
			state = caps.State(capability.FeatFirewall4)
		}
		conflicts = append(conflicts, Conflict{
			Config: "firewall", Section: "policy-capability",
			Reason: fmt.Sprintf("enabled firewall/NAT/client-block policy requires observable firewall4, but this device reports %s. Re-probe after restoring the exact nft read grant; policy is blocked rather than silently omitted", state),
		})
	}

	policies := append([]model.Policy(nil), site.Policies...)
	sort.Slice(policies, func(i, j int) bool {
		if policies[i].Order != policies[j].Order {
			return policies[i].Order < policies[j].Order
		}
		return policies[i].ID < policies[j].ID
	})
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		name := fmt.Sprintf("%s_policy_%s_%010d_%d", NamePrefix, policySectionKind(p.Kind), p.Order, p.ID)
		switch p.Kind {
		case model.PolicyFirewallRule:
			rule := p.Firewall
			if !policyZonesRendered(rule.SourceZone, rule.DestinationZone, zones) {
				conflicts = append(conflicts, missingPolicyZoneConflict(p.Name))
				continue
			}
			values := map[string]string{
				"name":       fmt.Sprintf("oonfeeWRT policy %d %s", p.ID, p.Name),
				"src":        fwZoneName(rule.SourceZone),
				"target":     strings.ToUpper(string(rule.Action)),
				"family":     "ipv4",
				OwnershipTag: "1",
			}
			if rule.DestinationZone != "" {
				values["dest"] = fwZoneName(rule.DestinationZone)
			}
			copyNonempty(values, "src_ip", rule.SourceCIDR)
			copyNonempty(values, "dest_ip", rule.DestinationCIDR)
			copyNonempty(values, "src_port", rule.SourcePort)
			copyNonempty(values, "dest_port", rule.DestinationPort)
			section := Section{Config: "firewall", Type: "rule", Name: name,
				Values: values, Manages: append([]string(nil), policyRuleManagedOptions...)}
			setProtocols(&section, rule.Protocols)
			if len(rule.SourceMACs) > 0 {
				section.Lists["src_mac"] = append([]string(nil), rule.SourceMACs...)
			}
			out = append(out, section)

		case model.PolicyPortForward:
			rule := p.PortForward
			if !zones[fwZoneName(rule.DestinationZone)] {
				conflicts = append(conflicts, missingPolicyZoneConflict(p.Name))
				continue
			}
			section := Section{Config: "firewall", Type: "redirect", Name: name,
				Values: map[string]string{
					"name":       fmt.Sprintf("oonfeeWRT policy %d %s", p.ID, p.Name),
					"src":        "wan",
					"dest":       fwZoneName(rule.DestinationZone),
					"src_dport":  strconv.Itoa(rule.ExternalPort),
					"dest_ip":    rule.DestinationIP,
					"dest_port":  strconv.Itoa(rule.DestinationPort),
					"target":     "DNAT",
					"family":     "ipv4",
					OwnershipTag: "1",
				}, Manages: append([]string(nil), policyRedirectManagedOptions...)}
			copyNonempty(section.Values, "src_ip", rule.SourceCIDR)
			setProtocols(&section, rule.Protocols)
			out = append(out, section)

		case model.PolicyStaticRoute:
			rule := p.StaticRoute
			iface := "wan"
			if rule.NetworkID > 0 {
				network, _ := site.NetworkByID(rule.NetworkID)
				iface = netIfaceName(network.Name)
				if !desiredSection(desired, "network", iface) {
					omissions = append(omissions, Omission{WLAN: p.Name, Kind: KindUndetermined,
						Reason: "the selected managed network was not renderable on this Gateway, so its route was left out rather than attached to an invented interface"})
					continue
				}
			}
			values := map[string]string{
				"interface":  iface,
				"target":     rule.Target,
				"gateway":    rule.Gateway,
				"table":      "main",
				OwnershipTag: "1",
			}
			if rule.Metric > 0 {
				values["metric"] = strconv.Itoa(rule.Metric)
			}
			out = append(out, Section{Config: "network", Type: "route", Name: name,
				Values: values, Manages: []string{"disabled", "enabled", "metric", "netmask", "mtu", "table", "valid", "source", "onlink", "type", "proto"}})
		}
	}

	activeZones := site.ActiveZoneNames()
	for _, client := range site.PolicyClients {
		macs, err := model.CanonicalMACs([]string{client.MAC})
		if err != nil {
			continue // Site.Validate reports the exact stored-state error.
		}
		mac := macs[0]
		if client.Blocked {
			if len(activeZones) == 0 {
				conflicts = append(conflicts, Conflict{Config: "firewall", Section: "client-" + safe(mac),
					Reason: fmt.Sprintf("client %s is marked blocked, but the site has no active managed source zone. The device's foreign lan zone is never edited or guessed; create/assign a managed network first", mac)})
			} else {
				for _, zone := range activeZones {
					fw := fwZoneName(zone)
					if !zones[fw] {
						conflicts = append(conflicts, missingPolicyZoneConflict("block "+mac))
						continue
					}
					out = append(out, Section{Config: "firewall", Type: "rule",
						Name: fmt.Sprintf("%s_client_block_%s_%s", NamePrefix, safe(mac), safe(zone)),
						Values: map[string]string{
							"name": "oonfeeWRT client-block " + mac + " " + zone,
							// fw4 treats an explicit wildcard destination as the
							// forward chain. Omitting dest would reject router input
							// and could break DHCP/DNS instead.
							"src": fw, "dest": "*", "proto": "all", "target": "REJECT",
							OwnershipTag: "1",
						}, Lists: map[string][]string{"src_mac": {mac}},
						Manages: append([]string(nil), policyRuleManagedOptions...)})
				}
			}
		}
		if client.FixedIP != "" {
			network, ok := fixedIPNetwork(site, client.FixedIP)
			iface := ""
			if ok {
				iface = netIfaceName(network.Name)
			}
			if !ok || !dhcpNetworks[iface] {
				omissions = append(omissions, Omission{WLAN: mac, Kind: KindUndetermined,
					Reason: "the fixed IP's managed DHCP network was not renderable on this Gateway, so no partial static lease was written"})
				continue
			}
			out = append(out, Section{Config: "dhcp", Type: "host",
				Name:   fmt.Sprintf("%s_fixed_%s", NamePrefix, safe(mac)),
				Values: map[string]string{"mac": mac, "ip": client.FixedIP, OwnershipTag: "1"},
				Manages: []string{"enable", "enabled", "instance", "force", "networkid", "dhcp_option", "name", "hostid",
					"dns", "duid", "tag", "match_tag", "broadcast", "leasetime"}})
		}
	}

	conflicts = append(conflicts, foreignExpandedPolicyConflicts(site, existing)...)
	return out, omissions, conflicts
}

func policySectionKind(kind model.PolicyKind) string {
	switch kind {
	case model.PolicyFirewallRule:
		return "rule"
	case model.PolicyPortForward:
		return "dnat"
	case model.PolicyStaticRoute:
		return "route"
	default:
		return "unknown"
	}
}

func setProtocols(section *Section, protocols []string) {
	protocols = model.CanonicalProtocols(protocols)
	if section.Lists == nil {
		section.Lists = map[string][]string{}
	}
	if len(protocols) == 1 {
		section.Values["proto"] = protocols[0]
		return
	}
	section.Lists["proto"] = protocols
}

func copyNonempty(values map[string]string, key, value string) {
	if value != "" {
		values[key] = value
	}
}

func policyZonesRendered(source, destination string, rendered map[string]bool) bool {
	if !rendered[fwZoneName(source)] {
		return false
	}
	return destination == "" || rendered[fwZoneName(destination)]
}

func missingPolicyZoneConflict(name string) Conflict {
	return Conflict{Config: "firewall", Section: "policy-" + safe(name),
		Reason: fmt.Sprintf("policy %q references a managed zone that was not renderable on this Gateway. Applying the rule without its source/destination zone would change its scope, so the whole device is blocked", name)}
}

func desiredSection(doc Doc, config, name string) bool {
	for _, section := range doc.Sections {
		if section.Config == config && section.Name == name {
			return true
		}
	}
	return false
}

func fixedIPNetwork(site model.Site, raw string) (model.Network, bool) {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return model.Network{}, false
	}
	var found model.Network
	n := 0
	for _, network := range site.Networks {
		prefix, err := netip.ParsePrefix(network.CIDR)
		if network.Enabled && network.VLAN > 1 && err == nil && prefix.Contains(addr) {
			found, n = network, n+1
		}
	}
	return found, n == 1
}

func foreignExpandedPolicyConflicts(site model.Site, existing Existing) []Conflict {
	var out []Conflict
	for _, policy := range site.Policies {
		if !policy.Enabled {
			continue
		}
		switch policy.Kind {
		case model.PolicyFirewallRule:
			out = append(out, foreignRuleConflicts(policy, existing)...)
		case model.PolicyPortForward:
			out = append(out, foreignRedirectConflicts(policy, existing)...)
		case model.PolicyStaticRoute:
			out = append(out, foreignRouteConflicts(policy, existing)...)
		}
	}
	for _, client := range site.PolicyClients {
		if client.Blocked {
			for _, zone := range site.ActiveZoneNames() {
				out = append(out, foreignClientBlockConflicts(client, zone, existing)...)
			}
		}
		if client.FixedIP != "" {
			out = append(out, foreignHostConflicts(client, existing)...)
		}
	}
	return dedupeConflicts(out)
}

func foreignClientBlockConflicts(client model.PolicyClient, zone string, existing Existing) []Conflict {
	macs, err := model.CanonicalMACs([]string{client.MAC})
	if err != nil {
		return nil
	}
	wantMAC, src := macs[0], fwZoneName(zone)
	var out []Conflict
	for name, values := range existing.In("firewall") {
		if values[OwnershipTag] == "1" || uciOptionFalse(values["enabled"]) ||
			!foreignSourceCouldMatch(values["src"], src) ||
			!foreignMACCouldMatch(values["src_mac"], wantMAC) {
			continue
		}
		contradicts := false
		switch values[".type"] {
		case "forwarding":
			contradicts = strings.TrimSpace(values["dest"]) != ""
		case "rule":
			contradicts = strings.TrimSpace(values["dest"]) != "" &&
				strings.EqualFold(strings.TrimSpace(values["target"]), "ACCEPT")
		case "redirect":
			contradicts = strings.TrimSpace(values["dest_ip"]) != "" &&
				strings.EqualFold(strings.TrimSpace(values["target"]), "DNAT")
		}
		if contradicts {
			out = append(out, Conflict{Config: "firewall", Section: name,
				Reason: fmt.Sprintf("foreign firewall section %s can allow forwarded traffic for blocked client %s from managed zone %s. The controller will not reorder, edit or delete human-owned policy", name, wantMAC, src)})
		}
	}
	return out
}

func foreignMACCouldMatch(raw, want string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed := false
	for _, field := range strings.Fields(strings.NewReplacer(",", " ", ";", " ").Replace(raw)) {
		if strings.HasPrefix(field, "!") {
			return true // exclusion syntax is not proof of disjointness
		}
		macs, err := model.CanonicalMACs([]string{field})
		if err != nil {
			return true
		}
		parsed = true
		if macs[0] == want {
			return true
		}
	}
	return !parsed
}

func foreignRuleConflicts(policy model.Policy, existing Existing) []Conflict {
	rule := policy.Firewall
	if rule == nil {
		return nil
	}
	src, dest := fwZoneName(rule.SourceZone), fwZoneName(rule.DestinationZone)
	var out []Conflict
	for name, values := range existing.In("firewall") {
		if values[OwnershipTag] == "1" || uciOptionFalse(values["enabled"]) {
			continue
		}
		foreignDest := values["dest"]
		if !foreignSourceCouldMatch(values["src"], src) {
			continue
		}
		if dest == "" {
			if foreignDest != "" && foreignDest != "*" {
				continue
			}
		} else if foreignDest != dest && foreignDest != "*" {
			continue
		}
		target := strings.ToUpper(strings.TrimSpace(values["target"]))
		contradicts := false
		switch values[".type"] {
		case "forwarding":
			contradicts = rule.Action != model.FirewallAccept && dest != ""
		case "rule":
			wantAccept := rule.Action == model.FirewallAccept
			contradicts = target == "ACCEPT" && !wantAccept ||
				(target == "DROP" || target == "REJECT") && wantAccept
		case "redirect":
			contradicts = strings.TrimSpace(values["dest_ip"]) != "" && rule.Action != model.FirewallAccept
		}
		if contradicts {
			out = append(out, Conflict{Config: "firewall", Section: name,
				Reason: fmt.Sprintf("foreign firewall section %s can contradict managed policy %q for %s to %s. oonfeeWRT will not reorder, edit or delete human-owned policy", name, policy.Name, src, displayDestination(dest))})
		}
	}
	return out
}

func foreignRedirectConflicts(policy model.Policy, existing Existing) []Conflict {
	rule := policy.PortForward
	if rule == nil {
		return nil
	}
	var out []Conflict
	for name, values := range existing.In("firewall") {
		if values[OwnershipTag] == "1" || uciOptionFalse(values["enabled"]) ||
			!foreignSourceCouldMatch(values["src"], "wan") {
			continue
		}
		protocolOverlaps := foreignProtocolsOverlap(values["proto"], rule.Protocols)
		switch values[".type"] {
		case "redirect":
			if !protocolOverlaps || !portCouldMatch(values["src_dport"], rule.ExternalPort) {
				continue
			}
			out = append(out, Conflict{Config: "firewall", Section: name,
				Reason: fmt.Sprintf("foreign redirect %s can claim WAN port %d used by managed port forward %q; ownership and evaluation order make the result unverifiable", name, rule.ExternalPort, policy.Name)})
		case "rule":
			target := strings.ToUpper(strings.TrimSpace(values["target"]))
			if target != "DROP" && target != "REJECT" || !familyCouldMatchIPv4(values["family"]) ||
				!foreignDestinationCouldMatch(values["dest"], fwZoneName(rule.DestinationZone)) || !protocolOverlaps ||
				!foreignCIDRsCouldOverlap(values["src_ip"], rule.SourceCIDR) ||
				!foreignCIDRContains(values["dest_ip"], rule.DestinationIP) ||
				!portCouldMatch(values["dest_port"], rule.DestinationPort) {
				continue
			}
			out = append(out, Conflict{Config: "firewall", Section: name,
				Reason: fmt.Sprintf("foreign WAN denial %s can match the post-DNAT destination of managed port forward %q before firewall4 accepts the translated flow; oonfeeWRT will not reorder or edit human-owned policy", name, policy.Name)})
		}
	}
	return out
}

func foreignProtocolsOverlap(raw string, protocols []string) bool {
	for _, protocol := range protocols {
		if protocolCouldMatch(raw, protocol) {
			return true
		}
	}
	return false
}

func foreignDestinationCouldMatch(raw, want string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if raw == "*" || raw == want {
		return true
	}
	return strings.ContainsAny(raw, "!{},;\"'") || len(strings.Fields(raw)) != 1
}

func foreignCIDRsCouldOverlap(raw, want string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || want == "" {
		return true
	}
	a, errA := netip.ParsePrefix(raw)
	b, errB := netip.ParsePrefix(want)
	return errA != nil || errB != nil || a.Overlaps(b)
}

func foreignCIDRContains(raw, wantIP string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	want, wantErr := netip.ParseAddr(wantIP)
	if prefix, err := netip.ParsePrefix(raw); err == nil && wantErr == nil {
		return prefix.Contains(want)
	}
	if addr, err := netip.ParseAddr(raw); err == nil && wantErr == nil {
		return addr == want
	}
	return true
}

func foreignRouteConflicts(policy model.Policy, existing Existing) []Conflict {
	want, err := netip.ParsePrefix(policy.StaticRoute.Target)
	if err != nil {
		return nil
	}
	var out []Conflict
	for name, values := range existing.In("network") {
		if values[OwnershipTag] == "1" || uciOptionTrue(values["disabled"]) {
			continue
		}
		switch values[".type"] {
		case "rule":
			out = append(out, Conflict{Config: "network", Section: name,
				Reason: fmt.Sprintf("foreign policy-routing rule %s can bypass managed route %q; oonfeeWRT will not infer or reorder a human-owned routing table", name, policy.Name)})
		case "route":
			got, ok := routePrefix(values)
			if !ok || got.Overlaps(want) {
				out = append(out, Conflict{Config: "network", Section: name,
					Reason: fmt.Sprintf("foreign route %s overlaps or cannot be proven disjoint from managed route %q (%s)", name, policy.Name, want)})
			}
		}
	}
	return out
}

func routePrefix(values map[string]string) (netip.Prefix, bool) {
	target := strings.TrimSpace(values["target"])
	if prefix, err := netip.ParsePrefix(target); err == nil && prefix.Addr().Is4() {
		return prefix.Masked(), true
	}
	addr, err := netip.ParseAddr(target)
	if err != nil || !addr.Is4() {
		return netip.Prefix{}, false
	}
	bits := 32
	if raw := strings.TrimSpace(values["netmask"]); raw != "" {
		mask := net.ParseIP(raw).To4()
		if mask == nil {
			return netip.Prefix{}, false
		}
		var ok bool
		bits, _ = net.IPMask(mask).Size()
		ok = bits >= 0
		if !ok {
			return netip.Prefix{}, false
		}
	}
	return netip.PrefixFrom(addr, bits).Masked(), true
}

func foreignHostConflicts(client model.PolicyClient, existing Existing) []Conflict {
	macs, err := model.CanonicalMACs([]string{client.MAC})
	if err != nil {
		return nil
	}
	wantMAC := macs[0]
	var out []Conflict
	for name, values := range existing.In("dhcp") {
		if values[OwnershipTag] == "1" || values[".type"] != "host" || uciOptionFalse(values["enable"]) {
			continue
		}
		rawMAC := strings.TrimSpace(values["mac"])
		macConflict := rawMAC != "" && foreignMACCouldMatch(rawMAC, wantMAC)
		if rawMAC == "" {
			for _, option := range []string{"duid", "hostid", "id", "name"} {
				macConflict = macConflict || strings.TrimSpace(values[option]) != ""
			}
		}
		if macConflict || strings.TrimSpace(values["ip"]) == client.FixedIP {
			out = append(out, Conflict{Config: "dhcp", Section: name,
				Reason: fmt.Sprintf("foreign static lease %s already claims client %s or address %s; oonfeeWRT will not overwrite it", name, wantMAC, client.FixedIP)})
		}
	}
	return out
}

func displayDestination(dest string) string {
	if dest == "" {
		return "the router"
	}
	return dest
}

func dedupeConflicts(in []Conflict) []Conflict {
	sort.Slice(in, func(i, j int) bool {
		if in[i].Config != in[j].Config {
			return in[i].Config < in[j].Config
		}
		if in[i].Section != in[j].Section {
			return in[i].Section < in[j].Section
		}
		return in[i].Reason < in[j].Reason
	})
	out := in[:0]
	for _, conflict := range in {
		if len(out) == 0 || out[len(out)-1] != conflict {
			out = append(out, conflict)
		}
	}
	return out
}
