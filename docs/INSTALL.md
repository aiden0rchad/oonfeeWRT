# Install oonfeeWRT

oonfeeWRT is a controller that runs on a computer, NAS, or server. It does not
replace OpenWrt firmware and no controller binary runs on a router.

This guide targets schema-19 patch release `v0.1.3`. The
[GitHub release](https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v0.1.3)
and its completed tag workflow are the publication source of truth; run the
download commands only after that release is available. Back up both the
controller and each router before using it on a network you cannot afford to
interrupt. Upgrade and rollback from historical `v0.1.0-rc.1` are documented
below.

## What can change a router

Nothing is installed merely by starting the controller, scanning, or adding a
device address. Router changes are separate, default-off actions:

1. **Controller access payload during adoption:** after an explicit prompt,
   one rpcd ACL JSON file and one scoped `oonfeewrt` login are added. This is not
   a package, executable, daemon, service, or firmware change. The supplied
   administrator credential is used for that SSH action and is not stored.
2. **LLDP capability:** after adoption, a separate workflow may install the
   official OpenWrt `lldpd` package and its feed dependencies. Refreshing the
   package index, installing the displayed exact plan, and configuring physical
   interfaces each require their own acknowledgement. Rollback restores the
   recorded configuration and service state and removes only packages recorded
   as additions.
3. **Network configuration:** WLAN, network, DHCP, and firewall changes occur
   only after Preview and Apply. They are controller desired state, not hidden
   adoption side effects.

Un-adoption removes the scoped login and ACL. It is blocked until any recorded
LLDP capability is rolled back, so package residue cannot be silently orphaned.

## Choose a controller host

Use a supported 64-bit Linux or macOS host on the management LAN:

- `linux/amd64`, `linux/arm64`, `darwin/amd64`, or `darwin/arm64` for binaries;
- `linux/amd64` or `linux/arm64` for the container image;
- network reachability from the controller to each OpenWrt management address.

The controller's HTTP listener has no native TLS. Bind it to loopback by
default. Use a trusted reverse proxy for TLS or deliberately bind to a trusted,
isolated management LAN.

## Install the binary

Set the release and platform. On macOS, `uname -m` reports `x86_64` rather than
the archive's `amd64`, so normalize it:

```sh
VERSION=v0.1.3
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
NAME="oonfeewrt_${VERSION#v}_${OS}_${ARCH}"
BASE="https://github.com/aiden0rchad/oonfeeWRT/releases/download/$VERSION"

curl --fail --location --remote-name "$BASE/$NAME.tar.gz"
curl --fail --location --remote-name "$BASE/SHA256SUMS"
grep "  $NAME.tar.gz\$" SHA256SUMS > "$NAME.sha256"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum --check "$NAME.sha256"
else
  shasum -a 256 --check "$NAME.sha256"
fi
tar -xzf "$NAME.tar.gz"
"$NAME/oonfeewrtd" -version
```

The macOS binaries are not Developer ID signed or notarized.
Use the checksum-verified archive above; do not bypass a checksum mismatch.

Keep the extracted binary in that directory or install it on `PATH`:

```sh
sudo install -m 0755 "$NAME/oonfeewrtd" /usr/local/bin/oonfeewrtd
sudo install -m 0755 "$NAME/oonfeewrt-recoverycheck" \
  /usr/local/bin/oonfeewrt-recoverycheck
```

Create separate private directories for persistent state and the unattended
passphrase. The database and `keyring.json` are created in the data directory.
Keep that directory mode `0700`. v0.1.0 and later force newly created SQLite
database, WAL, and SHM files to mode `0600`. Historical `v0.1.0-rc.1` relied on
the process umask for its sidecars, so keep `umask 077` during an RC upgrade.

```sh
install -d -m 0700 "$HOME/.local/share/oonfeewrt" "$HOME/.config/oonfeewrt"
umask 077
head -c 32 /dev/urandom | base64 > "$HOME/.config/oonfeewrt/passphrase"
chmod 600 "$HOME/.config/oonfeewrt/passphrase"

oonfeewrtd \
  -data-dir "$HOME/.local/share/oonfeewrt" \
  -passphrase-file "$HOME/.config/oonfeewrt/passphrase" \
  -listen 127.0.0.1:8080
```

Open `http://127.0.0.1:8080` and create the first administrator. The generated
passphrase enables unattended restarts: it is no longer an independent second
factor, so protect it with host permissions and backups. Setting
`OONFEE_PASSPHRASE` is rejected; secrets never belong in environment values.

For an interactive start, omit `-passphrase-file`. A terminal prompts twice on
first run and once after each restart.

## Run the container

After the tag workflow succeeds, its immutable release tag is
`ghcr.io/aiden0rchad/oonfeewrt:v0.1.3`. It is multi-platform, defaults to
UID `65532`, and has no shell or package manager. The command below instead uses
your non-root host UID with bind-mounted state, which keeps permissions and
backups straightforward on both Linux and Docker Desktop.

Install `cosign` from the
[official Sigstore instructions](https://docs.sigstore.dev/cosign/system_config/installation/),
then verify the GitHub Actions keyless identity before first use. Stable aliases
`0.1.3`, `0.1`, and `latest` resolve to the same final manifest, but deployments
should pin `v0.1.3` or its reported digest.

```sh
[ "$(id -u)" -ne 0 ] || { echo "run Docker as a non-root user" >&2; exit 1; }
install -d -m 0700 \
  "$HOME/.config/oonfeewrt" \
  "$HOME/.local/share/oonfeewrt-container"
umask 077
head -c 32 /dev/urandom | base64 > "$HOME/.config/oonfeewrt/passphrase"
chmod 600 "$HOME/.config/oonfeewrt/passphrase"
```

Bridge mode works on Linux and Docker Desktop. It intentionally publishes only
to host loopback. The shipped discovery path is a bounded TCP `/ubus` scan, not
ARP or mDNS; a bridge usually does not expose the router's LAN subnet, so adopt
by address.

```sh
cosign verify \
  --certificate-identity "https://github.com/aiden0rchad/oonfeeWRT/.github/workflows/release.yml@refs/tags/v0.1.3" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/aiden0rchad/oonfeewrt:v0.1.3

docker run -d \
  --name oonfeewrt \
  --restart unless-stopped \
  --read-only \
  --user "$(id -u):$(id -g)" \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --stop-timeout 150 \
  --tmpfs "/tmp:rw,noexec,nosuid,nodev,uid=$(id -u),gid=$(id -g),mode=0700" \
  -p 127.0.0.1:8080:8080 \
  --mount "type=bind,source=$HOME/.local/share/oonfeewrt-container,target=/data" \
  --mount "type=bind,source=$HOME/.config/oonfeewrt/passphrase,target=/run/secrets/oonfee-passphrase,readonly" \
  -e OONFEE_DATA_DIR=/data \
  -e OONFEE_LISTEN=:8080 \
  -e OONFEE_PASSPHRASE_FILE=/run/secrets/oonfee-passphrase \
  ghcr.io/aiden0rchad/oonfeewrt:v0.1.3
```

The release archive also includes `docker-compose.yml`. It uses the same final
image, a named data volume, a mode-0600 passphrase file owned by UID 65532, and
the loopback-only bridge mapping above. Host networking is documented in the
file but remains an explicit Linux-only opt-in.

Set the exact published image version when using the current v0.1.3 file:

```sh
OONFEE_VERSION=v0.1.3 docker compose up -d
```

The Compose file in the unreleased v0.1.4 source keeps the loopback default but
accepts `OONFEE_HTTP_BIND=<controller-LAN-IP>` when browsers must connect
directly from a trusted management LAN. This changes host-side publishing and
may be used with an exact compatible candidate or release image tag.
`OONFEE_HTTP_BIND=0.0.0.0` explicitly publishes on every host IPv4 interface;
it is not a browser URL. Prefer one concrete host IP, use a literal IP during
first-owner setup, enforce firewall policy, and do not expose the controller's
unencrypted port 8080 to the Internet.

On Linux, host networking lets the on-demand subnet scanner enumerate eligible
host LAN interfaces. Replace the `-p` line with `--network host` and set
`-e OONFEE_LISTEN=127.0.0.1:8080`. The scanner performs a bounded TCP probe for
an unauthenticated OpenWrt `/ubus` endpoint; it does not implement ARP or mDNS.
An ordinary bridged container may not see a host-LAN subnet to scan, so use
add-by-address there. Adoption, polling, Preview and Apply work over routed L3
in either mode.

## Put TLS in front

Keep the controller on loopback and proxy it from the same host. A minimal Caddy
site is:

```text
oonfeewrt.example.internal {
    reverse_proxy 127.0.0.1:8080
}
```

Use a name and certificate trusted by your clients. Preserve WebSocket upgrades
and `X-Forwarded-Proto: https`; Caddy does both by default. The controller then
marks session cookies `Secure`. Do not expose port 8080 directly to the Internet.

## Back up and upgrade

`oonfeewrt.db` and its sibling `keyring.json` are one recovery unit. A copied
database without its matching keyring cannot reveal sealed credentials, even
with the passphrase. Back up the passphrase separately.

For a live binary install, use SQLite's backup API rather than copying a
WAL-mode database file directly:

```sh
install -d -m 0700 /path/to/private-backup
sqlite3 "$HOME/.local/share/oonfeewrt/oonfeewrt.db" \
  ".backup '/path/to/private-backup/oonfeewrt.db'"
cp -p "$HOME/.local/share/oonfeewrt/keyring.json" \
  /path/to/private-backup/keyring.json
```

Verify a restore pair before depending on it:

```sh
OONFEE_PASSPHRASE_FILE="$HOME/.config/oonfeewrt/passphrase" \
  oonfeewrt-recoverycheck /path/to/private-backup/oonfeewrt.db
```

For a container install, stop it cleanly before copying the bind-mounted state.
This avoids requiring SQLite tooling inside the scratch image:

```sh
docker stop --time 150 oonfeewrt
install -d -m 0700 /path/to/private-backup
cp -p "$HOME/.local/share/oonfeewrt-container/oonfeewrt.db" \
  "$HOME/.local/share/oonfeewrt-container/keyring.json" \
  /path/to/private-backup/
docker start oonfeewrt
```

Verify the copied pair with the release archive's `oonfeewrt-recoverycheck`
before restarting after a real restore.

To upgrade, retain that backup, stop the old process cleanly, replace the binary
or container tag, and restart with the same data volume and passphrase file. The
controller migrates its database on startup and refuses an unsupported downgrade.

### Upgrade from v0.1.2 and roll back

v0.1.2 and v0.1.3 both use schema 19. The upgrade needs no database migration
and makes no router change. Before upgrading, preserve the matching database,
keyring, and passphrase recovery set. A clean rollback to v0.1.2 is
schema-compatible; retain the v0.1.3 data pair first.

v0.1.3 adds one read-only installed-route observation to the existing slower
network/topology collection cycle. Its exact
`/sbin/ip -4 route show table all` command pattern was already present in the
scoped controller ACL shipped before this release, so adopted devices do not
need re-adoption or an ACL refresh. The controller does not change route metrics,
PPPoE, firewall, failover or policy routing during this upgrade.

### Upgrade from v0.1.0-rc.1 and roll back

`v0.1.0-rc.1` uses schema 17; `v0.1.0` migrates it to schema 19. Before the
upgrade, stop the RC cleanly and copy its database and matching keyring. Verify
that pair with the RC archive's `oonfeewrt-recoverycheck`, then retain it without
opening it with the final daemon. Start v0.1.0 with a copy of the same data pair
and unchanged passphrase file.

Rollback is a data restore, not merely an image-tag change: stop v0.1.0, retain
the schema-19 state separately, restore the untouched schema-17 database and
matching keyring, then restart `v0.1.0-rc.1` with its prior passphrase. Never
point the RC daemon at a schema-19 database. Controller migration and rollback
make no router request and do not revert router configuration.

## Portable backup and restore

v0.1.0 automates the paired recovery workflow above. An owner can export one encrypted native
`.oowrtbak` containing a consistent live-WAL database snapshot and matching
wrapped key material from **Settings → Backup & Restore**. The artifact includes
controller state only; it is not a backup of foreign or unmanaged router UCI.

Use TLS through the trusted reverse proxy above, or access the controller
directly on loopback. Export start/download and restore mutations require recent
account-password reauthentication. Choose a separate export passphrase of at
least 16 Unicode characters and at most 4096 UTF-8 bytes; it is not stored by
the controller. Restore accepts the raw `.oowrtbak`, authenticates and migrates
only a disposable copy, and shows manifest identity, source/target schema and
recovery counts before any replacement.

Confirmation requires that export passphrase again, the current controller
runtime/boot passphrase, exact `RESTORE CONTROLLER`, and all four safety
acknowledgements. The runtime passphrase is not the signed-in account password.
For a container, keep `/data` on the same persistent volume and keep the same
protected `OONFEE_PASSPHRASE_FILE` across ordinary restarts; otherwise the
database/keyring pair will not reopen. The confirmed restore uses a controlled
in-process restart, not an unattended router operation.

Before replacement, the controller retains the prior pair as a mode-0600
encrypted artifact at
`<data-dir>/.oonfeewrt-recovery/safety-<restore-id>.oowrtbak`, protected by the
same export passphrase used for confirmation. It is not automatically expired
by time. After the applied-restore audit receipt is cleared, retention targets
three recognized safety artifacts, fills available slots newest-first and
prunes the rest. Artifacts referenced by an active restore marker, receipt or
suppression record are always preserved, even if that temporarily exceeds
three. For longer retention, copy one to protected backup storage
before pruning and document its passphrase separately. It can be uploaded through
the same staged restore workflow later.

Successful restore revokes all controller sessions. Restored desired state is
never automatically applied to routers, and router writes remain persistently
suppressed until an owner reauthenticates and enters exact
`RESUME ROUTER WRITES`. Read-only monitoring of restored devices may resume
after restart while that gate remains active. Removing it immediately
re-enables automatic 802.11k neighbour maintenance; the reconciler may write
hostapd RRM neighbour state even though restored desired configuration is not
automatically Applied. Review the restored inventory and intent before
removing the gate.

## First adoption

1. Set a root password in LuCI before adoption. A factory-default passwordless
   OpenWrt account is unsafe.
2. Open **Devices**, scan or add the router by management address, and inspect
   the reported model, firmware, SSH host key, and warnings.
3. Read the controller-access-payload disclosure. Select it only if you accept
   the ACL file and scoped-login changes described above.
4. Enter the router credential only when prompted. Review the read-only
   inspection and, when sharing hardware evidence, use its sanitized
   compatibility-report export rather than the raw response.
5. Leave LLDP off unless measured physical topology is worth installing an
   official router package. If enabled, review every package and interface plan.

The controller never needs a custom OpenWrt image or controller-authored router
package. See [`FRESH-START-VALIDATION.md`](FRESH-START-VALIDATION.md) in this
release archive for the factory-reset proof and known evidence gaps.
