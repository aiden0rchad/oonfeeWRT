package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func TestPolicyRoundTripPersistsCanonicalValidatedRule(t *testing.T) {
	db := open(t)
	seedZoneNetworks(t, db)
	ctx := context.Background()
	p := &model.Policy{Name: "deny", Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: true,
		Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "guest", DestinationZone: "wan",
			Protocols: []string{"UDP", "tcp", "udp"}, SourceCIDR: "10.0.20.99/24",
			DestinationPort: "00443", SourceMACs: []string{"AA-BB-CC-DD-EE-FF"}}}
	if err := db.SavePolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 || p.Order != 100 || strings.Join(p.Firewall.Protocols, ",") != "tcp,udp" ||
		p.Firewall.SourceCIDR != "10.0.20.0/24" || p.Firewall.DestinationPort != "443" ||
		strings.Join(p.Firewall.SourceMACs, ",") != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("save did not return canonical policy: %+v", p)
	}
	var raw string
	if err := db.SQL().QueryRowContext(ctx, `SELECT rule_json FROM fw_rules WHERE id=?`, p.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"protocols":["tcp","udp"]`, `"source_cidr":"10.0.20.0/24"`, `"destination_port":"443"`, `"source_macs":["aa:bb:cc:dd:ee:ff"]`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("stored JSON %s lacks %s", raw, want)
		}
	}
	site, err := db.Site(ctx)
	if err != nil || len(site.Policies) != 1 || site.Policies[0].ID != p.ID {
		t.Fatalf("round trip site=%+v err=%v", site.Policies, err)
	}
	p.Name = "deny renamed"
	if err := db.SavePolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := db.DeletePolicy(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeletePolicy(ctx, p.ID); err != ErrNotFound {
		t.Fatalf("second delete=%v, want ErrNotFound", err)
	}
}

func TestUnreadableStoredPolicyFailsSiteLoadClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"bad JSON":      `{`,
		"unknown field": `{"name":"x","kind":"static_route","origin":"manual","static_route":{"network_id":0,"target":"203.0.113.0/24","gateway":"192.0.2.1"},"priority":1}`,
		"trailing":      `{"name":"x","kind":"static_route","origin":"manual","static_route":{"network_id":0,"target":"203.0.113.0/24","gateway":"192.0.2.1"}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			db := open(t)
			if _, err := db.SQL().ExecContext(context.Background(),
				`INSERT INTO fw_rules(sort,rule_json,enabled) VALUES(1,?,1)`, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Site(context.Background()); err == nil {
				t.Fatalf("stored policy %s loaded by guessing", raw)
			}
		})
	}
}

func TestClientPolicyRoundTripAndPruneRetention(t *testing.T) {
	db := open(t)
	seedZoneNetworks(t, db)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour).Unix()
	if err := db.UpsertClients(ctx, []SeenClient{
		{MAC: "00:11:22:33:44:55", Scope: ScopeLocal},
		{MAC: "00:11:22:33:44:66", Scope: ScopeLocal},
		{MAC: "00:11:22:33:44:77", Scope: ScopeLocal},
		{MAC: "00:11:22:33:44:88", Scope: ScopeLocal},
	}, old); err != nil {
		t.Fatal(err)
	}
	blocked, fixed, group := true, "10.0.20.50", "cameras"
	client, err := db.SaveClientPolicy(ctx, "00:11:22:33:44:55", &blocked, &fixed, &group)
	if err != nil || !client.Blocked || client.FixedIP != fixed || client.Group != group {
		t.Fatalf("saved client=%+v err=%v", client, err)
	}
	if _, err := db.SaveClientPolicy(ctx, "00:11:22:33:44:66", nil, &fixed, nil); err == nil {
		t.Fatal("duplicate fixed IP accepted")
	}
	onlyFixed := "10.0.20.51"
	onlyGroup := "printers"
	if _, err := db.SaveClientPolicy(ctx, "00:11:22:33:44:66", nil, &onlyFixed, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveClientPolicy(ctx, "00:11:22:33:44:77", nil, nil, &onlyGroup); err != nil {
		t.Fatal(err)
	}
	pruned, err := db.PruneClients(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || pruned != 1 {
		t.Fatalf("pruned=%d err=%v, want only unprotected row", pruned, err)
	}
	clients, err := db.Clients(ctx, 0, 20)
	if err != nil || len(clients) != 3 {
		t.Fatalf("retained clients=%+v err=%v", clients, err)
	}
	empty := ""
	unblocked := false
	if _, err := db.SaveClientPolicy(ctx, client.MAC, &unblocked, &empty, &empty); err != nil {
		t.Fatal(err)
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, policyClient := range site.PolicyClients {
		if strings.EqualFold(policyClient.MAC, client.MAC) {
			t.Fatalf("cleared client remained in desired site: %+v", policyClient)
		}
	}
}

func TestClientPolicyAndPruneNeverReportADeletedSave(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	const mac = "00:11:22:33:44:99"
	for i := 0; i < 32; i++ {
		if _, err := db.SQL().ExecContext(ctx, `DELETE FROM clients WHERE mac=?`, mac); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertClients(ctx, []SeenClient{{MAC: mac, Scope: ScopeLocal}}, time.Now().Add(-48*time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
		group := "retained"
		start := make(chan struct{})
		var saveErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, saveErr = db.SaveClientPolicy(ctx, mac, nil, nil, &group)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = db.PruneClients(ctx, time.Now().Add(-24*time.Hour))
		}()
		close(start)
		wg.Wait()
		var count int
		if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM clients WHERE mac=? AND grp=?`, mac, group).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if saveErr == nil && count != 1 {
			t.Fatal("SaveClientPolicy returned success after its client row vanished")
		}
		if saveErr != nil && !errors.Is(saveErr, ErrNotFound) {
			t.Fatalf("concurrent client policy save = %v", saveErr)
		}
	}
}

func TestSchema15PolicyBoundaryPreservesExistingIntent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.db")
	db, err := Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"name":"route","kind":"static_route","origin":"manual","static_route":{"network_id":0,"target":"203.0.113.0/24","gateway":"192.0.2.1"}}`
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO fw_rules(sort,rule_json,enabled) VALUES(100,?,1)`, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM schema_version WHERE version>?`, 14); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, driver, path, testProtector(t, path))
	if err != nil {
		t.Fatalf("migrate v14 policy intent: %v", err)
	}
	defer db.Close()
	var version int
	if err := db.SQL().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	site, err := db.Site(ctx)
	if err != nil || version != schemaVersion || len(site.Policies) != 1 || site.Policies[0].Name != "route" {
		t.Fatalf("migrated version=%d policies=%+v err=%v", version, site.Policies, err)
	}
}
