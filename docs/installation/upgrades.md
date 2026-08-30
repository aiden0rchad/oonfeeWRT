# Upgrade and roll back

oonfeeWRT keeps controller state in SQLite plus a separate keyring. Upgrade safety depends on preserving a matching database/keyring/passphrase set before replacing a binary or image.

> **Outcome:** The controller runs v0.1.3 with its existing state intact, and you retain a verified recovery point suitable for the version you may need to restore.

## Before you begin

- Read the release notes for the version you are installing.
- Know whether the current install is a standalone binary or container.
- Locate the data directory or volume and the runtime passphrase file.
- Schedule a clean stop; do not upgrade during Apply, backup, restore, diagnostics generation, RF scan, or optional-package work.
- Preserve enough downtime to verify the new process before resuming changes.

**Router write impact:** Replacing the binary/image and migrating the database do not themselves contact or configure routers. After startup, read-only polling resumes and, when the write gate is open, automatic 802.11k neighbour reconciliation may update runtime hostapd neighbour lists. A restore, unlike an ordinary upgrade, activates a persistent router-write safety gate.

## Version facts for v0.1.3

- v0.1.3 uses controller database schema 19.
- v0.1.2 also uses schema 19, so v0.1.2 → v0.1.3 requires no schema migration.
- The one-time speed-test history pruning introduced by v0.1.1 is unchanged;
  v0.1.3 adds no startup data deletion.
- A clean v0.1.3 → v0.1.2 rollback is schema-compatible, though you should
  still retain the v0.1.3 recovery pair.
- v0.1.3's effective-WAN observation runs `/sbin/ip -4 route show table all`,
  a read-only command already present in the scoped ACL since v0.1.0. Existing
  adopted routers need neither ACL refresh nor re-adoption for this upgrade.
- Historical v0.1.0-rc.1 uses schema 17. Moving from that RC to a stable schema-19 daemon migrates forward; returning to the RC requires restoring the untouched schema-17 backup, not merely replacing the executable or image.

## 1. Create a verified pre-upgrade backup

The recommended operator path is **Settings → Backup & Restore**. Export an encrypted `.oowrtbak`, download it before it expires, record its separate export passphrase, and verify that the job completed.

For a standalone filesystem recovery pair while the controller is live, use SQLite's backup API:

```sh
install -d -m 0700 /path/to/private-backup
sqlite3 "$HOME/.local/share/oonfeewrt/oonfeewrt.db" \
  ".backup '/path/to/private-backup/oonfeewrt.db'"
cp -p "$HOME/.local/share/oonfeewrt/keyring.json" \
  /path/to/private-backup/keyring.json
```

Verify it:

```sh
OONFEE_PASSPHRASE_FILE="$HOME/.config/oonfeewrt/passphrase" \
  oonfeewrt-recoverycheck /path/to/private-backup/oonfeewrt.db
```

For the documented bind-mounted `docker run` layout, stop cleanly before copying:

```sh
docker stop --time 150 oonfeewrt
install -d -m 0700 /path/to/private-backup
cp -p "$HOME/.local/share/oonfeewrt-container/oonfeewrt.db" \
  "$HOME/.local/share/oonfeewrt-container/keyring.json" \
  /path/to/private-backup/
docker start oonfeewrt
```

For a Compose named volume, the portable `.oowrtbak` workflow avoids unsafe ad hoc copying. If you use volume-level backup tooling instead, stop the container first and preserve the whole database/keyring unit.

Never copy only the main SQLite file while WAL is active. It may omit committed state.

## 2A. Upgrade a standalone binary

1. Download, checksum-verify, and extract v0.1.3 using [Install the binary](binary.md).
2. Stop the old daemon using the same process manager or foreground terminal that started it. Give it time to finish a graceful shutdown.
3. Replace the executable:

   ```sh
   sudo install -m 0755 "$NAME/oonfeewrtd" /usr/local/bin/oonfeewrtd
   sudo install -m 0755 "$NAME/oonfeewrt-recoverycheck" \
     /usr/local/bin/oonfeewrt-recoverycheck
   ```

4. Confirm the installed version:

   ```sh
   oonfeewrtd -version
   ```

5. Start it with the unchanged absolute data directory and unchanged runtime passphrase source.

Do not point a new process at a copied database while leaving the old process running against the same files.

## 2B. Upgrade Docker Compose

From the directory containing the release Compose file and passphrase:

```sh
OONFEE_VERSION=v0.1.3 docker compose pull
OONFEE_VERSION=v0.1.3 docker compose up -d
```

The service keeps the existing `oonfee-data` volume and passphrase bind mount. Confirm that you did not add `-v` to any `down` command.

## 3. Verify the upgraded controller

```sh
curl --fail http://127.0.0.1:8080/healthz
```

For Compose:

```sh
OONFEE_VERSION=v0.1.3 docker compose ps
OONFEE_VERSION=v0.1.3 docker compose logs --tail=200 oonfeewrt
```

In the browser:

1. Sign in again; sessions are process-local and do not survive restart.
2. Confirm the expected devices, site settings, accounts, and event history.
3. Confirm devices resume read-only polling.
4. On a PPPoE or multi-default-candidate Gateway, allow one network/topology
   cycle (up to approximately 15 minutes), then verify the Dashboard path and
   device WAN chart use the installed main-table route's kernel device.
5. Confirm an unavailable route explains its source gap instead of selecting
   an equal-metric, multipath, or unmappable candidate.
6. Open **Settings → Backup & Restore** and confirm no restore-based router-write suppression is active after an ordinary upgrade.
7. Run Preview before the next Apply; do not assume desired and observed state still match after downtime.

## Roll back v0.1.3 to v0.1.2

v0.1.2 and v0.1.3 both use schema 19. Retain the current v0.1.3 data pair
first, stop v0.1.3 cleanly, replace the binary or image with v0.1.2, and start
it against the schema-19 data.

Rollback also removes v0.1.3's installed-main-route selection and explicit WAN
series proof. On PPPoE or multi-candidate gateways, Dashboard, topology, client
scope, and device traffic views can again follow older netifd-order/heuristic
behavior. Treat that as a functional rollback, not a data migration issue.

The v0.1.1 startup pruning of older speed-test rows cannot be reversed unless
those rows exist in a pre-v0.1.1 backup.

## Roll back to v0.1.0-rc.1

Do not point the RC daemon at a schema-19 database. Rollback is a data restore:

1. Stop the stable controller.
2. Retain its schema-19 database/keyring pair separately.
3. Restore the untouched schema-17 database and matching `keyring.json` captured before migration.
4. Use the prior runtime passphrase file.
5. Install the v0.1.0-rc.1 binary or image.
6. Start and verify the RC.

Controller rollback does not roll back router configuration.

## Troubleshooting and recovery

### Startup refuses to downgrade the database

The database schema is newer than the daemon understands. Stop. Install the compatible newer daemon or restore the older version's matching pre-upgrade database/keyring pair.

### The new daemon cannot unlock the keyring

Verify that the unchanged runtime passphrase file and the keyring from the same data pair are present. Do not create a new keyring over the existing database.

### The controller is empty after upgrade

It is probably using a new path or volume. Stop it before making changes. Reconnect the original data directory/volume and matching passphrase source, then restart.

### The UI loads old or missing assets

Reload the page. Official binaries embed content-hashed assets and serve `index.html` without persistent caching. Confirm the proxy does not cache `index.html` or override API `no-store` headers.

### A migration fails

Leave the failed data pair untouched for diagnosis. Restore the verified pre-upgrade pair and old version instead of manually editing `schema_version` or database tables.

## Next steps

- [Back up and restore the controller](../operations/backups.md)
- [Review routine maintenance](../operations/maintenance.md)
- [Verify reverse-proxy TLS](reverse-proxy.md)
