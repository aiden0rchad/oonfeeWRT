#!/bin/sh
# Verify deterministic release archives, checksums, and packaged executables.
set -eu

cd "$(dirname "$0")/.."

version=${1:-}
case "$version" in
  ''|dev|*[!A-Za-z0-9._+-]*)
    echo "usage: tools/reproducible-build-check.sh <release-version>" >&2
    exit 2
    ;;
esac

dirty=$(git status --porcelain --untracked-files=all)
if [ -n "$dirty" ]; then
  if [ "${OONFEE_RELEASE_ALLOW_DIRTY:-}" != 1 ]; then
    echo "release check: working tree is dirty; commit or stash every change first" >&2
    exit 1
  fi
  version="$version-dirty"
  echo "release check: WARNING: testing dirty tree as $version" >&2
fi

for entry in .git/ .run/ data/ local-backups/ '**/.oonfeewrt-recovery/' \
  '*.oowrtbak' 'oonfeewrt-diagnostics-*.zip' 'diagnostics-*.zip' \
  '**/.oonfeewrt-backup-*.db.tmp' '*-before-factory-reset.tar*' \
  '**/passphrase' '**/keyring.json' '**/*.db' '**/*.db-*' '**/*.key' '**/*.pem' \
  '.env' '.env.*' '**/.env' '**/.env.*' '**/node_modules/' ui/dist/; do
  grep -Fxq "$entry" .dockerignore || {
    echo "release check: .dockerignore must exclude $entry" >&2
    exit 1
  }
done

toolchain=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2 }' go.mod)
[ -n "$toolchain" ] || {
  echo "release check: go.mod must pin a toolchain" >&2
  exit 1
}
grep -Eq "golang:$toolchain-alpine@sha256:[0-9a-f]{64} AS build" deploy/Dockerfile || {
  echo "release check: deploy/Dockerfile Go image does not match go.mod toolchain go$toolchain" >&2
  exit 1
}

go mod verify
tidy_diff=$(go mod tidy -diff)
if [ -n "$tidy_diff" ]; then
  printf '%s\n' "$tidy_diff" >&2
  echo "release check: go.mod/go.sum are not tidy" >&2
  exit 1
fi
tmp=$(mktemp -d "${TMPDIR:-/tmp}/oonfeewrt-release.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
epoch=${SOURCE_DATE_EPOCH:-1700000000}
case "$epoch" in
  ''|*[!0-9]*) echo "release check: SOURCE_DATE_EPOCH must be an integer" >&2; exit 2 ;;
esac

SOURCE_DATE_EPOCH="$epoch" tools/release-build.sh "$version" "$tmp/first"
SOURCE_DATE_EPOCH="$epoch" tools/release-build.sh "$version" "$tmp/second"

cmp -s "$tmp/first/SHA256SUMS" "$tmp/second/SHA256SUMS" || {
  echo "release check: identical inputs produced different SHA256SUMS" >&2
  exit 1
}
for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  os=${target%-*}
  arch=${target#*-}
  archive="oonfeewrt_${version#v}_${os}_${arch}.tar.gz"
  cmp -s "$tmp/first/$archive" "$tmp/second/$archive" || {
    echo "release check: identical inputs produced different $archive files" >&2
    exit 1
  }
  gzip -t "$tmp/first/$archive" "$tmp/second/$archive"
done

verify_checksums() {
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$1" && sha256sum -c SHA256SUMS)
  else
    (cd "$1" && shasum -a 256 -c SHA256SUMS)
  fi
}
verify_checksums "$tmp/first"
verify_checksums "$tmp/second"

host_os=$(go env GOOS)
host_arch=$(go env GOARCH)
host_name="oonfeewrt_${version#v}_${host_os}_${host_arch}"
host_archive="$tmp/first/$host_name.tar.gz"
test -f "$host_archive" || {
  echo "release check: unsupported verification host $host_os/$host_arch" >&2
  exit 1
}
mkdir "$tmp/inspect"
tar -xzf "$host_archive" -C "$tmp/inspect"
controller="$tmp/inspect/$host_name/oonfeewrtd"
recovery="$tmp/inspect/$host_name/oonfeewrt-recoverycheck"
test -x "$controller" && test -x "$recovery" || {
  echo "release check: archive binaries are not executable" >&2
  exit 1
}

actual=$("$controller" -version)
[ "$actual" = "$version" ] || {
  echo "release check: embedded version is $actual, expected $version" >&2
  exit 1
}
if "$recovery" >"$tmp/recovery.out" 2>&1 || ! grep -Fq 'usage: recoverycheck' "$tmp/recovery.out"; then
  echo "release check: recovery checker executable did not report its usage contract" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$tmp/first/SHA256SUMS"
else
  shasum -a 256 "$tmp/first/SHA256SUMS"
fi
echo "release check: reproducible archives for $version"
