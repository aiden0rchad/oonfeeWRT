// Package adoption brings a stock OpenWrt device under management, and takes it
// back out again.
//
// The whole device-side footprint is created here: one ACL file and one rpcd
// login. Nothing else is ever written, which is what makes "we do not maintain
// OpenWrt" true in practice rather than as an aspiration.
package adoption

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/crypt"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// DefaultACLPath is the one file we add to a device.
const DefaultACLPath = "/usr/share/rpcd/acl.d/oonfeewrt.json"

// DefaultUser is the dedicated login we create. It is NOT root: the controller
// holds a credential scoped to exactly the access-groups in the ACL file, and
// verifying that scoping is part of adoption rather than an assumption.
const DefaultUser = "oonfeewrt"

// ACLGroups are the access-groups granted to the controller login. They must
// exist in the ACL file we install.
var ACLGroups = []string{"oonfeewrt"}

// Adopter performs adoption and un-adoption.
type Adopter struct {
	// ACL is the contents of deploy/acl/oonfeewrt.json.
	ACL []byte
	// ACLPath defaults to DefaultACLPath.
	ACLPath string
	// User defaults to DefaultUser.
	User string
	// Groups defaults to ACLGroups.
	Groups []string
	// NewPassword generates the controller credential. Defaults to 24 random
	// bytes, base64url-encoded.
	NewPassword func() (string, error)
	// VerifyController runs after the newly created controller credential has
	// authenticated and before its capabilities are accepted. The daemon uses it
	// to prove that the endpoint still reports the MAC identified before SSH made
	// any changes.
	VerifyController func(context.Context, *ubus.Client) error
	// Now is injectable for tests.
	Now func() time.Time
}

// Credential is the controller's device login. The caller seals it into the
// credential store; this package never persists anything.
type Credential struct {
	Username string
	Password string
}

// Result is what adoption produced.
type Result struct {
	Credential Credential
	CertFP     string
	// HostKeyFP is the bootstrap channel's peer identity, for pinning. Adoption
	// is the one moment we see it with no prior expectation.
	HostKeyFP string
	Caps      *capability.Registry
}

// RollbackError preserves the adoption failure while reporting the exact
// controller-owned footprint that may remain when automatic cleanup also
// fails. CleanupCommands contains idempotent stock-OpenWrt commands and never
// contains the generated controller credential.
type RollbackError struct {
	Primary         error
	Cleanup         error
	Residue         []string
	CleanupCommands []string
}

func (e *RollbackError) Error() string {
	return fmt.Sprintf("%v; automatic rollback failed: %v; "+
		"controller-owned residue may remain: %s; manual cleanup over SSH: %s",
		e.Primary, e.Cleanup, strings.Join(e.Residue, ", "),
		strings.Join(e.CleanupCommands, " ; "))
}

// Unwrap keeps errors.Is/errors.As focused on the failure that caused
// adoption to abort; cleanup failure is additional recovery information.
func (e *RollbackError) Unwrap() error { return e.Primary }

const adoptionRollbackTimeout = 30 * time.Second

func rollbackAdoption(ctx context.Context, boot Bootstrap, aclPath, user string,
	loginCreated bool, primary error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), adoptionRollbackTimeout)
	defer cancel()

	var cleanupErr error
	if loginCreated {
		cleanupErr = boot.RemoveFootprint(cleanupCtx, aclPath, user)
	} else {
		cleanupErr = boot.RemoveACL(cleanupCtx, aclPath)
	}
	if cleanupErr == nil {
		return primary
	}

	// A cleanup transport failure cannot prove which command, if any, landed.
	// Report the complete known controller-owned set as possible residue and
	// give idempotent commands that are safe whether each item exists or not.
	rep := &UnadoptReport{
		ACLPath: aclPath, User: user,
		ACLRemoved: false, LoginRemoved: !loginCreated,
	}
	return &RollbackError{
		Primary: primary, Cleanup: cleanupErr,
		Residue:         append([]string(nil), rep.Residue()...),
		CleanupCommands: append([]string(nil), rep.CleanupCommands()...),
	}
}

// Adopt runs the whole flow, then verifies the controller credential it created
// actually works and is properly scoped.
//
// It needs TWO channels, and that is a correction rather than a design choice.
// The probe and the verification run over ubus with the operator's session. The
// two writes — the ACL file and the rpcd login — cannot: measured on stock
// OpenWrt 25.12.5, root over ubus is refused both (see Bootstrap). So they go
// through the bootstrap channel, once, and nothing else ever does.
//
// The operator credential is used here and nowhere else. It is never persisted:
// it is requested again at un-adopt, because a controller that could remove its
// own ACL file could equally rewrite it and grant itself a shell
// (ARCHITECTURE §6). Callers must discard it when this returns.
//
// No rpcd restart is performed, and none is needed: measured on hardware, rpcd
// re-reads both /usr/share/rpcd/acl.d and the login config at session-creation
// time, so a freshly written ACL and login are live on the next login. That
// matters beyond tidiness — restarting rpcd destroys every session on the
// device, including any armed rollback's, so adoption must never do it
// casually.
func (a *Adopter) Adopt(ctx context.Context, operator *ubus.Client, boot Bootstrap) (*Result, error) {
	if len(a.ACL) == 0 {
		return nil, errors.New("adoption: no ACL content supplied")
	}
	if boot == nil {
		return nil, ErrNoBootstrap
	}
	aclPath, user, groups := a.aclPath(), a.user(), a.groups()

	// 1. Mint the controller credential. rpcd rejects a plaintext password
	//    outright, and target devices carry no mkpasswd/openssl, so the hash is
	//    computed here.
	password, err := a.newPassword()
	if err != nil {
		return nil, fmt.Errorf("adoption: generate password: %w", err)
	}
	hashed, err := crypt.Hash(password)
	if err != nil {
		return nil, fmt.Errorf("adoption: hash password: %w", err)
	}

	// 3. Install the ACL file BEFORE the login, so the login never exists with
	//    its access-groups undefined.
	if err := boot.InstallACL(ctx, aclPath, a.ACL); err != nil {
		return nil, fmt.Errorf("adoption: write %s: %w", aclPath, err)
	}

	// 4. Create the login.
	if err := boot.CreateLogin(ctx, user, hashed, groups); err != nil {
		primary := fmt.Errorf("adoption: create login %q: %w", user, err)
		return nil, rollbackAdoption(ctx, boot, aclPath, user, false, primary)
	}

	// 5. Prove it. An adoption that reports success without checking the
	//    credential it just created is how a device ends up in the inventory
	//    unreachable.
	verified, err := operator.FreshSession(ctx)
	if err != nil {
		primary := fmt.Errorf("adoption: cannot open a session to verify: %w", err)
		return nil, rollbackAdoption(ctx, boot, aclPath, user, true, primary)
	}
	defer verified.Close()
	if err := verified.Login(ctx, user, password); err != nil {
		primary := fmt.Errorf("adoption: the credential we just created does "+
			"not work: %w", err)
		return nil, rollbackAdoption(ctx, boot, aclPath, user, true, primary)
	}
	if a.VerifyController != nil {
		if err := a.VerifyController(ctx, verified); err != nil {
			primary := fmt.Errorf("adoption: controller identity verification failed: %w", err)
			return nil, rollbackAdoption(ctx, boot, aclPath, user, true, primary)
		}
	}

	// 6. Probe LAST, and on the CONTROLLER's session rather than the operator's.
	//
	// This ordering is a correction, and the reason is worth stating because the
	// original looked more natural. Probing first, as the operator, answers
	// "what can root see" — but the registry gates what every screen renders,
	// and screens render from what the CONTROLLER can reach. The two differ:
	// stock OpenWrt grants zero access to iwinfo.devices, so a probe run before
	// the ACL exists cannot see the radios at all and records survey, hostapd
	// and per-client accounting as NotObservable on hardware that has all three.
	//
	// Measured 2026-08-14 on a genuinely fresh device: the probe reported four
	// capabilities undetermined; the identical calls returned status 0 the
	// moment the ACL landed. Earlier runs missed it only because a leftover ACL
	// file was already on disk, which root's `list read '*'` expanded over.
	caps, err := capability.Probe(ctx, verified)
	if err != nil {
		primary := fmt.Errorf("adoption: capability probe: %w", err)
		return nil, rollbackAdoption(ctx, boot, aclPath, user, true, primary)
	}

	return &Result{
		Credential: Credential{Username: user, Password: password},
		CertFP:     operator.PinnedCert(),
		HostKeyFP:  boot.Fingerprint(),
		Caps:       caps,
	}, nil
}

// writeLogin stages and commits the rpcd login section.
//
// This is a `uci commit`, not an apply-with-rollback, and deliberately so:
// rollback protects the config the *controller* manages, whereas this is the
// credential that lets it manage anything. Arming a rollback here would mean a
// missed confirm silently removes our own access.
func (a *Adopter) writeLogin(ctx context.Context, c *ubus.Client, user, hashed string, groups []string) error {
	section := user
	if err := c.Call(ctx, "uci", "set", map[string]any{
		"config": "rpcd", "section": section, "type": "login",
		"values": map[string]any{
			"username": user,
			"password": hashed,
			"read":     groups,
			"write":    groups,
		},
	}, nil); err != nil {
		return err
	}
	return c.Call(ctx, "uci", "commit", map[string]any{"config": "rpcd"}, nil)
}

// Unadopt removes us from a device, in two phases with different credentials.
//
// Phase 1 runs under the CONTROLLER credential and reverts the UCI sections we
// own — the part that touches the user's configuration, and the part our own
// login is actually granted.
//
// Phase 2 needs the OPERATOR credential, re-prompted. The controller cannot do
// it and must not be able to: write access to its own ACL file is write access
// to arbitrary rpcd scope after the next login, and the rpcd login lives in a
// config with no section-level scoping, so "delete just our own login" is not
// expressible as a grant.
//
// boot may be nil, in which case phase 1 runs and ErrOperatorRequired is
// returned — the caller should then prompt and call again. controller may not
// be nil: without it no phase-1 result is proven, so phase 2 must not run.
type configCaller interface {
	Call(ctx context.Context, object, method string, args, out any) error
}

func (a *Adopter) Unadopt(ctx context.Context, controller configCaller, boot Bootstrap, owned []Section) (*UnadoptReport, error) {
	rep := &UnadoptReport{
		ACLPath: a.aclPath(), User: a.user(), FootprintRemains: true,
		ConfigRemains: append([]Section(nil), owned...),
	}

	// ---- phase 1: give the user's config back ----
	//
	// A missing controller session is not an empty phase: it means no deletion
	// was proved. Removing the login and ACL after that would strand every
	// managed section on the router while deleting the only credential and
	// ownership ledger that can reconcile it.
	if controller == nil {
		if len(owned) > 0 {
			rep.Errors = append(rep.Errors, ErrControllerRequired)
			return rep, ErrControllerRequired
		}
		// An empty, successfully read ownership ledger is proof that phase 1
		// has no work. Requiring a dead controller credential in this case would
		// block clean SSH removal of the login and ACL for no safety benefit.
		rep.ConfigRevertComplete = true
	}
	for _, cfg := range distinctConfigs(owned) {
		group := sectionsInConfig(owned, cfg)
		staged := true
		for _, s := range group {
			err := controller.Call(ctx, "uci", "delete", map[string]any{
				"config": s.Config, "section": s.Section}, nil)
			if err != nil && !isMissing(err) {
				rep.Errors = append(rep.Errors,
					fmt.Errorf("revert %s.%s: %w", s.Config, s.Section, err))
				staged = false
			}
		}
		if !staged {
			// Successful deletes in this config are only session-local so far.
			// Discard them rather than committing a partial hand-back.
			if err := controller.Call(ctx, "uci", "revert",
				map[string]any{"config": cfg}, nil); err != nil {
				rep.Errors = append(rep.Errors,
					fmt.Errorf("discard partial revert of %s: %w", cfg, err))
			}
			return rep, fmt.Errorf("adoption: could not revert every owned %s section", cfg)
		}
		if err := controller.Call(ctx, "uci", "commit",
			map[string]any{"config": cfg}, nil); err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("commit %s: %w", cfg, err))
			// A failed response does not prove whether the commit landed. Clear
			// any session-local delta, keep the sections in ConfigRemains, and
			// preserve the controller footprint so a retry can determine it.
			if rerr := controller.Call(ctx, "uci", "revert",
				map[string]any{"config": cfg}, nil); rerr != nil {
				rep.Errors = append(rep.Errors,
					fmt.Errorf("discard uncommitted revert of %s: %w", cfg, rerr))
			}
			return rep, fmt.Errorf("adoption: could not prove the revert of %s committed", cfg)
		}
		rep.Reverted = append(rep.Reverted, group...)
		rep.ConfigRemains = removeSections(rep.ConfigRemains, cfg)
	}
	if controller != nil {
		rep.ConfigRevertComplete = true
	}

	// ---- phase 2: remove the footprint ----
	//
	// Over the bootstrap channel, for the same reason it was installed that
	// way: ubus refuses both the rpcd config and the ACL directory even to
	// root. The login and the file go together in one command, so a device is
	// never left with a live credential pointing at access-groups that no
	// longer exist.
	if boot == nil {
		return rep, ErrOperatorRequired
	}
	if err := boot.RemoveFootprint(ctx, a.aclPath(), a.user()); err != nil {
		rep.Errors = append(rep.Errors, err)
	} else {
		rep.LoginRemoved = true
		rep.ACLRemoved = true
	}

	rep.FootprintRemains = !rep.LoginRemoved || !rep.ACLRemoved
	if len(rep.Errors) > 0 {
		return rep, fmt.Errorf("adoption: un-adopt completed with %d error(s)", len(rep.Errors))
	}
	return rep, nil
}

// ErrOperatorRequired signals that phase 2 needs the device admin credential.
var ErrOperatorRequired = errors.New("adoption: removing the login and ACL file " +
	"requires the device's operator credential — the controller deliberately " +
	"cannot remove itself")

// Section identifies one UCI section we own.
type Section struct {
	Config  string
	Section string
}

// UnadoptReport says exactly what was and was not removed, so the UI can show
// the residue instead of claiming a clean exit.
type UnadoptReport struct {
	Reverted             []Section
	ConfigRemains        []Section
	ConfigRevertComplete bool
	LoginRemoved         bool
	ACLRemoved           bool
	FootprintRemains     bool
	ACLPath              string
	User                 string
	Errors               []error
}

// Residue describes what is still on the device, for the fallback screen shown
// when the operator credential is unavailable.
func (r *UnadoptReport) Residue() []string {
	var out []string
	for _, s := range r.ConfigRemains {
		out = append(out, fmt.Sprintf("config section %s.%s", s.Config, s.Section))
	}
	if !r.ACLRemoved {
		out = append(out, r.ACLPath)
	}
	if !r.LoginRemoved {
		out = append(out, fmt.Sprintf("config login '%s' in /etc/config/rpcd", r.User))
	}
	return out
}

// CleanupCommands returns the exact stock-OpenWrt commands an operator can run
// over SSH for the footprint that remains. Values are validated before being
// placed in shell text; an unexpected internal value is omitted rather than
// turned into a root command.
func (r *UnadoptReport) CleanupCommands() []string {
	var out []string
	for _, cfg := range distinctConfigs(r.ConfigRemains) {
		if !identifier.MatchString(cfg) {
			continue
		}
		var verified []Section
		for _, s := range sectionsInConfig(r.ConfigRemains, cfg) {
			if !identifier.MatchString(s.Section) {
				continue
			}
			out = append(out, "uci -q delete "+cfg+"."+s.Section)
			verified = append(verified, s)
		}
		if len(verified) == 0 {
			continue
		}
		out = append(out, "uci commit "+cfg)
		for _, s := range verified {
			ref := cfg + "." + s.Section
			out = append(out, "uci -q get "+ref+" >/dev/null 2>&1 && echo 'ERROR: "+
				ref+" still present' || echo '"+ref+" gone'")
		}
	}
	if !r.LoginRemoved && identifier.MatchString(r.User) {
		out = append(out,
			"uci -q delete rpcd."+r.User,
			"uci commit rpcd",
			"uci -q get rpcd."+r.User+" >/dev/null 2>&1 && echo 'ERROR: login still present' || echo 'login gone'",
		)
	}
	if !r.ACLRemoved && safePath(r.ACLPath) {
		quoted := shellQuote(r.ACLPath)
		out = append(out,
			"rm -f "+quoted,
			"[ ! -e "+quoted+" ] && echo 'ACL gone' || echo 'ERROR: ACL still present'",
		)
	}
	return out
}

// ErrControllerRequired means phase 1 could not start. It is deliberately
// distinct from ErrOperatorRequired: supplying an SSH password cannot repair a
// controller session that cannot prove the managed configuration was removed.
var ErrControllerRequired = errors.New("adoption: the controller credential could not be used, so managed configuration was not reverted")

func writeFile(ctx context.Context, c *ubus.Client, path string, data []byte) error {
	// rpcd's file.write takes base64 when told to; that avoids any question of
	// how a JSON string round-trips through the shell-free path.
	return c.Call(ctx, "file", "write", map[string]any{
		"path":   path,
		"data":   base64.StdEncoding.EncodeToString(data),
		"base64": true,
		"mode":   0o644,
	}, nil)
}

// isMissing reports an error that means "already gone", which during un-adopt
// is success rather than failure.
func isMissing(err error) bool {
	var se *ubus.StatusError
	if errors.As(err, &se) {
		return se.Status == ubus.StatusNotFound || se.Status == ubus.StatusNoData
	}
	return false
}

func distinctConfigs(secs []Section) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range secs {
		if !seen[s.Config] {
			seen[s.Config] = true
			out = append(out, s.Config)
		}
	}
	return out
}

func sectionsInConfig(secs []Section, config string) []Section {
	var out []Section
	for _, s := range secs {
		if s.Config == config {
			out = append(out, s)
		}
	}
	return out
}

func removeSections(secs []Section, config string) []Section {
	out := secs[:0]
	for _, s := range secs {
		if s.Config != config {
			out = append(out, s)
		}
	}
	return out
}

func (a *Adopter) aclPath() string {
	if a.ACLPath != "" {
		return a.ACLPath
	}
	return DefaultACLPath
}

func (a *Adopter) user() string {
	if a.User != "" {
		return a.User
	}
	return DefaultUser
}

func (a *Adopter) groups() []string {
	if len(a.Groups) > 0 {
		return a.Groups
	}
	return ACLGroups
}

func (a *Adopter) newPassword() (string, error) {
	if a.NewPassword != nil {
		return a.NewPassword()
	}
	return randomPassword()
}
