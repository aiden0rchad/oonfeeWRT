//go:build integration

package daemon

import (
	"context"
	"os"
	"testing"
)

// The on-air check against real hardware. This is the one verification in the
// project that does not trust the management plane: each radio scans, and what
// it hears is compared with what the other devices claim to broadcast.
func TestIntegrationOnAir(t *testing.T) {
	if os.Getenv("OONFEE_ONAIR") != "1" {
		t.Skip("set OONFEE_ONAIR=1")
	}
	ctx := context.Background()
	cfg := testConfig(t, "operator passphrase")
	cfg.DataDir = os.Getenv("OONFEE_SEED_DIR")
	cfg.PassphraseFile = os.Getenv("OONFEE_SEED_PASSFILE")
	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.VerifyOnAir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, dev := range res.Devices {
		t.Logf("device %s: scanned=%v heard=%d errs=%v %s",
			dev.Name, dev.Scanned, dev.Heard, dev.ScanErrors, dev.Error)
	}
	for _, r := range res.Results {
		t.Logf("%-10s %-18s %-14s %-12s heard=%q witnesses=%v",
			r.Iface, r.BSSID, r.SSID, r.Verdict, r.HeardSSID, r.Witnesses)
		if r.Reason == "" {
			t.Errorf("%s has no reason", r.Verdict)
		}
	}
	t.Logf("faults: %d", res.Faults)
	if len(res.Results) == 0 {
		t.Fatalf("nothing to verify: %s", res.Note)
	}
	// At least one BSS must be genuinely confirmed, or this check proved
	// nothing at all and would be a green light with no evidence behind it.
	var confirmed int
	for _, r := range res.Results {
		if r.Verdict == "confirmed" {
			confirmed++
		}
	}
	if confirmed == 0 {
		t.Errorf("no BSS was confirmed on the air by another radio — the check " +
			"ran and established nothing")
	}
}
