package daemon

import (
	"context"
	"fmt"
	"net"
	"strconv"

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
		return nil, fmt.Errorf("daemon: log in to %s (%s): %w", dev.Name, dev.MAC, err)
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

// hostPort renders the device address, omitting the port when it is the scheme's
// default so the pinned URL matches what an operator would type.
func hostPort(dev *store.Device, https bool) string {
	port := dev.Port
	if port == 0 || (https && port == 443) || (!https && port == 80) {
		return dev.Host
	}
	return net.JoinHostPort(dev.Host, strconv.Itoa(port))
}
