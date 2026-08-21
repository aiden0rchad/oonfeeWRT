package adoption

import (
	"context"
	"errors"
)

// Bootstrap installs and removes the controller's device-side footprint.
//
// It exists because the obvious channel cannot do the job. ARCHITECTURE §6 said
// the ACL file is "written via file.write" over ubus, and §2 argued for ubus
// over SSH throughout. Measured on a stock OpenWrt 25.12.5 on 2026-08-14, as
// root:
//
//	uci.get  rpcd                            -> ubus status 6 (refused)
//	uci.set  rpcd.<login>                    -> ubus status 6 (refused)
//	file.write /usr/share/rpcd/acl.d/*.json  -> ubus status 6 (refused)
//	file.read  /etc/rc.local                 -> ubus status 0 (granted)
//
// Root over ubus is not root. rpcd's own ACL files bound what /ubus can do, and
// stock OpenWrt grants write access to neither the `rpcd` config nor the ACL
// directory — deliberately, because that is precisely the escalation a
// compromised web session would want. No access group anywhere on the device
// grants it, and adding one would itself require writing to the directory we
// cannot write to.
//
// So the footprint has to arrive by another channel, exactly once. Everything
// afterwards is ubus, and §2's argument for it is untouched: this is the
// bootstrap, not the transport.
type Bootstrap interface {
	// InstallACL writes the access-control file and verifies it landed intact.
	InstallACL(ctx context.Context, path string, content []byte) error

	// CreateLogin adds the rpcd login and commits it. passHash must already be
	// a crypt hash — rpcd rejects a plaintext password outright.
	CreateLogin(ctx context.Context, user, passHash string, groups []string) error

	// RemoveACL removes only the ACL file. Adoption uses this when login
	// creation fails: removing a login whose creation was not proved could
	// delete pre-existing state that does not belong to the controller.
	RemoveACL(ctx context.Context, aclPath string) error

	// RemoveFootprint deletes the login and the ACL file. Used by un-adopt.
	RemoveFootprint(ctx context.Context, aclPath, user string) error

	// Fingerprint identifies the channel's peer, for trust-on-first-use
	// pinning. Empty when the channel has no such notion.
	Fingerprint() string

	Close() error
}

// ErrNoBootstrap is returned when adoption is asked to run without a channel
// that can install the footprint.
var ErrNoBootstrap = errors.New("adoption: no bootstrap channel supplied, and " +
	"ubus cannot install the footprint — see the Bootstrap doc comment")
