package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func seedZoneNetworks(t *testing.T, db *DB) (guest, iot *model.Network) {
	t.Helper()
	ctx := context.Background()
	guest = &model.Network{Name: "guest", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "guest", Enabled: true}
	iot = &model.Network{Name: "iot", VLAN: 30, CIDR: "10.0.30.1/24", Zone: "iot", Enabled: true}
	for _, n := range []*model.Network{guest, iot} {
		if err := db.SaveNetwork(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	return guest, iot
}

func TestZonePolicyRoundTripIsExplicitAndDeterministic(t *testing.T) {
	db := open(t)
	seedZoneNetworks(t, db)
	ctx := context.Background()
	p := &model.ZonePolicy{Name: "guest", ForwardTo: []string{"wan", "iot", "wan"}}
	if err := db.SaveZonePolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if !p.Explicit || strings.Join(p.ForwardTo, ",") != "iot,wan" {
		t.Fatalf("saved policy = %+v, want explicit canonical destinations", p)
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.Zones) != 1 || !site.Zones[0].Explicit || strings.Join(site.Zones[0].ForwardTo, ",") != "iot,wan" {
		t.Fatalf("stored policies = %+v", site.Zones)
	}
	var raw string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT policy_json FROM zones WHERE name='guest'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `{"forward_to":["iot","wan"]}` {
		t.Errorf("policy JSON = %s, want stable canonical encoding", raw)
	}

	if err := db.DeleteZonePolicy(ctx, "guest"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteZonePolicy(ctx, "guest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second reset = %v, want ErrNotFound", err)
	}
	site, err = db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, effective := range site.EffectiveZonePolicies() {
		if effective.Name == "guest" && (effective.Explicit || strings.Join(effective.ForwardTo, ",") != "wan") {
			t.Fatalf("reset policy = %+v, want legacy wan default", effective)
		}
	}
}

func TestUnreadableZonePolicyFailsSiteLoadClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid json":  `{not-json`,
		"missing field": `{}`,
		"null list":     `{"forward_to":null}`,
		"null element":  `{"forward_to":[null]}`,
		"empty element": `{"forward_to":[""]}`,
		"wrong type":    `{"forward_to":"wan"}`,
		"unknown field": `{"forward_to":[],"forward_from":[]}`,
		"trailing json": `{"forward_to":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			db := open(t)
			seedZoneNetworks(t, db)
			if _, err := db.SQL().ExecContext(context.Background(),
				`INSERT INTO zones (name, policy_json) VALUES ('guest', ?)`, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Site(context.Background()); err == nil {
				t.Fatalf("stored policy %s loaded by guessing its meaning", raw)
			}
		})
	}
}

func TestSaveZonePolicyRejectsUnknownSelfAndReservedWan(t *testing.T) {
	db := open(t)
	seedZoneNetworks(t, db)
	for name, p := range map[string]model.ZonePolicy{
		"unknown source": {Name: "missing", ForwardTo: []string{"wan"}},
		"unknown dest":   {Name: "guest", ForwardTo: []string{"missing"}},
		"self":           {Name: "guest", ForwardTo: []string{"guest"}},
		"wan source":     {Name: "wan", ForwardTo: []string{"guest"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.SaveZonePolicy(context.Background(), &p); err == nil {
				t.Fatalf("invalid policy %+v was saved", p)
			}
		})
	}
}

func TestSaveNetworkRejectsUnsafeRenderedZoneIdentifiers(t *testing.T) {
	for name, zone := range map[string]string{
		"lan alias":      "lan!",
		"wan alias":      "wan!",
		"leading digit":  "20_guest",
		"empty rendered": "!!!",
	} {
		t.Run(name, func(t *testing.T) {
			db := open(t)
			n := &model.Network{Name: "bad", VLAN: 20, CIDR: "10.0.20.1/24", Zone: zone, Enabled: true}
			if err := db.SaveNetwork(context.Background(), n); err == nil {
				t.Fatalf("zone %q was persisted", zone)
			}
		})
	}

	db := open(t)
	ctx := context.Background()
	first := &model.Network{Name: "one", VLAN: 20, CIDR: "10.0.20.1/24", Zone: "abcdefghijk-one", Enabled: true}
	second := &model.Network{Name: "two", VLAN: 30, CIDR: "10.0.30.1/24", Zone: "abcdefghijk-two", Enabled: true}
	if err := db.SaveNetwork(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveNetwork(ctx, second); err == nil || !strings.Contains(err.Error(), "both render") {
		t.Fatalf("fw4-colliding zone save = %v", err)
	}
}

func TestLegacyUnsafeZoneIsSurfacedWithoutSilentRename(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO networks (name, vlan, cidr, zone, dhcp_json, enabled)
		 VALUES ('legacy', 20, '10.0.20.1/24', 'wan!', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if site.Networks[0].Zone != "wan!" {
		t.Fatalf("migration silently renamed legacy zone to %q", site.Networks[0].Zone)
	}
	if errs := site.ValidateZoneNames(); len(errs) == 0 || !strings.Contains(errs[0].Error(), "renders as wan") {
		t.Fatalf("legacy unsafe zone was not surfaced: %v", errs)
	}
}

func TestNetworkZoneRenameCannotOrphanRestrictivePolicy(t *testing.T) {
	db := open(t)
	guest, _ := seedZoneNetworks(t, db)
	ctx := context.Background()
	if err := db.SaveZonePolicy(ctx, &model.ZonePolicy{Name: "guest", ForwardTo: []string{}}); err != nil {
		t.Fatal(err)
	}
	guest.Zone = "visitors"
	err := db.SaveNetwork(ctx, guest)
	if err == nil || !strings.Contains(err.Error(), "update or reset") || !strings.Contains(err.Error(), "guest") {
		t.Fatalf("rename error = %v, want named policy recovery", err)
	}
	site, _ := db.Site(ctx)
	if site.Networks[0].Zone != "guest" || len(site.Zones) != 1 {
		t.Fatalf("refused rename partially landed: %+v %+v", site.Networks, site.Zones)
	}
}

func TestNetworkZoneRenameCannotOrphanDestinationReference(t *testing.T) {
	db := open(t)
	_, iot := seedZoneNetworks(t, db)
	ctx := context.Background()
	if err := db.SaveZonePolicy(ctx, &model.ZonePolicy{Name: "guest", ForwardTo: []string{"iot"}}); err != nil {
		t.Fatal(err)
	}
	iot.Zone = "things"
	err := db.SaveNetwork(ctx, iot)
	if err == nil || !strings.Contains(err.Error(), `forwards to "iot"`) {
		t.Fatalf("destination rename error = %v", err)
	}
}

func TestZoneRenameAllowedWhenAnotherNetworkStillUsesSource(t *testing.T) {
	db := open(t)
	guest, _ := seedZoneNetworks(t, db)
	ctx := context.Background()
	other := &model.Network{Name: "guest2", VLAN: 40, CIDR: "10.0.40.1/24", Zone: "guest", Enabled: true}
	if err := db.SaveNetwork(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveZonePolicy(ctx, &model.ZonePolicy{Name: "guest", ForwardTo: []string{}}); err != nil {
		t.Fatal(err)
	}
	guest.Zone = "visitors"
	if err := db.SaveNetwork(ctx, guest); err != nil {
		t.Fatalf("rename orphaned nothing but was refused: %v", err)
	}
}

func TestDirectionalZonePolicyCreatesSemanticV12Boundary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oonfee.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM secret_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE schema_version SET version=11`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got int
	if err := db.SQL().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != schemaVersion {
		t.Fatalf("schema version = %d, want current version %d so a v11 renderer cannot ignore policy",
			got, schemaVersion)
	}
}

func TestConcurrentPolicySaveAndZoneRenameCannotBothCommit(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for i := 0; i < 24; i++ {
		name := fmt.Sprintf("guest%d", i)
		n := &model.Network{
			Name: name, Zone: name, VLAN: 100 + i,
			CIDR: fmt.Sprintf("10.%d.0.1/24", i+1), Enabled: true,
		}
		if err := db.SaveNetwork(ctx, n); err != nil {
			t.Fatal(err)
		}
		rename := *n
		rename.Zone = fmt.Sprintf("visitors%d", i)
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			errs <- db.SaveZonePolicy(ctx, &model.ZonePolicy{Name: name, ForwardTo: []string{}})
		}()
		go func() {
			<-start
			errs <- db.SaveNetwork(ctx, &rename)
		}()
		close(start)
		first, second := <-errs, <-errs
		if (first == nil) == (second == nil) {
			t.Fatalf("iteration %d results = %v / %v, want exactly one commit", i, first, second)
		}
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if errs := site.ValidateZonePolicies(); len(errs) != 0 {
		t.Fatalf("serialized mutations left orphan policy: %v", errs)
	}
}
