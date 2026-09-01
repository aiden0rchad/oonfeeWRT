package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSchema20AddsClosedTopologyIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v19.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `
DROP INDEX topology_edges_closed_latest;
UPDATE schema_version SET version=19
 WHERE version=(SELECT MAX(version) FROM schema_version)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatalf("migrate v19: %v", err)
	}
	defer db.Close()
	if err := verifySchemaV20(ctx, db.SQL()); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version=%d, want %d", version, schemaVersion)
	}
}

func TestSchema20MigratesTopologyLuCISourceNamesWithoutLosingNewerState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v19-source-names.db")
	protector := testProtector(t, path)
	db, err := Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatal(err)
	}
	device := addObservationDevice(t, db)
	if _, err := db.SQL().ExecContext(ctx, `
INSERT INTO topology_source_states(device_id,source,state,reason,observed_at) VALUES
 (?, 'luci.getNetworkDevices', 'error', 'legacy older', 100),
 (?, 'luci-rpc.getNetworkDevices', 'observed', 'current newer', 200),
 (?, 'luci.getWirelessDevices', 'empty', 'legacy newer', 300),
 (?, 'luci-rpc.getWirelessDevices', 'error', 'current older', 200);
DROP INDEX topology_edges_closed_latest;
UPDATE schema_version SET version=19
 WHERE version=(SELECT MAX(version) FROM schema_version)`,
		device.ID, device.ID, device.ID, device.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, driver, path, protector)
	if err != nil {
		t.Fatalf("migrate v19 source names: %v", err)
	}
	defer db.Close()
	rows, err := db.SQL().QueryContext(ctx, `
SELECT source,state,reason,observed_at
  FROM topology_source_states
 WHERE device_id=?
 ORDER BY source`, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type state struct {
		source, status, reason string
		at                     int64
	}
	var got []state
	for rows.Next() {
		var row state
		if err := rows.Scan(&row.source, &row.status, &row.reason, &row.at); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []state{
		{source: "luci-rpc.getNetworkDevices", status: "observed", reason: "current newer", at: 200},
		{source: "luci-rpc.getWirelessDevices", status: "empty", reason: "legacy newer", at: 300},
	}
	if len(got) != len(want) {
		t.Fatalf("migrated source rows=%+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("migrated source row %d=%+v, want %+v", i, got[i], want[i])
		}
	}
}
