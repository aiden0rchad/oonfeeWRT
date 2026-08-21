package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"

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

func (db *DB) policies(ctx context.Context) ([]model.Policy, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT id, sort, rule_json, enabled FROM fw_rules ORDER BY sort, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list policies: %w", err)
	}
	defer rows.Close()
	out := []model.Policy{}
	for rows.Next() {
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
	rows, err := db.sql.QueryContext(ctx, `
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
	for i := range site.Policies {
		if site.Policies[i].ID == p.ID && p.ID > 0 {
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
	if errs := site.ValidatePolicies(); len(errs) > 0 {
		return model.PolicyClient{}, fmt.Errorf("store: invalid client policy: %w", errs[0])
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
