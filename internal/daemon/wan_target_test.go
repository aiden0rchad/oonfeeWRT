package daemon

import (
	"encoding/json"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestCollectorTargetSelectsOnlyTheManagedGatewayForWANProbes(t *testing.T) {
	d := &Daemon{}
	for _, tc := range []struct {
		name      string
		role      string
		functions []string
		want      bool
	}{
		{name: "gateway", role: "gateway", functions: []string{"gateway", "ap", "switch"}, want: true},
		{name: "gateway only", role: "gateway", functions: []string{"gateway"}, want: true},
		{name: "AP only", role: "ap", functions: []string{"ap", "switch"}},
		{name: "legacy AP", role: "ap", functions: nil},
		{name: "invalid empty set fails closed", role: "gateway", functions: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := d.target(&store.Device{ID: 1, MAC: "02:00:00:00:00:01", Role: tc.role, Functions: tc.functions})
			if got.Gateway != tc.want {
				t.Fatalf("Gateway = %v, want %v", got.Gateway, tc.want)
			}
		})
	}
}

func TestCollectorTargetAirtimeSplitRequiresStoredCapabilityProof(t *testing.T) {
	d := &Daemon{}
	for _, state := range []capability.State{
		capability.Unknown, capability.NotObservable, capability.Absent, capability.Present,
	} {
		t.Run(state.String(), func(t *testing.T) {
			caps := capability.NewRegistry()
			caps.Set(capability.FeatAirtimeSplit, state)
			blob, err := json.Marshal(caps)
			if err != nil {
				t.Fatal(err)
			}
			got := d.target(&store.Device{ID: 1, MAC: "02:00:00:00:00:01", CapsJSON: string(blob)})
			if got.AirtimeSplit != (state == capability.Present) {
				t.Fatalf("AirtimeSplit = %v for %s", got.AirtimeSplit, state)
			}
		})
	}
}
