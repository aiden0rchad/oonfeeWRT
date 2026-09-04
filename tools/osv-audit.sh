#!/bin/sh
# Scan one lockfile with a pinned, checksum-verified OSV-Scanner release.
set -eu

if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
  echo "usage: tools/osv-audit.sh <existing-lockfile>" >&2
  exit 2
fi

version=2.4.0
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)
    asset=osv-scanner_linux_amd64
    checksum=15314940c10d26af9c6649f150b8a47c1262e8fc7e17b1d1029b0e479e8ed8a0
    ;;
  Linux-aarch64|Linux-arm64)
    asset=osv-scanner_linux_arm64
    checksum=44e580752910f0ff36ec99aff59af20f65df1e859aa31e5605a8f0d055b496e9
    ;;
  Darwin-x86_64)
    asset=osv-scanner_darwin_amd64
    checksum=088119325156321c34c456ac3703d6013538fd71cbac82b891ab34db491e4d66
    ;;
  Darwin-arm64)
    asset=osv-scanner_darwin_arm64
    checksum=9ca3185ad63e9ab54f7cb90f46a7362be02d80e37f0123d095a54355ea202f5d
    ;;
  *)
    echo "osv-audit: unsupported platform $(uname -s)-$(uname -m)" >&2
    exit 2
    ;;
esac

umask 077
temp_root=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
scan_dir=$(mktemp -d "$temp_root/oonfeewrt-osv.XXXXXX")
case "$scan_dir" in
  "$temp_root"/oonfeewrt-osv.*) ;;
  *) echo "osv-audit: unsafe temporary path" >&2; exit 2 ;;
esac
trap 'find "$scan_dir" -depth -delete' EXIT
trap 'exit 130' HUP INT TERM

scanner=$scan_dir/$asset
curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 120 \
  --retry 3 --retry-all-errors \
  --output "$scanner" \
  "https://github.com/google/osv-scanner/releases/download/v$version/$asset"

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$scanner" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$scanner" | awk '{print $1}')
fi
if [ "$actual" != "$checksum" ]; then
  echo "osv-audit: checksum mismatch for $asset" >&2
  exit 1
fi

chmod 0700 "$scanner"
"$scanner" scan --lockfile="$1"
