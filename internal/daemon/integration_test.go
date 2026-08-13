//go:build integration

package daemon

import (
	"context"
	"os"
	"testing"

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
