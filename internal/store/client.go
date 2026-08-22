package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
  scope = CASE
    WHEN excluded.scope IN ('', ?) THEN clients.scope
    WHEN excluded.ip = clients.ip AND clients.scope = ? AND excluded.scope = ? THEN clients.scope
    ELSE excluded.scope
  END,
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
			now, now, ScopeUnknown, ScopeLocal, ScopeUpstream); err != nil {
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
	return scanClients(rows)
}

// ClientsByMACs returns only inventory rows referenced by another bounded
// result, such as a topology graph. It avoids turning the entire historical
// lease table into disconnected UI nodes.
func (db *DB) ClientsByMACs(ctx context.Context, macs []string) ([]Client, error) {
	if len(macs) == 0 {
		return []Client{}, nil
	}
	seen := make(map[string]bool, len(macs))
	normalized := make([]string, 0, len(macs))
	for _, raw := range macs {
		mac, err := canonicalMAC(raw)
		if err != nil {
			return nil, fmt.Errorf("store: topology client MAC: %w", err)
		}
		if !seen[mac] {
			seen[mac] = true
			normalized = append(normalized, mac)
		}
	}
	if len(normalized) > 10_000 {
		return nil, fmt.Errorf("store: too many topology client identities: %d", len(normalized))
	}
	sort.Strings(normalized)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(normalized)), ",")
	args := make([]any, len(normalized))
	for i := range normalized {
		args[i] = normalized[i]
	}
	rows, err := db.sql.QueryContext(ctx, `
SELECT mac, COALESCE(name,''), COALESCE(note,''), COALESCE(fixed_ip,''),
       COALESCE(ip,''), blocked, COALESCE(grp,''), first_seen, last_seen,
       COALESCE(scope,'')
  FROM clients
 WHERE lower(mac) IN (`+placeholders+`)
 ORDER BY mac`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list topology clients: %w", err)
	}
	return scanClients(rows)
}

// ClientExists reports whether a MAC is in the observed client inventory.
// Desired policy state deliberately does not duplicate that inventory.
func (db *DB) ClientExists(ctx context.Context, mac string) (bool, error) {
	var exists bool
	err := db.sql.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM clients WHERE lower(mac)=lower(?))`,
		strings.TrimSpace(mac)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: find client: %w", err)
	}
	return exists, nil
}

// scanClients reads the client column list, which two queries share.
func scanClients(rows *sql.Rows) ([]Client, error) {
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

// ClientFilter narrows the client list to one page.
//
// The three filters are the grid's three rails. Two of them are answered by the
// clients table; Connection is not, and that is the interesting one — see
// ClientsPage.
type ClientFilter struct {
	// SeenSince bounds the list to clients seen since then; 0 is everything
	// ever seen, which is the grid's "show offline clients" toggle.
	SeenSince int64
	// ActiveSince is the line between online and offline.
	ActiveSince int64
	// LiveActive carries current authoritative evidence that has not reached a
	// rollup yet (hostapd, a confirmed neighbor, or recent dynamic bridge FDB).
	LiveActive []string

	// WirelessKinds are the telemetry series kinds whose presence means "this
	// MAC was associated to a radio". Supplied by the caller rather than
	// hardcoded here: the store does not own the telemetry vocabulary, and the
	// same list has to drive both this query and the per-row enrichment the API
	// does, or the rail and the rows disagree about what "wireless" means.
	WirelessKinds []string
	// LiveWireless are MACs the latest in-memory baseline hostapd reads report
	// associated. They must participate in the SQL dimension before paging;
	// overlaying them afterwards can paint a row wireless while the Wireless
	// filter and its facet have already excluded that same row.
	LiveWireless []string
	// ExcludeMACs are managed infrastructure identities. They stay in inventory
	// history but cannot enter a client page, facet, or dashboard count.
	ExcludeMACs []string

	Presence   string // "", "online", "offline"
	Connection string // "", "wireless", "unknown"
	Scope      string // "", "local", "upstream", "unknown"

	Limit, Offset int
}

// ClientPage is one page of the client list, and the counts its filter rail
// needs to be honest about what is off the page.
type ClientPage struct {
	Clients    []Client `json:"clients"`
	Total      int      `json:"total"`
	Presence   []Facet  `json:"presence"`
	Connection []Facet  `json:"connection"`
	Scope      []Facet  `json:"scope"`
}

// clientDim is one facetable dimension: an expression that yields its value for
// a row, and the arguments that expression needs.
type clientDim struct {
	expr string
	args []any
	sel  string // the currently selected value, "" for no filter
}

// pred renders this dimension as a WHERE predicate, or nothing when unselected.
func (d clientDim) pred() (string, []any) {
	if d.sel == "" {
		return "", nil
	}
	return d.expr + " = ?", append(append([]any{}, d.args...), d.sel)
}

// dims builds the three facetable dimensions as SQL expressions.
//
// Writing them as expressions rather than as three bespoke queries is what
// makes the faceting rule enforceable in one place: every facet is "group by my
// expression, filter by everybody else's".
func (f ClientFilter) dims() (presence, connection, scope clientDim) {
	presence = clientDim{
		expr: `CASE WHEN ` + f.activeExists() + ` THEN 'online' ELSE 'offline' END`,
		args: f.activeArgs(),
		sel:  f.Presence,
	}

	// Connection is derived from telemetry and the current in-memory hostapd
	// association set, not stored on the client row, and
	// that is why it is expressed in SQL rather than computed after the fetch.
	// A rail counted over the returned page reports "4 wireless" from a page of
	// 100 while the table holds four hundred — in the same typeface as a true
	// number. Deriving it here keeps one source (the station series) and one
	// definition, at the cost of a correlated EXISTS per row, which the
	// series(kind, key) index serves.
	//
	// The value for "no wireless evidence" is "unknown", never "wired": a
	// client no managed AP has reported has not been shown to be on a cable, and
	// labelling it as such invents a fact.
	connection = clientDim{
		expr: `CASE WHEN ` + f.wirelessExists() + ` THEN 'wireless' ELSE 'unknown' END`,
		args: f.wirelessArgs(),
		sel:  f.Connection,
	}

	scope = clientDim{
		expr: `COALESCE(NULLIF(scope,''), '` + ScopeUnknown + `')`,
		sel:  f.Scope,
	}
	return presence, connection, scope
}

func (f ClientFilter) activeExists() string {
	if len(f.LiveActive) > 0 {
		return `lower(clients.mac) IN
			(SELECT lower(value) FROM json_each(?))`
	}
	return `0`
}

func (f ClientFilter) activeArgs() []any {
	if len(f.LiveActive) > 0 {
		raw, _ := json.Marshal(f.LiveActive)
		return []any{string(raw)}
	}
	return nil
}

func (f ClientFilter) wirelessExists() string {
	parts := make([]string, 0, 2)
	if len(f.LiveWireless) > 0 {
		// One JSON parameter rather than one bind variable per associated MAC.
		// SQLite ships json_each, and a busy controller can legitimately have
		// more stations than a conservative variable limit permits.
		parts = append(parts, `lower(clients.mac) IN
			(SELECT lower(value) FROM json_each(?))`)
	}
	if len(f.WirelessKinds) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.WirelessKinds)), ",")
		parts = append(parts, `EXISTS (SELECT 1 FROM rollup_5m r
		                  JOIN series se ON se.id = r.series_id
		                 WHERE se.kind IN (`+ph+`)
		                   AND lower(se.key) = lower(clients.mac) AND r.ts >= ?)`)
	}
	if len(parts) == 0 {
		// No kinds means nothing can be shown to be wireless. Rendering this as
		// a constant false keeps the column present and every row "unknown",
		// which is the honest reading; omitting the dimension would instead
		// make the rail disappear.
		return `0`
	}
	return `(` + strings.Join(parts, ` OR `) + `)`
}

func (f ClientFilter) wirelessArgs() []any {
	args := make([]any, 0, len(f.WirelessKinds)+2)
	if len(f.LiveWireless) > 0 {
		raw, _ := json.Marshal(f.LiveWireless) // []string cannot fail to marshal
		args = append(args, string(raw))
	}
	for _, k := range f.WirelessKinds {
		args = append(args, k)
	}
	if len(f.WirelessKinds) > 0 {
		args = append(args, f.ActiveSince)
	}
	return args
}

func (f ClientFilter) basePredicate() (string, []any) {
	base := `(? = 0 OR last_seen >= ?)`
	args := []any{f.SeenSince, f.SeenSince}
	if len(f.ExcludeMACs) > 0 {
		raw, _ := json.Marshal(f.ExcludeMACs)
		base += ` AND lower(clients.mac) NOT IN
			(SELECT lower(value) FROM json_each(?))`
		args = append(args, string(raw))
	}
	return base, args
}

// ClientsPage returns one page of clients and the counts for all three rails.
//
// Filters go to the database, not to the page it returned. Filtering a fetched
// page selects from the newest N clients overall rather than the newest N
// matching, so a view filtered to "wireless" can come back empty while wireless
// clients exist — the same defect the event log had before it was paged in SQL.
//
// Each facet is counted with the OTHER filters applied but not its own, so
// every option answers "how many would I get if I clicked that instead?".
// Counting each with its own filter applied would show its selected value and
// zero beside everything else, which is a rail that can only ever narrow.
func (db *DB) ClientsPage(ctx context.Context, f ClientFilter) (ClientPage, error) {
	if f.Limit <= 0 {
		f.Limit = 500
	}
	presence, connection, scope := f.dims()

	// The base predicate applies to everything, facets included: it is the
	// list's scope, not one of its rails.
	base, baseArgs := f.basePredicate()

	where := func(dims ...clientDim) (string, []any) {
		sql := base
		args := append([]any{}, baseArgs...)
		for _, d := range dims {
			p, a := d.pred()
			if p == "" {
				continue
			}
			sql += " AND " + p
			args = append(args, a...)
		}
		return sql, args
	}

	var page ClientPage

	sel, args := where(presence, connection, scope)
	rows, err := db.sql.QueryContext(ctx, `
SELECT mac, COALESCE(name,''), COALESCE(note,''), COALESCE(fixed_ip,''),
       COALESCE(ip,''), blocked, COALESCE(grp,''), first_seen, last_seen,
       COALESCE(scope,'')
  FROM clients
 WHERE `+sel+`
 ORDER BY last_seen DESC, mac
 LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return page, fmt.Errorf("store: list clients: %w", err)
	}
	page.Clients, err = scanClients(rows)
	if err != nil {
		return page, err
	}

	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM clients WHERE `+sel, args...).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("store: count clients: %w", err)
	}

	for _, d := range []struct {
		dim   clientDim
		other []clientDim
		out   *[]Facet
	}{
		{presence, []clientDim{connection, scope}, &page.Presence},
		{connection, []clientDim{presence, scope}, &page.Connection},
		{scope, []clientDim{presence, connection}, &page.Scope},
	} {
		w, wargs := where(d.other...)
		facets, err := db.clientFacet(ctx, d.dim, w, wargs)
		if err != nil {
			return page, err
		}
		*d.out = facets
	}
	return page, nil
}

func (db *DB) clientFacet(ctx context.Context, d clientDim, where string, whereArgs []any) ([]Facet, error) {
	// The dimension's own arguments come first: it appears in the SELECT list
	// and the GROUP BY, both ahead of the WHERE clause.
	args := append(append([]any{}, d.args...), whereArgs...)
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+d.expr+` AS v, COUNT(*) FROM clients
		  WHERE `+where+`
		  GROUP BY v ORDER BY COUNT(*) DESC, v`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: facet clients: %w", err)
	}
	defer rows.Close()
	out := []Facet{}
	for rows.Next() {
		var f Facet
		if err := rows.Scan(&f.Value, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ClientScopeCount is how many clients sit in one scope: how many have ever
// been seen, and how many are current.
type ClientScopeCount struct {
	Total  int `json:"total"`
	Active int `json:"active"`
}

// ClientCounts totals clients by scope, in one query.
//
// This exists because the local/upstream distinction has two callers — the
// dashboard's headline number and the client grid's filter rail — and they must
// not answer it differently. They did: the grid counted scopes and the
// dashboard counted rows, so a fleet with 3 clients and 11 upstream neighbours
// showed "3" on one screen and "14" on the other, both labelled as this
// network's devices. Whichever a person read first was the one they distrusted
// afterwards.
//
// Counting in SQL rather than over a fetched page is the other half. The
// dashboard used to load up to 5000 client rows to call len() on them, which
// also silently capped the total at 5000; the grid counted whatever page it had
// received, which is correct only while the page is the whole table. Neither
// survives server-side paging, and a count that changes when you turn to page 2
// is worse than no count.
//
// SeenSince bounds which clients are counted at all (0 = everything ever seen);
// current authoritative evidence decides which count as Active. All three scopes are
// always present in the result, zero-filled, so a caller never has to tell an
// absent key from a genuine zero — "0 local, 11 upstream" is an answer, and a
// missing key would render as an empty rail instead.
func (db *DB) ClientCounts(ctx context.Context, f ClientFilter) (map[string]ClientScopeCount, error) {
	out := map[string]ClientScopeCount{
		ScopeLocal: {}, ScopeUpstream: {}, ScopeUnknown: {},
	}
	active := f.activeExists()
	base, baseArgs := f.basePredicate()
	args := []any{ScopeUnknown}
	args = append(args, f.activeArgs()...)
	args = append(args, baseArgs...)
	rows, err := db.sql.QueryContext(ctx, `
SELECT COALESCE(NULLIF(scope,''), ?) AS s,
       COUNT(*),
       COALESCE(SUM(CASE WHEN `+active+` THEN 1 ELSE 0 END), 0)
  FROM clients
 WHERE `+base+`
 GROUP BY s`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: count clients by scope: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var c ClientScopeCount
		if err := rows.Scan(&scope, &c.Total, &c.Active); err != nil {
			return nil, err
		}
		// A scope this build does not know about is still counted rather than
		// dropped: the alternative is a total that quietly omits rows.
		out[scope] = c
	}
	return out, rows.Err()
}

// PruneClients forgets clients not seen for a long time.
//
// Without it the table grows forever on any network with randomised MACs, where
// a single phone can produce a new "client" per SSID per reconnect.
func (db *DB) PruneClients(ctx context.Context, before time.Time) (int64, error) {
	db.siteMu.Lock()
	defer db.siteMu.Unlock()
	res, err := db.sql.ExecContext(ctx,
		`DELETE FROM clients WHERE last_seen IS NOT NULL AND last_seen < ?
		   AND blocked = 0 AND (note IS NULL OR note = '')
		   AND (fixed_ip IS NULL OR fixed_ip = '')
		   AND (grp IS NULL OR grp = '')`, before.Unix())
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
