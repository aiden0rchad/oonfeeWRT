package adoption

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestVerifyACLHashRejectsMissingAndWrongDigests(t *testing.T) {
	content := []byte(`{"oonfeewrt": true}`)
	for _, output := range []string{"", " \n\t", strings.Repeat("0", sha256.Size*2) + "  acl.json\n"} {
		if err := verifyACLHash(content, output); err == nil {
			t.Fatalf("verifyACLHash accepted output %q", output)
		}
	}

	sum := sha256.Sum256(content)
	if err := verifyACLHash(content, hex.EncodeToString(sum[:])+"  acl.json\n"); err != nil {
		t.Fatalf("verifyACLHash rejected the expected digest: %v", err)
	}
}

func TestSSHRunErrorDoesNotExposeCommandOrVerifier(t *testing.T) {
	const verifier = "$6$rounds=5000$generated-salt$generated-verifier"
	cmd := "uci set rpcd.oonfeewrt.password='" + verifier + "' && uci commit rpcd"
	err := sshRunError(
		"create controller login",
		errors.New("exit status 1: "+verifier),
		"",
		"+ "+cmd+"\nremote repeated "+verifier,
		cmd,
		[]string{verifier},
	)
	got := err.Error()
	if !strings.Contains(got, "create controller login") {
		t.Fatalf("error lost its safe operation label: %q", got)
	}
	for name, secret := range map[string]string{"command": cmd, "verifier": verifier} {
		if strings.Contains(got, secret) {
			t.Errorf("error exposed the %s: %q", name, got)
		}
	}
	if !strings.Contains(got, "remote output withheld") {
		t.Errorf("sensitive command did not suppress remote output: %q", got)
	}
}

func TestSSHRunErrorRedactsAnEchoedCommand(t *testing.T) {
	const cmd = "sha256sum '/usr/share/rpcd/acl.d/oonfeewrt.json'"
	err := sshRunError("verify temporary ACL", errors.New("exit status 127"), "", "+ "+cmd, cmd, nil)
	if strings.Contains(err.Error(), cmd) {
		t.Fatalf("error exposed the remote command: %q", err)
	}
	if !strings.Contains(err.Error(), "verify temporary ACL") {
		t.Fatalf("error lost its safe operation label: %q", err)
	}
}

func TestBoundedBufferDrainsWithoutGrowingPastLimit(t *testing.T) {
	var buf boundedBuffer
	payload := []byte(strings.Repeat("x", maxSSHOutput+1024))
	n, err := buf.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write() = %d, %v; want %d, nil", n, err, len(payload))
	}
	if !buf.truncated || len(buf.String()) != maxSSHOutput {
		t.Fatalf("truncated=%v retained=%d; want true, %d", buf.truncated, len(buf.String()), maxSSHOutput)
	}
	if n, err := buf.Write([]byte("still drained")); err != nil || n != len("still drained") || len(buf.String()) != maxSSHOutput {
		t.Fatalf("post-limit Write() = %d, %v retained=%d", n, err, len(buf.String()))
	}
}

func TestParsePackageStateKeepsOnlyBoundedFacts(t *testing.T) {
	state, err := parsePackageState("manager=apk\npackage=lldpd\npackage=libcap\npackage=lldpd\n" +
		"package=bad value\nlldp_enabled=1\nlldp_running=0\nignored=secret\n")
	if err != nil {
		t.Fatal(err)
	}
	if state.Manager != "apk" || len(state.Installed) != 2 ||
		state.Installed[0] != "lldpd" || state.Installed[1] != "libcap" ||
		!state.LLDPEnabled || state.LLDPRunning {
		t.Fatalf("state=%+v", state)
	}
	if _, err := parsePackageState("manager=none\n"); err == nil {
		t.Fatal("router without a supported package manager was accepted")
	}
}

func TestLLDPPackageActionsRejectAnUnrecognizedManagerBeforeSSH(t *testing.T) {
	boot := &SSHBootstrap{}
	if _, err := boot.LLDPPlan(context.Background(), "sh", nil); err == nil {
		t.Fatal("plan accepted an arbitrary manager")
	}
	if err := boot.InstallLLDP(context.Background(), "sh"); err == nil {
		t.Fatal("install accepted an arbitrary manager")
	}
	if err := boot.RemoveLLDP(context.Background(), "sh", []string{"lldpd"}, false, false); err == nil {
		t.Fatal("removal accepted an arbitrary manager")
	}
	if _, err := boot.LLDPPlan(context.Background(), "apk", []string{"lldpd;reboot"}); err == nil {
		t.Fatal("plan accepted an unsafe package name")
	}
	if err := boot.RemoveLLDP(context.Background(), "apk", []string{"lldpd;reboot"}, false, false); err == nil {
		t.Fatal("removal accepted an unsafe package name")
	}
}

func TestAPKLLDPPlanRefreshesIndexesBeforeSimulation(t *testing.T) {
	if lldpAPKInstallPlanCommand != "{ apk update && apk --simulate add lldpd; } 2>&1" {
		t.Fatalf("plan command = %q", lldpAPKInstallPlanCommand)
	}
}

func TestLLDPInstallUsesTheIndexRefreshedByTheBoundPlan(t *testing.T) {
	for manager, commands := range map[string][2]string{
		"apk":  {lldpAPKInstallPlanCommand, lldpAPKInstallCommand},
		"opkg": {lldpOPKGInstallPlanCommand, lldpOPKGInstallCommand},
	} {
		t.Run(manager, func(t *testing.T) {
			if strings.Count(commands[0], " update") != 1 {
				t.Fatalf("plan must refresh exactly once: %q", commands[0])
			}
			if strings.Contains(commands[1], " update") || strings.Contains(commands[1], "-U") {
				t.Fatalf("install redundantly refreshed the package index: %q", commands[1])
			}
		})
	}
}

func TestLLDPDiagnosticsCommandIsReadOnlyAndBounded(t *testing.T) {
	for _, forbidden := range []string{" apk ", "uci set", "uci add", "uci delete", "uci commit", "/etc/init.d/"} {
		if strings.Contains(" "+lldpDiagnosticsCommand, forbidden) {
			t.Fatalf("diagnostic command contains mutating fragment %q", forbidden)
		}
	}
	for _, required := range []string{"uci -q show lldpd", "show interfaces", "show neighbors hidden"} {
		if !strings.Contains(lldpDiagnosticsCommand, required) {
			t.Fatalf("diagnostic command missing %q", required)
		}
	}
}

func TestLLDPInterfaceDiscoveryParsesOnlyBoundedNames(t *testing.T) {
	names, err := parseInterfaceNames("lan3\neth0.1\nlan3\n")
	if err != nil || strings.Join(names, ",") != "eth0.1,lan3" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	if _, err := parseInterfaceNames("lan3;reboot\n"); err == nil {
		t.Fatal("unsafe interface name accepted")
	}
	runtime, err := parseLLDPRuntimeInterfaces(`{"lldp":{"interface":{"lan3":{},"lan1":{}}}}`)
	if err != nil || strings.Join(runtime, ",") != "lan1,lan3" {
		t.Fatalf("runtime=%v err=%v", runtime, err)
	}
	for _, raw := range []string{
		`{"lldp":{"interface":[{"lan3":{}},{"lan1":{}}]}}`,
		`{"lldp":{"interface":[{"name":"lan3"},{"name":"lan1"}]}}`,
	} {
		runtime, err = parseLLDPRuntimeInterfaces(raw)
		if err != nil || strings.Join(runtime, ",") != "lan1,lan3" {
			t.Fatalf("runtime=%v err=%v for %s", runtime, err, raw)
		}
	}
	if _, err := parseLLDPRuntimeInterfaces(`{"lldp":`); err == nil {
		t.Fatal("malformed runtime JSON accepted")
	}
	if _, err := parseLLDPRuntimeInterfaces(`{"lldp":{"interface":[{"lan1":{},"lan2":{}}]}}`); err == nil {
		t.Fatal("ambiguous runtime interface accepted")
	}
}

func TestLLDPRestartWaitsForControlSocket(t *testing.T) {
	for _, required := range []string{"/etc/init.d/lldpd restart", "/var/run/lldpd.socket", "sleep 1", "exit 24"} {
		if !strings.Contains(lldpRestartReadyCommand, required) {
			t.Fatalf("restart readiness command missing %q", required)
		}
	}
}

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
