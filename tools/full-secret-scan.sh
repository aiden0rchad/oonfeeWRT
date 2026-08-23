#!/bin/sh
# Run pinned Gitleaks against non-ignored working-tree files and every local ref.
set -eu

cd "$(dirname "$0")/.."
root=$PWD

if [ "$(git rev-parse --is-shallow-repository)" != "false" ]; then
  echo "full-secret-scan: refusing a shallow repository; fetch full history first" >&2
  exit 2
fi

umask 077
scan_dir=$(mktemp -d /tmp/oonfeewrt-gitleaks.XXXXXX)
case "$scan_dir" in
  /tmp/oonfeewrt-gitleaks.*) ;;
  *) echo "full-secret-scan: unsafe temporary path" >&2; exit 2 ;;
esac
trap 'find "$scan_dir" -depth -delete' EXIT
trap 'exit 130' HUP INT TERM

tree_dir=$scan_dir/tree
mkdir "$tree_dir"
git ls-files --cached --others --exclude-standard -z \
  | perl -0ne 'chomp; print "$_\0" if -f $_ || -l $_' \
  | tar -cf - --null -T - \
  | tar -xf - -C "$tree_dir"

gitleaks() {
  go run github.com/zricethezav/gitleaks/v8@v8.30.1 "$@"
}

status=0
echo "== Gitleaks: current non-ignored tree =="
if ! (cd "$tree_dir" && gitleaks dir \
  --config "$root/.gitleaks.toml" \
  --gitleaks-ignore-path "$root" \
  --redact=100 --no-banner --no-color --verbose \
  --report-format json --report-path "$scan_dir/tree.json" .); then
  status=1
fi

echo ""
echo "== Gitleaks: all refs and history =="
if ! gitleaks git \
  --config "$root/.gitleaks.toml" \
  --gitleaks-ignore-path "$root" \
  --redact=100 --no-banner --no-color --verbose \
  --report-format json --report-path "$scan_dir/history.json" \
  --log-opts='--all -m' .; then
  status=1
fi

exit "$status"
