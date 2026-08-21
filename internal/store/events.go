package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type EventQuery struct {
	Scope     string // general excludes audit; audit includes only audit; empty is legacy/all.
	Category  string
	Severity  string
	ClientMAC string
	Before    *EventCursor
	Limit     int
}

func (q *EventQuery) normalize() error {
	switch q.Scope {
	case "", "general", "audit":
	default:
		return fmt.Errorf("store: invalid event scope %q", q.Scope)
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 1001 {
		return errors.New("store: event page limit exceeds 1001")
	}
	if q.ClientMAC != "" {
		mac, err := canonicalMAC(q.ClientMAC)
		if err != nil {
			return fmt.Errorf("store: event client MAC: %w", err)
		}
		q.ClientMAC = mac
	}
	if q.Before != nil && (q.Before.TS < 0 || q.Before.ID <= 0) {
		return errors.New("store: invalid event cursor")
	}
	return nil
}

// QueryEventsKeyset returns a stable page while new events are inserted.
func (db *DB) QueryEventsKeyset(ctx context.Context, query EventQuery) ([]Event, error) {
	if err := query.normalize(); err != nil {
		return nil, err
	}
	sqlText := `SELECT ` + eventColumns + ` FROM events
 WHERE (? = '' OR (? = 'general' AND category != 'audit') OR (? = 'audit' AND category = 'audit'))
   AND (? = '' OR category = ?)
   AND (? = '' OR severity = ?)
   AND (? = '' OR client_mac = ?)`
	args := []any{query.Scope, query.Scope, query.Scope,
		query.Category, query.Category, query.Severity, query.Severity,
		query.ClientMAC, query.ClientMAC}
	if query.Before != nil {
		sqlText += ` AND (ts < ? OR (ts = ? AND id < ?))`
		args = append(args, query.Before.TS, query.Before.TS, query.Before.ID)
	}
	sqlText += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, query.Limit)
	rows, err := db.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

func (db *DB) EventByID(ctx context.Context, id int64) (Event, error) {
	if id <= 0 {
		return Event{}, errors.New("store: event id must be positive")
	}
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE id=? LIMIT 1`, id)
	if err != nil {
		return Event{}, err
	}
	events, err := scanEvents(rows)
	if err != nil {
		return Event{}, err
	}
	if len(events) == 0 {
		return Event{}, ErrNotFound
	}
	return events[0], nil
}

func (db *DB) EventFacetsScoped(ctx context.Context, query EventQuery) (
	cats, sevs []Facet, total int, err error) {
	query.Before = nil
	query.Limit = 1
	if err := query.normalize(); err != nil {
		return nil, nil, 0, err
	}
	if query.ClientMAC != "" {
		return nil, nil, 0, errors.New("store: scoped event facets do not accept a client filter")
	}
	facet := func(group, other, value string) ([]Facet, error) {
		if (group != "category" && group != "severity") ||
			(other != "category" && other != "severity") {
			return nil, errors.New("store: invalid event facet column")
		}
		rows, err := db.sql.QueryContext(ctx, `SELECT `+group+`, COUNT(*) FROM events
 WHERE (? = '' OR (? = 'general' AND category != 'audit') OR (? = 'audit' AND category = 'audit'))
   AND (? = '' OR `+other+` = ?)
 GROUP BY `+group+` ORDER BY COUNT(*) DESC, `+group,
			query.Scope, query.Scope, query.Scope, value, value)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []Facet{}
		for rows.Next() {
			var item Facet
			if err := rows.Scan(&item.Value, &item.Count); err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, rows.Err()
	}
	sevs, err = facet("severity", "category", query.Category)
	if err != nil {
		return nil, nil, 0, err
	}
	cats, err = facet("category", "severity", query.Severity)
	if err != nil {
		return nil, nil, 0, err
	}
	err = db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM events
 WHERE (? = '' OR (? = 'general' AND category != 'audit') OR (? = 'audit' AND category = 'audit'))
   AND (? = '' OR category = ?) AND (? = '' OR severity = ?)`,
		query.Scope, query.Scope, query.Scope, query.Category, query.Category,
		query.Severity, query.Severity).Scan(&total)
	return cats, sevs, total, err
}

func normalizeEventScope(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
