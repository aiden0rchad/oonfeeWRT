package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/applyengine"
	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func bindingCaps(hardware string) *capability.Registry {
	caps := capability.NewRegistry()
	caps.Class = capability.ClassA
	caps.Board.Release = "OpenWrt test"
	caps.Radios = []capability.Radio{
		{Device: "radio0", Phy: "phy0", Frequency: 5180, Hardware: hardware},
		{Device: "radio1", Phy: "phy1", Frequency: 2437, Hardware: hardware},
	}
	return caps
}

func bindingReload(t *testing.T, d *Daemon, id int64) *store.Device {
	t.Helper()
	dev, err := d.Store.DeviceByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

func bindingPreview(t *testing.T, d *Daemon) *api.PreviewResult {
	t.Helper()
	p, err := d.Preview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.PreviewToken == "" {
		t.Fatal("preview returned no binding token")
	}
	return p
}

func bindingSaveWLAN(t *testing.T, d *Daemon, ids []int64, ssid, key string,
	pmf model.PMF) *model.WLAN {
	t.Helper()
	ctx := context.Background()
	network := &model.Network{
		Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Zone: "lan", Enabled: true,
	}
	if err := d.Store.SaveNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	group := &model.APGroup{Name: "binding-aps", DeviceIDs: ids}
	if err := d.Store.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	wlan := &model.WLAN{
		SSID: ssid, NetworkID: network.ID, GroupID: group.ID,
		Bands:    []model.Band{model.Band2G, model.Band5G},
		Security: model.Security{Mode: model.SecPSK2, Key: key, PMF: pmf}, Enabled: true,
	}
	if err := d.Store.SaveWLAN(ctx, wlan); err != nil {
		t.Fatal(err)
	}
	return wlan
}

func bindingConfigFingerprint(t *testing.T, d *Daemon, id int64) string {
	t.Helper()
	ctx := context.Background()
	client, err := d.Connect(ctx, bindingReload(t, d, id))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	existing, err := reconcile.ReadExisting(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := stateFingerprint(existing)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func bindingSetOption(t *testing.T, d *Daemon, id int64, config, section,
	option, value string) {
	t.Helper()
	ctx := context.Background()
	client, err := d.Connect(ctx, bindingReload(t, d, id))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Call(ctx, "uci", "set", map[string]any{
		"config": config, "section": section,
		"values": map[string]string{option: value},
	}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "uci", "commit", map[string]any{
		"config": config,
	}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
}

func bindingAddSection(t *testing.T, d *Daemon, id int64, config, section,
	typeName string, values map[string]string) {
	t.Helper()
	ctx := context.Background()
	client, err := d.Connect(ctx, bindingReload(t, d, id))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Call(ctx, "uci", "add", map[string]any{
		"config": config, "name": section, "type": typeName, "values": values,
	}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "uci", "commit", map[string]any{
		"config": config,
	}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
}

func bindingConverge(t *testing.T, d *Daemon, id int64) {
	t.Helper()
	ctx := context.Background()
	dev := bindingReload(t, d, id)
	site, err := d.Store.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := deviceCaps(dev)
	if err != nil {
		t.Fatal(err)
	}
	client, err := d.Connect(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	r := reconcile.New(d.Store)
	plan, err := r.PlanDevice(ctx, client, site, renderDevice(dev), caps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(ctx, client, id, plan, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewFingerprintsExcludePollingButBindProvisioningIdentity(t *testing.T) {
	at, seen := int64(1), int64(2)
	base := &store.Device{
		ID: 3, MAC: "00:11:22:33:44:55", Host: "192.0.2.1", Port: 80,
		Scheme: "http", CertFP: "cert", HostKeyFP: "ssh", Name: "ap",
		Role: "ap", Functions: []string{"ap"}, AdoptedAt: &at,
		CredEnc: []byte("sealed-secret"), Class: "A", CapsJSON: `{"a":1}`,
		FWRelease: "r1", LastSeen: &seen, PollState: "baseline", PollInterval: 60,
	}
	want, err := fleetStateFingerprint([]*store.Device{base})
	if err != nil {
		t.Fatal(err)
	}
	telemetry := *base
	newSeen := int64(999)
	telemetry.LastSeen, telemetry.PollState, telemetry.PollInterval = &newSeen, "focused", 900
	got, _ := fleetStateFingerprint([]*store.Device{&telemetry})
	if got != want {
		t.Fatal("polling telemetry invalidated a provisioning preview")
	}

	for name, mutate := range map[string]func(*store.Device){
		"host":         func(d *store.Device) { d.Host = "192.0.2.2" },
		"certificate":  func(d *store.Device) { d.CertFP = "other" },
		"ssh pin":      func(d *store.Device) { d.HostKeyFP = "other" },
		"name":         func(d *store.Device) { d.Name = "new" },
		"functions":    func(d *store.Device) { d.Functions = []string{"ap", "switch"} },
		"credential":   func(d *store.Device) { d.CredEnc = []byte("new-secret") },
		"capabilities": func(d *store.Device) { d.CapsJSON = `{"a":2}` },
		"firmware":     func(d *store.Device) { d.FWRelease = "r2" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := *base
			changed.Functions = append([]string(nil), base.Functions...)
			changed.CredEnc = append([]byte(nil), base.CredEnc...)
			mutate(&changed)
			fp, err := fleetStateFingerprint([]*store.Device{&changed})
			if err != nil {
				t.Fatal(err)
			}
			if fp == want {
				t.Fatalf("%s did not invalidate the fleet binding", name)
			}
			if strings.Contains(fp, "secret") {
				t.Fatal("credential plaintext/ciphertext escaped the SHA-256 digest")
			}
		})
	}
}

func TestPreviewFingerprintBindsSecretPlanValuesWithoutExposingThem(t *testing.T) {
	secret := "sentinel-passphrase-3dF8"
	site := model.Site{UUID: "site", WLANs: []model.WLAN{{
		SSID: "x", Security: model.Security{Mode: model.SecPSK2, Key: secret},
	}}}
	siteFP, err := siteStateFingerprint(site)
	if err != nil {
		t.Fatal(err)
	}
	dev := &store.Device{ID: 1, MAC: "00:11:22:33:44:66", Role: "ap"}
	plan := &reconcile.DevicePlan{Plan: applyengine.Plan{Ops: []applyengine.Op{{
		Kind: applyengine.OpSet, Config: "wireless", Section: "owned",
		Values: map[string]string{"key": secret},
	}}}}
	first, err := planStateFingerprint(siteFP, dev, nil, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(siteFP, secret) || strings.Contains(first, secret) {
		t.Fatal("a secret escaped an opaque preview digest")
	}
	plan.Plan.Ops[0].Values["key"] = "different-secret"
	second, _ := planStateFingerprint(siteFP, dev, nil, plan, nil)
	if first == second {
		t.Fatal("changing a secret plan value did not invalidate its fingerprint")
	}
}

func TestPreviewTokenIsKeyedStableAndOpaque(t *testing.T) {
	newKeys := func() *secrets.Keeper {
		t.Helper()
		keys, err := secrets.Create(secrets.DefaultPath(t.TempDir()), []byte("pass"),
			secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = keys.Close() })
		return keys
	}

	const secret = "example-WLAN-placeholder-not-for-the-browser"
	siteFP, err := siteStateFingerprint(model.Site{UUID: "site", WLANs: []model.WLAN{{
		SSID: "private", Security: model.Security{Mode: model.SecPSK2, Key: secret},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	fleetFP := strings.Repeat("b", 64)
	plans := map[int64]string{7: strings.Repeat("c", 64)}
	devices := []*store.Device{{ID: 7, Role: "ap"}}
	rawDigest, err := stateFingerprint(fleetPlanBinding{
		Version: previewBindingVersion, SiteFingerprint: siteFP,
		FleetFingerprint: fleetFP,
		Plans:            []boundPlan{{DeviceID: 7, Fingerprint: plans[7]}},
	})
	if err != nil {
		t.Fatal(err)
	}

	firstKeys := newKeys()
	first, err := previewToken(firstKeys, siteFP, fleetFP, plans, devices)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := previewToken(firstKeys, siteFP, fleetFP, plans, devices)
	if err != nil {
		t.Fatal(err)
	}
	if repeated != first {
		t.Fatal("the same Keeper and state produced different preview tokens")
	}
	mac, err := firstKeys.HMACSHA256(previewTokenDomain, []byte(rawDigest))
	if err != nil {
		t.Fatal(err)
	}
	if want := "pv1_" + fmt.Sprintf("%x", mac); first != want {
		t.Fatalf("preview token = %q, want the domain-separated keyed digest", first)
	}
	if strings.Contains(first, rawDigest) || strings.Contains(first, siteFP) ||
		strings.Contains(first, secret) {
		t.Fatal("preview token exposed a raw internal digest or site secret")
	}

	second, err := previewToken(newKeys(), siteFP, fleetFP, plans, devices)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("different Keepers produced the same public preview token")
	}
	if _, err := previewToken(nil, siteFP, fleetFP, plans, devices); err == nil {
		t.Fatal("preview token generation fell back when no Keeper was available")
	}
}

func TestPreviewFailsWhenKeeperIsClosed(t *testing.T) {
	d := openDaemon(t)
	if err := d.Keys.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Preview(context.Background()); !errors.Is(err, secrets.ErrClosed) {
		t.Fatalf("Preview with closed Keeper = %v, want ErrClosed", err)
	}
}

func TestPreviewTokenTracksSiteSecretAndRealFleetInputs(t *testing.T) {
	t.Run("policy intent", func(t *testing.T) {
		d := openDaemon(t)
		first := bindingPreview(t, d)
		if err := d.Store.SavePolicy(context.Background(), &model.Policy{
			Name:    "binding route",
			Kind:    model.PolicyStaticRoute,
			Origin:  model.PolicyOriginManual,
			Enabled: true,
			StaticRoute: &model.StaticRoute{
				Target:  "203.0.113.0/24",
				Gateway: "192.0.2.1",
			},
		}); err != nil {
			t.Fatal(err)
		}
		if got := bindingPreview(t, d).PreviewToken; got == first.PreviewToken {
			t.Fatal("policy change left the preview token valid")
		}
		if _, err := d.ApplySite(context.Background(), api.ApplyRequest{
			PreviewToken: first.PreviewToken,
		}); !errors.Is(err, api.ErrPreviewStale) {
			t.Fatalf("policy-stale token error = %v, want ErrPreviewStale", err)
		}
	})

	t.Run("site WLAN secret", func(t *testing.T) {
		d := openDaemon(t)
		wlan := bindingSaveWLAN(t, d, nil, "binding", "first-secret", model.PMFDisabled)
		first := bindingPreview(t, d).PreviewToken
		wlan.Security.Key = "second-secret"
		if err := d.Store.SaveWLAN(context.Background(), wlan); err != nil {
			t.Fatal(err)
		}
		second := bindingPreview(t, d).PreviewToken
		if first == second {
			t.Fatal("WLAN key change left the preview token valid")
		}
		if strings.Contains(first, "first-secret") || strings.Contains(second, "second-secret") {
			t.Fatal("preview token exposed a WLAN key")
		}
	})

	t.Run("device identity credential capabilities ownership and live UCI", func(t *testing.T) {
		addr := startMock(t)
		d := openDaemon(t)
		dev := seedAP(t, d, "60:38:e0:00:0d:01", "binding-ap", addr, capability.Present)
		if err := d.Store.SetCapabilities(context.Background(), dev.ID,
			bindingCaps("Generic MAC80211"), string(capability.ClassA)); err != nil {
			t.Fatal(err)
		}
		current := bindingPreview(t, d).PreviewToken
		if _, err := d.Store.SQL().ExecContext(context.Background(),
			`UPDATE devices SET last_seen=?, poll_state='focused', poll_interval_s=900 WHERE id=?`,
			int64(999), dev.ID); err != nil {
			t.Fatal(err)
		}
		if got := bindingPreview(t, d).PreviewToken; got != current {
			t.Fatal("LastSeen/poll state invalidated a preview")
		}
		changed := func(name string, mutate func(*store.Device)) {
			t.Helper()
			row := bindingReload(t, d, dev.ID)
			mutate(row)
			if err := d.Store.UpsertDevice(context.Background(), row); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			next := bindingPreview(t, d).PreviewToken
			if next == current {
				t.Fatalf("%s did not invalidate the real preview token", name)
			}
			current = next
		}
		changed("name", func(row *store.Device) { row.Name = "renamed-ap" })
		changed("functions", func(row *store.Device) {
			row.Functions = []string{"ap"}
		})
		changed("credential", func(row *store.Device) {
			blob, err := d.Keys.SealCredential(row.MAC, "root", "good")
			if err != nil {
				t.Fatal(err)
			}
			row.CredEnc = blob
		})
		changed("host", func(row *store.Device) {
			row.Host = strings.Replace(row.Host, "127.0.0.1", "localhost", 1)
		})

		caps := bindingCaps("Generic MAC80211 v2")
		if err := d.Store.SetCapabilities(context.Background(), dev.ID, caps,
			string(capability.ClassA)); err != nil {
			t.Fatal(err)
		}
		next := bindingPreview(t, d).PreviewToken
		if next == current {
			t.Fatal("capability change did not invalidate the real preview token")
		}
		current = next

		if err := d.Store.ReplaceOwned(context.Background(), dev.ID, []store.OwnedSection{{
			DeviceID: dev.ID, Config: "wireless", Section: "owned-marker",
			RenderedHash: "hash-one", AppliedAt: time.Now().Unix(),
		}}); err != nil {
			t.Fatal(err)
		}
		next = bindingPreview(t, d).PreviewToken
		if next == current {
			t.Fatal("owned configuration change did not invalidate the preview token")
		}
		current = next

		bindingSetOption(t, d, dev.ID, "wireless", "radio0", "binding_marker", "changed")
		if next = bindingPreview(t, d).PreviewToken; next == current {
			t.Fatal("live UCI plan input changed without invalidating the preview token")
		}
	})
}

func TestApplyRejectsMissingAndStaleBindingsBeforeAWrite(t *testing.T) {
	if _, err := (&Daemon{}).ApplySite(context.Background(), api.ApplyRequest{}); !errors.Is(err, api.ErrPreviewRequired) {
		t.Fatalf("missing token error = %v, want ErrPreviewRequired", err)
	}
	d := openDaemon(t)
	preview := bindingPreview(t, d)
	if err := d.Store.SetSiteName(context.Background(), "changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken,
	}); !errors.Is(err, api.ErrPreviewStale) {
		t.Fatalf("stale token error = %v, want ErrPreviewStale", err)
	}
}

func TestApplyErrorPreflightStopsFleetAndSelectionNarrows(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	good := seedAP(t, d, "60:38:e0:00:0e:01", "good-ap", addr, capability.Present)
	bad := seedAP(t, d, "60:38:e0:00:0e:02", "gone-ap", "127.0.0.1:1", capability.Present)
	if err := d.Store.SetCapabilities(context.Background(), good.ID,
		bindingCaps("Generic MAC80211"), string(capability.ClassA)); err != nil {
		t.Fatal(err)
	}
	// The fixture's stock OpenWrt BSS uses the same SSID. Mark it as
	// controller-owned so the plan replaces it rather than correctly blocking
	// on a human-owned duplicate; iwinfo still reports OpenWrt for the health gate.
	bindingSetOption(t, d, good.ID, "wireless", "default_radio0", "oonfeewrt", "1")
	bindingSaveWLAN(t, d, []int64{good.ID}, "OpenWrt", "hunter22", model.PMFDisabled)
	preview := bindingPreview(t, d)
	if len(preview.Devices) != 2 || preview.Devices[0].Error != "" ||
		len(preview.Devices[0].Changes) == 0 || preview.Devices[1].Error == "" {
		t.Fatalf("fixture did not produce pending-first/error-second rows: %+v", preview.Devices)
	}
	before := bindingConfigFingerprint(t, d, good.ID)
	if _, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken,
	}); err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("full-fleet error preflight = %v", err)
	}
	if after := bindingConfigFingerprint(t, d, good.ID); after != before {
		t.Fatal("the first device changed before a later error row was refused")
	}

	// The token always binds the full fleet, but row-level preflight applies to
	// the requested selection, and skipping devices itself needs explicit API
	// consent because the controller UI always applies the whole fleet.
	if _, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken, DeviceIDs: []int64{good.ID},
	}); err == nil || !strings.Contains(err.Error(), "partial-fleet") {
		t.Fatalf("unacknowledged partial selection = %v", err)
	}
	d.Config.ApplyDrain = applyengine.MinApplyBudget() + 5*time.Second
	res, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken, DeviceIDs: []int64{good.ID},
		AcknowledgePartialFleet: true,
	})
	if err != nil || len(res.Devices) != 1 || res.Devices[0].Outcome != string(applyengine.Applied) {
		t.Fatalf("selected healthy apply = %+v, %v", res, err)
	}
	if _, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken, DeviceIDs: []int64{good.ID},
		AcknowledgePartialFleet: true,
	}); !errors.Is(err, api.ErrPreviewStale) {
		t.Fatalf("replayed write token = %v, want stale", err)
	}

	// A fresh converged/no-op token can be replayed: no desired or plan input
	// changed, and both calls perform zero router writes.
	converged := bindingPreview(t, d)
	for i := 0; i < 2; i++ {
		if _, err := d.ApplySite(context.Background(), api.ApplyRequest{
			PreviewToken: converged.PreviewToken, DeviceIDs: []int64{good.ID},
			AcknowledgePartialFleet: true,
		}); err != nil {
			t.Fatalf("no-op replay %d: %v", i+1, err)
		}
	}
	_ = bad
}

func TestApplyBlockedPreflightStopsEarlierDevice(t *testing.T) {
	firstAddr, secondAddr := startMock(t), startMock(t)
	d := openDaemon(t)
	first := seedAP(t, d, "60:38:e0:00:0f:01", "first-ap", firstAddr, capability.Present)
	second := seedAP(t, d, "60:38:e0:00:0f:02", "blocked-ap", secondAddr, capability.Present)
	for _, dev := range []*store.Device{first, second} {
		if err := d.Store.SetCapabilities(context.Background(), dev.ID,
			bindingCaps("Generic MAC80211"), string(capability.ClassA)); err != nil {
			t.Fatal(err)
		}
	}
	wlan := bindingSaveWLAN(t, d, []int64{first.ID, second.ID},
		"blocked-binding", "test-passphrase", model.PMFDisabled)
	bindingAddSection(t, d, second.ID, "wireless",
		fmt.Sprintf("oowrt_wlan%d_radio0", wlan.ID), "wifi-iface",
		map[string]string{"device": "radio0", "mode": "ap", "ssid": "human-owned"})
	preview := bindingPreview(t, d)
	if len(preview.Devices[0].Changes) == 0 || !preview.Devices[1].Blocked {
		t.Fatalf("fixture did not produce pending-first/blocked-second: %+v", preview.Devices)
	}
	before := bindingConfigFingerprint(t, d, first.ID)
	if _, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken,
	}); err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("blocked preflight = %v", err)
	}
	if after := bindingConfigFingerprint(t, d, first.ID); after != before {
		t.Fatal("the first device changed before a later blocked row was refused")
	}
}

func TestApplyFatalDriverRiskNeedsServerAcknowledgement(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	dev := seedAP(t, d, "60:38:e0:00:10:01", "marvell-ap", addr, capability.Present)
	if err := d.Store.SetCapabilities(context.Background(), dev.ID,
		bindingCaps("Marvell 88W8964"), string(capability.ClassA)); err != nil {
		t.Fatal(err)
	}
	bindingSaveWLAN(t, d, []int64{dev.ID}, "dangerous", "test-passphrase", model.PMFOptional)
	bindingConverge(t, d, dev.ID)
	preview := bindingPreview(t, d)
	if len(preview.Devices) != 1 || len(preview.Devices[0].DriverDefects) == 0 {
		t.Fatalf("fixture did not trigger a driver defect: %+v", preview.Devices)
	}
	before := bindingConfigFingerprint(t, d, dev.ID)
	if _, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken,
	}); err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("unacknowledged fatal defect = %v", err)
	}
	if after := bindingConfigFingerprint(t, d, dev.ID); after != before {
		t.Fatal("fatal-risk refusal wrote to the router")
	}
	res, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken, AcknowledgeDriverRisk: true,
	})
	if err != nil || len(res.Devices) != 1 || res.Devices[0].Outcome != string(applyengine.Applied) {
		t.Fatalf("acknowledged driver-risk apply = %+v, %v", res, err)
	}
}

func TestApplyCautionNeedsServerAcknowledgementBeforeWrite(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	dev := seedAP(t, d, "60:38:e0:00:10:11", "mesh-ap", addr, capability.Present)
	caps := bindingCaps("Generic MAC80211")
	caps.Set(capability.FeatMesh, capability.Present)
	if err := d.Store.SetCapabilities(context.Background(), dev.ID, caps,
		string(capability.ClassA)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	network := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Zone: "lan", Enabled: true}
	if err := d.Store.SaveNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	group := &model.APGroup{Name: "mesh-aps", DeviceIDs: []int64{dev.ID}}
	if err := d.Store.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := d.Store.SaveMesh(ctx, &model.Mesh{
		MeshID: "open-binding", NetworkID: network.ID, GroupID: group.ID,
		Band: model.Band5G, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	preview := bindingPreview(t, d)
	if len(preview.Devices) != 1 || len(preview.Devices[0].Cautions) == 0 {
		t.Fatalf("fixture did not produce an open-mesh caution: %+v", preview.Devices)
	}
	before := bindingConfigFingerprint(t, d, dev.ID)
	if _, err := d.ApplySite(ctx, api.ApplyRequest{
		PreviewToken: preview.PreviewToken,
	}); err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("unacknowledged caution = %v", err)
	}
	if after := bindingConfigFingerprint(t, d, dev.ID); after != before {
		t.Fatal("caution refusal wrote to the router")
	}
}

func TestApplyTraversalPreflightStopsEarlierWirelessDevice(t *testing.T) {
	apAddr, gatewayAddr := startMock(t), startMock(t)
	d := openDaemon(t)
	ap := seedAP(t, d, "60:38:e0:00:11:01", "first-ap", apAddr, capability.Present)
	gateway := seedAP(t, d, "60:38:e0:00:11:02", "gateway", gatewayAddr, capability.Present)
	if err := d.Store.SetCapabilities(context.Background(), ap.ID,
		bindingCaps("Generic MAC80211"), string(capability.ClassA)); err != nil {
		t.Fatal(err)
	}
	bindingSetOption(t, d, ap.ID, "wireless", "default_radio0", "oonfeewrt", "1")
	gwCaps := bindingCaps("Generic MAC80211")
	gwCaps.Ports = capability.Ports{Bridge: "br-lan", LAN: []string{"lan1", "lan2"}, WAN: "wan"}
	gwCaps.Set(capability.FeatDSA, capability.Present)
	gwCaps.Set(capability.FeatFirewall4, capability.Present)
	if err := d.Store.SetCapabilities(context.Background(), gateway.ID, gwCaps,
		string(capability.ClassA)); err != nil {
		t.Fatal(err)
	}
	gw := bindingReload(t, d, gateway.ID)
	gw.Role, gw.Functions = "gateway", []string{"gateway", "ap", "switch"}
	if err := d.Store.UpsertDevice(context.Background(), gw); err != nil {
		t.Fatal(err)
	}
	bindingAddSection(t, d, gateway.ID, "network", "human_vlan1", "bridge-vlan",
		map[string]string{"device": "br-lan", "vlan": "1"})
	bindingSaveWLAN(t, d, []int64{ap.ID}, "OpenWrt", "hunter22", model.PMFDisabled)
	if err := d.Store.SaveNetwork(context.Background(), &model.Network{
		Name: "guest", VLAN: 20, CIDR: "192.168.20.1/24", Zone: "guest", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	preview := bindingPreview(t, d)
	if len(preview.Devices) != 2 || len(preview.Devices[0].Changes) == 0 ||
		preview.Devices[0].TouchesTraversal || !preview.Devices[1].TouchesTraversal {
		t.Fatalf("fixture did not produce wireless-first/traversal-last: %+v", preview.Devices)
	}
	before := bindingConfigFingerprint(t, d, ap.ID)
	if _, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken,
	}); err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("traversal preflight = %v", err)
	}
	if after := bindingConfigFingerprint(t, d, ap.ID); after != before {
		t.Fatal("wireless device changed before a later traversal row was refused")
	}
}

func TestPerDeviceReplanRejectsLiveDriftBeforeWrite(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	dev := seedAP(t, d, "60:38:e0:00:12:01", "drift-ap", addr, capability.Present)
	if err := d.Store.SetCapabilities(context.Background(), dev.ID,
		bindingCaps("Generic MAC80211"), string(capability.ClassA)); err != nil {
		t.Fatal(err)
	}
	state, err := d.buildPreview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bindingSetOption(t, d, dev.ID, "wireless", "radio0", "late_marker", "changed")
	before := bindingConfigFingerprint(t, d, dev.ID)
	_, err = d.applyDeviceBound(context.Background(), state.site, state.devices[0], false,
		state.siteFingerprint, state.fleetFingerprint, state.planFingerprints[dev.ID])
	if !errors.Is(err, api.ErrPreviewStale) {
		t.Fatalf("late live drift = %v, want ErrPreviewStale", err)
	}
	if after := bindingConfigFingerprint(t, d, dev.ID); after != before {
		t.Fatal("per-device stale-plan rejection wrote to the router")
	}
}

func TestApplyRunOutlivesRequestCancellationBetweenDevices(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t)
	first := seedAP(t, d, "60:38:e0:00:13:01", "first", addr, capability.Present)
	second := seedAP(t, d, "60:38:e0:00:13:02", "second", addr, capability.Present)
	preview := bindingPreview(t, d)
	releaseSecond, err := d.deviceOps.acquire(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		res *api.ApplyResult
		err error
	}, 1)
	go func() {
		res, err := d.ApplySite(ctx, api.ApplyRequest{PreviewToken: preview.PreviewToken})
		done <- struct {
			res *api.ApplyResult
			err error
		}{res, err}
	}()
	waitForGateUsers(t, &d.deviceOps, second.ID, 2)
	cancel()
	select {
	case got := <-done:
		t.Fatalf("request cancellation stopped the fleet run: %+v, %v", got.res, got.err)
	case <-time.After(50 * time.Millisecond):
	}
	if n := d.applies.inFlight(); n < 2 {
		t.Fatalf("apply barrier dropped between devices: in-flight=%d", n)
	}
	releaseSecond()
	select {
	case got := <-done:
		if got.err != nil || len(got.res.Devices) != 2 {
			t.Fatalf("detached fleet run = %+v, %v", got.res, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached fleet run did not finish after the second device was released")
	}
	_ = first
}

func TestApplyWillNotArmRollbackWithoutEnoughDrainBudget(t *testing.T) {
	addr := startMock(t)
	d := openDaemon(t) // testConfig intentionally has a two-second drain budget.
	dev := seedAP(t, d, "60:38:e0:00:13:11", "budget-ap", addr, capability.Present)
	if err := d.Store.SetCapabilities(context.Background(), dev.ID,
		bindingCaps("Generic MAC80211"), string(capability.ClassA)); err != nil {
		t.Fatal(err)
	}
	bindingSaveWLAN(t, d, []int64{dev.ID}, "pending-change", "test-passphrase", model.PMFDisabled)
	preview := bindingPreview(t, d)
	if len(preview.Devices[0].Changes) == 0 {
		t.Fatal("fixture has no pending change")
	}
	before := bindingConfigFingerprint(t, d, dev.ID)
	res, err := d.ApplySite(context.Background(), api.ApplyRequest{
		PreviewToken: preview.PreviewToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Aborted || len(res.Devices) != 1 ||
		!strings.Contains(res.Devices[0].Reason, "apply-drain budget") {
		t.Fatalf("short-budget result = %+v", res)
	}
	if after := bindingConfigFingerprint(t, d, dev.ID); after != before {
		t.Fatal("short-budget refusal wrote to the router")
	}
}

func TestErrorPreviewSerialisesChangesAsAnArray(t *testing.T) {
	d := openDaemon(t)
	dev := seedAP(t, d, "60:38:e0:00:14:01", "gone", "127.0.0.1:1", capability.Present)
	preview := bindingPreview(t, d)
	if preview.Devices[0].Error == "" || preview.Devices[0].Changes == nil {
		t.Fatalf("error row = %+v, want non-nil empty changes", preview.Devices[0])
	}
	blob, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"changes":[]`) {
		t.Fatalf("error preview JSON can crash d.changes.length: %s", blob)
	}
	_ = dev
}
