package model

import (
	"reflect"
	"testing"
)

func TestLegacyRolesExpandWithoutChangingBehaviour(t *testing.T) {
	for _, tc := range []struct {
		role Role
		want DeviceFunctions
	}{
		{RoleGateway, DeviceFunctions{FunctionGateway, FunctionAP, FunctionSwitch}},
		{RoleAP, DeviceFunctions{FunctionAP, FunctionSwitch}},
		{RoleSwitch, DeviceFunctions{FunctionSwitch}},
		{"", DeviceFunctions{FunctionAP, FunctionSwitch}},
	} {
		if got := FunctionsForRole(tc.role); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("FunctionsForRole(%q)=%v, want %v", tc.role, got, tc.want)
		}
	}
}

func TestFunctionsAreCanonicalAndChooseAStablePrimaryRole(t *testing.T) {
	got, err := ParseDeviceFunctions([]string{" switch ", "AP", "gateway", "ap"}, RoleSwitch)
	if err != nil {
		t.Fatal(err)
	}
	want := DeviceFunctions{FunctionGateway, FunctionAP, FunctionSwitch}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("functions=%v, want canonical %v", got, want)
	}
	if got.PrimaryRole() != RoleGateway {
		t.Fatalf("primary role=%q, want gateway", got.PrimaryRole())
	}

	apSwitch, _ := ParseDeviceFunctions([]string{"switch", "ap"}, RoleGateway)
	if apSwitch.PrimaryRole() != RoleAP || apSwitch.Routes() || !apSwitch.Wireless() || !apSwitch.Switches() {
		t.Fatalf("independent AP+switch functions behaved like legacy role: %v", apSwitch)
	}

	gatewayOnly, _ := ParseDeviceFunctions([]string{"gateway"}, RoleAP)
	if !gatewayOnly.Routes() || gatewayOnly.Wireless() || gatewayOnly.Switches() {
		t.Fatalf("gateway-only selection widened itself: %v", gatewayOnly)
	}
}

func TestFunctionsRejectEmptyAndUnknownSelections(t *testing.T) {
	for _, in := range [][]string{{}, {"router"}, {"ap", "mesh"}} {
		if _, err := ParseDeviceFunctions(in, RoleAP); err == nil {
			t.Errorf("ParseDeviceFunctions(%v) accepted invalid selection", in)
		}
	}
}

func TestStoredBadFunctionsFailClosedInsteadOfWideningLegacyRole(t *testing.T) {
	got := DeviceFunctionsOf([]string{"gateway", "made-up"}, "switch")
	if got == nil || len(got) != 0 {
		t.Fatalf("corrupt stored functions=%v, want explicit empty invalid set", got)
	}
	got = DeviceFunctionsOf([]string{}, "gateway")
	if got == nil || len(got) != 0 {
		t.Fatalf("stored [] functions=%v, want explicit empty invalid set", got)
	}
	legacy := DeviceFunctionsOf(nil, "gateway")
	if !reflect.DeepEqual(legacy, FunctionsForRole(RoleGateway)) {
		t.Fatalf("nil legacy functions=%v, want legacy bundle", legacy)
	}
}

func TestModelDeviceUsesFunctionsWhenPresentAndRoleWhenAbsent(t *testing.T) {
	legacy := Device{Role: RoleGateway}
	if !legacy.EffectiveFunctions().Wireless() || !legacy.EffectiveFunctions().Routes() {
		t.Fatal("legacy gateway lost its historical AP or routing behaviour")
	}

	newGateway := Device{Role: RoleGateway, Functions: DeviceFunctions{FunctionGateway}}
	if newGateway.EffectiveFunctions().Wireless() || !newGateway.EffectiveFunctions().Routes() {
		t.Fatal("new gateway-only device inherited wireless from primary role")
	}
}
