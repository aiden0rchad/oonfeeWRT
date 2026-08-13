# tools/probe.py

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
python3 probe.py 192.168.1.1 --user root --ask-password
```

Then the important one. This writes, but only to a scratch config no service
reads:

```sh
python3 probe.py 192.168.1.1 --user root --ask-password --write-tests --json report.json
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
