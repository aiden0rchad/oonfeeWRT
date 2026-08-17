//go:build integration

package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// Adoption against a real device. This one WRITES — it installs the ACL file
// and creates an rpcd login — so it has its own opt-in on top of the
// integration tag:
//
//	OONFEE_TEST_ADOPT=1 OONFEE_TEST_HOST=192.168.1.1 \
//	OONFEE_TEST_ADMIN_USER=root OONFEE_TEST_ADMIN_PASS=... \
//	go test -tags=integration ./internal/daemon/ -run TestIntegrationAdopt -v
//
// It prints the credential it created, because re-adopting rotates it and the
// other integration tests need the current one.
func TestIntegrationAdoptARealDevice(t *testing.T) {
	if os.Getenv("OONFEE_TEST_ADOPT") != "1" {
		t.Skip("set OONFEE_TEST_ADOPT=1 to run the test that writes to a device")
	}
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_ADMIN_USER")
	pass := os.Getenv("OONFEE_TEST_ADMIN_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_ADMIN_USER")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := testConfig(t, "operator passphrase")
	// testConfig ships a 2-second ApplyDrain, which is right for the unit tests
	// that exercise shutdown and wrong for anything that actually applies:
	// TrackApply gives EVERY apply a context with that deadline, so a real
	// apply — stage, arm a 90s rollback, health-check, confirm — was cancelled
	// mid-flight and reported "unknown" with the change sitting on the device.
	// The production default is 3 minutes for exactly this reason.
	cfg.ApplyDrain = 3 * time.Minute
	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if err := d.StartCollector(ctx, collector.Options{
		Baseline: time.Second, Log: quietLogger(),
	}); err != nil {
		t.Fatalf("StartCollector: %v", err)
	}

	res, err := d.Adopt(ctx, api.AdoptRequest{
		Host: host, Username: user, Password: pass, Name: "wrt3200acm", Role: "gateway",
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	t.Logf("adopted %s (%s) as device %d", res.Name, res.MAC, res.DeviceID)
	t.Logf("  model=%q class=%s firmware=%q", res.Model, res.Class, res.Firmware)
	t.Logf("  features: %v", res.Features)
	if len(res.Unobservable) > 0 {
		t.Logf("  could not determine: %v", res.Unobservable)
	}
	for _, q := range res.Quirks {
		t.Logf("  quirk: %s", q)
	}
	for _, w := range res.Warnings {
		t.Logf("  WARNING: %s", w)
	}

	if res.MAC == "" || res.Model == "" {
		t.Errorf("adoption returned an incomplete identity: %+v", res)
	}
	if res.Class == "" {
		t.Error("the device was not classified")
	}
	if len(res.Features) == 0 {
		t.Error("no capabilities were recorded")
	}

	// The credential must be sealed and usable — an adoption that reports
	// success without a working login is how a device joins the inventory
	// unreachable.
	dev, err := d.Store.DeviceByMAC(ctx, res.MAC)
	if err != nil {
		t.Fatalf("the adopted device is not in the inventory: %v", err)
	}
	if !dev.Adopted() || len(dev.CredEnc) == 0 {
		t.Fatalf("device row is not marked adopted or carries no credential: %+v", dev)
	}
	username, password, err := d.Keys.OpenCredential(dev.MAC, dev.CredEnc)
	if err != nil {
		t.Fatalf("the sealed credential will not open: %v", err)
	}
	c, err := d.Connect(ctx, dev)
	if err != nil {
		t.Fatalf("the credential adoption created does not work: %v", err)
	}
	defer c.Close()
	var board struct {
		Release struct {
			Description string `json:"description"`
		} `json:"release"`
	}
	if err := c.Call(ctx, "system", "board", nil, &board); err != nil {
		t.Fatalf("system.board on the new credential: %v", err)
	}
	t.Logf("the new controller login works: %s", board.Release.Description)

	// Re-adopting must be refused rather than silently rotating the credential
	// out from under a working install.
	if _, err := d.Adopt(ctx, api.AdoptRequest{
		Host: host, Username: user, Password: pass,
	}); err == nil {
		t.Error("adopting an already-adopted device succeeded")
	} else if !strings.Contains(err.Error(), "already adopted") {
		t.Errorf("unhelpful error for a re-adopt: %v", err)
	}

	// Printed last so it is easy to find. This is a lab device.
	t.Logf("CREDENTIAL %s / %s", username, password)
}

// ROADMAP Phase 0's second proof, in full: "Adopt a device, make changes,
// un-adopt it, and diff its config against a pre-adoption snapshot — the only
// residue should be nothing."
//
// This is the test that decides whether the project is trustworthy. A wrapper
// that cannot cleanly remove itself does not get installed twice.
//
//	OONFEE_TEST_ADOPT=1 OONFEE_TEST_HOST=192.168.1.1 \
//	OONFEE_TEST_ADMIN_USER=root OONFEE_TEST_ADMIN_PASS= \
//	go test -tags=integration ./internal/daemon/ -run TestIntegrationAdoptUnadoptLeavesNothing -v
func TestIntegrationAdoptUnadoptLeavesNothing(t *testing.T) {
	if os.Getenv("OONFEE_TEST_ADOPT") != "1" {
		t.Skip("set OONFEE_TEST_ADOPT=1 to run the test that writes to a device")
	}
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_ADMIN_USER")
	pass := os.Getenv("OONFEE_TEST_ADMIN_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_ADMIN_USER")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The whole device-visible state, before we touch it. Every UCI config and
	// the ACL directory — the two places anything of ours could land.
	snapshot := func(label string) (configs, acls string) {
		configs = sshRun(t, host, `for c in $(uci show | cut -d. -f1 | sort -u); do uci show "$c"; done`)
		acls = sshRun(t, host, `ls -1 /usr/share/rpcd/acl.d/ | sort`)
		t.Logf("%s: %d config lines, %d ACL files",
			label, len(strings.Split(configs, "\n")), len(strings.Split(acls, "\n")))
		return
	}
	beforeConfigs, beforeACLs := snapshot("before adoption")
	namedBefore := sshRun(t, host,
		`{ uci show 2>/dev/null; ls -1 /usr/share/rpcd/acl.d/; } | grep -i oonfee || true`)

	cfg := testConfig(t, "operator passphrase")
	// testConfig ships a 2-second ApplyDrain, which is right for the unit tests
	// that exercise shutdown and wrong for anything that actually applies:
	// TrackApply gives EVERY apply a context with that deadline, so a real
	// apply — stage, arm a 90s rollback, health-check, confirm — was cancelled
	// mid-flight and reported "unknown" with the change sitting on the device.
	// The production default is 3 minutes for exactly this reason.
	cfg.ApplyDrain = 3 * time.Minute
	d, err := Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if err := d.StartCollector(ctx, collector.Options{
		Baseline: 2 * time.Second, Log: quietLogger(),
	}); err != nil {
		t.Fatalf("StartCollector: %v", err)
	}

	// ---- adopt ----
	res, err := d.Adopt(ctx, api.AdoptRequest{
		Host: host, Username: user, Password: pass, Name: "roundtrip",
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	t.Logf("adopted %s as device %d", res.MAC, res.DeviceID)

	// Take the footprint off even if an assertion below fails.
	//
	// Without this, a failing run leaves its login and ACL on the device, and
	// the NEXT run reports "adoption installed no ACL file" — because the file
	// was already there — which points at adoption rather than at the previous
	// failure. Two runs were lost to exactly that. Best-effort and logged, not
	// asserted: the un-adopt on the happy path below is the one under test, and
	// a second one failing here would be noise attached to the wrong line.
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		out, err := d.Unadopt(context.Background(), api.UnadoptRequest{
			DeviceID: res.DeviceID, Username: user, Password: pass, Force: true,
		})
		t.Logf("cleanup un-adopt after a failed run: removed=%v remains=%v err=%v",
			out != nil && out.Removed, out != nil && out.FootprintRemains, err)
	})

	// It must genuinely be managed before removal proves anything.
	duringConfigs, duringACLs := snapshot("while adopted")
	if duringConfigs == beforeConfigs {
		t.Fatal("adoption changed no configuration; there is nothing to remove")
	}
	if duringACLs == beforeACLs {
		t.Fatal("adoption installed no ACL file; there is nothing to remove")
	}
	// ---- make changes ----
	//
	// The middle step of the proof, and it was missing.
	//
	// ROADMAP Phase 0 asks to "adopt a device, MAKE CHANGES, un-adopt it, and
	// diff its config against a pre-adoption snapshot". This test quoted that
	// line and then went straight from adopt to un-adopt, so nothing was ever
	// owned: `RevertedSections` was necessarily 0 and phase 1 — handing the
	// device's own configuration back — was never exercised at all, here or
	// anywhere on hardware. What the test actually proved was the narrower
	// claim that ADOPTION leaves nothing behind, which is worth having and is
	// not the same thing.
	//
	// A WLAN is the right change to make: it is what the controller is for, it
	// renders onto every radio the device has, and it is the section class
	// un-adopt has to revert.
	// The network and group are created rather than assumed. A fresh data
	// directory seeds only the site row — `networks` and `ap_groups` start
	// empty — so hardcoding ids 1 and 1 would fail a foreign key rather than
	// prove anything.
	//
	// VLAN 1 deliberately: that is the device's existing untagged LAN, which
	// the renderer explicitly does NOT own and will not rewrite, so this adds a
	// WLAN without touching the wired configuration of a live router.
	net := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24",
		Zone: "lan", Enabled: true}
	if err := d.Store.SaveNetwork(ctx, net); err != nil {
		t.Fatalf("SaveNetwork: %v", err)
	}
	grp := &model.APGroup{Name: "roundtrip", DeviceIDs: []int64{res.DeviceID}}
	if err := d.Store.SaveGroup(ctx, grp); err != nil {
		t.Fatalf("SaveGroup: %v", err)
	}
	if err := d.Store.SaveWLAN(ctx, &model.WLAN{
		SSID:      "oonfee-roundtrip",
		NetworkID: net.ID,
		GroupID:   grp.ID,
		Bands:     []model.Band{model.Band2G, model.Band5G},
		// A sentinel, never a real key: this repository is public, and the one
		// time a live passphrase went into a test it went into the test about
		// passphrases not leaking.
		Security: model.Security{Mode: model.SecPSK2, Key: "not-a-real-key-2f8Qv1xLpZ"},
		Enabled:  true,
	}); err != nil {
		t.Fatalf("save the WLAN this test exists to revert: %v", err)
	}
	ares, err := d.ApplySite(ctx, api.ApplyRequest{})
	if err != nil {
		t.Fatalf("ApplySite: %v", err)
	}
	for _, dev := range ares.Devices {
		// The reason, not just the outcome. "unknown" on its own says a human is
		// needed and nothing about what to look at; the engine writes a specific
		// sentence for every branch that produces it.
		t.Logf("applied to %s: %s — %s", dev.Name, dev.Outcome, dev.Reason)
	}
	// Owned sections must exist now, or the un-adopt below reverts nothing and
	// the proof is back to the version that could not fail.
	var owned int
	if err := d.Store.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM owned_sections WHERE device_id=?`, res.DeviceID).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if owned == 0 {
		t.Fatal("the apply recorded no owned sections, so un-adopt has nothing " +
			"to hand back and this test cannot prove what it claims")
	}
	t.Logf("owns %d section(s) on the device", owned)

	// Let it poll, so the removal happens against a live, working install
	// rather than a freshly-written one.
	time.Sleep(6 * time.Second)

	// ---- un-adopt ----
	out, err := d.Unadopt(ctx, api.UnadoptRequest{
		DeviceID: res.DeviceID, Username: user, Password: pass,
	})
	if err != nil {
		t.Fatalf("Unadopt: %v (%v)", err, out)
	}
	cleaned = true // the real removal ran; the safety net above must not repeat it
	t.Logf("un-adopted: reverted=%d login_removed=%v acl_removed=%v remains=%v",
		out.RevertedSections, out.LoginRemoved, out.ACLRemoved, out.FootprintRemains)
	// Phase 1 actually ran. Asserted rather than merely logged, because a zero
	// here with a clean diff below would look like a pass while proving only
	// that adoption tidies up after itself.
	if out.RevertedSections == 0 {
		t.Errorf("no sections were reverted, but %d were owned — phase 1 did "+
			"not hand the device's configuration back", owned)
	}
	for _, e := range out.Errors {
		t.Errorf("un-adopt reported: %s", e)
	}
	if !out.Removed {
		t.Errorf("the device was not removed from the inventory: %+v", out)
	}
	if out.FootprintRemains {
		t.Errorf("a footprint remains: %v", out.Residue)
	}

	// ---- the proof ----
	afterConfigs, afterACLs := snapshot("after un-adoption")

	if afterACLs != beforeACLs {
		t.Errorf("the ACL directory does not match the pre-adoption snapshot.\n%s",
			diffLines(beforeACLs, afterACLs))
	}
	if afterConfigs != beforeConfigs {
		t.Errorf("UCI configuration does not match the pre-adoption snapshot.\n%s",
			diffLines(beforeConfigs, afterConfigs))
	}

	// Nothing of ours by name that was not there already.
	//
	// A delta, not an absolute. A lab device carries scratch configs and SSIDs
	// named after the project from earlier testing, and an operator is perfectly
	// entitled to name their own SSID whatever they like — so the question is
	// what ADOPTION added, not what the string appears in.
	named := func() string {
		return sshRun(t, host,
			`{ uci show 2>/dev/null; ls -1 /usr/share/rpcd/acl.d/; } | grep -i oonfee || true`)
	}
	if added := onlyIn(namedBefore, named()); added != "" {
		t.Errorf("something named after the controller survived removal:\n%s", added)
	}

	// The inventory row is gone, and so is its telemetry.
	if _, err := d.Store.DeviceByMAC(ctx, res.MAC); err == nil {
		t.Error("the device is still in the inventory")
	}
	var series int
	if err := d.Store.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM series`).Scan(&series); err != nil {
		t.Fatal(err)
	}
	if series != 0 {
		t.Errorf("%d telemetry series survived the device that owned them", series)
	}
	if t.Failed() {
		return
	}
	t.Log("device is byte-for-byte as it was before adoption")
}

// onlyIn returns the lines present in after but not in before.
func onlyIn(before, after string) string {
	was := map[string]bool{}
	for _, l := range strings.Split(before, "\n") {
		was[strings.TrimSpace(l)] = true
	}
	var out []string
	for _, l := range strings.Split(after, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !was[l] {
			out = append(out, "  "+l)
		}
	}
	return strings.Join(out, "\n")
}
