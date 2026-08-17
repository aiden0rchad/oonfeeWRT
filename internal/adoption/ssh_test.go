package adoption

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
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
