//go:build integration

package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// TestSeedLiveInventory is a helper, not an assertion: it points a real data
// directory at the real device so the UI can be exercised by hand.
//
//	OONFEE_SEED_DIR=/path OONFEE_SEED_PASSFILE=/path/pass \
//	OONFEE_TEST_HOST=... OONFEE_TEST_USER=... OONFEE_TEST_PASS=... \
//	go test -tags=integration ./internal/daemon/ -run TestSeedLiveInventory
func TestSeedLiveInventory(t *testing.T) {
	dir := os.Getenv("OONFEE_SEED_DIR")
	if dir == "" {
		t.Skip("set OONFEE_SEED_DIR to seed a live data directory")
	}
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.Listen = "127.0.0.1:0"
	cfg.PassphraseFile = os.Getenv("OONFEE_SEED_PASSFILE")

	ctx := context.Background()
	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// Asked, not asserted. A literal here was wrong — it was the box's WAN-side
	// address while adoption identifies a device by its LAN bridge — so a
	// seeded row and a real adoption of the same box became two devices in the
	// inventory, and one physical AP got polled twice.
	mac, err := macOf(ctx, os.Getenv("OONFEE_TEST_HOST"), os.Getenv("OONFEE_TEST_PASS"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := d.Keys.SealCredential(mac,
		os.Getenv("OONFEE_TEST_USER"), os.Getenv("OONFEE_TEST_PASS"))
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	dev := &store.Device{MAC: mac, Host: os.Getenv("OONFEE_TEST_HOST"),
		Name: "wrt3200acm", Scheme: "http", Role: "gateway",
		AdoptedAt: &at, CredEnc: blob, Class: "A"}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded device %d at %s", dev.ID, dev.Host)
}
