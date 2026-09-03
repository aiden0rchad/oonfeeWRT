# Adopt the first OpenWrt device

Adoption gives the controller a scoped OpenWrt login and records what the device can actually do. It does not apply your desired WLAN, network, DHCP, or firewall configuration.

> **Outcome:** One router appears as a managed device, polls successfully with its generated scoped credential, and has an explicit Gateway, AP, and/or Switch responsibility.

## Before you begin

You need:

- an owner or administrator controller account;
- the router's management address;
- an existing OpenWrt administrator username and password for ubus;
- SSH password access, or an OpenSSH private key if Dropbear disables password authentication;
- controller-to-router reachability for HTTP or HTTPS and SSH;
- a current router backup and physical access for your first trial.

Set a root password first if the router does not already have one:

```sh
ROUTER_ADDRESS=192.0.2.1
ssh -t root@"$ROUTER_ADDRESS" passwd
```

oonfeeWRT warns rather than blocks an explicitly trusted passwordless lab router. Do not leave a production or normal LAN router passwordless.

## Router write impact

Read-only discovery and **Inspect capabilities** do not change the router.

Adoption requires a separate acknowledgement and then:

- writes `/usr/share/rpcd/acl.d/oonfeewrt.json`;
- creates the scoped `rpcd.oonfeewrt` login with a generated password;
- verifies that login and records device capabilities;
- records the router's SSH host key and, for HTTPS, its certificate fingerprint.

Adoption installs no package, binary, daemon, service, or firmware. It does not change network, WLAN, DHCP, or firewall settings. Those require a later Preview and Apply.

The administrator password and optional SSH private key exist only for this request and are not stored.

## 1. Open the adoption screen

Sign in and select **Adopt a device** in the left navigation.

The page always provides add-by-address. Its discovery scan is only a convenience:

- bridge-mode Docker's automatic plan normally sees only the container's own
  interface subnets, not the host LAN;
- Docker Desktop users should normally add by address;
- discovery is IPv4-only, on demand, and refuses networks wider than `/22`;
- the shipped scan probes TCP `/ubus`; it does not use ARP tables or mDNS in
  bridge or host-network mode.

If a scan lists the correct router, select **Adopt this**. Otherwise enter its management address directly.

## 2. Enter the existing router login

Fill in:

- **Address:** a router hostname or management IP;
- **Name:** optional; when blank, the device model can supply the name;
- **Protocol:** `http` or `https`, matching the router's uhttpd service;
- **Device username:** commonly `root` on a stock installation;
- **Device password (for ubus):** the router's existing password;
- **SSH private key:** only when SSH password authentication is disabled.

The ubus sign-in still needs the password even when an SSH key is supplied. The key is only for the one-time SSH bootstrap.

For HTTPS, adoption records the observed device certificate fingerprint. Later certificate changes are refused until explicitly reviewed rather than silently trusted.

## 3. Inspect before adopting

Select **Inspect capabilities**.

Inspection uses authenticated, read-only ubus calls. It creates no inventory row, login, ACL, package, or configuration. Review:

- model and firmware;
- radios;
- observed LAN layout and WAN port;
- active WAN default-route evidence;
- LAN DHCP-server evidence;
- switch mode;
- recommended device functions;
- unobservable items and notes.

An **Unknown** result means the credential, ACL, rpcd module, or driver did not supply enough evidence. It does not mean the feature is absent.

The radio count is a physical-radio inventory, not a count of SSIDs or BSS
interfaces. A radio that exposes both AP and station interfaces is counted
once. Likewise, **Single interface: `eth1` (no separate switch)** means the
board reported a LAN interface without independent switch ports; it is not
evidence that inspection missed a known switch.

To share hardware support evidence, select **Export sanitized compatibility
report**. The browser downloads `oonfeewrt-compatibility-report.json`. Its
format version is `1`, and it contains only bounded, allowlisted hardware,
firmware, physical-radio, port, feature-state, and supported-function facts.
It excludes router and site identity, MAC and network addresses, credentials,
network configuration, clients, live telemetry, timestamps, runtime
radio/PHY and bridge-member identifiers, and free-text notes. Board-declared
LAN/WAN labels remain because they are hardware compatibility evidence.

The report is built from the inspection already completed: exporting it makes
no extra router call, writes no controller state, and uploads nothing. If the
button is absent, read the inspection notes. Evidence outside the strict
safety bounds causes report generation to fail closed while leaving the
inspection result usable; do not replace the report with a raw response or
screenshot that may expose identifiers.

## 4. Select device functions

Select at least one function:

- **Gateway:** routes between managed networks and toward the Internet; receives addressing, DHCP, and firewall intent.
- **AP:** publishes managed WLANs and carries their networks; does not imply routing or DHCP.
- **Switch:** records wired responsibility and available port/topology visibility; it does not promise universal per-port or VLAN configuration.

Select every responsibility the router should perform. A combined OpenWrt router may be Gateway, AP, and Switch. An AP-only router should not receive Gateway just because it has a default route through its management LAN.

If oonfeeWRT will manage the network's gateway, adopt that device first. Only one managed Gateway is supported. AP-only adoption remains valid when routing is intentionally managed elsewhere.

Switch behavior is capability-dependent:

- `dsa-conditional` can manage VLAN carriage only when the existing LAN bridge is already VLAN-aware;
- `observe-only` exposes legacy switch telemetry/topology without promising writes;
- `unknown` retains the uncertainty;
- `none` means no switch capability was observed.

A generic single-interface LAN is separate from legacy `swconfig`. v0.1.4
does not create tagged VLAN attachments on that layout and leaves its existing
LAN/VLAN configuration unchanged. See [Networks, VLANs, and DHCP](../guide/networks.md)
before planning a tagged network.

## 5. Review the controller access payload

Open **Review exact router changes** and read the displayed plan. The scoped ACL grants:

- supported inventory, topology, radio/scan, OpenWrt log, and fixed-target `1.1.1.1` ICMP observations;
- read-only router UTC observation through `luci.getUnixtime`, with
  `luci.getLocaltime` as a compatibility fallback;
- ordinary UCI writes for controller-owned network, wireless, firewall, and
  DHCP sections after a separate Preview and Apply;
- allowlisted option-only patches to exact existing management-LAN, DHCP, and
  supported conventional `wan`/`wan6` sections only when an explicit
  non-**Router managed** IPv6 policy is separately previewed and applied; and
- runtime 802.11k neighbour-list updates for managed WLANs that request them.

It does not grant client disconnection or steering. The client keeps the roaming decision.

Select **Install the oonfeeWRT controller access payload?** only after you accept those exact changes. Leaving it unchecked keeps Adopt unavailable; cancelling leaves the router unchanged.

## 6. Adopt and wait for verification

Select **Adopt**.

The controller:

1. pins the resolved target for the operation;
2. verifies the administrator access and device identity;
3. writes and hashes the ACL file over SSH;
4. creates the scoped login;
5. signs in again using the new controller credential;
6. probes capabilities through the same access the controller will use later;
7. stores the device only after the safety checks succeed.

The radio survey is deliberately sampled twice, so adoption may take several seconds.

## Verify adoption

After completion:

1. Review **What the capability probe found**. Available, undetermined, driver-quirk, and note lists should agree with the hardware evidence.
2. Open **Devices** and select the device.
3. Confirm its address, MAC, firmware, functions, class, and poll status.
4. Wait for a successful poll and confirm **Last seen** advances.
5. Review the **Management overhead** section and unavailable sources.
6. Open **Dashboard** and confirm the device count changes.

Do not expect new SSIDs or network changes yet. Adoption and provisioning are separate.

## Make the first desired-state change safely

When you are ready:

1. Open **Settings → Network**.
2. Create or edit only the intended network, AP group, or WLAN.
3. Save. This changes controller desired state only.
4. Select **Preview changes**.
5. Read the per-device diff, source gaps, conflicts, and safety cautions.
6. Supply every acknowledgement requested by the current preview.
7. Select **Apply**.

Apply stages the UCI delta and invokes OpenWrt's rollback-protected apply. The controller confirms only after it reconnects and verifies expected interface, WLAN, and uplink health. A lost connection or failed health check leaves the rollback timer active so it can restore the previous configuration. Wait for a fresh read: an `unknown` or stranded outcome is not proof of rollback and may still have the change live.

## Troubleshooting and recovery

### Inspection cannot reach `/ubus`

Verify the protocol, address, and management route. The router needs `rpcd` and `uhttpd` with the ubus handler enabled. The expected uhttpd setting is:

```text
config uhttpd 'main'
    option ubus_prefix '/ubus'
```

Do not install packages blindly. First verify the existing OpenWrt service and configuration through LuCI or SSH.

### Discovery is empty

Enter the IP address directly. The shipped scanner needs an eligible, reachable
IPv4 interface subnet; it does not implement ARP-table or mDNS discovery. A
Docker bridge or Docker Desktop VM commonly exposes only its internal subnet,
not the router's management subnet.

### The login fails

Confirm that the password works for ubus as well as SSH. An SSH private key does not replace the ubus password. If the router has no root password, set one before continuing.

### SSH bootstrap fails

Confirm TCP/22 reachability, the administrator username, and whether Dropbear permits password authentication. Supply an OpenSSH private key when password authentication is disabled. A changed SSH host key requires explicit review; do not bypass it without verifying the physical device.

### Gateway selection is refused

The controller permits only one managed Gateway. Review the existing device functions before changing which router owns site routing.

### Adoption stops partway through

Read the returned rollback report. It includes exact cleanup commands if the controller cannot prove that its ACL or login was removed. Do not repeatedly Adopt until you know whether the prior scoped footprint remains.

### Data is unavailable after adoption

Review the device capability report. Missing rpcd access, optional modules, or driver support is shown as a source gap. If the release offers **Refresh controller access**, review and explicitly approve that ACL-only transaction; polling never widens the ACL silently.

### Remove the device later

Use **Devices → Remove from controller**. Normal un-adoption first reverts controller-owned configuration, then asks for the router administrator credential to remove the scoped login and ACL. It is blocked while an optional LLDP rollback ledger remains. If the router is gone, the force path records that configuration or controller footprint may remain.

## Next steps

- [Configure accounts and roles](../operations/accounts.md)
- [Create and verify backups](../operations/backups.md)
- [Put TLS in front of the controller](../installation/reverse-proxy.md)
- [Review routine maintenance](../operations/maintenance.md)
