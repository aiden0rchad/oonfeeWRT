//go:build integration

package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Sets up a real roaming WLAN across both APs, through the controller.
//
// Not an assertion — a setup helper, like TestSeedLiveInventory. It exists so
// the two-AP roaming test is reproducible rather than a sequence of uci
// commands someone has to remember.
//
//	OONFEE_ROAM=1 OONFEE_SEED_DIR=/path OONFEE_SEED_PASSFILE=/path/pass \
//	OONFEE_AP1=192.168.1.1 OONFEE_AP1_PASS=... \
//	OONFEE_AP2=192.168.1.2 OONFEE_AP2_PASS=... \
//	OONFEE_WLAN_SSID=... OONFEE_WLAN_KEY=... \
//	go test -tags=integration ./internal/daemon/ -run TestZZSetupRoaming -v
func TestZZSetupRoaming(t *testing.T) {
	if os.Getenv("OONFEE_ROAM") != "1" {
		t.Skip("set OONFEE_ROAM=1")
	}
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.DataDir = os.Getenv("OONFEE_SEED_DIR")
	cfg.Listen = "127.0.0.1:0"
	cfg.PassphraseFile = os.Getenv("OONFEE_SEED_PASSFILE")

	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// The MAC is read from the device rather than written down here.
	//
	// It used to be a literal, and the literals were wrong: one was the box's
	// WAN-side address and the other was a radio's, while adoption identifies a
	// device by its LAN bridge. A seeded row therefore carried a different
	// identity than a real adoption of the same box would, and the two coexisted
	// in the inventory as two devices — one physical AP polled twice, against a
	// budget of one request a minute.
	type ap struct{ host, pass, name string }
	aps := []ap{
		{os.Getenv("OONFEE_AP1"), os.Getenv("OONFEE_AP1_PASS"), "wrt3200acm"},
		{os.Getenv("OONFEE_AP2"), os.Getenv("OONFEE_AP2_PASS"), "archer-c6"},
	}
	var ids []int64
	for _, a := range aps {
		mac, err := macOf(ctx, a.host, a.pass)
		if err != nil {
			t.Fatalf("%s: %v", a.name, err)
		}
		blob, err := d.Keys.SealCredential(mac, "oonfeewrt", a.pass)
		if err != nil {
			t.Fatal(err)
		}
		at := int64(1)
		dev := &store.Device{MAC: mac, Host: a.host, Name: a.name, Scheme: "http",
			Role: string(model.RoleAP), AdoptedAt: &at, CredEnc: blob}
		if err := d.Store.UpsertDevice(ctx, dev); err != nil {
			t.Fatal(err)
		}
		res, err := d.Reprobe(ctx, dev.ID)
		if err != nil {
			t.Fatalf("%s probe: %v", a.name, err)
		}
		t.Logf("%s (%s): %s", a.name, a.host, res.Summary)
		ids = append(ids, dev.ID)
	}

	net := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	if err := d.Store.SaveNetwork(ctx, net); err != nil {
		t.Fatal(err)
	}
	grp := &model.APGroup{Name: "all-aps", DeviceIDs: ids}
	if err := d.Store.SaveGroup(ctx, grp); err != nil {
		t.Fatal(err)
	}
	w := &model.WLAN{
		SSID: os.Getenv("OONFEE_WLAN_SSID"), NetworkID: net.ID, GroupID: grp.ID,
		Bands: []model.Band{model.Band2G, model.Band5G},
		Security: model.Security{Mode: model.SecPSK2, Key: os.Getenv("OONFEE_WLAN_KEY"),
			PMF: model.PMFOptional},
		// FT on WPA2-PSK needs the compatibility acknowledgment: it breaks some
		// older clients, which is why the renderer refuses it silently otherwise.
		Roaming: model.Roaming{FT: true, FTOverDS: true, KV: true, FTWithPSK2: true},
		Enabled: true,
	}
	if err := d.Store.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}

	prev, err := d.Preview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range prev.Devices {
		t.Logf("preview %s: %d change(s) blocked=%v err=%q",
			row.Name, len(row.Changes), row.Blocked, row.Error)
		for _, om := range row.Omitted {
			t.Logf("   omitted: %s", om)
		}
	}
	res, err := d.ApplySite(ctx, api.ApplyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Devices {
		t.Logf("APPLY %s -> %s (%d changes) %s", r.Name, r.Outcome, r.Changes, r.Reason)
	}
	if res.Aborted {
		t.Fatalf("apply aborted after %s", res.AbortedAfter)
	}
}

// macOf asks the device for the identity adoption would give it.
//
// Deliberately the same function the real path uses. A helper that computed the
// identity its own way would produce rows that look adopted and do not match
// what adopting the same box actually yields — which is how one physical AP
// ends up in the inventory twice.
func macOf(ctx context.Context, host, pass string) (string, error) {
	c := ubus.New(ubus.Options{Host: host})
	defer c.Close()
	if err := c.Login(ctx, "oonfeewrt", pass); err != nil {
		return "", err
	}
	return deviceMAC(ctx, c)
}
