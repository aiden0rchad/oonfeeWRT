package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

type interleavedPolicyReader struct {
	siteReader
	batches      int
	beforeSecond func() error
}

func (r *interleavedPolicyReader) QueryContext(ctx context.Context, query string,
	args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "FROM client_observations observed") {
		r.batches++
		if r.batches == 2 && r.beforeSecond != nil {
			if err := r.beforeSecond(); err != nil {
				return nil, err
			}
		}
	}
	return r.siteReader.QueryContext(ctx, query, args...)
}

func observePolicyClients(t *testing.T, db *DB, seen []SeenClient, now int64) *Device {
	t.Helper()
	at := int64(1)
	gateway := &Device{
		MAC: "02:00:00:00:23:01", Host: "192.0.2.1", Name: "managed-gateway",
		Role: "gateway", Functions: []string{"gateway"}, ManagementMode: "managed", AdoptedAt: &at,
	}
	if err := db.UpsertDevice(context.Background(), gateway); err != nil {
		t.Fatal(err)
	}
	for i := range seen {
		seen[i].DeviceID = gateway.ID
	}
	if err := db.UpsertClients(context.Background(), seen, now); err != nil {
		t.Fatal(err)
	}
	return gateway
}

func TestPolicyRoundTripPersistsCanonicalValidatedRule(t *testing.T) {
	db := open(t)
	seedZoneNetworks(t, db)
	ctx := context.Background()
	observePolicyClients(t, db, []SeenClient{{MAC: "aa:bb:cc:dd:ee:ff", Scope: ScopeLocal}}, time.Now().Unix())
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
	observePolicyClients(t, db, []SeenClient{
		{MAC: "00:11:22:33:44:55", Scope: ScopeLocal},
		{MAC: "00:11:22:33:44:66", Scope: ScopeLocal},
		{MAC: "00:11:22:33:44:77", Scope: ScopeLocal},
		{MAC: "00:11:22:33:44:88", Scope: ScopeLocal},
	}, old)
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

func TestClientPolicyCanClearInvalidScopeOneClientAtATime(t *testing.T) {
	db := open(t)
	seedZoneNetworks(t, db)
	ctx := context.Background()
	seen := []SeenClient{
		{MAC: "00:11:22:33:44:51", Scope: ScopeLocal},
		{MAC: "00:11:22:33:44:52", Scope: ScopeLocal},
	}
	now := time.Now().Unix()
	gateway := observePolicyClients(t, db, seen, now)
	blocked := true
	for _, client := range seen {
		if _, err := db.SaveClientPolicy(ctx, client.MAC, &blocked, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := range seen {
		seen[i].DeviceID = gateway.ID
		seen[i].Scope = ScopeUpstream
	}
	if err := db.UpsertClients(ctx, seen, now+1); err != nil {
		t.Fatal(err)
	}
	unblocked := false
	for _, client := range seen {
		if _, err := db.SaveClientPolicy(ctx, client.MAC, &unblocked, nil, nil); err != nil {
			t.Fatalf("clear %s while another client remains invalid: %v", client.MAC, err)
		}
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.PolicyClients) != 0 {
		t.Fatalf("cleared client intent remains: %+v", site.PolicyClients)
	}
}

func TestPoliciesCanDisableUnprovedMACIntentOneAtATime(t *testing.T) {
	db := open(t)
	seedZoneNetworks(t, db)
	ctx := context.Background()
	macs := []string{"00:11:22:33:44:61", "00:11:22:33:44:62"}
	now := time.Now().Unix()
	observePolicyClients(t, db, []SeenClient{
		{MAC: macs[0], Scope: ScopeLocal}, {MAC: macs[1], Scope: ScopeLocal},
	}, now)
	policies := make([]*model.Policy, 0, len(macs))
	for i, mac := range macs {
		policy := &model.Policy{Name: fmt.Sprintf("deny-%d", i), Kind: model.PolicyFirewallRule,
			Origin: model.PolicyOriginManual, Enabled: true,
			Firewall: &model.FirewallRule{Action: model.FirewallDrop, SourceZone: "guest",
				DestinationZone: "wan", Protocols: []string{"all"}, SourceMACs: []string{mac}}}
		if err := db.SavePolicy(ctx, policy); err != nil {
			t.Fatal(err)
		}
		policies = append(policies, policy)
	}
	if _, err := db.PruneClients(ctx, time.Unix(now+1, 0)); err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies {
		policy.Enabled = false
		if err := db.SavePolicy(ctx, policy); err != nil {
			t.Fatalf("disable %q while another MAC policy remains unproved: %v", policy.Name, err)
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

func TestPolicySetRoundTripUpdatesReferencesAndRefusesOrphans(t *testing.T) {
	db := open(t)
	seedZoneNetworks(t, db)
	ctx := context.Background()
	now := time.Now().Unix()
	gateway := observePolicyClients(t, db, []SeenClient{
		{MAC: "00:11:22:33:44:55", Scope: ScopeLocal},
		{MAC: "00:11:22:33:44:66", Scope: ScopeLocal},
	}, now)
	set := &model.PolicySet{Name: "Cameras", Members: []string{
		"00-11-22-33-44-66", "00:11:22:33:44:55", "00:11:22:33:44:55",
	}}
	if err := db.SavePolicySet(ctx, set); err != nil {
		t.Fatal(err)
	}
	if set.ID == 0 || strings.Join(set.Members, ",") != "00:11:22:33:44:55,00:11:22:33:44:66" {
		t.Fatalf("saved set=%+v", set)
	}
	if pruned, err := db.PruneClients(ctx, time.Unix(now+1, 0)); err != nil || pruned != 0 {
		t.Fatalf("policy set members pruned=%d err=%v", pruned, err)
	}
	var sourceRows int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM client_observations WHERE mac='00:11:22:33:44:55'`).Scan(&sourceRows); err != nil || sourceRows != 0 {
		t.Fatalf("stale policy client provenance rows=%d err=%v", sourceRows, err)
	}
	policy := &model.Policy{Name: "secure cameras", Kind: model.PolicyFirewallRule,
		Origin: model.PolicyOriginObjectManager, Enabled: true,
		Firewall: &model.FirewallRule{Action: model.FirewallReject, SourceZone: "guest",
			DestinationZone: "wan", Protocols: []string{"all"}, SourceSetID: set.ID}}
	if err := db.SavePolicy(ctx, policy); err == nil || !strings.Contains(err.Error(), "has not been observed") {
		t.Fatalf("stale policy provenance save=%v", err)
	}
	if err := db.UpsertClients(ctx, []SeenClient{
		{DeviceID: gateway.ID, MAC: "00:11:22:33:44:55", Scope: ScopeLocal},
		{DeviceID: gateway.ID, MAC: "00:11:22:33:44:66", Scope: ScopeLocal},
	}, now+2); err != nil {
		t.Fatal(err)
	}
	if err := db.SavePolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}

	set.Members = []string{"00:11:22:33:44:66"}
	if err := db.SavePolicySet(ctx, set); err != nil {
		t.Fatal(err)
	}
	site, err := db.Site(ctx)
	if err != nil || len(site.PolicySets) != 1 || strings.Join(site.PolicySets[0].Members, ",") != "00:11:22:33:44:66" {
		t.Fatalf("round trip sets=%+v err=%v", site.PolicySets, err)
	}
	if err := db.DeletePolicySet(ctx, set.ID); err == nil || !strings.Contains(err.Error(), "still references") {
		t.Fatalf("referenced set delete=%v", err)
	}
	if err := db.DeletePolicy(ctx, policy.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeletePolicySet(ctx, set.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeletePolicySet(ctx, set.ID); err != ErrNotFound {
		t.Fatalf("second set delete=%v want ErrNotFound", err)
	}
}

func TestPolicySetRejectsUnknownInventoryMember(t *testing.T) {
	db := open(t)
	set := &model.PolicySet{Name: "Unknown", Members: []string{"00:11:22:33:44:55"}}
	if err := db.SavePolicySet(context.Background(), set); err == nil || !strings.Contains(err.Error(), "observed client inventory") {
		t.Fatalf("unknown inventory member save=%v", err)
	}
}

func TestPolicySetRequiresManagedGatewayObservation(t *testing.T) {
	for _, test := range []struct {
		name          string
		scope         string
		monitorSource bool
		unadoptSource bool
		want          string
	}{
		{name: "upstream at managed gateway", scope: ScopeUpstream, want: "observed as upstream"},
		{name: "unknown at managed gateway", scope: ScopeUnknown, want: "observed as unknown"},
		{name: "monitor AP only", scope: ScopeLocal, monitorSource: true, want: "has not been observed"},
		{name: "unadopted monitor residue", scope: ScopeLocal, monitorSource: true, unadoptSource: true, want: "has not been observed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := open(t)
			ctx := context.Background()
			now := time.Now().Unix()
			gateway := observePolicyClients(t, db, nil, now)
			sourceID := gateway.ID
			if test.monitorSource {
				at := int64(1)
				monitor := &Device{
					MAC: "02:00:00:00:21:55", Host: "198.51.100.1", Name: "observed-ap",
					Role: "ap", Functions: []string{"ap"}, ManagementMode: "monitor_only", AdoptedAt: &at,
				}
				if err := db.UpsertDevice(ctx, monitor); err != nil {
					t.Fatal(err)
				}
				sourceID = monitor.ID
			}
			if err := db.UpsertClients(ctx, []SeenClient{{
				DeviceID: sourceID, MAC: "00:11:22:33:44:55", Scope: test.scope,
			}}, now); err != nil {
				t.Fatal(err)
			}
			if test.unadoptSource {
				if _, err := db.SQL().ExecContext(ctx, `UPDATE devices SET adopted_at=NULL WHERE id=?`, sourceID); err != nil {
					t.Fatal(err)
				}
			}
			set := &model.PolicySet{Name: "unsafe", Members: []string{"00:11:22:33:44:55"}}
			if err := db.SavePolicySet(ctx, set); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("save error=%v, want %q", err, test.want)
			}
			if sets, err := db.policySetsOn(ctx, db.SQL()); err != nil || len(sets) != 0 {
				t.Fatalf("refused set persisted: sets=%v err=%v", sets, err)
			}
		})
	}
}

func TestPolicySetAcceptsManagedGatewayProofAlongsideMonitorObservation(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	const mac = "00:11:22:33:44:55"
	now := time.Now().Unix()
	observePolicyClients(t, db, []SeenClient{{MAC: mac, Scope: ScopeLocal}}, now)
	at := int64(1)
	monitor := &Device{MAC: "02:00:00:00:23:02", Host: "198.51.100.2", Name: "monitor",
		Role: "switch", Functions: []string{"switch"}, ManagementMode: "monitor_only", AdoptedAt: &at}
	if err := db.UpsertDevice(ctx, monitor); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClients(ctx, []SeenClient{{DeviceID: monitor.ID, MAC: mac, Scope: ScopeLocal}}, now+1); err != nil {
		t.Fatal(err)
	}
	set := &model.PolicySet{Name: "proved", Members: []string{mac}}
	if err := db.SavePolicySet(ctx, set); err != nil {
		t.Fatalf("managed-Gateway proof was contaminated by monitor inventory: %v", err)
	}
}

func TestPolicyMACScopeProblemsRequiresFreshPlausibleGatewayObservation(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	const mac = "00:11:22:33:44:56"
	now := time.Now()
	gateway := observePolicyClients(t, db, []SeenClient{{MAC: mac, Scope: ScopeLocal}}, now.Unix())

	check := func(wantProblem bool) {
		t.Helper()
		problems, err := db.PolicyMACScopeProblems(ctx, model.Site{}, mac)
		if err != nil {
			t.Fatal(err)
		}
		if wantProblem != (len(problems) > 0) {
			t.Fatalf("scope problems=%v, wantProblem=%t", problems, wantProblem)
		}
		if wantProblem && !strings.Contains(problems[0].Error(), "has not been observed") {
			t.Fatalf("unexpected scope problem: %v", problems[0])
		}
	}

	check(false)
	for _, observedAt := range []time.Time{
		now.Add(-DefaultClientTTL - time.Hour),
		now.Add(MaxClientObservationFutureSkew + time.Hour),
	} {
		if _, err := db.SQL().ExecContext(ctx, `
UPDATE client_observations SET last_seen=? WHERE device_id=? AND mac=?`,
			observedAt.Unix(), gateway.ID, mac); err != nil {
			t.Fatal(err)
		}
		check(true)
	}
}

func TestPolicySetExpansionBudgetRejectsSavesAtomically(t *testing.T) {
	db := open(t)
	seedZoneNetworks(t, db)
	ctx := context.Background()
	clients := make([]SeenClient, 1009)
	members := make([]string, len(clients))
	for i := range clients {
		mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x",
			byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
		clients[i] = SeenClient{MAC: mac, Scope: ScopeLocal}
		members[i] = mac
	}
	observePolicyClients(t, db, clients, time.Now().Unix())
	set := &model.PolicySet{Name: "bounded", Members: members[:1008]}
	if err := db.SavePolicySet(ctx, set); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 65; i++ {
		policy := &model.Policy{Name: fmt.Sprintf("reuse-%d", i), Kind: model.PolicyFirewallRule,
			Origin: model.PolicyOriginManual, Enabled: false,
			Firewall: &model.FirewallRule{Action: model.FirewallReject, SourceZone: "guest",
				DestinationZone: "wan", Protocols: []string{"all"}, SourceSetID: set.ID}}
		if err := db.SavePolicy(ctx, policy); err != nil {
			t.Fatalf("save policy %d: %v", i, err)
		}
	}
	over := &model.Policy{Name: "over-budget", Kind: model.PolicyFirewallRule,
		Origin: model.PolicyOriginManual, Enabled: false,
		Firewall: &model.FirewallRule{Action: model.FirewallReject, SourceZone: "guest",
			DestinationZone: "wan", Protocols: []string{"all"}, SourceSetID: set.ID}}
	if err := db.SavePolicy(ctx, over); err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
		t.Fatalf("over-budget policy save=%v", err)
	}
	set.Members = members
	if err := db.SavePolicySet(ctx, set); err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
		t.Fatalf("over-budget set update=%v", err)
	}
	site, err := db.Site(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(site.Policies) != 65 || len(site.PolicySets) != 1 || len(site.PolicySets[0].Members) != 1008 {
		t.Fatalf("refused changes persisted: policies=%d sets=%+v", len(site.Policies), site.PolicySets)
	}
}

func TestPolicySetStoredMemberBudgetRejectsSaveAtomically(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	tx, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	for setID := 1; setID <= model.MaxPolicySetMembersTotal/model.MaxPolicySetMembers; setID++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO policy_sets(id,name) VALUES (?,?)`,
			setID, fmt.Sprintf("set-%d", setID)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `
WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<31)
INSERT INTO policy_set_members(set_id,mac)
SELECT ?, printf('02:%02x:00:00:%02x:%02x', ?, ((a.x*32+b.x)>>8)&255, (a.x*32+b.x)&255)
  FROM n a CROSS JOIN n b`, setID, setID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO clients(mac,scope,first_seen,last_seen)
SELECT DISTINCT mac,'local',1,1 FROM policy_set_members`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	const candidateMAC = "02:ff:ff:ff:ff:ff"
	if err := db.UpsertClients(ctx, []SeenClient{{MAC: candidateMAC, Scope: ScopeLocal}}, 1); err != nil {
		t.Fatal(err)
	}
	set := &model.PolicySet{Name: "one-too-many", Members: []string{candidateMAC}}
	if err := db.SavePolicySet(ctx, set); err == nil || !strings.Contains(err.Error(), "site maximum") {
		t.Fatalf("over-limit stored member save=%v", err)
	}
	var sets int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM policy_sets`).Scan(&sets); err != nil ||
		sets != model.MaxPolicySetMembersTotal/model.MaxPolicySetMembers {
		t.Fatalf("refused set persisted: sets=%d err=%v", sets, err)
	}
	pruneCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if pruned, err := db.PruneClients(pruneCtx, time.Unix(2, 0)); err != nil || pruned != 1 {
		t.Fatalf("budget-scale indexed prune: pruned=%d err=%v", pruned, err)
	}
	var clients int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM clients`).Scan(&clients); err != nil ||
		clients != model.MaxPolicySetMembersTotal {
		t.Fatalf("prune removed policy-set identities: clients=%d err=%v", clients, err)
	}
}

func TestPolicyMACScopeProblemsHandlesMaximumExpansionWithinDeadline(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	now := time.Now().Unix()
	gateway := observePolicyClients(t, db, nil, now)
	tx, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.ExecContext(ctx, `
WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<255)
INSERT INTO clients(mac,scope,first_seen,last_seen)
SELECT printf('02:aa:bb:cc:%02x:%02x',a.x,b.x),'local',?,?
  FROM n a CROSS JOIN n b`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO client_observations(device_id,mac,scope,last_seen)
SELECT ?,lower(mac),'local',? FROM clients`, gateway.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	macs := make([]string, model.MaxExpandedPolicySourceMACs)
	for i := range macs {
		macs[i] = fmt.Sprintf("02:aa:bb:cc:%02x:%02x", byte(i>>8), byte(i))
	}
	// Pure-Go SQLite under the race detector can take more than 30 seconds on
	// a shared hosted runner at the full 65,536-MAC product limit. Keep the
	// maximum-size proof bounded without turning runner contention into a
	// product failure.
	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	problems, err := db.PolicyMACScopeProblems(checkCtx, model.Site{}, macs...)
	if err != nil || len(problems) != 0 {
		t.Fatalf("maximum indexed scope check: problems=%v err=%v", problems, err)
	}
}

func TestPolicyMACScopeProblemsUsesOneSnapshotAcrossBatches(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	clients := make([]SeenClient, 501)
	macs := make([]string, len(clients))
	for i := range clients {
		mac := fmt.Sprintf("02:bb:cc:dd:%02x:%02x", byte(i>>8), byte(i))
		clients[i], macs[i] = SeenClient{MAC: mac, Scope: ScopeLocal}, mac
	}
	now := time.Now().Unix()
	gateway := observePolicyClients(t, db, clients, now)
	var databasePath string
	var sequence int
	var databaseName string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA database_list`).
		Scan(&sequence, &databaseName, &databasePath); err != nil {
		t.Fatal(err)
	}
	dsn, err := dsnWithPragmas(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	writer.SetMaxOpenConns(1)
	defer writer.Close()
	if err := writer.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := db.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	reader := &interleavedPolicyReader{siteReader: tx, beforeSecond: func() error {
		_, err := writer.ExecContext(ctx, `
UPDATE client_observations SET scope='upstream',last_seen=?
 WHERE device_id=? AND mac=?`, now+1, gateway.ID, macs[len(macs)-1])
		return err
	}}
	problems, err := db.policyMACScopeProblemsOn(ctx, reader, model.Site{}, macs...)
	if err != nil || len(problems) != 0 || reader.batches != 2 {
		t.Fatalf("snapshot check: batches=%d problems=%v err=%v", reader.batches, problems, err)
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal(err)
	}
	problems, err = db.PolicyMACScopeProblems(ctx, model.Site{}, macs...)
	if err != nil || len(problems) == 0 || !strings.Contains(problems[0].Error(), "upstream") {
		t.Fatalf("new snapshot missed committed reclassification: problems=%v err=%v", problems, err)
	}
}
