package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// pinnedSSHServer starts an in-process SSH server that accepts any password,
// and returns its address.
//
// A real server, because the guard under test lives inside the handshake and
// no fake reaches it. It answers with a freshly generated key every time, so a
// device row seeded with any other fingerprint is exactly the situation the pin
// exists for: something at the stored address that is not what was adopted.
func pinnedSSHServer(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					conn.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				go func() {
					for ch := range chans {
						_ = ch.Reject(ssh.UnknownChannelType, "bootstrap only")
					}
				}()
				<-time.After(2 * time.Second)
				sc.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// seedPinned puts a device in the inventory at addr with a pin that addr's
// server cannot possibly match.
//
// adopted_at is deliberately left unset. The pin is checked in the SSH block,
// which does not consult it — only the phase-1 controller session does — and
// an adopted row would send an HTTP request to the SSH port and wait out the
// ubus timeout for a reply that never comes. Nothing about the branch under
// test differs.
func seedPinned(t *testing.T, ctx context.Context, d *Daemon, addr, pin string) *store.Device {
	t.Helper()
	dev := &store.Device{
		MAC: "aa:bb:cc:dd:ee:ff", Host: addr, Name: "ap1", HostKeyFP: pin,
	}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	got, err := d.Store.DeviceByID(ctx, dev.ID)
	if err != nil {
		t.Fatalf("DeviceByID: %v", err)
	}
	if got.HostKeyFP != pin {
		t.Fatalf("the pin did not reach the database, so this test cannot "+
			"prove anything about it: %q", got.HostKeyFP)
	}
	return dev
}

// Un-adopt dials the STORED address carrying the operator's freshly typed
// administrator password, so it is the one dial that must be pinned.
//
// The refusal in DialSSH was written, reviewed and shipped while both call
// sites left HostKeyFP empty and no column existed to hold it, which made the
// guard unreachable on every device. This is the wiring, not the guard: the
// value has to travel from the row to the dial or the check is decorative
// again.
func TestUnadoptRefusesADeviceWhoseHostKeyChanged(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "pass"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	addr := pinnedSSHServer(t)
	dev := seedPinned(t, ctx, d, addr, "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

	_, err = d.Unadopt(ctx, api.UnadoptRequest{
		DeviceID: dev.ID, Username: "root", Password: "not-a-real-password-7Kq2",
	})
	if err == nil {
		t.Fatal("un-adopt opened SSH to a host whose key does not match the " +
			"pin, and offered it the administrator password")
	}
	if !strings.Contains(err.Error(), "host key changed") {
		t.Fatalf("un-adopt failed for some other reason, so the pin may still "+
			"be unwired: %v", err)
	}

	// The row survives a refused dial. It is the only record of what is still
	// installed on that device, and deleting it because we could not reach the
	// device would lose the list of what needs removing by hand.
	if _, err := d.Store.DeviceByID(ctx, dev.ID); err != nil {
		t.Fatalf("the device was removed from the inventory despite the "+
			"footprint remaining on it: %v", err)
	}
}

// And the escape hatch, because the commonest reason a host key changes is a
// reflash — which also wipes the footprint un-adopt came to remove.
//
// Without this, adding the pin would make a reflashed device permanently
// un-removable from the inventory: every attempt fails at the dial, before
// Force is ever consulted. Force is documented as "remove it even if the
// device could not be reached at all", and a refused key is one way not to
// reach it.
func TestForcedUnadoptSurvivesARefusedHostKey(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "pass"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	addr := pinnedSSHServer(t)
	dev := seedPinned(t, ctx, d, addr, "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

	out, err := d.Unadopt(ctx, api.UnadoptRequest{
		DeviceID: dev.ID, Username: "root", Password: "not-a-real-password-7Kq2",
		Force: true,
	})
	if err != nil {
		t.Fatalf("Force could not remove a device whose host key is refused: %v", err)
	}
	if !out.Removed {
		t.Fatal("Force did not remove the inventory row")
	}
	if _, err := d.Store.DeviceByID(ctx, dev.ID); err == nil {
		t.Fatal("Unadopt reported the device removed but the row is still there")
	}

	// Honest about what it did NOT do. The row was the only record of the
	// login and ACL file left on that device, and it has just been deleted, so
	// this response is the last copy of that fact.
	joined := strings.Join(out.Errors, "\n")
	if !strings.Contains(joined, "host key changed") {
		t.Errorf("the forced removal did not report why SSH failed: %q", joined)
	}
	if !strings.Contains(joined, "NOT removed") {
		t.Errorf("the forced removal did not say the footprint is still on the "+
			"device: %q", joined)
	}
	if !out.FootprintRemains {
		t.Error("phase 2 never ran, so the footprint necessarily remains")
	}
}
