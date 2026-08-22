package adoption

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHBootstrap installs the footprint over SSH.
//
// Used for exactly two operations in a device's whole lifetime — adoption and
// un-adoption — because they are the two things ubus cannot do (see Bootstrap).
// Everything in between is ubus.
//
// The device-side assumptions are deliberately minimal, and were checked
// against a stock OpenWrt 25.12.5 rather than assumed:
//
//   - `base64` is NOT present on that build, so file content is piped to `cat`
//     over the session's stdin. That also means the content is never a shell
//     argument, so it needs no quoting and cannot be injected through.
//   - There is no `sftp-server`, so scp/sftp are unavailable.
//   - `uci`, `cat`, `mktemp` and `sha256sum` are present; the write is verified
//     with sha256sum rather than assumed to have landed.
type SSHBootstrap struct {
	client *ssh.Client
	hostFP string
}

// SSHOptions configure the connection.
type SSHOptions struct {
	Host     string // "192.168.1.1" or "host:port"
	Username string
	Password string
	// PrivateKey is an optional PEM key, used in preference to a password.
	PrivateKey []byte
	// HostKeyFP, when set, must match the device's key — trust on first use,
	// pinned thereafter, exactly like the TLS certificate.
	HostKeyFP string
	Timeout   time.Duration
}

type PackageState struct {
	Manager     string
	Installed   []string
	LLDPEnabled bool
	LLDPRunning bool
}

type LLDPConfigState struct {
	Export             string
	WiredBridgeMembers []string
	RuntimeInterfaces  []string
}

// DialSSH opens the bootstrap channel.
//
// Host key handling is trust-on-first-use: with no pin, the key is recorded and
// returned for the caller to store; with a pin, a mismatch is refused. There is
// no third option — a device reached for the first time has no prior key to
// check against, and refusing to adopt anything until an operator has manually
// collected fingerprints is a worse answer than recording what we saw.
func DialSSH(ctx context.Context, opt SSHOptions) (*SSHBootstrap, error) {
	if opt.Timeout <= 0 {
		opt.Timeout = 20 * time.Second
	}
	host := opt.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "22")
	}

	var seen string
	cfg := &ssh.ClientConfig{
		User:    opt.Username,
		Timeout: opt.Timeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = fingerprint(key)
			if opt.HostKeyFP == "" {
				return nil // first use
			}
			if seen != opt.HostKeyFP {
				return fmt.Errorf("the device's SSH host key changed: expected %s, "+
					"got %s. Either it was reflashed, or something is impersonating "+
					"it — this is not something to click through", opt.HostKeyFP, seen)
			}
			return nil
		},
	}
	if len(opt.PrivateKey) > 0 {
		signer, err := ssh.ParsePrivateKey(opt.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("adoption: unusable private key: %w", err)
		}
		cfg.Auth = append(cfg.Auth, ssh.PublicKeys(signer))
	}
	if opt.Password != "" {
		cfg.Auth = append(cfg.Auth, ssh.Password(opt.Password),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				a := make([]string, len(qs))
				for i := range a {
					a[i] = opt.Password
				}
				return a, nil
			}))
	}
	if len(cfg.Auth) == 0 {
		// An empty password is legitimate on a device with no root password —
		// which is common enough on stock OpenWrt that refusing it would block
		// the most ordinary first adoption there is.
		cfg.Auth = append(cfg.Auth, ssh.Password(""))
	}

	d := net.Dialer{Timeout: opt.Timeout}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("adoption: cannot reach %s over SSH: %w", host, err)
	}
	// Bound the HANDSHAKE. ClientConfig.Timeout is read only by ssh.Dial;
	// NewClientConn hands the connection to clientHandshake unbounded, and it
	// does not observe ctx either. So an address that accepts TCP and never
	// completes the version exchange — a stalled proxy, a port-forward to
	// nothing, a non-SSH service that never writes — hung adoption forever,
	// past the 90s adopt timeout and past the cancelled request, on a server
	// with no WriteTimeout.
	hs := time.Now().Add(opt.Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(hs) {
		hs = dl
	}
	_ = conn.SetDeadline(hs)

	c, chans, reqs, err := ssh.NewClientConn(conn, host, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("adoption: SSH to %s: %w", host, err)
	}
	// Cleared, and this half is load-bearing. The same connection carries every
	// InstallACL and CreateLogin session afterwards, and adoption's full flow —
	// a capability probe plus the writes plus a verification login — routinely
	// outlives the 30s both callers pass. A deadline left in place would kill
	// adoption mid-write, which is the one moment a device is half-configured.
	_ = conn.SetDeadline(time.Time{})

	return &SSHBootstrap{client: ssh.NewClient(c, chans, reqs), hostFP: seen}, nil
}

func fingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// Fingerprint is the device's SSH host key, for pinning.
func (b *SSHBootstrap) Fingerprint() string { return b.hostFP }

func (b *SSHBootstrap) Close() error {
	if b.client == nil {
		return nil
	}
	return b.client.Close()
}

// run executes one command, optionally feeding it stdin, and returns its output.
// operation is a fixed, operator-safe label: cmd may contain generated
// credentials and must never become part of an error or log message.
func (b *SSHBootstrap) run(ctx context.Context, stdin []byte, operation, cmd string, secrets ...string) (string, error) {
	sess, err := b.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("adoption: open SSH session: %w", err)
	}
	defer sess.Close()

	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	var out, errb boundedBuffer
	sess.Stdout = &out
	sess.Stderr = &errb

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	case err := <-done:
		if out.truncated || errb.truncated {
			return "", fmt.Errorf("adoption: %s output exceeded %d KiB", operation, maxSSHOutput/(1<<10))
		}
		if err != nil {
			return out.String(), sshRunError(operation, err, out.String(), errb.String(), cmd, secrets)
		}
	}
	return out.String(), nil
}

const maxSSHOutput = 64 << 10

// boundedBuffer drains remote output while retaining only a fixed prefix.
// Returning len(p) prevents a chatty peer from blocking the SSH channel after
// the capture limit is reached.
type boundedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxSSHOutput - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	if n > remaining {
		b.truncated = true
	}
	return n, nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

func sshRunError(operation string, runErr error, stdout, stderr, cmd string, secrets []string) error {
	redactions := append([]string{cmd}, secrets...)
	status := redactSSHError(runErr.Error(), redactions)
	if len(secrets) > 0 {
		// A remote shell may echo or reformat its input. For commands carrying a
		// verifier, suppressing its output is the only reliable redaction.
		return fmt.Errorf("adoption: %s failed: %s (remote output withheld)", operation, status)
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = strings.TrimSpace(stdout)
	}
	msg = redactSSHError(msg, redactions)
	if msg == "" {
		return fmt.Errorf("adoption: %s failed: %s", operation, status)
	}
	return fmt.Errorf("adoption: %s failed: %s (%s)", operation, status, msg)
}

func redactSSHError(msg string, values []string) string {
	for _, value := range values {
		if value != "" {
			msg = strings.ReplaceAll(msg, value, "[redacted]")
		}
	}
	return msg
}

// InstallACL writes the file and proves it landed.
//
// The content goes over stdin rather than inside the command, so no amount of
// JSON quoting can become shell syntax. It is written to a temporary file and
// moved into place, so a device that loses power mid-write is left with either
// the old file or the new one, never half of either — and then hashed, because
// "the command exited 0" is not the same as "the bytes are on disk".
func (b *SSHBootstrap) InstallACL(ctx context.Context, path string, content []byte) error {
	if !safePath(path) {
		return fmt.Errorf("adoption: refusing to write an unsafe path %q", path)
	}
	tmp := path + ".oonfee-tmp"
	if _, err := b.run(ctx, content, "write temporary ACL", "cat > "+shellQuote(tmp)); err != nil {
		return err
	}
	got, err := b.run(ctx, nil, "verify temporary ACL", "sha256sum "+shellQuote(tmp))
	if err != nil {
		return err
	}
	if err := verifyACLHash(content, got); err != nil {
		_, _ = b.run(ctx, nil, "discard temporary ACL", "rm -f "+shellQuote(tmp))
		return err
	}
	if _, err := b.run(ctx, nil, "install ACL",
		"mv "+shellQuote(tmp)+" "+shellQuote(path)+" && chmod 0644 "+shellQuote(path)); err != nil {
		return err
	}
	return nil
}

func verifyACLHash(content []byte, output string) error {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return fmt.Errorf("adoption: the ACL file could not be verified after transfer (wrote %d bytes, device returned no digest)", len(content))
	}
	want := sha256.Sum256(content)
	if fields[0] != hex.EncodeToString(want[:]) {
		return fmt.Errorf("adoption: the ACL file did not survive the transfer (wrote %d bytes, device returned a different digest)", len(content))
	}
	return nil
}

// crypt hashes use only this alphabet, so a hash that does not match it is not
// a hash and must never reach a shell.
var cryptHash = regexp.MustCompile(`^[A-Za-z0-9$./]+$`)
var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var packageIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+_.-]*$`)
var interfaceIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,31}$`)

// CreateLogin writes the rpcd login and commits it.
//
// Every interpolated value is validated against a strict alphabet first. The
// password hash contains `$`, which single quotes handle, but validating is
// what makes that safe rather than merely true today.
func (b *SSHBootstrap) CreateLogin(ctx context.Context, user, passHash string, groups []string) error {
	if !identifier.MatchString(user) {
		return fmt.Errorf("adoption: refusing an unsafe login name %q", user)
	}
	if !cryptHash.MatchString(passHash) {
		return fmt.Errorf("adoption: the password hash contains characters that " +
			"are not part of a crypt hash; refusing to pass it to a shell")
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "uci -q delete rpcd.%s; ", user)
	fmt.Fprintf(&sb, "uci set rpcd.%s=login && ", user)
	fmt.Fprintf(&sb, "uci set rpcd.%s.username=%s && ", user, shellQuote(user))
	fmt.Fprintf(&sb, "uci set rpcd.%s.password=%s && ", user, shellQuote(passHash))
	for _, g := range groups {
		if !identifier.MatchString(g) {
			return fmt.Errorf("adoption: refusing an unsafe access-group name %q", g)
		}
		fmt.Fprintf(&sb, "uci add_list rpcd.%s.read=%s && ", user, shellQuote(g))
		fmt.Fprintf(&sb, "uci add_list rpcd.%s.write=%s && ", user, shellQuote(g))
	}
	sb.WriteString("uci commit rpcd")

	if _, err := b.run(ctx, nil, "create controller login", sb.String(), passHash); err != nil {
		return err
	}
	return nil
}

// RemoveACL removes only the controller ACL. It is the rollback used when
// CreateLogin fails, because that failure does not prove ownership of the
// login section and therefore must not delete it.
func (b *SSHBootstrap) RemoveACL(ctx context.Context, aclPath string) error {
	if !safePath(aclPath) {
		return fmt.Errorf("adoption: refusing to remove an unsafe path %q", aclPath)
	}
	out, err := b.run(ctx, nil, "remove controller ACL", fmt.Sprintf(
		"rm -f %s; printf 'acl=%%s\\n' \"$([ -e %s ] && echo present || echo gone)\"",
		shellQuote(aclPath), shellQuote(aclPath)))
	if err != nil {
		return err
	}
	if strings.Contains(out, "acl=present") {
		return fmt.Errorf("adoption: the ACL file %s is still on the device after removal", aclPath)
	}
	return nil
}

// RemoveFootprint deletes the login and the ACL file.
//
// Missing is success: un-adopting a device that was already partly cleaned must
// not fail on the half that is already gone.
func (b *SSHBootstrap) RemoveFootprint(ctx context.Context, aclPath, user string) error {
	if !identifier.MatchString(user) {
		return fmt.Errorf("adoption: refusing an unsafe login name %q", user)
	}
	if !safePath(aclPath) {
		return fmt.Errorf("adoption: refusing to remove an unsafe path %q", aclPath)
	}
	// Report from what the device ANSWERS, not from the exit status of the last
	// command in the line.
	//
	// It used to be `uci -q delete …; uci commit rpcd; rm -f …` — three
	// statements joined by `;`, so the result was `rm -f`'s, and `rm -f`
	// succeeds on a file that was never there. Anything that made uci fail
	// while unlink still worked therefore reported a clean un-adopt: a full
	// /overlay (commit must write a new file, rm need not), a held uci lock, an
	// unparsable /etc/config/rpcd, a missing uci binary.
	//
	// The delete is staged in /tmp/.uci until committed, so an uncommitted
	// removal leaves `config login 'oonfeewrt'` in /etc/config/rpcd and it
	// comes back in full at the next reboot — while the ACL file is gone. The
	// login grants nothing without its ACL group, so this is a dead credential
	// rather than a live one; what it costs is the report. Un-adopt says the
	// device is clean, the controller deletes its inventory row, and the
	// residue nobody was told about is now recorded nowhere at all.
	out, err := b.run(ctx, nil, "remove controller footprint", fmt.Sprintf(
		"uci -q delete rpcd.%s; uci commit rpcd; rm -f %s; "+
			"printf 'login=%%s acl=%%s\n' "+
			"\"$(uci -q get rpcd.%s >/dev/null 2>&1 && echo present || echo gone)\" "+
			"\"$([ -e %s ] && echo present || echo gone)\"",
		user, shellQuote(aclPath), user, shellQuote(aclPath)))
	if err != nil {
		return err
	}
	if strings.Contains(out, "login=present") {
		return fmt.Errorf("adoption: the controller's login %q is still in "+
			"/etc/config/rpcd after removal — the delete was staged but not "+
			"committed, so it returns at the next reboot", user)
	}
	if strings.Contains(out, "acl=present") {
		return fmt.Errorf("adoption: the ACL file %s is still on the device "+
			"after removal", aclPath)
	}
	return nil
}

// PackageState reads only the fixed package/service facts needed by the LLDP
// capability workflow. It is not a generic remote-command surface.
func (b *SSHBootstrap) PackageState(ctx context.Context) (PackageState, error) {
	const cmd = `if command -v apk >/dev/null 2>&1; then
  echo manager=apk
  apk info 2>/dev/null | sed 's/^/package=/'
elif command -v opkg >/dev/null 2>&1; then
  echo manager=opkg
  opkg list-installed 2>/dev/null | sed 's/ .*/ /' | sed 's/ $//' | sed 's/^/package=/'
else
  echo manager=none
fi
[ -x /etc/init.d/lldpd ] && /etc/init.d/lldpd enabled >/dev/null 2>&1 && echo lldp_enabled=1 || echo lldp_enabled=0
pidof lldpd >/dev/null 2>&1 && echo lldp_running=1 || echo lldp_running=0`
	out, err := b.run(ctx, nil, "inspect package capability state", cmd)
	if err != nil {
		return PackageState{}, err
	}
	return parsePackageState(out)
}

func parsePackageState(output string) (PackageState, error) {
	var state PackageState
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "manager":
			state.Manager = value
		case "package":
			value = strings.TrimSpace(value)
			if packageIdentifier.MatchString(value) && !seen[value] {
				seen[value] = true
				state.Installed = append(state.Installed, value)
			}
		case "lldp_enabled":
			state.LLDPEnabled = value == "1"
		case "lldp_running":
			state.LLDPRunning = value == "1"
		}
	}
	if state.Manager != "apk" && state.Manager != "opkg" {
		return PackageState{}, fmt.Errorf("adoption: the router has neither apk nor opkg")
	}
	return state, nil
}

// LLDPPlan asks the router's package manager for the exact proposed change.
// Installation planning refreshes the package index; callers must disclose and
// obtain acknowledgement for that bounded write before invoking it.
const lldpAPKInstallPlanCommand = "{ apk update && apk --simulate add lldpd; } 2>&1"
const lldpOPKGInstallPlanCommand = "{ opkg update && opkg --noaction install lldpd; } 2>&1"
const lldpAPKInstallCommand = "apk add lldpd"
const lldpOPKGInstallCommand = "opkg install lldpd"

const lldpDiagnosticsCommand = `echo CONFIGURATION
uci -q show lldpd 2>&1 || echo lldpd_config=absent
echo RUNTIME_INTERFACES
lldpcli -f json show interfaces 2>&1
echo RUNTIME_NEIGHBORS
lldpcli -f json show neighbors hidden 2>&1`

// LLDPDiagnostics reads the fixed configuration and runtime facts needed to
// explain an installed service that reports no neighbours. It changes nothing.
func (b *SSHBootstrap) LLDPDiagnostics(ctx context.Context) (string, error) {
	out, err := b.run(ctx, nil, "inspect LLDP runtime interfaces", lldpDiagnosticsCommand)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if len(out) > 32*1024 {
		return "", fmt.Errorf("adoption: LLDP diagnostics exceeded 32 KiB")
	}
	return out, nil
}

// LLDPConfigPlanState reads only persistent configuration and physical bridge
// membership. Planning and rollback must remain available while lldpd is down.
func (b *SSHBootstrap) LLDPConfigPlanState(ctx context.Context) (LLDPConfigState, error) {
	exported, err := b.run(ctx, nil, "read LLDP configuration baseline", "uci -q export lldpd")
	if err != nil {
		return LLDPConfigState{}, err
	}
	if exported == "" || len(exported) > 16*1024 {
		return LLDPConfigState{}, fmt.Errorf("adoption: invalid LLDP configuration baseline")
	}
	const membersCommand = `for p in /sys/class/net/br-lan/lower_*; do
  [ -e "$p" ] || continue
  n=${p##*/lower_}
  [ -e "/sys/class/net/$n/phy80211" ] && continue
  [ -d "/sys/class/net/$n/wireless" ] && continue
  printf '%s\n' "$n"
done`
	membersRaw, err := b.run(ctx, nil, "read wired bridge members", membersCommand)
	if err != nil {
		return LLDPConfigState{}, err
	}
	members, err := parseInterfaceNames(membersRaw)
	if err != nil || len(members) == 0 {
		return LLDPConfigState{}, fmt.Errorf("adoption: no safe wired bridge member was found")
	}
	return LLDPConfigState{Export: exported, WiredBridgeMembers: members}, nil
}

func (b *SSHBootstrap) LLDPConfigState(ctx context.Context) (LLDPConfigState, error) {
	state, err := b.LLDPConfigPlanState(ctx)
	if err != nil {
		return LLDPConfigState{}, err
	}
	runtimeRaw, err := b.run(ctx, nil, "read LLDP runtime interfaces", "lldpcli -f json show interfaces")
	if err != nil {
		return LLDPConfigState{}, err
	}
	runtime, err := parseLLDPRuntimeInterfaces(runtimeRaw)
	if err != nil {
		return LLDPConfigState{}, err
	}
	state.RuntimeInterfaces = runtime
	return state, nil
}

const lldpRestartReadyCommand = "/etc/init.d/lldpd restart || exit $?; " +
	"for i in 1 2 3 4 5; do [ -S /var/run/lldpd.socket ] && exit 0; sleep 1; done; exit 24"

func (b *SSHBootstrap) ConfigureLLDP(ctx context.Context, expectedExport string, interfaces []string) error {
	if err := validateInterfaceNames(interfaces); err != nil {
		return err
	}
	expected := sha256.Sum256([]byte(expectedExport))
	cmd := "[ \"$(uci -q export lldpd | sha256sum | cut -d' ' -f1)\" = " + shellQuote(hex.EncodeToString(expected[:])) + " ] || exit 23; " +
		"uci -q delete lldpd.config.interface; "
	for _, name := range interfaces {
		cmd += "uci add_list lldpd.config.interface=" + shellQuote(name) + "; "
	}
	cmd += "uci commit lldpd; " + lldpRestartReadyCommand
	_, err := b.run(ctx, nil, "configure LLDP physical interfaces", cmd)
	return err
}

func (b *SSHBootstrap) RestoreLLDPConfig(ctx context.Context, expectedExport, baseline string) error {
	expected := sha256.Sum256([]byte(expectedExport))
	restored := sha256.Sum256([]byte(baseline))
	cmd := "[ \"$(uci -q export lldpd | sha256sum | cut -d' ' -f1)\" = " + shellQuote(hex.EncodeToString(expected[:])) + " ] || exit 23; " +
		"uci import lldpd && uci commit lldpd; " +
		"[ \"$(uci -q export lldpd | sha256sum | cut -d' ' -f1)\" = " + shellQuote(hex.EncodeToString(restored[:])) + " ] || exit 25; " +
		"/etc/init.d/lldpd restart"
	_, err := b.run(ctx, []byte(baseline), "restore LLDP configuration baseline", cmd)
	return err
}

func parseInterfaceNames(raw string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || seen[name] {
			continue
		}
		if !interfaceIdentifier.MatchString(name) {
			return nil, fmt.Errorf("adoption: invalid interface name %q", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

func validateInterfaceNames(names []string) error {
	if len(names) == 0 || len(names) > 32 {
		return fmt.Errorf("adoption: invalid LLDP interface count")
	}
	for _, name := range names {
		if !interfaceIdentifier.MatchString(name) {
			return fmt.Errorf("adoption: invalid interface name %q", name)
		}
	}
	return nil
}

func parseLLDPRuntimeInterfaces(raw string) ([]string, error) {
	var doc struct {
		LLDP json.RawMessage `json:"lldp"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("adoption: decode LLDP runtime interfaces: %w", err)
	}
	var lldp struct {
		Interface json.RawMessage `json:"interface"`
	}
	if err := json.Unmarshal(doc.LLDP, &lldp); err != nil {
		return nil, fmt.Errorf("adoption: decode LLDP runtime interfaces: %w", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(lldp.Interface, &object); err == nil {
		names := make([]string, 0, len(object))
		for name := range object {
			names = append(names, name)
		}
		if err := validateInterfaceNames(names); err != nil && len(names) > 0 {
			return nil, err
		}
		slices.Sort(names)
		return names, nil
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(lldp.Interface, &rows); err != nil {
		return nil, fmt.Errorf("adoption: decode LLDP runtime interfaces: %w", err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if nameRaw, ok := row["name"]; ok {
			var name string
			if err := json.Unmarshal(nameRaw, &name); err != nil || !interfaceIdentifier.MatchString(name) {
				return nil, fmt.Errorf("adoption: invalid LLDP runtime interface name")
			}
			seen[name] = true
			continue
		}
		if len(row) != 1 {
			return nil, fmt.Errorf("adoption: ambiguous LLDP runtime interface")
		}
		for name := range row {
			if !interfaceIdentifier.MatchString(name) {
				return nil, fmt.Errorf("adoption: invalid LLDP runtime interface name")
			}
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	if len(names) > 32 {
		return nil, fmt.Errorf("adoption: invalid LLDP interface count")
	}
	slices.Sort(names)
	return names, nil
}

func (b *SSHBootstrap) LLDPPlan(ctx context.Context, manager string, removePackages []string) (string, error) {
	if err := validatePackageNames(removePackages); err != nil {
		return "", err
	}
	remove := len(removePackages) > 0
	packageArgs := strings.Join(removePackages, " ")
	var operation, cmd string
	switch {
	case manager == "apk" && !remove:
		operation, cmd = "resolve LLDP package installation", lldpAPKInstallPlanCommand
	case manager == "apk" && remove:
		operation, cmd = "resolve LLDP package removal", "apk --simulate del "+packageArgs+" 2>&1"
	case manager == "opkg" && !remove:
		operation, cmd = "resolve LLDP package installation", lldpOPKGInstallPlanCommand
	case manager == "opkg" && remove:
		operation, cmd = "resolve LLDP package removal", "opkg --noaction remove "+packageArgs+" 2>&1"
	default:
		return "", fmt.Errorf("adoption: unsupported package manager %q", manager)
	}
	out, err := b.run(ctx, nil, operation, cmd)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if len(out) > 16*1024 {
		return "", fmt.Errorf("adoption: package plan exceeded 16 KiB")
	}
	if out == "" {
		out = "Package manager reported no package changes."
	}
	return out, nil
}

func (b *SSHBootstrap) InstallLLDP(ctx context.Context, manager string) error {
	var cmd string
	switch manager {
	case "apk":
		cmd = lldpAPKInstallCommand
	case "opkg":
		cmd = lldpOPKGInstallCommand
	default:
		return fmt.Errorf("adoption: unsupported package manager %q", manager)
	}
	cmd += " && /etc/init.d/lldpd enable && /etc/init.d/lldpd restart && command -v lldpcli >/dev/null"
	_, err := b.run(ctx, nil, "install and enable LLDP capability", cmd)
	return err
}

func (b *SSHBootstrap) RemoveLLDP(ctx context.Context, manager string, removePackages []string, wasEnabled, wasRunning bool) error {
	if err := validatePackageNames(removePackages); err != nil {
		return err
	}
	var cmd string
	if len(removePackages) > 0 {
		packageArgs := strings.Join(removePackages, " ")
		switch manager {
		case "apk":
			cmd = "apk del " + packageArgs
		case "opkg":
			cmd = "opkg remove " + packageArgs
		default:
			return fmt.Errorf("adoption: unsupported package manager %q", manager)
		}
	} else {
		if wasEnabled {
			cmd = "/etc/init.d/lldpd enable"
		} else {
			cmd = "/etc/init.d/lldpd disable"
		}
		if wasRunning {
			cmd += " && /etc/init.d/lldpd restart"
		} else {
			cmd += " && /etc/init.d/lldpd stop"
		}
	}
	_, err := b.run(ctx, nil, "remove LLDP capability", cmd)
	return err
}

func validatePackageNames(packages []string) error {
	for _, name := range packages {
		if !packageIdentifier.MatchString(name) {
			return fmt.Errorf("adoption: invalid package name %q", name)
		}
	}
	return nil
}

// safePath bounds what the bootstrap will touch to the one directory it has any
// business in. A path traversal here writes an arbitrary root-owned file.
func safePath(p string) bool {
	return strings.HasPrefix(p, "/usr/share/rpcd/acl.d/") &&
		!strings.Contains(p, "..") &&
		strings.HasSuffix(p, ".json")
}

// shellQuote wraps a value in single quotes, escaping any single quote within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
