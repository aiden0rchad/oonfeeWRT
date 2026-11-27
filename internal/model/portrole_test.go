package model

import "testing"

func TestValidLANPortName(t *testing.T) {
	for _, port := range []string{"lan1", "lan2", "eth0", "sw0p1", "10g1"} {
		if !ValidLANPortName(port) {
			t.Errorf("%q should be a port name", port)
		}
	}
	for _, port := range []string{"", "lan1:u*", "lan 2", "lan1:t", "wan/1"} {
		if ValidLANPortName(port) {
			t.Errorf("%q should not be a port name", port)
		}
	}
	if !ValidLANPortName("x") {
		t.Fatal("single letter port names are real")
	}
}

func TestPortRoleShape(t *testing.T) {
	if err := (PortRole{DeviceID: 1, Port: "lan2", Mode: PortRoleTrunk}).ValidateShape(); err != nil {
		t.Fatal(err)
	}
	if err := (PortRole{DeviceID: 1, Port: "lan2", Mode: PortRoleAccess, NetworkID: 4}).ValidateShape(); err != nil {
		t.Fatal(err)
	}
	for _, role := range []PortRole{
		{Port: "lan2", Mode: PortRoleTrunk},
		{DeviceID: 1, Port: "lan2:t", Mode: PortRoleTrunk},
		{DeviceID: 1, Port: "lan2", Mode: "hybrid"},
		{DeviceID: 1, Port: "lan2", Mode: PortRoleTrunk, NetworkID: 4},
		{DeviceID: 1, Port: "lan2", Mode: PortRoleAccess},
	} {
		if err := role.ValidateShape(); err == nil {
			t.Errorf("accepted %+v", role)
		}
	}
}

func TestSiteRejectsDuplicateAndUnknownPortRoles(t *testing.T) {
	site := Site{UUID: "abc", Networks: []Network{{ID: 2, Name: "Dom", VLAN: 50, Enabled: true}}, PortRoles: []PortRole{
		{DeviceID: 1, Port: "lan2", Mode: PortRoleAccess, NetworkID: 2},
		{DeviceID: 1, Port: "lan2", Mode: PortRoleTrunk},
		{DeviceID: 1, Port: "lan3", Mode: PortRoleAccess, NetworkID: 9},
	}}
	errs := site.ValidatePortRoles()
	if len(errs) != 2 {
		t.Fatalf("errors = %v, want the duplicate and the unknown network", errs)
	}
}

func TestMissingPortRoleIsNotARow(t *testing.T) {
	site := Site{PortRoles: []PortRole{{DeviceID: 1, Port: "lan2", Mode: PortRoleAccess, NetworkID: 2}}}
	if _, ok := site.PortRoleFor(1, "lan1"); ok {
		t.Fatal("an unsaved port must not grow a role of its own")
	}
	role, ok := site.PortRoleFor(1, "lan2")
	if !ok || role.Mode != PortRoleAccess || role.NetworkID != 2 {
		t.Fatalf("role = %+v ok=%v", role, ok)
	}
}
