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
// The ordering matters and is adoption's, not ours: probe while we still hold
// the operator credential (it reaches things the controller login deliberately
// cannot), then write the ACL, then the login, then verify the login works. A
// device that ends up in the inventory unreachable is worse than one that never
// got added.
func (d *Daemon) Adopt(ctx context.Context, req api.AdoptRequest) (*api.AdoptResult, error) {
	ctx, cancel := context.WithTimeout(ctx, adoptTimeout)
	defer cancel()

	https := req.Scheme == "https"
	host := req.Host
	if req.Port > 0 && !(https && req.Port == 443) && !(!https && req.Port == 80) {
		host = net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	}

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
		Host:       req.Host,
		Username:   req.Username,
		Password:   req.Password,
		PrivateKey: []byte(req.PrivateKey),
		Timeout:    30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("%w\n\nAdoption needs SSH once, to install the "+
			"access-control file and the controller's login. Neither can be done "+
			"over ubus: stock OpenWrt refuses both even to root, which is what "+
			"stops a compromised web session widening its own permissions. "+
			"Everything after adoption uses ubus alone", err)
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

	a := &adoption.Adopter{ACL: deploy.ACL}
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
	dev := &store.Device{
		MAC: mac, Host: req.Host, Port: req.Port, Name: name,
		Role: req.Role, CertFP: res.CertFP,
		AdoptedAt: &now, CredEnc: blob,
		Class: string(res.Caps.Class), CapsJSON: string(caps),
		FWRelease: res.Caps.Board.Release,
	}
	if https {
		dev.Scheme = "https"
	}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		return nil, fmt.Errorf("adopted %s but could not record it: %w", mac, err)
	}
	d.Track(dev)

	id := dev.ID
	_ = d.Store.LogEvent(ctx, store.Event{
		DeviceID: &id, Category: "audit", Severity: "info", Event: "device.adopted",
		Detail: map[string]any{
			"mac": mac, "host": req.Host, "model": res.Caps.Board.Model,
			"class": string(res.Caps.Class), "login": res.Credential.Username,
		},
	})
	d.Log.Info("adopted device", "mac", mac, "host", req.Host,
		"model", res.Caps.Board.Model, "class", res.Caps.Class)

	out := &api.AdoptResult{
		DeviceID: dev.ID, MAC: mac, Name: name,
		Model: res.Caps.Board.Model, Class: string(res.Caps.Class),
		Firmware: res.Caps.Board.Release, CertFP: res.CertFP,
		Notes: res.Caps.Notes,
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
	if noPassword {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%s accepts ANY password for %q, so it has no password set for that "+
				"account. Anyone who can reach it can administer it. The controller's "+
				"own login is password-protected regardless, but this should be fixed "+
				"on the device.", req.Host, req.Username))
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

	dev, err := d.Store.DeviceByID(ctx, req.DeviceID)
	if err != nil {
		return nil, err
	}
	out := &api.UnadoptResult{}

	// Stop polling first, so nothing re-opens a session while we take it apart.
	d.Untrack(dev.ID)

	var controller *ubus.Client
	if dev.Adopted() {
		if c, err := d.Connect(ctx, dev); err == nil {
			controller = c
			defer c.Close()
		} else {
			out.Errors = append(out.Errors,
				fmt.Sprintf("could not sign in with the controller credential: %v", err))
		}
	}

	// Phase 2 goes over SSH, because that is the only channel that can remove
	// what only SSH could install.
	var boot adoption.Bootstrap
	if req.Username != "" {
		b, err := adoption.DialSSH(ctx, adoption.SSHOptions{
			Host: dev.Host, Username: req.Username, Password: req.Password,
			PrivateKey: []byte(req.PrivateKey), Timeout: 30 * time.Second,
		})
		if err != nil {
			return nil, fmt.Errorf("could not open an SSH session with the "+
				"supplied administrator credential: %w", err)
		}
		boot = b
		defer b.Close()
	}

	owned, err := d.ownedSections(ctx, dev.ID)
	if err != nil {
		return nil, err
	}
	a := &adoption.Adopter{ACL: deploy.ACL}
	rep, uerr := a.Unadopt(ctx, controller, boot, owned)
	if rep != nil {
		out.RevertedSections = len(rep.Reverted)
		out.LoginRemoved = rep.LoginRemoved
		out.ACLRemoved = rep.ACLRemoved
		out.FootprintRemains = rep.FootprintRemains
		out.Residue = rep.Residue()
		for _, e := range rep.Errors {
			out.Errors = append(out.Errors, e.Error())
		}
	}
	if errors.Is(uerr, adoption.ErrOperatorRequired) {
		out.NeedsOperator = true
		return out, api.ErrOperatorRequired
	}

	// Clean, or the caller accepted the residue.
	if (rep != nil && !rep.FootprintRemains) || req.Force {
		if err := d.deleteDevice(ctx, dev.ID); err != nil {
			return out, err
		}
		out.Removed = true
		d.Log.Info("removed device from the inventory", "mac", dev.MAC,
			"footprint_remains", out.FootprintRemains)
	}
	if uerr != nil && !errors.Is(uerr, adoption.ErrOperatorRequired) {
		return out, uerr
	}
	return out, nil
}

// ownedSections lists the UCI sections we wrote, so un-adopt can revert exactly
// those and nothing else.
func (d *Daemon) ownedSections(ctx context.Context, deviceID int64) ([]adoption.Section, error) {
	rows, err := d.Store.SQL().QueryContext(ctx,
		`SELECT config, section FROM owned_sections WHERE device_id=?`, deviceID)
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
// key, so their orphans are collected explicitly. Untrack already does that,
// but this runs after the row is gone, which is the only point at which the
// rows are actually orphaned.
func (d *Daemon) deleteDevice(ctx context.Context, id int64) error {
	if _, err := d.Store.SQL().ExecContext(ctx, `DELETE FROM devices WHERE id=?`, id); err != nil {
		return fmt.Errorf("daemon: delete device %d: %w", id, err)
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
