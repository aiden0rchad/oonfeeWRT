package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// These ceilings are intentionally far above a practical controller site but
// low enough that hostile 8 GiB artifacts cannot turn validation into a bulk
// Go allocation. Telemetry is checked by SQLite integrity, not materialized.
const (
	recoveryMaxAdmins        = 4096
	recoveryMaxDevices       = 4096
	recoveryMaxNetworks      = 256
	recoveryMaxZones         = 1024
	recoveryMaxPolicies      = 4096
	recoveryMaxPolicyClients = 16384
	recoveryMaxGroups        = 1024
	recoveryMaxGroupMembers  = 65536
	recoveryMaxWLANs         = 4096
	recoveryMaxMeshes        = 4096
	recoveryMaxUplinks       = 4096
	recoveryMaxOverrides     = 65536
	recoveryMaxOwnedSections = 262144
	recoveryMaxCatalogRows   = 4096
	recoveryMaxCatalogBytes  = 4 << 20
	recoveryMaxRowBytes      = 256 << 10
	recoveryMaxStateBytes    = 16 << 20
	recoveryMaxCipherBytes   = 64 << 10
	recoveryMaxAdminRowBytes = 4 << 10
	recoveryMaxPassHashBytes = 1024
)

// RecoveryCounts is the non-secret inventory from one stable validation
// snapshot. The public recovery package maps it to its long-lived API type.
type RecoveryCounts struct {
	Schema        int
	Devices       int
	Credentials   int
	OwnedSections int
	WLANs         int
	Meshes        int
}

type recoveryBound struct {
	label     string
	table     string
	where     string
	rowBytes  string
	maxRows   int64
	maxBytes  int64
	maxPerRow int64
}

var recoveryBounds = []recoveryBound{
	{"controller accounts", "admins", "", bytesOf("id", "username", "pass_hash", "created_at", "last_login", "role", "enabled", "deleted_at"), recoveryMaxAdmins, recoveryMaxStateBytes, recoveryMaxAdminRowBytes},
	{"device inventory", "devices", "", bytesOf("id", "mac", "host", "port", "scheme", "cert_fp", "host_key_fp", "name", "role", "functions_json", "adopted_at", "cred_enc", "class", "caps_json", "fw_release", "last_seen", "poll_state", "poll_interval_s"), recoveryMaxDevices, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"site", "site", "", bytesOf("id", "uuid", "name"), 1, recoveryMaxRowBytes, recoveryMaxRowBytes},
	{"networks", "networks", "", bytesOf("id", "name", "vlan", "cidr", "zone", "dhcp_json", "ipv6_json", "enabled"), recoveryMaxNetworks, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"zone policies", "zones", "", bytesOf("name", "policy_json"), recoveryMaxZones, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"policies", "fw_rules", "", bytesOf("id", "sort", "rule_json", "enabled"), recoveryMaxPolicies, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"client policies", "clients", "WHERE blocked<>0 OR COALESCE(fixed_ip,'')<>'' OR COALESCE(grp,'')<>''", bytesOf("mac", "fixed_ip", "blocked", "grp"), recoveryMaxPolicyClients, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"AP groups", "ap_groups", "", bytesOf("id", "name"), recoveryMaxGroups, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"AP group members", "ap_group_members", "", bytesOf("group_id", "device_id"), recoveryMaxGroupMembers, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"WLANs", "wlans", "", bytesOf("id", "ssid", "network_id", "group_id", "bands", "security_json", "security_key_enc", "roaming_json", "options_json", "enabled"), recoveryMaxWLANs, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"meshes", "meshes", "", bytesOf("id", "mesh_id", "network_id", "group_id", "band", "key", "key_enc", "enabled"), recoveryMaxMeshes, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"uplinks", "uplinks", "", bytesOf("id", "device_id", "wlan_id", "band", "enabled"), recoveryMaxUplinks, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"device overrides", "device_overrides", "", bytesOf("device_id", "path", "value_json"), recoveryMaxOverrides, recoveryMaxStateBytes, recoveryMaxRowBytes},
	{"owned sections", "owned_sections", "", bytesOf("device_id", "config", "section", "rendered_hash", "rendered_hash_enc", "applied_at"), recoveryMaxOwnedSections, recoveryMaxStateBytes, recoveryMaxRowBytes},
}

// bytesOf is used only with compile-time column names above.
func bytesOf(columns ...string) string {
	parts := make([]string, 0, len(columns))
	for _, column := range columns {
		parts = append(parts, "COALESCE(length(CAST("+column+" AS BLOB)),0)")
	}
	return strings.Join(parts, "+")
}

// InspectRecovery validates one immutable logical snapshot. It performs no
// writes even when db was opened writable, bounds every row set before any
// model or blob materialization, and never returns opened secret values.
func (db *DB) InspectRecovery(ctx context.Context,
	verifyCredential func(string, []byte) error,
	validatePasswordHash func(string) error) (counts RecoveryCounts, retErr error) {
	if ctx == nil {
		return counts, errors.New("recovery context is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	if db == nil || db.sql == nil {
		return counts, errors.New("recovery database is unavailable")
	}
	if verifyCredential == nil {
		return counts, errors.New("recovery credential verifier is unavailable")
	}
	if validatePasswordHash == nil {
		return counts, errors.New("recovery password-hash validator is unavailable")
	}

	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return counts, recoveryQueryError(ctx, "open recovery snapshot failed")
	}
	defer func() {
		if err := tx.Rollback(); retErr == nil && err != nil && !errors.Is(err, sql.ErrTxDone) {
			retErr = errors.New("close recovery snapshot failed")
		}
	}()

	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&counts.Schema); err != nil {
		return counts, recoveryQueryError(ctx, "read schema version failed")
	}
	if counts.Schema != schemaVersion {
		return counts, errors.New("recovery database schema is unsupported")
	}
	if err := validateRecoveryCatalogBounds(ctx, tx); err != nil {
		return counts, err
	}
	if err := verifyCurrentSchema(ctx, tx); err != nil {
		return counts, recoveryQueryError(ctx, "recovery database schema attestation failed")
	}
	if err := validateRecoveryBounds(ctx, tx); err != nil {
		return counts, err
	}
	if err := validateRecoveryCiphertextBounds(ctx, tx); err != nil {
		return counts, err
	}
	var integrity string
	if err := tx.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return counts, recoveryQueryError(ctx, "database integrity check failed")
	}
	foreignKeys, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return counts, recoveryQueryError(ctx, "foreign-key check failed")
	}
	hasForeignKeyFailure := foreignKeys.Next()
	foreignKeyReadErr := foreignKeys.Err()
	foreignKeyCloseErr := foreignKeys.Close()
	if hasForeignKeyFailure || foreignKeyReadErr != nil || foreignKeyCloseErr != nil {
		return counts, recoveryQueryError(ctx, "foreign-key check failed")
	}

	complete, err := db.verifySecretStateOn(ctx, tx)
	if err != nil || !complete {
		return counts, recoveryQueryError(ctx, "recovery database secret state failed verification")
	}
	if err := validateRecoveryAdmins(ctx, tx, validatePasswordHash); err != nil {
		return counts, err
	}

	site, err := db.siteOn(ctx, tx, false)
	if err != nil {
		return counts, recoveryQueryError(ctx, "stored site could not be opened")
	}
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	if len(site.Validate()) != 0 {
		return counts, errors.New("stored site validation failed")
	}
	counts.WLANs, counts.Meshes = len(site.WLANs), len(site.Meshes)

	if err := validateRecoveryDevices(ctx, tx, verifyCredential, &counts); err != nil {
		return counts, err
	}
	if err := validateRecoveryOwned(ctx, tx, db, &counts); err != nil {
		return counts, err
	}
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	return counts, nil
}

func validateRecoveryCatalogBounds(ctx context.Context, q siteReader) error {
	var rows, total, largest int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*),
	 COALESCE(SUM(length(CAST(COALESCE(sql,'') AS BLOB))),0),
	 COALESCE(MAX(length(CAST(COALESCE(sql,'') AS BLOB))),0)
	 FROM sqlite_master`).Scan(&rows, &total, &largest); err != nil {
		return recoveryQueryError(ctx, "recovery schema bounds could not be read")
	}
	if rows < 0 || rows > recoveryMaxCatalogRows || total < 0 ||
		total > recoveryMaxCatalogBytes || largest > recoveryMaxRowBytes {
		return errors.New("recovery schema exceeds validation bounds")
	}
	return nil
}

func validateRecoveryBounds(ctx context.Context, q siteReader) error {
	var total int64
	for _, bound := range recoveryBounds {
		if err := ctx.Err(); err != nil {
			return err
		}
		query := fmt.Sprintf(`SELECT COUNT(*),COALESCE(SUM(%s),0),COALESCE(MAX(%s),0) FROM %s %s`,
			bound.rowBytes, bound.rowBytes, bound.table, bound.where)
		var rows, bytes, perRow int64
		if err := q.QueryRowContext(ctx, query).Scan(&rows, &bytes, &perRow); err != nil {
			return recoveryQueryError(ctx, "recovery state bounds could not be read")
		}
		if rows < 0 || rows > bound.maxRows || bytes < 0 ||
			(bound.maxBytes > 0 && bytes > bound.maxBytes) ||
			(bound.maxPerRow > 0 && perRow > bound.maxPerRow) {
			return fmt.Errorf("recovery %s exceeds validation bounds", bound.label)
		}
		if bytes > recoveryMaxStateBytes-total {
			return errors.New("recovery desired state exceeds validation bounds")
		}
		total += bytes
	}
	return nil
}

func validateRecoveryCiphertextBounds(ctx context.Context, q siteReader) error {
	checks := []struct{ table, column string }{
		{"secret_state", "key_check"},
		{"devices", "cred_enc"},
		{"wlans", "security_key_enc"},
		{"meshes", "key_enc"},
		{"owned_sections", "rendered_hash_enc"},
	}
	for _, check := range checks {
		if err := ctx.Err(); err != nil {
			return err
		}
		query := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL AND length(%s)>?`,
			check.table, check.column, check.column)
		var oversized int64
		if err := q.QueryRowContext(ctx, query, recoveryMaxCipherBytes).Scan(&oversized); err != nil {
			return recoveryQueryError(ctx, "sealed recovery state bounds could not be read")
		}
		if oversized != 0 {
			return errors.New("sealed recovery state exceeds validation bounds")
		}
	}
	return nil
}

func validateRecoveryAdmins(ctx context.Context, q siteReader,
	validatePasswordHash func(string) error) error {
	var oversized int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins
	 WHERE length(CAST(username AS BLOB))>64 OR length(CAST(pass_hash AS BLOB))>?`,
		recoveryMaxPassHashBytes).Scan(&oversized); err != nil {
		return recoveryQueryError(ctx, "owner account inventory could not be read")
	}
	if oversized != 0 {
		return errors.New("controller account validation failed")
	}
	rows, err := q.QueryContext(ctx,
		`SELECT username,pass_hash,created_at,last_login,role,enabled,deleted_at
		 FROM admins ORDER BY id`)
	if err != nil {
		return recoveryQueryError(ctx, "owner account inventory could not be read")
	}
	defer rows.Close()
	enabledOwners := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var username, passHash string
		var role AccountRole
		var enabled int
		var created int64
		var lastLogin, deleted sql.NullInt64
		if err := rows.Scan(&username, &passHash, &created, &lastLogin, &role, &enabled, &deleted); err != nil {
			return recoveryQueryError(ctx, "owner account inventory could not be read")
		}
		hashErr := validatePasswordHash(passHash)
		if err := ctx.Err(); err != nil {
			return err
		}
		if !role.Valid() || enabled != 0 && enabled != 1 ||
			ValidateAccountUsername(username) != nil || hashErr != nil {
			return errors.New("controller account validation failed")
		}
		if role == RoleOwner && enabled == 1 && !deleted.Valid {
			enabledOwners++
		}
	}
	if err := rows.Err(); err != nil {
		return recoveryQueryError(ctx, "owner account inventory could not be read")
	}
	if enabledOwners == 0 {
		return errors.New("recovery database has no enabled owner account")
	}
	return nil
}

func validateRecoveryDevices(ctx context.Context, q siteReader,
	verifyCredential func(string, []byte) error, counts *RecoveryCounts) error {
	rows, err := q.QueryContext(ctx, deviceCols+` ORDER BY name,mac`)
	if err != nil {
		return recoveryQueryError(ctx, "device inventory could not be read")
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		device, err := scanDevice(rows)
		if err != nil {
			return recoveryQueryError(ctx, "device inventory could not be read")
		}
		counts.Devices++
		if device.FunctionError != "" {
			return errors.New("device inventory validation failed")
		}
		if device.Adopted() && len(device.CredEnc) == 0 {
			return errors.New("an adopted device has no stored credential")
		}
		if len(device.CredEnc) != 0 {
			verifyErr := verifyCredential(device.MAC, device.CredEnc)
			clear(device.CredEnc)
			if err := ctx.Err(); err != nil {
				return err
			}
			if verifyErr != nil {
				return errors.New("a stored device credential failed verification")
			}
			counts.Credentials++
		}
	}
	if err := rows.Err(); err != nil {
		return recoveryQueryError(ctx, "device inventory could not be read")
	}
	return nil
}

func validateRecoveryOwned(ctx context.Context, q siteReader, db *DB,
	counts *RecoveryCounts) error {
	rows, err := q.QueryContext(ctx, `SELECT device_id,config,section,
	 length(CAST(rendered_hash AS BLOB)),
	 rendered_hash_enc,applied_at
	 FROM owned_sections ORDER BY device_id,config,section`)
	if err != nil {
		return recoveryQueryError(ctx, "owned-section inventory could not be read")
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var deviceID int64
		var config, section string
		var legacyBytes int64
		var appliedAt int64
		var sealed []byte
		if err := rows.Scan(&deviceID, &config, &section, &legacyBytes, &sealed, &appliedAt); err != nil {
			return recoveryQueryError(ctx, "owned-section inventory could not be read")
		}
		if legacyBytes != 0 || len(sealed) == 0 {
			return errors.New("an owned-section verifier is missing or not sealed")
		}
		length, err := db.authenticateText(sealed, ownedHashAAD(deviceID, config, section),
			"ownership verifier")
		clear(sealed)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil || length == 0 {
			return errors.New("an owned-section verifier failed verification")
		}
		counts.OwnedSections++
	}
	if err := rows.Err(); err != nil {
		return recoveryQueryError(ctx, "owned-section inventory could not be read")
	}
	return nil
}

func recoveryQueryError(ctx context.Context, message string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return errors.New(message)
}
