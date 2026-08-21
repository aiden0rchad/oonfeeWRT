package main

import (
	"strings"
	"testing"
)

func TestFormatStaleOptionRedactsCredentialsOnly(t *testing.T) {
	const secret = "live-stale-redacted-placeholder-8mR4"
	got := formatStaleOption("sae_password", secret)
	if strings.Contains(got, secret) || got != "sae_password=<redacted>" {
		t.Fatalf("sensitive stale option = %q", got)
	}
	if got := formatStaleOption("encryption", "sae"); got != "encryption=sae" {
		t.Fatalf("non-secret stale option = %q", got)
	}
}
