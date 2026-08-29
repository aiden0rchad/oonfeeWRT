# Install the standalone binary

The standalone release contains one static controller executable with the web UI embedded. Docker is not required.

> **Outcome:** A checksum-verified oonfeeWRT v0.1.2 binary runs on a supported Linux or macOS host, stores private state outside the program directory, and answers only on loopback.

## Prerequisites

- A 64-bit `linux/amd64`, `linux/arm64`, `darwin/amd64`, or `darwin/arm64` host.
- `curl`, `tar`, and either `sha256sum` or `shasum`.
- Network reachability to each router's management address.
- A private directory for controller state and a separate private passphrase file.

The release is built with Go 1.26.6 and includes the UI; the host does not need Go or Node.js.

**Write impact:** Installing the executable writes files on the controller host only. A first start with new state does not contact a router. Starting with existing adopted-device state resumes polling and may resume automatic runtime 802.11k neighbour maintenance.

## 1. Select and download the correct archive

Run the following in a clean download directory:

```sh
VERSION=v0.1.2
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
```

This normalizes macOS `x86_64` to the release archive's `amd64` name.

## 2. Verify the checksum before extracting

```sh
grep "  $NAME.tar.gz\$" SHA256SUMS > "$NAME.sha256"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum --check "$NAME.sha256"
else
  shasum -a 256 --check "$NAME.sha256"
fi
```

Continue only when the command reports that the archive is valid. Do not bypass a mismatch or use an archive whose filename is absent from the release checksum file.

The macOS binaries are not Developer ID signed or notarized. The checksum-verified GitHub release is the authenticity path documented for v0.1.2.

## 3. Extract and inspect the version

```sh
tar -xzf "$NAME.tar.gz"
"$NAME/oonfeewrtd" -version
```

Expected output:

```text
v0.1.2
```

The archive also includes `oonfeewrt-recoverycheck`, installation/validation documentation, notices, and the release Compose file.

## 4. Install the executables

You may run from the extracted directory. To put both supported executables on `PATH`:

```sh
sudo install -m 0755 "$NAME/oonfeewrtd" /usr/local/bin/oonfeewrtd
sudo install -m 0755 "$NAME/oonfeewrt-recoverycheck" \
  /usr/local/bin/oonfeewrt-recoverycheck
```

Verify the installed daemon:

```sh
oonfeewrtd -version
```

## 5. Create private state and passphrase paths

```sh
install -d -m 0700 "$HOME/.local/share/oonfeewrt" "$HOME/.config/oonfeewrt"
umask 077
head -c 32 /dev/urandom | base64 > "$HOME/.config/oonfeewrt/passphrase"
chmod 600 "$HOME/.config/oonfeewrt/passphrase"
```

The generated file unlocks the controller keyring on unattended restarts. It is not a browser password. Because the daemon can read it without a person, host file permissions become the protection boundary.

Back up this passphrase, the data directory's `oonfeewrt.db`, and its matching `keyring.json`. The passphrase cannot recreate a lost keyring, and a keyring from another controller cannot open this database.

## 6. Start the controller on loopback

```sh
oonfeewrtd \
  -data-dir "$HOME/.local/share/oonfeewrt" \
  -passphrase-file "$HOME/.config/oonfeewrt/passphrase" \
  -listen 127.0.0.1:8080
```

Keep it in the foreground for the first setup so startup errors remain visible. The daemon logs to standard error and also maintains a private bounded `controller.jsonl` log family inside the data directory.

For an interactive passphrase instead, omit `-passphrase-file`. First run prompts twice; later starts prompt once. Interactive startup requires a terminal.

Do not set `OONFEE_PASSPHRASE`. The daemon rejects a passphrase in the environment because environment values can leak through process inspection, child processes, crash reports, and container metadata.

## 7. Verify and create the first owner

In another terminal:

```sh
curl --fail http://127.0.0.1:8080/healthz
```

Expected output:

```text
ok
```

Open [http://127.0.0.1:8080](http://127.0.0.1:8080), create the first owner account, and sign in.

## Supported flags and environment

```text
-data-dir PATH
-listen ADDRESS
-passphrase-file PATH
-log-level debug|info|warn|error
-version
-healthcheck
```

The equivalent non-secret environment variables are:

```text
OONFEE_DATA_DIR
OONFEE_LISTEN
OONFEE_PASSPHRASE_FILE
```

`-data-dir` must be absolute. Its default is `/data`; the documented standalone command overrides it deliberately. The default listener is `:8080`, which is not loopback-only, so always pass the intended bind address.

## Troubleshooting and recovery

### `data directory ... must be an absolute path`

Pass an absolute path. `$PWD/data` is acceptable because the shell expands it before starting the daemon.

### `stdin is not a terminal` and no passphrase is available

The process is unattended. Add:

```sh
-passphrase-file /absolute/path/to/passphrase
```

or set `OONFEE_PASSPHRASE_FILE` to that absolute path.

### The passphrase file is not accepted

It must be a non-empty regular file with no group or world permission bits:

```sh
chmod 600 "$HOME/.config/oonfeewrt/passphrase"
```

Only one final newline is stripped. Other leading or trailing spaces are part of the passphrase.

### The key cannot be unwrapped

The passphrase is wrong, or `keyring.json` is corrupt or truncated. Restore the matching database, keyring, and passphrase backup. There is no reset that preserves sealed router credentials.

### `keyring.json` is missing beside an existing database

Startup refuses to create an unrelated keyring. Restore the matching keyring. Moving the database aside starts an empty controller and requires re-adoption; do that only as a deliberate reset.

### The database is newer than the daemon

The daemon refuses an unsupported downgrade. Restore a backup made for the older schema or run the compatible newer release. See [Upgrades and rollback](upgrades.md).

### The UI says the binary was built without it

Official release binaries embed the UI. For a local source build, run:

```sh
make build
```

The `make build` target installs UI dependencies, builds the embedded UI, and then builds the daemon.

### Another program uses port 8080

Choose a different loopback port:

```sh
oonfeewrtd \
  -data-dir "$HOME/.local/share/oonfeewrt" \
  -passphrase-file "$HOME/.config/oonfeewrt/passphrase" \
  -listen 127.0.0.1:8081
```

## Next steps

- [Adopt the first device](../getting-started/first-adoption.md)
- [Add trusted HTTPS](reverse-proxy.md)
- [Create and verify a backup](../operations/backups.md)
- [Plan upgrades and rollback](upgrades.md)
