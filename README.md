# oonfeeWRT

Self-hosted, UniFi-inspired management for stock OpenWrt.

[![Release](https://img.shields.io/github/v/release/aiden0rchad/oonfeeWRT)](https://github.com/aiden0rchad/oonfeeWRT/releases)
[![CI](https://github.com/aiden0rchad/oonfeeWRT/actions/workflows/ci.yml/badge.svg)](https://github.com/aiden0rchad/oonfeeWRT/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/aiden0rchad/oonfeeWRT)](LICENSE)
[![Documentation](https://img.shields.io/badge/docs-capabilities%20%26%20guides-2a78d6)](https://aiden0rchad.github.io/oonfeeWRT/)

**[Explore the complete documentation →](https://aiden0rchad.github.io/oonfeeWRT/)**
Capabilities, guided setup, safe configuration, operations, security,
troubleshooting, and engineering reference—with full-text search and light/dark
themes.

oonfeeWRT is a controller, not firmware. It runs on your server, NAS, mini-PC,
or Mac and manages OpenWrt devices through their existing interfaces. Your
routers stay on stock OpenWrt and continue to work with LuCI.

**Docker is optional.** Run the standalone binary directly on a supported
64-bit Linux or macOS host, or use the container/Compose setup. The controller
does not need a dedicated machine and is not installed on the managed routers.

## Current release: v0.1.5

Released September 10, 2026. [Read the complete release notes](docs/releases/v0.1.5.md).

- **Managed** and **Monitor only** adoption modes: one managed Gateway plus
  multiple reachable monitor-only routed devices and subnets.
- Named exact-MAC policy sets, reusable firewall `source_set_id` rules, a
  set-aware Master Table, and an Object Manager **Secure (IPv4)** draft workflow.
- Consistent page headers and responsive light/dark layouts across the main
  controller screens.
- A documented Phase 5 flow-visibility feasibility boundary. No DPI or flow
  package is installed or shipped.

## Preview

[![oonfeeWRT live dashboard showing Internet health, speed tests, and fleet status](docs/images/dashboard-overview.jpg)](docs/images/dashboard-overview.jpg)

*Live Internet health, controller-host speed tests, and fleet status.*

[![oonfeeWRT radio and channel planning dashboard](docs/images/radios-channel-plan.jpg)](docs/images/radios-channel-plan.jpg)

*Live radio inventory and evidence-aware channel planning.*

## What it provides

- A fleet dashboard with WAN reachability and throughput when the controller
  can prove one unique, usable lowest-metric main-table IPv4 default
  route—including PPPoE runtime devices—and the exact runtime device exists in
  RX/TX history, plus topology, clients, radios, events, and controller-host
  speed tests.
- Reviewed site configuration for networks, VLANs, DHCP, firewall zones, and
  WLANs, plus explicit per-network IPv6 **Router managed**, **Prefix
  delegation**, or **Disabled** policy, with OpenWrt's rollback timer protecting
  every Apply.
- Device adoption, health monitoring, telemetry, logs, RF tools, and explicit
  source-coverage gaps instead of guessed data.
- A monitor-only device mode for reachable routers that should contribute
  polling, inventory, telemetry, events, and topology without becoming Preview,
  Apply, or site-configuration targets.
- Reusable named policy sets of exact client MAC addresses, with safe CRUD,
  set-backed firewall sources, concrete resolution in the Master Table, and
  the normal redacted Preview/acknowledged Apply boundary.
- A sanitized, versioned compatibility-report download after read-only Inspect,
  with hardware/capability evidence but no address, MAC, credentials, network
  configuration, clients, timestamps, or free-text notes.
- Local owner, administrator, operator, and read-only accounts with session
  management and revocation.
- Downloadable, redacted diagnostics bundles containing controller evidence and
  stored router model, firmware, and capability data.
- Encrypted controller backup and staged restore with compatibility checks,
  controlled restart, and a persistent router-write gate.
- Optional LLDP using the official OpenWrt `lldpd` package, with an exact plan,
  separate consent, durable ownership records, and rollback.

## Project boundaries

oonfeeWRT does not build or replace OpenWrt, run controller-authored software on
routers, broker cloud access, or silently install packages.

Adoption can create only one scoped `oonfeewrt` login and one rpcd ACL JSON
file after you approve the displayed plan. Managed devices use the managed ACL
group; monitor-only devices use the distinct read-only `oonfeewrt-monitor` ACL
group. This bootstrap is required for polling, so monitor only means no
desired-configuration authority, not zero router writes during adoption. The
router administrator credential used for that one-time action is not stored.
Optional packages and configuration changes have separate review and consent
flows.

A site permits at most one managed Gateway. Additional reachable OpenWrt
routers may be adopted as monitor only, including across routed management
subnets, but they are excluded from Preview, Apply, desired/site configuration,
optional LLDP install/config/remove mutations, wireless-neighbor mutations, and
other package/config/remove operations. Existing LLDP observation, scoped ACL
maintenance, and un-adoption remain available.
oonfeeWRT does not create the routes or VPN needed to reach those devices.

Controller-created configuration is ownership-tagged, and ordinary Apply and
cleanup remain limited to those owned sections. The narrow exception is an
explicitly selected management-LAN IPv6 policy: after Preview, oonfeeWRT may
patch an allowlisted set of options on the exact existing LAN interface, its
matching DHCP section, and supported conventional `wan`/`wan6` sections. It
never claims, renames, creates, or deletes those foreign sections, and ambiguous
targets or conflicting static IPv6 values block Preview.

## Installation options and requirements

oonfeeWRT supports two equivalent ways to run the controller:

| Method | Supported controller hosts | Notes |
|---|---|---|
| Standalone binary | `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` | No Docker required; the UI is embedded in the binary |
| Container / Docker Compose | `linux/amd64`, `linux/arm64` images | Convenient for an existing NAS, mini-PC, SBC, or Docker Desktop host |

A controller host must be able to reach each router's management address over
SSH plus the selected HTTP or HTTPS `/ubus` endpoint. Remote sites need an
existing routed management network or VPN; oonfeeWRT does not provide cloud
brokering or automatic NAT traversal.

The documented minimum is OpenWrt 21.02 or newer with SSH, `rpcd`, `uhttpd`, and
its `/ubus` handler. OpenWrt 24.10 and 25.12 are the primary current
assumptions. Actual support is capability-driven; published end-to-end physical
evidence currently covers two routers on OpenWrt 25.12.5. Begin with one
non-critical device and review the detected capability gaps.

A 64-bit host with 1 GB of RAM and 2 GB of free storage is a practical starting
point. The controller's engineering envelope is at most 256 MB steady-state RSS
at 25 devices, 2% of one modern CPU core for an idle fleet, and 2 GB of disk at
the full 13-month retention depth.

oonfeeWRT is not memory-only. Raw telemetry is held in RAM temporarily;
completed rollups, configuration, accounts, events, and audit history are
stored in SQLite. Preserve the data directory and its matching `keyring.json`
and passphrase when running either installation method.

### Run the standalone binary

Download and checksum-verify the archive for your platform by following the
[binary installation guide](docs/INSTALL.md#install-the-binary), then run:

```sh
install -d -m 0700 "$PWD/data"
./oonfeewrtd -data-dir "$PWD/data" -listen 127.0.0.1:8080
```

The first interactive start asks you to create the controller passphrase. Open
[http://127.0.0.1:8080](http://127.0.0.1:8080) and create the first owner
account. For unattended startup, use `-passphrase-file` with a mode-`0600`
file as described in the installation guide.

### Run with Docker Compose

Requirements:

- Docker with Compose support.

Create a private working directory and download the release Compose file:

```sh
install -d -m 0700 oonfeewrt
cd oonfeewrt
umask 077

curl --fail --location \
  --output docker-compose.yml \
  https://raw.githubusercontent.com/aiden0rchad/oonfeeWRT/v0.1.5/deploy/docker-compose.yml

head -c 32 /dev/urandom | base64 > passphrase
sudo chown 65532:65532 passphrase
sudo chmod 600 passphrase

printf '%s\n' \
  'OONFEE_VERSION=v0.1.5' \
  'OONFEE_HTTP_BIND=127.0.0.1' > .env
chmod 600 .env
docker compose up -d
```

Open [http://127.0.0.1:8080](http://127.0.0.1:8080) and create the first owner
account. The default Compose configuration publishes HTTP only on host
loopback, runs as UID 65532, drops all capabilities, uses a read-only root
filesystem, and stores controller state in a named volume. It pulls
`ghcr.io/aiden0rchad/oonfeewrt:v0.1.5` for `linux/amd64` or `linux/arm64`.

The v0.1.5 Compose file also accepts a Compose-only host bind IP. When browsers
must connect from another machine, change `.env` to the controller's specific
management-LAN address, then recreate the service:

```dotenv
OONFEE_VERSION=v0.1.5
OONFEE_HTTP_BIND=192.168.1.20
```

```sh
docker compose up -d
```

The version is required by the release Compose file. Retaining the bind value
keeps a later recreate from silently returning to the loopback default.

`OONFEE_HTTP_BIND=0.0.0.0` publishes on every host IPv4 interface. This is an
explicit opt-in, not a browser address: open `http://<controller-LAN-IP>:8080`.
There is no native TLS listener, so restrict direct HTTP to a trusted,
firewalled management network and never expose port 8080 to the Internet.

The `passphrase` file unlocks the controller keyring and is not your owner
account password. Back it up with the controller state and keep both private.
`docker compose down -v` deletes the named data volume.

Bridge networking works on Linux and Docker Desktop. The shipped discovery path
is a bounded TCP `/ubus` scan of eligible interface subnets; it does not use ARP
or mDNS. A container bridge usually does not expose the router's LAN subnet, so
add routers by address. Linux host networking can expose the host's LAN
interfaces and is an explicit opt-in described in the Compose file.

For checksummed binaries, signature verification, reverse-proxy TLS,
persistence, upgrades, and rollback, follow the
[installation guide](docs/INSTALL.md).

### Upgrade from v0.1.4

Export and verify a portable backup before upgrading. For a direct rollback,
also retain a consistent pre-upgrade database/keyring pair or whole-volume
snapshot and its matching runtime passphrase. v0.1.5 migrates schema 20 to
schema 21 for device management mode, schema 22 for reusable policy sets, then
schema 23 for per-device client provenance, bounded MAC lookup indexes, and a
rebuilt one-managed-Gateway uniqueness guard based on canonical device
functions plus the compatibility role. Existing devices remain Managed.
v0.1.4 cannot open the migrated data. Replacing only the binary or image tag is
not a valid rollback.

Compose users should download or deliberately merge the v0.1.5 Compose file
and pin the intended image tag or digest. Follow the [upgrade and rollback
guide](docs/installation/upgrades.md) before changing the running version.

## Common questions

### Is Docker required?

No. Docker Compose is one quick-start option. The release also includes
standalone 64-bit Linux and macOS binaries with the web UI embedded.

### Does the controller run on an OpenWrt router?

No. It runs separately on a computer, NAS, SBC, or server and manages stock
OpenWrt devices. This keeps controller storage and upgrades away from the
routers' limited flash, RAM, and firmware lifecycle.

### Why does adoption ask for SSH access?

SSH is a bounded bootstrap, cleanup, and separately approved optional-package
path, not the steady-state management transport. Stock rpcd cannot create the
scoped login and ACL through ubus, even when logged in as root. The administrator
credential is used for the approved action and is not stored. Normal polling
and configuration use rpcd/ubus.

### What happens if a configuration change breaks connectivity?

Every Apply is previewed and uses OpenWrt's rollback window. The controller
confirms only after reconnecting and reading the expected state. If the router
becomes unreachable or the operation is interrupted, OpenWrt rolls the change
back. Ordinary writes and cleanup stay within owned UCI sections; the explicit
management-LAN IPv6 exception is limited to the reviewed option patches
described in the safety model below.

## First adoption

1. Set a router root password if it does not already have one:

   ```sh
   ROUTER_ADDRESS=192.0.2.1
   ssh -t root@"$ROUTER_ADDRESS" passwd
   ```

   The controller warns rather than blocking an explicitly trusted,
   passwordless lab router. Do not rely on that outside isolated testing.

2. In **Devices**, add the router by address or run the on-demand discovery
   scan.
3. Inspect discovered capabilities and source gaps, then choose **Managed** or
   **Monitor only**. Use monitor only when the router must remain outside site
   configuration; it will still need the scoped polling credential.
4. Review the controller-access payload. Approving it creates the scoped login
   and ACL; cancelling changes nothing.
5. Preview configuration before Apply. Monitor-only devices never participate
   in Preview or Apply, and router changes never happen merely because a device
   was discovered or listed.

## Safety model

- Apply uses `uci.apply` with a rollback window, then confirms only after the
  controller can read the expected state. An interrupted or unhealthy Apply
  reverts on the router.
- Ownership tags restrict ordinary writes and cleanup to controller-created
  sections. Explicit management-LAN IPv6 policy uses separately reviewed,
  option-only patches to the exact supported existing sections.
- RF scans, speed tests, capability installation, and other disruptive actions
  require explicit acknowledgement.
- Un-adoption restores or removes controller-owned configuration, then removes
  the scoped login and ACL. It is blocked while an optional LLDP installation
  still has a rollback record.
- Restoring a controller never automatically applies restored desired
  configuration. Router writes remain suppressed until an owner reviews and
  explicitly resumes them. Portable restore also clears source-relative client
  observations: they are evidence gathered by the source controller, not
  portable authorization for MAC-targeted writes. A fresh managed-Gateway poll
  must establish that proof on the destination controller.
- Monitor-only devices receive the distinct read-only `oonfeewrt-monitor` ACL
  and are excluded from desired/site configuration, optional LLDP install/
  config/remove mutations, wireless-neighbor mutations, and other package/
  config/remove operations after the acknowledged bootstrap. Existing LLDP
  observation remains available. Backend checks preserve these boundaries even
  when a request does not originate in the UI; explicit ACL maintenance and
  un-adoption remain available.
- A separately acknowledged RF scan remains available on a capable
  monitor-only radio. It is an active, transient observation: the serving radio
  goes off-channel and clients may pause, roam, or disconnect, but the scan has
  no intended persistent configuration change.
- Reusable policy sets are controller-side intent. Empty/malformed sets and
  missing or ambiguous references fail closed, and a referenced set cannot be
  deleted. Membership changes require a new Preview and explicit Apply before
  they affect the managed Gateway.
- MAC-based desired state is local-scope only. Policy-set creation/update,
  enabled direct/set firewall rules, Object Manager **Secure** drafts, and
  client block or fixed-address intent require a stored **local** observation
  from the currently adopted managed Gateway. Monitor-only observations neither
  satisfy nor contaminate that proof. After an upgrade, portable restore, or
  provenance expiry, active MAC intent blocks Preview until the Gateway observes
  it again. Portable restore deliberately clears this nonportable evidence;
  authorization also rejects observations older than 30 days or more than five
  minutes in the future independently of cleanup.
  Existing block/fixed-address intent can still be cleared one client at a
  time. Use network/zone or explicit IP scope instead.
- The HTTP listener has no native TLS. Keep it on loopback or an isolated
  management network, and use a trusted reverse proxy for remote access.

## Backup and diagnostics

Owners can use **Settings → Backup & Restore** to export an encrypted
`.oowrtbak` file. Export requires recent account reauthentication and a
separate passphrase that the controller does not retain. Restore decrypts and
validates in disposable staging, shows a compatibility preview, creates a
safety backup, and completes through a controlled restart.

For filesystem-level recovery, `oonfeewrt.db` and `keyring.json` are one
unit. The runtime passphrase cannot recreate a lost keyring. See the
[installation guide](docs/INSTALL.md#back-up-and-upgrade) before copying live
state.

Diagnostics bundles are bounded, redacted ZIP files generated from stored
controller evidence. They make no router management call and exclude
credentials, WLAN keys, private keys, session material, and controller
passphrases.

## Current limitations

- End-to-end hardware validation covers a Linksys WRT3200ACM and TP-Link Archer
  C6 v2 on OpenWrt 25.12.5. Read-only inspection is additionally
  reporter-confirmed from v0.1.3 on one Cudy M3000 v2/MT7981 Filogic variant;
  adoption, Apply, VLANs, polling budgets, and other Filogic boards remain
  unverified. Three-or-more-AP fan-out, real mesh backhaul, wireless uplink,
  literal peer isolation, the full Filogic/class-B resource envelope, and
  MT7621 also remain unverified.
- A live delegated IPv6 prefix and complete end-to-end IPv6 client path have
  not been proved on release hardware. Prefix delegation still depends on a
  working ISP or upstream delegation and a supported router layout.
- v0.1.4's transitive wired-topology correction has automated and candidate
  image coverage. Final validation on the reporter's multi-router hardware in
  [issue #25](https://github.com/aiden0rchad/oonfeeWRT/issues/25) remains pending.
- The speed test runs from the controller host or container through Cloudflare,
  not from a router. It uses approximately 15 MiB, is bounded to 30 seconds,
  and can temporarily saturate the WAN. Loaded latency and jitter are not
  measured.
- Native controller TLS, cloud remote access, multi-WAN management, manual WAN
  selection, gateway-run speed tests, DPI, and application-flow history are not
  included in v0.1.5. The flow feasibility page is a gated research plan, not a
  shipped capability.
- Optional LLDP may install official-feed packages. Adoption itself never
  installs a package, daemon, service, firmware, or executable.

Detailed hardware evidence and known gaps are in the
[fresh-start validation record](docs/FRESH-START-VALIDATION.md) and
[parity matrix](docs/PARITY-MATRIX.md).

## Build from source

Go 1.26.6 and Node.js 22 are the release toolchain.

```sh
make check
make build
./oonfeewrtd -data-dir "$PWD/.run" -listen 127.0.0.1:8080
```

For unattended startup, use `-passphrase-file` with a mode-`0600` file.
oonfeeWRT rejects passphrases supplied through environment variables.

## Documentation

- [Documentation site — capabilities, setup, guides, and troubleshooting](https://aiden0rchad.github.io/oonfeeWRT/)
- [Install, upgrade, TLS, and recovery](docs/INSTALL.md)
- [v0.1.5 release notes](docs/releases/v0.1.5.md)
- [v0.1.4 release notes](docs/releases/v0.1.4.md)
- [v0.1.3 release notes](docs/releases/v0.1.3.md)
- [v0.1.2 release notes](docs/releases/v0.1.2.md)
- [v0.1.1 release notes](docs/releases/v0.1.1.md)
- [v0.1.0 release notes](docs/releases/v0.1.0.md)
- [Architecture and security boundaries](docs/ARCHITECTURE.md)
- [Hardware validation](docs/FRESH-START-VALIDATION.md)
- [Feature parity and evidence](docs/PARITY-MATRIX.md)
- [Roadmap](docs/ROADMAP.md)
- [Risk register](docs/RISKS.md)

## Support future development

If oonfeeWRT is useful to you, you can support future development, hands-on
testing across more OpenWrt hardware, and careful release validation.

[![Buy me a coffee](https://img.buymeacoffee.com/button-api/?text=Buy%20me%20a%20coffee&emoji=&slug=aiden0rchad&button_colour=FFDD00&font_colour=000000&font_family=Cookie&outline_colour=000000&coffee_colour=ffffff)](https://buymeacoffee.com/aiden0rchad)

## License

Apache License 2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE), and
[THIRD_PARTY_LICENSES](third_party/THIRD_PARTY_LICENSES). Every release archive and
container image includes the same notices.

## AI transparency

AI coding tools have been used substantially during development to help draft
and iterate on implementation code, tests, debugging, and documentation. The
maintainer supplies the product direction, networking architecture, security
boundaries, hardware knowledge, review, and final decisions, and remains
responsible for what the project ships.

AI output is not treated as evidence that the software is correct or secure.
CI runs Go tests, `go vet`, the race detector, `govulncheck`, UI unit and browser
tests, OSV dependency scans, release smoke tests, and repository/history secret
scans.
Hardware behavior is checked separately against physical OpenWrt devices and
the known coverage gaps are published above.

oonfeeWRT has not received an independent security audit or third-party
penetration test. It is a new project: start with non-critical hardware, keep
backups, review every proposed router change, and report unexpected behavior.
