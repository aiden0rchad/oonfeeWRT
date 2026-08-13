//go:build integration

package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// Integration test for the one path that mock coverage cannot prove: a
// credential sealed into the database, unsealed at connect time, and accepted by
// a real device's rpcd.
//
//	OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt \
//	OONFEE_TEST_PASS=... go test -tags=integration ./internal/daemon/ -v
//
// Read-only against the device: it logs in and reads board info.

func TestIntegrationSealedCredentialOpensARealSession(t *testing.T) {
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	ctx := context.Background()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const mac = "aa:bb:cc:dd:ee:ff"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatalf("SealCredential: %v", err)
	}
	adopted := int64(1)
	dev := &store.Device{
		MAC: mac, Host: host, Name: "integration", Scheme: "http",
		AdoptedAt: &adopted, CredEnc: blob,
	}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}

	// Reload from the database rather than reusing the in-memory struct: the
	// point of the test is that the blob survives a round trip through SQLite,
	// which is where a BLOB column could quietly become something else.
	loaded, err := d.Store.DeviceByMAC(ctx, mac)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	c, err := d.Connect(ctx, loaded)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	var board struct {
		Release struct {
			Description string `json:"description"`
		} `json:"release"`
	}
	if err := c.Call(ctx, "system", "board", nil, &board); err != nil {
		t.Fatalf("system.board over the reconstituted session: %v", err)
	}
	if board.Release.Description == "" {
		t.Fatal("system.board returned no release description")
	}
	t.Logf("connected with a sealed credential: %s", board.Release.Description)

	// A credential recorded against one device must not open another's session,
	// even with the same keyring.
	other := *loaded
	other.MAC = "11:22:33:44:55:66"
	if _, err := d.Connect(ctx, &other); err == nil {
		t.Fatal("the sealed credential opened under a different device MAC")
	}
}

// The whole Phase 0 path, end to end against real hardware: a credential sealed
// into the keyring, an adopted device in the inventory, the collector polling
// it, and the sink recording what it learned.
func TestIntegrationCollectorPollsARealDevice(t *testing.T) {
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const mac = "60:38:e0:00:00:01"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatalf("SealCredential: %v", err)
	}
	at := int64(1)
	dev := &store.Device{MAC: mac, Host: host, Name: "wrt3200acm", Scheme: "http",
		AdoptedAt: &at, CredEnc: blob}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}

	if err := d.StartCollector(ctx, collector.Options{
		Baseline: 500 * time.Millisecond, Focused: 200 * time.Millisecond,
		Log: quietLogger(),
	}); err != nil {
		t.Fatalf("StartCollector: %v", err)
	}

	// last_seen moving is the observable proof that a poll completed, went
	// through the sink, and reached the database.
	var seen int64
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err := d.Store.DeviceByMAC(ctx, mac)
		if err != nil {
			t.Fatalf("DeviceByMAC: %v", err)
		}
		if got.LastSeen != nil && *got.LastSeen > 0 {
			seen = *got.LastSeen
			if got.PollState != string(collector.Baseline) {
				t.Errorf("poll_state = %q, want %q", got.PollState, collector.Baseline)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if seen == 0 {
		t.Fatal("the device was never marked as seen; no poll completed")
	}
	t.Logf("polled a real device through the full stack; last_seen=%d", seen)

	// No unreachable events: the device answered, so the sink must not have
	// recorded a failure alongside the success.
	events, err := d.Store.RecentEvents(ctx, 50)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	for _, e := range events {
		if e.Event == "device.unreachable" {
			t.Errorf("a reachable device logged %s: %+v", e.Event, e.Detail)
		}
	}

	// Focus must raise the tier and take effect promptly, not on the next
	// baseline interval.
	release := d.Focus(dev.ID)
	defer release()
	if tier, ok := d.collectorRef().Tier(dev.ID); !ok || tier != collector.Focused {
		t.Fatalf("tier after Focus = %q (known=%v), want %q", tier, ok, collector.Focused)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := d.Store.DeviceByMAC(ctx, mac)
		if err != nil {
			t.Fatalf("DeviceByMAC: %v", err)
		}
		if got.PollState == string(collector.Focused) {
			t.Log("focused polling reached the device and the database")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no focused poll was recorded after Focus")
}
