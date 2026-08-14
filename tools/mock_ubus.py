#!/usr/bin/env python3
"""
mock_ubus.py — a faithful-enough ubus-over-HTTP simulator of a WRT3200ACM.

This is the contract fixture for oonfeeWRT development: probe.py, the Go ubus
client, and the apply-engine integration tests all run against it, so product
code can be built and CI'd with zero hardware.

    python3 mock_ubus.py [--port 8088]
    # login password: "good"

What it models faithfully (because the design depends on it):

  * UCI staged-vs-committed semantics. `uci.set/add/delete` STAGE a delta.
    `uci.commit` applies one config's delta. `uci.apply {rollback:true}`
    commits ALL staged deltas, snapshots the pre-apply committed state, and
    arms a rollback timer; `uci.confirm` cancels it; timer expiry RESTORES the
    snapshot. Committing manually before apply therefore disarms rollback —
    exactly like real rpcd, and exactly the bug class the probe exists to catch.
  * JSON-RPC batching (array bodies).
  * WRT3200ACM identity: mvebu/cortexa9, 512 MB RAM, DSA switch, dual radios.
  * Per-session state, as measured on hardware: each login gets its own token,
    staged UCI deltas are scoped to it, and `uci.confirm` is refused to any
    session other than the one that applied — without cancelling the timer.
    After a rollback the applying session still reads the value it failed to
    set, while a fresh session reads the reverted one.
  * mwlwifi's real survey quirks: `iwinfo.survey` works, but reports `noise`
    unsigned and leaves rx_time/tx_time uninitialised, so airtime is
    computable and interference is not.

What it does not model: timing realism, wireless reload behavior, hostapd
events, or multiple devices (run several instances on different ports for a
fleet).
"""

import argparse
import copy
import hashlib
import http.server
import json
import secrets
import socketserver
import threading
import time

PASSWORD = "good"

# --------------------------------------------------------------------------
# SHA-512 crypt ($6$), so logins are verified the way rpcd verifies them.
#
# rpcd treats a non-"$p$" password field as a crypt hash and compares
# crypt(input, stored) == stored. Measured on hardware: a PLAINTEXT value there
# never matches — the correct and an incorrect password are both rejected — so
# this fixture must reject it too, or adoption would appear to work with a
# password format the device silently refuses.
# --------------------------------------------------------------------------

_B64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
_PERM = [(0, 21, 42), (22, 43, 1), (44, 2, 23), (3, 24, 45), (25, 46, 4),
         (47, 5, 26), (6, 27, 48), (28, 49, 7), (50, 8, 29), (9, 30, 51),
         (31, 52, 10), (53, 11, 32), (12, 33, 54), (34, 55, 13), (56, 14, 35),
         (15, 36, 57), (37, 58, 16), (59, 17, 38), (18, 39, 60), (40, 61, 19),
         (62, 20, 41)]


def _b64_24(b2, b1, b0, n):
    w = (b2 << 16) | (b1 << 8) | b0
    out = ""
    for _ in range(n):
        out += _B64[w & 0x3F]
        w >>= 6
    return out


def sha512_crypt(password, salt, rounds=5000):
    pw, sl = password.encode(), salt.encode()[:16]
    b = hashlib.sha512(pw + sl + pw).digest()
    a = hashlib.sha512()
    a.update(pw + sl)
    i = len(pw)
    while i > 0:
        a.update(b if i > 64 else b[:i])
        i -= 64
    i = len(pw)
    while i > 0:
        a.update(b if i % 2 else pw)
        i >>= 1
    a = a.digest()
    dp = hashlib.sha512(pw * len(pw)).digest()
    p = (dp * (len(pw) // 64 + 1))[:len(pw)]
    ds = hashlib.sha512(sl * (16 + a[0])).digest()
    sq = (ds * (len(sl) // 64 + 1))[:len(sl)]
    prev = a
    for i in range(rounds):
        h = hashlib.sha512()
        h.update(p if i % 2 else prev)
        if i % 3:
            h.update(sq)
        if i % 7:
            h.update(p)
        h.update(prev if i % 2 else p)
        prev = h.digest()
    out = "".join(_b64_24(prev[x], prev[y], prev[z], 4) for x, y, z in _PERM)
    out += _b64_24(0, 0, prev[63], 2)
    prefix = "$6$" + (f"rounds={rounds}$" if rounds != 5000 else "")
    return prefix + salt[:16] + "$" + out


def check_login(username, password):
    """Verify against the rpcd config exactly as rpcd would."""
    for sec in committed.get("rpcd", {}).values():
        if sec.get(".type") != "login" and "username" not in sec:
            continue
        if sec.get("username") != username:
            continue
        stored = sec.get("password", "")
        if stored.startswith("$p$"):
            # "look the user up in /etc/shadow" — stand in with the fixture
            # password so existing tests keep working.
            return password == PASSWORD
        if stored.startswith("$6$"):
            rest = stored[3:]
            rounds = 5000
            if rest.startswith("rounds="):
                end = rest.index("$")
                rounds = int(rest[len("rounds="):end])
                rest = rest[end + 1:]
            salt = rest.split("$", 1)[0]
            return sha512_crypt(password, salt, rounds) == stored
        return False  # plaintext never matches, as measured
    return False

# Every login gets its own token. A single shared constant would make the two
# session-scoped behaviours real rpcd has — confirm is bound to the applying
# session, and staged deltas are keyed by token — impossible to reproduce, so
# an apply engine that reconnects before confirming would pass CI and then
# silently revert on hardware.
sessions = set()


def new_session():
    sid = secrets.token_hex(16)
    sessions.add(sid)
    return sid

# --------------------------------------------------------------------------
# UCI state: committed[config][section] = {".type":…, opts…}; staged deltas
# --------------------------------------------------------------------------

committed = {
    "network": {
        "lan": {".type": "interface", "proto": "static",
                "ipaddr": "192.168.1.1", "netmask": "255.255.255.0",
                "device": "br-lan"},
        "wan": {".type": "interface", "proto": "dhcp", "device": "wan"},
    },
    # Present but empty, standing in for the `touch /etc/config/…` the probe
    # requires on a real device: uci.add returns NOT_FOUND without it.
    "oonfeewrt_probe": {},
    "oonfeewrt_probe2": {},
    "wireless": {
        "radio0": {".type": "wifi-device", "type": "mac80211", "band": "5g",
                   "channel": "36", "htmode": "VHT80"},
        "radio1": {".type": "wifi-device", "type": "mac80211", "band": "2g",
                   "channel": "6", "htmode": "HT20"},
        "default_radio0": {".type": "wifi-iface", "device": "radio0",
                           "mode": "ap", "ssid": "OpenWrt", "network": "lan",
                           "encryption": "psk2", "key": "hunter22"},
    },
    "firewall": {
        "defaults": {".type": "defaults", "input": "REJECT",
                     "output": "ACCEPT", "forward": "REJECT",
                     "synflood_protect": "1"},
        # note: no flow_offloading options — Armada 385 doesn't need them
    },
    "dhcp": {
        "lan": {".type": "dhcp", "interface": "lan", "start": "100",
                "limit": "150", "leasetime": "12h"},
    },
    "system": {
        "@system[0]": {".type": "system", "hostname": "wrt3200acm",
                       "timezone": "UTC"},
    },
    "uhttpd": {
        "main": {".type": "uhttpd", "listen_http": "0.0.0.0:80",
                 "ubus_prefix": "/ubus"},
    },
    # Present on every real device, and adoption writes its login here. The
    # root entry uses `$p$root`, meaning "look this user up in /etc/shadow";
    # anything else in that field is treated as a crypt hash, and a plaintext
    # value simply never matches (measured: rpcd rejects both the correct and
    # an incorrect password).
    "rpcd": {
        "@rpcd[0]": {".type": "rpcd", "socket": "/var/run/ubus/ubus.sock",
                     "timeout": "30"},
        "@login[0]": {".type": "login", "username": "root",
                      "password": "$p$root", "read": "*", "write": "*"},
    },
}
staged = {}          # session -> config -> [ (op, section, payload) ]
dirty_configs = set()  # configs with foreign (LuCI/SSH) uncommitted edits
written_files = {}     # path -> bytes, so adoption's footprint is assertable
reject_logins = set()  # test-only fault injection: usernames to refuse
acl_gaps = set()       # (object, method) pairs rpcd refuses to proxy at all
rollback = {}        # {"snapshot", "staged_snapshot", "owner", "deadline"}
survey_calls = 0     # drives the reproduced mwlwifi survey-noise instability
info_calls = 0       # ditto for iwinfo.info, which is just as unstable
lock = threading.RLock()


def stage(sid, config, op, section, payload):
    staged.setdefault(sid, {}).setdefault(config, []).append(
        (op, section, payload))


def effective(sid, config):
    """committed state with this session's staged delta laid over it.

    rpcd scopes staged deltas to the session token, and uci.get reads through
    them. Reading `committed` alone would mean a session never sees its own
    uncommitted writes — and, after a rollback, would hide the fact that the
    applying session still reads the value it failed to set.
    """
    cfg = copy.deepcopy(committed.get(config)) if config in committed else None
    for op, section, payload in staged.get(sid, {}).get(config, []):
        if cfg is None:
            cfg = {}
        if op == "add":
            sec = {".type": payload["type"]}
            sec.update(payload.get("values", {}))
            cfg[section] = sec
        elif op == "set":
            sec = cfg.setdefault(section, {".type": payload.get("type") or "unknown"})
            if payload.get("type"):
                sec[".type"] = payload["type"]
            sec.update(payload.get("values", {}))
        elif op == "delete":
            cfg.pop(section, None)
    return cfg


def commit_config(sid, config):
    """Apply one config's staged delta to committed state."""
    for op, section, payload in staged.get(sid, {}).pop(config, []):
        cfg = committed.setdefault(config, {})
        if op == "add":
            sec = {".type": payload["type"]}
            sec.update(payload.get("values", {}))
            cfg[section] = sec
        elif op == "set":
            sec = cfg.setdefault(section, {".type": payload.get("type") or "unknown"})
            if payload.get("type"):
                sec[".type"] = payload["type"]
            sec.update(payload.get("values", {}))
        elif op == "delete":
            cfg.pop(section, None)


def apply_all(sid, rb, timeout):
    """uci.apply: commit this session's staged deltas; optionally arm rollback.

    One transaction across every staged config — real rpcd commits and reverts
    them all together, so a per-config apply loop would model something the
    device cannot do.
    """
    if rb:
        rollback["snapshot"] = copy.deepcopy(committed)
        rollback["staged_snapshot"] = copy.deepcopy(staged.get(sid, {}))
        rollback["owner"] = sid
        rollback["deadline"] = time.time() + timeout
    for config in list(staged.get(sid, {}).keys()):
        commit_config(sid, config)


def confirm(sid):
    """Only the session that applied may confirm. Returns True on success.

    A wrong-session confirm must NOT cancel the timer: on hardware it is
    refused and the change still reverts, which is precisely the case a
    reconnect-then-confirm controller gets wrong.
    """
    if not rollback:
        return False
    if rollback.get("owner") != sid:
        return None          # denied, timer left running
    rollback.clear()
    return True


def rollback_watchdog():
    global committed
    while True:
        time.sleep(0.5)
        with lock:
            dl = rollback.get("deadline")
            if dl and time.time() > dl:
                committed = copy.deepcopy(rollback["snapshot"])
                # Restore the applier's delta too. rpcd reverts /etc/config
                # but the applying session's staged change comes back with it,
                # so that session keeps reading the value it failed to set
                # while a fresh session sees the reverted one.
                owner = rollback.get("owner")
                if owner is not None:
                    staged[owner] = copy.deepcopy(
                        rollback.get("staged_snapshot") or {})
                rollback.clear()


# --------------------------------------------------------------------------
# ubus objects
# --------------------------------------------------------------------------

OBJECTS = {
    "session": {"login": {}, "list": {}, "destroy": {}, "access": {}},
    "uci": {m: {} for m in ("configs", "get", "set", "add", "delete",
                            "changes", "revert", "commit", "apply",
                            "confirm", "rollback")},
    "system": {"board": {}, "info": {}, "reboot": {}},
    "file": {"read": {}, "write": {}, "exec": {}, "list": {}, "stat": {},
             "remove": {}},
    "iwinfo": {m: {} for m in ("devices", "info", "assoclist", "freqlist",
                               "txpowerlist", "scan", "countrylist",
                               "survey")},
    "network": {"reload": {}, "restart": {}},
    "network.interface": {"dump": {}},
    "network.device": {"status": {}},
    "network.wireless": {"status": {}},
    # hostapd is the cheap source the architecture now prefers over iwinfo for
    # per-AP status and client lists (1 ms vs ~30 ms measured on class A).
    "hostapd.wlan0": {m: {} for m in ("get_status", "get_clients",
                                      "get_features", "list_bans",
                                      "del_client")},
    "hostapd.wlan1": {m: {} for m in ("get_status", "get_clients",
                                      "get_features", "list_bans",
                                      "del_client")},
    "luci-rpc": {m: {} for m in ("getNetworkDevices", "getWirelessDevices",
                                 "getHostHints", "getDHCPLeases",
                                 "getBoardJSON")},
    "hostapd.wlan0": {"get_clients": {}, "bss_transition_request": {},
                      "del_client": {}},
    "hostapd.wlan1": {"get_clients": {}, "del_client": {}},
}

WHICH = {"iw": "/usr/sbin/iw", "iwinfo": "/usr/bin/iwinfo",
         "df": "/bin/df", "ip": "/sbin/ip", "nft": "/usr/sbin/nft",
         "opkg": "/bin/opkg", "ethtool": "/usr/sbin/ethtool",
         "bridge": "/usr/sbin/bridge"}

OPKG_INSTALLED = """rpcd - 2024.09.01
rpcd-mod-file - 2024.09.01
rpcd-mod-iwinfo - 2024.09.01
rpcd-mod-luci - 24.1
rpcd-mod-rpcsys - 2024.09.01
uhttpd - 2024.10.1
uhttpd-mod-ubus - 2024.10.1
libustream-mbedtls - 2024.1
px5g-mbedtls - 10
firewall4 - 2024.10.1
nftables - 1.0.9
umdns - 2024.3"""

# Shape captured from real associated stations on mwlwifi. The per-direction
# counters are NESTED — retries/failed/packets/bytes live inside rx/tx, not as
# flat tx_retries/rx_packets keys. Anything probing for the flat form concludes
# the data is missing and reaches for `iw station dump`, which is a process
# spawn the budget forbids on the fast loop.
ASSOC = [{"mac": "AA:BB:CC:11:22:33", "signal": -48, "signal_avg": -47,
          "noise": -95, "inactive": 100, "connected_time": 53, "thr": 129640,
          "authorized": True, "authenticated": True, "preamble": "short",
          "wme": True, "mfp": False, "tdls": False,
          "rx": {"packets": 1298, "bytes": 315537, "rate": 144400, "mcs": 15,
                 "mhz": 20, "ht": True, "vht": False, "he": False,
                 "eht": False, "short_gi": True, "40mhz": False,
                 "drop_misc": 0},
          "tx": {"packets": 1184, "bytes": 747875, "rate": 144400, "mcs": 15,
                 "mhz": 20, "ht": True, "vht": False, "he": False,
                 "eht": False, "short_gi": True, "40mhz": False,
                 "retries": 0, "failed": 0}},
         {"mac": "AA:BB:CC:44:55:66", "signal": -67, "signal_avg": -66,
          "noise": -95, "inactive": 120, "connected_time": 900, "thr": 58500,
          "authorized": True, "authenticated": True, "preamble": "short",
          "wme": True, "mfp": False, "tdls": False,
          "rx": {"packets": 4210, "bytes": 512000, "rate": 130000, "mcs": 7,
                 "mhz": 20, "ht": True, "short_gi": True, "drop_misc": 0},
          "tx": {"packets": 5522, "bytes": 980000, "rate": 195000, "mcs": 9,
                 "mhz": 40, "ht": True, "short_gi": True,
                 "retries": 214, "failed": 3}}]


def ok(rid, data=None):
    res = [0] if data is None else [0, data]
    return {"jsonrpc": "2.0", "id": rid, "result": res}


def err(rid, code):
    """A ubus status inside a successful JSON-RPC response.

    This is how a *proxied* call fails: the session was fine and the object
    handler refused the target. Status 6 here is permanent — re-authenticating
    changes nothing.
    """
    return {"jsonrpc": "2.0", "id": rid, "result": [code]}


def denied(rid):
    """A JSON-RPC error, which is how rpcd refuses to proxy a call at all.

    Real rpcd returns -32002 for BOTH an invalid/expired session and an
    object+method in no granted access-group. Returning status 6 for a dead
    session instead — as this mock used to — teaches a client never to
    re-login on expiry, because status 6 is the code it must NOT retry.
    """
    return {"jsonrpc": "2.0", "id": rid,
            "error": {"code": -32002, "message": "Access denied"}}


def exec_cmd(rid, cmd, params):
    def out(code, stdout):
        return ok(rid, {"code": code, "stdout": stdout, "stderr": ""})
    # rpcd resolves a command to its ABSOLUTE PATH before matching the ACL, so
    # callers legitimately pass either form. Compare on the basename.
    cmd = (cmd or "").rsplit("/", 1)[-1]
    if cmd == "nft":
        return out(0, '{"nftables":[{"metainfo":{"version":"1.1.6"}}]}')
    if cmd == "nlbw":
        return out(0, '{"columns":["mac","conns","rx_bytes","rx_pkts",'
                      '"tx_bytes","tx_pkts"],"data":['
                      '["aa:bb:cc:11:22:33",4,25488470,3412,304176,4173]]}')
    if cmd == "true":
        return out(0, "")
    if cmd == "which":
        p = WHICH.get(params[0] if params else "")
        return out(0 if p else 1, (p + "\n") if p else "")
    if cmd == "opkg":
        return out(0, OPKG_INSTALLED + "\n")
    if cmd == "df":
        return out(0, "Filesystem 1K-blocks Used Available Use% Mounted on\n"
                      "/dev/ubi0_1 219136 60416 158720 28% /overlay\n")
    if cmd == "iw":
        if params == ["dev"]:
            return out(0, "phy#0\n\tInterface wlan0\nphy#1\n\tInterface wlan1\n")
        if len(params) >= 3 and params[2] == "survey":
            # mwlwifi: survey exists but reports no busy time
            return out(0, "Survey data from wlan0\n"
                          "\tfrequency: 5180 MHz [in use]\n")
        if len(params) >= 3 and params[2] == "station":
            return out(0, "Station aa:bb:cc:11:22:33 (on wlan0)\n"
                          "\tsignal: -54 dBm\n\ttx retries: 3120\n"
                          "\ttx failed: 45\n\ttx packets: 120433\n")
        return out(0, "")
    if cmd == "sh":
        script = params[-1] if params else ""
        if "dsa" in script:
            return out(0, "/sys/class/net/lan1/dsa\n/sys/class/net/lan2/dsa\n")
        return out(0, "")
    if cmd == "ping":
        return out(0, "1 packets transmitted, 1 received, time 12ms\n")
    return out(127, "")


def handle_one(req):
    rid = req.get("id")
    method = req.get("method")
    p = req.get("params", [])

    if method == "list":
        if p and isinstance(p[0], str) and len(p[0]) == 32:
            return {"jsonrpc": "2.0", "id": rid, "result": OBJECTS}
        return {"jsonrpc": "2.0", "id": rid, "result": {}}
    if method != "call":
        return {"jsonrpc": "2.0", "id": rid,
                "error": {"code": -32601, "message": "unknown method"}}

    sess, obj, meth, args = (list(p) + [{}] * 4)[:4]
    args = args or {}

    if obj == "session" and meth == "login":
        if args.get("username") in reject_logins:
            return err(rid, 6)   # injected fault, see __test.reject_login
        if args.get("username") == "root" and args.get("password") == PASSWORD \
                or check_login(args.get("username"), args.get("password", "")):
            # While a rollback is armed, rpcd hands ANY new login the applying
            # session's token rather than minting a fresh one. Measured on
            # hardware, and it is deliberate: it is how a controller that lost
            # its connection can still confirm. The consequence is sharp — an
            # independent session is UNAVAILABLE inside the confirmation
            # window, so a health check cannot get one, and destroying "the
            # verification session" destroys the applying one and guarantees
            # the change reverts.
            owner = rollback.get("owner")
            if owner in sessions:
                return ok(rid, {"ubus_rpc_session": owner, "timeout": 300,
                                "expires": 300})
            return ok(rid, {"ubus_rpc_session": new_session(), "timeout": 300,
                            "expires": 300})
        return err(rid, 6)
    if sess not in sessions:
        return denied(rid)  # dead session -> JSON-RPC -32002, not status 6

    # An object+method in no granted access-group: rpcd refuses to PROXY, so
    # this is -32002 with a perfectly valid session. That is the permanent half
    # of the denial contract, and without it a client's "one re-login then give
    # up" policy is never exercised.
    if (obj, meth) in acl_gaps or (obj, "*") in acl_gaps:
        return denied(rid)

    if obj == "session" and meth == "destroy":
        # Really invalidate it. Returning OK while leaving the token usable
        # would hide the -32002 path entirely, which is the one a client must
        # recover from with exactly one re-login.
        sessions.discard(sess)
        staged.pop(sess, None)
        return ok(rid)
    if obj == "session" and meth == "access":
        return ok(rid, {"access": True})
    if obj == "session" and meth == "list":
        return ok(rid, {"ubus_rpc_session": sess, "timeout": 300, "expires": 300})

    if obj == "system" and meth == "board":
        return ok(rid, {
            "kernel": "6.6.52", "hostname": "wrt3200acm",
            "system": "ARMv7 Processor rev 1 (v7l)",
            "model": "Linksys WRT3200ACM", "board_name": "linksys,wrt3200acm",
            "rootfs_type": "squashfs",
            "release": {"distribution": "OpenWrt", "version": "25.12.0",
                        "revision": "r28000-abcdef",
                        "target": "mvebu/cortexa9",
                        "description": "OpenWrt 25.12.0"}})
    if obj == "system" and meth == "info":
        return ok(rid, {"uptime": 400000, "load": [8000, 9000, 8500],
                        "memory": {"total": 536870912, "free": 340000000,
                                   "buffered": 20000000, "cached": 60000000}})

    if obj == "file":
        if meth == "exec":
            return exec_cmd(rid, args.get("command"), args.get("params", []))
        if meth == "read":
            if args.get("path") == "/proc/stat":
                t = int(time.time() * 100)
                return ok(rid, {"data": f"cpu  {t % 100000} 0 {t % 50000} "
                                        f"{t % 900000} 0 0 0 0\n"})
            return err(rid, 4)
        if meth == "write":
            data = args.get("data") or ""
            if args.get("base64"):
                import base64 as _b64
                try:
                    data = _b64.b64decode(data).decode()
                except Exception:
                    pass
            written_files[args.get("path")] = data
            return ok(rid, {})
        if meth == "remove":
            # NOT_FOUND on an absent path: during un-adopt that means "already
            # gone", which is success, and the caller distinguishes them.
            if args.get("path") in written_files:
                del written_files[args.get("path")]
                return ok(rid, {})
            return err(rid, 4)
        if meth == "list" and args.get("path") == "/tmp/.uci":
            # The system savedir, where LuCI and the uci CLI stage their edits.
            # rpcd scopes ITS deltas to a per-session dir, so uci.changes cannot
            # see any of this — listing here is the only way PREFLIGHT can
            # detect that a human is mid-edit. Stale zero-length files linger
            # after a commit, so consumers must filter on size, and this fixture
            # ships some so a naive presence check fails in CI.
            entries = [{"name": n, "type": "file", "size": 0}
                       for n in ("firewall", "rpcd", "wireless")
                       if n not in dirty_configs]
            entries += [{"name": n, "type": "file", "size": 44}
                        for n in sorted(dirty_configs)]
            return ok(rid, {"entries": entries})
        if meth == "list":
            return err(rid, 4)
        return err(rid, 8)

    # Test-only hook: stand in for "a human is editing in LuCI right now".
    # There is no CLI inside the mock, so tests need a way to dirty the system
    # savedir. Real devices need no such hook.
    if obj == "__test" and meth == "set_acl_gap":
        acl_gaps.clear()
        for pair in args.get("pairs") or []:
            acl_gaps.add((pair.get("object"), pair.get("method", "*")))
        return ok(rid, {"gaps": sorted(f"{o}.{m}" for o, m in acl_gaps)})
    if obj == "__test" and meth == "reject_login":
        # Stands in for anything that leaves a freshly written login unusable —
        # a botched hash, a config that did not land. Adoption must notice.
        reject_logins.clear()
        reject_logins.update(args.get("usernames") or [])
        return ok(rid, {"rejecting": sorted(reject_logins)})
    if obj == "__test" and meth == "written":
        return ok(rid, {"paths": sorted(written_files),
                        "content": written_files.get(args.get("path"), "")})
    if obj == "__test" and meth == "set_dirty":
        dirty_configs.clear()
        dirty_configs.update(args.get("configs") or [])
        return ok(rid, {"dirty": sorted(dirty_configs)})

    if obj == "iwinfo":
        dev = args.get("device", "wlan0")
        if meth == "devices":
            return ok(rid, {"devices": ["wlan0", "wlan1"]})
        if meth == "info":
            g5 = dev == "wlan0"
            # The 2.4 GHz radio's noise floor is unstable here too. Measured
            # 2026-08-13 over 20 samples: iwinfo.info spread 42 dB and
            # iwinfo.survey 46 dB on the SAME radio, while the 5 GHz radio held
            # within 7 dB on both. So the instability belongs to the radio, not
            # to the method, and "read noise from iwinfo.info instead" fixes
            # only the unsigned encoding. Alternating (rather than reproducing
            # the real ~1-in-4 rate) keeps a two-sample check deterministic.
            global info_calls
            info_calls += 1
            noise = -92
            if not g5 and info_calls % 2 == 0:
                noise = -58
            return ok(rid, {"phy": "phy0" if g5 else "phy1",
                            "ssid": "OpenWrt", "mode": "Master",
                            "channel": 36 if g5 else 6,
                            "frequency": 5180 if g5 else 2437,
                            "txpower": 23, "quality": 60, "quality_max": 70,
                            "signal": -54, "noise": noise,
                            "country": "US", "hwmodes": ["ac", "n"],
                            "hardware": {"name": "Marvell 88W8964"}})
        if meth == "assoclist":
            return ok(rid, {"results": ASSOC})
        if meth == "freqlist":
            return ok(rid, {"results": [
                {"channel": 36, "mhz": 5180, "restricted": False},
                {"channel": 52, "mhz": 5260, "restricted": True}]})
        if meth == "txpowerlist":
            return ok(rid, {"results": [{"dbm": 23, "mw": 200, "active": True}]})
        if meth == "survey":
            # mwlwifi really does serve this natively. Three measured traps are
            # reproduced deliberately: `noise` comes back UNSIGNED here (161
            # for -95) while iwinfo.info reports it signed; rx_time/tx_time are
            # uninitialised garbage that also OVERFLOWS int64, so a consumer
            # decoding them as signed loses the whole object including the
            # usable fields; and the noise reading itself is unstable.
            #
            # The instability is measured, not invented: on 2026-08-13 the 2.4
            # GHz radio sat at -95 dBm and jumped to -70 dBm sporadically, a 25
            # dB spread over 12 samples, while the 5 GHz radio on the same
            # driver stayed within 2 dB. Channel busy time did not explain the
            # excursions. The fixture ALTERNATES on the 2.4 GHz radio rather
            # than reproducing the real ~1-in-4 rate, because the property under
            # test is "two consecutive reads disagree" and alternation makes
            # that deterministic no matter what else has called survey first —
            # a random or modulo-3 fixture would depend on the mock's shared
            # call counter and flake.
            global survey_calls
            survey_calls += 1
            g5 = dev == "wlan0"
            noise = 161
            if not g5 and survey_calls % 2 == 0:
                noise = 186  # -70 dBm
            # busy_time and active_time are monotonic COUNTERS in ms, and they
            # do NOT share an epoch. Measured 2026-08-13: the 5 GHz radio read
            # active=24427 against busy=922104 while both advanced correctly, so
            # the absolute ratio said 1354% where the delta ratio said 1.7%. On
            # 2.4 GHz the absolute ratio said 25.9% against a true 73.3% — the
            # dangerous case, because 25.9% looks entirely reasonable.
            #
            # Reproduced with a large busy offset so a consumer that divides the
            # absolutes gets an obviously impossible number in CI, and both
            # counters advancing so a consumer that divides the deltas gets a
            # steady 25%.
            active = 19849 + survey_calls * 1000
            busy = 900000 + survey_calls * 250
            return ok(rid, {"results": [{
                "mhz": 5180 if g5 else 2437,
                "noise": noise,
                "active_time": active, "busy_time": busy, "busy_time_ext": 0,
                "rx_time": 13869070124637487105, "tx_time": 0}]})
        return err(rid, 8)  # NOT_SUPPORTED

    if obj == "network.interface" and meth == "dump":
        return ok(rid, {"interface": [
            {"interface": "lan", "up": True, "proto": "static",
             "device": "br-lan",
             "ipv4-address": [{"address": "192.168.1.1", "mask": 24}]},
            {"interface": "wan", "up": True, "proto": "dhcp",
             "device": "wan",
             "ipv4-address": [{"address": "203.0.113.7", "mask": 24}]}]})
    if obj == "network.device" and meth == "status":
        return ok(rid, {"br-lan": {"up": True, "carrier": True, "mtu": 1500,
                                   "macaddr": "60:38:e0:aa:bb:cc",
                                   "statistics": {"rx_bytes": 123456789,
                                                  "tx_bytes": 987654321}}})
    if obj.startswith("hostapd."):
        g5 = obj.endswith("wlan0")
        if meth == "get_status":
            # `utilization` is the 802.11 BSS-Load 0-255 scale, NOT a percent —
            # 172 is ~67%. Anything rendering it as a percentage is wrong, so
            # the fixture reports it the way hardware does.
            return ok(rid, {"phy": "phy0" if g5 else "phy1",
                            "ssid": "OpenWrt", "bssid": "30:23:03:db:be:42",
                            "channel": 36 if g5 else 6,
                            "freq": 5180 if g5 else 2437,
                            "driver": "nl80211", "status": "ENABLED",
                            "airtime": {"time": 2132274, "time_busy": 1534433,
                                        "utilization": 172}})
        if meth == "get_clients":
            # Byte/packet counters agree exactly with iwinfo.assoclist (verified
            # per-MAC on hardware), so this is a trustworthy cheap source for
            # volume. But `rate` here is 100x iwinfo's kbit/s value, and
            # per-client `airtime` is zero on mwlwifi — both reproduced so a
            # consumer that mixes the two units, or plots airtime, fails in CI.
            clients = {}
            for st in ASSOC:
                clients[st["mac"].lower()] = {
                    "auth": True, "assoc": True, "authorized": True,
                    "preauth": False, "wds": False, "wmm": True,
                    "ht": st["rx"].get("ht", True), "vht": False, "he": False,
                    "wps": False, "mfp": False, "mbo": False,
                    "rrm": [0, 0, 0, 0, 0], "extended_capabilities": [],
                    "aid": 1 + len(clients),
                    "bytes": {"rx": st["rx"]["bytes"], "tx": st["tx"]["bytes"]},
                    "airtime": {"rx": 0, "tx": 0},
                    "packets": {"rx": st["rx"]["packets"],
                                "tx": st["tx"]["packets"]},
                    "rate": {"rx": st["rx"]["rate"] * 100,
                             "tx": st["tx"]["rate"] * 100},
                    "signal": st["signal"], "capabilities": {},
                }
            return ok(rid, {"freq": 5180 if g5 else 2437, "clients": clients})
        if meth == "list_bans":
            return ok(rid, {"bans": []})
        if meth == "get_features":
            return ok(rid, {"ht": True, "vht": g5, "he": False})
        if meth == "del_client":
            return ok(rid, {})
        return err(rid, 3)

    if obj == "network.wireless" and meth == "status":
        return ok(rid, {"radio0": {"up": True, "config": {"channel": "36"},
                                   "interfaces": [{"ifname": "wlan0",
                                                   "config": {"ssid": "OpenWrt",
                                                              "mode": "ap"}}]},
                        "radio1": {"up": True, "config": {"channel": "6"},
                                   "interfaces": []}})
    if obj == "network":
        return ok(rid, {})

    if obj == "luci-rpc":
        if meth == "getHostHints":
            return ok(rid, {"AA:BB:CC:11:22:33":
                            {"ipaddrs": ["192.168.1.130"],
                             "name": "roland-laptop"},
                            "AA:BB:CC:44:55:66":
                            {"ipaddrs": ["192.168.1.131"], "name": "iot-plug"}})
        if meth == "getNetworkDevices":
            # DSA user ports carry devtype "dsa" with the conduit as parent —
            # this is how the controller detects a switch without any
            # filesystem grant.
            devs = {"br-lan": {"devtype": "bridge"},
                    "eth0": {"devtype": "ethernet"},
                    "wan": {"devtype": "dsa", "parent": "eth0"}}
            for i in range(1, 5):
                devs[f"lan{i}"] = {"devtype": "dsa", "parent": "eth0"}
            for w in ("wlan0", "wlan1"):
                devs[w] = {"devtype": "wlan"}
            return ok(rid, devs)
        if meth == "getDHCPLeases":
            return ok(rid, {"dhcp_leases": [
                {"macaddr": "AA:BB:CC:11:22:33", "ipaddr": "192.168.1.130",
                 "hostname": "roland-laptop", "expires": 30000}]})
        return ok(rid, {})

    if obj.startswith("hostapd.") and meth == "get_clients":
        return ok(rid, {"freq": 5180, "clients": {
            "aa:bb:cc:11:22:33": {"auth": True, "assoc": True,
                                  "signal": -54, "aid": 1}}})

    if obj == "uci":
        with lock:
            return handle_uci(rid, sess, meth, args)

    if obj in OBJECTS and meth in OBJECTS.get(obj, {}):
        return ok(rid, {})
    return err(rid, 4)  # NOT_FOUND


def handle_uci(rid, sid, meth, args):
    config = args.get("config")
    # uci.add will not create a config file that does not exist — it returns
    # NOT_FOUND, which is why the scratch configs must be touched first.
    if meth in ("add", "set", "delete") and config not in committed:
        return err(rid, 4)
    if meth == "configs":
        return ok(rid, {"configs": sorted(committed.keys())})
    if meth == "get":
        cfg = effective(sid, config)
        if cfg is None:
            return err(rid, 4)
        section, option = args.get("section"), args.get("option")
        if section and option:
            val = cfg.get(section, {}).get(option)
            return ok(rid, {"value": val}) if val is not None else err(rid, 4)
        if section:
            sec = cfg.get(section)
            return ok(rid, {"values": sec}) if sec else err(rid, 4)
        return ok(rid, {"values": cfg})
    if meth == "add":
        name = args.get("name") or f"cfg{int(time.time()*1000) % 100000:05x}"
        stage(sid, config, "add", name, {"type": args.get("type"),
                                         "values": args.get("values", {})})
        return ok(rid, {"section": name})
    if meth == "set":
        stage(sid, config, "set", args.get("section"),
              {"values": args.get("values", {}), "type": args.get("type")})
        return ok(rid, {})
    if meth == "delete":
        stage(sid, config, "delete", args.get("section"), {})
        return ok(rid, {})
    if meth == "changes":
        mine = staged.get(sid, {})
        if config:
            ch = [[op, s] + ([json.dumps(pl)] if pl else [])
                  for op, s, pl in mine.get(config, [])]
            return ok(rid, {"changes": ch})
        return ok(rid, {"changes": {c: [[op, s] for op, s, _ in items]
                                    for c, items in mine.items()}})
    if meth == "revert":
        staged.get(sid, {}).pop(config, None)
        return ok(rid, {})
    if meth == "commit":
        commit_config(sid, config)
        return ok(rid, {})
    if meth == "apply":
        owner = rollback.get("owner")
        if owner is not None and owner != sid and rollback.get("deadline", 0) > time.time():
            # Measured: rpcd refuses a second armed apply while one is pending,
            # with status 6 — which here means "an apply is already armed", NOT
            # an authorization failure. The existing timer is left alone.
            return err(rid, 6)
        apply_all(sid, bool(args.get("rollback")),
                  int(args.get("timeout", 10)))
        return ok(rid, {})
    if meth == "confirm":
        res = confirm(sid)
        if res is None:
            return err(rid, 6)   # wrong session; timer keeps running
        return ok(rid, {}) if res else err(rid, 5)
    if meth == "rollback":
        dl = rollback.get("deadline")
        if dl:
            rollback["deadline"] = 0  # watchdog restores on next tick
            return ok(rid, {})
        return err(rid, 5)  # NO_DATA — nothing pending
    return err(rid, 3)


# --------------------------------------------------------------------------

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        if self.path != "/ubus":
            self.send_error(404)
            return
        n = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(n))
        except json.JSONDecodeError:
            self.send_error(400)
            return
        if isinstance(body, list):                 # batch
            out = [handle_one(r) for r in body]
        else:
            out = handle_one(body)
        data = json.dumps(out).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8088)
    args = ap.parse_args()
    threading.Thread(target=rollback_watchdog, daemon=True).start()
    socketserver.ThreadingTCPServer.allow_reuse_address = True
    with socketserver.ThreadingTCPServer(("127.0.0.1", args.port), Handler) as s:
        print(f"mock WRT3200ACM ubus on http://127.0.0.1:{args.port}/ubus "
              f"(password: {PASSWORD!r})")
        s.serve_forever()


if __name__ == "__main__":
    main()
