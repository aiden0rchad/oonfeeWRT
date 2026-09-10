package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func recoveryPolicyFixture(t *testing.T) *DB {
	t.Helper()
	db := open(t)
	ctx := context.Background()
	if _, err := db.Site(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateFirstAdmin(ctx, "recovery-owner", "test-password-hash"); err != nil {
		t.Fatal(err)
	}
	return db
}

func inspectRecoveryFixture(db *DB) error {
	_, err := db.InspectRecovery(context.Background(),
		func(string, []byte) error { return nil },
		func(string) error { return nil })
	return err
}

func TestInspectRecoveryRejectsPolicySetMemberMissingFromClientInventory(t *testing.T) {
	db := recoveryPolicyFixture(t)
	if _, err := db.SQL().Exec(`INSERT INTO policy_sets (id,name) VALUES (1,'Recovered clients')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO policy_set_members (set_id,mac) VALUES (1,'02:00:00:00:23:01')`); err != nil {
		t.Fatal(err)
	}

	err := inspectRecoveryFixture(db)
	if err == nil || !strings.Contains(err.Error(), "policy set membership validation failed") {
		t.Fatalf("recovery error=%v, want missing policy-set client rejection", err)
	}
	if strings.Contains(err.Error(), "02:00:00:00:23:01") {
		t.Fatalf("recovery error exposed a client identifier: %v", err)
	}
}

func TestInspectRecoveryMatchesLocalPolicySetMembersCaseInsensitively(t *testing.T) {
	db := recoveryPolicyFixture(t)
	if _, err := db.SQL().Exec(`INSERT INTO clients (mac,scope) VALUES ('02:00:00:00:23:0A','local')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO policy_sets (id,name) VALUES (1,'Recovered clients')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO policy_set_members (set_id,mac) VALUES (1,'02:00:00:00:23:0a')`); err != nil {
		t.Fatal(err)
	}

	if err := inspectRecoveryFixture(db); err != nil {
		t.Fatalf("case-insensitive inventory match was rejected: %v", err)
	}
}

func TestInspectRecoveryRejectsNonLocalPolicySetMember(t *testing.T) {
	db := recoveryPolicyFixture(t)
	if _, err := db.SQL().Exec(`INSERT INTO clients (mac,scope) VALUES ('02:00:00:00:23:0b','upstream')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO policy_sets (id,name) VALUES (1,'Upstream clients')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO policy_set_members (set_id,mac) VALUES (1,'02:00:00:00:23:0b')`); err != nil {
		t.Fatal(err)
	}

	err := inspectRecoveryFixture(db)
	if err == nil || !strings.Contains(err.Error(), "stored policy MAC scope validation failed") {
		t.Fatalf("recovery error=%v, want non-local policy MAC rejection", err)
	}
	if strings.Contains(err.Error(), "02:00:00:00:23:0b") {
		t.Fatalf("recovery error exposed a client identifier: %v", err)
	}
}

func TestInspectRecoveryRejectsPolicyMACsWithRoutedMonitorDevice(t *testing.T) {
	db := recoveryPolicyFixture(t)
	if _, err := db.SQL().Exec(`INSERT INTO clients (mac,scope) VALUES ('02:00:00:00:23:0c','local')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO policy_sets (id,name) VALUES (1,'Local clients')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO policy_set_members (set_id,mac) VALUES (1,'02:00:00:00:23:0c')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO devices (mac,host,name,role,functions_json,management_mode,adopted_at)
VALUES ('02:00:00:00:23:0d','192.0.2.13','observed-router','gateway','["gateway"]','monitor_only',1)`); err != nil {
		t.Fatal(err)
	}

	err := inspectRecoveryFixture(db)
	if err == nil || !strings.Contains(err.Error(), "stored policy MAC scope validation failed") {
		t.Fatalf("recovery error=%v, want routed monitor policy-MAC rejection", err)
	}
	if strings.Contains(err.Error(), "observed-router") {
		t.Fatalf("recovery error exposed a device identifier: %v", err)
	}
}

func TestInspectRecoveryRejectsDeviceRoleFunctionsMismatch(t *testing.T) {
	db := recoveryPolicyFixture(t)
	if _, err := db.SQL().Exec(`
INSERT INTO devices (mac,host,name,role,functions_json,management_mode,adopted_at)
VALUES ('02:00:00:00:23:02','192.0.2.2','mismatched-router','ap','["gateway"]','managed',1)`); err != nil {
		t.Fatal(err)
	}

	err := inspectRecoveryFixture(db)
	if err == nil || !strings.Contains(err.Error(), "device inventory validation failed") {
		t.Fatalf("recovery error=%v, want role/functions mismatch rejection", err)
	}
	if strings.Contains(err.Error(), "mismatched-router") {
		t.Fatalf("recovery error exposed a device identifier: %v", err)
	}
}

func TestInspectRecoveryRejectsAmplifiedPolicySetExpansion(t *testing.T) {
	db := recoveryPolicyFixture(t)
	tx, err := db.SQL().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(`INSERT INTO policy_sets (id,name) VALUES (1,'Amplified clients')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < model.MaxPolicySetMembers; i++ {
		mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x",
			byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
		if _, err := tx.Exec(`INSERT INTO clients (mac) VALUES (?)`, mac); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO policy_set_members (set_id,mac) VALUES (1,?)`, mac); err != nil {
			t.Fatal(err)
		}
	}
	policyCount := model.MaxExpandedPolicySourceMACs/model.MaxPolicySetMembers + 1
	for i := 0; i < policyCount; i++ {
		policy := model.Policy{ID: i + 1, Name: fmt.Sprintf("amplified-%d", i),
			Kind: model.PolicyFirewallRule, Origin: model.PolicyOriginManual, Enabled: i%2 == 0,
			Order: (i + 1) * 100,
			Firewall: &model.FirewallRule{Action: model.FirewallReject, SourceZone: "wan",
				Protocols: []string{"all"}, SourceSetID: 1}}
		raw, err := encodePolicy(policy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO fw_rules (id,sort,rule_json,enabled) VALUES (?,?,?,?)`,
			policy.ID, policy.Order, raw, policy.Enabled); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	err = inspectRecoveryFixture(db)
	if err == nil || !strings.Contains(err.Error(), "stored site validation failed") {
		t.Fatalf("recovery error=%v, want expanded policy-set rejection", err)
	}
}
