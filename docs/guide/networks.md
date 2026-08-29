# Networks, VLANs, and DHCP

Network settings describe the site you want: IPv4 networks, optional VLANs,
DHCP service, and firewall zones. Saving a model records intent in the
controller. **Preview and Apply are separate actions.**

<div class="write-impact warning"><strong>Router write impact</strong><span>Editing and saving desired state changes only the controller database. Apply can change controller-owned `network`, `dhcp`, `firewall`, and related wireless UCI sections after preview, preflight, and acknowledgement.</span></div>

## Before you configure a network

Have all of these ready:

- a tested controller backup and router configuration backup;
- one adopted non-critical device for the first rollout;
- an IPv4 CIDR and, when applicable, VLAN ID;
- the intended firewall zone and forwarding behavior;
- a DHCP pool that fits inside the subnet and does not include infrastructure
  addresses you plan to configure statically;
- capability evidence for DSA versus legacy swconfig and for the relevant
  network/firewall rpcd modules;
- physical switch and trunk configuration outside oonfeeWRT, if traffic must
  traverse unmanaged infrastructure.

::: danger Keep a recovery path
An incorrect management VLAN, bridge, firewall input policy, or DHCP plan can
disconnect the controller and clients. Start with one non-critical device,
stay on a known management path, and rely on the rollback timer—not on an
untested assumption that another path will work.
:::

## Understand the model

### Network

A network gives a named IPv4 segment its CIDR, optional VLAN ID, DHCP behavior,
and firewall-zone membership. Networks are site-wide; device functions and
capability evidence determine what each Preview can safely render.

### VLAN

A VLAN ID labels traffic; it does not configure every switch between the
controller and the client. oonfeeWRT will not silently convert a bridge that is
not already VLAN-aware. Legacy swconfig port writes remain observe-only in
v0.1.3 because port topology and safe mutation are hardware-specific.

### DHCP

DHCP settings must stay within the network CIDR. The editor validates missing,
inverted, or out-of-range pool values before Apply. Plan the router address,
static infrastructure, reservations, and dynamic range together.

### Firewall zone

The zone anchors forwarded traffic policy and explicit firewall-rule scope. A
zone name is not cosmetic: moving a network can change effective policy. The
Zone Matrix edits whole-zone forwarding; router-local input rules are separate
explicit policies. oonfeeWRT previews controller-owned firewall sections and
blocks or warns on conflicts it cannot safely reconcile.

## Create a network

1. Open **Settings → Network**.
2. Find **Networks** and choose **Add network**.
3. Enter a unique, descriptive name.
4. Enter the IPv4 CIDR, such as `192.168.20.1/24`, using the router address in
   the prefix where the editor expects the interface address.
5. If the segment is tagged, enter its VLAN ID and confirm the upstream trunks
   already carry it.
6. Choose or create the firewall zone.
7. Enable DHCP only if this managed Gateway should provide it.
8. Set the DHCP pool and lease options shown by the editor.
9. Save desired state.

At this point no router Apply has happened.

## Review zone behavior

Before Apply, open the **Policy Engine → Zone Matrix** and **Master Table** and
answer:

- Is forwarding to WAN allowed or denied as intended?
- Which other managed zones can this zone initiate connections toward?
- Are inbound port forwards or explicit gateway-input rules required?
- Does foreign firewall configuration already cover or conflict with this
  path?

The Zone Matrix does not model DHCP/DNS access to the Gateway itself. Review
the network's DHCP settings and any explicit gateway-input rules separately.

Use explicit policy rules for exceptions. Avoid broad forwarding merely to
make a test pass.

## Preview the fleet change

Open the review action in Settings and generate a fresh Preview. A preview is
bound to the current desired state and fleet; if either changes, generate it
again.

Inspect every device bucket:

- **Changes** — owned UCI sections/options that would be staged;
- **No change** — desired state already matches observed owned state;
- **Omissions** — capability or device-function rules intentionally skip work;
- **Conflicts/blockers** — foreign state, missing sources, unsupported bridge
  shape, driver defects, or invalid intent;
- **Acknowledgements** — traversal or hardware risks you must explicitly
  accept.

The preview redacts WLAN and mesh keys. A preview should never be used as a way
to reveal stored secrets.

## Apply and verify

1. Resolve blockers rather than bypassing them on the router.
2. Read every acknowledgement and select only those you understand.
3. Start Apply once. The operation is durable; do not create a second attempt
   merely because the browser disconnects.
4. oonfeeWRT preflights the fleet, applies non-Gateway devices first, and the
   Gateway last.
5. Each device stages a bounded UCI batch and starts OpenWrt's rollback window.
6. The controller reconnects, reads the expected state, runs the health checks,
   and confirms only on success.

After completion:

- confirm the operation receipt reports a known outcome for every device;
- verify the management path still works;
- connect a test client to the segment;
- verify address, gateway, DNS, and intended Internet/inter-zone reachability;
- inspect **Logs → Audit** for the Apply record;
- compare the device's owned-state/capability view with the preview.

## If connectivity fails

Do not immediately repeat Apply. OpenWrt should revert an unconfirmed change
when the rollback window expires.

1. Keep the router powered and avoid restarting it during the window.
2. Wait for the durable operation status to settle.
3. Reconnect through the original management path.
4. Confirm the prior configuration returned in LuCI or with read-only UCI
   inspection.
5. Read the controller receipt and logs for the device boundary it crossed.
6. Correct desired state, generate a new preview, and try again only after the
   outcome is known.

If the outcome is reported as **unknown** or **possible write**, treat the
router as changed until independently inspected.

## Coexist with LuCI and existing UCI

oonfeeWRT owns only sections it created and recorded. Human-managed sections
remain visible but are not silently rewritten. This has two consequences:

- foreign configuration can block a requested intent when both would control
  the same effective behavior;
- renaming or manually removing an owned section outside the controller can
  break ownership evidence and requires investigation, not automatic takeover.

Make a deliberate ownership choice. Do not repeatedly edit the same logical
network in both LuCI and oonfeeWRT.

## Common problems

| Preview finding | Meaning | Safe response |
|---|---|---|
| Bridge is not VLAN-aware | The live bridge cannot accept the requested safe rendering | Convert it manually with a tested OpenWrt-specific plan, or use an untagged design; oonfeeWRT will not convert it silently |
| Legacy swconfig device | Per-port VLAN writes are not safely generalized | Keep port configuration outside oonfeeWRT and use supported observation/site features |
| Foreign firewall conflict | A human-owned rule/zone affects the requested traffic path | Inspect the exact UCI/nft behavior, then remove or redesign one owner intentionally |
| Missing `firewall4` capability | The controller cannot establish the required firewall backend | Install/enable the supported OpenWrt component outside adoption, then reprobe |
| DHCP range invalid | Pool is incomplete, outside the subnet, or conflicts with the interface plan | Correct the CIDR/pool before preview |
| Stale preview | Desired state or fleet changed after preview | Generate and review a new preview |
| Apply interrupted | Browser or controller connection ended during a durable operation | Reopen the operation status; never assume failure means no write |

## Related guides

- [Policy Engine and firewall](./policy-engine.md)
- [Wi-Fi, roaming, and overrides](./wifi.md)
- [Safety and ownership model](../concepts/safety.md)
- [Backup and staged restore](../operations/backups.md)
