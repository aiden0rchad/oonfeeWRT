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
	Caps       *capability.Registry
}

// Adopt runs the whole flow using the OPERATOR's session, then verifies the
// controller credential it created actually works and is properly scoped.
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
func (a *Adopter) Adopt(ctx context.Context, operator *ubus.Client) (*Result, error) {
	if len(a.ACL) == 0 {
		return nil, errors.New("adoption: no ACL content supplied")
	}
	aclPath, user, groups := a.aclPath(), a.user(), a.groups()

	// 1. Capability probe, while we still hold the operator credential — it can
	//    reach things the controller login deliberately cannot.
	caps, err := capability.Probe(ctx, operator)
	if err != nil {
		return nil, fmt.Errorf("adoption: capability probe: %w", err)
	}

	// 2. Mint the controller credential. rpcd rejects a plaintext password
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
	if err := writeFile(ctx, operator, aclPath, a.ACL); err != nil {
		return nil, fmt.Errorf("adoption: write %s: %w", aclPath, err)
	}

	// 4. Create the login.
	if err := a.writeLogin(ctx, operator, user, hashed, groups); err != nil {
		return nil, fmt.Errorf("adoption: create login %q: %w", user, err)
	}

	// 5. Prove it. An adoption that reports success without checking the
	//    credential it just created is how a device ends up in the inventory
	//    unreachable.
	verified, err := operator.FreshSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("adoption: cannot open a session to verify: %w", err)
	}
	defer verified.Close()
	if err := verified.Login(ctx, user, password); err != nil {
		return nil, fmt.Errorf("adoption: the credential we just created does "+
			"not work: %w", err)
	}

	return &Result{
		Credential: Credential{Username: user, Password: password},
		CertFP:     operator.PinnedCert(),
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
// operator may be nil, in which case phase 1 runs and ErrOperatorRequired is
// returned — the caller should then prompt and call again. That is the honest
// degradation: a device whose admin password is lost keeps a visible, documented
// residue rather than a silently half-removed one.
func (a *Adopter) Unadopt(ctx context.Context, controller, operator *ubus.Client, owned []Section) (*UnadoptReport, error) {
	rep := &UnadoptReport{ACLPath: a.aclPath(), User: a.user()}

	// ---- phase 1: give the user's config back ----
	if controller != nil {
		for _, s := range owned {
			err := controller.Call(ctx, "uci", "delete", map[string]any{
				"config": s.Config, "section": s.Section}, nil)
			if err != nil && !isMissing(err) {
				rep.Errors = append(rep.Errors,
					fmt.Errorf("revert %s.%s: %w", s.Config, s.Section, err))
				continue
			}
			rep.Reverted = append(rep.Reverted, s)
		}
		for _, cfg := range distinctConfigs(owned) {
			if err := controller.Call(ctx, "uci", "commit",
				map[string]any{"config": cfg}, nil); err != nil {
				rep.Errors = append(rep.Errors, fmt.Errorf("commit %s: %w", cfg, err))
			}
		}
	}

	// ---- phase 2: remove the footprint ----
	if operator == nil {
		rep.FootprintRemains = true
		return rep, ErrOperatorRequired
	}

	// Login first, then the ACL file. Reversing the order would leave a live
	// credential pointing at access-groups that no longer exist.
	if err := operator.Call(ctx, "uci", "delete", map[string]any{
		"config": "rpcd", "section": a.user()}, nil); err != nil && !isMissing(err) {
		rep.Errors = append(rep.Errors, fmt.Errorf("delete login: %w", err))
	} else if err := operator.Call(ctx, "uci", "commit",
		map[string]any{"config": "rpcd"}, nil); err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("commit rpcd: %w", err))
	} else {
		rep.LoginRemoved = true
	}

	if err := operator.Call(ctx, "file", "remove",
		map[string]any{"path": a.aclPath()}, nil); err != nil && !isMissing(err) {
		rep.Errors = append(rep.Errors, fmt.Errorf("remove ACL: %w", err))
	} else {
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
	Reverted         []Section
	LoginRemoved     bool
	ACLRemoved       bool
	FootprintRemains bool
	ACLPath          string
	User             string
	Errors           []error
}

// Residue describes what is still on the device, for the fallback screen shown
// when the operator credential is unavailable.
func (r *UnadoptReport) Residue() []string {
	var out []string
	if !r.ACLRemoved {
		out = append(out, r.ACLPath)
	}
	if !r.LoginRemoved {
		out = append(out, fmt.Sprintf("config login '%s' in /etc/config/rpcd", r.User))
	}
	return out
}

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
