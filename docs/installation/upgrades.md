# Upgrade and roll back

oonfeeWRT keeps controller state in SQLite plus a separate keyring. Upgrade safety depends on preserving a matching database/keyring/passphrase set before replacing a binary or image.

> **Outcome:** The controller runs v0.1.7 with its existing state intact, and you retain a verified recovery unit matching the version you may need to restore.

::: info Upgrading from v0.1.6
v0.1.7 retains **schema 25** and adds no database migration over v0.1.6.
The update refines the interface and navigation; it does not require
re-adoption, a router helper, or new router permissions. Preserve a verified
matching recovery unit before replacing any binary or image.
:::

::: warning Upgrading from v0.1.5 or earlier
v0.1.5 uses schema **23**. v0.1.7 applies the migrations introduced in v0.1.6:
schema **23 → 24 → 25**: persistent alert state, then encrypted AdGuard
connection configuration. Starting v0.1.7 against your existing data performs
the migrations; building or previewing the isolated demo does not.

Before v0.1.7 opens your data, preserve a verified, matching
**schema-23 database + keyring + runtime passphrase + v0.1.5 binary/image**.
Use a disposable data copy if evaluating first. A v0.1.5 binary cannot open
schema 24 or 25. Changing only the executable or image tag is not a rollback.
:::

## Upgrade v0.1.6 to v0.1.7 {#upgrade-v016-to-v017}

1. Complete any in-flight operation, export and verify a backup, and retain
   the matching database, keyring, runtime passphrase, and v0.1.6 binary/image.
2. Stop the old controller cleanly. Replace it with the checksum-verified
   v0.1.7 binary or pinned container, keeping the same data volume and passphrase.
3. Start the controller and sign in again. Confirm `v0.1.7`, healthy service
   status, expected devices, and the existing router-write gate state.
4. Check the new navigation: **Workspace** contains operational screens;
   **Insights** contains Statistics, Reports, and Alerts; **Settings** contains
   Firmware and Integrations alongside Network and permitted maintenance tabs.
   Existing `/firmware` and `/integrations` bookmarks still work.
5. Verify the intended Reports period has finished loading before exporting.
   Missing measurements remain unavailable; the compact Dashboard summary
   does not change the meaning of its fleet counts.

The schema number is unchanged, but it is not a substitute for a recovery plan.
For a controlled return to the earlier deployment, stop the new process,
retain its current state separately, and restore the verified matching
pre-upgrade recovery unit with the original binary/image. Never run two
controllers against the same SQLite files. Restoring an older recovery point
discards controller changes made since that point and does not undo router changes.

<span id="evaluate-development-without-losing-a-stable-rollback"></span>

## Upgrade v0.1.5 to v0.1.7 {#upgrade-v015-to-v016}

This direct upgrade uses the same schema-23 → 25 migration path introduced in
v0.1.6; installing that intermediate version first is not required. The older
section anchor is retained so existing recovery-guide bookmarks keep working.

1. Export and verify a portable backup from v0.1.5. Keep its separate export
   passphrase outside the backup itself.
2. Also take the consistent raw database/keyring recovery unit described below,
   while still on schema 23, and verify it with the **v0.1.5** recovery helper.
   Retain the matching runtime passphrase and exact released binary/image.
3. Stop the old daemon before copying or replacing a live data directory.
   Never let two controllers open the same SQLite files.
4. Start v0.1.7 only against the intended copy or upgrade target. Schema
   24 stores alert rules, evaluation state, incidents, and bounded delivery
   state; schema 25 stores encrypted AdGuard connection settings. Neither
   migration installs a router helper or flashes firmware.
5. Reauthenticate after restart. Verify devices, configuration, source
   coverage, role gates, and backup access before enabling any new alert
   delivery or service connection. An ordinary upgrade does not create alert
   rules or configure an external integration for you.
6. To return to v0.1.5, stop v0.1.7 and retain its schema-25 recovery unit
   separately. Restore the untouched matching schema-23 unit and start the
   v0.1.5 binary/image with its passphrase. Do not hand-edit the schema number.

Restoring older state loses controller edits made after the recovery point and
does not undo router configuration or notifications already sent. A portable
backup created by a schema-25 controller is not a way to feed
newer state into v0.1.5. If only a **pre-upgrade** v0.1.5 portable backup
remains, restore it through v0.1.5's staged restore in a fresh supported data
directory; do not point that daemon at the migrated volume.

When using the v0.1.7 portable-restore path, external alert delivery is
paused, its pending outbox is cancelled, and evaluation/hold continuity resets.
Rules, history, cooldowns, and encrypted destination settings are retained.
An Owner reviews the restored environment and explicitly re-enables future
delivery; old queued notifications are not replayed. This is separate from
the router-write suppression gate. An ordinary startup upgrade does not
perform this restore-specific reset.

For UI exploration without a database migration or router connection, use the
[isolated demo](../guide/demo.md). The installation steps below target v0.1.7;
historical rollback targets are documented separately at the end.

## Before you begin

- Read the release notes for the version you are installing.
- Know whether the current install is a standalone binary or container.
- Locate the data directory or volume and the runtime passphrase file.
- Schedule a clean stop; do not upgrade during Apply, backup, restore, diagnostics generation, RF scan, or optional-package work.
- Preserve enough downtime to verify the new process before resuming changes.

**Router write impact:** Replacing the binary/image and migrating the database do not themselves contact or configure routers. After startup, read-only polling resumes and, for managed devices only when the write gate is open, automatic 802.11k neighbour reconciliation may update runtime hostapd neighbour lists. Monitor-only devices are excluded. A restore, unlike an ordinary upgrade, activates a persistent router-write safety gate.

## Version facts for v0.1.7

- v0.1.7 keeps v0.1.6's controller database schema **25**. From v0.1.5/schema 23,
  startup adds schema 24's durable alert snapshot/outbox and schema 25's
  encrypted AdGuard connection. No rules, destinations, or helpers are enabled
  automatically. Earlier supported databases run the earlier steps first:
- Schema 20 → 21 adds `management_mode`; every existing device becomes
  **Managed**, preserving v0.1.4 behavior. No router is silently converted to
  Monitor only.
- Schema 21 → 22 adds named policy sets, exact-MAC membership, and stable
  firewall `source_set_id` references. Existing policy rules are preserved and
  no set is invented.
- Schema 22 → 23 adds source-relative `client_observations`, its MAC lookup,
  and the case-insensitive global-client MAC index used by bounded policy
  checks. It drops the legacy Gateway index before canonicalizing the
  compatibility role, then rebuilds the one-managed-Gateway uniqueness guard
  from `functions_json` plus `role`, preventing either representation from
  bypassing the site limit. It does not infer provenance from merged global
  client rows.
- v0.1.5 cannot open schema 25; v0.1.4 cannot open schema 23 or 25.
  Returning to either version requires its matching pre-upgrade database,
  keyring, runtime passphrase, and binary/image—not only an older executable.
- Startup migration does not contact or configure routers, install a package,
  change ownership, or create an Apply plan.
- Existing networks decode as **Router managed**, so startup does not change
  their IPv6 settings. Prefix delegation or Disable still requires Preview and
  Apply.
- Existing router access remains sufficient for normal polling. Router-clock
  status uses newly allowlisted read-only LuCI methods; already-adopted routers
  need a separately reviewed controller-access refresh only if that clock
  status is wanted. Re-adoption is not required.
- Historical v0.1.0-rc.1 uses schema 17. v0.1.7 can migrate supported schema-17
  state through schemas 18–25. Returning to the RC requires
  restoring the untouched schema-17 backup, not merely replacing the executable
  or image.

## 1. Create a verified pre-upgrade backup

Always use **Settings → Backup & Restore** to export an encrypted `.oowrtbak`,
download it before it expires, record its separate export passphrase, and
verify that the job completed. For the simplest direct rollback,
also retain one of the raw recovery units below before v0.1.7 opens
the live data. The public recovery helper verifies raw databases but does not
extract `.oowrtbak` files. Keep schema 23 for v0.1.5, schema 20 for v0.1.4,
or the exact schema supported by your earlier rollback target. Verify with
that source release's matching recovery helper before replacing it.

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
but restoring it to an older version uses that version's clean-instance path rather
than an in-place file extraction.

Never copy only the main SQLite file while WAL is active. It may omit committed state.

## 2A. Upgrade a standalone binary

1. Download, checksum-verify, and extract v0.1.7 using [Install the binary](binary.md).
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

Download the exact v0.1.7 Compose file beside the existing one, compare it,
and reapply only intentional local
changes. Do not replace `.env`, `passphrase`, or the named volume:

```sh
curl --fail --location \
  --output docker-compose.yml.v0.1.7 \
  https://raw.githubusercontent.com/aiden0rchad/oonfeeWRT/v0.1.7/deploy/docker-compose.yml
diff -u docker-compose.yml docker-compose.yml.v0.1.7
OONFEE_VERSION=v0.1.7 docker compose -f docker-compose.yml.v0.1.7 config --quiet
```

After reviewing the diff, replace `docker-compose.yml` with the v0.1.7 file or
merge its changes deliberately. Update the existing `.env` without removing
other intentional deployment values. Pin both the release and the publish
address you intend to retain across future lifecycle commands:

```dotenv
OONFEE_VERSION=v0.1.7
OONFEE_HTTP_BIND=127.0.0.1
```

Use the controller's specific trusted management-LAN IP instead of `127.0.0.1`
when remote browsers must connect directly. Then, from that directory:

```sh
docker compose config --quiet
docker compose pull
docker compose up -d
```

The service keeps the existing `oonfee-data` volume and passphrase bind mount.
Confirm that you did not add `-v` to any `down` command.

Loopback remains the default. If you do not persist `.env`, repeat both
`OONFEE_VERSION` and `OONFEE_HTTP_BIND` on every Compose lifecycle command.
Do not publish raw port 8080 to the Internet.

## 3. Verify the upgraded controller

```sh
CONTROLLER_URL=http://127.0.0.1:8080
curl --fail "$CONTROLLER_URL/healthz"
```

Set `CONTROLLER_URL` to `http://<controller-LAN-IP>:8080` instead when `.env`
publishes only on that trusted address.

For Compose:

```sh
docker compose ps
docker compose logs --tail=200 oonfeewrt
docker compose exec oonfeewrt /oonfeewrtd -version
```

In the browser:

1. Sign in again; sessions are process-local and do not survive restart.
2. Confirm the expected devices, site settings, accounts, and event history.
3. Confirm devices resume read-only polling and retain their chosen management
   mode. Devices predating v0.1.5 become **Managed** during schema 21 migration.
   Do not change one to Monitor only merely as an upgrade
   check. When ownership requires a different mode, use the reviewed
   un-adopt/re-adopt workflow and inspect the replacement ACL plan.
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
9. Open **Policy Engine → Object Manager** and confirm existing named client sets
   are preserved. Sets are initially empty only when upgrading from before
   their schema 22 introduction. Creating a
   test set changes controller desired state; do not do it as a health check.
10. Generate Preview and verify Monitor-only devices are absent from the target
    fleet and set-backed rules show their exact resolved MAC members. Schema 23
    first introduced source-relative provenance with a successful managed-Gateway
    poll. Upgrades from before schema 23, or portable restores, can temporarily fail active MAC
    intent closed until every referenced MAC has a stored local Gateway
    observation. Monitor-only observations neither satisfy nor contaminate that
    proof. Clear existing block/fixed-address intent per client if needed, or use
    network/zone or explicit IPv4 scope instead.
11. Open **Settings → Backup & Restore** and confirm the pre-upgrade router-write
    gate state is preserved. Existing restore-based suppression must remain
    active until its separate recovery review is complete. Do not resume
    router writes merely to validate an upgrade.
12. Run Preview before the next Apply; do not assume desired and observed state still match after downtime.
13. Review **Statistics**, **Reports**, and the separate **Accounts** workspace.
    Missing historical buckets must remain gaps, not zero activity.
14. Review **Alerts**, **Firmware**, and **Integrations** without enabling a
    webhook, checking an external service, or installing the optional helper
    just to validate the upgrade. Fresh v0.1.5 upgrades have no alert rules or
    AdGuard connection; firmware installation remains unavailable in v0.1.7.

## Roll back v0.1.5 to v0.1.4

The following explains that earlier release transition. When rolling back
directly from v0.1.7, retain its schema-25 recovery unit and use the same exact
pre-v0.1.5 schema-20 target below; schema 23 is not a compatible substitute.

Do not point v0.1.4 at a database that v0.1.5 migrated to schema 23. Rollback is
a matched data restore:

1. Stop v0.1.5 cleanly.
2. Retain the schema-23 database/keyring pair separately for diagnosis or a
   later return to v0.1.5.
3. Restore the verified pre-upgrade schema-20 database and matching
   `keyring.json`.
4. Restore/use the passphrase that belongs to that pair.
5. Install the v0.1.4 binary or set the exact v0.1.4 image tag, then start it.
6. Verify accounts, devices, event history, and polling before making changes.

This rollback removes monitor-only mode, reusable policy sets, and the v0.1.5
UI changes. The restored schema-20 state predates any v0.1.5 set/membership
edits and treats its devices under the v0.1.4 one-managed-Gateway model. It does
not undo router settings deliberately applied while v0.1.5 was running; review
those separately.

If the portable `.oowrtbak` is your only schema-20 recovery point, do not aim
v0.1.4 at the schema-23 volume. Start v0.1.4 against a new empty data directory
or volume, create its temporary owner, and use **Settings → Backup & Restore**
to restore the pre-upgrade artifact with its export passphrase. Confirmation
also uses that clean instance's runtime passphrase. Verify the restored state
before discarding either the old schema-23 data or the recovery artifact.

## Older rollback targets

The v0.1.3 daemon uses schema 19 and cannot open schema 20, 23, or 25. Reaching
v0.1.3 requires its own matching pre-v0.1.4 schema-19 database, keyring, and
passphrase; do not use the v0.1.4 recovery point described above. A portable
schema-19 backup must be restored through clean v0.1.3 state.

The v0.1.1 startup pruning of older speed-test rows cannot be reversed unless
those rows exist in a pre-v0.1.1 backup.

## Roll back to v0.1.0-rc.1

Do not point the RC daemon at a schema-19, schema-20, schema-23, or schema-25 database. Rollback is a data restore:

1. Stop the stable controller.
2. Retain its current database/keyring pair separately (schema 25 for v0.1.7).
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
