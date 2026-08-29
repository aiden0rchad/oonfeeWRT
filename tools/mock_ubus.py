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

What it does not model: timing realism, hostapd events, or multiple devices
(run several instances on different ports for a fleet). Wireless apply updates
its per-section runtime inventory immediately; product code still polls it.
"""

import argparse
import copy
import hashlib
import http.server
import ipaddress
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
nft_runtime_mode = "live"
nft_runtime_snapshot = ""
nft_lag_reads = 0
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
    configs = list(staged.get(sid, {}).keys())
    if rb:
        rollback["snapshot"] = copy.deepcopy(committed)
        rollback["staged_snapshot"] = copy.deepcopy(staged.get(sid, {}))
        rollback["owner"] = sid
        rollback["deadline"] = time.time() + timeout
        rollback["wireless_changed"] = "wireless" in configs
    for config in configs:
        commit_config(sid, config)
    if "wireless" in configs:
        sync_wireless_runtime()


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
                if rollback.get("wireless_changed"):
                    sync_wireless_runtime()
                rollback.clear()


# --------------------------------------------------------------------------
# ubus objects
# --------------------------------------------------------------------------

# The hostapd methods a real BSS object carries, as `ubus -v list` reported them
# on both reference devices. `bss_transition_request` is present here because
# the device has it — the controller's ACL deliberately does not grant it, and
# a fixture that hid it would make that decision invisible.
HOSTAPD_METHODS = ("get_status", "get_clients", "get_features", "list_bans",
                   "del_client", "bss_transition_request",
                   "rrm_nr_get_own", "rrm_nr_list", "rrm_nr_set")

# One BSS per hostapd object: what `rrm_nr_get_own` reports about itself, and
# what `rrm_nr_set` has most recently stored. Both SSIDs match so the two BSSes
# are each other's neighbour, which is the case worth exercising.
NR_OWN = {
    "hostapd.wlan0": {"bssid": "02:00:00:ab:24:42", "ssid": "OpenWrt",
                      "nr": "020000ab2442ef1900008024090603022a00"},
    "hostapd.wlan1": {"bssid": "02:00:00:ab:24:41", "ssid": "OpenWrt",
                      "nr": "020000ab2441ef0900005106070603000100"},
}
NR_LISTS = {}

# What `luci-rpc.getWirelessDevices` reports, shaped from a real capture on an
# Archer C6 running OpenWrt 25.12.5 (2026-08-16).
#
# This used to be unimplemented — the luci-rpc branch fell through to `{}` — so
# every consumer saw a device with NO interface modes at all. That is not a
# smaller world, it is a different one: `servesClients` reads an unknown mode as
# "assume AP", which is the behaviour §5o exists to replace, so the mode filter
# it added has never once been exercised against this mock.
#
# Two details are load-bearing and both come from the capture. `section` is
# present on some entries and absent on others, so consumers must treat it as
# optional. And the real response carries the wireless passphrase in `key`, in
# plaintext, on every entry — reproduced here so that a decoder which widens its
# struct and starts carrying it around fails a test rather than a review.
def _radio(band, channel, htmode, path, ifname, section, ssid):
    """One radio as luci-rpc.getWirelessDevices actually reports it.

    Captured field-for-field from an Archer C6 on OpenWrt 25.12.5. The shape
    matters more than the values, and three details here have already cost this
    project real bugs:

      * `config.network` and `config.device` are ARRAYS, not strings. The
        reconciler carries a comment about being bitten by exactly this — "the
        mock returned strings throughout, which is why this survived until a
        preview ran against hardware".
      * `disabled` is a BOOLEAN at both radio and config level, not the string
        "0" that `uci get` returns for the same option. A fixture that answers
        with the uci spelling would let code pass here and misread hardware.
      * `pending` and `retry_setup_failed` exist and are how a radio that
        failed to come up is distinguished from one that is merely off. Nothing
        reads them yet; the fixture carries them so that when something does,
        it is written against the real shape.
    """
    return {
        "up": True,
        "disabled": False,
        "pending": False,
        "autostart": True,
        "retry_setup_failed": False,
        "config": {
            "disabled": False,
            "type": "mac80211",
            "band": band,
            "channel": channel,
            "htmode": htmode,
            "path": path,
        },
        "interfaces": [{
            "ifname": ifname,
            "section": section,
            # stations and vlans are present and empty on a healthy AP with no
            # clients. Absent and empty are different answers, and the device
            # gives the second.
            "stations": [],
            "vlans": [],
            "config": {
                "network": ["lan"],
                "device": [section.replace("default_", "")],
                "mode": "ap",
                "encryption": "psk2",
                "key": "plaintext-passphrase",
                "ssid": ssid,
                "radios": [],
            },
            # The per-interface iwinfo block. The device carries a second copy
            # of the radio's identity here, and it is where `hardware` lives —
            # the string the capability registry matches driver defects on.
            "iwinfo": {
                "bssid": "00:11:22:33:44:55",
                "channel": int(channel),
                "country": "US",
                "encryption": {"enabled": True},
                "frequency": 5180 if band == "5g" else 2412,
                "frequency_offset": 0,
                "hardware": {"name": "Marvell 88W8964"},
                "htmodes": ["HT20", "HT40", "VHT20", "VHT40", "VHT80"],
                "hwmodes": ["a", "n", "ac"] if band == "5g" else ["b", "g", "n"],
                "hwmodes_text": "802.11nac" if band == "5g" else "802.11bgn",
                "mode": "Master",
                "noise": 161,
                "phy": "phy0" if band == "5g" else "phy1",
                "quality_max": 70,
                "ssid": ssid,
                "txpower": 20,
                "txpower_offset": 0,
            },
        }],
    }


WIRELESS_DEVICES = {
    "radio0": _radio("5g", "36", "VHT80", "pci0000:00/0000:00:00.0",
                     "wlan0", "default_radio0", "OpenWrt"),
    "radio1": _radio("2g", "6", "HT20", "pci0000:00/0000:00:01.0",
                     "wlan1", "default_radio1", "OpenWrt"),
}

DEFAULT_BOARD = {
    "kernel": "6.6.52", "hostname": "wrt3200acm",
    "system": "ARMv7 Processor rev 1 (v7l)",
    "model": "Linksys WRT3200ACM", "board_name": "linksys,wrt3200acm",
    "rootfs_type": "squashfs",
    "release": {"distribution": "OpenWrt", "version": "25.12.0",
                "revision": "r28000-abcdef", "target": "mvebu/cortexa9",
                "description": "OpenWrt 25.12.0"},
}
DEFAULT_BOARD_NETWORK = {
    "lan": {"device": "br-lan", "ports": ["lan1", "lan2", "lan3", "lan4"]},
    "wan": {"device": "wan"},
}
DEFAULT_NETWORK_DEVICES = {
    "br-lan": {"devtype": "bridge"}, "eth0": {"devtype": "ethernet"},
    "wan": {"devtype": "dsa", "parent": "eth0"},
    **{f"lan{i}": {"devtype": "dsa", "parent": "eth0"} for i in range(1, 5)},
    "wlan0": {"devtype": "wlan"}, "wlan1": {"devtype": "wlan"},
}

board = copy.deepcopy(DEFAULT_BOARD)
board_network = copy.deepcopy(DEFAULT_BOARD_NETWORK)
network_devices = copy.deepcopy(DEFAULT_NETWORK_DEVICES)

INITIAL_COMMITTED = copy.deepcopy(committed)
INITIAL_WIRELESS_DEVICES = copy.deepcopy(WIRELESS_DEVICES)


def sync_wireless_runtime():
    """Reflect committed wifi-iface sections in LuCI/hostapd runtime state."""
    per_radio = {name: [] for name in WIRELESS_DEVICES}
    for section, values in committed.get("wireless", {}).items():
        if values.get(".type") != "wifi-iface" or values.get("disabled") == "1":
            continue
        radio = values.get("device")
        if radio in per_radio:
            per_radio[radio].append((section, values))

    for radio_name, radio in WIRELESS_DEVICES.items():
        base = INITIAL_WIRELESS_DEVICES[radio_name]
        base_iface = base["interfaces"][0]
        radio_index = radio_name.removeprefix("radio")
        interfaces = []
        for index, (section, values) in enumerate(per_radio[radio_name]):
            iface = copy.deepcopy(base_iface)
            ifname = f"wlan{radio_index}" + (f"-{index}" if index else "")
            iface["ifname"] = ifname
            iface["section"] = section
            iface["config"].update({
                "network": values.get("network", "").split(),
                "device": [radio_name],
                "mode": values.get("mode", "ap"),
                "encryption": values.get("encryption", "none"),
                "ssid": values.get("ssid", ""),
            })
            if "key" in values:
                iface["config"]["key"] = values["key"]
            else:
                iface["config"].pop("key", None)
            iface["iwinfo"]["ssid"] = values.get("ssid", "")
            interfaces.append(iface)
        radio["interfaces"] = interfaces


def wireless_runtime_interface(ifname):
    for radio in WIRELESS_DEVICES.values():
        for iface in radio.get("interfaces", []):
            if iface.get("ifname") == ifname:
                return iface
    return None


def reset_fixture(sid):
    """Restore mutable router state while keeping the caller authenticated."""
    global info_calls, survey_calls, nft_runtime_mode, nft_runtime_snapshot
    global nft_lag_reads
    with lock:
        sessions.intersection_update({sid})
        committed.clear()
        committed.update(copy.deepcopy(INITIAL_COMMITTED))
        staged.clear()
        dirty_configs.clear()
        written_files.clear()
        reject_logins.clear()
        acl_gaps.clear()
        rollback.clear()
        NR_LISTS.clear()
        WIRELESS_DEVICES.clear()
        WIRELESS_DEVICES.update(copy.deepcopy(INITIAL_WIRELESS_DEVICES))
        board.clear()
        board.update(copy.deepcopy(DEFAULT_BOARD))
        board_network.clear()
        board_network.update(copy.deepcopy(DEFAULT_BOARD_NETWORK))
        network_devices.clear()
        network_devices.update(copy.deepcopy(DEFAULT_NETWORK_DEVICES))
        info_calls = 0
        survey_calls = 0
        nft_runtime_mode = "live"
        nft_runtime_snapshot = ""
        nft_lag_reads = 0

OBJECTS = {
    "session": {"login": {}, "list": {}, "destroy": {}, "access": {}},
    "uci": {m: {} for m in ("configs", "get", "set", "add", "delete",
                            "changes", "revert", "commit", "apply",
                            "confirm", "rollback")},
    "system": {"board": {}, "info": {}, "reboot": {}},
    "service": {"list": {}},
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
    #
    # One entry per BSS and no duplicates. This dict had two `hostapd.wlan0`
    # keys and two `hostapd.wlan1` keys, so the later pair silently replaced the
    # earlier and the mock advertised a hostapd with no `get_status` at all —
    # invisible, because the dispatcher answers hostapd.* before consulting
    # this table, while `ubus list` (which is what discovery and the capability
    # probe fingerprint on) read the truncated version.
    "hostapd.wlan0": {m: {} for m in HOSTAPD_METHODS},
    "hostapd.wlan1": {m: {} for m in HOSTAPD_METHODS},
    "luci-rpc": {m: {} for m in ("getNetworkDevices", "getWirelessDevices",
                                 "getHostHints", "getDHCPLeases",
                                 "getBoardJSON")},
}

WHICH = {"iw": "/usr/sbin/iw", "iwinfo": "/usr/bin/iwinfo",
         "df": "/bin/df", "ip": "/sbin/ip", "nft": "/usr/sbin/nft",
         "opkg": "/bin/opkg", "apk": "/usr/bin/apk",
         "ethtool": "/usr/sbin/ethtool",
         "bridge": "/usr/sbin/bridge", "brctl": "/usr/sbin/brctl"}

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

# The reference device runs apk, not opkg — opkg exits 4 (not found) there — and
# apk glues the version onto the name with a hyphen. Both matter: the capability
# probe reads this list to decide 802.11s mesh support, since nothing else can
# answer it (iwinfo reports PHY modes, not interface modes, and `iw phy` is not
# in the ACL). wpad-mesh-openssl is what the reference device actually reports.
APK_INSTALLED = """rpcd-2024.09.01-r1 arm_cortex-a9_vfpv3-d16 {feeds/base} (ISC) [installed]
rpcd-mod-file-2024.09.01-r1 arm_cortex-a9_vfpv3-d16 {feeds/base} (ISC) [installed]
rpcd-mod-iwinfo-2024.09.01-r1 arm_cortex-a9_vfpv3-d16 {feeds/base} (ISC) [installed]
rpcd-mod-luci-24.1-r1 arm_cortex-a9_vfpv3-d16 {feeds/luci} (Apache-2.0) [installed]
uhttpd-2024.10.1-r1 arm_cortex-a9_vfpv3-d16 {feeds/base} (ISC) [installed]
uhttpd-mod-ubus-2024.10.1-r1 arm_cortex-a9_vfpv3-d16 {feeds/base} (ISC) [installed]
firewall4-2024.10.1-r1 arm_cortex-a9_vfpv3-d16 {feeds/base} (ISC) [installed]
nftables-1.0.9-r1 arm_cortex-a9_vfpv3-d16 {feeds/base} (ISC) [installed]
hostapd-common-2025.08.26~ca266cc2-r2 arm_cortex-a9_vfpv3-d16 {feeds/base} (BSD-3-Clause) [installed]
wpad-mesh-openssl-2025.08.26~ca266cc2-r2 arm_cortex-a9_vfpv3-d16 {feeds/base} (BSD-3-Clause) [installed]"""

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


def nft_enabled(section):
    return str(section.get("enabled", "1")).strip().lower() not in (
        "0", "false", "no", "off")


def nft_items(value):
    if isinstance(value, list):
        return [str(item) for item in value]
    return str(value or "").replace(",", " ").split()


def nft_values(value):
    return [item.lower() for item in nft_items(value)]


def nft_chain(name, hook=None, policy=None):
    chain = {"family": "inet", "table": "fw4", "name": name}
    if hook:
        chain.update({"type": "filter", "hook": hook, "prio": 0})
    if policy:
        chain["policy"] = policy
    return {"chain": chain}


def nft_rule(chain, expr, comment=""):
    rule = {"family": "inet", "table": "fw4", "chain": chain,
            "expr": expr}
    if comment:
        rule["comment"] = comment
    return {"rule": rule}


def nft_jump(target):
    return {"jump": {"target": target}}


def nft_match(left, right, op="=="):
    return {"match": {"op": op, "left": left, "right": right}}


def render_nft_ruleset():
    """Small fw4-shaped runtime derived from committed firewall UCI state."""
    firewall = committed.get("firewall", {})
    zones = {}
    forwardings = []
    rules = []
    for _, section in sorted(firewall.items()):
        if not nft_enabled(section):
            continue
        section_type = section.get(".type")
        if section_type == "zone" and section.get("name"):
            zones[str(section["name"])] = section
        elif section_type == "forwarding" and section.get("src") \
                and section.get("dest"):
            forwardings.append((str(section["src"]), str(section["dest"])))
        elif section_type == "rule" and section.get("src") \
                and str(section.get("target", "")).upper() == "ACCEPT":
            rules.append(section)

    records = [
        {"metainfo": {"version": "1.1.6"}},
        {"table": {"family": "inet", "name": "fw4"}},
        nft_chain("input", "input", "drop"),
        nft_chain("forward", "forward", "drop"),
        nft_chain("handle_reject"),
        nft_rule("handle_reject", [{"reject": None}],
                 "!fw4: Reject any other traffic"),
    ]

    network = committed.get("network", {})

    def zone_devices(zone):
        devices = []
        for interface in nft_items(zone.get("network")):
            device = network.get(interface, {}).get("device")
            if device:
                devices.append(str(device))
        return sorted(set(devices))

    def interface_match(devices):
        return devices[0] if len(devices) == 1 else {"set": devices}

    destinations = {"wan"}
    destinations.update(zones)
    destinations.update(dest for _, dest in forwardings)
    for dest in sorted(destinations):
        records.append(nft_chain("accept_to_" + dest))
        devices = zone_devices(zones[dest]) if dest in zones else [
            str(network.get("wan", {}).get("device") or "wan")]
        records.append(nft_rule("accept_to_" + dest, [
            nft_match({"meta": {"key": "oifname"}},
                      interface_match(devices),
                      "==" if len(devices) == 1 else "in"),
            {"accept": None},
        ],
                                "!fw4: Accept traffic towards " + dest))

    for source in sorted(zones):
        devices = zone_devices(zones[source])
        records.extend((nft_chain("input_" + source),
                        nft_chain("forward_" + source),
                        nft_chain("reject_from_" + source),
                        nft_chain("reject_to_" + source)))
        records.append(nft_rule("input", [
            nft_match({"meta": {"key": "iifname"}},
                      interface_match(devices),
                      "==" if len(devices) == 1 else "in"),
            nft_jump("input_" + source),
        ], "!fw4: Handle " + source + " input traffic"))
        records.append(nft_rule("forward", [
            nft_match({"meta": {"key": "iifname"}},
                      interface_match(devices),
                      "==" if len(devices) == 1 else "in"),
            nft_jump("forward_" + source),
        ], "!fw4: Handle " + source + " forward traffic"))
        for src, dest in sorted(forwardings):
            if src == source:
                records.append(nft_rule("forward_" + source,
                                        [nft_jump("accept_to_" + dest)],
                                        "!fw4: Accept " + source + " to " +
                                        dest + " forwarding"))
        records.append(nft_rule("forward_" + source,
                                [nft_jump("reject_to_" + source)]))
        records.append(nft_rule("reject_from_" + source, [
            nft_match({"meta": {"key": "iifname"}},
                      interface_match(devices),
                      "==" if len(devices) == 1 else "in"),
            nft_jump("handle_reject"),
        ], "!fw4: Reject " + source + " input traffic"))
        records.append(nft_rule("reject_to_" + source,
                                [nft_match({"meta": {"key": "oifname"}},
                                           interface_match(devices),
                                           "==" if len(devices) == 1 else "in"),
                                 nft_jump("handle_reject")],
                                "!fw4: Reject " + source + " output traffic"))

    for section in rules:
        source = str(section["src"])
        if source not in zones:
            continue
        protocols = nft_values(section.get("proto"))
        for protocol in protocols:
            expr = []
            if str(section.get("family", "")).lower() == "ipv4":
                expr.append(nft_match({"meta": {"key": "nfproto"}}, "ipv4"))
            expr.append(nft_match({"meta": {"key": "l4proto"}}, protocol))
            if section.get("src_port"):
                expr.append(nft_match({"payload": {
                    "protocol": protocol, "field": "sport"}},
                    int(section["src_port"])))
            if section.get("dest_port"):
                expr.append(nft_match({"payload": {
                    "protocol": protocol, "field": "dport"}},
                    int(section["dest_port"])))
            expr.append({"accept": None})
            records.append(nft_rule("input_" + source, expr,
                                    "!fw4: " + str(section.get("name", ""))))

    for source in sorted(zones):
        records.append(nft_rule("input_" + source,
                                [nft_jump("reject_from_" + source)]))

    return json.dumps({"nftables": records}, separators=(",", ":"))


def exec_cmd(rid, cmd, params):
    global nft_lag_reads
    def out(code, stdout):
        return ok(rid, {"code": code, "stdout": stdout, "stderr": ""})
    # rpcd resolves a command to its ABSOLUTE PATH before matching the ACL, so
    # callers legitimately pass either form. Compare on the basename.
    cmd = (cmd or "").rsplit("/", 1)[-1]
    if cmd == "nft":
        if nft_runtime_mode == "nonzero":
            return out(1, "")
        if nft_runtime_mode == "malformed":
            return out(0, "{")
        if nft_runtime_mode == "empty":
            return out(0, '{"nftables":[{"metainfo":{"version":"1.1.6"}}]}')
        if nft_runtime_mode == "stale" or \
                nft_runtime_mode == "lag" and nft_lag_reads > 0:
            if nft_runtime_mode == "lag":
                nft_lag_reads -= 1
            return out(0, nft_runtime_snapshot)
        return out(0, render_nft_ruleset())
    if cmd == "brctl" and len(params) == 2 and params[0] == "showmacs":
        return out(0, "port no mac addr is local? ageing timer\n"
                      "  1 aa:bb:cc:11:22:33 no 12.34\n")
    if cmd == "nlbw":
        return out(0, '{"columns":["mac","conns","rx_bytes","rx_pkts",'
                      '"tx_bytes","tx_pkts"],"data":['
                      '["aa:bb:cc:11:22:33",4,25488470,3412,304176,4173]]}')
    if cmd == "true":
        return out(0, "")
    if cmd == "which":
        p = WHICH.get(params[0] if params else "")
        return out(0 if p else 1, (p + "\n") if p else "")
    if cmd == "apk":
        return out(0, APK_INSTALLED + "\n")
    if cmd == "opkg":
        # The reference device does not have opkg at all — it exits 4. Kept
        # answering here so the older-format parsing path stays exercised.
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
        if params != ["-q", "-c", "3", "-W", "1", "1.1.1.1"]:
            return out(2, "")
        return out(0, "PING 1.1.1.1 (1.1.1.1): 56 data bytes\n\n"
                      "--- 1.1.1.1 ping statistics ---\n"
                      "3 packets transmitted, 3 packets received, "
                      "0% packet loss\n"
                      "round-trip min/avg/max = 11.500/12.000/12.500 ms\n")
    return out(127, "")


DNSMASQ_RUNTIME_CONFIG = "/var/etc/dnsmasq.conf.cfg01411c"


def dnsmasq_runtime_config():
    """Render the active ranges dnsmasq builds from committed UCI state.

    Health checks run after uci.apply while rollback is armed. Reading staged
    UCI here would reproduce the exact false proof the controller forbids, so
    this derives only from committed runtime state.
    """
    lines = []
    with lock:
        networks = committed.get("network", {})
        for _, pool in sorted(committed.get("dhcp", {}).items()):
            if pool.get(".type") != "dhcp" or pool.get("ignore") == "1":
                continue
            iface = pool.get("interface")
            network = networks.get(iface, {})
            if network.get(".type") != "interface":
                continue
            try:
                subnet = ipaddress.IPv4Network(
                    f'{network["ipaddr"]}/{network["netmask"]}', strict=False)
                start = int(pool.get("start", "100"))
                limit = int(pool.get("limit", "150"))
                first = subnet.network_address + start
                last = first + limit - 1
                if limit < 1 or last >= subnet.broadcast_address:
                    continue
            except (KeyError, ValueError, ipaddress.AddressValueError):
                continue
            lines.append(
                f"dhcp-range=set:{iface},{first},{last},{subnet.netmask},"
                f'{pool.get("leasetime", "12h")}')
    return "\n".join(lines) + ("\n" if lines else "")


def runtime_interfaces():
    out = []
    with lock:
        for name, section in sorted(committed.get("network", {}).items()):
            if section.get(".type") != "interface":
                continue
            row = {"interface": name, "up": True,
                   "proto": section.get("proto", "")}
            if section.get("device"):
                row["device"] = section["device"]
            if section.get("ipaddr") and section.get("netmask"):
                try:
                    prefix = ipaddress.IPv4Network(
                        f'{section["ipaddr"]}/{section["netmask"]}',
                        strict=False).prefixlen
                    row["ipv4-address"] = [{
                        "address": section["ipaddr"], "mask": prefix,
                    }]
                except (ValueError, ipaddress.AddressValueError):
                    row["ipv4-address"] = []
            out.append(row)
    return out


def handle_one(req):
    rid = req.get("id")
    method = req.get("method")
    p = req.get("params", [])

    if method == "list":
        # `list` needs NO session, and that is the whole reason discovery uses
        # it: no credential, no session, no failed-login record, and it returns
        # the object graph, which is a far stronger fingerprint than anything a
        # login attempt yields.
        #
        # This used to answer `{}` unless the first parameter was 32 characters
        # long — i.e. unless it looked like a session token — which is backwards
        # from the device. Discovery sends `params: ["*"]`, so against this mock
        # it saw an empty object list and graded a perfectly good OpenWrt box as
        # merely "reachable". Nothing caught it because discovery's own tests
        # use their own fixture; it surfaced when a daemon test asked discovery
        # to identify the mock.
        return {"jsonrpc": "2.0", "id": rid, "result": OBJECTS}
    if method != "call":
        return {"jsonrpc": "2.0", "id": rid,
                "error": {"code": -32601, "message": "unknown method"}}

    sess, obj, meth, args = (list(p) + [{}] * 4)[:4]
    args = args or {}

    # uhttpd's HTTP ubus bridge owns this reserved field: it injects the
    # session from params[0] and rejects callers that also put it in args.
    # Mirror that boundary so direct-ubus-shaped requests fail in tests exactly
    # as they do on stock OpenWrt.
    if "ubus_rpc_session" in args:
        return {"jsonrpc": "2.0", "id": rid,
                "error": {"code": -32602, "message": "invalid parameters"}}

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
        target = (args.get("object"), args.get("function"))
        allowed = target not in acl_gaps and (target[0], "*") not in acl_gaps
        return ok(rid, {"access": allowed})
    if obj == "session" and meth == "list":
        return ok(rid, {"ubus_rpc_session": sess, "timeout": 300, "expires": 300})

    if obj == "system" and meth == "board":
        return ok(rid, copy.deepcopy(board))
    if obj == "system" and meth == "info":
        return ok(rid, {"uptime": 400000, "load": [8000, 9000, 8500],
                        "memory": {"total": 536870912, "free": 340000000,
                                   "buffered": 20000000, "cached": 60000000}})

    if obj == "file":
        if meth == "exec":
            return exec_cmd(rid, args.get("command"), args.get("params", []))
        if meth == "read":
            requested_path = args.get("path", "")
            if requested_path == "/proc/stat":
                t = int(time.time() * 100)
                return ok(rid, {"data": f"cpu  {t % 100000} 0 {t % 50000} "
                                        f"{t % 900000} 0 0 0 0\n"})
            if requested_path == DNSMASQ_RUNTIME_CONFIG:
                return ok(rid, {"data": dnsmasq_runtime_config()})
            prefix, suffix = "/sys/class/net/", "/brport/isolated"
            if requested_path.startswith(prefix) and requested_path.endswith(suffix):
                ifname = requested_path[len(prefix):-len(suffix)]
                iface = wireless_runtime_interface(ifname)
                if iface is None:
                    return err(rid, 4)
                section = iface.get("section")
                value = committed.get("wireless", {}).get(section, {}).get(
                    "bridge_isolate", "0")
                return ok(rid, {"data": value + "\n"})
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
    if obj == "__test" and meth == "reset":
        reset_fixture(sess)
        return ok(rid, {})
    if obj == "__test" and meth == "set_acl_gap":
        acl_gaps.clear()
        for pair in args.get("pairs") or []:
            acl_gaps.add((pair.get("object"), pair.get("method", "*")))
        return ok(rid, {"gaps": sorted(f"{o}.{m}" for o, m in acl_gaps)})
    if obj == "__test" and meth == "set_board":
        values = args.get("board")
        network = args.get("network")
        devices = args.get("network_devices")
        if not all(isinstance(value, dict) for value in (values, network, devices)):
            return err(rid, 2)
        with lock:
            board.clear()
            board.update(copy.deepcopy(values))
            board_network.clear()
            board_network.update(copy.deepcopy(network))
            network_devices.clear()
            network_devices.update(copy.deepcopy(devices))
        return ok(rid, {})
    if obj == "__test" and meth == "set_nft_runtime":
        global nft_runtime_mode, nft_runtime_snapshot, nft_lag_reads
        mode = args.get("mode") or "live"
        if mode not in ("live", "stale", "lag", "malformed", "nonzero", "empty"):
            return err(rid, 2)
        nft_runtime_mode = mode
        nft_runtime_snapshot = render_nft_ruleset()
        nft_lag_reads = max(0, int(args.get("reads") or 0))
        return ok(rid, {"mode": mode, "lag_reads": nft_lag_reads})
    if obj == "__test" and meth == "reject_login":
        # Stands in for anything that leaves a freshly written login unusable —
        # a botched hash, a config that did not land. Adoption must notice.
        reject_logins.clear()
        reject_logins.update(args.get("usernames") or [])
        return ok(rid, {"rejecting": sorted(reject_logins)})
    if obj == "__test" and meth == "written":
        return ok(rid, {"paths": sorted(written_files),
                        "content": written_files.get(args.get("path"), "")})
    if obj == "__test" and meth == "add_wifi_iface":
        # Add an interface to a radio, so a test can exercise a mesh point or a
        # 4-address station without a second real router. Pass ifname="" to
        # model a section whose interface the driver never created — the §5q
        # signature, which is otherwise unreproducible without mwlwifi.
        radio = args.get("radio") or "radio0"
        entry = {"config": {"mode": args.get("mode") or "ap"}}
        if args.get("ifname"):
            entry["ifname"] = args["ifname"]
        if args.get("section"):
            entry["section"] = args["section"]
        with lock:
            WIRELESS_DEVICES.setdefault(radio, {"interfaces": []})
            WIRELESS_DEVICES[radio]["interfaces"].append(entry)
        return ok(rid, {"interfaces": len(WIRELESS_DEVICES[radio]["interfaces"])})
    if obj == "__test" and meth == "switch_off_radio":
        # A radio switched OFF, which is a different state from a radio with no
        # interfaces and was not expressible here before.
        #
        # The distinction is the whole point: with no interfaces the radio is
        # merely idle and a WLAN applied to it comes up. Switched off, the same
        # WLAN writes a correct section, applies successfully, and broadcasts
        # nothing — and the health check then fails looking for an SSID that was
        # never going to appear.
        #
        # Both spellings are set because the device reports both, and they are
        # not the same field: `disabled` on the radio object is what luci-rpc
        # answers, while `wireless.<radio>.disabled` in uci is what the renderer
        # reads. A fixture that set only one would let a bug through whichever
        # side it was on.
        name = args.get("radio", "radio0")
        with lock:
            if name in WIRELESS_DEVICES:
                WIRELESS_DEVICES[name]["disabled"] = True
                WIRELESS_DEVICES[name]["up"] = False
                WIRELESS_DEVICES[name]["config"]["disabled"] = True
            committed.setdefault("wireless", {}).setdefault(name, {})["disabled"] = "1"
        return ok(rid, {"radio": name, "disabled": True})
    if obj == "__test" and meth == "disable_radios":
        # Strip every interface from every radio, leaving the radios
        # themselves — which is exactly how stock OpenWrt ships: the radios
        # exist and are disabled, so nothing is broadcasting.
        #
        # This is the shape that made the controller unable to bring a fresh
        # router into service at all: iwinfo.devices returns [] because it
        # enumerates INTERFACES, the probe recorded zero radios, and the
        # renderer then refused to give the device a WLAN it could never
        # otherwise obtain. Reproducible here so that never regresses.
        with lock:
            for radio in WIRELESS_DEVICES.values():
                radio["interfaces"] = []
        return ok(rid, {"radios": len(WIRELESS_DEVICES), "interfaces": 0})
    if obj == "__test" and meth == "set_dirty":
        dirty_configs.clear()
        dirty_configs.update(args.get("configs") or [])
        return ok(rid, {"dirty": sorted(dirty_configs)})

    if obj == "iwinfo":
        if meth == "devices":
            # Derived from WIRELESS_DEVICES rather than hardcoded, because the
            # whole point of the disable_radios hook is that this list goes
            # empty while the radios remain. A constant here would make the
            # fixture unable to express the state that broke adoption.
            with lock:
                names = [i["ifname"] for r in WIRELESS_DEVICES.values()
                         for i in r.get("interfaces", []) if i.get("ifname")]
            return ok(rid, {"devices": names})
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
        if meth == "scan":
            g5 = dev == "wlan0"
            return ok(rid, {"results": [{
                "bssid": "02:11:22:33:44:55" if g5 else "02:11:22:33:44:66",
                "ssid": "neighbour-5g" if g5 else "neighbour-2g",
                "channel": 36 if g5 else 6,
                "mhz": 5180 if g5 else 2437,
                "signal": -61,
            }]})
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

    if obj == "service" and meth == "list":
        if args.get("name") not in (None, "", "dnsmasq"):
            return ok(rid, {})
        return ok(rid, {"dnsmasq": {"instances": {"cfg01411c": {
            "running": True,
            "pid": 321,
            "command": ["/usr/sbin/dnsmasq", "-C",
                        DNSMASQ_RUNTIME_CONFIG, "-k"],
            "mount": {DNSMASQ_RUNTIME_CONFIG: DNSMASQ_RUNTIME_CONFIG},
        }}}})

    if obj == "network.interface" and meth == "dump":
        interfaces = runtime_interfaces()
        for row in interfaces:
            if row["interface"] == "wan":
                row["ipv4-address"] = [{"address": "203.0.113.7", "mask": 24}]
        return ok(rid, {"interface": interfaces})
    if obj == "network.device" and meth == "status":
        return ok(rid, {"br-lan": {"up": True, "carrier": True, "mtu": 1500,
                                   "macaddr": "02:00:00:aa:bb:cc",
                                   "statistics": {"rx_bytes": 123456789,
                                                  "tx_bytes": 987654321}}})
    if obj.startswith("hostapd."):
        ifname = obj.removeprefix("hostapd.")
        iface = wireless_runtime_interface(ifname)
        if iface is None:
            return err(rid, 4)
        iw = iface.get("iwinfo", {})
        g5 = iw.get("frequency", 0) >= 5000
        if meth == "get_status":
            # `utilization` is the 802.11 BSS-Load 0-255 scale, NOT a percent —
            # 172 is ~67%. Anything rendering it as a percentage is wrong, so
            # the fixture reports it the way hardware does.
            return ok(rid, {"phy": iw.get("phy", "phy0" if g5 else "phy1"),
                            "ssid": iface.get("config", {}).get("ssid", ""),
                            "bssid": iw.get("bssid", "02:00:00:ab:24:42"),
                            "channel": iw.get("channel", 36 if g5 else 6),
                            "freq": iw.get("frequency", 5180 if g5 else 2437),
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
        # 802.11k neighbour reports.
        #
        # The response shape is measured; identifiers are stable synthetic
        # fixtures. mwlwifi and ath9k/ath10k both answer this way.
        # `rrm_nr_get_own` returns a POSITIONAL triple, not an object, and the
        # element is opaque hex the controller relays untouched.
        #
        # `rrm_nr_list` returns entries in hostapd's own storage order, which is
        # neither insertion order nor sorted — so it is deliberately shuffled
        # here. A consumer that compares lists order-sensitively converges
        # never, and against an ordered fixture it would look fine.
        if meth == "rrm_nr_get_own":
            with lock:
                return ok(rid, {"value": [
                    NR_OWN[obj]["bssid"], NR_OWN[obj]["ssid"],
                    NR_OWN[obj]["nr"]]})
        if meth == "rrm_nr_list":
            with lock:
                return ok(rid, {"list": list(reversed(NR_LISTS.get(obj, [])))})
        if meth == "rrm_nr_set":
            entries = (args or {}).get("list")
            if not isinstance(entries, list):
                return err(rid, 2)
            for e in entries:
                if not isinstance(e, list) or len(e) != 3:
                    return err(rid, 2)
            with lock:
                NR_LISTS[obj] = [list(e) for e in entries]
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
        if meth == "getBoardJSON":
            return ok(rid, {"network": copy.deepcopy(board_network)})
        if meth == "getHostHints":
            return ok(rid, {"AA:BB:CC:11:22:33":
                            {"ipaddrs": ["192.0.2.130"],
                             "name": "example-laptop"},
                            "AA:BB:CC:44:55:66":
                            {"ipaddrs": ["192.0.2.131"], "name": "iot-plug"}})
        if meth == "getNetworkDevices":
            # DSA user ports carry devtype "dsa" with the conduit as parent —
            # this is how the controller detects a switch without any
            # filesystem grant.
            return ok(rid, copy.deepcopy(network_devices))
        if meth == "getWirelessDevices":
            with lock:
                return ok(rid, copy.deepcopy(WIRELESS_DEVICES))
        if meth == "getDHCPLeases":
            return ok(rid, {"dhcp_leases": [
                {"macaddr": "AA:BB:CC:11:22:33", "ipaddr": "192.0.2.130",
                 "hostname": "example-laptop", "expires": 30000}]})
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
