package model

import "testing"

func TestAllowsRouterAccessTreatsOmissionAsClosed(t *testing.T) {
	if (Network{}).AllowsRouterAccess() {
		t.Fatal("a missing switch is open")
	}
	off := false
	if (Network{AllowRouterAccess: &off}).AllowsRouterAccess() {
		t.Fatal("an explicit false switch is open")
	}
	on := true
	if !(Network{AllowRouterAccess: &on}).AllowsRouterAccess() {
		t.Fatal("an explicit true switch is closed")
	}
}
