package model

import (
	"sort"
	"strings"
)

// FirewallZoneMaxLen is the fw4 firewall-zone identifier limit.
const FirewallZoneMaxLen = 11

// FirewallZoneName returns the identifier fw4 actually sees. Human names are
// reduced to UCI-safe lower-case characters and fw4 reads only the first 11.
// Keeping this in the model and using it from the renderer prevents validation
// and rendering from disagreeing about aliases such as "wan!".
func FirewallZoneName(name string) string {
	base := firewallZoneBase(name)
	if base == "" {
		return "net"
	}
	if len(base) > FirewallZoneMaxLen {
		return base[:FirewallZoneMaxLen]
	}
	return base
}

func firewallZoneBase(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == ' ':
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// ZonePolicy is the set of destinations to which one managed firewall zone
// may initiate connections. Forwarding is directional: replies are admitted
// by OpenWrt's stateful firewall, but a destination cannot initiate a new
// connection back unless it has its own reverse edge.
//
// Explicit is false only for the effective legacy default. Persisted rows are
// explicit, including an empty ForwardTo list (block every forwarding edge).
type ZonePolicy struct {
	Name      string
	ForwardTo []string
	Explicit  bool
}

// ActiveZoneNames returns the distinct firewall zones backed by enabled,
// controller-managed routed networks. VLAN 0/1 is the device's foreign LAN and
// never produces a managed zone.
func (s Site) ActiveZoneNames() []string {
	// Group by the identifier fw4 sees first. A syntactically invalid source
	// or two distinct labels that collapse to one identifier is not manageable
	// and therefore must not leak into the effective/API matrix; Site.Validate
	// reports the reason so it can be repaired.
	byID := map[string]map[string]bool{}
	for _, n := range s.Networks {
		if !n.Enabled || n.VLAN <= 1 {
			continue
		}
		raw := n.Zone
		if raw == "" {
			raw = n.Name
		}
		name := strings.TrimSpace(raw)
		if name == "" || name != raw {
			continue
		}
		base := firewallZoneBase(name)
		if base == "" || base[0] < 'a' || base[0] > 'z' {
			continue
		}
		id := FirewallZoneName(name)
		// lan and wan are the device's foreign zones. wan may be a destination;
		// neither can be a controller-managed source.
		if id == "lan" || id == "wan" {
			continue
		}
		if byID[id] == nil {
			byID[id] = map[string]bool{}
		}
		byID[id][name] = true
	}
	var out []string
	for _, labels := range byID {
		if len(labels) != 1 {
			continue
		}
		for name := range labels {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateZoneNames checks the identifiers that will exist on the router,
// rather than only their human labels.
func (s Site) ValidateZoneNames() []error {
	seen := map[string]string{}
	var errs []error
	for _, n := range s.Networks {
		if !n.Enabled || n.VLAN <= 1 {
			continue
		}
		raw := n.Zone
		if raw == "" {
			raw = n.Name
		}
		name := strings.TrimSpace(raw)
		if name == "" || name != raw {
			errs = append(errs, errf("network %q firewall zone %q must be a nonblank exact name without leading or trailing whitespace", n.Name, raw))
			continue
		}
		base := firewallZoneBase(name)
		switch {
		case base == "":
			errs = append(errs, errf("network %q has firewall zone %q, which contains no usable letters or digits", n.Name, name))
			continue
		case base[0] >= '0' && base[0] <= '9':
			errs = append(errs, errf("network %q has firewall zone %q, whose rendered identifier %q starts with a digit; OpenWrt firewall zone identifiers must start with a letter", n.Name, name, FirewallZoneName(name)))
			continue
		}
		id := FirewallZoneName(name)
		if id == "lan" || id == "wan" {
			detail := "device-owned zone"
			if id == "wan" {
				detail = "device-owned, destination-only uplink zone"
			}
			errs = append(errs, errf("network %q cannot use firewall zone %q: it renders as %s, the %s; choose a distinct managed zone name", n.Name, name, id, detail))
			continue
		}
		if previous, ok := seen[id]; ok && previous != name {
			errs = append(errs, errf("firewall zones %q and %q both render as %q, so OpenWrt cannot keep their policies separate; choose names that differ within the first %d usable characters", previous, name, id, FirewallZoneMaxLen))
			continue
		}
		seen[id] = name
	}
	return errs
}

// CanonicalZoneDestinations removes duplicates and returns stable ordering.
// It deliberately does not trim or otherwise repair names: policy validation
// requires an exact match with an active zone so a typo cannot become access.
func CanonicalZoneDestinations(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, name := range in {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// EffectiveZonePolicies returns one policy for each active managed source.
// A missing explicit row preserves the product's historical source -> wan
// forwarding exactly; an explicit empty list means no forwarding at all.
func (s Site) EffectiveZonePolicies() []ZonePolicy {
	byName := make(map[string]ZonePolicy, len(s.Zones))
	for _, p := range s.Zones {
		p.ForwardTo = CanonicalZoneDestinations(p.ForwardTo)
		p.Explicit = true
		byName[p.Name] = p
	}
	out := make([]ZonePolicy, 0, len(s.Networks))
	for _, name := range s.ActiveZoneNames() {
		if p, ok := byName[name]; ok {
			out = append(out, p)
		} else {
			out = append(out, ZonePolicy{
				Name: name, ForwardTo: []string{"wan"}, Explicit: false,
			})
		}
	}
	return out
}

// ValidateZonePolicies verifies only the directional firewall policy. Keeping
// this separate lets the store guard a network rename without making an
// unrelated, already-existing WLAN problem prevent the operator fixing it.
func (s Site) ValidateZonePolicies() []error {
	active := map[string]bool{}
	for _, name := range s.ActiveZoneNames() {
		active[name] = true
	}
	seen := map[string]bool{}
	var errs []error
	for _, p := range s.Zones {
		if p.Name == "" || p.Name != strings.TrimSpace(p.Name) {
			errs = append(errs, errf("zone policy source %q must be a nonblank exact zone name", p.Name))
			continue
		}
		if seen[p.Name] {
			errs = append(errs, errf("zone policy source %q is defined more than once", p.Name))
			continue
		}
		seen[p.Name] = true
		sourceID := FirewallZoneName(p.Name)
		if sourceID == "lan" || sourceID == "wan" {
			errs = append(errs, errf("zone policy source %q renders as reserved device-owned zone %q; wan is destination-only and lan is not controller-managed", p.Name, sourceID))
		} else if !active[p.Name] {
			errs = append(errs, errf("zone policy source %q is not an active managed zone", p.Name))
		}
		for _, dest := range p.ForwardTo {
			switch {
			case dest == "" || dest != strings.TrimSpace(dest):
				errs = append(errs, errf("zone policy %q has invalid destination %q; use an exact zone name", p.Name, dest))
			case dest == p.Name:
				errs = append(errs, errf("zone policy %q cannot forward to itself", p.Name))
			case FirewallZoneName(dest) == "wan" && dest != "wan":
				errs = append(errs, errf("zone policy %q destination %q renders as the reserved wan zone; use the exact destination name wan", p.Name, dest))
			case dest != "wan" && !active[dest]:
				errs = append(errs, errf("zone policy %q forwards to %q, which is not an active managed zone or wan", p.Name, dest))
			}
		}
	}
	return errs
}
