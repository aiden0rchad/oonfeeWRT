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
  * The mwlwifi gap: `iwinfo.survey` is NOT_SUPPORTED and `iw survey dump`
    returns no "busy time" line — so capability gating gets exercised in CI
    instead of discovered in the field.

What it does not model: timing realism, wireless reload behavior, hostapd
events, or multiple devices (run several instances on different ports for a
fleet).
"""

import argparse
import copy
import http.server
import json
import socketserver
import threading
import time

SESSION = "a" * 32
PASSWORD = "good"

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
}
staged = {}          # config -> [ (op, section, payload) ]
rollback = {}        # {"snapshot": deep copy, "deadline": ts} when armed
lock = threading.RLock()


def stage(config, op, section, payload):
    staged.setdefault(config, []).append((op, section, payload))


def commit_config(config):
    """Apply one config's staged delta to committed state."""
    for op, section, payload in staged.pop(config, []):
        cfg = committed.setdefault(config, {})
        if op == "add":
            sec = {".type": payload["type"]}
            sec.update(payload.get("values", {}))
            cfg[section] = sec
        elif op == "set":
            cfg.setdefault(section, {".type": "unknown"}).update(
                payload.get("values", {}))
        elif op == "delete":
            cfg.pop(section, None)


def apply_all(rb, timeout):
    """uci.apply: commit ALL staged deltas; optionally arm rollback."""
    if rb:
        rollback["snapshot"] = copy.deepcopy(committed)
        rollback["deadline"] = time.time() + timeout
    for config in list(staged.keys()):
        commit_config(config)


def confirm():
    rollback.clear()


def rollback_watchdog():
    global committed
    while True:
        time.sleep(0.5)
        with lock:
            dl = rollback.get("deadline")
            if dl and time.time() > dl:
                committed = copy.deepcopy(rollback["snapshot"])
                rollback.clear()
                staged.clear()


# --------------------------------------------------------------------------
# ubus objects
# --------------------------------------------------------------------------

OBJECTS = {
    "session": {"login": {}, "list": {}, "destroy": {}, "access": {}},
    "uci": {m: {} for m in ("configs", "get", "set", "add", "delete",
                            "changes", "revert", "commit", "apply",
                            "confirm", "rollback")},
    "system": {"board": {}, "info": {}, "reboot": {}},
    "file": {"read": {}, "write": {}, "exec": {}, "list": {}, "stat": {}},
    "iwinfo": {m: {} for m in ("devices", "info", "assoclist", "freqlist",
                               "txpowerlist", "scan", "countrylist")},
    # note: no "survey" method — mwlwifi
    "network": {"reload": {}, "restart": {}},
    "network.interface": {"dump": {}},
    "network.device": {"status": {}},
    "network.wireless": {"status": {}},
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

ASSOC = [{"mac": "AA:BB:CC:11:22:33", "signal": -54, "noise": -92,
          "inactive": 800,
          "rx": {"rate": 585000, "mcs": 7, "40mhz": False},
          "tx": {"rate": 866700, "mcs": 9, "vht": True, "mhz": 80},
          "tx_packets": 120433, "rx_packets": 90211},
         {"mac": "AA:BB:CC:44:55:66", "signal": -67, "noise": -92,
          "inactive": 120,
          "rx": {"rate": 130000}, "tx": {"rate": 195000},
          "tx_packets": 5522, "rx_packets": 4210}]


def ok(rid, data=None):
    res = [0] if data is None else [0, data]
    return {"jsonrpc": "2.0", "id": rid, "result": res}


def err(rid, code):
    return {"jsonrpc": "2.0", "id": rid, "result": [code]}


def exec_cmd(rid, cmd, params):
    def out(code, stdout):
        return ok(rid, {"code": code, "stdout": stdout, "stderr": ""})
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
        if args.get("password") == PASSWORD:
            return ok(rid, {"ubus_rpc_session": SESSION, "timeout": 300,
                            "expires": 300})
        return err(rid, 6)
    if sess != SESSION:
        return err(rid, 6)  # PERMISSION_DENIED

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
            return ok(rid, {})
        return err(rid, 8)

    if obj == "iwinfo":
        if meth == "devices":
            return ok(rid, {"devices": ["wlan0", "wlan1"]})
        if meth == "info":
            dev = args.get("device", "wlan0")
            g5 = dev == "wlan0"
            return ok(rid, {"phy": "phy0" if g5 else "phy1",
                            "ssid": "OpenWrt", "mode": "Master",
                            "channel": 36 if g5 else 6,
                            "frequency": 5180 if g5 else 2437,
                            "txpower": 23, "quality": 60, "quality_max": 70,
                            "signal": -54, "noise": -92,
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
        return err(rid, 8)  # survey and others: NOT_SUPPORTED (mwlwifi)

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
            return handle_uci(rid, meth, args)

    if obj in OBJECTS and meth in OBJECTS.get(obj, {}):
        return ok(rid, {})
    return err(rid, 4)  # NOT_FOUND


def handle_uci(rid, meth, args):
    config = args.get("config")
    if meth == "configs":
        return ok(rid, {"configs": sorted(committed.keys())})
    if meth == "get":
        cfg = committed.get(config)
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
        stage(config, "add", name, {"type": args.get("type"),
                                    "values": args.get("values", {})})
        return ok(rid, {"section": name})
    if meth == "set":
        stage(config, "set", args.get("section"),
              {"values": args.get("values", {})})
        return ok(rid, {})
    if meth == "delete":
        stage(config, "delete", args.get("section"), {})
        return ok(rid, {})
    if meth == "changes":
        if config:
            ch = [[op, s] + ([json.dumps(pl)] if pl else [])
                  for op, s, pl in staged.get(config, [])]
            return ok(rid, {"changes": ch})
        return ok(rid, {"changes": {c: [[op, s] for op, s, _ in items]
                                    for c, items in staged.items()}})
    if meth == "revert":
        staged.pop(config, None)
        return ok(rid, {})
    if meth == "commit":
        commit_config(config)
        return ok(rid, {})
    if meth == "apply":
        apply_all(bool(args.get("rollback")), int(args.get("timeout", 10)))
        return ok(rid, {})
    if meth == "confirm":
        confirm()
        return ok(rid, {})
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
