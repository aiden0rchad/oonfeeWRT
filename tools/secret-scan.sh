#!/bin/sh
# Look for credentials in the tree and in git history.
#
# Written after one was found in both: the lab AP's live WPA passphrase, read
# off the device with `uci get` and committed to a PUBLIC repository — inside
# the test whose subject is that passphrases do not leak. Removing it from the
# tree does not unpublish it, so the only real remedy was rotating the key.
#
# Pattern-based on purpose. A scanner that hardcodes the secrets it looks for
# has to contain them, which is the problem it exists to prevent.
#
#   tools/secret-scan.sh          # working tree and history
#   tools/secret-scan.sh --tree   # working tree only (fast)
set -eu
cd "$(dirname "$0")/.."
status=0

# Values that are obviously placeholders. Kept as one list so a reviewer can see
# exactly what is being excused, rather than the exclusions being scattered.
FAKE='not-a-real|plaintext-passphrase|hunter2|s3cret|example|placeholder|redacted|the-routers-admin|timing-equaliser|a-sufficiently-long|a-mesh-passphrase|mesh-health-check|<redacted>'

say() { printf '%s\n' "$*"; }

# Case-insensitive. The first version was not, so `devicePassword = "..."` —
# the exact shape a Go constant takes — went straight through a scanner that
# reported "clean". Verified by planting one before trusting the result.
say "== assignments that look like a credential =="
if grep -rniE '(passphrase|password|psk|secret|token|api[_-]?key)[[:space:]]*[:=][[:space:]]*"[^"]{10,}"' \
     --include='*.go' --include='*.ts' --include='*.tsx' --include='*.py' \
     --include='*.sh' --include='*.json' --include='*.md' . 2>/dev/null \
   | grep -v node_modules | grep -viE "$FAKE"; then
  say "  ^ each of these must be a placeholder, or it does not belong here"
  status=1
else
  say "  none"
fi

say ""
say "== files that should never be tracked =="
if git ls-files | grep -iE '(^|/)(id_rsa|.*\.pem|.*\.key|keyring\.json|\.env)$'; then
  status=1
else
  say "  none"
fi

say ""
say "== runtime state is ignored =="
for f in .run/keyring.json .run/oonfeewrt.db; do
  if git ls-files --error-unmatch "$f" >/dev/null 2>&1; then
    say "  TRACKED: $f"
    status=1
  fi
done
[ "$status" = 0 ] && say "  none tracked"

if [ "${1:-}" != "--tree" ]; then
  say ""
  say "== ever added in history =="
  # History is the half that matters: a secret removed from the tree is still
  # published, and every clone and fork already has it.
  # Captured rather than piped into `if`. A pipeline's status is its LAST
  # command, so ending this in `sort` made it report a finding every time it
  # ran, with nothing listed — a scanner that cries wolf is one people switch
  # off, which is the same failure as the watchdog in STATUS §5z.
  hits=$(git log -p --all 2>/dev/null | grep -E '^\+' \
     | grep -iE '(passphrase|password|psk|secret|token)[[:space:]]*[:=][[:space:]]*"[^"]{10,}"' 2>/dev/null \
     | grep -viE "$FAKE" | sort -u || true)
  if [ -n "$hits" ]; then
    printf '%s\n' "$hits"
    say "  ^ present in history. Removing it from the tree does NOT unpublish it:"
    say "    treat the value as compromised and rotate it."
    status=1
  else
    say "  none"
  fi
fi

say ""
if [ "$status" = 0 ]; then
  say "clean"
else
  say "FINDINGS ABOVE"
fi
exit "$status"
