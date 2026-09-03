# oonfeeWRT v0.1.4

v0.1.4 adds explicit per-network IPv6 controls, improves router-log and clock
diagnostics, fixes transitive wired-topology presentation, and lets Docker
Compose publish on a chosen management address. It also reduces controller work
on unchanged topology data and updates the SSH dependency for two denial-of-
service fixes.

oonfeeWRT manages stock OpenWrt through existing SSH/ubus interfaces. It is not
firmware and installs no controller-authored executable on a router.

Released September 3, 2026. The stable `v0.1.4` tag workflow published the
release archives and OCI image. Verify downloaded archives with `SHA256SUMS`;
the published OCI image also carries a keyless signature, SBOM, and provenance
attestation.

## Highlights

- Networks now have an explicit IPv6 policy: **Router managed** preserves the
  router's current settings, **Prefix delegation** configures a `/48`–`/64` LAN
  assignment, and **Disabled** stops controller-managed RA, DHCPv6, NDP, and
  delegated assignment. Existing networks migrate to Router managed and
  receive no IPv6 write merely by upgrading.
- IPv6 changes still use Preview and Apply. Management-LAN support is limited
  to allowlisted option changes on the exact LAN and DHCP sections and, when
  present in supported forms, conventional `wan`/`wan6` controls. Disable sets
  `network.wan.ipv6=0` and a DHCPv6 `network.wan6.auto=0`; Prefix delegation
  coordinates a supported PPP parent and DHCPv6 client. Static IPv6 values on
  these targets block Disable instead of being silently removed. The
  controller never creates, claims, renames, or deletes these foreign sections.
- Building on the exact-warning compaction shipped in v0.1.1, the Logs UI now
  receives current condition state independently of the selected event filters
  and page. It names each affected router, shows the retained occurrence count,
  and links to that router's primary-network IPv6 setting. Verified quiet log
  coverage clears the current banner without deleting retained history;
  compaction still does not suppress or delete messages in the router's own log.
- Router event-time diagnostics can read UTC through `luci.getUnixtime`, with
  `luci.getLocaltime` as a compatibility fallback. The read is observation
  only. Already-adopted routers need a separately reviewed controller-access
  refresh before clock status becomes available.
- Wired topology no longer presents a managed device as attached directly to
  multiple upstream devices when fresh FDB/LLDP evidence proves a transitive
  path. The live projection follows a fresh multi-hop path while raw intervals
  remain in history; stale, failed, or ambiguous sources become visible again
  instead of being treated as proof.
- FDB-only clean paths are labeled as inferred. Missing BusyBox VLAN provenance
  is neutral unavailable metadata rather than a warning on every edge.
- A partial-source observation of the same link geometry now retains the prior
  semantic payload instead of splitting a new history interval. Closed-history
  lookup is indexed, and the browser avoids overlapping or abandoned topology
  requests. These changes reduce the CPU and database work reported in issue
  #20.
- Docker Compose accepts `OONFEE_HTTP_BIND=<controller-LAN-IP>` to publish on one
  deliberate host address. Loopback remains the default. `0.0.0.0` is an
  explicit, firewalled-network-only opt-in because the controller has no native
  TLS listener.
- `golang.org/x/crypto` is updated to v0.56.0, addressing reachable SSH denial-
  of-service advisories GO-2026-6354 and GO-2026-6355. The release toolchain is
  Go 1.26.6.

Issues [#20](https://github.com/aiden0rchad/oonfeeWRT/issues/20),
[#25](https://github.com/aiden0rchad/oonfeeWRT/issues/25), and
[#26](https://github.com/aiden0rchad/oonfeeWRT/issues/26) supplied field reports
that shaped these changes. The fixes have automated and candidate-image
coverage. At release time, final validation on each reporter's hardware
remained pending.

## IPv6 scope

IPv6 policy is selected per network, not a global IPv6 stack switch or a
multi-WAN manager. On the management LAN, that selected policy necessarily
coordinates conventional upstream `wan` and DHCPv6 `wan6` controls when those
exact, supported sections exist. Missing conventional WAN sections are valid
and remain absent; custom/static `wan6` protocols remain router-managed;
wrong-type targets block Preview rather than being guessed.

Prefix delegation still depends on a working ISP/upstream delegation, and a
live delegated prefix plus end-to-end IPv6 client path have not yet been proved
on release hardware. Explicit modes remove `ra_default`; the controller does
not invent an upstream IPv6 default route.

Controller-owned VLANs receive the matching netifd, odhcpd, DHCP, DNS, and
ICMPv6 policy. The existing management LAN uses only the narrower option-patch
path described above. A missing or ambiguous LAN/DHCP target and incompatible
present WAN targets block Preview; absent conventional WAN sections do not.

## Upgrade and rollback

Create and verify a portable backup before upgrading. For a direct rollback,
also stop v0.1.3 and retain its raw database/keyring pair or a consistent whole-
volume snapshot, plus the matching runtime passphrase. A portable backup can
instead be restored through a separate clean v0.1.3 controller; there is no
public command that extracts it into an old live data directory.

v0.1.4 migrates the controller database from schema 19 to schema 20. The
migration adds a closed-topology history index and normalizes old development-era
`luci.get*Devices` topology-source keys to their actual
`luci-rpc.get*Devices` names, retaining the newest duplicate observation. It
does not remove user configuration, credentials, secrets, or topology
intervals.

The v0.1.3 binary cannot open a schema-20 database. Rolling back to v0.1.3
therefore requires restoring the matching pre-upgrade schema-19 database,
keyring, and passphrase, or restoring the portable backup through clean v0.1.3
state. Replacing only the binary or image tag is not a valid rollback.

Ordinary upgrade startup does not configure routers. Read-only polling resumes,
and existing IPv6 settings remain untouched under the Router managed default.
Only operators who want router-clock status on an existing adoption need to
review and apply the updated controller-access payload; re-adoption is not
required.

See the bundled `INSTALL.md` (source: `docs/INSTALL.md`) for verified download,
container, backup, restore, signature, and rollback commands.

## Security and scope

- IPv6, adoption, Apply, RF scans, and optional capability installation retain
  their existing plan, acknowledgement, and audit boundaries.
- Explicit management-LAN IPv6 modes are the narrow exception to the normal
  owned-section write rule. They may patch only allowlisted options on exact
  existing LAN/DHCP and supported conventional `wan`/`wan6` sections, behind
  Preview and Apply; they cannot create, claim, rename, or delete those sections.
- The HTTP listener has no native TLS. The supplied Compose file publishes on
  host loopback unless `OONFEE_HTTP_BIND` explicitly selects another address;
  use a trusted reverse proxy before remote access.
- Custom policy routing, `mwan3`, multi-WAN health/failover, bonding, manual WAN
  selection, and gateway-run speed tests remain outside this release.
- No independent security audit or penetration test has been completed.
