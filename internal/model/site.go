// Package model is the desired state: what the operator asked for, expressed
// once, for the whole site.
//
// Nothing here knows about UCI, devices' quirks, or ubus. That translation is
// internal/render's job, and keeping the two apart is what lets the same WLAN
// definition fan out across radios and hardware classes without the site model
// growing per-device special cases.
package model

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// Band is a radio band a WLAN may be published on.
type Band string

const (
	Band2G Band = "2g"
	Band5G Band = "5g"
	Band6G Band = "6g"
)

// Valid reports a band the model knows about.
func (b Band) Valid() bool {
	return b == Band2G || b == Band5G || b == Band6G
}

// BandForFrequency classifies a radio by its operating frequency in MHz.
// Capability probing reports frequency; the site model speaks in bands.
func BandForFrequency(mhz int) (Band, bool) {
	switch {
	case mhz >= 2400 && mhz < 2500:
		return Band2G, true
	case mhz >= 5000 && mhz < 5900:
		return Band5G, true
	case mhz >= 5925 && mhz <= 7125:
		return Band6G, true
	}
	return "", false
}

// SecurityMode is the WLAN's authentication scheme, in UCI's vocabulary so the
// renderer does not have to invent a mapping.
type SecurityMode string

const (
	SecSAE      SecurityMode = "sae"       // WPA3 only
	SecSAEMixed SecurityMode = "sae-mixed" // WPA2/WPA3 transitional
	SecPSK2     SecurityMode = "psk2"      // WPA2-PSK
	SecOWE      SecurityMode = "owe"       // opportunistic wireless encryption
	SecNone     SecurityMode = "none"      // open
)

// NeedsKey reports whether the mode requires a passphrase.
func (m SecurityMode) NeedsKey() bool {
	return m == SecSAE || m == SecSAEMixed || m == SecPSK2
}

// PMF is protected management frames. WPA3 requires it; WPA2 may offer it.
type PMF string

const (
	PMFDisabled PMF = "0"
	PMFOptional PMF = "1"
	PMFRequired PMF = "2"
)

// Security is how a WLAN authenticates.
type Security struct {
	Mode SecurityMode
	Key  string
	PMF  PMF
}

// Roaming is the 802.11r/k/v feature set. Consistent roaming config across
// every AP is the thing a controller uniquely guarantees — it is essentially
// impossible to maintain by hand across a fleet.
type Roaming struct {
	FT       bool // 802.11r fast transition
	FTOverDS bool // over-the-DS rather than over-the-air
	KV       bool // 802.11k neighbour reports + 802.11v BSS transition

	// FTWithPSK2 records that the operator explicitly accepted the
	// compatibility warning for 802.11r on WPA2-PSK, which breaks some older
	// clients. Render enforces the flag; the UI is responsible for the warning.
	FTWithPSK2 bool
}

// WLANOptions are the per-SSID toggles that do not fit elsewhere.
type WLANOptions struct {
	Hidden   bool
	Isolate  bool
	MaxAssoc int
	// AllowUplink lets devices join this network as a 4-address (WDS) bridge
	// rather than as a client.
	//
	// Off by default and deliberately explicit. It changes what the AP accepts
	// from the air, and a network that quietly bridged anything presenting
	// 4-address frames is a different security posture than the operator asked
	// for. It is also the half people forget: configure the station and not
	// this, and the link silently never forms.
	AllowUplink bool
}

// DHCPConfig is the DHCP server policy for one network.
//
// Start is an offset from the subnet's network address and Limit is the number
// of leases, matching OpenWrt's /etc/config/dhcp vocabulary. Keeping that
// vocabulary in the model makes the render exact while still letting it reject
// a pool that falls outside the subnet before anything reaches a router.
type DHCPConfig struct {
	Enabled   bool   `json:"enabled"`
	Start     int    `json:"start"`
	Limit     int    `json:"limit"`
	LeaseTime string `json:"leasetime"`
}

// DefaultDHCPConfig is the policy older network rows received from the
// renderer before DHCP became editable. It is also the default for a new
// network, so upgrading does not create a device diff by itself.
func DefaultDHCPConfig() DHCPConfig {
	return DHCPConfig{Enabled: true, Start: 100, Limit: 150, LeaseTime: "12h"}
}

var dhcpLeaseTime = regexp.MustCompile(`^(?:[1-9][0-9]*[smhdw]|infinite)$`)

// Validate checks the values dnsmasq cannot safely repair for us.
func (d DHCPConfig) Validate(cidr string) error {
	if !d.Enabled {
		return nil
	}
	if d.Start < 1 {
		return errf("DHCP pool start must be at least 1 (an offset from the network address)")
	}
	if d.Limit < 1 {
		return errf("DHCP pool limit must be at least 1 lease")
	}
	if !dhcpLeaseTime.MatchString(strings.TrimSpace(d.LeaseTime)) {
		return errf("DHCP lease time %q is invalid; use a number followed by s, m, h, d or w, or infinite", d.LeaseTime)
	}

	prefix, err := validateNetworkCIDR(cidr)
	if err != nil {
		return err
	}
	if prefix.Bits() > 30 {
		return errf("DHCP needs an IPv4 subnet with usable host addresses; /%d has none", prefix.Bits())
	}
	hosts := uint64(1) << (32 - prefix.Bits())
	last := uint64(d.Start) + uint64(d.Limit) - 1
	if last >= hosts-1 {
		return errf("DHCP pool offsets %d–%d do not fit this /%d subnet; the highest usable offset is %d",
			d.Start, last, prefix.Bits(), hosts-2)
	}

	gateway := ipv4HostOffset(prefix)
	if gateway >= uint64(d.Start) && gateway <= last {
		return errf("DHCP pool offsets %d–%d include the gateway address at offset %d",
			d.Start, last, gateway)
	}
	return nil
}

// validateNetworkCIDR validates the address the router itself will use. A
// syntactically valid prefix can still name the subnet or broadcast address;
// Linux may accept that string, but it is not a usable gateway for clients.
func validateNetworkCIDR(cidr string) (netip.Prefix, error) {
	raw := strings.TrimSpace(cidr)
	prefix, err := netip.ParsePrefix(raw)
	if err != nil || !prefix.Addr().Is4() {
		return netip.Prefix{}, errf("network address %q must be an IPv4 address in CIDR form", cidr)
	}
	if prefix.Bits() < 8 {
		return netip.Prefix{}, errf("network address %q uses a prefix shorter than /8, which is refused as a likely typo", cidr)
	}
	if prefix.Bits() <= 30 {
		hosts := uint64(1) << (32 - prefix.Bits())
		offset := ipv4HostOffset(prefix)
		if offset == 0 {
			return netip.Prefix{}, errf("network address %q uses the subnet address as its gateway; choose a usable host address", cidr)
		}
		if offset == hosts-1 {
			return netip.Prefix{}, errf("network address %q uses the broadcast address as its gateway; choose a usable host address", cidr)
		}
	}
	return prefix, nil
}

func ipv4HostOffset(prefix netip.Prefix) uint64 {
	addr, network := prefix.Addr().As4(), prefix.Masked().Addr().As4()
	value := func(a [4]byte) uint64 {
		return uint64(uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]))
	}
	return value(addr) - value(network)
}

// Network is one L2/L3 segment: a VLAN with addressing.
type Network struct {
	ID   int
	Name string // the UCI interface name, e.g. "lan"
	VLAN int
	CIDR string
	Zone string
	// Nil means the caller omitted DHCP. A pointer is intentional: disabled is
	// a real value, not the same thing as an absent configuration.
	DHCP *DHCPConfig
	// LegacyDHCPDefaults records an upgraded dhcp_json={} row. Its effective
	// values remain the historical defaults, but the distinction matters when
	// those defaults cannot fit a small subnet: the operator must explicitly
	// customize or disable the pool before any site apply.
	LegacyDHCPDefaults bool
	Enabled            bool
}

// EffectiveDHCP returns the explicit policy or the historical default.
func (n Network) EffectiveDHCP() DHCPConfig {
	if n.DHCP == nil {
		return DefaultDHCPConfig()
	}
	return *n.DHCP
}

// WLAN is one SSID, published on some bands, for some group of APs.
type WLAN struct {
	ID        int
	SSID      string
	NetworkID int
	GroupID   int
	Bands     []Band
	Security  Security
	Roaming   Roaming
	Options   WLANOptions
	Enabled   bool
}

// PublishesOn reports whether this WLAN wants the given band.
func (w WLAN) PublishesOn(b Band) bool {
	for _, want := range w.Bands {
		if want == b {
			return true
		}
	}
	return false
}

// APGroup is a set of devices that share WLAN assignments.
type APGroup struct {
	ID        int
	Name      string
	DeviceIDs []int64
}

// Contains reports membership.
func (g APGroup) Contains(deviceID int64) bool {
	for _, id := range g.DeviceIDs {
		if id == deviceID {
			return true
		}
	}
	return false
}

// Device is the site model's view of a managed device. The operational view
// (address, credential, capabilities) lives in the store; this is only what
// rendering needs.
type Device struct {
	ID        int64
	Name      string
	Role      Role // legacy primary role; Functions is authoritative when set
	Functions DeviceFunctions
}

// EffectiveFunctions preserves legacy struct literals while making every
// renderer decision against the independently selected functions.
func (d Device) EffectiveFunctions() DeviceFunctions {
	if d.Functions == nil {
		return FunctionsForRole(d.Role)
	}
	return d.Functions
}

// Site is the whole desired state.
//
// UUID is stable for the life of the site and feeds the deterministic
// mobility-domain derivation, so every AP computes the same value with no
// coordination between them.
type Site struct {
	UUID     string
	Name     string
	Networks []Network
	// Zones contains only explicit directional forwarding policies. Use
	// EffectiveZonePolicies for the legacy source -> wan default when absent.
	Zones []ZonePolicy
	// Policies are ordered firewall/NAT/routing records. PolicyClients carries
	// only desired client actions; observed client state is deliberately absent.
	Policies      []Policy
	PolicyClients []PolicyClient
	WLANs         []WLAN
	// Meshes are 802.11s backhauls. Separate from WLANs because a mesh point is
	// a different interface mode, not a WLAN with a flag — see mesh.go.
	Meshes []Mesh
	// Uplinks are devices that reach the network over the air rather than over
	// a cable. See uplink.go for why this is a device property rather than a
	// bridge object.
	Uplinks []Uplink
	Groups  []APGroup
	// Overrides are the places individual devices are allowed to differ. See
	// override.go for what is overridable and, more importantly, what is not.
	Overrides Overrides
}

// NetworkByID looks up a network.
func (s Site) NetworkByID(id int) (Network, bool) {
	for _, n := range s.Networks {
		if n.ID == id {
			return n, true
		}
	}
	return Network{}, false
}

// GroupByID looks up an AP group.
func (s Site) GroupByID(id int) (APGroup, bool) {
	for _, g := range s.Groups {
		if g.ID == id {
			return g, true
		}
	}
	return APGroup{}, false
}

// WLANsFor returns the enabled WLANs assigned to a device via its groups, in
// stable ID order so a render is reproducible.
func (s Site) WLANsFor(deviceID int64) []WLAN {
	var out []WLAN
	for _, w := range s.WLANs {
		if !w.Enabled {
			continue
		}
		g, ok := s.GroupByID(w.GroupID)
		if !ok || !g.Contains(deviceID) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// Validate catches the site-level mistakes that would otherwise become
// confusing per-device render failures.
func (s Site) Validate() []error {
	var errs []error
	if strings.TrimSpace(s.UUID) == "" {
		errs = append(errs, errf("site UUID is required: it seeds the "+
			"mobility-domain derivation that keeps roaming consistent across APs"))
	}
	errs = append(errs, s.ValidateZoneNames()...)
	seenVLAN := map[int]string{}
	type routedSubnet struct {
		name   string
		prefix netip.Prefix
	}
	var routed []routedSubnet
	for _, n := range s.Networks {
		if prev, dup := seenVLAN[n.VLAN]; dup {
			errs = append(errs, errf("VLAN %d is used by both %q and %q", n.VLAN, prev, n.Name))
		}
		seenVLAN[n.VLAN] = n.Name
		// VLAN 0/1 is the operator-owned LAN and disabled networks render
		// nothing. Their dormant DHCP values must not block an unrelated apply.
		if n.Enabled && n.VLAN > 1 {
			prefix, err := validateNetworkCIDR(n.CIDR)
			if err != nil {
				errs = append(errs, errf("network %q: %v", n.Name, err))
				continue
			}
			routed = append(routed, routedSubnet{name: n.Name, prefix: prefix.Masked()})
			if err := n.EffectiveDHCP().Validate(n.CIDR); err != nil {
				if n.LegacyDHCPDefaults {
					errs = append(errs, errf("network %q still inherits the legacy DHCP defaults "+
						"(pool offsets 100–249, lease 12h), which are unsafe for %s: %v. "+
						"Open Networks → %s and either customize Pool start and Pool limit, "+
						"or turn DHCP server off, then Save. Applying is blocked until one is "+
						"chosen; no device was changed", n.Name, n.CIDR, err, n.Name))
				} else {
					errs = append(errs, errf("network %q: %v", n.Name, err))
				}
			}
		}
	}
	// Linux installs a connected route for every addressed interface. Two active
	// VLANs with overlapping prefixes therefore make egress selection ambiguous,
	// even though each address and DHCP pool is valid in isolation.
	sort.Slice(routed, func(i, j int) bool {
		if c := routed[i].prefix.Addr().Compare(routed[j].prefix.Addr()); c != 0 {
			return c < 0
		}
		if routed[i].prefix.Bits() != routed[j].prefix.Bits() {
			return routed[i].prefix.Bits() < routed[j].prefix.Bits()
		}
		return routed[i].name < routed[j].name
	})
	for i := range routed {
		for j := i + 1; j < len(routed); j++ {
			if !routed[i].prefix.Contains(routed[j].prefix.Addr()) &&
				!routed[j].prefix.Contains(routed[i].prefix.Addr()) {
				continue
			}
			errs = append(errs, errf("networks %q (%s) and %q (%s) overlap; give each active routed VLAN a distinct IPv4 subnet",
				routed[i].name, routed[i].prefix, routed[j].name, routed[j].prefix))
		}
	}
	errs = append(errs, s.ValidateZonePolicies()...)
	errs = append(errs, s.ValidatePolicies()...)
	for _, w := range s.WLANs {
		if strings.TrimSpace(w.SSID) == "" {
			errs = append(errs, errf("WLAN %d has no SSID", w.ID))
		}
		if _, ok := s.NetworkByID(w.NetworkID); !ok {
			errs = append(errs, errf("WLAN %q references unknown network %d", w.SSID, w.NetworkID))
		}
		if _, ok := s.GroupByID(w.GroupID); !ok {
			errs = append(errs, errf("WLAN %q references unknown AP group %d", w.SSID, w.GroupID))
		}
		if len(w.Bands) == 0 {
			errs = append(errs, errf("WLAN %q selects no bands", w.SSID))
		}
		if w.Security.Mode.NeedsKey() && w.Security.Key == "" {
			errs = append(errs, errf("WLAN %q uses %s but has no key", w.SSID, w.Security.Mode))
		}
	}
	for _, m := range s.Meshes {
		errs = append(errs, m.Validate()...)
		if _, ok := s.NetworkByID(m.NetworkID); !ok {
			errs = append(errs, errf("mesh %q references unknown network %d",
				m.MeshID, m.NetworkID))
		}
		if _, ok := s.GroupByID(m.GroupID); !ok {
			errs = append(errs, errf("mesh %q references unknown AP group %d",
				m.MeshID, m.GroupID))
		}
	}

	// Uplinks, checked here rather than only where they render.
	//
	// They were not, and the guard was therefore dead: every sentence
	// Uplink.Validate was written to produce — the network is disabled, it does
	// not accept wireless bridges, it is not published on that band — could
	// never reach an operator. Found by review before an uplink could be
	// created through the product, which is the only reason it was latent
	// rather than shipped. §6's rule about guards that cannot fire.
	for _, u := range s.Uplinks {
		if !u.Enabled {
			continue
		}
		errs = append(errs, u.Validate(s)...)
	}
	// Two meshes with the same ID on the same band are one mesh whose config
	// disagrees with itself: nodes peer on the mesh ID, so whichever section
	// hostapd reads last wins and the other's settings vanish silently.
	seenMesh := map[string]bool{}
	for _, m := range s.Meshes {
		k := strings.ToLower(strings.TrimSpace(m.MeshID)) + "/" + string(m.Band)
		if seenMesh[k] {
			errs = append(errs, errf("mesh %q is defined twice on %s; nodes peer "+
				"on the mesh ID, so these are one mesh with two conflicting "+
				"configurations", m.MeshID, m.Band))
		}
		seenMesh[k] = true
	}
	return errs
}
