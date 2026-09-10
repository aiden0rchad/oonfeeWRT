package model

import "testing"

func TestParseManagementMode(t *testing.T) {
	for _, raw := range []string{"", "managed", " MANAGED "} {
		mode, err := ParseManagementMode(raw)
		if err != nil || mode != ManagementModeManaged || !mode.Configures() {
			t.Errorf("ParseManagementMode(%q)=(%q,%v), want managed", raw, mode, err)
		}
	}
	for _, raw := range []string{"monitor_only", " Monitor_Only "} {
		mode, err := ParseManagementMode(raw)
		if err != nil || mode != ManagementModeMonitorOnly || mode.Configures() {
			t.Errorf("ParseManagementMode(%q)=(%q,%v), want monitor_only", raw, mode, err)
		}
	}
	if _, err := ParseManagementMode("multi_site"); err == nil {
		t.Fatal("unknown management mode was accepted")
	}
}
