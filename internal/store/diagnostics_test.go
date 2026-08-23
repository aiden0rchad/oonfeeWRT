package store

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
)

func TestDiagnosticEvidenceIsBoundedAndSecretSelective(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	registry := capability.NewRegistry()
	registry.Board = capability.Board{Model: "Test Router", Target: "ath79/generic", Kernel: "6.6.1", Release: "OpenWrt test"}
	registry.Set(capability.FeatDSA, capability.Present)
	caps, _ := json.Marshal(registry)
	device := &Device{MAC: "02:00:00:00:00:01", Host: "router.example", Name: "Hall AP",
		Role: "ap", CapsJSON: string(caps), FWRelease: "fallback"}
	if err := db.UpsertDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO device_capability_installs
(device_id,capability,package_manager,requested_packages_json,baseline_packages_json,added_packages_json,services_json,state,detail,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?)`, device.ID, "lldp", "apk", `[]`, `[]`, `[]`, `[]`, "installed", "", 1); err != nil {
		t.Fatal(err)
	}
	const observedMS = int64(1_700_000_123_456)
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO topology_source_states
(device_id,source,state,reason,observed_at) VALUES (?,?,?,?,?)`, device.ID, "lldp", "error", "permission denied", observedMS); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO clients
(mac,name,note,ip,fixed_ip) VALUES (?,?,?,?,?)`, "02:00:00:00:00:02", "Kitchen Camera", "private note", "192.0.2.8", "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO site(id,uuid,name) VALUES (1,'diagnostic-site','Diagnostic Site')
ON CONFLICT(id) DO UPDATE SET name=excluded.name;
INSERT INTO networks(id,name,vlan,cidr,zone) VALUES (91,'Diagnostic Network',4091,'198.51.100.0/24','Diagnostic Network Zone');
INSERT INTO ap_groups(id,name) VALUES (91,'Diagnostic AP Group');
INSERT INTO wlans(id,ssid,network_id,group_id,security_json,security_key_enc) VALUES (91,'Diagnostic WLAN',91,91,'{}',X'736563726574');
INSERT INTO meshes(id,mesh_id,network_id,group_id,band,key,key_enc) VALUES (91,'Diagnostic Mesh',91,91,'5g','','736563726574');
INSERT INTO zones(id,name,policy_json) VALUES (91,'Diagnostic Zone','{}');
INSERT INTO foreign_ssid_notes(device_id,section,ssid,note,decided_at,decided_by) VALUES (?,'radio91','Foreign Diagnostic WLAN','foreign private note',1,'tester')`, device.ID); err != nil {
		t.Fatal(err)
	}
	owner, err := db.CreateFirstAdmin(ctx, "OwnerVisible", "hash")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := db.CreateAdmin(ctx, "DeletedAlias", "hash", RoleViewer,
		AccountActor{AdminID: owner.ID, Username: owner.Username})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE admins SET deleted_at=? WHERE id=?`, time.Now().Unix(), deleted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO events
(ts,category,severity,event,detail_json,source,action) VALUES (?,?,?,?,?,?,?)`,
		1_700_000_123, "audit", "info", "test.event", `{"login":"router-secret"}`, "controller", "read"); err != nil {
		t.Fatal(err)
	}

	controller, err := db.DiagnosticController(ctx)
	if err != nil || controller.Schema != schemaVersion || controller.Health != "healthy" {
		t.Fatalf("controller=%+v err=%v", controller, err)
	}
	devices, truncated, err := db.DiagnosticDevices(ctx, diagnosticDeviceLimit)
	if err != nil || truncated || len(devices) != 1 {
		t.Fatalf("devices=%+v truncated=%v err=%v", devices, truncated, err)
	}
	got := devices[0]
	if got.Host != "router.example" || got.Model != "Test Router" || got.Target != "ath79/generic" ||
		got.PackageManager != "apk" || !strings.Contains(got.CapabilityState, "managed:lldp=installed") {
		t.Fatalf("device=%+v", got)
	}
	sources, truncated, err := db.DiagnosticSources(ctx, diagnosticSourceLimit)
	if err != nil || truncated || len(sources) != 1 || sources[0].Detail != "permission denied" ||
		!sources[0].ObservedAt.Equal(time.UnixMilli(observedMS).UTC()) {
		t.Fatalf("sources=%+v truncated=%v err=%v", sources, truncated, err)
	}
	identifiers, _, err := db.DiagnosticIdentifiers(ctx, diagnosticIdentifierLimit)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, identifier := range identifiers {
		joined += identifier.Kind + "=" + identifier.Value + "\n"
	}
	for _, want := range []string{
		"site=Diagnostic Site", "network=Diagnostic Network", "zone=Diagnostic Network Zone",
		"group=Diagnostic AP Group", "wlan=Diagnostic WLAN", "mesh=Diagnostic Mesh",
		"zone=Diagnostic Zone", "wlan=Foreign Diagnostic WLAN", "client=Kitchen Camera",
		"client=02:00:00:00:00:02", "address=192.0.2.8", "account=DeletedAlias",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("identifiers missing %q: %s", want, joined)
		}
	}
	for _, forbidden := range []string{
		"private note", "foreign private note", "192.0.2.9", "198.51.100.0/24",
		"router-secret", "736563726574",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("identifiers leaked %q: %s", forbidden, joined)
		}
	}
	prioritized, truncated, err := db.DiagnosticIdentifiers(ctx, 1)
	if err != nil || !truncated || len(prioritized) != 1 || prioritized[0].Value != "Diagnostic Site" {
		t.Fatalf("prioritized identifiers=%+v truncated=%v err=%v", prioritized, truncated, err)
	}
	events, _, err := db.DiagnosticEvents(ctx, "audit", diagnosticEventLimit)
	if err != nil {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	var diagnosticEvent *DiagnosticEvent
	for i := range events {
		if events[i].Event == "test.event" {
			diagnosticEvent = &events[i]
		}
	}
	if diagnosticEvent == nil {
		t.Fatalf("events=%+v", events)
	}
	if strings.Contains(strings.Join([]string{diagnosticEvent.Event, diagnosticEvent.Source, diagnosticEvent.Action}, " "), "router-secret") {
		t.Fatal("diagnostic event projection selected detail_json")
	}
}

func TestDiagnosticReadLimitsRejectOverflowBeforeSQL(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if _, _, err := db.DiagnosticDevices(ctx, math.MaxInt); err == nil {
		t.Fatal("DiagnosticDevices accepted MaxInt")
	}
	if _, _, err := db.DiagnosticSources(ctx, math.MaxInt); err == nil {
		t.Fatal("DiagnosticSources accepted MaxInt")
	}
	if _, _, err := db.DiagnosticIdentifiers(ctx, math.MaxInt); err == nil {
		t.Fatal("DiagnosticIdentifiers accepted MaxInt")
	}
	if _, _, err := db.DiagnosticEvents(ctx, "general", math.MaxInt); err == nil {
		t.Fatal("DiagnosticEvents accepted MaxInt")
	}
}

func TestDiagnosticQueriesBoundMultiMegabyteCorruptText(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := &Device{MAC: "02:00:00:00:00:91", Host: "router.example", Name: "Corrupt fixture",
		Role: "ap", CapsJSON: "{}"}
	if err := db.UpsertDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("x", 3<<20)
	if _, err := db.SQL().ExecContext(ctx, `PRAGMA ignore_check_constraints=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE devices SET caps_json=? WHERE id=?`, huge, device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO device_capability_installs
(device_id,capability,package_manager,requested_packages_json,baseline_packages_json,added_packages_json,services_json,state,detail,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?)`, device.ID, huge, huge, `[]`, `[]`, `[]`, `[]`, huge, "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO topology_source_states
(device_id,source,state,reason,observed_at) VALUES (?,?,?,?,?)`, device.ID, huge, huge, huge, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO clients(mac,name,ip) VALUES (?,?,?)`,
		"02:00:00:00:00:92", huge, "192.0.2.92"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"topology_source_states", "topology_edges", "radio_scans", "ingest_cursors"} {
		var count int
		if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil ||
			table == "topology_source_states" && count != 1 || table != "topology_source_states" && count != 0 {
			t.Fatalf("unexpected %s count=%d err=%v", table, count, err)
		}
	}

	devices, _, err := db.DiagnosticDevices(ctx, diagnosticDeviceLimit)
	if err != nil || len(devices) != 1 {
		t.Fatalf("devices=%+v err=%v", devices, err)
	}
	if len(devices[0].PackageManager) > diagnosticTextLimit || len(devices[0].CapabilityState) > diagnosticTextLimit ||
		!containsDiagnosticGap(devices[0].Gaps, "capability registry was truncated") ||
		!containsDiagnosticGap(devices[0].Gaps, "capability installation text evidence was truncated") {
		t.Fatalf("unbounded device evidence=%+v", devices[0])
	}
	sources, sourceTruncated, err := db.DiagnosticSources(ctx, diagnosticSourceLimit)
	if err != nil || !sourceTruncated || len(sources) != 1 || len(sources[0].Kind) > diagnosticTextLimit ||
		len(sources[0].State) > diagnosticTextLimit || len(sources[0].Detail) > diagnosticTextLimit {
		t.Fatalf("sources=%+v truncated=%v err=%v", sources, sourceTruncated, err)
	}
	identifiers, identifierTruncated, err := db.DiagnosticIdentifiers(ctx, diagnosticIdentifierLimit)
	if err != nil || !identifierTruncated {
		t.Fatalf("identifiers truncated=%v err=%v", identifierTruncated, err)
	}
	for _, identifier := range identifiers {
		if len(identifier.Value) > diagnosticTextLimit {
			t.Fatalf("unbounded identifier length=%d", len(identifier.Value))
		}
	}
}

func containsDiagnosticGap(gaps []string, want string) bool {
	for _, gap := range gaps {
		if gap == want {
			return true
		}
	}
	return false
}
