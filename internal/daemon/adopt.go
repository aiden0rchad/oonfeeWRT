package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/deploy"
	"github.com/aiden0rchad/oonfeewrt/internal/adoption"
	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// adoptTimeout bounds the whole flow. The capability probe alone takes ~3 s on
// the reference device (it samples the survey twice, deliberately), and the
// writes and the verification login add more. Generous, because the alternative
// to a slow synchronous request here is a job queue, and a job queue for
// something an operator does a handful of times is the wrong trade.
const adoptTimeout = 90 * time.Second

// Adopt brings a device under management.
//
// The operator credential passed in is used for exactly one transaction and is
// never written anywhere: not to the database, not to the log, not into an
// error. What persists is the scoped login adoption creates, sealed under the
// device's MAC.
//
// The ordering matters and is adoption's, not ours: sign in with the operator
// credential, open the bootstrap channel, install the ACL and controller login,
// verify that scoped login, then probe through it. A device that ends up in the
// inventory unreachable is worse than one that never got added.
func (d *Daemon) Adopt(ctx context.Context, req api.AdoptRequest) (*api.AdoptResult, error) {
	ctx, cancel := context.WithTimeout(ctx, adoptTimeout)
	defer cancel()

	// Before touching the device: an unrecognised role is rejected here rather
	// than stored verbatim. It used to be stored, and compared later with an
	// exact string match, so "Gateway" adopted a router as an access point —
	// no addressing, no DHCP, no firewall zone, and nothing anywhere saying so.
	role, err := model.ParseRole(req.Role)
	if err != nil {
		return nil, err
	}
	functions, err := model.ParseDeviceFunctions(req.Functions, role)
	if err != nil {
		return nil, err
	}
	req.Role = string(functions.PrimaryRole())
	req.Functions = functions.Strings()

	https := req.Scheme == "https"
	endpoint, err := d.resolveWorkflowEndpoint(ctx, req.Host)
	if err != nil {
		return nil, err
	}
	host, err := endpoint.httpAuthority(req.Port, https)
	if err != nil {
		return nil, err
	}

	// One address is one device, checked before the device is touched at all.
	//
	// The identity check further down cannot cover this: it compares the MAC
	// this device reports, so a box whose identity ever changes — a renamed
	// bridge, an altered board file, an identifying interface that moved —
	// passes it, and the fleet quietly gains a second adopted row for one AP.
	// Every consequence is silent: polled twice against a budget of one request
	// a minute, listed twice on every screen, and reaching the 802.11k
	// distributor under two device ids. Observed for real, from hand-seeded
	// rows whose MACs did not match what adoption derives.
	//
	// Placed here rather than beside the MAC check because it needs nothing
	// from the device. Refusing after opening SSH and minting a session would
	// be a write-shaped conversation with a router we were never going to
	// adopt.
	releaseAdoption, err := d.beginAdoption(ctx, endpoint.inventoryHost(https), functions)
	if err != nil {
		return nil, err
	}
	defer releaseAdoption()

	operator := ubus.New(ubus.Options{Host: host, HTTPS: https, Timeout: 30 * time.Second})
	defer operator.Close()
	if err := operator.Login(ctx, req.Username, req.Password); err != nil {
		return nil, fmt.Errorf("could not sign in to %s: %w", req.Host, err)
	}

	// The bootstrap channel. Needed because ubus refuses the two writes that
	// adoption is FOR — see adoption.Bootstrap. Opened before anything is
	// changed so a device that cannot be bootstrapped is refused rather than
	// half-adopted.
	boot, err := adoption.DialSSH(ctx, adoption.SSHOptions{
		Host:       endpoint.sshAddress(),
		Username:   req.Username,
		Password:   req.Password,
		PrivateKey: []byte(req.PrivateKey),
		Timeout:    30 * time.Second,
	})
	if err != nil {
		return nil, sshBootstrapFailure(err)
	}
	defer boot.Close()

	// Does this device authenticate ANYTHING for that account? Measured on the
	// reference device 2026-08-14: a stock OpenWrt with no root password set
	// accepts root with an empty password, the correct password, and a wrong
	// one, because rpcd's `$p$root` looks the account up in /etc/shadow and an
	// empty entry matches everything.
	//
	// That is the device's configuration rather than a controller bug, and it is
	// not a reason to refuse — an operator may knowingly run that way on a
	// trusted LAN. But it means the credential just accepted proves nothing, and
	// a controller is far better placed to notice it than a person is.
	noPassword := acceptsAnyPassword(ctx, host, https, req.Username)

	// The identity, before anything is written. A device we cannot identify
	// cannot have a credential sealed to it, and finding that out after
	// creating a login on it would leave a footprint we could not attribute.
	mac, err := deviceMAC(ctx, operator)
	if err != nil {
		return nil, err
	}
	if existing, err := d.Store.DeviceByMAC(ctx, mac); err == nil && existing.Adopted() {
		return nil, fmt.Errorf("%s is already adopted as %q; un-adopt it first",
			mac, existing.Name)
	}

	a := &adoption.Adopter{
		ACL: deploy.ACL,
		VerifyController: func(verifyCtx context.Context, controller *ubus.Client) error {
			verifiedMAC, err := deviceMAC(verifyCtx, controller)
			if err != nil {
				return fmt.Errorf("could not re-read the device identity: %w", err)
			}
			if verifiedMAC != mac {
				return fmt.Errorf("the endpoint changed identity from %s to %s during adoption; "+
					"the device was not added to inventory", mac, verifiedMAC)
			}
			return nil
		},
	}
	res, err := a.Adopt(ctx, operator, boot)
	if err != nil {
		return nil, err
	}

	blob, err := d.Keys.SealCredential(mac, res.Credential.Username, res.Credential.Password)
	if err != nil {
		// The login exists on the device but we cannot store its password, so
		// say so plainly — the operator has to remove it by hand or re-adopt.
		return nil, fmt.Errorf("adopted %s but could not seal its credential, so "+
			"the login %q is now orphaned on the device: %w",
			mac, res.Credential.Username, err)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = res.Caps.Board.Model
	}
	if name == "" {
		name = mac
	}
	caps, err := json.Marshal(res.Caps)
	if err != nil {
		return nil, fmt.Errorf("adoption: encode capability record: %w", err)
	}

	now := time.Now().Unix()
	inventoryHost := endpoint.inventoryHost(https && res.CertFP != "")
	dev := &store.Device{
		MAC: mac, Host: inventoryHost, Port: effectiveDevicePort(req.Port, https), Name: name,
		Role: req.Role, Functions: req.Functions,
		CertFP: res.CertFP, HostKeyFP: res.HostKeyFP,
		AdoptedAt: &now, CredEnc: blob,
		Class: string(res.Caps.Class), CapsJSON: string(caps),
		FWRelease: res.Caps.Board.Release,
	}
	if https {
		dev.Scheme = "https"
	}
	if err := d.registerDevice(ctx, dev); err != nil {
		return nil, fmt.Errorf("adopted %s but could not record it: %w", mac, err)
	}
	// A new AP knows about no neighbours, and every existing AP knows nothing
	// about it. Both are fixed by the same cycle, and waiting up to fifteen
	// minutes for the periodic one would leave the fleet advertising 802.11k
	// and answering with a stale picture for exactly as long as someone is
	// standing there watching their new access point come up.
	d.nudgeNeighbours()

	id := dev.ID
	_ = d.Store.LogEvent(ctx, store.Event{
		DeviceID: &id, Category: "audit", Severity: "info", Event: "device.adopted",
		Detail: map[string]any{
			"mac": mac, "host": inventoryHost, "model": res.Caps.Board.Model,
			"class": string(res.Caps.Class), "login": res.Credential.Username,
			"functions": req.Functions,
		},
	})
	d.Log.Info("adopted device", "mac", mac, "host", inventoryHost,
		"model", res.Caps.Board.Model, "class", res.Caps.Class)

	out := &api.AdoptResult{
		DeviceID: dev.ID, MAC: mac, Name: name,
		Role: dev.Role, Functions: append([]string(nil), dev.Functions...),
		Model: res.Caps.Board.Model, Class: string(res.Caps.Class),
		Firmware: res.Caps.Board.Release, CertFP: res.CertFP,
		// The pin that was just recorded, so an operator standing at the device
		// can compare it against `ssh-keygen -lf` on the host key there. The
		// field has existed on this type since adoption was written and was
		// never filled in — a fingerprint nobody is shown is one nobody can
		// check, and this is the single moment both ends are known to be the
		// same box.
		HostKeyFP: res.HostKeyFP,
		Notes:     res.Caps.Notes,
	}
	for f, st := range res.Caps.Features {
		if st.Buildable() {
			out.Features = append(out.Features, string(f))
		}
	}
	for _, f := range res.Caps.Unobservable() {
		out.Unobservable = append(out.Unobservable, string(f))
	}
	for _, q := range res.Caps.Quirks {
		out.Quirks = append(out.Quirks, fmt.Sprintf("%s.%s — %s", q.Source, q.Field, q.Reason))
	}
	sortStrings(out.Features)
	// Does the role match what was actually found? A warning, never a refusal —
	// see roleFit. Silence is the failure mode being avoided: an old router
	// adopted as an access point that renders nothing, with no error to explain
	// it, is the likeliest disappointment when the point is repurposing
	// hardware nobody has catalogued.
	out.Warnings = append(out.Warnings, functionFit(functions, res.Caps)...)
	if noPassword {
		out.Warnings = append(out.Warnings,
			passwordlessAccountWarning(req.Host, req.Username))
		d.Log.Warn("adopted a device that accepts any password",
			"mac", mac, "host", req.Host, "user", req.Username)
		_ = d.Store.LogEvent(ctx, store.Event{
			DeviceID: &id, Category: "security", Severity: "warning",
			Event:  "device.no_password",
			Detail: map[string]any{"mac": mac, "user": req.Username},
		})
	}
	return out, nil
}

func sshBootstrapFailure(err error) error {
	return fmt.Errorf("%w\n\nAdoption needs SSH once, to install the "+
		"access-control file and the controller's login. Neither can be done "+
		"over ubus: stock OpenWrt refuses both even to root, which is what "+
		"stops a compromised web session widening its own permissions. Check "+
		"that Dropbear is running and reachable on port 22. If SSH password "+
		"authentication is disabled, paste an SSH private key in the optional "+
		"field; the device password is still required for the ubus sign-in. "+
		"Everything after adoption uses ubus alone", err)
}

func passwordlessAccountWarning(host, user string) string {
	target := shellCommandArg(user + "@" + host)
	return fmt.Sprintf(
		"%s accepts ANY password for %q, so it has no password set for that "+
			"account. Anyone who can reach it can administer it. Fix it now: run "+
			"`ssh -t %s passwd`, or in LuCI open System → Administration → Router "+
			"Password. The controller's own login is password-protected regardless. "+
			"The controller will not change /etc/shadow or set this device password "+
			"for you.", host, user, target)
}

func shellCommandArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// acceptsAnyPassword reports that the device authenticates a account with a
// password that is certainly wrong.
//
// One extra login, at adoption only, and read-only. A false answer here is
// harmless — it only suppresses a warning — so any error is treated as "no".
func acceptsAnyPassword(ctx context.Context, host string, https bool, user string) bool {
	probe := ubus.New(ubus.Options{Host: host, HTTPS: https, Timeout: 15 * time.Second})
	defer probe.Close()
	wrong := "oonfeewrt-not-a-password-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	return probe.Login(ctx, user, wrong) == nil
}

// Unadopt removes the controller from a device, in the two phases the design
// requires.
//
// Phase 1 runs under the CONTROLLER credential and gives the user's config
// back. Phase 2 needs the OPERATOR credential and cannot be done with our own:
// write access to our ACL file is write access to arbitrary rpcd scope after
// the next login.
//
// The inventory row is deleted only when the device is actually clean, or when
// the caller explicitly forces it. Deleting the row while a login and an ACL
// file remain on the device would lose the only record of what needs removing.
func (d *Daemon) Unadopt(ctx context.Context, req api.UnadoptRequest) (*api.UnadoptResult, error) {
	ctx, cancel := context.WithTimeout(ctx, adoptTimeout)
	defer cancel()

	release, err := d.deviceOps.acquire(ctx, req.DeviceID)
	if err != nil {
		return nil, err
	}
	defer release()

	// Read everything only after admission. An apply that won the gate may have
	// changed the final ownership set; this operation must clean that set, not a
	// snapshot captured while the apply was still running.
	dev, err := d.Store.DeviceByID(ctx, req.DeviceID)
	if err != nil {
		return nil, err
	}
	capabilities, err := d.Store.CapabilityInstalls(ctx, dev.ID)
	if err != nil {
		return nil, err
	}
	if len(capabilities) != 0 {
		return nil, fmt.Errorf("daemon: remove the controller-installed %s capability before un-adopting %s; its durable rollback record cannot be discarded", capabilities[0].Capability, dev.Name)
	}
	owned, err := d.ownedSections(ctx, dev.ID)
	if err != nil {
		return nil, err
	}
	out := &api.UnadoptResult{}

	// Freeze the existing poller rather than removing it. A failed or partial
	// un-adopt keeps the same device identity, along with every WebSocket focus
	// lease attached to that poller. Removing and re-adding it would leave those
	// subscriptions pointing at a stopped poller until the browser reconnects.
	resumePolling := func() {}
	if collector := d.collectorRef(); collector != nil {
		resumePolling = collector.Quiesce(dev.ID)
	}
	defer resumePolling()

	var controller *ubus.Client
	if dev.Adopted() {
		if c, err := d.Connect(ctx, dev); err == nil {
			controller = c
			defer c.Close()
		} else if len(owned) > 0 {
			out.Errors = append(out.Errors,
				fmt.Sprintf("could not sign in with the controller credential: %v", err))
		}
	}

	// Phase 2 goes over SSH, because that is the only channel that can remove
	// what only SSH could install.
	//
	// Pinned to the host key recorded at adoption. This is the dial the pin
	// exists for: the operator has just typed their administrator password into
	// the un-adopt panel, and it is about to be offered to whatever answers on
	// port 22 at the stored address. Adoption itself is genuinely first use and
	// stays unpinned — there is nothing to check against, and refusing to adopt
	// until someone has collected fingerprints by hand is a worse answer.
	//
	// dev.HostKeyFP is empty for devices adopted before the pin existed, which
	// makes this an unpinned first use for them too; the successful dial below
	// records what it saw so the NEXT one is checked.
	var boot adoption.Bootstrap
	// Do not even open the phase-2 channel when phase 1 has no controller
	// session. In particular, an SSH credential cannot turn an unproved config
	// hand-back into a safe footprint removal.
	if (controller != nil || len(owned) == 0) && req.Username != "" {
		b, err := adoption.DialSSH(ctx, adoption.SSHOptions{
			Host: dev.Host, Username: req.Username, Password: req.Password,
			PrivateKey: []byte(req.PrivateKey), HostKeyFP: dev.HostKeyFP,
			Timeout: 30 * time.Second,
		})
		switch {
		case err != nil && req.Force:
			// Force means "take it out of the inventory even if the device
			// cannot be reached", and a refused host key is one way not to
			// reach it — the commonest reason a host key changes is a reflash,
			// which also wipes the footprint we came to remove. Refusing here
			// would make a reflashed device permanently un-removable, so the
			// failure is reported and phase 2 is skipped rather than the whole
			// removal being abandoned. The residue is still reported honestly:
			// with no SSH session, RemoveFootprint never runs and the report
			// says the login and ACL remain.
			out.Errors = append(out.Errors, fmt.Sprintf(
				"could not open an SSH session with the supplied administrator "+
					"credential, and removal was forced, so the controller's "+
					"footprint was NOT removed from the device: %v", err))
			d.Log.Warn("forced un-adopt could not open SSH", "mac", dev.MAC,
				"host", dev.Host, "err", err)
		case err != nil:
			return nil, fmt.Errorf("could not open an SSH session with the "+
				"supplied administrator credential: %w", err)
		default:
			boot = b
			defer b.Close()
			// Learn the key if this device predates the pin. First use only —
			// SetHostKeyFP refuses to replace one, so a device that is already
			// pinned cannot be re-pinned by connecting to it, and the dial
			// above has in any case already checked it.
			if dev.HostKeyFP == "" {
				if fp := b.Fingerprint(); fp != "" {
					if err := d.Store.SetHostKeyFP(ctx, dev.ID, fp); err != nil {
						d.Log.Warn("could not pin the device's SSH host key",
							"mac", dev.MAC, "err", err)
					}
				}
			}
		}
	}

	a := &adoption.Adopter{ACL: deploy.ACL}
	var rep *adoption.UnadoptReport
	var uerr error
	if controller == nil {
		// A typed nil *ubus.Client becomes a non-nil interface and would panic
		// on the first Call. Pass an actual nil to preserve the phase-1 state.
		rep, uerr = a.Unadopt(ctx, nil, boot, owned)
	} else {
		rep, uerr = a.Unadopt(ctx, controller, boot, owned)
	}
	if rep != nil {
		out.RevertedSections = len(rep.Reverted)
		out.ConfigRevertComplete = rep.ConfigRevertComplete
		for _, s := range rep.ConfigRemains {
			out.ConfigRemains = append(out.ConfigRemains, s.Config+"."+s.Section)
		}
		out.LoginRemoved = rep.LoginRemoved
		out.ACLRemoved = rep.ACLRemoved
		out.FootprintRemains = rep.FootprintRemains
		out.Residue = rep.Residue()
		out.CleanupCommands = rep.CleanupCommands()
		for _, e := range rep.Errors {
			out.Errors = append(out.Errors, e.Error())
		}
	}
	// Force is checked BEFORE this early return, not after.
	//
	// It used to sit below, next to the clean-removal case, which made it dead
	// code in the only situation it exists for. Force is documented as "remove
	// it from the inventory even if the device could not be reached at all —
	// for hardware that is gone for good", and a device that is gone for good
	// always fails phase 2: no controller session, no SSH, so ErrOperatorRequired
	// every time. The flag returned above without ever being read, and the
	// caller got a 409 telling them to supply a credential for a router that no
	// longer exists.
	if errors.Is(uerr, adoption.ErrOperatorRequired) && !req.Force {
		out.NeedsOperator = true
		return out, api.ErrOperatorRequired
	}

	// Clean, or the caller accepted the residue.
	if (rep != nil && rep.ConfigRevertComplete && !rep.FootprintRemains) || req.Force {
		// Logged before deletion so it is initially attributed to the exact row.
		// deleteDevice then detaches the reusable integer id while the detail's
		// stable MAC keeps the historical identity.
		id := dev.ID
		_ = d.Store.LogEvent(ctx, store.Event{
			DeviceID: &id, Category: "audit", Severity: "info",
			Event: "device.unadopted",
			Detail: map[string]any{
				"mac": dev.MAC, "footprint_remains": out.FootprintRemains,
				"config_revert_complete": out.ConfigRevertComplete,
				"config_remains":         len(out.ConfigRemains),
				"reverted_sections":      out.RevertedSections, "forced": req.Force,
			},
		})
		if err := d.deleteDevice(ctx, dev.ID); err != nil {
			return out, err
		}
		out.Removed = true
		if out.FootprintRemains || len(out.ConfigRemains) > 0 {
			// Forced. The inventory row was the only record of what is still on
			// that device, and it has just been deleted — so this warning and
			// the residue in the response are the last copy of it.
			d.Log.Warn("forced removal: the device keeps a footprint and the "+
				"controller no longer has a record of it", "mac", dev.MAC,
				"host", dev.Host, "residue", out.Residue)
		} else {
			d.Log.Info("removed device from the inventory", "mac", dev.MAC)
		}
		// The removed AP is still in every other AP's neighbour list, telling
		// clients to consider roaming to something that is no longer part of
		// this network. Removal only happens on a cycle that read the whole
		// fleet (roaming.Union), and this one will: the row is gone, so there
		// is no unreachable device left to make the cycle incomplete.
		d.nudgeNeighbours()
	}
	if uerr != nil && !errors.Is(uerr, adoption.ErrOperatorRequired) &&
		!(req.Force && out.Removed) {
		return out, uerr
	}
	return out, nil
}

// ownedSections lists the UCI sections we wrote, so un-adopt can revert exactly
// those and nothing else.
func (d *Daemon) ownedSections(ctx context.Context, deviceID int64) ([]adoption.Section, error) {
	rows, err := d.Store.SQL().QueryContext(ctx,
		`SELECT config, section FROM owned_sections WHERE device_id=? ORDER BY config, section`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("daemon: list owned sections: %w", err)
	}
	defer rows.Close()
	var out []adoption.Section
	for rows.Next() {
		var s adoption.Section
		if err := rows.Scan(&s.Config, &s.Section); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// deleteDevice removes the inventory row and everything keyed to it.
//
// The series cascade takes care of itself; the rollup tables carry no foreign
// key, so their orphans are collected explicitly after the row is gone.
func (d *Daemon) deleteDevice(ctx context.Context, id int64) error {
	collector := d.collectorRef()
	resumePolling := func() {}
	if collector != nil {
		// This is an emission boundary for direct callers too. Unadopt already
		// quiesces during remote cleanup; nesting is reference-counted.
		resumePolling = collector.Quiesce(id)
	}
	defer resumePolling()

	d.telemetryLifecycle.Lock()
	defer d.telemetryLifecycle.Unlock()
	tx, err := d.Store.SQL().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("daemon: begin device %d deletion: %w", id, err)
	}
	defer tx.Rollback() //nolint:errcheck
	var deviceMAC string
	if err := tx.QueryRowContext(ctx, `SELECT mac FROM devices WHERE id=?`, id).Scan(&deviceMAC); err != nil {
		return fmt.Errorf("daemon: load device %d identity for deletion: %w", id, err)
	}
	parsedMAC, err := net.ParseMAC(deviceMAC)
	if err != nil {
		return fmt.Errorf("daemon: parse device %d identity for deletion: %w", id, err)
	}
	closedAt := time.Now().UnixMilli()
	deviceNode := "device:" + parsedMAC.String()
	if _, err := tx.ExecContext(ctx, `
UPDATE topology_edges
   SET valid_to=CASE WHEN last_seen>? THEN last_seen ELSE ? END
 WHERE valid_to IS NULL AND (child_node=? OR parent_node=? OR parent_device_id=?)`,
		closedAt, closedAt, deviceNode, deviceNode, id); err != nil {
		return fmt.Errorf("daemon: close device %d topology history: %w", id, err)
	}
	// Events intentionally survive un-adoption, but devices use a reusable
	// INTEGER PRIMARY KEY. Detach the historical provenance before deleting the
	// row so a later device cannot inherit an old router's log/audit history.
	if _, err := tx.ExecContext(ctx, `UPDATE events SET device_id=NULL WHERE device_id=?`, id); err != nil {
		return fmt.Errorf("daemon: detach device %d event history: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE id=?`, id); err != nil {
		return fmt.Errorf("daemon: delete device %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("daemon: commit device %d deletion: %w", id, err)
	}
	if collector != nil {
		collector.Remove(id)
	}
	// Keep the current gate entry: un-adopt still holds it and queued callers
	// retain its pointer. Bumping its generation makes every waiter that joined
	// before this identity boundary fail after admission, while operations that
	// begin against a later device using the same ID get the new generation.
	d.deviceOps.invalidate(id)
	d.purgeDeviceStateLocked(id)
	// Drop the ownership claims with the device. ForgetOwned exists for exactly
	// this and was never called from anywhere, so every un-adopted device left
	// its claims behind forever.
	//
	// Not merely untidy: sqlite reuses a freed INTEGER PRIMARY KEY, so the next
	// device adopted takes the id of one that was removed and inherits its
	// claims. A later un-adopt would then try to revert sections that device
	// never had, and report a footprint from another router's config.
	// Logged, not returned. The device row is already gone by this point, so
	// the removal HAPPENED — returning an error here would report a failure for
	// something that succeeded, and the caller would tell an operator to try
	// again on a device that is no longer in the inventory.
	//
	// Both tables cascade on the delete anyway, and SweepOrphans catches
	// whatever a failure here leaves behind, so the cost of continuing is a row
	// that the next maintenance tick removes.
	if err := d.Store.ForgetOwned(ctx, id); err != nil {
		d.Log.Warn("could not drop ownership claims for a removed device; the "+
			"orphan sweep will clear them", "device", id, "err", err)
	}
	if err := d.Store.ForgetForeignNotes(ctx, id); err != nil {
		d.Log.Warn("could not drop foreign-SSID notes for a removed device; the "+
			"orphan sweep will clear them", "device", id, "err", err)
	}
	if err := d.Store.SweepOrphans(ctx); err != nil {
		d.Log.Error("could not sweep telemetry of the removed device", "err", err)
	}
	return nil
}

// deviceMAC reads the device's stable identity.
//
// The LAN bridge's MAC is OpenWrt's conventional identity for a box, and it is
// what the credential is sealed against — so it is read before anything is
// written, and a device that will not answer is refused rather than adopted
// under a name we would have to invent.
func deviceMAC(ctx context.Context, c *ubus.Client) (string, error) {
	var devices map[string]struct {
		MAC string `json:"macaddr"`
	}
	if err := c.Call(ctx, "network.device", "status", nil, &devices); err != nil {
		return "", fmt.Errorf("could not read the device's interfaces: %w", err)
	}
	// br-lan first, then any ethernet, then anything with a MAC that is not
	// loopback. Deterministic order so the same device always yields the same
	// identity.
	for _, prefer := range []string{"br-lan", "eth0", "eth1"} {
		if v, ok := devices[prefer]; ok && validMAC(v.MAC) {
			return strings.ToLower(v.MAC), nil
		}
	}
	best := ""
	for name, v := range devices {
		if name == "lo" || !validMAC(v.MAC) {
			continue
		}
		if best == "" || name < best {
			best = name
		}
	}
	if best != "" {
		return strings.ToLower(devices[best].MAC), nil
	}
	return "", errors.New("the device reported no usable MAC address, so there is " +
		"nothing stable to identify it by")
}

func validMAC(s string) bool {
	if s == "" || s == "00:00:00:00:00:00" {
		return false
	}
	_, err := net.ParseMAC(s)
	return err == nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
