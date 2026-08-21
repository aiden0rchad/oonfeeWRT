#!/bin/sh
# Verify that one clean source tree produces one versioned controller binary.
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

for entry in .git/ .run/ data/ '**/passphrase' '**/keyring.json' '**/*.db' '**/*.db-*' '**/*.key' '**/*.pem' '.env' '.env.*' '**/.env' '**/.env.*' '**/node_modules/' ui/dist/; do
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
grep -Fq "golang:$toolchain-alpine AS build" deploy/Dockerfile || {
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
npm --prefix ui ci

tmp=$(mktemp -d "${TMPDIR:-/tmp}/oonfeewrt-release.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

build() {
  CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X main.version=$version" \
    -o "$1" ./cmd/oonfeewrtd
}

npm --prefix ui run build
test -f ui/dist/index.html || {
  echo "release check: UI build produced no index.html" >&2
  exit 1
}
build "$tmp/first"

npm --prefix ui run build
test -f ui/dist/index.html || {
  echo "release check: UI rebuild produced no index.html" >&2
  exit 1
}
build "$tmp/second"
cmp -s "$tmp/first" "$tmp/second" || {
  echo "release check: identical inputs produced different binaries" >&2
  exit 1
}

actual=$("$tmp/first" -version)
[ "$actual" = "$version" ] || {
  echo "release check: embedded version is $actual, expected $version" >&2
  exit 1
}

if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$tmp/first"
else
  shasum -a 256 "$tmp/first"
fi
echo "release check: reproducible $version"
