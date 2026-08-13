// Package model is the desired state: what the operator asked for, expressed
// once, for the whole site.
//
// Nothing here knows about UCI, devices' quirks, or ubus. That translation is
// internal/render's job, and keeping the two apart is what lets the same WLAN
// definition fan out across radios and hardware classes without the site model
// growing per-device special cases.
package model

import "strings"

// Band is a radio band a WLAN may be published on.
type Band string

const (
	Band2G Band = "2g"
	Band5G Band = "5g"
	Band6G Band = "6g"
)

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
}

// Network is one L2/L3 segment: a VLAN with addressing.
type Network struct {
	ID      int
	Name    string // the UCI interface name, e.g. "lan"
	VLAN    int
	CIDR    string
	Zone    string
	Enabled bool
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
	ID   int64
	Name string
	Role string // gateway|ap|switch
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
	WLANs    []WLAN
	Groups   []APGroup
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
	seenVLAN := map[int]string{}
	for _, n := range s.Networks {
		if prev, dup := seenVLAN[n.VLAN]; dup {
			errs = append(errs, errf("VLAN %d is used by both %q and %q", n.VLAN, prev, n.Name))
		}
		seenVLAN[n.VLAN] = n.Name
	}
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
	return errs
}
