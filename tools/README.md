# Tools

## Controller database tools

`dryrun`, `optdiff`, `stalecheck`, `livecheck`, `recoverycheck`, and `applyone`
open controller state through the same schema-14 cryptographic boundary as the
daemon. The current source schema is **17**: 14 remains the secret-sealing
epoch, 15 is the cross-feature policy semantic boundary, 16 is the attested
observability shape, and 17 adds the optional-capability rollback ledger. Set
`OONFEE_PASSPHRASE_FILE` to an absolute path naming the controller's mode-0600
passphrase file. The tools open `keyring.json` next to the database and refuse a
missing, wrong, or mismatched keyring.

```sh
export OONFEE_PASSPHRASE_FILE=/absolute/path/to/mode-600-passphrase

go run ./tools/dryrun /absolute/path/to/oonfeewrt.db
go run ./tools/optdiff /absolute/path/to/oonfeewrt.db
go run ./tools/stalecheck /absolute/path/to/oonfeewrt.db
go run ./tools/livecheck /absolute/path/to/oonfeewrt.db
go run ./tools/recoverycheck /absolute/path/to/recovery/oonfeewrt.db
go run ./tools/applyone /absolute/path/to/oonfeewrt.db DEVICE_HOST
```

The first five open SQLite with `mode=ro` plus `query_only`. They require schema
17 and `secret_state.scrub_complete=1`; they never migrate, finish a scrub or
repair a colliding/partial observability table. Start the controller writable
first when upgrading an older database.
The first four may read the routers named by the store, but do not stage or
apply router changes. `recoverycheck` makes no network calls: it requires an
exact sibling `keyring.json`, opens and validates every sealed record, and
prints counts only. Run it on an isolated recovery copy: it refuses sibling
SQLite `-wal` or `-journal` files that contain state rather than blessing a
snapshot whose self-contained database state is uncertain. A transient
`-shm` file and empty sidecars carry no recoverable database pages and are not
treated as backup members.

`applyone` is different: it opens the controller store writable, may migrate an
older database, and applies to the one explicitly named router. Before using it,
take a consistent SQLite `.backup` (or stop/checkpoint cleanly) and copy the
matching `keyring.json`. Do not discover a schema migration during a router
apply.

The lab has already been promoted through the attested schema 16 boundary to
schema 17 and validated there; that promotion is no longer pending. For any
other older store, start the daemon writable and complete/validate migration
before using a write-capable tool. Source tests alone are not evidence that a
particular live store has been promoted.

Database and keyring are one restore unit. A passphrase cannot recreate the
keyring's random data key. A database copied alone from a live WAL store may be
stale, and a pre-v14 database backup may contain plaintext WLAN/mesh keys and
secret-derived ownership hashes. Migration neither rewrites nor deletes old
backups; protect them and require explicit operator confirmation before
deletion.

## `probe.py`

Validates every `[verify]` assumption in the oonfeeWRT design against a real
OpenWrt device. **Run this before writing product code.**

Python 3.8+, standard library only. No `pip install`, no `jq`. Runs on macOS
as shipped.

---

## Prepare the device

The probe needs `/ubus` reachable. On a stock install:

```sh
opkg update
opkg install rpcd rpcd-mod-file rpcd-mod-iwinfo rpcd-mod-luci uhttpd-mod-ubus
uci set uhttpd.main.ubus_prefix=/ubus
uci commit uhttpd
/etc/init.d/uhttpd restart
/etc/init.d/rpcd restart
```

(On OpenWrt 25.x substitute `apk add` for `opkg install`.)

Note what you installed — the probe reports which packages are present, and part
of what you're measuring is how much of this a *stock* device already has. On a
stock 25.12.5 image all of the above was already present and `ubus_prefix` was
already set, so try the read-only run before changing anything.

**The ACL must go on first, and `--write-tests` cannot pass without it.** A stock
device grants no access to `uci.configs` or `iwinfo.devices`, and no access to a
config named `oonfeewrt_probe`, *regardless of which user you authenticate as* —
rpcd's `list read '*'` only expands over the access-groups defined on disk. See
IMPLEMENTATION §10. So:

```sh
scp -O deploy/acl/oonfeewrt.json root@DEVICE:/usr/share/rpcd/acl.d/
ssh root@DEVICE 'touch /etc/config/oonfeewrt_probe /etc/config/oonfeewrt_probe2'
ssh root@DEVICE '/etc/init.d/rpcd restart'
```

The `touch` is required because `uci.add` returns `NOT_FOUND` for a config file
that does not exist — it will not create one. (OpenWrt has no sftp-server, so
plain `scp` fails; use `scp -O`, or pipe through `ssh 'cat > path'`.)

The `oonfeewrt-probe` access group in that file exists only for this tool. Do
not ship it to production devices.

## Run it

Read-only first. This touches nothing:

```sh
python3 probe.py 192.0.2.1 --user root --ask-password
```

Then the important one. This writes, but only to a scratch config no service
reads:

```sh
python3 probe.py 192.0.2.1 --user root --ask-password --write-tests --json report.json
```

Useful flags:

| Flag | Effect |
|---|---|
| `--https` | use HTTPS (certificate not verified — measuring handshake cost, not trusting the device) |
| `--write-tests` | run the apply/confirm/rollback tests |
| `--poll-seconds N` | length of the CPU-cost window (`0` to skip) |
| `--json PATH` | dump raw findings for diffing across devices |

## What it checks

1. Device identity, and which **class** it is per `DEVICE-BUDGET.md`
2. Full ubus object/method surface, vs. what the architecture assumes
3. Whether **JSON-RPC batching** works
4. **Transport cost** — keep-alive vs. fresh connection, which is the dominant
   cost on weak CPUs
5. Device CPU during a simulated focused poll
6. Which binaries `file.exec` can reach
7. Installed packages and free `/overlay` space
8. Radio data: station fields, survey availability, whether interference and
   airtime are computable
9. DSA switch, firewall4, and **flow-offload settings** (the accounting tradeoff)
10. UCI read path, and how much foreign config already exists
11. **apply / confirm / rollback** — including a deliberate no-confirm test that
    proves the device reverts itself

It ends with a **VERDICT** section translating findings into design decisions.
That's the actual deliverable; the rest is evidence.

## Safety

Read-only unless you pass `--write-tests`. Those tests:

- touch only a scratch config named `oonfeewrt_probe`, which no service reads,
  so applying it cannot affect networking
- never touch `network`, `wireless`, `firewall`, `dhcp`, `system`, or `dropbear`
- clean up after themselves, including on failure
- deliberately let one rollback timer expire, to prove the revert actually happens

Still — run it first on a device you can physically reach. That advice applies to
any tool that writes to a router, including the one you're about to build.

## Then what

Fold anything surprising back into `docs/ARCHITECTURE.md` **before** writing
product code. Findings that would change the design:

Read the report's three-state marks carefully: `PASS` / `FAIL` / `n/a`. An `n/a`
reading **NOT OBSERVABLE** means the ACL blocked the check, not that the device
lacks the capability — never cut a feature on that evidence. (An earlier version
of this tool conflated the two and reported "no DSA" and "legacy iptables" for a
device that has both.)

**The same trap applies to ad-hoc scripts you write alongside this one**, and it
is easy to walk into. A ubus helper that returns `None` on failure, consumed as
`(call(...) or {}).get("results", [])`, turns *one failed call* into *"zero
stations"* — indistinguishable from an empty radio. That pattern produced a
confident, wrong finding during hardware validation (a 131-second "divergence"
between `iwinfo.assoclist` and `hostapd.get_clients` that did not exist; 57
status-checked samples later showed 100 % agreement). Any measurement script
here should return the call status alongside the data and print it, so a failure
can never masquerade as a measurement.

| Finding | Consequence |
|---|---|
| Rollback doesn't revert | Redesign the safety mechanism. Blocking. |
| No batching | Longer poll intervals; revisit the budget |
| Handshake > 120 ms | Persistent connections mandatory; consider ECDSA or HTTP on a management VLAN |
| Hardware offload on | Per-client accounting off by default; surface the tradeoff |
| No survey data | Radios screen loses interference and airtime |
| No DSA | Hide the Ports screen for that device |

Run it against each hardware class you intend to support and diff the JSON. The
differences between them are your capability model, discovered rather than
guessed.
