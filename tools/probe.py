#!/usr/bin/env python3
"""
oonfeeWRT device probe
======================

Validates every assumption the oonfeeWRT architecture rests on, against a real
OpenWrt device. Run this BEFORE writing product code.

Python 3.8+, standard library only. No pip install, no jq, runs on macOS as-is.

    python3 probe.py 192.168.1.1 --user root --password 'hunter2'
    python3 probe.py 192.168.1.1 --user root --password 'hunter2' --write-tests
    python3 probe.py 192.168.1.1 --user root --ask-password --json report.json

SAFETY
------
Read-only by default. It logs in, enumerates, reads, and times things.

The apply/confirm/rollback tests (--write-tests) are the whole point of the
exercise, and they are written to be safe:

  * They only ever touch a scratch config named `oonfeewrt_probe`, which no
    service on the device reads. Applying it cannot affect networking.
  * They never touch network, wireless, firewall, dhcp, system, or dropbear.
  * They clean up after themselves, including on failure.
  * The deliberate-rollback test waits out a short timer without confirming, to
    verify the device really does revert itself. This is the single most
    important behaviour in the whole design, so it is worth proving.

Even so: run it first on a device you can physically reach.
"""

import argparse
import getpass
import http.client
import json
import re
import ssl
import statistics
import sys
import time
from urllib.parse import urlparse

NULL_SESSION = "00000000000000000000000000000000"

# ubus status codes
UBUS_OK = 0
UBUS_STATUS_DENIED = 6
UBUS_STATUS_NOT_FOUND = 4
UBUS_STATUS_NO_DATA = 5
UBUS_STATUS = {
    0: "OK", 1: "INVALID_COMMAND", 2: "INVALID_ARGUMENT", 3: "METHOD_NOT_FOUND",
    4: "NOT_FOUND", 5: "NO_DATA", 6: "PERMISSION_DENIED", 7: "TIMEOUT",
    8: "NOT_SUPPORTED", 9: "UNKNOWN_ERROR", 10: "CONNECTION_FAILED",
}

# rpcd reports ubus-level failures as JSON-RPC errors rather than as a status
# code in the result array. An ACL denial arrives as -32002, so these have to be
# folded back into UBUS_STATUS or every unpermitted method looks like a crash.
# Protocol faults (-32700/-32600/-32603) are absent on purpose: those are real
# errors and still raise.
JSONRPC_TO_UBUS = {
    -32000: 4,   # object not found
    -32001: 6,   # session not found -> denied
    -32002: 6,   # access denied
    -32003: 7,   # timeout
    -32004: 8,   # not supported
    -32005: 9,   # unknown
    -32006: 10,  # connection failed
    -32601: 3,   # method not found
    -32602: 2,   # invalid parameters
}

# Objects the architecture assumes exist.
# (object, [methods we care about], required)
# "required" means the design genuinely cannot proceed without it. Everything
# else degrades one feature, so it reports as informational rather than failure —
# otherwise the failure list fills with noise and stops being read.
EXPECTED_OBJECTS = [
    ("session",          ["login", "list", "destroy"], True),
    ("uci",              ["configs", "get", "set", "add", "delete", "changes",
                          "revert", "commit", "apply", "confirm", "rollback"], True),
    ("system",           ["board", "info", "reboot"], True),
    ("file",             ["read", "write", "exec", "list", "stat"], True),
    ("iwinfo",           ["devices", "info", "assoclist", "freqlist",
                          "txpowerlist", "scan", "countrylist", "survey"], False),
    ("network",          ["reload", "restart"], True),
    ("network.interface", ["dump"], True),
    ("network.device",   ["status"], False),
    ("network.wireless", ["status"], False),
    ("luci-rpc",         ["getNetworkDevices", "getWirelessDevices",
                          "getHostHints", "getDHCPLeases", "getBoardJSON"], False),
    ("luci",             ["getVersion", "getInitList"], False),
    ("rc",               ["list", "init"], False),
    ("service",          ["list"], False),
    ("dhcp",             ["ipv4leases", "ipv6leases"], False),
]

# Binaries the telemetry design wants to reach via file.exec.
EXPECTED_BINARIES = [
    ("iw",       "per-station stats, channel survey"),
    ("iwinfo",   "radio info fallback"),
    ("lldpcli",  "topology adjacency (pkg: lldpd)"),
    ("nlbw",     "per-client accounting (pkg: nlbwmon)"),
    ("ethtool",  "per-port switch stats"),
    ("bridge",   "fdb / vlan topology"),
    ("ip",       "link and route state"),
    ("df",       "free overlay space"),
    ("nft",      "firewall4 present"),
    ("iptables", "legacy firewall present"),
    ("vnstat",   "long-term interface totals"),
    ("wg",       "wireguard state"),
    ("apk",      "package manager (25.x+)"),
    ("opkg",     "package manager (24.x and earlier)"),
]

# Packages the tier-2 features want.
EXPECTED_PACKAGES = [
    "rpcd", "rpcd-mod-file", "rpcd-mod-iwinfo", "rpcd-mod-luci", "rpcd-mod-rpcsys",
    "uhttpd", "uhttpd-mod-ubus", "libustream-mbedtls", "px5g-mbedtls",
    "nlbwmon", "lldpd", "usteer", "dawn", "sqm-scripts", "vnstat2",
    "wireguard-tools", "firewall4", "nftables", "umdns",
]


# --------------------------------------------------------------------------
# transport
# --------------------------------------------------------------------------

class UbusError(Exception):
    pass


class Ubus:
    """Minimal ubus-over-HTTP JSON-RPC client with keep-alive."""

    def __init__(self, host, port=None, scheme=None, timeout=15, insecure=True):
        if "://" in host:
            u = urlparse(host)
            scheme = scheme or u.scheme
            host = u.hostname
            port = port or u.port
        elif host.count(":") == 1:
            # bare "host:port" — urlparse won't split this without a scheme
            h, _, p = host.partition(":")
            if p.isdigit():
                host, port = h, port or int(p)
        self.scheme = scheme or "http"
        self.host = host
        self.port = port or (443 if self.scheme == "https" else 80)
        self.timeout = timeout
        self.insecure = insecure
        self.session = NULL_SESSION
        self._conn = None
        self._id = 0
        self.cert_info = None
        self.last_status = None

    # -- connection -------------------------------------------------------

    def _connect(self):
        if self.scheme == "https":
            ctx = ssl.create_default_context()
            if self.insecure:
                ctx.check_hostname = False
                ctx.verify_mode = ssl.CERT_NONE
            conn = http.client.HTTPSConnection(
                self.host, self.port, timeout=self.timeout, context=ctx)
            conn.connect()
            try:
                sock = conn.sock
                self.cert_info = {
                    "cipher": sock.cipher(),
                    "tls_version": sock.version(),
                    "peercert_der_sha256": None,
                }
                der = sock.getpeercert(binary_form=True)
                if der:
                    import hashlib
                    self.cert_info["peercert_der_sha256"] = \
                        hashlib.sha256(der).hexdigest()
            except Exception:
                pass
            return conn
        conn = http.client.HTTPConnection(
            self.host, self.port, timeout=self.timeout)
        conn.connect()
        return conn

    def connect(self):
        if self._conn is None:
            self._conn = self._connect()
        return self._conn

    def close(self):
        if self._conn is not None:
            try:
                self._conn.close()
            except Exception:
                pass
            self._conn = None

    # -- raw request ------------------------------------------------------

    def _post(self, payload, fresh=False):
        body = json.dumps(payload)
        headers = {"Content-Type": "application/json"}
        for attempt in (0, 1):
            try:
                if fresh:
                    conn = self._connect()
                else:
                    conn = self.connect()
                conn.request("POST", "/ubus", body=body, headers=headers)
                resp = conn.getresponse()
                self.last_status = resp.status
                data = resp.read()
                if fresh:
                    conn.close()
                if resp.status != 200:
                    raise UbusError(f"HTTP {resp.status}: {data[:200]!r}")
                return json.loads(data)
            except (http.client.HTTPException, ConnectionError, OSError) as e:
                self.close()
                if attempt or fresh:
                    raise UbusError(f"transport: {e}") from e
        raise UbusError("unreachable")

    def _next_id(self):
        self._id += 1
        return self._id

    # -- api --------------------------------------------------------------

    def call(self, obj, method, args=None, fresh=False, raw=False):
        """Returns (status_code, data). Never raises on ubus-level errors."""
        payload = {
            "jsonrpc": "2.0", "id": self._next_id(), "method": "call",
            "params": [self.session, obj, method, args if args is not None else {}],
        }
        r = self._post(payload, fresh=fresh)
        if raw:
            return r
        if "error" in r:
            err = r["error"] or {}
            status = JSONRPC_TO_UBUS.get(err.get("code"))
            if status is None:
                raise UbusError(f"jsonrpc error: {r['error']}")
            return status, None
        res = r.get("result")
        if isinstance(res, list):
            code = res[0]
            data = res[1] if len(res) > 1 else None
            return code, data
        return UBUS_OK, res

    def call_ok(self, obj, method, args=None):
        code, data = self.call(obj, method, args)
        if code != UBUS_OK:
            raise UbusError(f"{obj}.{method} -> {UBUS_STATUS.get(code, code)}")
        return data

    def batch(self, calls):
        """calls = [(obj, method, args), ...]. Returns (supported, results)."""
        payload = [{
            "jsonrpc": "2.0", "id": self._next_id(), "method": "call",
            "params": [self.session, o, m, a or {}],
        } for (o, m, a) in calls]
        try:
            r = self._post(payload)
        except UbusError as e:
            return False, str(e)
        if isinstance(r, list) and len(r) == len(calls):
            return True, r
        return False, r

    def list_objects(self):
        """Try the documented shapes; return (objects_dict, which_form_worked)."""
        for params in ([self.session, "*"], ["*"], [self.session], []):
            payload = {"jsonrpc": "2.0", "id": self._next_id(),
                       "method": "list", "params": params}
            try:
                r = self._post(payload)
            except UbusError:
                continue
            res = r.get("result")
            if isinstance(res, dict) and res:
                return res, f"params={params!r}"
            if isinstance(res, list) and res and isinstance(res[0], str):
                return {name: None for name in res}, f"params={params!r}"
        return {}, "no form worked"

    def login(self, username, password):
        self.session = NULL_SESSION
        code, data = self.call("session", "login",
                               {"username": username, "password": password})
        if code != UBUS_OK or not data or "ubus_rpc_session" not in data:
            raise UbusError(
                f"login failed: {UBUS_STATUS.get(code, code)} {data!r}")
        self.session = data["ubus_rpc_session"]
        self._creds = (username, password)
        return data

    def fresh_session(self):
        """A second, independent ubus session against the same device.

        Not the same as close() — that drops the TCP connection but keeps the
        session token, and rpcd scopes staged UCI deltas to the token. Reads
        through the applying session therefore still see a delta that rollback
        has already undone on disk.
        """
        other = Ubus(self.host, port=self.port, scheme=self.scheme,
                     timeout=self.timeout, insecure=self.insecure)
        other.login(*self._creds)
        return other

    # -- convenience ------------------------------------------------------

    def read_file(self, path):
        code, data = self.call("file", "read", {"path": path})
        if code != UBUS_OK:
            return None
        return (data or {}).get("data")

    def exec_(self, command, params=None):
        code, data = self.call("file", "exec",
                               {"command": command, "params": params or []})
        if code != UBUS_OK:
            return None
        return data

    def exec_status(self, command, params=None):
        """(reachable, data). reachable is False only when the ACL blocked us.

        exec_() collapses "denied" and "ran, found nothing" into None, which
        makes an unreachable probe indistinguishable from an absent capability.
        Anything deciding whether a feature EXISTS must use this instead.
        """
        code, data = self.call("file", "exec",
                               {"command": command, "params": params or []})
        if code == UBUS_STATUS_DENIED:
            return False, None
        return True, (data if code == UBUS_OK else None)


# --------------------------------------------------------------------------
# report scaffolding
# --------------------------------------------------------------------------

class Report:
    def __init__(self):
        self.sections = []
        self.data = {}
        self.warnings = []
        self.failures = []

    def section(self, title):
        self.sections.append(("h", title))

    def line(self, text=""):
        self.sections.append(("p", text))

    def item(self, ok, label, detail=""):
        mark = {True: "PASS", False: "FAIL", None: "n/a "}[ok]
        self.sections.append(("i", (mark, label, detail)))
        if ok is False:
            self.failures.append(f"{label} — {detail}" if detail else label)

    def warn(self, text):
        self.warnings.append(text)
        self.sections.append(("w", text))

    def render(self):
        out = []
        for kind, payload in self.sections:
            if kind == "h":
                out.append("")
                out.append(payload)
                out.append("-" * len(payload))
            elif kind == "p":
                out.append(payload)
            elif kind == "w":
                out.append(f"  !! {payload}")
            else:
                mark, label, detail = payload
                line = f"  [{mark}] {label}"
                if detail:
                    line = f"{line:<52} {detail}"
                out.append(line)
        return "\n".join(out)


# --------------------------------------------------------------------------
# probes
# --------------------------------------------------------------------------

def probe_identity(ub, rep):
    rep.section("1. Device identity")
    board = {}
    info = {}
    try:
        board = ub.call_ok("system", "board") or {}
        rep.item(True, "system.board")
    except UbusError as e:
        rep.item(False, "system.board", str(e))
    try:
        info = ub.call_ok("system", "info") or {}
        rep.item(True, "system.info")
    except UbusError as e:
        rep.item(False, "system.info", str(e))

    rel = (board.get("release") or {})
    fields = {
        "model": board.get("model"),
        "board_name": board.get("board_name"),
        "system": board.get("system"),
        "kernel": board.get("kernel"),
        "openwrt_release": rel.get("description") or rel.get("version"),
        "target": rel.get("target"),
        # No package architecture here: system.board does not expose it (it
        # lives in /etc/openwrt_release as DISTRIB_ARCH). `target` is the
        # closest thing this call offers, so don't invent an `arch` row.
        "rootfs_type": board.get("rootfs_type"),
    }
    for k, v in fields.items():
        if v:
            rep.line(f"      {k:<18} {v}")

    mem = info.get("memory") or {}
    if mem:
        tot = mem.get("total", 0) // (1024 * 1024)
        free = mem.get("free", 0) // (1024 * 1024)
        rep.line(f"      {'ram_total_mb':<18} {tot}")
        rep.line(f"      {'ram_free_mb':<18} {free}")
        if tot and tot < 128:
            rep.warn(f"Only {tot} MB RAM — below the class-C floor.")
    if info.get("load"):
        rep.line(f"      {'load':<18} {info['load']}")

    rep.data["board"] = board
    rep.data["info"] = info

    # Device class per DEVICE-BUDGET.md
    sysname = (board.get("system") or "").lower()
    target = (rel.get("target") or "").lower()
    ram_mb = (mem.get("total", 0) // (1024 * 1024)) or 0
    if "mt7621" in sysname or "mt7621" in target or "1004kc" in sysname:
        cls, why = "C (constrained)", "MT7621 / MIPS 1004Kc"
    elif "armada" in sysname or "mvebu" in target:
        cls, why = "A (comfortable)", "Marvell Armada / mvebu"
    elif "mt7981" in sysname or "filogic" in target or "mediatek" in target:
        cls, why = "B (modern efficient)", "MT798x / Filogic"
    elif ram_mb >= 256:
        cls, why = "B (assumed)", f"{ram_mb} MB RAM, unrecognised SoC"
    else:
        cls, why = "C (assumed)", f"{ram_mb} MB RAM, unrecognised SoC"
    rep.line()
    rep.line(f"      DEVICE CLASS: {cls}   ({why})")
    rep.data["device_class"] = cls
    return board


def probe_objects(ub, rep):
    rep.section("2. ubus object surface")
    objects, form = ub.list_objects()
    rep.line(f"  list form that worked: {form}")
    rep.line(f"  objects visible to this user: {len(objects)}")
    rep.data["ubus_objects"] = {
        k: (sorted(v.keys()) if isinstance(v, dict) else None)
        for k, v in objects.items()
    }

    hostapd = sorted(k for k in objects if k.startswith("hostapd"))
    missing_optional = []
    for name, methods, required in EXPECTED_OBJECTS:
        if name not in objects:
            if required:
                rep.item(False, f"object {name}", "ABSENT — design blocker")
            else:
                rep.item(None, f"object {name}", "absent (optional)")
                missing_optional.append(name)
            continue
        have = objects.get(name)
        if not isinstance(have, dict):
            rep.item(True, f"object {name}", "present (methods unlisted)")
            continue
        gaps = [m for m in methods if m not in have]
        if gaps:
            rep.item(False if required else None, f"object {name}",
                     f"missing: {', '.join(gaps)}")
        else:
            rep.item(True, f"object {name}", f"{len(have)} methods")
    rep.data["missing_optional_objects"] = missing_optional
    if "luci-rpc" in missing_optional:
        rep.warn("luci-rpc absent (pkg: rpcd-mod-luci). getHostHints is the "
                 "cheapest way to fill the Client Devices table — without it "
                 "you pay many more round-trips for the same data.")

    rep.line()
    if hostapd:
        rep.item(True, "hostapd.* objects", ", ".join(hostapd[:6]))
        first = objects.get(hostapd[0])
        if isinstance(first, dict):
            rep.line(f"      methods: {', '.join(sorted(first.keys()))}")
    else:
        rep.item(None, "hostapd.* objects",
                 "none — no AP-mode radio up, or hostapd ubus disabled")
    return objects


def probe_batch(ub, rep):
    rep.section("3. JSON-RPC batching")
    rep.line("  The architecture assumes many ubus calls can ride one HTTP")
    rep.line("  request. On class-C hardware this is the biggest cheap win.")
    ok, res = ub.batch([
        ("system", "info", {}),
        ("system", "board", {}),
        ("network.interface", "dump", {}),
    ])
    if ok:
        rep.item(True, "batch of 3 accepted", f"{len(res)} responses")
        rep.data["batch_supported"] = True
    else:
        rep.item(False, "batch of 3 accepted", f"got: {str(res)[:120]}")
        rep.warn("No batching. Budget one HTTP round-trip per ubus call — "
                 "raises the cost of the focused poll rate substantially.")
        rep.data["batch_supported"] = False
    return ok


def probe_tls_cost(ub, rep, iterations=8):
    rep.section("4. Transport cost (the dominant cost on weak CPUs)")

    def timeit(fresh):
        samples = []
        for _ in range(iterations):
            t0 = time.perf_counter()
            try:
                ub.call("system", "info", {}, fresh=fresh)
            except UbusError:
                return None
            samples.append((time.perf_counter() - t0) * 1000.0)
        return samples

    warm = timeit(fresh=False)
    cold = timeit(fresh=True)
    ub.close()

    if warm:
        rep.item(True, "keep-alive request",
                 f"median {statistics.median(warm):6.1f} ms")
        rep.data["ms_keepalive_median"] = statistics.median(warm)
    if cold:
        rep.item(True, "new-connection request",
                 f"median {statistics.median(cold):6.1f} ms")
        rep.data["ms_fresh_median"] = statistics.median(cold)
    if warm and cold:
        med_w = statistics.median(warm)
        med_c = statistics.median(cold)
        # clamp: on loopback/fast links timing noise can make this negative
        overhead = max(0.0, med_c - med_w)
        rep.line()
        rep.line(f"  connection setup overhead: {overhead:.1f} ms per request")
        if ub.scheme == "https":
            rep.line(f"  TLS: {ub.cert_info}")
            if overhead > 120:
                rep.warn(f"TLS handshake costs {overhead:.0f} ms. Persistent "
                         "connections are mandatory, not an optimisation. "
                         "Consider ECDSA certs.")
        elif overhead > 40:
            rep.warn(f"Even over plain HTTP, connection setup costs "
                     f"{overhead:.0f} ms. Keep connections alive.")
        rep.data["ms_connection_overhead"] = overhead


def probe_poll_cost(ub, rep, seconds=20):
    rep.section("5. What a focused poll actually costs this device")
    before = ub.read_file("/proc/stat")
    if not before:
        rep.item(None, "CPU delta measurement", "file.read /proc/stat denied")
        return

    def cpu_totals(text):
        for ln in text.splitlines():
            if ln.startswith("cpu "):
                v = [int(x) for x in ln.split()[1:]]
                idle = v[3] + (v[4] if len(v) > 4 else 0)
                return sum(v), idle
        return None, None

    t0_tot, t0_idle = cpu_totals(before)
    calls = [("system", "info", {}), ("network.device", "status", {}),
             ("iwinfo", "devices", {}), ("luci-rpc", "getHostHints", {})]
    n = 0
    start = time.time()
    while time.time() - start < seconds:
        for o, m, a in calls:
            try:
                ub.call(o, m, a)
                n += 1
            except UbusError:
                pass
        time.sleep(1.0)
    after = ub.read_file("/proc/stat")
    t1_tot, t1_idle = cpu_totals(after or "")

    if t1_tot and t0_tot and t1_tot > t0_tot:
        busy = (t1_tot - t0_tot) - (t1_idle - t0_idle)
        pct = 100.0 * busy / (t1_tot - t0_tot)
        rep.item(True, f"{n} calls over {seconds}s",
                 f"device CPU busy {pct:.2f}% during window")
        rep.data["poll_cpu_pct"] = pct
        rep.line("      (whole-device figure, includes normal traffic —")
        rep.line("       compare against an idle baseline run to isolate us)")
        if pct > 12:
            rep.warn(f"{pct:.1f}% CPU during a ~1 Hz poll. Lengthen intervals "
                     "for this class and lean hard on batching.")
    else:
        rep.item(None, "CPU delta measurement", "insufficient jiffy delta")


def probe_binaries_and_packages(ub, rep):
    rep.section("6. Binaries reachable via file.exec")
    code, _ = ub.call("file", "exec", {"command": "true", "params": []})
    if code != UBUS_OK:
        rep.item(False, "file.exec", UBUS_STATUS.get(code, code))
        rep.warn("No file.exec. Channel survey, LLDP and per-port stats are "
                 "unreachable — the Radios and Topology screens lose data.")
        return
    # Deliberately not "file.exec: permitted" — under rpcd exec is never a
    # global capability, only a per-command-line one, and claiming otherwise
    # makes the per-binary results below look more authoritative than they are.
    rep.item(True, "file.exec", "`true` is permitted; each command is granted "
                                "individually")

    found = {}
    for binname, why in EXPECTED_BINARIES:
        ok_exec, r = ub.exec_status("which", [binname])
        if not ok_exec:
            found[binname] = None
            rep.item(None, f"  {binname}",
                     "NOT OBSERVABLE — `which` is not in this ACL's exec list")
            continue
        path = ((r or {}).get("stdout") or "").strip()
        ok = bool(path) and (r or {}).get("code") == 0
        found[binname] = path or None
        rep.item(True if ok else None, f"  {binname}",
                 path if ok else f"absent — {why}")
    rep.data["binaries"] = found

    rep.section("7. Installed packages")
    pm = "apk" if found.get("apk") else ("opkg" if found.get("opkg") else None)
    if not pm:
        rep.item(None, "package manager", "neither apk nor opkg found")
        return
    rep.line(f"  package manager: {pm}")
    if pm == "apk":
        r = ub.exec_("apk", ["list", "--installed"]) or {}
    else:
        r = ub.exec_("opkg", ["list-installed"]) or {}
    text = (r.get("stdout") or "")
    installed = set()
    for ln in text.splitlines():
        m = re.match(r"^([A-Za-z0-9._+-]+?)(?:-\d|\s+-\s+|\s)", ln.strip())
        if m:
            installed.add(m.group(1))
    for pkg in EXPECTED_PACKAGES:
        rep.item(True if pkg in installed else None, f"  {pkg}",
                 "" if pkg in installed else "not installed")
    rep.data["packages_installed"] = sorted(installed)

    # free space — a full overlay is how you brick a 16 MB router
    r = ub.exec_("df", ["-k", "/overlay"]) or {}
    for ln in (r.get("stdout") or "").splitlines()[1:]:
        parts = ln.split()
        if len(parts) >= 4:
            free_mb = int(parts[3]) / 1024.0
            rep.line()
            rep.line(f"  free /overlay: {free_mb:.1f} MB")
            rep.data["overlay_free_mb"] = free_mb
            if free_mb < 3:
                rep.warn(f"Only {free_mb:.1f} MB free on /overlay. Offer no "
                         "tier-2 package installs on this device.")
            break


def probe_radios(ub, rep):
    rep.section("8. Radio / RF data availability")
    code, devs = ub.call("iwinfo", "devices")
    if code != UBUS_OK:
        rep.item(False, "iwinfo.devices", UBUS_STATUS.get(code, code))
        return
    devices = (devs or {}).get("devices", [])
    rep.item(True, "iwinfo.devices", ", ".join(devices) or "none")
    rep.data["radios"] = {}

    for dev in devices:
        rep.line()
        rep.line(f"  --- {dev} ---")
        code, info = ub.call("iwinfo", "info", {"device": dev})
        entry = {}
        if code == UBUS_OK and info:
            entry["info"] = info
            for k in ("channel", "frequency", "txpower", "mode", "hwmodes",
                      "country", "hardware"):
                if k in info:
                    rep.line(f"      {k:<12} {info[k]}")
        code, al = ub.call("iwinfo", "assoclist", {"device": dev})
        if code == UBUS_OK:
            results = (al or {}).get("results", [])
            entry["assoc_count"] = len(results)
            rep.item(True, f"  assoclist ({dev})", f"{len(results)} stations")
            if results:
                st = results[0]
                keys = sorted(st.keys())
                rep.line(f"      station fields: {', '.join(keys)}")

                def has_field(direction, field):
                    """iwinfo nests per-direction counters as {"tx":{"retries":…}};
                    some builds flatten them to tx_retries. Accept both, or the
                    report claims data is missing while it sits one level down —
                    which pushes the design to spawn `iw station dump` for
                    counters it already has."""
                    nested = isinstance(st.get(direction), dict) and \
                        field in st[direction]
                    return nested or f"{direction}_{field}" in st

                def has_rate(direction):
                    return has_field(direction, "rate")

                # These drive the Radios and Client Devices columns.
                for need in ("signal", "signal_avg", "noise", "inactive",
                             "connected_time"):
                    rep.item(need in st, f"      field '{need}'")
                rep.item(has_rate("rx"), "      rx rate (nested or flat)")
                rep.item(has_rate("tx"), "      tx rate (nested or flat)")
                for direction, field in (("tx", "retries"), ("tx", "failed"),
                                         ("tx", "packets"), ("rx", "packets"),
                                         ("tx", "bytes"), ("rx", "bytes")):
                    present = has_field(direction, field)
                    rep.item(True if present else None,
                             f"      field '{direction}.{field}'",
                             "" if present else "use `iw station dump` instead")
                entry["station_fields"] = keys
                entry["station_sample"] = st

                # A field can be present and still unusable. mwlwifi reports a
                # per-station noise floor that jumps tens of dB between reads,
                # so an SNR column computed per sample flails visibly. Presence
                # checks cannot catch this; only re-reading can.
                noises = []
                for _ in range(4):
                    time.sleep(0.4)
                    c2, a2 = ub.call("iwinfo", "assoclist", {"device": dev})
                    if c2 != UBUS_OK:
                        break
                    for s2 in (a2 or {}).get("results", []):
                        if s2.get("mac") == st.get("mac") and "noise" in s2:
                            noises.append(s2["noise"])
                if len(noises) >= 3:
                    spread = max(noises) - min(noises)
                    stable = spread <= 6
                    rep.item(True if stable else None,
                             "      noise floor stable across reads",
                             f"spread {spread} dB over {len(noises)} reads"
                             + ("" if stable else
                                " — DO NOT compute per-sample SNR; smooth it "
                                "or omit the column"))
                    entry["noise_spread_db"] = spread
                    if not stable:
                        rep.warn(
                            f"{dev}: per-station noise varies by {spread} dB "
                            "between reads. Use signal_avg against a smoothed "
                            "noise floor, or show RSSI without SNR.")
        else:
            rep.item(None, f"  assoclist ({dev})", UBUS_STATUS.get(code, code))

        # Cross-source presence check. These two were measured disagreeing for
        # 131 s continuously on real hardware, with hostapd's event log
        # bracketing the window — i.e. iwinfo was the one under-reporting, most
        # likely for a power-saving station. Which source to trust is still
        # open, so report the divergence rather than pick a winner.
        code_h, hc = ub.call(f"hostapd.{dev}", "get_clients")
        if code_h == UBUS_OK and code == UBUS_OK:
            iw_macs = {s.get("mac", "").lower()
                       for s in (al or {}).get("results", [])}
            ha_macs = {m.lower() for m in (hc or {}).get("clients", {})}
            ghosts = ha_macs - iw_macs
            missing = iw_macs - ha_macs
            agree = not ghosts and not missing
            rep.item(True if agree else None,
                     f"  hostapd/iwinfo agree on who is connected ({dev})",
                     "both sources list the same stations" if agree else
                     f"{len(ghosts)} in hostapd only, {len(missing)} in iwinfo "
                     "only — sources disagree; cross-check before trusting "
                     "either as the client list")
            entry["presence_ghosts"] = sorted(ghosts)
            if ghosts:
                rep.warn(
                    f"{dev}: hostapd reports {len(ghosts)} station(s) that "
                    "iwinfo does not. Do not source the client list from one "
                    "of these alone — see ARCHITECTURE section 5.")

        code, sv = ub.call("iwinfo", "survey", {"device": dev})
        rep.item(True if code == UBUS_OK else None, f"  iwinfo.survey ({dev})",
                 "native survey (cheap — no process spawn)" if code == UBUS_OK
                 else "absent — fall back to `iw survey dump` via file.exec")
        if code == UBUS_OK:
            rows = (sv or {}).get("results") or []
            # A 5 GHz radio returns one row per frequency and only the in-use
            # one carries counters, so rows[0] is often the empty one.
            row = max(rows, key=lambda r: r.get("active_time") or 0) if rows else {}
            # Airtime only needs these two. rx_time/tx_time are reported but
            # mwlwifi leaves them uninitialised, so they are not usable here.
            usable = bool(row.get("active_time")) and "busy_time" in row
            rep.item(usable, "    busy/active time present",
                     "channel utilization computable from the cheap path"
                     if usable else "no airtime counters in the native survey")
            entry["survey_usable"] = usable
            # Aggregate, never assign: this key is global but the loop is per
            # radio, so a plain assignment lets the last radio (or the exec
            # fallback) overwrite a native yes with a no.
            rep.data["survey_dump_usable"] = (
                rep.data.get("survey_dump_usable") or usable)
            entry["survey_fields"] = sorted(row)
            # iwinfo.survey reports noise unsigned; iwinfo.info reports it
            # signed. Reading the wrong one puts +163 dBm on screen.
            n_sv, n_info = row.get("noise"), (entry.get("info") or {}).get("noise")
            if isinstance(n_sv, int) and n_sv > 0:
                rep.warn(f"iwinfo.survey noise={n_sv} on {dev} is unsigned "
                         f"(info reports {n_info}). Take noise from "
                         "iwinfo.info, never from iwinfo.survey.")
        rep.data["radios"][dev] = entry

    # `iw survey dump` is only a FALLBACK for drivers with no native
    # iwinfo.survey. It must never overwrite a native answer: the fallback can
    # only ever equal or understate it, and a denied file.exec would otherwise
    # be recorded as "this driver has no airtime data".
    if rep.data.get("survey_dump_usable"):
        return
    ok, r = ub.exec_status("iw", ["dev"])
    if not ok:
        return
    phys = re.findall(r"Interface\s+(\S+)", (r or {}).get("stdout") or "")
    if not phys:
        return
    ok, r = ub.exec_status("iw", ["dev", phys[0], "survey", "dump"])
    if not ok:
        return
    has_busy = "busy time" in ((r or {}).get("stdout") or "")
    rep.line()
    # Absence is a driver capability fact, not a probe failure — the design
    # capability-gates those columns.
    rep.item(True if has_busy else None,
             "iw survey dump provides busy/active time",
             "interference + airtime computable" if has_busy
             else "driver gap — omit those columns")
    rep.data["survey_dump_usable"] = has_busy


def probe_switch_and_firewall(ub, rep):
    rep.section("9. Switch and firewall capability")
    # Absence and unreachability are different answers. Reporting "no DSA"
    # because the ACL blocked the check deletes a screen from a device that
    # supports it, so an unreachable probe records None, never False.
    #
    # Neither check touches the filesystem. luci-rpc.getNetworkDevices already
    # rides in the normal poll and tags DSA user ports with devtype "dsa", and
    # nft is detected by running the one nft command the ACL already grants.
    # The /sys route looks narrower but is not: rpcd canonicalises paths, so a
    # /sys/class/net/* grant silently never matches (those are symlinks into
    # /sys/devices), and widening it to /sys/devices/* hands over a subtree
    # because '*' crosses '/'.
    dsa = None
    code, devs = ub.call("luci-rpc", "getNetworkDevices")
    if code == UBUS_OK and isinstance(devs, dict):
        dsa = any((d or {}).get("devtype") == "dsa" for d in devs.values())
    rep.item(True if dsa else None, "DSA switch present",
             "per-port stats available" if dsa else
             "no DSA — hide the Ports screen on this device" if dsa is False
             else "NOT OBSERVABLE — luci-rpc denied, capability unknown")
    rep.data["dsa"] = dsa

    ok, r = ub.exec_status("/usr/sbin/nft",
                           ["--terse", "--json", "list", "ruleset"])
    fw4 = (r or {}).get("code") == 0 if ok else None
    rep.item(True if fw4 else None, "firewall4 / nftables",
             "zone model maps cleanly" if fw4 else
             "legacy iptables path" if fw4 is False
             else "NOT OBSERVABLE — file.exec denied, capability unknown")
    rep.data["firewall4"] = fw4

    # Flow offloading — the tradeoff from DEVICE-BUDGET section 3.3
    code, data = ub.call("uci", "get", {"config": "firewall",
                                        "section": "defaults"})
    if code == UBUS_OK and data:
        vals = (data or {}).get("values", data) or {}
        sw = vals.get("flow_offloading")
        hw = vals.get("flow_offloading_hw")
        rep.line()
        rep.line(f"  flow_offloading    = {sw!r}")
        rep.line(f"  flow_offloading_hw = {hw!r}")
        rep.data["flow_offloading"] = {"sw": sw, "hw": hw}
        if hw == "1":
            rep.warn("Hardware flow offload is ON. Per-client bandwidth "
                     "accounting (nlbwmon) will under-report or not work. "
                     "Do not change this silently — surface the tradeoff.")
        elif sw == "1":
            rep.warn("Software flow offload is ON. Accounting needs flowtable "
                     "counters (~3% throughput cost, kernel 5.7+).")


def probe_uci_read(ub, rep):
    rep.section("10. UCI read path")
    code, data = ub.call("uci", "configs")
    if code != UBUS_OK:
        rep.item(False, "uci.configs", UBUS_STATUS.get(code, code))
        return
    configs = (data or {}).get("configs", [])
    rep.item(True, "uci.configs", f"{len(configs)} configs")
    rep.data["uci_configs"] = configs
    for cfg in ("network", "wireless", "firewall", "dhcp", "system", "uhttpd"):
        code, _ = ub.call("uci", "get", {"config": cfg})
        rep.item(code == UBUS_OK, f"  read {cfg}",
                 "" if code == UBUS_OK else UBUS_STATUS.get(code, code))

    # Does anything already look like it isn't ours? (coexistence check)
    code, w = ub.call("uci", "get", {"config": "wireless"})
    if code == UBUS_OK and w:
        vals = (w or {}).get("values", {}) or {}
        ifaces = [k for k, v in vals.items()
                  if isinstance(v, dict) and v.get(".type") == "wifi-iface"]
        rep.line()
        rep.line(f"  existing wifi-iface sections: {len(ifaces)}")
        rep.line("  (all of these are foreign config — the reconciler must")
        rep.line("   read them for display and never write or delete them)")


def probe_apply_rollback(ub, rep, timeout=12):
    """The single most important behaviour in the design."""
    rep.section("11. apply / confirm / rollback  [WRITE TESTS]")
    rep.line("  Scratch config `oonfeewrt_probe` only. No service reads it.")

    SCRATCH = "oonfeewrt_probe"

    def cleanup():
        # Settle any armed rollback FIRST. If a timer is still running (an
        # aborted run, a Ctrl-C during the wait), the revert/delete/commit
        # below get undone by the device seconds later and the run reports a
        # clean exit over a scratch config that is still there. NO_DATA just
        # means nothing was pending.
        ub.call("uci", "rollback", {})
        ub.call("uci", "revert", {"config": SCRATCH})
        ub.call("uci", "delete", {"config": SCRATCH, "section": "probe"})
        ub.call("uci", "commit", {"config": SCRATCH})

    try:
        # -- create scratch section (plain commit path — this is the baseline,
        #    rollback protection is deliberately not in play yet)
        code, _ = ub.call("uci", "add", {
            "config": SCRATCH, "type": "probe", "name": "probe",
            "values": {"marker": "v1"}})
        if code != UBUS_OK:
            rep.item(False, "create scratch config", UBUS_STATUS.get(code, code))
            rep.warn("Cannot write UCI at all with these credentials.")
            return
        rep.item(True, "create scratch config", f"uci: {SCRATCH}")

        code, ch = ub.call("uci", "changes", {"config": SCRATCH})
        rep.item(code == UBUS_OK, "uci.changes lists staged delta",
                 json.dumps((ch or {}).get("changes", ""))[:60])

        code, _ = ub.call("uci", "commit", {"config": SCRATCH})
        rep.item(code == UBUS_OK, "uci.commit (baseline)",
                 UBUS_STATUS.get(code, ""))

        # ORDERING MATTERS, and it is subtle: uci.set only STAGES a delta.
        # uci.apply{rollback:true} is what commits it — snapshotting the
        # pre-apply state for the revert. If you commit manually before apply,
        # there is nothing staged, the snapshot equals the new state, and
        # rollback silently protects NOTHING. This is how LuCI itself works:
        # save() stages, apply() commits-with-rollback. Never commit first.

        # -- happy path: stage, apply with rollback armed, confirm
        code, _ = ub.call("uci", "set", {
            "config": SCRATCH, "section": "probe", "values": {"marker": "v2"}})
        rep.item(code == UBUS_OK, "stage change (uci.set, NO commit)")
        code, _ = ub.call("uci", "apply", {"rollback": True, "timeout": timeout})
        applied = code == UBUS_OK
        rep.item(applied, "uci.apply {rollback:true} commits staged delta",
                 UBUS_STATUS.get(code, code) if not applied else
                 f"rollback timer armed, {timeout}s")
        if not applied:
            rep.warn("apply with rollback is unsupported here. The safety "
                     "guarantee must be rebuilt another way BEFORE shipping.")
            cleanup()
            return

        time.sleep(1.0)
        code, _ = ub.call("uci", "confirm", {})
        confirmed = code == UBUS_OK
        rep.item(confirmed, "uci.confirm cancels the timer",
                 UBUS_STATUS.get(code, code) if not confirmed else "")

        code, data = ub.call("uci", "get", {
            "config": SCRATCH, "section": "probe", "option": "marker"})
        kept = (data or {}).get("value") == "v2"
        rep.item(kept, "confirmed change persisted",
                 f"marker={(data or {}).get('value')!r}")

        # -- the real test: stage, apply, then deliberately never confirm
        rep.line()
        rep.line(f"  Deliberate-rollback test: staging marker=v3, applying with")
        rep.line(f"  rollback armed, then waiting {timeout + 6}s WITHOUT confirming.")
        rep.line("  The device should restore marker=v2 by itself.")
        ub.call("uci", "set", {
            "config": SCRATCH, "section": "probe", "values": {"marker": "v3"}})
        code, _ = ub.call("uci", "apply", {"rollback": True, "timeout": timeout})
        if code != UBUS_OK:
            rep.item(False, "second apply", UBUS_STATUS.get(code, code))
            cleanup()
            return

        deadline = time.time() + timeout + 6
        while time.time() < deadline:
            time.sleep(2)
            sys.stderr.write(f"\r      waiting… {int(deadline - time.time()):2d}s ")
            sys.stderr.flush()
        sys.stderr.write("\r" + " " * 40 + "\r")

        # Verification MUST use a second session. rpcd's rollback restores
        # /etc/config but leaves the applying session's staged delta in place,
        # and session-scoped reads overlay that delta — so the session that
        # applied a change can never observe its own revert. Reading through
        # `ub` here reports the change as surviving when it did not.
        verifier = ub.fresh_session()
        try:
            code, data = verifier.call("uci", "get", {
                "config": SCRATCH, "section": "probe", "option": "marker"})
        finally:
            # Destroy, not just close: closing drops the socket but leaves a
            # live rpcd token behind until it times out.
            try:
                verifier.call("session", "destroy", {})
            except UbusError:
                pass
            verifier.close()
        got = (data or {}).get("value")
        reverted = got == "v2"
        rep.item(reverted, "UNCONFIRMED CHANGE AUTO-REVERTED",
                 f"marker={got!r} (expected 'v2')")
        stale = ub.call("uci", "get", {
            "config": SCRATCH, "section": "probe", "option": "marker"})[1]
        rep.item(None, "  applying session still reads",
                 f"marker={(stale or {}).get('value')!r} — confirm/verify needs "
                 "a fresh session")
        rep.data["rollback_works"] = reverted
        if reverted:
            rep.line()
            rep.line("  >> This is the guarantee the whole design rests on.")
            rep.line("     It works. Build the apply cycle around it.")
        else:
            rep.warn("Rollback did NOT revert the change. Do not ship an "
                     "apply path until this is understood — this is the "
                     "difference between a bad push and a car trip.")
    finally:
        cleanup()
        rep.line("  scratch config cleaned up")


# --------------------------------------------------------------------------

def verdict(rep):
    """Translate findings into design decisions. This is the point of the run."""
    d = rep.data
    rep.section("VERDICT — what this device means for the design")
    out = rep.line

    cls = d.get("device_class", "unknown")
    out(f"  Device class: {cls}")
    out()

    rb = d.get("rollback_works")
    if rb is True:
        out("  * SAFETY: apply/confirm/rollback works. Build the apply cycle")
        out("    exactly as ARCHITECTURE section 4 describes. This is the")
        out("    guarantee everything else depends on.")
    elif rb is False:
        out("  * SAFETY: rollback did NOT revert. STOP. Understand why before")
        out("    writing any apply path. Options: a staged-snapshot + scheduled")
        out("    revert built from stock tooling, or refuse to manage this")
        out("    release. Do not ship an apply path without the guarantee.")
    else:
        out("  * SAFETY: untested. Re-run with --write-tests. Do this before")
        out("    writing product code, not after.")
    out()

    if d.get("batch_supported"):
        out("  * TRANSPORT: JSON-RPC batching works. Collapse each poll into a")
        out("    single batched request — the cheapest win available.")
    else:
        out("  * TRANSPORT: no batching. Budget one round-trip per ubus call")
        out("    and lengthen the focused poll interval to compensate.")

    ovh = d.get("ms_connection_overhead")
    if ovh is not None:
        out(f"  * TRANSPORT: connection setup costs {ovh:.1f} ms.", )
        if ovh > 120:
            out("    Persistent connections are mandatory. Consider ECDSA certs")
            out("    and consider plain HTTP on an isolated management VLAN.")
        else:
            out("    Keep-alive still required, but this is not a bottleneck.")
    out()

    fo = d.get("flow_offloading") or {}
    if fo.get("hw") == "1":
        out("  * ACCOUNTING: hardware offload is ON. Per-client bandwidth is")
        out("    unavailable without disabling it. Default nlbwmon OFF, and")
        out("    surface the tradeoff rather than deciding for the user.")
    elif fo.get("sw") == "1":
        out("  * ACCOUNTING: software offload is ON. Accounting needs flowtable")
        out("    counters, ~3% throughput cost. Make it an explicit opt-in.")
    else:
        out("  * ACCOUNTING: no offload configured. nlbwmon is safe to offer,")
        out("    but check whether this device needs offload for its WAN speed.")
    out()

    if d.get("survey_dump_usable"):
        out("  * RADIOS SCREEN: survey data available — channel utilization")
        out("    (busy/active) is computable. Interference and the airtime")
        out("    split additionally need rx_time/tx_time; capability-gate them")
        out("    per driver rather than assuming this screen is complete.")
    elif d.get("survey_dump_usable") is None:
        out("  * RADIOS SCREEN: UNDETERMINED — could not observe survey data.")
        out("    Do not cut the columns on this evidence; re-probe with radios")
        out("    enabled and the iwinfo/file.exec grants in place.")
    else:
        out("  * RADIOS SCREEN: no usable survey data. The interference and")
        out("    airtime columns must be omitted on this device, not faked.")

    if d.get("dsa"):
        out("  * PORTS SCREEN: DSA present — per-port stats available.")
    elif d.get("dsa") is None:
        out("  * PORTS SCREEN: UNDETERMINED — the DSA check was denied, not")
        out("    answered — luci-rpc.getNetworkDevices was denied. Grant it")
        out("    and re-probe before hiding anything.")
    else:
        out("  * PORTS SCREEN: no DSA. Hide the Ports screen for this device")
        out("    entirely (UI-SPEC section 7: absent, not greyed out).")

    free = d.get("overlay_free_mb")
    if free is not None:
        if free < 3:
            out(f"  * PACKAGES: only {free:.1f} MB free. Offer NO tier-2")
            out("    installs here. Refuse cleanly and show the reason.")
        else:
            out(f"  * PACKAGES: {free:.1f} MB free — tier-2 installs viable.")

    cpu = d.get("poll_cpu_pct")
    if cpu is not None:
        out()
        out(f"  * BUDGET: device CPU was {cpu:.1f}% busy during the poll window")
        out("    (whole-device, not just us — run once idle to get a baseline")
        out("    and subtract). Compare against DEVICE-BUDGET section 2.")

    out()
    out("  Fold anything surprising back into docs/ARCHITECTURE.md before")
    out("  writing product code. That is what this probe is for.")


def main():
    ap = argparse.ArgumentParser(
        description="Probe an OpenWrt device for oonfeeWRT feasibility.")
    ap.add_argument("host", help="device address, e.g. 192.168.1.1 or https://…")
    ap.add_argument("--user", default="root")
    ap.add_argument("--password", default=None)
    ap.add_argument("--ask-password", action="store_true")
    ap.add_argument("--https", action="store_true",
                    help="use HTTPS (default: plain HTTP)")
    ap.add_argument("--write-tests", action="store_true",
                    help="run the apply/confirm/rollback tests (safe: scratch "
                         "config only, but it does write to the device)")
    ap.add_argument("--poll-seconds", type=int, default=20,
                    help="duration of the CPU-cost measurement (0 to skip)")
    ap.add_argument("--json", metavar="PATH", help="write raw findings as JSON")
    args = ap.parse_args()

    password = args.password
    if args.ask_password or password is None:
        password = getpass.getpass(f"password for {args.user}@{args.host}: ")

    ub = Ubus(args.host, scheme="https" if args.https else None)
    rep = Report()

    print(f"oonfeeWRT probe — {args.host} — {time.strftime('%Y-%m-%d %H:%M:%S')}")
    print("=" * 72)

    try:
        ub.login(args.user, password)
    except UbusError as e:
        print(f"\nFATAL: {e}\n")
        print("Checklist:")
        print("  * is uhttpd's ubus handler enabled?")
        print("      uci set uhttpd.main.ubus_prefix=/ubus && uci commit uhttpd")
        print("      /etc/init.d/uhttpd restart")
        print("  * are rpcd and rpcd-mod-luci installed?")
        print("  * correct credentials, and is the device reachable?")
        sys.exit(2)

    print("  login OK\n")

    def run(fn, *a):
        """A section that dies must not take the other ten with it."""
        try:
            fn(ub, rep, *a)
        except Exception as e:
            rep.section(f"{fn.__name__}  [ABORTED]")
            rep.item(False, fn.__name__, f"{type(e).__name__}: {e}")

    try:
        run(probe_identity)
        run(probe_objects)
        run(probe_batch)
        run(probe_tls_cost)
        if args.poll_seconds:
            run(probe_poll_cost, args.poll_seconds)
        run(probe_binaries_and_packages)
        run(probe_radios)
        run(probe_switch_and_firewall)
        run(probe_uci_read)
        if args.write_tests:
            run(probe_apply_rollback)
        else:
            rep.section("11. apply / confirm / rollback  [SKIPPED]")
            rep.line("  Re-run with --write-tests to validate the rollback")
            rep.line("  guarantee. It is the single most important behaviour")
            rep.line("  in the design and it is worth proving before you code.")
        verdict(rep)
    finally:
        ub.close()

    print(rep.render())

    print("\n" + "=" * 72)
    if rep.failures:
        print(f"FAILURES ({len(rep.failures)}):")
        for f in rep.failures:
            print(f"  - {f}")
    else:
        print("No hard failures.")
    if rep.warnings:
        print(f"\nWARNINGS ({len(rep.warnings)}):")
        for w in rep.warnings:
            print(f"  - {w}")

    if args.json:
        with open(args.json, "w") as fh:
            json.dump(rep.data, fh, indent=2, default=str)
        print(f"\nraw findings -> {args.json}")


if __name__ == "__main__":
    main()
