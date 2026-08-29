---
title: CLI and environment reference
description: Exact oonfeewrtd and recovery helper flags, environment variables, defaults, and safe command examples for v0.1.2.
---

# CLI and environment reference

This reference applies to the **v0.1.2** release executables
`oonfeewrtd` and `oonfeewrt-recoverycheck`.

## `oonfeewrtd`

```text
oonfeewrtd [flags]
```

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-data-dir <absolute-path>` | `/data`, or `OONFEE_DATA_DIR` | Directory containing `oonfeewrt.db`, `keyring.json`, logs, and private job/recovery directories |
| `-listen <host:port>` | `:8080`, or `OONFEE_LISTEN` | HTTP bind address |
| `-passphrase-file <path>` | unset, or `OONFEE_PASSPHRASE_FILE` | Read runtime passphrase from a protected regular file instead of prompting |
| `-log-level <level>` | `info` | One of `debug`, `info`, `warn`, `error` |
| `-healthcheck` | false | Probe the configured listener's `/healthz` and exit without opening controller data |
| `-version` | false | Print the embedded version and exit without opening controller data |
| `-h`, `-help` | — | Print standard flag help; v0.1.2 then exits non-zero (a known CLI quirk) |

Flags are parsed after environment configuration, so an explicit flag overrides
a valid corresponding environment value. Environment loading/validation happens
first; an invalid non-empty environment setting fails before flags are parsed.

### Environment variables

| Variable | Accepted value | Notes |
|---|---|---|
| `OONFEE_DATA_DIR` | Absolute directory path | Default `/data` |
| `OONFEE_LISTEN` | Go-style `host:port` | Default `:8080` |
| `OONFEE_PASSPHRASE_FILE` | Path to protected regular file | Used for unattended startup and recovery tooling |
| `OONFEE_PASSPHRASE` | **Rejected** | A passphrase value must never be supplied through the environment |

`OONFEE_PASSPHRASE` fails startup because environment values can be exposed
through process inspection, child processes, crash reports, and
`docker inspect`.

### Data-directory rules

`-data-dir` must be absolute. A relative path is rejected so a working-directory
change cannot silently create a second empty controller.

The process secures the data directory to mode `0700` and newly created SQLite
database/WAL/SHM files to `0600`. Run it as the operating-system user that owns
the state. The container defaults to UID `65532`; bind-mounted deployments may
instead explicitly run as the owning host UID/GID.

### Passphrase-file rules

The file must:

- be a regular file, not a directory or device;
- not be group- or world-readable;
- be non-empty; and
- be readable by the controller process.

One trailing LF or CRLF sequence is removed; other whitespace is retained. Mode
`0600` is the documented choice.

The first interactive start, with no passphrase file, prompts twice to create
the keyring. Later interactive starts prompt once. A non-interactive process
without `-passphrase-file` has no passphrase source and fails instead of
starting with an empty keyring.

## Common daemon commands

### Print the version

```sh
oonfeewrtd -version
```

For release v0.1.2 the output must be:

```text
v0.1.2
```

### Interactive local start

```sh
install -d -m 0700 "$PWD/data"
oonfeewrtd \
  -data-dir "$PWD/data" \
  -listen 127.0.0.1:8080
```

Open `http://127.0.0.1:8080` after the controller reports that it is listening.

### Unattended local start

```sh
install -d -m 0700 "$HOME/.local/share/oonfeewrt" "$HOME/.config/oonfeewrt"
umask 077
head -c 32 /dev/urandom | base64 > "$HOME/.config/oonfeewrt/passphrase"
chmod 600 "$HOME/.config/oonfeewrt/passphrase"

oonfeewrtd \
  -data-dir "$HOME/.local/share/oonfeewrt" \
  -passphrase-file "$HOME/.config/oonfeewrt/passphrase" \
  -listen 127.0.0.1:8080
```

The generated passphrase file is now the unattended unlock secret, not an
independent second factor. Back it up and protect it accordingly.

### Set environment configuration

```sh
export OONFEE_DATA_DIR=/absolute/path/to/oonfeewrt-data
export OONFEE_LISTEN=127.0.0.1:8080
export OONFEE_PASSPHRASE_FILE=/absolute/path/to/mode-600-passphrase
oonfeewrtd
```

Do not export the passphrase value itself.

### Change log level

```sh
oonfeewrtd \
  -data-dir /absolute/path/to/oonfeewrt-data \
  -passphrase-file /absolute/path/to/mode-600-passphrase \
  -listen 127.0.0.1:8080 \
  -log-level debug
```

Debug logging can contain more operational detail. Reproduce the problem,
collect a diagnostics bundle if appropriate, then return to `info`.

## Healthcheck

```sh
oonfeewrtd -listen 127.0.0.1:8080 -healthcheck
```

The healthcheck:

- derives `http://<listener>/healthz` from the configured listener;
- converts wildcard `0.0.0.0` to `127.0.0.1` and `::` to `::1`;
- uses a four-second timeout;
- does not use ambient HTTP proxy variables;
- expects status `200` and body exactly `ok`; and
- does not open the database, keyring, data directory, or passphrase file.

It checks process HTTP liveness, not router reachability, database backup
quality, or full fleet health.

Container example:

```sh
docker exec oonfeewrt /oonfeewrtd -healthcheck
```

The release container uses this executable healthcheck internally.

## Signals and shutdown

`SIGINT` and `SIGTERM` start graceful shutdown. The controller stops accepting
work, drains bounded requests/background jobs, lets an Apply that reached its
critical stage resolve its confirm/rollback outcome, flushes complete metric
windows, checkpoints SQLite, and exits.

Do not use `SIGKILL` for routine upgrades. The supplied container uses a
150-second stop timeout to give Apply and storage work room to finish.

## `oonfeewrt-recoverycheck`

The release archive includes a read-only recovery helper:

```text
oonfeewrt-recoverycheck /path/to/recovery/oonfeewrt.db
```

It has no data/listen flags. Set `OONFEE_PASSPHRASE_FILE` to the controller
runtime passphrase file:

```sh
OONFEE_PASSPHRASE_FILE=/absolute/path/to/mode-600-passphrase \
  oonfeewrt-recoverycheck /absolute/path/to/backup/oonfeewrt.db
```

The helper requires:

- exactly one database path;
- a regular, non-symlink database file;
- a regular, non-symlink sibling `keyring.json`;
- no non-empty `oonfeewrt.db-wal` or `oonfeewrt.db-journal` sidecar;
- a current-schema database; and
- the passphrase that opens that exact keyring.

It opens SQLite through the read-only recovery boundary and makes no network
call. A successful result has this shape:

```text
schema=19 devices=<n> credentials=<n> owned_sections=<n> wlans=<n> meshes=<n>
```

Those counts prove that the pair can be opened and its recovery invariants are
readable. They do not prove that every router is reachable or currently matches
the stored desired state.

## Manual backup verification example

For a live binary installation, use SQLite's online backup API and copy the
matching keyring:

```sh
install -d -m 0700 /absolute/path/to/private-backup
sqlite3 /absolute/path/to/live/oonfeewrt.db \
  ".backup '/absolute/path/to/private-backup/oonfeewrt.db'"
cp -p /absolute/path/to/live/keyring.json \
  /absolute/path/to/private-backup/keyring.json

OONFEE_PASSPHRASE_FILE=/absolute/path/to/mode-600-passphrase \
  oonfeewrt-recoverycheck /absolute/path/to/private-backup/oonfeewrt.db
```

Do not copy only the live main SQLite file while WAL mode is active.

## Exit behavior

- Normal `-version`, successful `-healthcheck`, and clean daemon shutdown exit
  successfully.
- Configuration, startup, listener, passphrase, or healthcheck failures print
  an `oonfeewrtd:` error to standard error and exit non-zero.
- `oonfeewrt-recoverycheck` exits `2` for invocation/path errors and `1` when
  the selected recovery pair fails validation.

See [Requirements](./requirements.md), [Data and retention](../concepts/data-retention.md),
and [Troubleshooting](./troubleshooting.md).
