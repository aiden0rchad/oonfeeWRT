# Quick start

This page starts oonfeeWRT v0.1.2 on one host and creates the first owner account. It offers a standalone-binary path and a Docker Compose path; use only one.

> **Outcome:** The controller answers at `http://127.0.0.1:8080`, `/healthz` returns `ok`, and you can sign in as the first owner.

## Before you begin

You need:

- a supported 64-bit Linux or macOS host;
- network reachability from that host to the OpenWrt management address;
- either Docker with Compose support or the v0.1.2 release archive;
- a private place to retain the controller runtime passphrase and data.

**Write impact:** With a new data directory or volume, these steps write only to the controller host and do not contact a router. Reusing existing controller state resumes adopted-device polling and may resume automatic runtime 802.11k neighbour maintenance. Router access on a fresh controller begins when you explicitly inspect or adopt a device.

## Option A: standalone binary

Follow [Install the binary](../installation/binary.md) to download and checksum-verify the correct release. Then start it with a private data directory:

```sh
install -d -m 0700 "$PWD/data"
./oonfeewrtd -data-dir "$PWD/data" -listen 127.0.0.1:8080
```

On the first interactive start, the daemon asks twice for a new runtime passphrase. There is no recovery if it is lost.

Keep this terminal open while completing the browser setup. For unattended startup, stop the daemon with `Ctrl-C` and configure the mode-`0600` passphrase file described in the [binary installation guide](../installation/binary.md).

## Option B: Docker Compose

Create a private working directory, download the exact v0.1.2 Compose file, and create the runtime passphrase:

```sh
mkdir -p oonfeewrt
cd oonfeewrt

curl --fail --location \
  --output docker-compose.yml \
  https://raw.githubusercontent.com/aiden0rchad/oonfeeWRT/v0.1.2/deploy/docker-compose.yml

umask 077
head -c 32 /dev/urandom | base64 > passphrase
sudo chown 65532:65532 passphrase
sudo chmod 600 passphrase

OONFEE_VERSION=v0.1.2 docker compose up -d
```

The supplied Compose file:

- publishes the controller only on `127.0.0.1:8080`;
- runs as UID/GID 65532;
- uses a read-only root filesystem;
- drops all Linux capabilities and disables privilege gain;
- stores controller state in the named volume `oonfee-data`;
- reads the runtime passphrase from `./passphrase`.

Do not run `docker compose down -v` unless you intend to delete the named controller-data volume.

## Create the first owner

1. On the controller host, open [http://127.0.0.1:8080](http://127.0.0.1:8080).
2. Enter a username. Controller usernames are 1–64 ASCII characters, must start with a letter or digit, and may contain letters, digits, `.`, `_`, or `-`.
3. Enter a password of at least 12 characters and confirm it.
4. Complete setup. The first account becomes the owner and is signed in.

The account password is not the runtime passphrase. Keep both in your password-management and recovery plan.

## Verify the controller

From the controller host:

```sh
curl --fail http://127.0.0.1:8080/healthz
```

Expected output:

```text
ok
```

For Docker, also check:

```sh
OONFEE_VERSION=v0.1.2 docker compose ps
OONFEE_VERSION=v0.1.2 docker compose logs --tail=100 oonfeewrt
```

In the browser, confirm that the left navigation shows **Dashboard**, **Topology**, **Radios**, **Devices**, **Client Devices**, **Policy Engine**, **Settings**, **Adopt a device**, and **Logs**.

## If it does not start

### The daemon says no passphrase is available

An unattended process has no terminal from which to prompt. Configure `-passphrase-file` or `OONFEE_PASSPHRASE_FILE` with a regular file that is not readable by group or other users.

### The passphrase file is rejected

For a standalone install:

```sh
chmod 600 /absolute/path/to/passphrase
```

For the supplied Compose file, it must also be readable by UID 65532:

```sh
sudo chown 65532:65532 passphrase
sudo chmod 600 passphrase
```

### The keyring cannot be unlocked

The runtime passphrase is wrong, or `keyring.json` is corrupt or truncated. Do not delete it and restart: a replacement keyring cannot decrypt the existing database. Restore the matching database, keyring, and passphrase backup.

### The database exists but `keyring.json` does not

Startup intentionally refuses this state. Restore the matching keyring. Move the database aside only if you deliberately want a new, empty controller and accept re-adopting every device.

### Port 8080 is already in use

Choose another loopback port for a standalone process, for example:

```sh
./oonfeewrtd -data-dir "$PWD/data" -listen 127.0.0.1:8081
```

Then open `http://127.0.0.1:8081`. If using Compose, change the host side of `127.0.0.1:8080:8080` and keep the container side at `8080`.

### The browser is on another computer

`127.0.0.1` is reachable only on the controller host. Do not expose raw port 8080 to the Internet. Configure [reverse-proxy TLS](../installation/reverse-proxy.md), or use a deliberate isolated management network.

## Next steps

- [Adopt the first OpenWrt device](first-adoption.md)
- [Install with the binary in detail](../installation/binary.md)
- [Install with Docker in detail](../installation/docker.md)
- [Protect access with a reverse proxy](../installation/reverse-proxy.md)
- [Create a recovery backup](../operations/backups.md)
