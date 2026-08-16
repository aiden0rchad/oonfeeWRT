package daemon

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/aiden0rchad/oonfeewrt/internal/discovery"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Connect opens a logged-in session to an adopted device, unsealing its
// credential on the way.
//
// This is the only path from the credential store to a live session, which
// keeps the two rules that go with it in one place: an un-adopted device has no
// credential to unseal, and a trust-on-first-use certificate pin that is not
// written back is not a pin at all.
func (d *Daemon) Connect(ctx context.Context, dev *store.Device) (*ubus.Client, error) {
	if dev == nil {
		return nil, fmt.Errorf("daemon: no device")
	}
	if !dev.Adopted() {
		return nil, fmt.Errorf("daemon: device %s (%s) is not adopted; there is no "+
			"controller credential for it yet", dev.Name, dev.MAC)
	}
	user, pass, err := d.Keys.OpenCredential(dev.MAC, dev.CredEnc)
	if err != nil {
		return nil, err
	}

	https := dev.Scheme == "https"
	c := ubus.New(ubus.Options{
		Host:       hostPort(dev, https),
		HTTPS:      https,
		PinnedCert: dev.CertFP,
	})
	if err := c.Login(ctx, user, pass); err != nil {
		c.Close()
		return nil, fmt.Errorf("daemon: log in to %s (%s): %w%s",
			dev.Name, dev.MAC, err, resetHint(ctx, dev, https))
	}

	// Persist a first-use pin. Without this the "first use" is every restart,
	// and the pin protects nothing — it would accept whatever certificate is
	// presented, forever, while looking like it was checking something.
	if https && dev.CertFP == "" {
		if fp := c.PinnedCert(); fp != "" {
			if err := d.Store.SetCertFP(ctx, dev.ID, fp); err != nil {
				d.Log.Error("could not persist the certificate pin; it will be "+
					"re-learned on the next connection", "device", dev.MAC, "err", err)
			} else {
				dev.CertFP = fp
				d.Log.Info("pinned device certificate on first use",
					"device", dev.MAC, "sha256", fp)
			}
		}
	}
	return c, nil
}

// resetHint distinguishes a device that has forgotten us from one that is
// simply not answering.
//
// A factory reset removes the rpcd login and the ACL file and leaves everything
// else about the device intact, so the controller is left holding a credential
// for a box that is on the network, healthy, and has never heard of it. Without
// this, that state reads as `PERMISSION_DENIED` once a minute forever, which is
// the same thing an operator sees when a password was rotated, when an ACL was
// narrowed, or when the keyring is wrong — four different problems behind one
// message, and only one of them has an obvious fix.
//
// The check is discovery's own probe: an unauthenticated `list`, with no
// credential, no session and no failed-login record. Reused rather than
// rewritten because a hand-rolled `session.list` would be refused by a stock
// ACL, and a refusal is not the question being asked.
//
// Only ever reached on a login failure, so it costs nothing in the normal case.
// It deliberately adds a HINT rather than replacing the error — the underlying
// failure is still what happened, and a caller matching on it must keep working.
func resetHint(ctx context.Context, dev *store.Device, https bool) string {
	// hostPort is the authority on where this device lives, because Host may
	// already carry a port — that is how the tests and any non-default
	// deployment address a device, and splitting dev.Host/dev.Port naively
	// probes port 80 on a host called "127.0.0.1:54321".
	scheme, port := "http", 80
	if https {
		scheme, port = "https", 443
	}
	host := hostPort(dev, https)
	if h, p, err := net.SplitHostPort(host); err == nil {
		if n, cerr := strconv.Atoi(p); cerr == nil {
			host, port = h, n
		}
	}
	cand := discovery.Probe(ctx, host, port, scheme, discovery.Options{})
	if cand.Verdict != discovery.VerdictOpenWrt {
		// It did not answer as an OpenWrt device either. That is unreachable or
		// wedged, and saying anything about adoption here would send an
		// operator to rebuild a device that just needs to come back.
		return ""
	}
	return "; the device is reachable and published its ubus objects, and simply " +
		"refused this credential — which is what a factory reset looks like, " +
		"because it removes the controller's login and ACL file and leaves " +
		"everything else intact. Un-adopt it (forcing, since there is no " +
		"footprint left to remove) and adopt it again"
}

// hostPort renders the device address, omitting the port when it is the scheme's
// default so the pinned URL matches what an operator would type.
func hostPort(dev *store.Device, https bool) string {
	port := dev.Port
	if port == 0 || (https && port == 443) || (!https && port == 80) {
		return dev.Host
	}
	return net.JoinHostPort(dev.Host, strconv.Itoa(port))
}
