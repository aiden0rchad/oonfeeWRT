//go:build integration

package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// Moves a WLAN off one device by taking it out of the AP group, so any client
// on it has to choose another AP. Used to put a client on the WRT3200ACM, which
// has never carried one since the factory reset — the single condition every
// pre-reset wedge shared.
func TestZZMoveWLANOffDevice(t *testing.T) {
	if os.Getenv("OONFEE_MOVE") == "" {
		t.Skip("set OONFEE_MOVE=<deviceID> to drop, or OONFEE_MOVE=restore")
	}
	ctx := context.Background()
	cfg := testConfig(t, "operator passphrase")
	cfg.DataDir = os.Getenv("OONFEE_SEED_DIR")
	cfg.PassphraseFile = os.Getenv("OONFEE_SEED_PASSFILE")
	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	site, err := d.Store.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	grp := site.Groups[0]
	want := os.Getenv("OONFEE_MOVE")

	if want == "restore" {
		grp.DeviceIDs = []int64{4, 5}
	} else {
		var keep []int64
		for _, id := range grp.DeviceIDs {
			if id != 4 {
				keep = append(keep, id)
			}
		}
		grp.DeviceIDs = keep
	}
	if err := d.Store.SaveGroup(ctx, &grp); err != nil {
		t.Fatal(err)
	}
	res, err := d.ApplySite(ctx, api.ApplyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Devices {
		t.Logf("APPLY %s -> %s (%d changes)", r.Name, r.Outcome, r.Changes)
	}
	_ = model.Band5G
}
