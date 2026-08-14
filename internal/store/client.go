package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Client is one thing on the network, as the Client Devices grid shows it.
//
// The identity is the MAC. That is a weaker identity than it used to be —
// phones randomise per SSID — but it is the only one the device reports, and
// pretending otherwise by inventing a fingerprint-based identity would silently
// merge two devices or split one.
type Client struct {
	MAC       string   `json:"mac"`
	Name      string   `json:"name"`
	Note      string   `json:"note,omitempty"`
	FixedIP   string   `json:"fixed_ip,omitempty"`
	Blocked   bool     `json:"blocked"`
	Group     string   `json:"group,omitempty"`
	FirstSeen *int64   `json:"first_seen"`
	LastSeen  *int64   `json:"last_seen"`
	IPv4      string   `json:"ipv4,omitempty"`
	DeviceID  *int64   `json:"device_id,omitempty"`
	Signal    *int     `json:"signal,omitempty"`
	Iface     string   `json:"iface,omitempty"`
	RxRate    *int64   `json:"rx_kbit,omitempty"`
	TxRate    *int64   `json:"tx_kbit,omitempty"`
	Uptime    *int64   `json:"connected_seconds,omitempty"`
	RetryPct  *float64 `json:"tx_retry_pct,omitempty"`

	// Scope is which side of the router this client is on: ScopeLocal,
	// ScopeUpstream, or ScopeUnknown.
	//
	// Three-state, and the third value is the point. A gateway's neighbour
	// tables cover every interface, so its client list mixes the network it
	// serves with the network it connects to — 8 of 16 on the reference device
	// were upstream. But a host with no observed IPv4, or one whose address
	// falls in no interface's subnet, has not been shown to be either, and
	// guessing would put someone else's hardware in a list captioned "your
	// devices".
	Scope string `json:"scope"`
}

// Client scopes. Empty in the database means undetermined, which is what a row
// written before the subnets were known still holds.
const (
	ScopeLocal    = "local"
	ScopeUpstream = "upstream"
	ScopeUnknown  = "unknown"
)

// SeenClient is what one poll learned about one host.
type SeenClient struct {
	MAC   string
	Name  string
	IPv4  string
	Scope string
}

// UpsertClients records the hosts a poll saw, in one transaction.
//
// Name is only written when we have one, and never overwritten with an empty
// string: a client that stops answering reverse DNS should not lose the name it
// had. The operator's own rename, when that exists, will need to win over both —
// which is why the column is separate from anything the device reports.
func (db *DB) UpsertClients(ctx context.Context, seen []SeenClient, now int64) error {
	if len(seen) == 0 {
		return nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin client upsert: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// scope follows the same rule as name and ip: a determination is never
	// overwritten with a non-determination. The subnets are re-read every
	// fifteen minutes, so a poll in between could otherwise downgrade a client
	// that was correctly classified back to "unknown" — and the grid would
	// flicker devices in and out of the view for reasons no operator could see.
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO clients (mac, name, ip, scope, first_seen, last_seen)
VALUES (?,?,?,?,?,?)
ON CONFLICT(mac) DO UPDATE SET
  name  = CASE WHEN excluded.name  != '' THEN excluded.name  ELSE clients.name  END,
  ip    = CASE WHEN excluded.ip    != '' THEN excluded.ip    ELSE clients.ip    END,
  scope = CASE WHEN excluded.scope NOT IN ('', ?) THEN excluded.scope ELSE clients.scope END,
  last_seen = excluded.last_seen`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range seen {
		if c.MAC == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, c.MAC, c.Name, c.IPv4, c.Scope,
			now, now, ScopeUnknown); err != nil {
			return fmt.Errorf("store: upsert client %s: %w", c.MAC, err)
		}
	}
	return tx.Commit()
}

// Clients lists known clients, newest activity first.
//
// activeSince, when non-zero, restricts the list to clients seen since then —
// the grid's default view. Passing zero returns everything ever seen, which is
// the "show offline clients" toggle.
func (db *DB) Clients(ctx context.Context, activeSince int64, limit int) ([]Client, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := db.sql.QueryContext(ctx, `
SELECT mac, COALESCE(name,''), COALESCE(note,''), COALESCE(fixed_ip,''),
       COALESCE(ip,''), blocked, COALESCE(grp,''), first_seen, last_seen,
       COALESCE(scope,'')
  FROM clients
 WHERE (? = 0 OR last_seen >= ?)
 ORDER BY last_seen DESC, mac
 LIMIT ?`, activeSince, activeSince, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list clients: %w", err)
	}
	defer rows.Close()
	out := []Client{}
	for rows.Next() {
		var c Client
		if err := rows.Scan(&c.MAC, &c.Name, &c.Note, &c.FixedIP, &c.IPv4,
			&c.Blocked, &c.Group, &c.FirstSeen, &c.LastSeen, &c.Scope); err != nil {
			return nil, err
		}
		if c.Scope == "" {
			// A row written before the subnets were known. Undetermined, and it
			// says so rather than defaulting to local — defaulting is how
			// someone else's hardware ends up in a list captioned "yours".
			c.Scope = ScopeUnknown
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneClients forgets clients not seen for a long time.
//
// Without it the table grows forever on any network with randomised MACs, where
// a single phone can produce a new "client" per SSID per reconnect.
func (db *DB) PruneClients(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`DELETE FROM clients WHERE last_seen IS NOT NULL AND last_seen < ?
		   AND blocked = 0 AND (note IS NULL OR note = '')`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune clients: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SetClientFingerprint stores whatever identification we have. Kept as opaque
// JSON: what goes in it is a Phase 4 question, and freezing a schema for it now
// would be guessing.
func (db *DB) SetClientFingerprint(ctx context.Context, mac string, fp any) error {
	blob, err := json.Marshal(fp)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx,
		`UPDATE clients SET fingerprint_json=? WHERE mac=?`, string(blob), mac)
	return err
}
