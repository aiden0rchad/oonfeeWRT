# Upgrade and roll back

oonfeeWRT keeps controller state in SQLite plus a separate keyring. Upgrade safety depends on preserving a matching database/keyring/passphrase set before replacing a binary or image.

> **Outcome:** The controller runs v0.1.4 with its existing state intact, and you retain a verified schema-19 recovery point for any rollback to v0.1.3.

## Before you begin

- Read the release notes for the version you are installing.
- Know whether the current install is a standalone binary or container.
- Locate the data directory or volume and the runtime passphrase file.
- Schedule a clean stop; do not upgrade during Apply, backup, restore, diagnostics generation, RF scan, or optional-package work.
- Preserve enough downtime to verify the new process before resuming changes.

**Router write impact:** Replacing the binary/image and migrating the database do not themselves contact or configure routers. After startup, read-only polling resumes and, when the write gate is open, automatic 802.11k neighbour reconciliation may update runtime hostapd neighbour lists. A restore, unlike an ordinary upgrade, activates a persistent router-write safety gate.

## Version facts for v0.1.4

- v0.1.4 uses controller database schema 20. Stable v0.1.1 through v0.1.3 use
  schema 19, which v0.1.4 migrates automatically at startup.
- The migration adds a closed-topology history index and normalizes the old
  development-era `luci.getNetworkDevices` and `luci.getWirelessDevices`
  source names to their actual `luci-rpc` names. If both spellings exist for a
  device, only the newest source-state observation is retained. User
  configuration, credentials, secrets, and topology intervals are preserved.
- v0.1.3 cannot open schema 20. A v0.1.4 → v0.1.3 rollback requires restoring
  the matching pre-upgrade schema-19 database, keyring, and passphrase.
- Existing networks decode as **Router managed**, so startup does not change
  their IPv6 settings. Prefix delegation or Disable still requires Preview and
  Apply.
- Existing router access remains sufficient for normal polling. Router-clock
  status uses newly allowlisted read-only LuCI methods; already-adopted routers
  need a separately reviewed controller-access refresh only if that clock
  status is wanted. Re-adoption is not required.
- Historical v0.1.0-rc.1 uses schema 17. v0.1.4 can migrate supported schema-17
  state through schemas 18 and 19 to schema 20. Returning to the RC requires
  restoring the untouched schema-17 backup, not merely replacing the executable
  or image.

## 1. Create a verified pre-upgrade backup

Always use **Settings → Backup & Restore** to export an encrypted `.oowrtbak`,
download it before it expires, record its separate export passphrase, and
verify that the job completed. For the simplest direct rollback to v0.1.3,
also retain one of the raw schema-19 recovery units below before v0.1.4 opens
the live data. The public recovery helper verifies raw databases but does not
extract `.oowrtbak` files.

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

For a Compose named volume, stop the service and use trusted volume-snapshot
tooling to preserve the whole volume if you want a direct in-place rollback.
Record the exact Compose project/volume identity and retain the matching
passphrase file. A portable `.oowrtbak` remains the preferred off-host backup,
but restoring it to v0.1.3 uses the separate clean-instance path below rather
than an in-place file extraction.

Never copy only the main SQLite file while WAL is active. It may omit committed state.

## 2A. Upgrade a standalone binary

1. Download, checksum-verify, and extract v0.1.4 using [Install the binary](binary.md).
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

The v0.1.3 Compose file does not contain `OONFEE_HTTP_BIND`. Download the new
file beside the existing one, compare it, and reapply only intentional local
changes. Do not replace `.env`, `passphrase`, or the named volume:

```sh
curl --fail --location \
  --output docker-compose.yml.v0.1.4 \
  https://raw.githubusercontent.com/aiden0rchad/oonfeeWRT/v0.1.4/deploy/docker-compose.yml
diff -u docker-compose.yml docker-compose.yml.v0.1.4
OONFEE_VERSION=v0.1.4 docker compose -f docker-compose.yml.v0.1.4 config --quiet
```

After reviewing the diff, replace `docker-compose.yml` with the v0.1.4 file or
merge its changes deliberately. From that directory:

```sh
OONFEE_VERSION=v0.1.4 docker compose pull
OONFEE_VERSION=v0.1.4 docker compose up -d
```

The service keeps the existing `oonfee-data` volume and passphrase bind mount. Confirm that you did not add `-v` to any `down` command.

Loopback remains the default. To use the new trusted-management-LAN bind, put
`OONFEE_HTTP_BIND=<controller-LAN-IP>` in `.env` before `pull`/`up`, or repeat
it on every Compose lifecycle command. Do not publish raw port 8080 to the
Internet.

## 3. Verify the upgraded controller

```sh
curl --fail http://127.0.0.1:8080/healthz
```

For Compose:

```sh
OONFEE_VERSION=v0.1.4 docker compose ps
OONFEE_VERSION=v0.1.4 docker compose logs --tail=200 oonfeewrt
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
6. Open **Settings → Networks** and confirm existing networks show **Router
   managed** IPv6 policy unless you previously chose another policy in a test
   build. Do not select Prefix delegation or Disabled merely as a verification
   step; either choice becomes a router change only after Preview and Apply.
7. If router-clock status matters, review and apply the optional
   controller-access refresh for each previously adopted router, then confirm
   the clock warning clears or reports the actual offset. This access refresh
   does not change router time or NTP settings.
8. For a wired multi-hop layout, confirm the live topology does not show a
   managed downstream device attached directly to multiple upstream devices.
9. Open **Settings → Backup & Restore** and confirm no restore-based router-write suppression is active after an ordinary upgrade.
10. Run Preview before the next Apply; do not assume desired and observed state still match after downtime.

## Roll back v0.1.4 to v0.1.3

Do not point v0.1.3 at a database that v0.1.4 migrated to schema 20. Rollback is
a matched data restore:

1. Stop v0.1.4 cleanly.
2. Retain the schema-20 database/keyring pair separately for diagnosis or a
   later return to v0.1.4.
3. Restore the verified pre-upgrade schema-19 database and matching
   `keyring.json`.
4. Restore/use the passphrase that belongs to that pair.
5. Install the v0.1.3 binary or set the exact v0.1.3 image tag, then start it.
6. Verify accounts, devices, event history, and polling before making changes.

This rollback removes v0.1.4 IPv6 policy, clock observation, topology fixes,
Docker bind option, and security update. It does not undo router settings that
were deliberately applied while v0.1.4 was running; review those separately.

If the portable `.oowrtbak` is your only schema-19 recovery point, do not aim
v0.1.3 at the schema-20 volume. Start v0.1.3 against a new empty data directory
or volume, create its temporary owner, and use **Settings → Backup & Restore**
to restore the pre-upgrade artifact with its export passphrase. Confirmation
also uses that clean instance's runtime passphrase. Verify the restored state
before discarding either the old schema-20 data or the recovery artifact.

The v0.1.1 startup pruning of older speed-test rows cannot be reversed unless
those rows exist in a pre-v0.1.1 backup.

## Roll back to v0.1.0-rc.1

Do not point the RC daemon at a schema-19 or schema-20 database. Rollback is a data restore:

1. Stop the stable controller.
2. Retain its current schema-20 database/keyring pair separately.
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
