# Install with Docker

The v0.1.2 container is a multi-platform Linux image containing one static controller binary, CA roots, licenses, and release material. The final image has no shell or package manager.

> **Outcome:** oonfeeWRT runs as non-root in a hardened container, publishes HTTP only on host loopback, and persists state in a named Docker volume.

## Prerequisites

- Docker with Compose support.
- A Linux amd64/arm64 host or Docker Desktop capable of running the Linux image.
- Permission to run Docker as a non-root host user.
- Controller-host network reachability to each router.
- `curl`, `/dev/urandom`, `base64`, `sudo`, and a private working directory.

**Write impact:** With a new named volume, these steps create host/container state and do not contact a router. Reusing an existing controller volume resumes adopted-device polling and may resume automatic runtime 802.11k neighbour maintenance.

## 1. Create the deployment directory

```sh
mkdir -p oonfeewrt
cd oonfeewrt
```

Keep `docker-compose.yml` and `passphrase` in this private directory. The controller database itself lives in Docker's named `oonfee-data` volume.

## 2. Download the release Compose file

```sh
curl --fail --location \
  --output docker-compose.yml \
  https://raw.githubusercontent.com/aiden0rchad/oonfeeWRT/v0.1.2/deploy/docker-compose.yml
```

The file pins the image version through the required `OONFEE_VERSION` value instead of silently following `latest`.

## 3. Create the runtime passphrase

```sh
umask 077
head -c 32 /dev/urandom | base64 > passphrase
sudo chown 65532:65532 passphrase
sudo chmod 600 passphrase
```

The Compose service runs as UID/GID 65532, so the bind-mounted mode-`0600` file must belong to that UID. A missing source path is an error; Compose is configured not to create an empty directory in its place.

This is the controller runtime/boot passphrase, not an owner account password. Store a protected recovery copy separately.

## 4. Start v0.1.2

```sh
OONFEE_VERSION=v0.1.2 docker compose up -d
```

The release image is:

```text
ghcr.io/aiden0rchad/oonfeewrt:v0.1.2
```

The supplied service hardening includes:

- process UID/GID 65532;
- read-only root filesystem;
- all Linux capabilities dropped;
- `no-new-privileges:true`;
- private `noexec,nosuid,nodev` tmpfs;
- state mounted at `/data`;
- host-loopback mapping `127.0.0.1:8080:8080`;
- 150-second stop grace period for an in-flight rollback/confirm cycle.

## 5. Verify the service

```sh
OONFEE_VERSION=v0.1.2 docker compose ps
OONFEE_VERSION=v0.1.2 docker compose logs --tail=100 oonfeewrt
curl --fail http://127.0.0.1:8080/healthz
```

The health request should print:

```text
ok
```

Open [http://127.0.0.1:8080](http://127.0.0.1:8080) on the controller host and create the first owner.

## Container networking and discovery

The default bridge mode is the safe portable choice. Polling, adoption, and Apply are normal outbound layer-3 connections and work when the container can route to the router.

What bridge mode does not provide is the controller host's LAN layer-2 view:

- add-by-address works;
- routed TCP subnet probing may work;
- ARP and mDNS discovery do not cross the bridge.

On Linux, full layer-2 discovery is an explicit host-network opt-in. In `docker-compose.yml`, remove `ports:`, add:

```yaml
network_mode: host
```

and set:

```yaml
OONFEE_LISTEN: "127.0.0.1:8080"
```

Host mode exposes the daemon directly according to its listen address. Review host firewall and TLS policy before using a non-loopback bind. Docker Desktop users should use add-by-address.

## Optional: verify the published image signature

Install `cosign` using Sigstore's official instructions, then verify the v0.1.2 GitHub Actions identity:

```sh
cosign verify \
  --certificate-identity "https://github.com/aiden0rchad/oonfeeWRT/.github/workflows/release.yml@refs/tags/v0.1.2" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/aiden0rchad/oonfeewrt:v0.1.2
```

Deployments should pin `v0.1.2` or the reported digest. The `0.1.2`, `0.1`, and `latest` aliases may resolve to the same manifest but are not immutable deployment intent.

## Optional: run without Compose

This uses bind-mounted state owned by your non-root host user:

```sh
[ "$(id -u)" -ne 0 ] || { echo "run Docker as a non-root user" >&2; exit 1; }
install -d -m 0700 \
  "$HOME/.config/oonfeewrt" \
  "$HOME/.local/share/oonfeewrt-container"
umask 077
head -c 32 /dev/urandom | base64 > "$HOME/.config/oonfeewrt/passphrase"
chmod 600 "$HOME/.config/oonfeewrt/passphrase"

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
  ghcr.io/aiden0rchad/oonfeewrt:v0.1.2
```

## Stop and restart safely

Compose sends SIGTERM and allows 150 seconds for shutdown:

```sh
OONFEE_VERSION=v0.1.2 docker compose stop
OONFEE_VERSION=v0.1.2 docker compose start
```

The longer grace period lets an Apply that reached OpenWrt's rollback-protected stage finish its confirm decision. Do not force-kill the container during an Apply unless the host itself is failing.

## Troubleshooting and recovery

### The passphrase mount is denied

For the supplied Compose service:

```sh
sudo chown 65532:65532 passphrase
sudo chmod 600 passphrase
```

Confirm that `passphrase` is a file, not a directory.

### The container repeatedly restarts

Inspect the startup error:

```sh
OONFEE_VERSION=v0.1.2 docker compose logs --tail=200 oonfeewrt
```

Common causes are a missing/unreadable passphrase, the wrong passphrase for the volume's keyring, a missing keyring beside an existing database, an unsupported database downgrade, or port 8080 already in use.

### Discovery finds no router

Use **Adopt a device** and enter the router IP. Empty layer-2 discovery is expected in bridge mode and on Docker Desktop.

### The controller appears empty after recreating the service

Check that the same `oonfee-data` volume is mounted at `/data`. A new volume creates a new controller. Do not copy only `oonfeewrt.db`; restore its matching `keyring.json` and use a consistent snapshot.

### The browser cannot connect from another computer

The default mapping is intentionally loopback-only. Add [trusted reverse-proxy TLS](reverse-proxy.md) or deliberately bind on an isolated management LAN. Do not publish raw port 8080 to the Internet.

### Avoid accidental deletion

This removes the container but preserves the named volume:

```sh
OONFEE_VERSION=v0.1.2 docker compose down
```

This also deletes the named volume and controller state:

```sh
OONFEE_VERSION=v0.1.2 docker compose down -v
```

Use `-v` only after a verified backup and only when deletion is intentional.

## Next steps

- [Adopt the first device](../getting-started/first-adoption.md)
- [Add trusted HTTPS](reverse-proxy.md)
- [Back up and restore the controller](../operations/backups.md)
- [Upgrade or roll back](upgrades.md)
