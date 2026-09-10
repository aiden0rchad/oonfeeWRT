package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

type storedPolicy struct {
	Name        string              `json:"name"`
	Kind        model.PolicyKind    `json:"kind"`
	Origin      model.PolicyOrigin  `json:"origin"`
	Firewall    *model.FirewallRule `json:"firewall,omitempty"`
	PortForward *model.PortForward  `json:"port_forward,omitempty"`
	StaticRoute *model.StaticRoute  `json:"static_route,omitempty"`
}

func (db *DB) policySetsOn(ctx context.Context, q siteReader) ([]model.PolicySet, error) {
	rows, err := q.QueryContext(ctx, `
SELECT s.id, s.name, m.mac
  FROM policy_sets s
  LEFT JOIN policy_set_members m ON m.set_id=s.id
 ORDER BY s.id, m.mac`)
	if err != nil {
		return nil, fmt.Errorf("store: list policy sets: %w", err)
	}
	defer rows.Close()
	sets := []model.PolicySet{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var id int
		var name string
		var member sql.NullString
		if err := rows.Scan(&id, &name, &member); err != nil {
			return nil, err
		}
		if len(sets) == 0 || sets[len(sets)-1].ID != id {
			sets = append(sets, model.PolicySet{ID: id, Name: name, Members: []string{}})
		}
		if member.Valid {
			sets[len(sets)-1].Members = append(sets[len(sets)-1].Members, member.String)
		}
	}
	return sets, rows.Err()
}

// PolicyMACScopeProblems proves that MAC-based desired state can reach the one
// managed Gateway at layer 2. Presentation inventory is global, so policy
// trust comes only from the per-device observation recorded by that Gateway.
func (db *DB) PolicyMACScopeProblems(ctx context.Context, site model.Site,
	extraMACs ...string) (problems []error, retErr error) {
	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("validate policy MAC scope: begin snapshot: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); retErr == nil && err != nil && !errors.Is(err, sql.ErrTxDone) {
			retErr = fmt.Errorf("validate policy MAC scope: close snapshot: %w", err)
		}
	}()
	problems, retErr = db.policyMACScopeProblemsOn(ctx, tx, site, extraMACs...)
	return problems, retErr
}

func (db *DB) policyMACScopeProblemsOn(ctx context.Context, q siteReader,
	site model.Site, extraMACs ...string) ([]error, error) {
	now := time.Now()
	oldestObservation := now.Add(-DefaultClientTTL).Unix()
	newestObservation := now.Add(MaxClientObservationFutureSkew).Unix()
	macs := append([]string(nil), extraMACs...)
	for _, policy := range site.Policies {
		if !policy.Enabled || policy.Firewall == nil {
			continue
		}
		if policy.Firewall.SourceSetID > 0 {
			if set, ok := site.PolicySetByID(policy.Firewall.SourceSetID); ok {
				macs = append(macs, set.Members...)
			}
		} else {
			macs = append(macs, policy.Firewall.SourceMACs...)
		}
	}
	for _, client := range site.PolicyClients {
		if client.Blocked || client.FixedIP != "" {
			macs = append(macs, client.MAC)
		}
	}
	if len(macs) == 0 {
		return nil, nil
	}
	macs, err := model.CanonicalMACs(macs)
	if err != nil {
		return nil, fmt.Errorf("validate policy MAC scope: %w", err)
	}
	var gatewayID int64
	var gatewayRole, gatewayFunctionsJSON string
	err = q.QueryRowContext(ctx, `
SELECT id, role, functions_json
  FROM devices
 WHERE adopted_at IS NOT NULL
   AND management_mode='managed'
   AND role='gateway'
   AND functions_json IN ('["gateway"]','["gateway","ap"]','["gateway","switch"]','["gateway","ap","switch"]')
 LIMIT 1`).Scan(&gatewayID, &gatewayRole, &gatewayFunctionsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return []error{fmt.Errorf("MAC-based policy scope requires an adopted managed Gateway")}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("validate policy MAC scope: inspect managed Gateway: %w", err)
	}
	var storedFunctions []string
	role, roleErr := model.ParseRole(gatewayRole)
	decodeErr := json.Unmarshal([]byte(gatewayFunctionsJSON), &storedFunctions)
	functions, functionErr := model.ParseDeviceFunctions(storedFunctions, role)
	if roleErr != nil || decodeErr != nil || storedFunctions == nil || functionErr != nil || role != functions.PrimaryRole() {
		return []error{fmt.Errorf("MAC-based policy scope is unavailable because the managed Gateway has invalid stored function state")}, nil
	}
	const queryBatch = 500
	scopes := make(map[string]string, len(macs))
	for start := 0; start < len(macs); start += queryBatch {
		end := min(start+queryBatch, len(macs))
		args := make([]any, end-start)
		marks := make([]string, end-start)
		for i, mac := range macs[start:end] {
			args[i], marks[i] = mac, "?"
		}
		args = append([]any{gatewayID, oldestObservation, newestObservation}, args...)
		rows, err := q.QueryContext(ctx, `SELECT observed.mac,observed.scope
  FROM client_observations observed
 WHERE observed.device_id=?
   AND observed.last_seen BETWEEN ? AND ?
   AND observed.mac IN (`+strings.Join(marks, ",")+`)
   AND EXISTS (SELECT 1 FROM clients WHERE clients.mac=observed.mac COLLATE NOCASE)`, args...)
		if err != nil {
			return nil, fmt.Errorf("validate policy MAC scope: read client inventory: %w", err)
		}
		for rows.Next() {
			var mac, scope string
			if err := rows.Scan(&mac, &scope); err != nil {
				rows.Close()
				return nil, fmt.Errorf("validate policy MAC scope: read client inventory: %w", err)
			}
			scopes[mac] = scope
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil || closeErr != nil {
			return nil, fmt.Errorf("validate policy MAC scope: read client inventory: %w", errors.Join(err, closeErr))
		}
	}
	for _, mac := range macs {
		scope, exists := scopes[mac]
		if !exists {
			return []error{fmt.Errorf("policy client %s has not been observed by the managed Gateway", mac)}, nil
		}
		if scope != ScopeLocal {
			observed := ScopeUnknown
			if scope == ScopeUpstream {
				observed = ScopeUpstream
			}
			return []error{fmt.Errorf("policy client %s was observed as %s by the managed Gateway, not local", mac, observed)}, nil
		}
	}
	return nil, nil
}

// SavePolicySet creates or replaces one reusable source-MAC set. Membership is
// replaced atomically and each new member must already exist in client
// inventory, preventing a typo from becoming an enforceable identity.
func (db *DB) SavePolicySet(ctx context.Context, set *model.PolicySet) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	if set == nil {
		return fmt.Errorf("store: a policy set is required")
	}
	candidate := *set
	candidate.Members = append([]string(nil), set.Members...)
	members, err := model.CanonicalMACs(candidate.Members)
	if err != nil {
		return fmt.Errorf("store: invalid policy set: %w", err)
	}
	candidate.Members = members
	if len(candidate.Members) == 0 {
		return fmt.Errorf("store: invalid policy set: at least one member is required")
	}
	if len(candidate.Members) > model.MaxPolicySetMembers {
		return fmt.Errorf("store: invalid policy set: %d members exceeds maximum %d",
			len(candidate.Members), model.MaxPolicySetMembers)
	}
	if candidate.ID < 0 {
		return fmt.Errorf("store: invalid policy set id")
	}

	site, err := db.Site(ctx)
	if err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin policy set save: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	for _, mac := range candidate.Members {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM clients WHERE lower(mac)=lower(?))`, mac).Scan(&exists); err != nil {
			return fmt.Errorf("store: verify policy set member %s: %w", mac, err)
		}
		if !exists {
			return fmt.Errorf("store: policy set member %s is not in the observed client inventory", mac)
		}
	}

	if candidate.ID == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO policy_sets (name) VALUES (?)`, candidate.Name)
		if err != nil {
			return fmt.Errorf("store: create policy set %q: %w", candidate.Name, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: read policy set id: %w", err)
		}
		candidate.ID = int(id)
		site.PolicySets = append(site.PolicySets, candidate)
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE policy_sets SET name=? WHERE id=?`, candidate.Name, candidate.ID)
		if err != nil {
			return fmt.Errorf("store: update policy set %d: %w", candidate.ID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		found := false
		for i := range site.PolicySets {
			if site.PolicySets[i].ID == candidate.ID {
				site.PolicySets[i] = candidate
				found = true
				break
			}
		}
		if !found {
			return ErrNotFound
		}
	}

	if errs := site.ValidatePolicies(); len(errs) > 0 {
		return fmt.Errorf("store: invalid policy set: %w", errs[0])
	}
	if problems, err := db.policyMACScopeProblemsOn(ctx, tx, site, candidate.Members...); err != nil {
		return err
	} else if len(problems) > 0 {
		return fmt.Errorf("store: invalid policy set: %w", problems[0])
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM policy_set_members WHERE set_id=?`, candidate.ID); err != nil {
		return fmt.Errorf("store: replace policy set members: %w", err)
	}
	for _, mac := range candidate.Members {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO policy_set_members (set_id, mac) VALUES (?,?)`, candidate.ID, mac); err != nil {
			return fmt.Errorf("store: add %s to policy set %d: %w", mac, candidate.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit policy set: %w", err)
	}
	*set = candidate
	return nil
}

// DeletePolicySet refuses to orphan any saved rule, including disabled rules
// that could be enabled later.
func (db *DB) DeletePolicySet(ctx context.Context, id int) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	if id <= 0 {
		return ErrNotFound
	}
	site, err := db.Site(ctx)
	if err != nil {
		return err
	}
	for _, policy := range site.Policies {
		if policy.Firewall != nil && policy.Firewall.SourceSetID == id {
			return fmt.Errorf("store: policy %q still references this policy set; update or delete it first", policy.Name)
		}
	}
	res, err := db.sql.ExecContext(ctx, `DELETE FROM policy_sets WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: delete policy set %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) policies(ctx context.Context) ([]model.Policy, error) {
	return db.policiesOn(ctx, db.sql)
}

func (db *DB) policiesOn(ctx context.Context, q siteReader) ([]model.Policy, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, sort, rule_json, enabled FROM fw_rules ORDER BY sort, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list policies: %w", err)
	}
	defer rows.Close()
	out := []model.Policy{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var p model.Policy
		var raw string
		if err := rows.Scan(&p.ID, &p.Order, &raw, &p.Enabled); err != nil {
			return nil, err
		}
		var stored storedPolicy
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&stored); err != nil {
			return nil, fmt.Errorf("store: policy %d has unreadable rule: %w", p.ID, err)
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("store: policy %d has unreadable rule: trailing JSON", p.ID)
		}
		p.Name, p.Kind, p.Origin = stored.Name, stored.Kind, stored.Origin
		p.Firewall, p.PortForward, p.StaticRoute = stored.Firewall, stored.PortForward, stored.StaticRoute
		out = append(out, p)
	}
	return out, rows.Err()
}

func (db *DB) policyClients(ctx context.Context) ([]model.PolicyClient, error) {
	return db.policyClientsOn(ctx, db.sql)
}

func (db *DB) policyClientsOn(ctx context.Context, q siteReader) ([]model.PolicyClient, error) {
	rows, err := q.QueryContext(ctx, `
SELECT mac, COALESCE(grp,''), blocked, COALESCE(fixed_ip,'')
  FROM clients
 WHERE blocked != 0 OR COALESCE(fixed_ip,'') != '' OR COALESCE(grp,'') != ''
 ORDER BY lower(mac)`)
	if err != nil {
		return nil, fmt.Errorf("store: list client policy: %w", err)
	}
	defer rows.Close()
	out := []model.PolicyClient{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var client model.PolicyClient
		if err := rows.Scan(&client.MAC, &client.Group, &client.Blocked, &client.FixedIP); err != nil {
			return nil, err
		}
		out = append(out, client)
	}
	return out, rows.Err()
}

// SavePolicy persists desired state only. The next Preview binds its concrete
// per-device render; no router is contacted here.
func (db *DB) SavePolicy(ctx context.Context, p *model.Policy) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	if p == nil {
		return fmt.Errorf("store: a policy is required")
	}
	if p.Origin == "" {
		p.Origin = model.PolicyOriginManual
	}
	site, err := db.Site(ctx)
	if err != nil {
		return err
	}
	if p.ID == 0 && p.Order == 0 {
		for _, current := range site.Policies {
			if current.Order >= p.Order {
				p.Order = current.Order + 100
			}
		}
		if p.Order == 0 {
			p.Order = 100
		}
	}
	replaced := false
	candidate := -1
	clearsActiveMACPolicy := false
	for i := range site.Policies {
		if site.Policies[i].ID == p.ID && p.ID > 0 {
			clearsActiveMACPolicy = activeMACPolicy(site.Policies[i]) && !p.Enabled
			site.Policies[i] = *p
			replaced = true
			candidate = i
			break
		}
	}
	if p.ID > 0 && !replaced {
		return ErrNotFound
	}
	if !replaced {
		site.Policies = append(site.Policies, *p)
		candidate = len(site.Policies) - 1
	}
	if errs := site.ValidatePolicies(); len(errs) > 0 {
		return fmt.Errorf("store: invalid policy: %w", errs[0])
	}
	if !clearsActiveMACPolicy {
		if problems, err := db.PolicyMACScopeProblems(ctx, site); err != nil {
			return err
		} else if len(problems) > 0 {
			return fmt.Errorf("store: invalid policy: %w", problems[0])
		}
	}
	// ValidatePolicies canonicalizes set-like rule fields. Persist and return
	// that exact validated candidate, never the caller's pre-validation form.
	*p = site.Policies[candidate]
	raw, err := encodePolicy(*p)
	if err != nil {
		return err
	}
	if p.ID == 0 {
		res, err := db.sql.ExecContext(ctx,
			`INSERT INTO fw_rules (sort, rule_json, enabled) VALUES (?,?,?)`,
			p.Order, raw, p.Enabled)
		if err != nil {
			return fmt.Errorf("store: save policy %q: %w", p.Name, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: read saved policy id: %w", err)
		}
		p.ID = int(id)
		return nil
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE fw_rules SET sort=?, rule_json=?, enabled=? WHERE id=?`,
		p.Order, raw, p.Enabled, p.ID)
	if err != nil {
		return fmt.Errorf("store: save policy %q: %w", p.Name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func activeMACPolicy(policy model.Policy) bool {
	return policy.Enabled && policy.Firewall != nil &&
		(policy.Firewall.SourceSetID > 0 || len(policy.Firewall.SourceMACs) > 0)
}

func encodePolicy(p model.Policy) (string, error) {
	raw, err := json.Marshal(storedPolicy{
		Name: p.Name, Kind: p.Kind, Origin: p.Origin,
		Firewall: p.Firewall, PortForward: p.PortForward, StaticRoute: p.StaticRoute,
	})
	if err != nil {
		return "", fmt.Errorf("store: encode policy %q: %w", p.Name, err)
	}
	return string(raw), nil
}

func (db *DB) DeletePolicy(ctx context.Context, id int) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	res, err := db.sql.ExecContext(ctx, `DELETE FROM fw_rules WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: delete policy %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SaveClientPolicy updates only desired columns. Nil preserves a field; an
// empty fixed IP or group explicitly clears it.
func (db *DB) SaveClientPolicy(ctx context.Context, mac string, blocked *bool,
	fixedIP, group *string) (model.PolicyClient, error) {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	if blocked == nil && fixedIP == nil && group == nil {
		return model.PolicyClient{}, fmt.Errorf("store: client policy changed no fields")
	}
	var current model.PolicyClient
	err := db.sql.QueryRowContext(ctx, `
SELECT mac, COALESCE(grp,''), blocked, COALESCE(fixed_ip,'')
  FROM clients WHERE lower(mac)=lower(?)`, strings.TrimSpace(mac)).
		Scan(&current.MAC, &current.Group, &current.Blocked, &current.FixedIP)
	if err == sql.ErrNoRows {
		return model.PolicyClient{}, ErrNotFound
	}
	if err != nil {
		return model.PolicyClient{}, fmt.Errorf("store: read client policy: %w", err)
	}
	hadEnforcement := current.Blocked || current.FixedIP != ""
	if blocked != nil {
		current.Blocked = *blocked
	}
	if fixedIP != nil {
		current.FixedIP = strings.TrimSpace(*fixedIP)
	}
	if group != nil {
		current.Group = strings.TrimSpace(*group)
	}
	site, err := db.Site(ctx)
	if err != nil {
		return model.PolicyClient{}, err
	}
	found := false
	for i := range site.PolicyClients {
		if strings.EqualFold(site.PolicyClients[i].MAC, current.MAC) {
			site.PolicyClients[i] = current
			found = true
			break
		}
	}
	if !found {
		site.PolicyClients = append(site.PolicyClients, current)
	}
	clearsEnforcement := hadEnforcement && !current.Blocked && current.FixedIP == ""
	if clearsEnforcement {
		// A scope change can make several saved clients invalid at once. Permit
		// removing this client's final router-affecting intent without requiring
		// every other client to be repaired in the same request. The resulting
		// record is still validated in isolation, so this path cannot smuggle in
		// a malformed MAC or group while reducing enforcement.
		if errs := (model.Site{PolicyClients: []model.PolicyClient{current}}).ValidatePolicies(); len(errs) > 0 {
			return model.PolicyClient{}, fmt.Errorf("store: invalid client policy: %w", errs[0])
		}
	} else {
		if errs := site.ValidatePolicies(); len(errs) > 0 {
			return model.PolicyClient{}, fmt.Errorf("store: invalid client policy: %w", errs[0])
		}
		if problems, err := db.PolicyMACScopeProblems(ctx, site); err != nil {
			return model.PolicyClient{}, err
		} else if len(problems) > 0 {
			return model.PolicyClient{}, fmt.Errorf("store: invalid client policy: %w", problems[0])
		}
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE clients SET blocked=?, fixed_ip=NULLIF(?,''), grp=NULLIF(?,'') WHERE mac=?`,
		current.Blocked, current.FixedIP, current.Group, current.MAC)
	if err != nil {
		return model.PolicyClient{}, fmt.Errorf("store: save client policy: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return model.PolicyClient{}, fmt.Errorf("store: inspect saved client policy: %w", err)
	} else if n == 0 {
		return model.PolicyClient{}, ErrNotFound
	}
	return current, nil
}
