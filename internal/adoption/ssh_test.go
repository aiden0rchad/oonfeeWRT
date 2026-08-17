package adoption

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// A host that accepts TCP and never speaks SSH must not hang adoption.
//
// ClientConfig.Timeout is read only by ssh.Dial; NewClientConn hands the
// connection to clientHandshake unbounded, and it does not observe ctx either.
// So a stalled proxy, a port-forward to nothing, or any non-SSH service that
// accepts and never writes held DialSSH open forever — past the 90s adopt
// timeout, past the cancelled request, on a server with no WriteTimeout.
//
// This package's real SSH bootstrap had no tests at all before this one: the
// code that writes the ACL file, creates the controller's login and removes
// them again was exercised only through a fake.
func TestDialSSHDoesNotHangOnASilentHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept and say nothing, holding the connection open.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Deliberately never written to and never closed.
			_ = conn
		}
	}()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := DialSSH(context.Background(), SSHOptions{
			Host: ln.Addr().String(), Username: "root",
			Timeout: 2 * time.Second,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a host that never spoke SSH produced a working connection")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("took %s to give up; the handshake is not bounded", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("DialSSH never returned: the handshake has no deadline, and " +
			"neither the adopt timeout nor a cancelled request can interrupt it")
	}
}

// And the context's deadline is honoured when it is the tighter of the two.
func TestDialSSHHonoursAShorterContextDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = DialSSH(ctx, SSHOptions{
		Host: ln.Addr().String(), Username: "root",
		Timeout: 30 * time.Second, // deliberately much longer than the ctx
	})
	if err == nil {
		t.Fatal("expected a failure against a silent host")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s despite a 500ms context deadline", elapsed)
	}
	if strings.Contains(err.Error(), "cannot reach") {
		t.Log("failed at dial rather than handshake, which is also bounded")
	}
}

// sshServer starts an in-process SSH server with a freshly generated host key
// and accepts any password.
//
// A real server rather than a fake, because the thing under test is the host
// key callback, and that only runs inside a genuine handshake. Two servers
// started in one test have different keys, which is what lets a test stand in
// for "the box at this address is not the box you adopted".
func sshServer(t *testing.T) (addr string) {
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

// The pin, end to end: what adoption records is what un-adopt checks, and a
// different box at the same address is refused.
//
// This branch had never run. The refusal at ssh.go was written, reviewed and
// shipped while both call sites left HostKeyFP empty, there was no column to
// store it in, and nothing exercised it — so the guard could not fire on any
// device, ever. It matters at un-adopt rather than adoption: adoption is
// genuinely first use, but un-adopt dials the STORED address carrying the
// operator's freshly typed administrator password.
func TestDialSSHPinsTheHostKeyItRecorded(t *testing.T) {
	ctx := context.Background()
	addr := sshServer(t)

	// First use: unpinned, and it tells the caller what it saw.
	first, err := DialSSH(ctx, SSHOptions{
		Host: addr, Username: "root", Password: "x", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("first use should be trust-on-first-use: %v", err)
	}
	pin := first.Fingerprint()
	first.Close()
	if !strings.HasPrefix(pin, "SHA256:") {
		t.Fatalf("fingerprint %q is not in the form an operator can compare "+
			"against ssh-keygen -lf", pin)
	}

	// The same device, with that pin: accepted.
	again, err := DialSSH(ctx, SSHOptions{
		Host: addr, Username: "root", Password: "x", HostKeyFP: pin,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("the recorded pin rejected the device it was taken from: %v", err)
	}
	if again.Fingerprint() != pin {
		t.Errorf("fingerprint is not stable across dials: %q then %q",
			pin, again.Fingerprint())
	}
	again.Close()

	// A DIFFERENT box answering at that address, carrying the pin from the
	// first one: refused, before any credential is of use to it.
	other := sshServer(t)
	b, err := DialSSH(ctx, SSHOptions{
		Host: other, Username: "root", Password: "x", HostKeyFP: pin,
		Timeout: 5 * time.Second,
	})
	if err == nil {
		b.Close()
		t.Fatal("a different host key was accepted; the pin is decorative")
	}
	if !strings.Contains(err.Error(), "host key changed") {
		t.Errorf("the refusal does not say what happened, so an operator "+
			"cannot tell a reflash from an impersonation: %v", err)
	}
}
