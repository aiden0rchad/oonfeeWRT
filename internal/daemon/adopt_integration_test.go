//go:build integration

package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
)

// Adoption against a real device. This one WRITES — it installs the ACL file
// and creates an rpcd login — so it has its own opt-in on top of the
// integration tag:
//
//	OONFEE_TEST_ADOPT=1 OONFEE_TEST_HOST=192.168.1.1 \
//	OONFEE_TEST_ADMIN_USER=root OONFEE_TEST_ADMIN_PASS=... \
//	go test -tags=integration ./internal/daemon/ -run TestIntegrationAdopt -v
//
// It prints the credential it created, because re-adopting rotates it and the
// other integration tests need the current one.
func TestIntegrationAdoptARealDevice(t *testing.T) {
	if os.Getenv("OONFEE_TEST_ADOPT") != "1" {
		t.Skip("set OONFEE_TEST_ADOPT=1 to run the test that writes to a device")
	}
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_ADMIN_USER")
	pass := os.Getenv("OONFEE_TEST_ADMIN_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_ADMIN_USER")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if err := d.StartCollector(ctx, collector.Options{
		Baseline: time.Second, Log: quietLogger(),
	}); err != nil {
		t.Fatalf("StartCollector: %v", err)
	}

	res, err := d.Adopt(ctx, api.AdoptRequest{
		Host: host, Username: user, Password: pass, Name: "wrt3200acm", Role: "gateway",
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	t.Logf("adopted %s (%s) as device %d", res.Name, res.MAC, res.DeviceID)
	t.Logf("  model=%q class=%s firmware=%q", res.Model, res.Class, res.Firmware)
	t.Logf("  features: %v", res.Features)
	if len(res.Unobservable) > 0 {
		t.Logf("  could not determine: %v", res.Unobservable)
	}
	for _, q := range res.Quirks {
		t.Logf("  quirk: %s", q)
	}
	for _, w := range res.Warnings {
		t.Logf("  WARNING: %s", w)
	}

	if res.MAC == "" || res.Model == "" {
		t.Errorf("adoption returned an incomplete identity: %+v", res)
	}
	if res.Class == "" {
		t.Error("the device was not classified")
	}
	if len(res.Features) == 0 {
		t.Error("no capabilities were recorded")
	}

	// The credential must be sealed and usable — an adoption that reports
	// success without a working login is how a device joins the inventory
	// unreachable.
	dev, err := d.Store.DeviceByMAC(ctx, res.MAC)
	if err != nil {
		t.Fatalf("the adopted device is not in the inventory: %v", err)
	}
	if !dev.Adopted() || len(dev.CredEnc) == 0 {
		t.Fatalf("device row is not marked adopted or carries no credential: %+v", dev)
	}
	username, password, err := d.Keys.OpenCredential(dev.MAC, dev.CredEnc)
	if err != nil {
		t.Fatalf("the sealed credential will not open: %v", err)
	}
	c, err := d.Connect(ctx, dev)
	if err != nil {
		t.Fatalf("the credential adoption created does not work: %v", err)
	}
	defer c.Close()
	var board struct {
		Release struct {
			Description string `json:"description"`
		} `json:"release"`
	}
	if err := c.Call(ctx, "system", "board", nil, &board); err != nil {
		t.Fatalf("system.board on the new credential: %v", err)
	}
	t.Logf("the new controller login works: %s", board.Release.Description)

	// Re-adopting must be refused rather than silently rotating the credential
	// out from under a working install.
	if _, err := d.Adopt(ctx, api.AdoptRequest{
		Host: host, Username: user, Password: pass,
	}); err == nil {
		t.Error("adopting an already-adopted device succeeded")
	} else if !strings.Contains(err.Error(), "already adopted") {
		t.Errorf("unhelpful error for a re-adopt: %v", err)
	}

	// Printed last so it is easy to find. This is a lab device.
	t.Logf("CREDENTIAL %s / %s", username, password)
}
