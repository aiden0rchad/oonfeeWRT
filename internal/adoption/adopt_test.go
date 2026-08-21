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

// fakeBoot stands in for the SSH channel. It records the footprint, which makes
// these tests assert on what adoption INSTALLED rather than on what one
// transport happened to report.
type fakeBoot struct {
	// mirror writes the login into the mock's rpcd config, the way the real SSH
	// bootstrap writes it into the device's. Without it the verification step —
	// "log in with the credential we just created" — has nothing to verify
	// against, and the most important assertion in adoption would pass
	// vacuously.
	mirror *ubus.Client

	acl                  map[string][]byte
	login                string
	passHash             string
	groups               []string
	installed            bool
	failACL              error
	failLogin            error
	failRemoveACL        error
	failRemoveFootprint  error
	removeACLCalls       int
	removeFootprintCalls int
	closed               bool
}

func newBoot() *fakeBoot { return &fakeBoot{acl: map[string][]byte{}} }

// newBootFor mirrors into the mock so the created credential really works.
func newBootFor(c *ubus.Client) *fakeBoot {
	return &fakeBoot{acl: map[string][]byte{}, mirror: c}
}

func (b *fakeBoot) InstallACL(_ context.Context, path string, content []byte) error {
	if b.failACL != nil {
		return b.failACL
	}
	b.acl[path] = append([]byte(nil), content...)
	b.installed = true
	return nil
}

func (b *fakeBoot) CreateLogin(_ context.Context, user, passHash string, groups []string) error {
	if b.failLogin != nil {
		return b.failLogin
	}
	b.login, b.passHash, b.groups = user, passHash, groups
	if b.mirror != nil {
		if err := b.mirror.Call(context.Background(), "uci", "set", map[string]any{
			"config": "rpcd", "section": user, "type": "login",
			"values": map[string]any{
				"username": user, "password": passHash,
				"read": groups, "write": groups,
			},
		}, nil); err != nil {
			return err
		}
		return b.mirror.Call(context.Background(), "uci", "commit",
			map[string]any{"config": "rpcd"}, nil)
	}
	return nil
}

func (b *fakeBoot) RemoveACL(_ context.Context, aclPath string) error {
	b.removeACLCalls++
	if b.failRemoveACL != nil {
		return b.failRemoveACL
	}
	delete(b.acl, aclPath)
	return nil
}

func (b *fakeBoot) RemoveFootprint(ctx context.Context, aclPath, user string) error {
	b.removeFootprintCalls++
	if b.failRemoveFootprint != nil {
		return b.failRemoveFootprint
	}
	delete(b.acl, aclPath)
	if b.login == user {
		b.login, b.passHash, b.groups = "", "", nil
	}
	if b.mirror != nil {
		_ = b.mirror.Call(ctx, "uci", "delete",
			map[string]any{"config": "rpcd", "section": user}, nil)
		_ = b.mirror.Call(ctx, "uci", "commit", map[string]any{"config": "rpcd"}, nil)
	}
	return nil
}

func (b *fakeBoot) Fingerprint() string { return "SHA256:test-host-key" }
func (b *fakeBoot) Close() error        { b.closed = true; return nil }

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

	boot := newBootFor(op)
	res, err := testAdopter().Adopt(ctx, op, boot)
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

	if res.HostKeyFP == "" {
		t.Error("the bootstrap channel's host key was not captured for pinning")
	}

	// The entire device-side footprint: one file and one login.
	if len(boot.acl) != 1 {
		t.Fatalf("footprint should be exactly one file, got %v", boot.acl)
	}
	content := string(boot.acl[DefaultACLPath])
	if !strings.Contains(content, `"oonfeewrt"`) {
		t.Errorf("ACL content did not survive the write: %q", content)
	}
	if boot.login != DefaultUser {
		t.Errorf("login = %q, want %q", boot.login, DefaultUser)
	}
	// rpcd rejects a plaintext password outright, so this must be a crypt hash.
	if !strings.HasPrefix(boot.passHash, "$6$") {
		t.Errorf("the login was created with %q, not a SHA-512 crypt hash", boot.passHash)
	}
	if boot.passHash == res.Credential.Password {
		t.Error("the plaintext password was written to the device")
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
	boot := newBootFor(op)
	if _, err := a.Adopt(ctx, op, boot); err == nil {
		t.Fatal("Adopt should fail when the new credential cannot log in")
	} else if !strings.Contains(err.Error(), "does not work") {
		t.Fatalf("error should name the verification failure, got: %v", err)
	}
	if len(boot.acl) != 0 || boot.login != "" || boot.removeFootprintCalls != 1 {
		t.Fatalf("failed controller login left a footprint: acl=%v login=%q cleanup=%d",
			boot.acl, boot.login, boot.removeFootprintCalls)
	}
}

func TestAdoptCreateLoginFailureRollsBackOnlyTheACL(t *testing.T) {
	op := operatorClient(t)
	primary := errors.New("create login fault")
	boot := newBoot()
	boot.login = DefaultUser
	boot.passHash = "foreign-state-must-remain"
	boot.failLogin = primary

	_, err := testAdopter().Adopt(context.Background(), op, boot)
	if !errors.Is(err, primary) {
		t.Fatalf("create-login failure was not preserved: %v", err)
	}
	if boot.removeACLCalls != 1 || boot.removeFootprintCalls != 0 {
		t.Fatalf("cleanup calls: ACL=%d footprint=%d, want ACL only",
			boot.removeACLCalls, boot.removeFootprintCalls)
	}
	if len(boot.acl) != 0 {
		t.Fatalf("ACL remains after rollback: %v", boot.acl)
	}
	if boot.login != DefaultUser || boot.passHash != "foreign-state-must-remain" {
		t.Fatalf("create-login failure removed pre-existing login state: login=%q hash=%q",
			boot.login, boot.passHash)
	}
}

func TestAdoptFreshSessionFailureRollsBackTheFootprint(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)
	if err := op.Call(ctx, "__test", "reject_login",
		map[string]any{"usernames": []string{"root"}}, nil); err != nil {
		t.Skipf("mock does not support login fault injection: %v", err)
	}
	t.Cleanup(func() {
		_ = op.Call(ctx, "__test", "reject_login",
			map[string]any{"usernames": []string{}}, nil)
	})

	boot := newBootFor(op)
	_, err := testAdopter().Adopt(ctx, op, boot)
	if err == nil || !strings.Contains(err.Error(), "cannot open a session to verify") {
		t.Fatalf("fresh-session failure was not preserved: %v", err)
	}
	if len(boot.acl) != 0 || boot.login != "" || boot.removeFootprintCalls != 1 {
		t.Fatalf("fresh-session failure left a footprint: acl=%v login=%q cleanup=%d",
			boot.acl, boot.login, boot.removeFootprintCalls)
	}
}

func TestAdoptReverifiesIdentityOnTheNewControllerSession(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)
	a := testAdopter()
	called := false
	a.VerifyController = func(_ context.Context, controller *ubus.Client) error {
		called = true
		if controller.Session() == op.Session() {
			return errors.New("identity check received the operator session")
		}
		// This call is accepted only after CreateLogin mirrored the new scoped
		// credential into rpcd, so the callback cannot have run before proof.
		return controller.Call(ctx, "system", "board", nil, nil)
	}
	if _, err := a.Adopt(ctx, op, newBootFor(op)); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !called {
		t.Fatal("controller identity verification never ran")
	}
}

func TestAdoptStopsWhenControllerIdentityChanges(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)
	a := testAdopter()
	a.VerifyController = func(context.Context, *ubus.Client) error {
		return errors.New("MAC changed")
	}
	boot := newBootFor(op)
	if _, err := a.Adopt(ctx, op, boot); err == nil {
		t.Fatal("identity mismatch was accepted")
	} else if !strings.Contains(err.Error(), "controller identity verification failed") ||
		!strings.Contains(err.Error(), "MAC changed") {
		t.Fatalf("identity failure lost its cause: %v", err)
	}
	if len(boot.acl) != 0 || boot.login != "" || boot.removeFootprintCalls != 1 {
		t.Fatalf("identity failure left a footprint: acl=%v login=%q cleanup=%d",
			boot.acl, boot.login, boot.removeFootprintCalls)
	}
}

func TestAdoptCapabilityProbeFailureRollsBackAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	op := operatorClient(t)
	a := testAdopter()
	a.VerifyController = func(context.Context, *ubus.Client) error {
		cancel()
		return nil
	}
	boot := newBootFor(op)

	_, err := a.Adopt(ctx, op, boot)
	if err == nil || !strings.Contains(err.Error(), "capability probe") ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("capability-probe failure was not preserved: %v", err)
	}
	if len(boot.acl) != 0 || boot.login != "" || boot.removeFootprintCalls != 1 {
		t.Fatalf("capability failure left a footprint: acl=%v login=%q cleanup=%d",
			boot.acl, boot.login, boot.removeFootprintCalls)
	}
}

func TestAdoptRollbackFailureReportsSafeResidueAndRemedy(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)
	const password = "controller-example-placeholder"
	primary := errors.New("identity changed")
	cleanup := errors.New("cleanup channel failed")
	a := testAdopter()
	a.NewPassword = func() (string, error) { return password, nil }
	a.VerifyController = func(context.Context, *ubus.Client) error { return primary }
	boot := newBootFor(op)
	boot.failRemoveFootprint = cleanup
	t.Cleanup(func() {
		boot.failRemoveFootprint = nil
		_ = boot.RemoveFootprint(context.Background(), DefaultACLPath, DefaultUser)
	})

	_, err := a.Adopt(ctx, op, boot)
	var rollback *RollbackError
	if !errors.As(err, &rollback) || !errors.Is(err, primary) ||
		!errors.Is(rollback.Cleanup, cleanup) {
		t.Fatalf("rollback failure lost its causes: %v", err)
	}
	wantResidue := []string{
		DefaultACLPath,
		"config login '" + DefaultUser + "' in /etc/config/rpcd",
	}
	if fmt.Sprint(rollback.Residue) != fmt.Sprint(wantResidue) {
		t.Fatalf("residue = %v, want %v", rollback.Residue, wantResidue)
	}
	commands := strings.Join(rollback.CleanupCommands, "\n")
	if !strings.Contains(commands, "uci -q delete rpcd."+DefaultUser) ||
		!strings.Contains(commands, "rm -f '"+DefaultACLPath+"'") {
		t.Fatalf("manual remedy is incomplete: %q", commands)
	}
	for name, secret := range map[string]string{
		"password": password,
		"hash":     boot.passHash,
	} {
		if secret != "" && (strings.Contains(err.Error(), secret) || strings.Contains(commands, secret)) {
			t.Errorf("rollback report exposed controller %s: %q", name, err)
		}
	}
}

func TestAdoptACLOnlyRollbackFailureDoesNotPrescribeLoginRemoval(t *testing.T) {
	op := operatorClient(t)
	const password = "another-controller-example-placeholder"
	primary := errors.New("create login failed")
	cleanup := errors.New("ACL cleanup failed")
	a := testAdopter()
	a.NewPassword = func() (string, error) { return password, nil }
	boot := newBoot()
	boot.login = DefaultUser
	boot.passHash = "foreign-login-hash"
	boot.failLogin = primary
	boot.failRemoveACL = cleanup

	_, err := a.Adopt(context.Background(), op, boot)
	var rollback *RollbackError
	if !errors.As(err, &rollback) || !errors.Is(err, primary) {
		t.Fatalf("rollback failure lost its primary cause: %v", err)
	}
	if fmt.Sprint(rollback.Residue) != fmt.Sprint([]string{DefaultACLPath}) {
		t.Fatalf("ACL-only residue = %v", rollback.Residue)
	}
	commands := strings.Join(rollback.CleanupCommands, "\n")
	if strings.Contains(commands, "uci") || strings.Contains(commands, "rpcd."+DefaultUser) {
		t.Fatalf("ACL-only remedy would remove an unproved login: %q", commands)
	}
	if !strings.Contains(commands, "rm -f '"+DefaultACLPath+"'") {
		t.Fatalf("ACL-only remedy omitted the ACL: %q", commands)
	}
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), boot.passHash) {
		t.Fatalf("ACL-only rollback report exposed a credential: %v", err)
	}
}

func TestAdoptRefusesWithoutACLContent(t *testing.T) {
	op := operatorClient(t)
	a := &Adopter{}
	if _, err := a.Adopt(context.Background(), op, newBoot()); err == nil {
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
	if _, err := a.Adopt(ctx, op, newBootFor(op)); err != nil {
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
	commands := rep.CleanupCommands()
	want := "uci -q delete rpcd.oonfeewrt\nuci commit rpcd\n" +
		"uci -q get rpcd.oonfeewrt >/dev/null 2>&1 && echo 'ERROR: login still present' || echo 'login gone'\n" +
		"rm -f '/usr/share/rpcd/acl.d/oonfeewrt.json'\n" +
		"[ ! -e '/usr/share/rpcd/acl.d/oonfeewrt.json' ] && echo 'ACL gone' || echo 'ERROR: ACL still present'"
	if got := strings.Join(commands, "\n"); got != want {
		t.Errorf("manual cleanup recipe = %q, want %q", got, want)
	}
}

func TestCleanupCommandsNeverPrintUnsafeRootCommands(t *testing.T) {
	rep := &UnadoptReport{
		User:    "oonfeewrt; reboot",
		ACLPath: "/etc/shadow",
	}
	if got := rep.CleanupCommands(); len(got) != 0 {
		t.Fatalf("unsafe internal values became root commands: %v", got)
	}
}

func TestUnadoptWithOperatorRemovesTheFootprint(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)
	a := testAdopter()
	boot := newBootFor(op)
	if _, err := a.Adopt(ctx, op, boot); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if len(boot.acl) == 0 {
		t.Fatal("precondition: the ACL should be on the device")
	}

	rep, err := a.Unadopt(ctx, op, boot, []Section{
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
	if len(boot.acl) != 0 {
		t.Errorf("ACL file should be removed from the device, still have %v", boot.acl)
	}
	if boot.login != "" {
		t.Errorf("the login should be gone, still have %q", boot.login)
	}
}

// Re-running un-adopt must be safe: "already gone" is success, not an error.
func TestUnadoptIsIdempotent(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)
	a := testAdopter()
	boot := newBootFor(op)
	if _, err := a.Adopt(ctx, op, boot); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if _, err := a.Unadopt(ctx, op, boot, nil); err != nil {
		t.Fatalf("first Unadopt: %v", err)
	}
	rep, err := a.Unadopt(ctx, op, boot, nil)
	if err != nil {
		t.Fatalf("second Unadopt should be a no-op, got %v (%v)", err, rep.Errors)
	}
	if rep.FootprintRemains {
		t.Error("nothing left to remove, so no footprint should be reported")
	}
}

// Adoption cannot proceed without a channel that can actually install the
// footprint. ubus refuses both writes even to root, so a nil bootstrap is a
// hard error rather than something to attempt and fail at halfway.
func TestAdoptRefusesWithoutABootstrap(t *testing.T) {
	op := operatorClient(t)
	if _, err := testAdopter().Adopt(context.Background(), op, nil); !errors.Is(err, ErrNoBootstrap) {
		t.Fatalf("got %v, want ErrNoBootstrap", err)
	}
}

// A failure installing the ACL must stop before the login is created. A login
// pointing at access-groups that do not exist is a credential that authenticates
// and can do nothing — the most confusing possible half-state.
func TestAdoptDoesNotCreateALoginIfTheACLFails(t *testing.T) {
	op := operatorClient(t)
	boot := newBoot()
	boot.failACL = errors.New("no space left on device")

	if _, err := testAdopter().Adopt(context.Background(), op, boot); err == nil {
		t.Fatal("Adopt succeeded despite the ACL write failing")
	}
	if boot.login != "" {
		t.Fatalf("a login %q was created after the ACL write failed", boot.login)
	}
}

// The reverse ordering check: a login failure happens after the ACL landed,
// and adoption must remove only that proved write.
func TestAdoptRollsBackTheACLWhenTheLoginFails(t *testing.T) {
	op := operatorClient(t)
	boot := newBoot()
	boot.failLogin = errors.New("uci: entry not found")

	_, err := testAdopter().Adopt(context.Background(), op, boot)
	if err == nil {
		t.Fatal("Adopt succeeded despite the login failing")
	}
	if !strings.Contains(err.Error(), "create login") {
		t.Errorf("the error does not name the failed step: %v", err)
	}
	if len(boot.acl) != 0 || boot.removeACLCalls != 1 || boot.removeFootprintCalls != 0 {
		t.Fatalf("login failure cleanup: acl=%v removeACL=%d removeFootprint=%d",
			boot.acl, boot.removeACLCalls, boot.removeFootprintCalls)
	}
}

// The registry gates what every screen renders, and screens render from what
// the CONTROLLER can reach — so the probe must run on the controller's session,
// after its ACL is in place. Probing first, as the operator, answers a
// different question and gets it wrong on a genuinely fresh device.
func TestProbeRunsAfterTheACLIsInstalled(t *testing.T) {
	ctx := context.Background()
	op := operatorClient(t)

	boot := newBootFor(op)
	// Fail the ACL write. If the probe ran first, it would already have
	// happened and the capability record would exist; it must not.
	boot.failACL = errors.New("disk full")
	if _, err := testAdopter().Adopt(ctx, op, boot); err == nil {
		t.Fatal("Adopt succeeded despite the ACL write failing")
	}

	// Now let it succeed and confirm the record comes back populated.
	boot2 := newBootFor(op)
	res, err := testAdopter().Adopt(ctx, op, boot2)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if res.Caps == nil {
		t.Fatal("no capability record")
	}
	if len(res.Caps.Features) == 0 {
		t.Fatal("the capability record is empty")
	}
	// The ACL must have been installed BEFORE the probe could have run.
	if len(boot2.acl) == 0 || boot2.login == "" {
		t.Fatalf("footprint incomplete when the probe ran: acl=%v login=%q",
			len(boot2.acl), boot2.login)
	}
}
