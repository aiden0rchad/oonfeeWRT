# Install oonfeeWRT

oonfeeWRT is a controller that runs on a computer, NAS, or server. It does not
replace OpenWrt firmware and no controller binary runs on a router.

This guide targets the `v0.1.0-rc.1` release candidate. Use its artifacts only
after [GitHub Releases](https://github.com/aiden0rchad/oonfeeWRT/releases) lists
that tag; source documentation alone does not prove publication succeeded. Back
up both the controller and each router before using it on a network you cannot
afford to interrupt.

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
VERSION=v0.1.0-rc.1
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

The release-candidate macOS binaries are not Developer ID signed or notarized.
Use the checksum-verified archive above; do not bypass a checksum mismatch.

Keep the extracted binary in that directory or install it on `PATH`:

```sh
sudo install -m 0755 "$NAME/oonfeewrtd" /usr/local/bin/oonfeewrtd
sudo install -m 0755 "$NAME/oonfeewrt-recoverycheck" \
  /usr/local/bin/oonfeewrt-recoverycheck
```

Create separate private directories for persistent state and the unattended
passphrase. The database and `keyring.json` are created in the data directory.

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

After the tag workflow succeeds, its image is
`ghcr.io/aiden0rchad/oonfeewrt:v0.1.0-rc.1`. It is multi-platform, defaults to
UID `65532`, and has no shell or package manager. The command below instead uses
your non-root host UID with bind-mounted state, which keeps permissions and
backups straightforward on both Linux and Docker Desktop.

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
to host loopback. Layer-2 discovery does not cross the bridge; adopt by address.

```sh
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
  ghcr.io/aiden0rchad/oonfeewrt:v0.1.0-rc.1
```

On Linux, full ARP/mDNS discovery requires host networking. Replace the `-p`
line with `--network host` and set
`-e OONFEE_LISTEN=127.0.0.1:8080`. Add-by-address works in either mode.

## Put TLS in front

Keep the controller on loopback and proxy it from the same host. A minimal Caddy
site is:

```caddyfile
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

## First adoption

1. Set a root password in LuCI before adoption. A factory-default passwordless
   OpenWrt account is unsafe.
2. Open **Devices**, scan or add the router by management address, and inspect
   the reported model, firmware, SSH host key, and warnings.
3. Read the controller-access-payload disclosure. Select it only if you accept
   the ACL file and scoped-login changes described above.
4. Enter the router credential only when prompted. Confirm the post-adoption
   capability report before applying any network configuration.
5. Leave LLDP off unless measured physical topology is worth installing an
   official router package. If enabled, review every package and interface plan.

The controller never needs a custom OpenWrt image or controller-authored router
package. See [`FRESH-START-VALIDATION.md`](FRESH-START-VALIDATION.md) in this
release archive for the factory-reset proof and known evidence gaps.
