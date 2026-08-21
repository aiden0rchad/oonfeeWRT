package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestDeviceViewReturnsFunctionsAndCanonicalLegacyRole(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		name      string
		device    store.Device
		wantRole  string
		wantFuncs []string
	}{
		{
			"new gateway only",
			store.Device{Role: "gateway", Functions: []string{"gateway"}},
			"gateway", []string{"gateway"},
		},
		{
			"legacy gateway",
			store.Device{Role: "gateway"},
			"gateway", []string{"gateway", "ap", "switch"},
		},
		{
			"new AP and switch",
			store.Device{Role: "ap", Functions: []string{"switch", "ap"}},
			"ap", []string{"ap", "switch"},
		},
		{
			"corrupt functions fail closed without relabeling",
			store.Device{Role: "gateway", Functions: []string{}, FunctionError: "invalid stored functions"},
			"gateway", []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.viewDevice(&tc.device, time.Unix(0, 0))
			if got.Role != tc.wantRole || !reflect.DeepEqual(got.Functions, tc.wantFuncs) {
				t.Fatalf("role=%q functions=%v, want %q %v",
					got.Role, got.Functions, tc.wantRole, tc.wantFuncs)
			}
			if tc.device.FunctionError != "" && got.FunctionError != tc.device.FunctionError {
				t.Fatalf("function error=%q, want %q", got.FunctionError, tc.device.FunctionError)
			}
		})
	}
}
