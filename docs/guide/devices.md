# Discovery, adoption, and devices

The Devices workflow moves from **unknown address**, to **read-only inspection**,
to **explicitly adopted functions**. Discovery never equals adoption, and
adoption never equals applying network configuration.

<div class="write-impact warning"><strong>Router write impact</strong><span>Discovery, add-by-address, inspection, telemetry, rename, and reprobe are read-only to the router. Adoption can create a scoped login and ACL only after review and approval. Optional LLDP and un-adoption have separate change plans.</span></div>

## The device lifecycle

1. **Discover or add** an address.
2. **Inspect** model, firmware, interfaces, functions, and source gaps without
   saving a management credential.
3. **Adopt** selected Gateway, AP, or Switch functions and approve scoped
   controller access.
4. **Observe and configure** through the device and site views.
5. **Reprobe or refresh access** when firmware or capabilities change.
6. **Un-adopt** through a reviewed cleanup plan when the device leaves the
   controller.

## Find a device

Open **Adopt a device** or use the adoption action from **Devices**.

### Add by address

Add-by-address is the reliable path in every supported deployment. Enter the
router's management hostname or IPv4 address and continue to inspection.

Use it when:

- the controller runs in Docker bridge mode;
- the router is across a routed management VLAN or VPN;
- local discovery does not cover the subnet;
- you already know the management address.

### Scan the local subnet

Discovery is an explicit, on-demand IPv4 scan. The scan will not accept a
network wider than `/22`. It probes each eligible address over TCP for an
unauthenticated OpenWrt `/ubus` endpoint; it does not implement ARP or mDNS
discovery. A bridged container may not expose the LAN subnets to this scan.

Discovery only lists candidates. It does not create a router login, change UCI,
install a package, or begin continuous management.

## Inspect before adoption

Provide the router administrator credential only in the inspection/adoption
flow. The controller can use it to establish facts such as:

- model, board, and firmware;
- management address and device MAC;
- radio and interface evidence;
- LAN, WAN, default-route, and switch-mode evidence;
- recommended Gateway, AP, or Switch functions;
- capability features, unobservable sources, driver notes, and source gaps.

Unknown is not the same as absent. If the controller could not read a source,
the inspection should say so. Fix the source or accept the limitation; do not
convert the gap into a capability claim.

::: danger Identity pins begin at adoption
Read-only inspection uses ubus and does not open SSH. Adoption records the SSH
host key and, for HTTPS management, the rpcd certificate fingerprint. Later
unexpected changes are refused. They can indicate a factory reset,
reinstallation, address reuse, or interception; verify the device out of band
before force-un-adopting and adopting it again.
:::

## Choose device functions

Functions describe what the controller may expect from the device:

| Function | Use it for | Important boundary |
|---|---|---|
| Gateway | Default route, WAN observation, routed networks, DHCP, and firewall behavior | Only one managed Gateway is supported |
| AP | WLAN broadcast, associated stations, radio metrics, roaming, and RF tools | A device can be AP-only or combine functions |
| Switch | Infrastructure placement and switch observations | Per-port VLAN writing depends on hardware; legacy swconfig remains observe-only |

Select only functions the device actually performs. You can combine functions
on an all-in-one router. A wrong function choice creates misleading
expectations and can block a later preview when required capabilities are not
present.

## Review the controller access payload

Stock rpcd cannot create its own scoped management identity. Adoption can use
SSH once to create:

- one `oonfeewrt` login; and
- one reviewable rpcd ACL JSON file.

The router administrator credential is used for that bounded bootstrap and is
not stored. Normal collection and configuration use rpcd/ubus with the scoped
credential. Adoption does not install a package, executable, daemon, service,
or firmware.

1. Expand the displayed access plan.
2. Review the destination paths and ACL operations.
3. Confirm the inspected MAC/model evidence and selected functions.
4. Approve the controller access payload only if the scope is acceptable.
5. Wait for the post-adoption capability report before configuring the site.

See [Adopt your first device](../getting-started/first-adoption.md) for the full
first-device runbook.

## Read the Devices list

The list is the fleet-level view. Use status and last-seen time together:

- **Online** means management evidence is fresh enough for the current
  collection cadence.
- **Offline** means the freshness threshold was crossed; it does not prove the
  router itself is powered off.
- **Unavailable** or a source gap means a specific value could not be
  established.

Open a row for the detail workspace.

## Use device detail

The detail view can include:

- name, address, MAC, firmware, class, and selected functions;
- load, memory, client count, and poll duration;
- traffic and radio time series;
- radio inventory and broadcast provenance;
- capability states and actionable source gaps;
- current collection tier and adjustable slower interval;
- controller requests, bytes, polls, and router CPU attributed to management;
- controller-installed package accounting;
- ownership state and reviewed change or un-adoption actions.

### Focused collection

Opening a live device workspace can move it from the baseline collection tier
to a faster focused tier. The verified defaults are approximately 60 seconds
at baseline and 10 seconds while focused. Slow or overloaded devices can back
off, up to 10 minutes, and Apply temporarily quiesces collection.

Use **poll less often** when management overhead matters more than freshness.
Check the overhead card afterward rather than assuming the interval change had
the intended effect.

### Rename

Renaming changes the controller's display identity only. It does not rename
the OpenWrt hostname. Use distinct names when two devices share the same model
or default hostname.

### Reprobe capabilities

Reprobe after a firmware upgrade, package change, ACL refresh, or when the UI
says an older capability record has no answer for a new feature. Reprobe reads
sources; it does not Apply site configuration.

Compare the new report with the earlier one. A newly missing source can be an
ACL regression or package/module change, not necessarily lost hardware.

### Refresh the ACL

Use ACL refresh only when the controller identifies that the adopted scoped
identity lacks a currently required permission. This uses the device
administrator credential for a bounded update. Review the exact payload again;
do not treat it as a generic repair step for transport failures.

## Optional LLDP

LLDP can improve physical topology evidence, but it is not part of adoption.
The separate workflow can:

1. refresh the OpenWrt package index after acknowledgement;
2. show the exact official-feed package plan;
3. install `lldpd` and recorded feed dependencies after acknowledgement;
4. show the exact physical-interface configuration plan;
5. record before-state, added packages, and service ownership for rollback.

::: warning Router package change
LLDP is the only shipped optional package workflow. Do not approve it merely
to remove an “unavailable” label. Install it only when improved topology
evidence is worth the storage, service, and change footprint on that device.
:::

Rollback restores the recorded configuration and service state and removes
only packages recorded as additions. A live LLDP ownership record blocks
un-adoption until rollback is resolved.

## Un-adopt safely

Un-adoption first plans restoration/removal of controller-owned configuration,
then removes the scoped login and ACL. It must not rewrite human-owned UCI.

Before starting:

- export a controller backup;
- make a router configuration backup;
- resolve any in-progress Apply;
- roll back optional LLDP;
- ensure the administrator credential and management path still work.

If the device is unreachable, the force path can remove the controller record
but cannot prove router cleanup. Save the displayed residue report and manual
commands. Do not assume a lost device is clean merely because it disappeared
from the controller.

## Troubleshooting

| Symptom | Check | Action |
|---|---|---|
| Scan finds no device | Bridged-container subnet visibility, routed subnet, subnet size, or controller reachability | Add the router by hostname or IPv4 address; verify the controller can route to its management endpoint |
| Inspection cannot connect | Address, administrator password, selected HTTP/HTTPS protocol, `rpcd`, `uhttpd` ubus handler, or firewall | Verify the ubus management endpoint from the controller host; SSH is not used by inspection |
| Adoption refuses Gateway | Another adopted device already has the Gateway function | Review and un-adopt the existing Gateway before adopting a replacement; functions cannot be reassigned in place in v0.1.1 |
| Host key or certificate changed | Factory reset, firmware reinstall, address reuse, interception | Verify identity out of band before force-un-adopting and adopting the device again |
| Metrics say unavailable | Source not readable, driver lacks metric, ACL gap, or no completed poll | Read the source explanation; reprobe or refresh ACL only when it names a repairable cause |
| Device flips offline/online | Slow polls, unstable transport, adaptive backoff, overloaded router | Review poll duration, overhead, controller logs, and network path before shortening intervals |
| Un-adoption is blocked | Active LLDP ledger or another conflicting operation | Roll back LLDP and allow active operations to finish |

## Related guides

- [Adopt your first device](../getting-started/first-adoption.md)
- [Safety and ownership model](../concepts/safety.md)
- [Requirements and compatibility](../reference/requirements.md)
- [Clients and topology](./clients-topology.md)
