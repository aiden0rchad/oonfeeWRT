package main

import (
	"strings"
	"testing"
)

func TestOptionDiffRedactsCurrentAndDesiredCredentials(t *testing.T) {
	const current = "current-secret-sentinel-2cN7"
	const desired = "desired-secret-sentinel-9pL3"
	for name, got := range map[string]string{
		"changed": formatOptionChange("key", current, desired),
		"deleted": formatOptionDelete("key", current),
	} {
		if strings.Contains(got, current) || strings.Contains(got, desired) {
			t.Errorf("%s diff leaked a credential: %q", name, got)
		}
		if !strings.Contains(got, "key") || !strings.Contains(got, "redacted") {
			t.Errorf("%s diff is not actionable: %q", name, got)
		}
	}

	if got := formatOptionChange("encryption", "psk2", "sae"); got != `encryption: "psk2" -> "sae"` {
		t.Errorf("non-secret diff = %q", got)
	}
}
