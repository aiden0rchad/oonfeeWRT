#!/bin/sh
# Fails if the UI bundle exceeds the budget in DEVICE-BUDGET §8.
#
# The budget is about the BROWSER, not the controller host: 1.5 MB gzipped is
# what a phone on a bad link can load without the page feeling broken. It is a
# ceiling, not a target — the current build is an order of magnitude under it,
# and the check exists so that stops being true on purpose rather than by
# accident.
set -eu
LIMIT_KB=1536
cd "$(dirname "$0")/.."

[ -f ui/dist/index.html ] || { echo "budget_check: no UI build; run: npm --prefix ui run build" >&2; exit 1; }

total=0
for f in $(find ui/dist -type f \( -name '*.js' -o -name '*.css' -o -name '*.html' \)); do
  kb=$(gzip -9 -c "$f" | wc -c | tr -d ' ')
  total=$((total + kb))
done
total_kb=$((total / 1024))
printf 'UI bundle: %d KB gzipped (limit %d KB)\n' "$total_kb" "$LIMIT_KB"
if [ "$total_kb" -gt "$LIMIT_KB" ]; then
  echo "budget_check: over budget" >&2
  exit 1
fi
