# Optional oonfeeWRT router helper

Optional package in the controller's v0.1.6 source tree; the helper has its own
version, `0.1.1`. This is **not required for ordinary
adoption or monitoring**. No released SDK package or hardware-validation claim
is implied by the source being present.

The helper runs only when rpcd invokes it. There is no service/daemon, added
network listener, automatic installation, telemetry upload, or update loop.
It exposes only `status`, `board`, `info`, and `wireguard` through the `oonfeewrt-agent` ubus
object. It accepts no caller-controlled command, filename, or shell argument.

`status` reports protocol version `1`, helper version, and the presence of
`sysupgrade`/`owut`. Tool presence is not a compatibility or flash-readiness
test. `board` and `info` delegate to the corresponding existing `system` ubus
reads with a five-second timeout. `wireguard` reads only interface names, public
peer keys, latest-handshake timestamps, and transfer counters through fixed
`wg show` field selectors. It never collects `dump`, private keys, preshared
keys, or configuration files. WireGuard tools are optional and are not installed
by this package. No firmware writes are implemented.

## Build with the matching OpenWrt SDK

1. Obtain the official SDK matching the target router's OpenWrt release and
   target. Verify the SDK download through OpenWrt's published checksums.
2. Copy this entire directory into the SDK as `package/oonfeewrt-agent`.
3. In the SDK root, run:

   ```sh
   make defconfig
   make package/oonfeewrt-agent/compile V=s
   ```

4. Inspect the generated package and dependencies (`rpcd`, `ubus`, and `jshn`). The
   package contains only:

   ```text
   /usr/libexec/rpcd/oonfeewrt-agent
   /usr/share/rpcd/acl.d/oonfeewrt-agent.json
   ```

5. Build/test on disposable hardware before a production installation. The
   repository tests verify the script method contract and narrow ACL; they do
   not replace a target SDK build or hardware test.

## Explicit installation and access

Install the locally built package using the router's normal package-management
workflow. Review its contents before installation. There is deliberately no
post-install script that edits login permissions or restarts rpcd.

After installation, reload rpcd in a maintenance window to discover the new
plugin. On versions where a reload does not discover plugins, a restart may be
needed and will clear rpcd sessions. Check your release's behavior rather than
restarting a managed router's services unexpectedly.

Local root can inspect the methods without granting remote access:

```sh
ubus -v list oonfeewrt-agent
ubus call oonfeewrt-agent status '{}'
ubus call oonfeewrt-agent board '{}'
ubus call oonfeewrt-agent info '{}'
ubus call oonfeewrt-agent wireguard '{}'
```

For authenticated remote use, manually add **only** the
`oonfeewrt-agent-read` group to the chosen rpcd login's read permissions. Keep
its existing permissions intact. Do not grant a wildcard group, `file.exec`,
shell execution, or write access. This package does not create a login, store
a password, change TLS, expose rpcd to another network, or amend oonfeeWRT's
default adoption payload.

The Firmware screen does not infer installation from the package's availability.
The Integrations screen calls `wireguard` only after an Admin or Owner explicitly
requests a check for an adopted device. An absent helper, missing read grant, or
missing WireGuard tools is unavailable evidence, not an empty peer inventory.
Ordinary monitoring remains on the existing scoped rpcd path.

## Removal

Remove the package through the router's package manager, remove the optional
read group from any login to which you explicitly added it, and reload rpcd.
The package manager removes the two helper files; no controller-owned network
configuration is part of this package.

## Checks

From the oonfeeWRT repository root:

```sh
go test ./deploy -run TestOptionalAgent
sh -n deploy/openwrt-agent/files/oonfeewrt-agent
```

See the [Firmware guide](../../docs/guide/firmware.md) for the catalogue check's
scope and the requirements that remain before controller-managed installation.
The plugin protocol is documented by
[OpenWrt rpcd](https://openwrt.org/docs/techref/rpcd).
