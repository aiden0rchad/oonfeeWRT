#!/bin/sh
# Build the embedded UI once, then package deterministic static binaries.
set -eu

cd "$(dirname "$0")/.."

version=${1:-}
out=${2:-dist}
case "$version" in
  ''|*[!A-Za-z0-9._+-]*)
    echo "usage: tools/release-build.sh <version> [empty-output-directory]" >&2
    exit 2
    ;;
esac
if [ -L "$out" ] || { [ -d "$out" ] && [ -n "$(find "$out" -mindepth 1 -maxdepth 1 -print -quit)" ]; }; then
  echo "release build: output must be a new or empty directory: $out" >&2
  exit 1
fi

epoch=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || date +%s)}
case "$epoch" in
  ''|*[!0-9]*) echo "release build: SOURCE_DATE_EPOCH must be an integer" >&2; exit 2 ;;
esac

mkdir -p "$out"
out=$(cd "$out" && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/oonfeewrt-package.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

npm --prefix ui ci
npm --prefix ui run build
test -f ui/dist/index.html || {
  echo "release build: UI build produced no index.html" >&2
  exit 1
}

for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  os=${target%-*}
  arch=${target#*-}
  name="oonfeewrt_${version#v}_${os}_${arch}"
  stage="$tmp/$name"
  mkdir "$stage"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X main.version=$version" \
    -o "$stage/oonfeewrtd" ./cmd/oonfeewrtd
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid=" \
    -o "$stage/oonfeewrt-recoverycheck" ./tools/recoverycheck
  cp LICENSE NOTICE docs/INSTALL.md docs/FRESH-START-VALIDATION.md "$stage/"

  python3 - "$stage" "$out/$name.tar.gz" "$epoch" "$name" <<'PY'
import gzip
import pathlib
import sys
import tarfile

stage, archive, epoch, prefix = pathlib.Path(sys.argv[1]), sys.argv[2], int(sys.argv[3]), sys.argv[4]
with open(archive, "wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch) as gz:
    with tarfile.open(fileobj=gz, mode="w", format=tarfile.USTAR_FORMAT) as tar:
        root = tarfile.TarInfo(prefix)
        root.type, root.mode, root.mtime = tarfile.DIRTYPE, 0o755, epoch
        tar.addfile(root)
        for path in sorted(stage.iterdir(), key=lambda item: item.name):
            info = tar.gettarinfo(str(path), f"{prefix}/{path.name}")
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            info.mtime = epoch
            info.mode = 0o755 if path.name in {"oonfeewrtd", "oonfeewrt-recoverycheck"} else 0o644
            with path.open("rb") as source:
                tar.addfile(info, source)
PY
done

(
  cd "$out"
  if command -v sha256sum >/dev/null 2>&1; then
    LC_ALL=C sha256sum ./*.tar.gz | sed 's| \./| |' > SHA256SUMS
  else
    LC_ALL=C shasum -a 256 ./*.tar.gz | sed 's| \./| |' > SHA256SUMS
  fi
)

printf 'release build: %s\n' "$out"
