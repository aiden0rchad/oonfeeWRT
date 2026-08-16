package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// These run the whole distribution against tools/mock_ubus.py, whose neighbour
// replies are copied from captures of both reference devices.
//
// The pure rules live in internal/roaming with their own tests; what is checked
// here is everything in between — that the positional reply shape decodes, that
// the batched read and the batched write line up, that a converged fleet is
// left alone, and that the gates skip for the right reasons. All of that is
// wiring, and wiring is where this project's mock-green bugs have lived.

func startMock(t *testing.T) string {
	t.Helper()
	root, err := repoRootFrom(t)
	if err != nil {
		t.Skipf("cannot locate the repo root: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	port := addr[strings.LastIndex(addr, ":")+1:]
	_ = ln.Close()

	cmd := exec.Command("python3", filepath.Join(root, "tools", "mock_ubus.py"),
		"--port", port)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start the mock: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mock did not come up on %s", addr)
	return ""
}

func repoRootFrom(t *testing.T) (string, error) {
	t.Helper()
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", fmt.Errorf("no go.mod above the working directory")
}

// seedAP registers one adopted device pointed at the mock, with a capability
// record that says it can take neighbour lists.
func seedAP(t *testing.T, d *Daemon, mac, name, host string, feat capability.State) *store.Device {
	t.Helper()
	ctx := context.Background()

	blob, err := d.Keys.SealCredential(mac, "root", "good")
	if err != nil {
		t.Fatal(err)
	}
	caps := capability.NewRegistry()
	caps.Board.Release = "OpenWrt 25.12.5"
	caps.Set(capability.FeatNeighborReport, feat)
	blobCaps, _ := json.Marshal(caps)

	at := int64(1)
	dev := &store.Device{
		MAC: mac, Host: host, Name: name, Scheme: "http", Role: "ap",
		AdoptedAt: &at, CredEnc: blob, CapsJSON: string(blobCaps),
	}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	return dev
}

// seedRoamingWLAN puts one 802.11k WLAN in the site model, matching the SSID
// the mock's BSSes carry.
func seedRoamingWLAN(t *testing.T, d *Daemon, ssid string, kv bool) {
	t.Helper()
	ctx := context.Background()

	net := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	if err := d.Store.SaveNetwork(ctx, net); err != nil {
		t.Fatal(err)
	}
	grp := &model.APGroup{Name: "all-aps"}
	if err := d.Store.SaveGroup(ctx, grp); err != nil {
		t.Fatal(err)
	}
	w := &model.WLAN{
		SSID: ssid, NetworkID: net.ID, GroupID: grp.ID, Enabled: true,
		Bands:    []model.Band{model.Band2G, model.Band5G},
		Security: model.Security{Mode: model.SecPSK2, Key: "not-used-here"},
		Roaming:  model.Roaming{FT: true, KV: kv, FTWithPSK2: true},
	}
	if err := d.Store.SaveWLAN(ctx, w); err != nil {
		t.Fatal(err)
	}
}

func openDaemon(t *testing.T) *Daemon {
	t.Helper()
	d, err := Open(context.Background(), testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// The whole path, and then the property that makes it affordable: running it
// again against an unchanged fleet must contact nothing.
func TestDistributeNeighboursFillsAndThenConverges(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	seedRoamingWLAN(t, d, "OpenWrt", true)
	seedAP(t, d, "60:38:e0:00:0b:01", "ap-one", addr, capability.Present)

	ctx := context.Background()
	first, err := d.DistributeNeighbours(ctx)
	if err != nil {
		t.Fatalf("DistributeNeighbours: %v", err)
	}
	if first.Updated != 2 {
		t.Fatalf("want both BSSes updated, got %d updated / %d unchanged: %+v",
			first.Updated, first.Unchanged, first.Devices)
	}
	// Each BSS must have been given exactly the other one: the device's two
	// radios carry the same SSID, so they are each other's neighbour and
	// neither is its own.
	for _, dev := range first.Devices {
		for _, b := range dev.BSSes {
			if b.Neighbours != 1 {
				t.Errorf("%s got %d neighbours, want 1", b.Iface, b.Neighbours)
			}
			if b.Failed != "" {
				t.Errorf("%s failed: %s", b.Iface, b.Failed)
			}
		}
	}

	second, err := d.DistributeNeighbours(ctx)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Updated != 0 || second.Unchanged != 2 {
		t.Errorf("a converged fleet was written to again: %d updated, %d "+
			"unchanged. This is the property the request budget depends on — "+
			"hostapd returns its list in its own order, and an order-sensitive "+
			"comparison re-pushes to every AP forever",
			second.Updated, second.Unchanged)
	}
}

// The three-state gate. A device adopted before the ACL carried the rrm grants
// reports NotObservable, and the operator has to be told the remedy is
// re-adoption rather than different hardware.
func TestDistributeSkipsDevicesThatCannotBeAsked(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	seedRoamingWLAN(t, d, "OpenWrt", true)
	seedAP(t, d, "60:38:e0:00:0b:02", "old-acl", addr, capability.NotObservable)

	res, err := d.DistributeNeighbours(context.Background())
	if err != nil {
		t.Fatalf("DistributeNeighbours: %v", err)
	}
	if len(res.Devices) != 1 {
		t.Fatalf("want one device row, got %d", len(res.Devices))
	}
	row := res.Devices[0]
	if row.Error != "" {
		t.Errorf("a standing limitation was reported as an error: %q", row.Error)
	}
	if !strings.Contains(row.Skipped, "re-adopt") {
		t.Errorf("the skip reason does not name the remedy: %q", row.Skipped)
	}
	if res.Updated != 0 {
		t.Errorf("wrote to a device that cannot be asked")
	}
}

// A WLAN with 802.11k switched off gets no neighbour lists. The renderer writes
// no rrm_neighbor_report for it, so the AP will not answer a client's request —
// filling a list it cannot use is work that looks like a feature and is not.
func TestDistributeIgnoresWLANsWithoutKV(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	seedRoamingWLAN(t, d, "OpenWrt", false)
	seedAP(t, d, "60:38:e0:00:0b:03", "no-kv", addr, capability.Present)

	res, err := d.DistributeNeighbours(context.Background())
	if err != nil {
		t.Fatalf("DistributeNeighbours: %v", err)
	}
	if res.Updated != 0 {
		t.Errorf("distributed to a WLAN that did not ask for 802.11k")
	}
	if res.Note == "" {
		t.Error("an empty run with no explanation is indistinguishable from a " +
			"broken feature")
	}
}

// An SSID the controller does not manage must be left entirely alone. Rewriting
// hand-made configuration is what ARCHITECTURE §0 forbids, and a neighbour list
// an operator set up themselves is exactly that.
func TestDistributeNeverTouchesUnmanagedSSIDs(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	seedRoamingWLAN(t, d, "some-other-network", true)
	seedAP(t, d, "60:38:e0:00:0b:04", "foreign", addr, capability.Present)

	res, err := d.DistributeNeighbours(context.Background())
	if err != nil {
		t.Fatalf("DistributeNeighbours: %v", err)
	}
	if res.Updated != 0 {
		t.Fatalf("wrote a neighbour list to an SSID the controller does not manage")
	}
	if len(res.Devices) != 1 || res.Devices[0].Skipped == "" {
		t.Errorf("the device should be reported as carrying nothing managed: %+v",
			res.Devices)
	}
}

// A device that has gone away is a row that says so, not a failure for the
// fleet — the same rule Preview follows.
func TestDistributeReportsUnreachableDevicesWithoutFailing(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	seedRoamingWLAN(t, d, "OpenWrt", true)
	seedAP(t, d, "60:38:e0:00:0b:05", "here", addr, capability.Present)
	seedAP(t, d, "60:38:e0:00:0b:06", "gone", "127.0.0.1:1", capability.Present)

	res, err := d.DistributeNeighbours(context.Background())
	if err != nil {
		t.Fatalf("one unreachable device failed the whole cycle: %v", err)
	}
	var reachable, broken int
	for _, dev := range res.Devices {
		if dev.Error != "" {
			broken++
			continue
		}
		reachable += len(dev.BSSes)
	}
	if broken != 1 {
		t.Errorf("want one device reported as unreachable, got %d", broken)
	}
	if reachable != 2 {
		t.Errorf("the reachable device was not reconciled: %d BSSes", reachable)
	}
}

// decodeOwnNR guards the one shape that would be silently corrupting: a reply
// that is short. The controller relays these bytes to other APs, and an entry
// with an empty element makes an AP answer a client with a candidate it has no
// channel for.
func TestDecodeOwnNRRejectsShortReplies(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ``},
		{"no value", `{}`},
		{"two fields", `{"value":["30:23:03:db:be:42","OpenWrt"]}`},
		{"blank element", `{"value":["30:23:03:db:be:42","OpenWrt",""]}`},
		{"blank ssid", `{"value":["30:23:03:db:be:42","","3023"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := decodeOwnNR(json.RawMessage(tc.raw), 1, "phy0-ap0"); ok {
				t.Error("accepted an incomplete own-NR reply")
			}
		})
	}

	good := `{"value":["30:23:03:db:be:42","OpenWrt","302303dbbe42ef19"]}`
	n, ok := decodeOwnNR(json.RawMessage(good), 7, "phy0-ap1")
	if !ok {
		t.Fatal("rejected a well-formed reply")
	}
	if n.DeviceID != 7 || n.Iface != "phy0-ap1" || n.NR != "302303dbbe42ef19" {
		t.Errorf("decoded wrong: %+v", n)
	}
}

// An unreachable device must not have its BSSes deleted from the APs that ARE
// reachable.
//
// The mock instance here carries both BSSes of the managed SSID, so a complete
// cycle would give each one the other. Adding a device that cannot be reached
// makes the cycle incomplete — and the reconciler must then still refuse to
// shrink a list, because it cannot tell "that AP is gone" from "I could not ask
// about that AP".
func TestIncompleteCycleNeverShrinksAList(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	seedRoamingWLAN(t, d, "OpenWrt", true)
	seedAP(t, d, "60:38:e0:00:0b:07", "here", addr, capability.Present)

	ctx := context.Background()
	if _, err := d.DistributeNeighbours(ctx); err != nil {
		t.Fatal(err)
	}

	// Now a second device appears and cannot be reached. Nothing about the
	// first device changed, so its lists must survive untouched.
	seedAP(t, d, "60:38:e0:00:0b:08", "gone", "127.0.0.1:1", capability.Present)

	res, err := d.DistributeNeighbours(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 0 {
		t.Errorf("rewrote %d list(s) because an unrelated device was "+
			"unreachable", res.Updated)
	}
	for _, dev := range res.Devices {
		if dev.Error != "" {
			continue
		}
		for _, b := range dev.BSSes {
			if b.Neighbours != 1 {
				t.Errorf("%s/%s dropped to %d neighbours during an incomplete "+
					"cycle", dev.Name, b.Iface, b.Neighbours)
			}
		}
	}
}
