package main

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestValidateApplyTargetFailsClosed(t *testing.T) {
	at := int64(1)
	base := store.Device{Name: "router", Role: "gateway", Functions: []string{"gateway"},
		ManagementMode: "managed", AdoptedAt: &at}
	for _, test := range []struct {
		name   string
		mutate func(*store.Device)
		want   string
	}{
		{name: "unadopted", mutate: func(d *store.Device) { d.AdoptedAt = nil }, want: "not adopted"},
		{name: "monitor only", mutate: func(d *store.Device) { d.ManagementMode = "monitor_only" }, want: "monitor-only"},
		{name: "invalid mode", mutate: func(d *store.Device) { d.ManagementModeError = "corrupt" }, want: "invalid management mode"},
		{name: "invalid functions", mutate: func(d *store.Device) { d.FunctionError = "corrupt" }, want: "invalid function state"},
	} {
		t.Run(test.name, func(t *testing.T) {
			device := base
			test.mutate(&device)
			if err := validateApplyTarget(&device); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error=%v, want %q", err, test.want)
			}
		})
	}
	if err := validateApplyTarget(&base); err != nil {
		t.Fatalf("valid managed target rejected: %v", err)
	}
}

func TestValidateApplyFleetRejectsCorruptRoutingClaimBesideTarget(t *testing.T) {
	at := int64(1)
	target := &store.Device{ID: 1, Name: "canonical", Role: "gateway",
		Functions: []string{"gateway"}, ManagementMode: "managed", AdoptedAt: &at}
	corrupt := &store.Device{ID: 2, Name: "noncanonical", Role: "gateway",
		Functions: []string{}, FunctionError: "stored device functions are invalid",
		ManagementMode: "managed", AdoptedAt: &at}
	if err := validateApplyFleet([]*store.Device{target, corrupt}, target); err == nil ||
		!strings.Contains(err.Error(), "invalid device metadata") {
		t.Fatalf("fleet validation error=%v", err)
	}
	if err := validateApplyFleet([]*store.Device{target}, target); err != nil {
		t.Fatalf("valid fleet rejected: %v", err)
	}
}
