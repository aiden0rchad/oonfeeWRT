#!/bin/zsh
# Watch the WRT3200ACM for the wedge described in STATUS §0.
#
# The signature is specific on purpose. An earlier version counted any
# daemon.err as a fault and fired on a successful 802.11r roam: mwlwifi logs
# "key addition failed" and an STA_VLAN -2 during FT association, then the
# client associates and passes traffic normally. A detector that reports that
# as a fault will be ignored on the day the real one happens.
#
# The real wedge, caught directly on 2026-08-16:
#   STA ... deauthenticated due to inactivity
#   ~66s later: nl80211_recv_beacons->nl_recvmsgs failed: -5
# and hostapd enters uninterruptible sleep. Only sysrq recovers it.
LOG="$1"; HOST=192.168.1.1
# A function, not a string. zsh does not word-split an unquoted parameter, so
# "$S ..." ran the whole ssh command line as a single command name, every probe
# came back empty, and the monitor reported the router UNREACHABLE while it was
# answering fine. A false "down" is the same failure as a false fault.
S() { ssh -o StrictHostKeyChecking=no -o ConnectTimeout=8 -o BatchMode=yes root@$HOST "$@"; }
echo "started $(date +%H:%M:%S)  watching for nl_recvmsgs -5 / hostapd D-state" > "$LOG"
while true; do
  OUT=$(S 'UP=$(cut -d. -f1 /proc/uptime)
    # MEMAddrAccess is NOT the cause. mwl_heartbeat_handle() calls
    # mwl_fwcmd_get_addr_value(), which issues HOSTCMD_CMD_MEM_ADDR_ACCESS
    # (0x001d), and the wait is on the response 0x8000|cmd = 0x801d. So a
    # repeating 0x801d is the heartbeat probe of the driver ITSELF failing — the
    # watchdog confirming the firmware is already dead, not the thing that
    # killed it. It is still the most reliable confirmation, so it is counted.
    #
    # Note the heartbeat is optional (priv->heartbeat must be non-zero), so an
    # ABSENCE of 0x801d does not prove the firmware is alive.
    WEDGE=$(logread | grep -c "nl80211_recv_beacons->nl_recvmsgs failed: -5")
    FW=$(logread | grep -c "MEMAddrAccess timed out")
    # The genuine EARLY warning: a key-install or station-add command failing.
    # Measured on this box, twice — a "key addition failed" preceded the
    # firmware flood by 85 seconds in one wedge and by 10.5 minutes in the
    # other, both on the same client during an 802.11r association. Four
    # independent bug reports show the same ordering. This is the only signal
    # that arrives while the radio is still working.
    EARLY=$(logread | grep -cE "key addition failed|failed to (remove|set) key|0x9122=UpdateEncryption|0x9111=SetNewStation")
    # Field 4, not 3. busybox `ps w` is PID USER VSZ STAT COMMAND, so matching
    # on $3 tested the VSZ column and reported hostapd_D=0 while hostapd sat in
    # uninterruptible sleep. A watchdog reading the wrong column is a watchdog
    # that reports healthy through the failure it exists to catch.
    #
    # Sampled five times over five seconds, and only a hostapd in D for EVERY
    # sample counts. D is a normal, momentary state for any process in a
    # blocking syscall, so one sample proves nothing — an earlier version fired
    # on a device that went on to serve traffic for another two minutes. A
    # wedged hostapd never leaves D.
    DSTATE=1
    for _ in 1 2 3 4 5; do
      ps w | grep "[h]ostapd" | awk "\$4 ~ /D/" | grep -q . || DSTATE=0
      sleep 1
    done
    ALIVE=$(pgrep hostapd >/dev/null && echo yes || echo no)
    echo "$UP|$WEDGE|$DSTATE|$ALIVE|$FW|$EARLY"' 2>/dev/null)
  if [ -z "$OUT" ]; then
    echo "$(date +%H:%M:%S) UNREACHABLE" >> "$LOG"
  else
    UP=${OUT%%|*}; R=${OUT#*|}; W=${R%%|*}; R=${R#*|}; D=${R%%|*}; R=${R#*|}
    A=${R%%|*}; R=${R#*|}; F=${R%%|*}; E=${R#*|}
    echo "$(date +%H:%M:%S) up=${UP}s EARLY_key_fail=$E fw_heartbeat=$F netlink_err=$W hostapd_D=$D alive=$A" >> "$LOG"
    # The early signal is reported loudly but does NOT stop the watch: it
    # arrives while the radio still works, and the interesting question is how
    # long it has left.
    [ "$E" != "0" ] && echo "  ^ EARLY WARNING: key/station command failure — a wedge has followed this twice" >> "$LOG"
    if [ "$W" != "0" ] || [ "$D" != "0" ] || [ "$F" != "0" ]; then
      { echo "--- WEDGE $(date +%H:%M:%S) ---"
        S 'echo "== D-state =="; ps w | awk "\$3 ~ /D/"; echo "== last 40 wireless =="; logread | grep -iE "hostapd|mwlwifi|nl80211" | tail -40'
      } >> "$LOG" 2>&1
      echo "captured; stopping so the evidence is not overwritten" >> "$LOG"
      exit 0
    fi
  fi
  sleep 180
done
