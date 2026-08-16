//go:build integration

package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
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

	type ap struct{ mac, host, pass, name string }
	aps := []ap{
		{"60:38:e0:db:be:40", os.Getenv("OONFEE_AP1"), os.Getenv("OONFEE_AP1_PASS"), "wrt3200acm"},
		{"84:d8:1b:c5:19:35", os.Getenv("OONFEE_AP2"), os.Getenv("OONFEE_AP2_PASS"), "archer-c6"},
	}
	var ids []int64
	for _, a := range aps {
		blob, err := d.Keys.SealCredential(a.mac, "oonfeewrt", a.pass)
		if err != nil {
			t.Fatal(err)
		}
		at := int64(1)
		dev := &store.Device{MAC: a.mac, Host: a.host, Name: a.name, Scheme: "http",
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
