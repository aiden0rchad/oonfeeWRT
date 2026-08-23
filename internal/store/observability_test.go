package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func TestSchema16MigratesEventProvenanceWithoutLosingLegacyRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v15.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
INSERT INTO events (ts,category,severity,event,detail_json)
VALUES (123,'system','info','legacy','{}')`); err != nil {
		t.Fatal(err)
	}
	downgradeFixtureToV15(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("migrate v15: %v", err)
	}
	defer db.Close()
	var version int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version=%d, want %d", version, schemaVersion)
	}
	events, err := db.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "legacy" ||
		events[0].Source != "controller" || events[0].IngestedAt != 123000 {
		t.Fatalf("migrated legacy event=%+v", events)
	}
}

func TestSchema16IsADowngradeBoundaryWithoutRepeatingSecretMigration(t *testing.T) {
	if schemaVersion != 19 || secretSchemaVersion != 14 {
		t.Fatalf("schema epochs=(%d,%d), want (19,14)", schemaVersion, secretSchemaVersion)
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE schema_version SET version=?`, schemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if newer, err := Open(ctx, driver, path, testProtector(t, path)); err == nil {
		newer.Close()
		t.Fatal("v19 build accepted a v20 database")
	} else if !strings.Contains(err.Error(), "refusing to downgrade") {
		t.Fatalf("downgrade error=%v", err)
	}
}

func TestSchema16AttestationRejectsCollidingTableAndRollsBackMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "collision.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
INSERT INTO events(ts,category,severity,event,detail_json)
VALUES(77,'system','info','survives','{}')`); err != nil {
		t.Fatal(err)
	}
	downgradeFixtureToV15(t, db)
	// Same name and superficially similar columns, but wrong types/nullability
	// and no primary key. IF NOT EXISTS alone would accept this collision.
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE ingest_cursors (
		device_id TEXT, source TEXT, boot_id TEXT, cursor TEXT, updated_at INTEGER
	)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if migrated, err := Open(ctx, driver, path, testProtector(t, path)); err == nil {
		migrated.Close()
		t.Fatal("migration accepted a colliding malformed v16 table")
	} else if !strings.Contains(err.Error(), "schema v16 attestation") {
		t.Fatalf("collision error=%v", err)
	}

	raw, err := sql.Open(driver, path)
	if err != nil {
		t.Fatal(err)
	}
	var version, events int
	if err := raw.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event='survives'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if version != 15 || events != 1 {
		t.Fatalf("failed migration changed durable state: version=%d events=%d", version, events)
	}
	var created int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='topology_edges'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatal("failed migration committed a new v16 table")
	}
	if _, err := raw.ExecContext(ctx, `DROP TABLE ingest_cursors`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("migration after removing collision: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A current, attested database must reopen without rewriting its version.
	db, err = Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("reopen attested v16 database: %v", err)
	}
	db.Close()
}

func TestSchema16AttestationRejectsNonExactIndexesAndChecks(t *testing.T) {
	tests := []struct {
		name       string
		statements []string
		wantError  string
	}{
		{
			name:       "missing events timestamp index",
			statements: []string{`DROP INDEX events_ts`},
			wantError:  "index events_ts is missing",
		},
		{
			name: "extra partial index predicate",
			statements: []string{
				`DROP INDEX events_source_identity`,
				`CREATE UNIQUE INDEX events_source_identity
				 ON events(device_id, source, source_boot, source_id)
				 WHERE source_id IS NOT NULL AND trim(source_id) <> '' AND 0`,
			},
			wantError: "index events_source_identity has the wrong predicate",
		},
		{
			name: "extra restrictive check",
			statements: []string{
				`DROP TABLE topology_source_states`,
				`CREATE TABLE topology_source_states (
				 device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
				 source TEXT NOT NULL,
				 state TEXT NOT NULL CHECK (state IN ('unknown','empty','observed','error')),
				 reason TEXT NOT NULL DEFAULT '',
				 observed_at INTEGER NOT NULL,
				 PRIMARY KEY (device_id, source),
				 CHECK /* comments cannot hide an extra constraint */ (observed_at >= 0)
				 ) WITHOUT ROWID`,
			},
			wantError: "table topology_source_states has CHECK constraints",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "malformed-v16.db")
			protector := testProtector(t, path)
			db, err := Open(ctx, driver, path, protector)
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range test.statements {
				if _, err := db.SQL().ExecContext(ctx, statement); err != nil {
					t.Fatalf("mutate schema (%s): %v", statement, err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			if reopened, err := OpenReadOnly(ctx, driver, path, protector); err == nil {
				reopened.Close()
				t.Fatal("read-only open accepted a malformed schema v16 database")
			} else if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("attestation error=%v, want %q", err, test.wantError)
			}
		})
	}
}

func downgradeFixtureToV15(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	statements := []string{
		`DROP TABLE radio_scan_bss`,
		`DROP TABLE radio_scans`,
		`DROP TABLE topology_source_states`,
		`DROP TABLE topology_edges`,
		`DROP TABLE ingest_cursors`,
		`DROP INDEX events_source_identity`,
		`DROP INDEX events_client_time`,
		`DROP INDEX events_category_time`,
		`DROP INDEX events_severity_time`,
		`DROP INDEX events_ts`,
		`ALTER TABLE events RENAME TO events_v16_fixture`,
		`CREATE TABLE events (
		 id INTEGER PRIMARY KEY, ts INTEGER NOT NULL,
		 device_id INTEGER, category TEXT NOT NULL,
		 severity TEXT NOT NULL, event TEXT NOT NULL,
		 detail_json TEXT NOT NULL DEFAULT '{}')`,
		`INSERT INTO events(id,ts,device_id,category,severity,event,detail_json)
		 SELECT id,ts,device_id,category,severity,event,detail_json FROM events_v16_fixture`,
		`DROP TABLE events_v16_fixture`,
		`CREATE INDEX events_ts ON events(ts)`,
		`UPDATE schema_version SET version=15`,
	}
	for _, stmt := range statements {
		if _, err := db.SQL().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare v15 fixture (%s): %v", stmt, err)
		}
	}
}

func TestEventProvenanceDedupesReplayAndKeysetPagesTies(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := addObservationDevice(t, db)
	port := 443
	e := Event{
		TS: 200, DeviceID: &device.ID, Category: "security", Severity: "warning",
		Event: "firewall.drop", Source: "logd", SourceBoot: "boot-a:logd-7",
		SourceID: "4294967295", ClientMAC: "AA:BB:CC:DD:EE:01",
		Action: "drop", Direction: "out", InIface: "br-lan", OutIface: "wan",
		SrcIP: "192.0.2.2", DstIP: "198.51.100.9", DstPort: &port,
		ZoneIn: "lan", ZoneOut: "wan", Detail: map[string]any{"raw": "sanitized"},
	}
	inserted, err := db.AppendEvent(ctx, e)
	if err != nil || !inserted {
		t.Fatalf("first event inserted=%v err=%v", inserted, err)
	}
	inserted, err = db.AppendEvent(ctx, e)
	if err != nil || inserted {
		t.Fatalf("replay inserted=%v err=%v", inserted, err)
	}
	malformed := e
	malformed.SourceBoot = ""
	if _, err := db.AppendEvent(ctx, malformed); err == nil {
		t.Fatal("producer id without a boot/generation identity was accepted")
	}
	e.SourceBoot = "boot-b:logd-1"
	if inserted, err = db.AppendEvent(ctx, e); err != nil || !inserted {
		t.Fatalf("new producer epoch inserted=%v err=%v", inserted, err)
	}
	for i := 0; i < 5; i++ {
		if err := db.LogEvent(ctx, Event{TS: 200, Category: "system", Severity: "info",
			Event: "tie"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := db.QueryEventsBefore(ctx, "", "", "", nil, 3)
	if err != nil || len(first) != 3 {
		t.Fatalf("first page len=%d err=%v", len(first), err)
	}
	cursor := EventCursor{TS: first[2].TS, ID: first[2].ID}
	second, err := db.QueryEventsBefore(ctx, "", "", "", &cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, event := range append(first, second...) {
		if seen[event.ID] {
			t.Fatalf("event id %d repeated across keyset pages", event.ID)
		}
		seen[event.ID] = true
	}
	if len(seen) != 7 {
		t.Fatalf("keyset pages saw %d events, want 7", len(seen))
	}
	var enriched Event
	for _, event := range append(first, second...) {
		if event.Source == "logd" {
			enriched = event
			break
		}
	}
	if enriched.ClientMAC != "aa:bb:cc:dd:ee:01" || enriched.DstPort == nil ||
		*enriched.DstPort != 443 || enriched.ZoneOut != "wan" {
		t.Fatalf("event enrichment did not round trip: %+v", enriched)
	}
	if _, err := db.QueryEventsBefore(ctx, "", "", "", &EventCursor{TS: 1}, 10); err == nil {
		t.Fatal("malformed keyset cursor was accepted")
	}
}

func TestLatestClientAssociationEventsRestoresOnlyNewestSourcedState(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	first := addObservationDevice(t, db)
	second := &Device{MAC: "02:00:00:00:00:17", Host: "192.0.2.17", Name: "p4-b"}
	if err := db.UpsertDevice(ctx, second); err != nil {
		t.Fatal(err)
	}
	clientA, clientB := "02:00:00:00:10:01", "02:00:00:00:10:02"
	seed := []Event{
		{TS: 10, DeviceID: &first.ID, Category: "client", Severity: "info",
			Event: "client.connect", Source: "logd", SourceBoot: "boot:a", SourceID: "1",
			ClientMAC: clientA, Action: "connect", InIface: "phy0-ap0"},
		{TS: 20, DeviceID: &first.ID, Category: "client", Severity: "info",
			Event: "client.disconnect", Source: "logd", SourceBoot: "boot:a", SourceID: "2",
			ClientMAC: clientA, Action: "disconnect", InIface: "phy0-ap0"},
		{TS: 10, DeviceID: &first.ID, Category: "client", Severity: "info",
			Event: "client.connect", Source: "logd", SourceBoot: "boot:a", SourceID: "3",
			ClientMAC: clientB, Action: "connect", InIface: "phy0-ap0"},
		{TS: 20, DeviceID: &second.ID, Category: "client", Severity: "info",
			Event: "client.roam", Source: "logd", SourceBoot: "boot:b", SourceID: "1",
			ClientMAC: clientB, Action: "roam", InIface: "phy1-ap0"},
		// Newer but unsourced: an API/audit event cannot replace producer state.
		{TS: 30, DeviceID: &first.ID, Category: "client", Severity: "info",
			Event: "client.connect", ClientMAC: clientA, Action: "connect", InIface: "wrong"},
	}
	for _, event := range seed {
		if err := db.LogEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.LatestClientAssociationEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ClientMAC != clientA || got[1].ClientMAC != clientB {
		t.Fatalf("association seed order/content=%+v", got)
	}
	if got[0].Action != "disconnect" {
		t.Fatalf("latest disconnect was suppressed by stale/unsourced connect: %+v", got[0])
	}
	if got[1].Action != "roam" || got[1].DeviceID == nil || *got[1].DeviceID != second.ID ||
		got[1].InIface != "phy1-ap0" {
		t.Fatalf("latest roam lost device/interface provenance: %+v", got[1])
	}
}

func TestLatestClientAssociationEventsRejectsSeedsBeforeAnyProducerGap(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	first := addObservationDevice(t, db)
	second := &Device{MAC: "02:00:00:00:00:18", Host: "192.0.2.18", Name: "p4-gap"}
	if err := db.UpsertDevice(ctx, second); err != nil {
		t.Fatal(err)
	}
	client := "02:00:00:00:10:03"
	if err := db.LogEvent(ctx, Event{
		TS: 1, DeviceID: &first.ID, Category: "client", Severity: "info",
		Event: "client.connect", Source: "logd", SourceBoot: "boot:a", SourceID: "1",
		ClientMAC: client, Action: "connect", InIface: "phy0-ap0", IngestedAt: 1_000,
	}); err != nil {
		t.Fatal(err)
	}
	// A newer transition on the second AP was cap-pruned. Its durable gap must
	// prevent the older first-AP connect from being resurrected after restart.
	if err := db.SaveIngestCursor(ctx, IngestCursor{
		DeviceID: second.ID, Source: "logd", BootID: "boot:b", Cursor: "9",
		UpdatedAt: 3_000, ContinuityGapAt: 2_000,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.LatestClientAssociationEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("association before another AP's gap was restored: %+v", got)
	}
	if err := db.LogEvent(ctx, Event{
		TS: 4, DeviceID: &first.ID, Category: "client", Severity: "info",
		Event: "client.connect", Source: "logd", SourceBoot: "boot:a", SourceID: "2",
		ClientMAC: client, Action: "connect", InIface: "phy0-ap0", IngestedAt: 4_000,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = db.LatestClientAssociationEvents(ctx)
	if err != nil || len(got) != 1 || got[0].SourceID != "2" {
		t.Fatalf("post-gap association was not restored: %+v err=%v", got, err)
	}
}

func TestIngestCursorRoundTripAndDeviceCascade(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := addObservationDevice(t, db)
	if err := db.SaveIngestCursor(ctx, IngestCursor{DeviceID: device.ID,
		Source: "logd", BootID: "boot:pid", Cursor: "9", UpdatedAt: 20,
		ContinuityGapAt: 10}); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadIngestCursor(ctx, device.ID, "logd")
	if err != nil || got.BootID != "boot:pid" || got.Cursor != "9" || got.ContinuityGapAt != 10 {
		t.Fatalf("cursor=%+v err=%v", got, err)
	}
	listed, err := db.IngestCursorsBySource(ctx, "logd")
	if err != nil || len(listed) != 1 || listed[0] != got {
		t.Fatalf("listed cursors=%+v err=%v", listed, err)
	}
	if _, err := db.IngestCursorsBySource(ctx, " "); err == nil {
		t.Fatal("blank cursor source was accepted")
	}
	if err := db.SaveIngestCursor(ctx, IngestCursor{DeviceID: device.ID, Source: "logd"}); err == nil {
		t.Fatal("blank cursor identity was accepted")
	}
	if err := db.SaveIngestCursor(ctx, IngestCursor{DeviceID: device.ID,
		Source: "logd", BootID: "boot:pid", Cursor: "10", ContinuityGapAt: -1}); err == nil {
		t.Fatal("negative cursor gap time was accepted")
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM devices WHERE id=?`, device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LoadIngestCursor(ctx, device.ID, "logd"); err != ErrNotFound {
		t.Fatalf("cursor survived device delete: %v", err)
	}
}

func TestIngestCursorContinuityGapCannotBeErasedByAStaleWriter(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := addObservationDevice(t, db)
	first := IngestCursor{DeviceID: device.ID, Source: "logd", BootID: "boot:1:0",
		Cursor: "2:2000", UpdatedAt: 2_000, ContinuityGapAt: 1_500}
	if err := db.SaveIngestCursor(ctx, first); err != nil {
		t.Fatal(err)
	}
	stale := first
	stale.Cursor, stale.UpdatedAt, stale.ContinuityGapAt = "3:3000", 3_000, 0
	if err := db.SaveIngestCursor(ctx, stale); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadIngestCursor(ctx, device.ID, "logd")
	if err != nil || got.Cursor != stale.Cursor || got.ContinuityGapAt != first.ContinuityGapAt {
		t.Fatalf("cursor=%+v err=%v", got, err)
	}
}

func TestAppendEventsAndCursorIsAtomicBoundedAndReplaySafe(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := addObservationDevice(t, db)
	cursor := IngestCursor{DeviceID: device.ID, Source: "logd",
		BootID: "boot:pid:0", Cursor: "2:2000", UpdatedAt: 2000}
	events := []Event{
		{TS: 1, DeviceID: &device.ID, Category: "system", Severity: "info",
			Event: "one", Source: "logd", SourceBoot: cursor.BootID, SourceID: "1"},
		{TS: 2, DeviceID: &device.ID, Category: "system", Severity: "info",
			Event: "two", Source: "logd", SourceBoot: cursor.BootID, SourceID: "2"},
	}
	inserted, err := db.AppendEventsAndCursor(ctx, events, cursor)
	if err != nil || inserted != 2 {
		t.Fatalf("first batch inserted=%d err=%v", inserted, err)
	}
	inserted, err = db.AppendEventsAndCursor(ctx, events, cursor)
	if err != nil || inserted != 0 {
		t.Fatalf("replay inserted=%d err=%v", inserted, err)
	}
	wrappedCursor := cursor
	wrappedCursor.BootID = "boot:pid:1"
	wrappedCursor.Cursor = "0:3000"
	wrappedCursor.UpdatedAt = 3000
	wrapped := Event{TS: 3, DeviceID: &device.ID, Category: "system", Severity: "info",
		Event: "wrapped", Source: "logd", SourceBoot: wrappedCursor.BootID, SourceID: "1"}
	if inserted, err = db.AppendEventsAndCursor(ctx, []Event{wrapped}, wrappedCursor); err != nil || inserted != 1 {
		t.Fatalf("wrapped generation inserted=%d err=%v", inserted, err)
	}
	gotCursor, err := db.LoadIngestCursor(ctx, device.ID, "logd")
	if err != nil || gotCursor.Cursor != wrappedCursor.Cursor {
		t.Fatalf("cursor=%+v err=%v", gotCursor, err)
	}
	mismatched := events[0]
	mismatched.SourceBoot = "different:producer:0"
	if _, err := db.AppendEventsAndCursor(ctx, []Event{mismatched}, cursor); err == nil {
		t.Fatal("sourced event identity was allowed to diverge from its cursor")
	}

	cursor = wrappedCursor
	next := cursor
	next.Cursor, next.UpdatedAt = "4:4000", 4000
	bad := append(slices.Clone(events[:1]), Event{
		TS: 3, DeviceID: &device.ID, Category: "system", Severity: "info",
		Event: "bad-json", Source: "logd", SourceBoot: cursor.BootID, SourceID: "3",
		Detail: func() {},
	})
	if _, err := db.AppendEventsAndCursor(ctx, bad, next); err == nil {
		t.Fatal("batch accepted an event whose detail cannot be sanitized/encoded")
	}
	gotCursor, _ = db.LoadIngestCursor(ctx, device.ID, "logd")
	if gotCursor.Cursor != cursor.Cursor {
		t.Fatalf("validation failure advanced cursor to %q", gotCursor.Cursor)
	}

	if _, err := db.SQL().ExecContext(ctx, `CREATE TRIGGER fail_test_event
		BEFORE INSERT ON events WHEN NEW.event='sql-fail'
		BEGIN SELECT RAISE(ABORT,'forced event failure'); END`); err != nil {
		t.Fatal(err)
	}
	sqlFailure := []Event{
		{TS: 3, DeviceID: &device.ID, Category: "system", Severity: "info",
			Event: "three", Source: "logd", SourceBoot: cursor.BootID, SourceID: "3"},
		{TS: 4, DeviceID: &device.ID, Category: "system", Severity: "info",
			Event: "sql-fail", Source: "logd", SourceBoot: cursor.BootID, SourceID: "4"},
	}
	if _, err := db.AppendEventsAndCursor(ctx, sqlFailure, next); err == nil {
		t.Fatal("forced SQL failure was reported as success")
	}
	gotCursor, _ = db.LoadIngestCursor(ctx, device.ID, "logd")
	if gotCursor.Cursor != cursor.Cursor {
		t.Fatalf("SQL failure advanced cursor to %q", gotCursor.Cursor)
	}
	var count int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE source='logd'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("failed transaction left %d events, want original 3", count)
	}
	if _, err := db.AppendEventsAndCursor(ctx, make([]Event, 513), next); err == nil {
		t.Fatal("batch larger than 512 was accepted")
	}
}

func TestTopologyIntervalsUnknownStateAndMalformedRowsFailClosed(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := addObservationDevice(t, db)
	edge := &model.TopologyEdge{
		ChildNode: "client:aa:bb:cc:dd:ee:01", ChildMAC: "AA:BB:CC:DD:EE:01",
		ParentNode: "device:" + device.MAC, ParentDeviceID: &device.ID,
		ParentPort: "lan3", Medium: "wired", Confidence: "inferred",
		ValidFrom: 100, LastSeen: 120,
		Evidence: []model.TopologyEvidence{{Kind: "fdb", Source: "brctl"}},
	}
	if err := db.SaveTopologyEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}
	if edge.ID == 0 || edge.ChildMAC != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("edge not identified/canonicalized: %+v", edge)
	}
	if err := db.SaveTopologySourceState(ctx, model.TopologySourceObservation{
		DeviceID: device.ID, Source: "brctl", State: model.TopologySourceEmpty,
		ObservedAt: 120}); err != nil {
		t.Fatal(err)
	}
	states, err := db.TopologySourceStates(ctx)
	if err != nil || len(states) != 1 || states[0].State != model.TopologySourceEmpty {
		t.Fatalf("source states=%+v err=%v", states, err)
	}
	current, err := db.TopologyEdgesAt(ctx, 0)
	if err != nil || len(current) != 1 {
		t.Fatalf("current edges=%+v err=%v", current, err)
	}
	closed := int64(130)
	edge.ValidTo = &closed
	if err := db.SaveTopologyEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}
	if current, err = db.TopologyEdgesAt(ctx, 0); err != nil || len(current) != 0 {
		t.Fatalf("closed edge remained current: %+v err=%v", current, err)
	}
	if history, err := db.TopologyEdgesAt(ctx, 125); err != nil || len(history) != 1 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	if history, truncated, err := db.TopologyEdgesBetween(ctx, 110, 140, 100); err != nil || truncated ||
		len(history) != 1 || history[0].ID != edge.ID {
		t.Fatalf("interval history=%+v truncated=%v err=%v", history, truncated, err)
	}
	if _, _, err := db.TopologyEdgesBetween(ctx, 140, 110, 100); err == nil {
		t.Fatal("inverted topology replay interval was accepted")
	}
	bad := *edge
	bad.ID = 0
	bad.ChildNode = "device:not-a-mac"
	if err := db.SaveTopologyEdge(ctx, &bad); err == nil {
		t.Fatal("malformed topology identity was accepted")
	}
	bad = *edge
	bad.ID = 0
	bad.Evidence = []model.TopologyEvidence{{Source: "brctl"}}
	if err := db.SaveTopologyEdge(ctx, &bad); err == nil {
		t.Fatal("malformed topology evidence was accepted")
	}
	if _, err := db.SQL().ExecContext(ctx, `
INSERT INTO topology_edges
 (child_node,parent_node,medium,confidence,valid_from,valid_to,last_seen)
VALUES ('mac:aa:bb:cc:dd:ee:02','synthetic:internet','wired','unknown',20,10,20)`); err == nil {
		t.Fatal("database accepted an inverted topology interval")
	}
}

func TestTopologyEdgesBetweenReturnsNewestBoundedIntervalsChronologically(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	for i, validFrom := range []int64{100, 200, 300} {
		validTo := validFrom + 50
		edge := &model.TopologyEdge{
			ChildNode:  fmt.Sprintf("mac:02:00:00:00:00:%02x", i+1),
			ParentNode: "synthetic:internet", Medium: "uplink", Confidence: "measured",
			ValidFrom: validFrom, ValidTo: &validTo, LastSeen: validTo - 1,
			Evidence: []model.TopologyEvidence{}, Ambiguities: []string{},
		}
		if err := db.SaveTopologyEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}

	edges, truncated, err := db.TopologyEdgesBetween(ctx, 50, 400, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(edges) != 2 || edges[0].ValidFrom != 200 || edges[1].ValidFrom != 300 {
		t.Fatalf("edges=%+v truncated=%v", edges, truncated)
	}
	if _, _, err := db.TopologyEdgesBetween(ctx, 50, 400, 0); err == nil {
		t.Fatal("zero topology history limit was accepted")
	}
}

func TestLatestClosedTopologyEdgesSinceReturnsOnePerChild(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	ptr := func(value int64) *int64 { return &value }
	for _, edge := range []*model.TopologyEdge{
		{ChildNode: "device:02:00:00:00:00:01", ParentNode: "device:02:00:00:00:00:11", Medium: "wired", Confidence: "measured", ValidFrom: 100, ValidTo: ptr(int64(150)), LastSeen: 149},
		{ChildNode: "device:02:00:00:00:00:01", ParentNode: "device:02:00:00:00:00:12", ParentPort: "lan3", Medium: "wired", Confidence: "measured", ValidFrom: 200, ValidTo: ptr(int64(250)), LastSeen: 249},
		{ChildNode: "device:02:00:00:00:00:02", ParentNode: "device:02:00:00:00:00:13", Medium: "wired", Confidence: "measured", ValidFrom: 210, ValidTo: ptr(int64(260)), LastSeen: 259},
	} {
		edge.Evidence, edge.Ambiguities = []model.TopologyEvidence{}, []string{}
		if err := db.SaveTopologyEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}
	edges, err := db.LatestClosedTopologyEdgesSince(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 || edges[0].ParentNode != "device:02:00:00:00:00:12" || edges[0].ParentPort != "lan3" || edges[1].ChildNode != "device:02:00:00:00:00:02" {
		t.Fatalf("latest closed edges=%+v", edges)
	}
	if _, err := db.LatestClosedTopologyEdgesSince(ctx, 0); err == nil {
		t.Fatal("zero cutoff accepted")
	}
}

func TestApplyTopologyObservationCommitsOrRollsBackAsOneUnit(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := addObservationDevice(t, db)
	edge := func(lastByte string, from, seen int64) model.TopologyEdge {
		mac := "aa:bb:cc:dd:ee:" + lastByte
		return model.TopologyEdge{
			ChildNode: "client:" + mac, ChildMAC: mac,
			ParentNode: "device:" + device.MAC, ParentDeviceID: &device.ID,
			ParentPort: "lan1", Medium: "wired", Confidence: "observed",
			ValidFrom: from, LastSeen: seen,
			Evidence: []model.TopologyEvidence{{Kind: "fdb", Source: "brctl", DeviceID: &device.ID}},
		}
	}
	continuing, closing := edge("21", 100, 120), edge("22", 100, 120)
	if err := db.SaveTopologyEdge(ctx, &continuing); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveTopologyEdge(ctx, &closing); err != nil {
		t.Fatal(err)
	}
	continuing.LastSeen = 200
	closedAt := int64(200)
	closing.ValidTo = &closedAt
	opened := edge("23", 200, 200)
	if err := db.ApplyTopologyObservation(ctx, TopologyChanges{
		Open: []model.TopologyEdge{opened}, Update: []model.TopologyEdge{continuing},
		Close: []model.TopologyEdge{closing},
	}, []model.TopologySourceObservation{{
		DeviceID: device.ID, Source: "brctl", State: model.TopologySourceObserved, ObservedAt: 200,
	}}); err != nil {
		t.Fatal(err)
	}
	current, err := db.TopologyEdgesAt(ctx, 0)
	if err != nil || len(current) != 2 {
		t.Fatalf("current after apply=%+v err=%v", current, err)
	}
	byChild := map[string]model.TopologyEdge{}
	for _, got := range current {
		byChild[got.ChildNode] = got
	}
	if byChild[continuing.ChildNode].LastSeen != 200 || byChild[opened.ChildNode].ID == 0 {
		t.Fatalf("atomic apply did not update and open: %+v", current)
	}
	if history, err := db.TopologyEdgesAt(ctx, 150); err != nil || len(history) != 2 {
		t.Fatalf("closed history=%+v err=%v", history, err)
	}
	states, err := db.TopologySourceStates(ctx)
	if err != nil || len(states) != 1 || states[0].ObservedAt != 200 {
		t.Fatalf("source states=%+v err=%v", states, err)
	}

	if _, err := db.SQL().ExecContext(ctx, `
CREATE TRIGGER reject_forced_topology_source
BEFORE INSERT ON topology_source_states WHEN NEW.source='forced'
BEGIN SELECT RAISE(ABORT, 'forced topology rollback'); END`); err != nil {
		t.Fatal(err)
	}
	continuing = byChild[continuing.ChildNode]
	continuing.LastSeen = 300
	opened = byChild[opened.ChildNode]
	rollbackClosedAt := int64(300)
	opened.ValidTo = &rollbackClosedAt
	rollbackOpen := edge("24", 300, 300)
	if err := db.ApplyTopologyObservation(ctx, TopologyChanges{
		Open: []model.TopologyEdge{rollbackOpen}, Update: []model.TopologyEdge{continuing},
		Close: []model.TopologyEdge{opened},
	}, []model.TopologySourceObservation{{
		DeviceID: device.ID, Source: "forced", State: model.TopologySourceObserved, ObservedAt: 300,
	}}); err == nil {
		t.Fatal("forced source-state failure was reported as success")
	}
	current, err = db.TopologyEdgesAt(ctx, 0)
	if err != nil || len(current) != 2 {
		t.Fatalf("current after rollback=%+v err=%v", current, err)
	}
	byChild = map[string]model.TopologyEdge{}
	for _, got := range current {
		byChild[got.ChildNode] = got
	}
	if byChild[continuing.ChildNode].LastSeen != 200 || byChild[opened.ChildNode].ValidTo != nil {
		t.Fatalf("failed observation partially changed intervals: %+v", current)
	}
	if _, exists := byChild[rollbackOpen.ChildNode]; exists {
		t.Fatalf("failed observation left opened interval: %+v", current)
	}
	states, err = db.TopologySourceStates(ctx)
	if err != nil || len(states) != 1 || states[0].Source != "brctl" {
		t.Fatalf("failed observation changed source states: %+v err=%v", states, err)
	}
}

func TestApplyTopologyObservationReplacesObsoleteDeviceSourcesAtomically(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := addObservationDevice(t, db)
	initial := []model.TopologySourceObservation{
		{DeviceID: device.ID, Source: "bridge-fdb:br-lan", State: model.TopologySourceObserved, ObservedAt: 100},
		{DeviceID: device.ID, Source: "bridge-stp:br-lan", State: model.TopologySourceObserved, ObservedAt: 100},
		{DeviceID: device.ID, Source: "default-route", State: model.TopologySourceObserved, ObservedAt: 100},
	}
	if err := db.ApplyTopologyObservation(ctx, TopologyChanges{}, initial); err != nil {
		t.Fatal(err)
	}
	current := []model.TopologySourceObservation{
		{DeviceID: device.ID, Source: "default-route", State: model.TopologySourceEmpty, ObservedAt: 200},
	}
	if err := db.ApplyTopologyObservation(ctx, TopologyChanges{
		ReplaceSourcesFor: []int64{device.ID},
	}, current); err != nil {
		t.Fatal(err)
	}
	states, err := db.TopologySourceStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].DeviceID != device.ID ||
		states[0].Source != "default-route" || states[0].State != model.TopologySourceEmpty ||
		states[0].ObservedAt != 200 {
		t.Fatalf("obsolete source states survived authoritative replacement: %+v", states)
	}

	if err := db.ApplyTopologyObservation(ctx, TopologyChanges{
		ReplaceSourcesFor: []int64{device.ID},
	}, nil); err == nil {
		t.Fatal("source replacement without an authoritative observation was accepted")
	}
	states, err = db.TopologySourceStates(ctx)
	if err != nil || len(states) != 1 || states[0].Source != "default-route" {
		t.Fatalf("rejected replacement changed source states: %+v err=%v", states, err)
	}
}

func TestRadioScanUsesStableRadioKeyAndAtomicValidatedObservations(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	device := addObservationDevice(t, db)
	scan := &model.RadioScan{Radio: model.RadioKey{DeviceID: device.ID, Section: "radio0"},
		StartedAt: 100, Status: model.RadioScanRunning}
	if err := db.CreateRadioScan(ctx, scan); err != nil {
		t.Fatal(err)
	}
	signal, width := -62, 80
	observations := []model.RadioScanBSS{{
		BSSID: "AA:BB:CC:DD:EE:10", SSID: "guest", MHz: 5180, Channel: 36,
		Signal: &signal, Width: &width,
	}}
	if err := db.FinishRadioScan(ctx, scan.ID, model.RadioScanCompleted, 110,
		json.RawMessage(`{"source":"iwinfo.scan"}`), observations); err != nil {
		t.Fatal(err)
	}
	got, bss, err := db.RadioScanByID(ctx, scan.ID)
	if err != nil || got.Radio.Section != "radio0" || got.Status != model.RadioScanCompleted ||
		len(bss) != 1 || bss[0].BSSID != "aa:bb:cc:dd:ee:10" {
		t.Fatalf("scan=%+v bss=%+v err=%v", got, bss, err)
	}
	bad := &model.RadioScan{Radio: model.RadioKey{DeviceID: device.ID, Section: "wlan0/phy0"}}
	if err := db.CreateRadioScan(ctx, bad); err == nil {
		t.Fatal("runtime interface-shaped radio key was accepted")
	}
	pending := &model.RadioScan{Radio: model.RadioKey{DeviceID: device.ID, Section: "radio1"}}
	if err := db.CreateRadioScan(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRadioScan(ctx, pending.ID, model.RadioScanCompleted, 120, nil,
		[]model.RadioScanBSS{{BSSID: "not-a-mac", MHz: 2412, Channel: 1}}); err == nil {
		t.Fatal("malformed BSS was accepted")
	}
	still, rows, err := db.RadioScanByID(ctx, pending.ID)
	if err != nil || still.Status != model.RadioScanRunning || len(rows) != 0 {
		t.Fatalf("failed completion was not atomic: scan=%+v rows=%+v err=%v", still, rows, err)
	}
}

func TestSchema16IndexesAndNoRadioCredentialColumns(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	want := map[string][]string{
		"events_ts":              {"ts"},
		"events_source_identity": {"device_id", "source", "source_boot", "source_id"},
		"events_client_time":     {"client_mac", "ts", "id"},
		"events_category_time":   {"category", "ts", "id"},
		"events_severity_time":   {"severity", "ts", "id"},
		"topology_edges_active":  {"child_node", "valid_to", "last_seen"},
		"topology_edges_replay":  {"valid_from", "valid_to"},
		"radio_scans_radio_time": {"device_id", "radio_key", "started_at", "id"},
	}
	for index, columns := range want {
		rows, err := db.SQL().QueryContext(ctx, `PRAGMA index_info(`+index+`)`)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var seq, cid int
			var name string
			if err := rows.Scan(&seq, &cid, &name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		rows.Close()
		if !slices.Equal(got, columns) {
			t.Errorf("index %s columns=%v, want %v", index, got, columns)
		}
	}
	rows, err := db.SQL().QueryContext(ctx, `PRAGMA table_info(radio_scan_bss)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(name)
		if strings.Contains(lower, "password") || strings.Contains(lower, "psk") ||
			strings.Contains(lower, "secret") || strings.Contains(lower, "security_key") {
			t.Fatalf("radio observation schema persists a credential column %q", name)
		}
	}
}

func addObservationDevice(t *testing.T, db *DB) *Device {
	t.Helper()
	device := &Device{MAC: "02:00:00:00:00:16", Host: "192.0.2.16", Name: "p4"}
	if err := db.UpsertDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	return device
}
