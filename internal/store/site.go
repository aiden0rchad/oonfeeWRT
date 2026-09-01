package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// The site model's persistence: the desired state an operator expressed, which
// internal/render turns into UCI and internal/reconcile pushes to devices.
//
// The tables have been in schema.sql since the beginning; nothing read or wrote
// them until Phase 2, because until Phase 2 there were no screens to express
// anything with. This file is that gap closed.
//
// One property matters more than the rest: **the site UUID is generated once
// and never changes.** It seeds the deterministic mobility-domain derivation
// (IMPLEMENTATION §5), which is what lets every AP in a group compute the same
// 802.11r domain with no coordination between them. Regenerating it would
// silently re-key roaming across the whole fleet and break fast transition
// until every device had been re-applied.

type siteReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Site loads the whole desired state, creating the site row on first use.
func (db *DB) Site(ctx context.Context) (model.Site, error) {
	s, err := db.siteOn(ctx, db.sql, true)
	if err == sql.ErrNoRows {
		s, err = db.createSite(ctx)
		if err != nil {
			return model.Site{}, err
		}
		return db.populateSiteOn(ctx, db.sql, s, true)
	}
	if err != nil {
		return model.Site{}, fmt.Errorf("store: read site: %w", err)
	}
	return s, nil
}

// siteOn reads existing desired state through one caller-owned snapshot.
// revealSecrets=false authenticates sealed WLAN/mesh values but retains only
// the minimum presence/length shape required by model validation.
func (db *DB) siteOn(ctx context.Context, q siteReader, revealSecrets bool) (model.Site, error) {
	var s model.Site
	if err := q.QueryRowContext(ctx, `SELECT uuid, name FROM site WHERE id=1`).
		Scan(&s.UUID, &s.Name); err != nil {
		return model.Site{}, err
	}
	return db.populateSiteOn(ctx, q, s, revealSecrets)
}

func (db *DB) populateSiteOn(ctx context.Context, q siteReader, s model.Site,
	revealSecrets bool) (model.Site, error) {
	var err error
	if s.Networks, err = db.networksOn(ctx, q); err != nil {
		return model.Site{}, err
	}
	if s.Zones, err = db.zonePoliciesOn(ctx, q); err != nil {
		return model.Site{}, err
	}
	if s.Policies, err = db.policiesOn(ctx, q); err != nil {
		return model.Site{}, err
	}
	if s.PolicyClients, err = db.policyClientsOn(ctx, q); err != nil {
		return model.Site{}, err
	}
	if s.Groups, err = db.groupsOn(ctx, q); err != nil {
		return model.Site{}, err
	}
	if s.WLANs, err = db.wlansOn(ctx, q, revealSecrets); err != nil {
		return model.Site{}, err
	}
	if s.Uplinks, err = db.uplinksOn(ctx, q); err != nil {
		return s, err
	}
	if s.Meshes, err = db.meshesOn(ctx, q, revealSecrets); err != nil {
		return model.Site{}, err
	}
	if s.Overrides, err = db.overridesOn(ctx, q); err != nil {
		return model.Site{}, err
	}
	return s, nil
}

// ---- per-device overrides ----

// Overrides loads every device's deviations from the site model.
//
// A row whose path this build does not understand is SKIPPED rather than
// failing the load, and that asymmetry with the WLAN loader is deliberate. An
// unreadable security blob would mean publishing a network with unknown
// security, so it must fail; an unknown override key means one setting is not
// applied on one device, which is recoverable and much less bad than a
// controller that will not start because of a row from a newer version.
func (db *DB) Overrides(ctx context.Context) (model.Overrides, error) {
	return db.overridesOn(ctx, db.sql)
}

func (db *DB) overridesOn(ctx context.Context, q siteReader) (model.Overrides, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT device_id, path, value_json FROM device_overrides
		  ORDER BY device_id, path`)
	if err != nil {
		return nil, fmt.Errorf("store: list device overrides: %w", err)
	}
	defer rows.Close()
	out := model.Overrides{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var deviceID int64
		var path, raw string
		if err := rows.Scan(&deviceID, &path, &raw); err != nil {
			return nil, err
		}
		wlanID, key, err := model.ParseOverridePath(path)
		if err != nil {
			continue
		}
		var value string
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			continue
		}
		out[deviceID] = append(out[deviceID], model.Override{
			DeviceID: deviceID, WLANID: wlanID, Key: key, Value: value,
		})
	}
	return out, rows.Err()
}

// SetOverride records one deviation. An empty value removes it.
func (db *DB) SetOverride(ctx context.Context, o model.Override) error {
	if !o.Key.Valid() {
		return fmt.Errorf("store: %q is not an overridable setting", o.Key)
	}
	if o.Value == "" {
		return db.ClearOverride(ctx, o.DeviceID, o.Path())
	}
	raw, err := json.Marshal(o.Value)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO device_overrides (device_id, path, value_json) VALUES (?,?,?)
		 ON CONFLICT(device_id, path) DO UPDATE SET value_json = excluded.value_json`,
		o.DeviceID, o.Path(), string(raw))
	if err != nil {
		return fmt.Errorf("store: set override %s on device %d: %w",
			o.Path(), o.DeviceID, err)
	}
	return nil
}

// ClearOverride removes one deviation, returning the device to the site model.
func (db *DB) ClearOverride(ctx context.Context, deviceID int64, path string) error {
	_, err := db.sql.ExecContext(ctx,
		`DELETE FROM device_overrides WHERE device_id=? AND path=?`, deviceID, path)
	return err
}

// createSite writes the singleton row.
//
// The UUID is random and permanent. It is not derived from anything — not the
// hostname, not a device MAC — because every such source can change, and a
// mobility domain that changes when someone renames a controller is worse than
// one that means nothing to a human.
func (db *DB) createSite(ctx context.Context) (model.Site, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return model.Site{}, fmt.Errorf("store: generate site UUID: %w", err)
	}
	s := model.Site{UUID: hex.EncodeToString(buf[:]), Name: "Site"}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO site (id, uuid, name) VALUES (1, ?, ?)`, s.UUID, s.Name); err != nil {
		return model.Site{}, fmt.Errorf("store: create site: %w", err)
	}
	return s, nil
}

// SetSiteName renames the site. The UUID is deliberately not settable.
func (db *DB) SetSiteName(ctx context.Context, name string) error {
	if _, err := db.Site(ctx); err != nil { // ensure the row exists
		return err
	}
	_, err := db.sql.ExecContext(ctx, `UPDATE site SET name=? WHERE id=1`, name)
	return err
}

// ---- networks ----

func (db *DB) networks(ctx context.Context) ([]model.Network, error) {
	return db.networksOn(ctx, db.sql)
}

func (db *DB) networksOn(ctx context.Context, q siteReader) ([]model.Network, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, name, vlan, cidr, zone, dhcp_json, ipv6_json, enabled
		   FROM networks ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list networks: %w", err)
	}
	defer rows.Close()
	out := []model.Network{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var n model.Network
		var rawDHCP, rawIPv6 string
		if err := rows.Scan(&n.ID, &n.Name, &n.VLAN, &n.CIDR, &n.Zone,
			&rawDHCP, &rawIPv6, &n.Enabled); err != nil {
			return nil, err
		}
		dhcp := model.DefaultDHCPConfig()
		if err := json.Unmarshal([]byte(rawDHCP), &dhcp); err != nil {
			return nil, fmt.Errorf("store: decode DHCP for network %d: %w", n.ID, err)
		}
		n.DHCP = &dhcp
		n.LegacyDHCPDefaults = isLegacyDHCPJSON(rawDHCP)
		ipv6, err := decodeIPv6JSON(rawIPv6)
		if err != nil {
			return nil, fmt.Errorf("store: decode IPv6 for network %d: %w", n.ID, err)
		}
		n.IPv6 = &ipv6
		out = append(out, n)
	}
	return out, rows.Err()
}

// zonePolicies loads only explicit rows. The model supplies the legacy
// source -> wan default for active zones with no row.
//
// Policy is security state, so its JSON is decoded strictly: repairing a
// missing or misspelled forwarding list to an empty/default value would turn
// unreadable access control into a different access control without consent.
func (db *DB) zonePolicies(ctx context.Context) ([]model.ZonePolicy, error) {
	return db.zonePoliciesOn(ctx, db.sql)
}

func (db *DB) zonePoliciesOn(ctx context.Context, q siteReader) ([]model.ZonePolicy, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name, policy_json FROM zones ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list zone policies: %w", err)
	}
	defer rows.Close()
	out := []model.ZonePolicy{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var stored struct {
			ForwardTo *[]string `json:"forward_to"`
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&stored); err != nil {
			return nil, fmt.Errorf("store: zone policy %q has unreadable policy: %w", name, err)
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("store: zone policy %q has unreadable policy: trailing JSON", name)
		}
		if stored.ForwardTo == nil {
			return nil, fmt.Errorf("store: zone policy %q has unreadable policy: forward_to must be an array", name)
		}
		for _, dest := range *stored.ForwardTo {
			if strings.TrimSpace(dest) == "" {
				return nil, fmt.Errorf("store: zone policy %q has unreadable policy: forward_to must contain nonblank strings", name)
			}
		}
		out = append(out, model.ZonePolicy{
			Name: name, ForwardTo: model.CanonicalZoneDestinations(*stored.ForwardTo), Explicit: true,
		})
	}
	return out, rows.Err()
}

// SaveZonePolicy creates or replaces one explicit directional forwarding
// policy. An empty destination list is meaningful and is therefore persisted.
func (db *DB) SaveZonePolicy(ctx context.Context, p *model.ZonePolicy) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	if p == nil {
		return fmt.Errorf("store: a zone policy is required")
	}
	p.ForwardTo = model.CanonicalZoneDestinations(p.ForwardTo)
	p.Explicit = true
	site, err := db.Site(ctx)
	if err != nil {
		return err
	}
	replaced := false
	for i := range site.Zones {
		if site.Zones[i].Name == p.Name {
			site.Zones[i] = *p
			replaced = true
			break
		}
	}
	if !replaced {
		site.Zones = append(site.Zones, *p)
	}
	if errs := site.ValidateZonePolicies(); len(errs) > 0 {
		return fmt.Errorf("store: invalid zone policy: %w", errs[0])
	}
	raw, err := json.Marshal(struct {
		ForwardTo []string `json:"forward_to"`
	}{ForwardTo: p.ForwardTo})
	if err != nil {
		return fmt.Errorf("store: encode zone policy %q: %w", p.Name, err)
	}
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO zones (name, policy_json) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET policy_json=excluded.policy_json`,
		p.Name, string(raw))
	if err != nil {
		return fmt.Errorf("store: save zone policy %q: %w", p.Name, err)
	}
	return nil
}

// DeleteZonePolicy removes the explicit row, restoring the legacy source ->
// wan default for that active zone.
func (db *DB) DeleteZonePolicy(ctx context.Context, name string) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	res, err := db.sql.ExecContext(ctx, `DELETE FROM zones WHERE name=?`, name)
	if err != nil {
		return fmt.Errorf("store: reset zone policy %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ensureNetworkChangeSafe validates the actual fw4 identifiers and rejects an
// edit that would make a saved source or destination disappear. A policy must
// be changed explicitly; silently dropping it during a network rename could
// widen or narrow access.
func (db *DB) ensureNetworkChangeSafe(ctx context.Context, replacement *model.Network, deleting int) error {
	site, err := db.Site(ctx)
	if err != nil {
		return err
	}
	found := false
	networks := make([]model.Network, 0, len(site.Networks)+1)
	for _, current := range site.Networks {
		if deleting != 0 && current.ID == deleting {
			found = true
			continue
		}
		if replacement != nil && current.ID == replacement.ID {
			networks = append(networks, *replacement)
			found = true
			continue
		}
		networks = append(networks, current)
	}
	if replacement != nil && replacement.ID == 0 {
		networks = append(networks, *replacement)
		found = true
	}
	if !found {
		return ErrNotFound
	}
	site.Networks = networks
	if errs := site.ValidateZoneNames(); len(errs) > 0 {
		return fmt.Errorf("store: invalid network zone: %v", errs[0])
	}
	if errs := site.ValidateZonePolicies(); len(errs) > 0 {
		return fmt.Errorf("store: network change would orphan directional zone policy: %v; update or reset that zone policy first", errs[0])
	}
	if errs := site.ValidatePolicies(); len(errs) > 0 {
		return fmt.Errorf("store: network change would invalidate policy: %v; update or remove that policy first", errs[0])
	}
	return nil
}

// SaveNetwork inserts or updates a network. A zero ID inserts.
func (db *DB) SaveNetwork(ctx context.Context, n *model.Network) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("store: a network needs a name")
	}
	// A new network gets a firewall zone of its own, named after itself.
	//
	// It used to default to "lan", and nothing in the UI ever set it — so every
	// VLAN network the product could create asked the renderer for a second
	// firewall zone named lan, beside the one the device already has, carrying
	// input REJECT and forward REJECT. render.renderZones refuses that now
	// (it is the operator's zone, not ours to edit), which would have made the
	// default path a blocked apply.
	//
	// Naming it after the network is also what the renderer already assumed in
	// its own fallback, and it matches the zone's stated intent: a new network
	// is isolated — it can reach out and cannot reach in — until an operator
	// says otherwise.
	if n.Zone == "" {
		n.Zone = n.Name
	}
	if n.IPv6 != nil {
		if err := n.IPv6.Validate(); err != nil {
			return fmt.Errorf("store: invalid IPv6 policy for network %q: %w", n.Name, err)
		}
	}
	if err := db.ensureNetworkChangeSafe(ctx, n, 0); err != nil {
		return err
	}
	if n.ID == 0 {
		dhcp := n.EffectiveDHCP()
		rawDHCP, err := json.Marshal(dhcp)
		if err != nil {
			return fmt.Errorf("store: encode DHCP for network %q: %w", n.Name, err)
		}
		ipv6 := n.EffectiveIPv6()
		rawIPv6, err := json.Marshal(ipv6)
		if err != nil {
			return fmt.Errorf("store: encode IPv6 for network %q: %w", n.Name, err)
		}
		res, err := db.sql.ExecContext(ctx,
			`INSERT INTO networks (name, vlan, cidr, zone, dhcp_json, ipv6_json, enabled)
			 VALUES (?,?,?,?,?,?,?)`,
			n.Name, n.VLAN, n.CIDR, n.Zone, string(rawDHCP), string(rawIPv6), n.Enabled)
		if err != nil {
			return fmt.Errorf("store: create network: %w", err)
		}
		id, _ := res.LastInsertId()
		n.ID = int(id)
		n.DHCP = &dhcp
		n.IPv6 = &ipv6
		n.LegacyDHCPDefaults = false
		return nil
	}
	// A nil policy is an older or partial client's omission, not a request to
	// reset it. COALESCE keeps both policy columns unchanged atomically while
	// allowing either object to be updated independently.
	var rawDHCP, rawIPv6 any
	if n.DHCP != nil {
		encoded, err := json.Marshal(n.DHCP)
		if err != nil {
			return fmt.Errorf("store: encode DHCP for network %q: %w", n.Name, err)
		}
		rawDHCP = string(encoded)
	}
	if n.IPv6 != nil {
		encoded, err := json.Marshal(n.IPv6)
		if err != nil {
			return fmt.Errorf("store: encode IPv6 for network %q: %w", n.Name, err)
		}
		rawIPv6 = string(encoded)
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE networks
		    SET name=?, vlan=?, cidr=?, zone=?,
		        dhcp_json=COALESCE(?, dhcp_json),
		        ipv6_json=COALESCE(?, ipv6_json), enabled=?
		  WHERE id=?`,
		n.Name, n.VLAN, n.CIDR, n.Zone, rawDHCP, rawIPv6, n.Enabled, n.ID)
	if err != nil {
		return err
	}
	if changed, err := res.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return ErrNotFound
	}
	var storedDHCP, storedIPv6 string
	if err := db.sql.QueryRowContext(ctx,
		`SELECT dhcp_json, ipv6_json FROM networks WHERE id=?`, n.ID).
		Scan(&storedDHCP, &storedIPv6); err != nil {
		return err
	}
	dhcp := model.DefaultDHCPConfig()
	if err := json.Unmarshal([]byte(storedDHCP), &dhcp); err != nil {
		return fmt.Errorf("store: decode DHCP for network %d: %w", n.ID, err)
	}
	ipv6, err := decodeIPv6JSON(storedIPv6)
	if err != nil {
		return fmt.Errorf("store: decode IPv6 for network %d: %w", n.ID, err)
	}
	n.DHCP = &dhcp
	n.LegacyDHCPDefaults = isLegacyDHCPJSON(storedDHCP)
	n.IPv6 = &ipv6
	return nil
}

func isLegacyDHCPJSON(raw string) bool {
	return isEmptyJSONObject(raw)
}

func isEmptyJSONObject(raw string) bool {
	var fields map[string]json.RawMessage
	return json.Unmarshal([]byte(raw), &fields) == nil && fields != nil && len(fields) == 0
}

func decodeIPv6JSON(raw string) (model.IPv6Config, error) {
	if strings.TrimSpace(raw) == "null" {
		return model.IPv6Config{}, errors.New("IPv6 policy must be a JSON object")
	}
	var stored struct {
		Mode             *model.IPv6Mode `json:"mode"`
		AssignmentLength *int            `json:"assignment_length"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return model.IPv6Config{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return model.IPv6Config{}, errors.New("trailing JSON")
	}
	// The column existed before IPv6 policy was implemented and therefore has
	// historical {} rows. That exact empty object means preserve. Once either
	// policy field is present, require the complete policy rather than guessing
	// a missing value.
	if stored.Mode == nil && stored.AssignmentLength == nil {
		return model.DefaultIPv6Config(), nil
	}
	var missing []string
	if stored.Mode == nil {
		missing = append(missing, "mode")
	}
	if stored.AssignmentLength == nil {
		missing = append(missing, "assignment_length")
	}
	if len(missing) > 0 {
		return model.IPv6Config{}, fmt.Errorf("IPv6 policy is missing %s", strings.Join(missing, ", "))
	}
	config := model.IPv6Config{Mode: *stored.Mode, AssignmentLength: *stored.AssignmentLength}
	if err := config.Validate(); err != nil {
		return model.IPv6Config{}, err
	}
	return config, nil
}

// DeleteNetwork removes a network, refusing while a WLAN still points at it.
//
// The foreign key would catch this too, but as an opaque constraint error. A
// WLAN referencing a deleted network renders nothing and reports no reason,
// which is exactly the silent-gap failure this project keeps finding.
func (db *DB) DeleteNetwork(ctx context.Context, id int) error {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	var n int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wlans WHERE network_id=?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("store: %d WLAN(s) still use this network; "+
			"move or delete them first", n)
	}
	if err := db.ensureNetworkChangeSafe(ctx, nil, id); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `DELETE FROM networks WHERE id=?`, id)
	if err != nil {
		return err
	}
	if changed, err := res.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- AP groups ----

func (db *DB) groups(ctx context.Context) ([]model.APGroup, error) {
	return db.groupsOn(ctx, db.sql)
}

func (db *DB) groupsOn(ctx context.Context, q siteReader) ([]model.APGroup, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name FROM ap_groups ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list AP groups: %w", err)
	}
	defer rows.Close()
	byID := map[int]*model.APGroup{}
	out := []model.APGroup{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var g model.APGroup
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		byID[out[i].ID] = &out[i]
	}

	mrows, err := q.QueryContext(ctx,
		`SELECT group_id, device_id FROM ap_group_members ORDER BY group_id, device_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list AP group members: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var gid int
		var did int64
		if err := mrows.Scan(&gid, &did); err != nil {
			return nil, err
		}
		if g, ok := byID[gid]; ok {
			g.DeviceIDs = append(g.DeviceIDs, did)
		}
	}
	return out, mrows.Err()
}

// SaveGroup inserts or updates a group and replaces its membership.
func (db *DB) SaveGroup(ctx context.Context, g *model.APGroup) error {
	if strings.TrimSpace(g.Name) == "" {
		return fmt.Errorf("store: an AP group needs a name")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if g.ID == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO ap_groups (name) VALUES (?)`, g.Name)
		if err != nil {
			return fmt.Errorf("store: create AP group: %w", err)
		}
		id, _ := res.LastInsertId()
		g.ID = int(id)
	} else {
		res, err := tx.ExecContext(ctx,
			`UPDATE ap_groups SET name=? WHERE id=?`, g.Name, g.ID)
		if err != nil {
			return err
		}
		if changed, err := res.RowsAffected(); err != nil {
			return err
		} else if changed == 0 {
			return ErrNotFound
		}
	}
	// Replace membership wholesale. A diff would be cheaper and would also have
	// to get removal right; the set is a handful of rows.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ap_group_members WHERE group_id=?`, g.ID); err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, did := range g.DeviceIDs {
		if seen[did] {
			continue // a device listed twice is one member, not two
		}
		seen[did] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ap_group_members (group_id, device_id) VALUES (?,?)`,
			g.ID, did); err != nil {
			return fmt.Errorf("store: add device %d to group %d: %w", did, g.ID, err)
		}
	}
	return tx.Commit()
}

// DeleteGroup removes a group, refusing while a WLAN still targets it.
func (db *DB) DeleteGroup(ctx context.Context, id int) error {
	var n int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wlans WHERE group_id=?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("store: %d WLAN(s) still target this group; "+
			"move or delete them first", n)
	}
	res, err := db.sql.ExecContext(ctx, `DELETE FROM ap_groups WHERE id=?`, id)
	if err != nil {
		return err
	}
	if changed, err := res.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- WLANs ----

func (db *DB) wlans(ctx context.Context) ([]model.WLAN, error) {
	return db.wlansOn(ctx, db.sql, true)
}

func (db *DB) wlansOn(ctx context.Context, q siteReader, revealSecrets bool) ([]model.WLAN, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, ssid, network_id, group_id, bands, security_json,
		        security_key_enc, roaming_json, options_json, enabled
		   FROM wlans ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list WLANs: %w", err)
	}
	defer rows.Close()
	out := []model.WLAN{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var w model.WLAN
		var bands, roam, opts string
		var sec []byte
		var keyEnc []byte
		if err := rows.Scan(&w.ID, &w.SSID, &w.NetworkID, &w.GroupID, &bands,
			&sec, &keyEnc, &roam, &opts, &w.Enabled); err != nil {
			return nil, err
		}
		w.Bands = parseBands(bands)
		// A row whose JSON will not parse must not silently become a WLAN with
		// open security. Refuse the whole load instead: a site model that is
		// partly guessed is worse than one that will not open.
		var security storedSecurity
		decoder := json.NewDecoder(bytes.NewReader(sec))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&security); err != nil {
			clear(sec)
			return nil, fmt.Errorf("store: WLAN %d has unreadable security: %w", w.ID, err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			clear(sec)
			return nil, fmt.Errorf("store: WLAN %d has unreadable security: trailing JSON", w.ID)
		}
		clear(sec)
		w.Security.Mode, w.Security.PMF = security.Mode, security.PMF
		if revealSecrets {
			w.Security.Key, err = db.openText(keyEnc, wlanKeyAAD(w.ID), fmt.Sprintf("WLAN %d key", w.ID))
			clear(keyEnc)
			if err != nil {
				return nil, err
			}
		} else {
			length, authErr := db.authenticateText(keyEnc, wlanKeyAAD(w.ID), fmt.Sprintf("WLAN %d key", w.ID))
			clear(keyEnc)
			if authErr != nil {
				return nil, authErr
			}
			if length > 0 {
				w.Security.Key = "xxxxxxxx"
			}
		}
		if err := json.Unmarshal([]byte(roam), &w.Roaming); err != nil {
			return nil, fmt.Errorf("store: WLAN %d has unreadable roaming: %w", w.ID, err)
		}
		if err := json.Unmarshal([]byte(opts), &w.Options); err != nil {
			return nil, fmt.Errorf("store: WLAN %d has unreadable options: %w", w.ID, err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SaveWLAN inserts or updates a WLAN.
//
// An empty Security.Key on an UPDATE means "leave the key alone", not "clear
// it". The UI never has to round-trip a passphrase to change an unrelated
// field, which is the difference between a screen that can safely omit the
// secret and one that must carry it through every edit.
//
// Clearing a key is done by changing the security mode to one that needs none;
// there is no way to have a keyed mode with no key, and Site.Validate rejects
// it if one ever appears.
func (db *DB) SaveWLAN(ctx context.Context, w *model.WLAN) error {
	if strings.TrimSpace(w.SSID) == "" {
		return fmt.Errorf("store: a WLAN needs an SSID")
	}
	if !w.Security.Mode.NeedsKey() {
		w.Security.Key = ""
	}
	sec, err := json.Marshal(storedSecurity{Mode: w.Security.Mode, PMF: w.Security.PMF})
	if err != nil {
		return err
	}
	roam, err := json.Marshal(w.Roaming)
	if err != nil {
		return err
	}
	opts, err := json.Marshal(w.Options)
	if err != nil {
		return err
	}
	bands := formatBands(w.Bands)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin WLAN save: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if w.ID == 0 {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO wlans (ssid, network_id, group_id, bands, security_json,
			                    security_key_enc, roaming_json, options_json, enabled)
			 VALUES (?,?,?,?,?,NULL,?,?,?)`,
			w.SSID, w.NetworkID, w.GroupID, bands, string(sec),
			string(roam), string(opts), w.Enabled)
		if err != nil {
			return fmt.Errorf("store: create WLAN: %w", err)
		}
		id, _ := res.LastInsertId()
		w.ID = int(id)
		if w.Security.Key != "" {
			sealed, err := db.sealText(w.Security.Key, wlanKeyAAD(w.ID))
			if err != nil {
				return fmt.Errorf("store: seal WLAN %d key: %w", w.ID, err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE wlans SET security_key_enc=? WHERE id=?`, sealed, w.ID); err != nil {
				return fmt.Errorf("store: store WLAN %d key: %w", w.ID, err)
			}
		}
		return tx.Commit()
	}

	var keyEnc []byte
	if w.Security.Mode.NeedsKey() {
		if w.Security.Key == "" {
			if err := tx.QueryRowContext(ctx,
				`SELECT security_key_enc FROM wlans WHERE id=?`, w.ID).Scan(&keyEnc); err != nil {
				if err == sql.ErrNoRows {
					return ErrNotFound
				}
				return err
			}
			w.Security.Key, err = db.openText(keyEnc, wlanKeyAAD(w.ID), fmt.Sprintf("WLAN %d key", w.ID))
			if err != nil {
				return err
			}
		} else {
			keyEnc, err = db.sealText(w.Security.Key, wlanKeyAAD(w.ID))
			if err != nil {
				return fmt.Errorf("store: seal WLAN %d key: %w", w.ID, err)
			}
		}
	} else {
		// Switching to Open or OWE also erases any dormant old passphrase.
		w.Security.Key = ""
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE wlans SET ssid=?, network_id=?, group_id=?, bands=?, security_json=?,
		                  security_key_enc=?, roaming_json=?, options_json=?, enabled=? WHERE id=?`,
		w.SSID, w.NetworkID, w.GroupID, bands, string(sec), nullableBlob(keyEnc),
		string(roam), string(opts), w.Enabled, w.ID)
	if err != nil {
		return fmt.Errorf("store: update WLAN %d: %w", w.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit WLAN %d: %w", w.ID, err)
	}
	return nil
}

// DeleteWLAN removes a WLAN from the model.
//
// This does not touch any device. The sections it produced stay on their APs
// until the next apply, which prunes them — a delete that reached out to
// hardware immediately would be an apply nobody previewed.
func (db *DB) DeleteWLAN(ctx context.Context, id int) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM wlans WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- band encoding ----

// Bands are stored as a CSV string, which is what the schema has had all along.
// Parsing is lenient about spacing and case and drops anything unrecognised
// rather than failing the load: a band this build does not know about is a
// forward-compatibility question, not a corrupt row.
func parseBands(s string) []model.Band {
	out := []model.Band{}
	for _, part := range strings.Split(s, ",") {
		switch model.Band(strings.ToLower(strings.TrimSpace(part))) {
		case model.Band2G:
			out = append(out, model.Band2G)
		case model.Band5G:
			out = append(out, model.Band5G)
		case model.Band6G:
			out = append(out, model.Band6G)
		}
	}
	return out
}

func formatBands(bs []model.Band) string {
	seen := map[model.Band]bool{}
	parts := make([]string, 0, len(bs))
	for _, b := range bs {
		if seen[b] {
			continue
		}
		seen[b] = true
		parts = append(parts, string(b))
	}
	sort.Strings(parts) // stable storage, so a no-op save is a no-op
	return strings.Join(parts, ",")
}

// ---- 802.11s mesh backhauls ----

func (db *DB) meshes(ctx context.Context) ([]model.Mesh, error) {
	return db.meshesOn(ctx, db.sql, true)
}

func (db *DB) meshesOn(ctx context.Context, q siteReader, revealSecrets bool) ([]model.Mesh, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, mesh_id, network_id, group_id, band,
		        length(CAST(key AS BLOB)), key_enc, enabled
		   FROM meshes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list meshes: %w", err)
	}
	defer rows.Close()
	out := []model.Mesh{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var m model.Mesh
		var band string
		var legacyKeyBytes int64
		var keyEnc []byte
		if err := rows.Scan(&m.ID, &m.MeshID, &m.NetworkID, &m.GroupID,
			&band, &legacyKeyBytes, &keyEnc, &m.Enabled); err != nil {
			return nil, err
		}
		if legacyKeyBytes != 0 {
			return nil, fmt.Errorf("store: mesh %d still has an unsealed legacy key", m.ID)
		}
		if revealSecrets {
			m.Key, err = db.openText(keyEnc, meshKeyAAD(m.ID), fmt.Sprintf("mesh %d key", m.ID))
			clear(keyEnc)
			if err != nil {
				return nil, err
			}
		} else {
			length, authErr := db.authenticateText(keyEnc, meshKeyAAD(m.ID), fmt.Sprintf("mesh %d key", m.ID))
			clear(keyEnc)
			if authErr != nil {
				return nil, authErr
			}
			switch {
			case length >= 8:
				m.Key = "xxxxxxxx"
			case length > 0:
				m.Key = strings.Repeat("x", length)
			}
		}
		m.Band = model.Band(band)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveMesh inserts or updates a mesh backhaul.
//
// An empty Key on an update preserves the stored one, the same rule SaveWLAN
// follows: the API never sends a passphrase back out, so a client that read a
// mesh and wrote it back would otherwise silently convert an encrypted mesh
// into an open one — and an open mesh is joinable by anyone in radio range.
func (db *DB) SaveMesh(ctx context.Context, m *model.Mesh) error {
	return db.SaveMeshWithOptions(ctx, m, SaveMeshOptions{})
}

// SaveMeshOptions separates an omitted write-only key (preserve) from an
// explicit request to make the mesh open (erase). An empty string cannot carry
// both meanings safely.
type SaveMeshOptions struct {
	ClearKey bool
}

// SaveMeshWithOptions inserts or updates a mesh with explicit key semantics.
func (db *DB) SaveMeshWithOptions(ctx context.Context, m *model.Mesh, options SaveMeshOptions) error {
	if strings.TrimSpace(m.MeshID) == "" {
		return fmt.Errorf("store: a mesh needs a mesh ID")
	}
	if options.ClearKey && m.Key != "" {
		return errors.New("store: mesh key and ClearKey are mutually exclusive")
	}
	if options.ClearKey {
		m.Key = ""
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin mesh save: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if m.ID == 0 {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO meshes (mesh_id, network_id, group_id, band, key, key_enc, enabled)
			 VALUES (?,?,?,?, '', NULL, ?)`,
			m.MeshID, m.NetworkID, m.GroupID, string(m.Band), m.Enabled)
		if err != nil {
			return fmt.Errorf("store: create mesh: %w", err)
		}
		id, _ := res.LastInsertId()
		m.ID = int(id)
		if m.Key != "" {
			sealed, err := db.sealText(m.Key, meshKeyAAD(m.ID))
			if err != nil {
				return fmt.Errorf("store: seal mesh %d key: %w", m.ID, err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE meshes SET key_enc=? WHERE id=?`, sealed, m.ID); err != nil {
				return fmt.Errorf("store: store mesh %d key: %w", m.ID, err)
			}
		}
		return tx.Commit()
	}
	var keyEnc []byte
	if options.ClearKey {
		keyEnc = nil
	} else if m.Key == "" {
		if err := tx.QueryRowContext(ctx,
			`SELECT key_enc FROM meshes WHERE id=?`, m.ID).Scan(&keyEnc); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		m.Key, err = db.openText(keyEnc, meshKeyAAD(m.ID), fmt.Sprintf("mesh %d key", m.ID))
		if err != nil {
			return err
		}
	} else {
		keyEnc, err = db.sealText(m.Key, meshKeyAAD(m.ID))
		if err != nil {
			return fmt.Errorf("store: seal mesh %d key: %w", m.ID, err)
		}
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE meshes SET mesh_id=?, network_id=?, group_id=?, band=?, key='', key_enc=?,
		                   enabled=? WHERE id=?`,
		m.MeshID, m.NetworkID, m.GroupID, string(m.Band), nullableBlob(keyEnc), m.Enabled, m.ID)
	if err != nil {
		return fmt.Errorf("store: update mesh %d: %w", m.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// DeleteMesh removes a mesh backhaul.
func (db *DB) DeleteMesh(ctx context.Context, id int) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM meshes WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: delete mesh %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// uplinks loads every wireless uplink in the site.
func (db *DB) uplinks(ctx context.Context) ([]model.Uplink, error) {
	return db.uplinksOn(ctx, db.sql)
}

func (db *DB) uplinksOn(ctx context.Context, q siteReader) ([]model.Uplink, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, device_id, wlan_id, band, enabled FROM uplinks ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list uplinks: %w", err)
	}
	defer rows.Close()
	var out []model.Uplink
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var u model.Uplink
		var band string
		if err := rows.Scan(&u.ID, &u.DeviceID, &u.WLANID, &band, &u.Enabled); err != nil {
			return nil, err
		}
		u.Band = model.Band(band)
		out = append(out, u)
	}
	return out, rows.Err()
}

// SaveUplink creates or updates a device's wireless uplink.
//
// One per device, enforced by a UNIQUE constraint rather than by this function
// checking first: a router with two wireless uplinks into the same network is a
// layer-2 loop rather than redundancy, and a check-then-insert would let two
// concurrent writers past it. The constraint is turned into a sentence here
// because "UNIQUE constraint failed: uplinks.device_id" tells an operator
// nothing about loops.
func (db *DB) SaveUplink(ctx context.Context, u *model.Uplink) error {
	if u.DeviceID == 0 {
		return fmt.Errorf("store: a wireless uplink needs a device")
	}
	if u.Band == "" {
		return fmt.Errorf("store: a wireless uplink needs a band — a device " +
			"joins on one radio and leaves the other free to serve clients")
	}
	if u.ID == 0 {
		res, err := db.sql.ExecContext(ctx,
			`INSERT INTO uplinks (device_id, wlan_id, band, enabled) VALUES (?,?,?,?)`,
			u.DeviceID, u.WLANID, string(u.Band), u.Enabled)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("store: this device already has a wireless " +
					"uplink. A device with two would bridge the same network to " +
					"itself twice, which is a layer-2 loop rather than " +
					"redundancy — edit the existing one instead")
			}
			return fmt.Errorf("store: create uplink: %w", err)
		}
		id, _ := res.LastInsertId()
		u.ID = int(id)
		return nil
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE uplinks SET device_id=?, wlan_id=?, band=?, enabled=? WHERE id=?`,
		u.DeviceID, u.WLANID, string(u.Band), u.Enabled, u.ID)
	if err != nil {
		return fmt.Errorf("store: update uplink %d: %w", u.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUplink removes an uplink.
//
// The row going away is what makes the renderer prune the station section from
// the device — and on a device with no cable that section is its only route, so
// this is the one delete in the site model that can strand a device. The apply
// path is where that is caught: applyengine.IsUplinkSection makes the prune
// count as touching the management path, so it needs an explicit
// acknowledgment rather than going through as an ordinary wireless change.
func (db *DB) DeleteUplink(ctx context.Context, id int) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM uplinks WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: delete uplink %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
