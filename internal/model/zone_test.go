package model

import (
	"strings"
	"testing"
)

func zoneSite() Site {
	return Site{UUID: "site", Networks: []Network{
		{ID: 1, Name: "lan", VLAN: 1, Zone: "lan", Enabled: true},
		{ID: 2, Name: "guest", VLAN: 20, Zone: "guest", Enabled: true},
		{ID: 3, Name: "iot", VLAN: 30, Zone: "iot", Enabled: true},
		{ID: 4, Name: "off", VLAN: 40, Zone: "off", Enabled: false},
	}}
}

func TestEffectiveZonePoliciesPreserveLegacyWanDefault(t *testing.T) {
	s := zoneSite()
	got := s.EffectiveZonePolicies()
	if len(got) != 2 || got[0].Name != "guest" || got[1].Name != "iot" {
		t.Fatalf("effective policies = %+v, want active sources in stable order", got)
	}
	for _, p := range got {
		if p.Explicit || len(p.ForwardTo) != 1 || p.ForwardTo[0] != "wan" {
			t.Errorf("legacy policy for %q = %+v, want implicit wan only", p.Name, p)
		}
	}
}

func TestEffectiveZonePolicyKeepsExplicitEmptyAndCanonicalizes(t *testing.T) {
	s := zoneSite()
	s.Zones = []ZonePolicy{
		{Name: "iot", ForwardTo: nil, Explicit: true},
		{Name: "guest", ForwardTo: []string{"wan", "iot", "wan"}, Explicit: true},
	}
	got := s.EffectiveZonePolicies()
	if len(got[0].ForwardTo) != 2 || got[0].ForwardTo[0] != "iot" || got[0].ForwardTo[1] != "wan" {
		t.Errorf("guest destinations = %v, want sorted/deduplicated", got[0].ForwardTo)
	}
	if got[1].ForwardTo == nil || len(got[1].ForwardTo) != 0 || !got[1].Explicit {
		t.Errorf("explicit block-all policy changed meaning: %+v", got[1])
	}
}

func TestZonePolicyValidationIsDirectionalAndClosed(t *testing.T) {
	for name, policy := range map[string]ZonePolicy{
		"self":           {Name: "guest", ForwardTo: []string{"guest"}},
		"unknown dest":   {Name: "guest", ForwardTo: []string{"missing"}},
		"unknown source": {Name: "missing", ForwardTo: []string{"wan"}},
		"spaced name":    {Name: " guest", ForwardTo: []string{"wan"}},
		"spaced dest":    {Name: "guest", ForwardTo: []string{" wan"}},
		"reserved wan":   {Name: "wan", ForwardTo: []string{"guest"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := zoneSite()
			s.Zones = []ZonePolicy{policy}
			if errs := s.ValidateZonePolicies(); len(errs) == 0 {
				t.Fatalf("policy %+v passed validation", policy)
			}
		})
	}

	s := zoneSite()
	s.Zones = []ZonePolicy{{Name: "guest", ForwardTo: []string{"iot", "wan"}}}
	if errs := s.ValidateZonePolicies(); len(errs) != 0 {
		t.Fatalf("valid one-way policy failed: %v", errs)
	}
}

func TestWanNeverBecomesManagedPolicySource(t *testing.T) {
	s := zoneSite()
	s.Networks = append(s.Networks,
		Network{ID: 5, Name: "bad", VLAN: 50, Zone: "WAN", Enabled: true})
	for _, name := range s.ActiveZoneNames() {
		if name == "wan" {
			t.Fatal("foreign wan zone became a managed policy source")
		}
	}
	for _, p := range s.EffectiveZonePolicies() {
		if p.Name == "wan" {
			t.Fatal("UI/effective contract exposed wan as a managed source")
		}
	}
	s.Zones = []ZonePolicy{{Name: "WaN", ForwardTo: []string{"guest"}}}
	errs := s.ValidateZonePolicies()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "destination-only") {
		t.Fatalf("wan source error = %v, want actionable destination-only message", errs)
	}
	s.Zones = nil
	found := false
	for _, err := range s.Validate() {
		found = found || strings.Contains(err.Error(), "destination-only")
	}
	if !found {
		t.Fatalf("network in reserved wan zone validation = %v", s.Validate())
	}
}

func TestManagedZoneNamesValidateTheirRenderedFw4Identity(t *testing.T) {
	for name, zones := range map[string][]string{
		"lan alias":          {"lan!"},
		"wan alias":          {"wan!"},
		"outer whitespace":   {" guest "},
		"leading digit":      {"20_guest"},
		"no usable name":     {"!!!"},
		"normalized collide": {"guest-zone-a", "guest zone a"},
		"truncated collide":  {"abcdefghijk-one", "abcdefghijk-two"},
	} {
		t.Run(name, func(t *testing.T) {
			s := Site{}
			for i, zone := range zones {
				s.Networks = append(s.Networks, Network{
					ID: i + 1, Name: "net" + string(rune('a'+i)), VLAN: 20 + i,
					Zone: zone, Enabled: true,
				})
			}
			if errs := s.ValidateZoneNames(); len(errs) == 0 {
				t.Fatalf("zones %v passed validation; rendered identities are unsafe", zones)
			}
			if active := s.ActiveZoneNames(); len(active) != 0 {
				t.Fatalf("invalid zones leaked into the effective/API sources: %v", active)
			}
		})
	}

	s := Site{Networks: []Network{
		{ID: 1, Name: "one", VLAN: 20, Zone: "shared", Enabled: true},
		{ID: 2, Name: "two", VLAN: 30, Zone: "shared", Enabled: true},
	}}
	if errs := s.ValidateZoneNames(); len(errs) != 0 {
		t.Fatalf("two networks deliberately sharing one zone failed: %v", errs)
	}
	if active := s.ActiveZoneNames(); len(active) != 1 || active[0] != "shared" {
		t.Fatalf("shared managed zone effective sources = %v", active)
	}
}
