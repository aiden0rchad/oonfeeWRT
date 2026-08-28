# Policy Engine and firewall

The Policy Engine turns explicit intent into reviewable firewall, forwarding,
route, and client-policy objects. It exposes both the high-level relationship
and the concrete draft so broad access is not hidden behind friendly labels.

<div class="write-impact warning"><strong>Router write impact</strong><span>Editing and saving policies changes controller desired state. Router firewall, DHCP, or route state changes only through a fresh Preview and acknowledged Apply.</span></div>

## Before you create policy

- Define the networks and firewall zones first.
- Confirm the managed Gateway has current firewall/backend capability evidence.
- Document the source, destination, protocol, ports, and business reason.
- Back up controller and Gateway configuration.
- Begin with a narrow test rule and a known client.
- Know how you will retain management access if the rule is wrong.

The Policy Engine is IPv4-focused in v0.1.1. QoS, application/DPI identity,
proved priority semantics, switch ACLs, device/group routing, and advanced
traffic classification remain unavailable or gated.

## The three views

### Master Table

The Master Table combines saved rules with synthesized zone-forwarding and
client-policy rows. Use it to see which policies exist, their types, display
order, enabled state, origin, deployability, and editable source.

Rows in this view include:

- explicit firewall rules;
- port forwards;
- static routes;
- client block/fixed-address/group intent;
- whole-zone forwarding relationships.

Display order helps reviewers understand intent, but do not assume a numeric
priority has packet-processing semantics unless the preview/concrete backend
proves it.

### Zone Matrix

The matrix shows effective relationships from each managed source zone to
other zones and WAN. Same-zone cells are informational. Editable cells open the
source policy rather than mutating the router immediately.

Use it to spot broad forwarding before reading the exception rules:

- Which zones may initiate toward WAN?
- Which managed zones may reach each other?
- Which directions are read-only or derived?
- Does an allowed relationship exceed the narrow service actually needed?

### Object Manager

Object Manager compiles selected networks/clients and an intended action into a
visible **unsaved draft**. Read the concrete result before saving it.

The client inventory can be partial. A selector built from incomplete current
inventory is not proof that every intended endpoint is covered. Prefer stable
network or explicit address objects when identity must survive MAC
randomization or an offline client.

## Create an explicit firewall rule

1. Open **Policy Engine → Master Table**.
2. Add a rule and choose **Firewall rule**.
3. Give it a descriptive name and display order.
4. Choose `accept`, `drop`, or `reject` deliberately.
5. Select the source zone.
6. Select a destination zone, or leave it router-local only when that is the
   intended model.
7. Choose protocols.
8. Add source/destination IPv4 CIDRs, ports/ranges, or source MACs only as
   needed.
9. Save desired state.
10. Review the Zone Matrix and generate a fresh Preview.

Avoid `0.0.0.0/0`, all protocols, and all ports unless the zone relationship is
intentionally broad and documented.

## Create a port forward

A port forward exposes a service through the managed Gateway.

Required intent includes:

- destination zone;
- protocol;
- external port;
- destination IPv4 address;
- destination port;
- optional allowed source IPv4 CIDR.

::: danger Internet exposure
A correct port-forward rendering can still expose an insecure service. Patch
the destination, restrict source CIDRs when possible, use application-level
authentication/TLS, and verify the service from an external test path.
:::

After Apply, test both an allowed and a denied source. Review Gateway logs and
the exact rendered preview; a successful Apply proves configuration state, not
that the application is safe.

## Create a static route

Specify:

- target IPv4 network;
- next-hop IPv4 address;
- egress network;
- optional metric.

Verify the next hop is reachable on the selected network and that the return
route exists. A forward route without a return path can look like a firewall
failure.

After Apply, test from the Gateway and a representative client, then confirm
that the route does not capture management or default traffic unexpectedly.

## Manage client intent

Client desired policy can record a fixed IPv4 address, group, or block intent.
The inventory identity and DHCP/firewall backend still determine how that
intent is realized.

Before using a fixed address:

- keep it inside the intended subnet;
- keep it outside the dynamic pool or reserve it intentionally;
- account for randomized MAC addresses;
- verify there is no existing static assignment.

## Review the concrete preview

The Preview is the decision boundary. Expand the Gateway and check:

- exact controller-owned UCI additions/updates/removals;
- protocol and port normalization;
- source/destination zones and router-local versus forwarded behavior;
- route target, next hop, network, and metric;
- foreign rule/route conflicts;
- capability omissions or backend gaps;
- acknowledgement requirements;
- redaction of any unrelated secret values.

If a foreign rule already rejects or redirects the same path, resolve ownership
explicitly. oonfeeWRT must not reorder or rewrite human-owned firewall state to
make its preview converge.

## Apply and validate

1. Generate a fresh preview after the final edit.
2. Start Apply once and follow the durable receipt.
3. Confirm management access remains available.
4. Test the intended allowed path.
5. Test at least one intended denied path.
6. For port forwards, test from outside the Gateway rather than hairpinning
   unless hairpin behavior is itself the requirement.
7. For routes, test the return path and traceroute/route evidence.
8. Check **Logs → Audit** and the Gateway device state.

## Troubleshooting

| Symptom | Likely cause | Action |
|---|---|---|
| Rule saves but is not on the router | Saving desired state is not Apply | Generate a Preview and use the acknowledged Apply workflow |
| Preview blocks on firewall capability | `firewall4`/rpcd source is absent or unknown | Read the capability report, repair the supported OpenWrt source, and reprobe |
| Preview reports a foreign conflict | Human-owned UCI/nft rule controls the same path | Inspect exact order/effect; redesign or remove one owner intentionally |
| Port forward applies but service is unreachable | Application down, destination firewall, wrong route, ISP/CGNAT, or source restriction | Test each hop and service locally before changing the rule |
| Static route works one way | Missing return route or stateful firewall path | Correct the remote network and zone policy; do not add an unrelated broad allow |
| Object Manager list is incomplete | Client inventory/source coverage is partial | Use network/stable explicit selectors and fix coverage before relying on it |
| Management is lost | Policy affected router input or management path | Let OpenWrt rollback unconfirmed Apply; inspect the durable outcome before retrying |

## Related guides

- [Networks, VLANs, and DHCP](./networks.md)
- [Safety and ownership model](../concepts/safety.md)
- [Logs and diagnostics](./logs-diagnostics.md)
- [Capability and support matrix](../reference/capabilities.md)
