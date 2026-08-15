package model

import "testing"

// The defect this type exists to prevent.
//
// Role was free text, stored verbatim and compared with `dev.Role != "gateway"`.
// "Gateway" is not "gateway", so the obvious capitalisation adopted a router as
// an access point: no address, no DHCP server, no firewall zone, no forwarding.
// A VLAN with nothing on it, and nothing anywhere saying why.
func TestRoleParsingIsForgivingAboutFormAndStrictAboutMeaning(t *testing.T) {
	for _, in := range []string{"gateway", "Gateway", " GATEWAY ", "GateWay"} {
		got, err := ParseRole(in)
		if err != nil {
			t.Errorf("ParseRole(%q): %v", in, err)
			continue
		}
		if got != RoleGateway || !got.Routes() {
			t.Errorf("ParseRole(%q) = %q; a capitalised gateway is still a gateway",
				in, got)
		}
	}
}

// A typo must be refused, not quietly demoted. Silently rendering an access
// point is the failure that is impossible to diagnose from the outcome.
func TestAnUnknownRoleIsRefusedWithTheValidOnes(t *testing.T) {
	for _, in := range []string{"gatway", "router", "mesh", "bridge"} {
		_, err := ParseRole(in)
		if err == nil {
			t.Errorf("ParseRole(%q) was accepted", in)
			continue
		}
		// The message has to name the alternatives: an operator who typed
		// "router" needs to know the word is "gateway", and a bare rejection
		// makes them guess.
		for _, want := range []string{"gateway", "ap", "switch"} {
			if !contains(err.Error(), want) {
				t.Errorf("ParseRole(%q) error %q does not mention %q", in, err, want)
			}
		}
	}
}

// Empty means access point: the role that changes least about a device.
// Guessing "gateway" would hand an unlabelled device addressing and firewall
// rules nobody asked for.
func TestAnUnsetRoleIsAnAccessPoint(t *testing.T) {
	got, err := ParseRole("")
	if err != nil || got != RoleAP {
		t.Fatalf("ParseRole(\"\") = %q, %v; want an access point", got, err)
	}
	// And the zero value of the type has to agree, or the same input means
	// different things depending on whether it came through the API or a
	// struct literal.
	var zero Role
	if !zero.Wireless() || zero.Routes() {
		t.Errorf("the zero Role behaves as %q; it must behave as an access point",
			zero)
	}
	if zero.String() != string(RoleAP) {
		t.Errorf("the zero Role renders as %q, want %q", zero, RoleAP)
	}
}

// A row already in the database cannot be rejected — refusing to load a device
// because its role string is unrecognised makes a bad write unrecoverable
// without hand-editing SQLite.
func TestAStoredRoleIsNormalisedRatherThanRejected(t *testing.T) {
	if got := RoleOf("nonsense"); got != RoleAP {
		t.Errorf("RoleOf(nonsense) = %q, want the access-point fallback", got)
	}
	if got := RoleOf("SWITCH"); got != RoleSwitch {
		t.Errorf("RoleOf(SWITCH) = %q, want %q", got, RoleSwitch)
	}
}

// Each role gets exactly the capabilities it is named for.
func TestWhatEachRoleLicenses(t *testing.T) {
	for _, tc := range []struct {
		role     Role
		wireless bool
		routes   bool
	}{
		{RoleGateway, true, true},
		{RoleAP, true, false},
		// The one that used to do nothing at all: a device adopted as a switch
		// was an access point in every respect that mattered.
		{RoleSwitch, false, false},
	} {
		if got := tc.role.Wireless(); got != tc.wireless {
			t.Errorf("%s.Wireless() = %v, want %v", tc.role, got, tc.wireless)
		}
		if got := tc.role.Routes(); got != tc.routes {
			t.Errorf("%s.Routes() = %v, want %v", tc.role, got, tc.routes)
		}
		if tc.role.Describe() == "" {
			t.Errorf("%s has no description; the picker would offer a bare word",
				tc.role)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
