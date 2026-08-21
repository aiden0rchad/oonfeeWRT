package model

import (
	"strings"
	"testing"
)

func TestLegacyNetworkGetsHistoricalDHCPDefaults(t *testing.T) {
	n := Network{}
	got := n.EffectiveDHCP()
	want := DHCPConfig{Enabled: true, Start: 100, Limit: 150, LeaseTime: "12h"}
	if got != want {
		t.Fatalf("legacy DHCP = %+v, want %+v", got, want)
	}
}

func TestDHCPPoolMustFitTheSubnetAndExcludeTheGateway(t *testing.T) {
	for _, tc := range []struct {
		name string
		cidr string
		dhcp DHCPConfig
		want string
	}{
		{"valid custom pool", "10.0.2.1/24", DHCPConfig{true, 20, 80, "30m"}, ""},
		{"outside subnet", "10.0.2.1/25", DHCPConfig{true, 100, 150, "12h"}, "do not fit"},
		{"contains gateway", "10.0.2.100/24", DHCPConfig{true, 100, 20, "12h"}, "gateway"},
		{"subnet address is not a gateway", "10.0.2.0/24", DHCPConfig{true, 20, 80, "12h"}, "subnet address"},
		{"broadcast address is not a gateway", "10.0.2.255/24", DHCPConfig{true, 20, 80, "12h"}, "broadcast address"},
		{"malformed CIDR", "10.0.2.1", DHCPConfig{true, 20, 80, "12h"}, "cidr"},
		{"ambiguous leading zero", "010.0.2.1/24", DHCPConfig{true, 20, 80, "12h"}, "cidr"},
		{"no usable hosts", "10.0.2.0/31", DHCPConfig{true, 1, 1, "12h"}, "usable host"},
		{"invalid lease", "10.0.2.1/24", DHCPConfig{true, 20, 80, "forever"}, "lease time"},
		// Disabled means no dnsmasq section is rendered, so dormant pool values
		// cannot make an intentionally static network invalid.
		{"disabled", "10.0.2.1/30", DHCPConfig{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.dhcp.Validate(tc.cidr)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error = %v, want text %q", err, tc.want)
			}
		})
	}
}

func TestInvalidNetworkAddressBlocksApplyEvenWithDHCPDisabled(t *testing.T) {
	d := DHCPConfig{Enabled: false, Start: 100, Limit: 150, LeaseTime: "12h"}
	for _, cidr := range []string{"", "10.0.20.1", "10.0.20.0/24", "10.0.20.255/24"} {
		s := Site{UUID: "site", Networks: []Network{{
			Name: "iot", VLAN: 20, CIDR: cidr, DHCP: &d, Enabled: true,
		}}}
		if errs := s.Validate(); len(errs) == 0 {
			t.Errorf("CIDR %q did not block apply", cidr)
		}
	}
}

func TestActiveRoutedSubnetsCannotOverlap(t *testing.T) {
	off := DHCPConfig{Enabled: false}
	wide := Network{Name: "wide", VLAN: 20, CIDR: "10.0.20.1/24", DHCP: &off, Enabled: true}
	narrow := Network{Name: "narrow", VLAN: 30, CIDR: "10.0.20.129/25", DHCP: &off, Enabled: true}

	var first string
	for _, networks := range [][]Network{{wide, narrow}, {narrow, wide}} {
		errs := (Site{UUID: "site", Networks: networks}).Validate()
		if len(errs) != 1 {
			t.Fatalf("validation = %v, want one overlap", errs)
		}
		got := errs[0].Error()
		for _, want := range []string{"wide", "10.0.20.0/24", "narrow", "10.0.20.128/25", "overlap"} {
			if !strings.Contains(got, want) {
				t.Errorf("overlap error %q does not name %q", got, want)
			}
		}
		if first != "" && got != first {
			t.Errorf("overlap error depends on model order:\nfirst:  %s\nsecond: %s", first, got)
		}
		first = got
	}
}

func TestDormantOrInvalidSubnetsDoNotCreateOverlapErrors(t *testing.T) {
	off := DHCPConfig{Enabled: false}
	s := Site{UUID: "site", Networks: []Network{
		{Name: "active", VLAN: 20, CIDR: "10.0.20.1/24", DHCP: &off, Enabled: true},
		{Name: "disabled", VLAN: 30, CIDR: "10.0.20.129/25", DHCP: &off, Enabled: false},
		{Name: "operator-lan", VLAN: 1, CIDR: "10.0.20.254/24", DHCP: &off, Enabled: true},
		// Individually invalid because .0 is the subnet address; report that
		// error, but do not compound it with an overlap claim.
		{Name: "invalid", VLAN: 40, CIDR: "10.0.20.0/25", DHCP: &off, Enabled: true},
	}}
	errs := s.Validate()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "subnet address") {
		t.Fatalf("validation = %v, want only the invalid-address error", errs)
	}
	if strings.Contains(errs[0].Error(), "overlap") {
		t.Fatalf("invalid/dormant subnet produced an overlap error: %v", errs)
	}
}

func TestInvalidDHCPBlocksTheWholeSiteBeforeRender(t *testing.T) {
	d := DHCPConfig{Enabled: true, Start: 100, Limit: 150, LeaseTime: "12h"}
	s := Site{UUID: "site", Networks: []Network{{
		Name: "small", VLAN: 20, CIDR: "10.0.2.1/25", DHCP: &d, Enabled: true,
	}}}
	errs := s.Validate()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "small") {
		t.Fatalf("validation = %v; the failing network must block apply by name", errs)
	}
}

func TestSmallSubnetLegacyDHCPRequiresExplicitCustomizeOrDisable(t *testing.T) {
	s := Site{UUID: "site", Networks: []Network{{
		Name: "small", VLAN: 20, CIDR: "10.0.2.1/25",
		DHCP:               func() *DHCPConfig { d := DefaultDHCPConfig(); return &d }(),
		LegacyDHCPDefaults: true, Enabled: true,
	}}}
	errs := s.Validate()
	if len(errs) != 1 {
		t.Fatalf("validation = %v, want one legacy DHCP diagnostic", errs)
	}
	got := errs[0].Error()
	for _, want := range []string{"legacy DHCP defaults", "customize Pool start", "turn DHCP server off", "Applying is blocked", "no device was changed"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic %q does not contain %q", got, want)
		}
	}

	// Either explicit resolution removes the upgrade blocker.
	custom := DHCPConfig{Enabled: true, Start: 20, Limit: 80, LeaseTime: "12h"}
	s.Networks[0].DHCP, s.Networks[0].LegacyDHCPDefaults = &custom, false
	if got := s.Validate(); len(got) != 0 {
		t.Fatalf("customized legacy pool remains invalid: %v", got)
	}
	off := DHCPConfig{Enabled: false}
	s.Networks[0].DHCP = &off
	if got := s.Validate(); len(got) != 0 {
		t.Fatalf("disabled legacy pool remains invalid: %v", got)
	}
}

func TestDormantDHCPDoesNotBlockAnUnrelatedApply(t *testing.T) {
	bad := DHCPConfig{Enabled: true, Start: 100, Limit: 150, LeaseTime: "12h"}
	for _, n := range []Network{
		{Name: "operator-lan", VLAN: 1, CIDR: "10.0.0.1/30", DHCP: &bad, Enabled: true},
		{Name: "disabled", VLAN: 20, CIDR: "10.0.20.1/30", DHCP: &bad, Enabled: false},
	} {
		s := Site{UUID: "site", Networks: []Network{n}}
		if errs := s.Validate(); len(errs) != 0 {
			t.Errorf("%s produced dormant DHCP errors: %v", n.Name, errs)
		}
	}
}
