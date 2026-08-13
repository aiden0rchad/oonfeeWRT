package adoption

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

var mockAddr string

func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mockAddr = fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("python3", filepath.Join(root, "tools", "mock_ubus.py"),
		"--port", fmt.Sprint(port))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := waitReady(mockAddr, 10*time.Second); err != nil {
		_ = cmd.Process.Kill()
		fmt.Fprintln(os.Stderr, "mock not ready:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	os.Exit(code)
}

func repoRoot() (string, error) {
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("go.mod not found")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitReady(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timeout")
}

func operatorClient(t *testing.T) *ubus.Client {
	t.Helper()
	c := ubus.New(ubus.Options{Host: mockAddr})
	if err := c.Login(context.Background(), "root", "good"); err != nil {
		t.Fatalf("operator login: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func testAdopter() *Adopter {
	return &Adopter{ACL: []byte(`{"oonfeewrt":{"description":"test"}}`)}
}

// written asks the mock what adoption actually put on the device.
func written(t *testing.T, c *ubus.Client, path string) (paths []string, content string) {
	t.Helper()
	var out struct {
		Paths   []string `json:"paths"`
		Content string   `json:"content"`
	}
	if err := c.Call(context.Background(), "__test", "written",
		map[string]any{"path": path}, &out); err != nil {
		t.Fatalf("__test.written: %v", err)
	}
	return out.Paths, out.Content
}

func TestAdoptInstallsExactlyOneFileAndOneLogin(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)

	res, err := testAdopter().Adopt(ctx, op)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if res.Credential.Username != DefaultUser {
		t.Errorf("username = %q, want %q", res.Credential.Username, DefaultUser)
	}
	if len(res.Credential.Password) < 20 {
		t.Errorf("generated password looks too short: %q", res.Credential.Password)
	}
	if res.Caps == nil {
		t.Error("adoption should carry the capability snapshot it probed")
	}

	// The entire device-side footprint: one file.
	paths, content := written(t, op, DefaultACLPath)
	if len(paths) != 1 || paths[0] != DefaultACLPath {
		t.Fatalf("footprint should be exactly one file at %s, got %v", DefaultACLPath, paths)
	}
	if !strings.Contains(content, `"oonfeewrt"`) {
		t.Errorf("ACL content did not survive the write: %q", content)
	}
}

// A password that could be mistaken for a crypt prefix or a field separator is
// one more way to write a broken /etc/config/rpcd.
func TestGeneratedPasswordAvoidsConfigMetacharacters(t *testing.T) {
	for i := 0; i < 64; i++ {
		p, err := randomPassword()
		if err != nil {
			t.Fatalf("randomPassword: %v", err)
		}
		if strings.ContainsAny(p, "$:'\" \n") {
			t.Fatalf("password %q contains a character that is unsafe in uci config", p)
		}
	}
}

// Adoption must not claim success without proving the credential it created
// works — otherwise a device joins the inventory unreachable.
func TestAdoptVerifiesTheCredentialItCreated(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)

	// Inject the fault: the login will be written but will not work, standing
	// in for a botched hash or a config that did not land.
	if err := op.Call(ctx, "__test", "reject_login",
		map[string]any{"usernames": []string{DefaultUser}}, nil); err != nil {
		t.Skipf("mock does not support login fault injection: %v", err)
	}
	t.Cleanup(func() {
		_ = op.Call(ctx, "__test", "reject_login",
			map[string]any{"usernames": []string{}}, nil)
	})

	a := testAdopter()
	if _, err := a.Adopt(ctx, op); err == nil {
		t.Fatal("Adopt should fail when the new credential cannot log in")
	} else if !strings.Contains(err.Error(), "does not work") {
		t.Fatalf("error should name the verification failure, got: %v", err)
	}
}

func TestAdoptRefusesWithoutACLContent(t *testing.T) {
	op := operatorClient(t)
	a := &Adopter{}
	if _, err := a.Adopt(context.Background(), op); err == nil {
		t.Fatal("adopting with no ACL content must fail")
	}
}

// The rule the design turns on: the controller cannot remove itself, so
// un-adopt without the operator credential must stop and say so rather than
// half-finish silently.
func TestUnadoptWithoutOperatorStopsAndReportsResidue(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)
	a := testAdopter()
	if _, err := a.Adopt(ctx, op); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	ctrl := ubus.New(ubus.Options{Host: mockAddr})
	if err := ctrl.Login(ctx, "root", "good"); err != nil {
		t.Fatalf("controller login: %v", err)
	}
	defer ctrl.Close()

	owned := []Section{{Config: "wireless", Section: "default_radio0"}}
	rep, err := a.Unadopt(ctx, ctrl, nil, owned)
	if !errors.Is(err, ErrOperatorRequired) {
		t.Fatalf("want ErrOperatorRequired, got %v", err)
	}
	if len(rep.Reverted) != 1 {
		t.Errorf("phase 1 should still revert owned sections, got %v", rep.Reverted)
	}
	if !rep.FootprintRemains {
		t.Error("the footprint must be reported as remaining")
	}
	residue := rep.Residue()
	if len(residue) != 2 {
		t.Fatalf("residue should name both the ACL file and the login, got %v", residue)
	}
	// The fallback screen shows these, so they must be the real paths.
	if !strings.Contains(residue[0], DefaultACLPath) {
		t.Errorf("residue should name the ACL path, got %q", residue[0])
	}
	if !strings.Contains(residue[1], "/etc/config/rpcd") {
		t.Errorf("residue should name the rpcd login, got %q", residue[1])
	}
}

func TestUnadoptWithOperatorRemovesTheFootprint(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)
	a := testAdopter()
	if _, err := a.Adopt(ctx, op); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if paths, _ := written(t, op, DefaultACLPath); len(paths) == 0 {
		t.Fatal("precondition: the ACL should be on the device")
	}

	rep, err := a.Unadopt(ctx, op, op, []Section{
		{Config: "wireless", Section: "default_radio0"}})
	if err != nil {
		t.Fatalf("Unadopt: %v (%v)", err, rep.Errors)
	}
	if !rep.ACLRemoved || !rep.LoginRemoved {
		t.Fatalf("both the ACL and the login should be gone: %+v", rep)
	}
	if rep.FootprintRemains {
		t.Error("a complete un-adopt leaves no footprint")
	}
	if len(rep.Residue()) != 0 {
		t.Errorf("residue should be empty, got %v", rep.Residue())
	}
	if paths, _ := written(t, op, DefaultACLPath); len(paths) != 0 {
		t.Errorf("ACL file should be removed from the device, still have %v", paths)
	}
}

// Re-running un-adopt must be safe: "already gone" is success, not an error.
func TestUnadoptIsIdempotent(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)
	a := testAdopter()
	if _, err := a.Adopt(ctx, op); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if _, err := a.Unadopt(ctx, op, op, nil); err != nil {
		t.Fatalf("first Unadopt: %v", err)
	}
	rep, err := a.Unadopt(ctx, op, op, nil)
	if err != nil {
		t.Fatalf("second Unadopt should be a no-op, got %v (%v)", err, rep.Errors)
	}
	if rep.FootprintRemains {
		t.Error("nothing left to remove, so no footprint should be reported")
	}
}
