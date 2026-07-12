#!/usr/bin/env bash
# ============================================================================
# Pandion binary-upload e2e (sync mode "binaries"): build a binary LOCALLY, ship
# it to a node as-is (no remote build), and run it — proving you can deploy a
# prebuilt artifact. Also proves the key behavior: `binaries` mode does NOT apply
# .gitignore, so gitignored build output is uploaded (unlike source mode).
# Self-cleaning.  Usage:  ./scripts/e2e_binaries.sh [provider]   (default digitalocean)
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

PROV="${1:-digitalocean}"
ID="e2e-binaries-$PROV"
BIN="./bin/pandion"
DIST="$(mktemp -d)"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -rf "$DIST"
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# Build the artifact LOCALLY (operator + node are both Ubuntu; prefer static to be safe).
cat > "$DIST/app.c" <<'C'
#include <stdio.h>
int main(void){ puts("HELLO_FROM_PREBUILT_BINARY"); return 0; }
C
( cd "$DIST" && { gcc -static -O2 app.c -o app 2>/dev/null || gcc -O2 app.c -o app; } )
rm -f "$DIST/app.c"
# A wrong-architecture binary to trip the arch guard (arm64 ELF; we never run it).
GOOS=linux GOARCH=arm64 go build -o "$DIST/app_arm64" ./cmd/pandion 2>/dev/null && c_ok "cross-built arm64 fixture" || c_in "arm64 cross-build unavailable (arch-guard assert will be skipped)"
# A .gitignore that excludes the binary — binaries mode must upload it ANYWAY.
printf 'app\n*.bin\n' > "$DIST/.gitignore"
file "$DIST/app" 2>/dev/null | sed 's/^/  /' || true

c_in "[1] up --sync-mode binaries: upload the prebuilt binary as-is, no remote build..."
OUT=$(timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID" \
  --workspace "$DIST" --sync-mode binaries --ttl 20m --max-cost 1.00 \
  -- 'ls -1; echo "RUNUSER=$(whoami)"; ./app' 2>&1)
echo "---------------- streamed ----------------"; echo "$OUT" | tail -25; echo "------------------------------------------"

echo "$OUT" | grep -q "syncing .* files ->" && c_ok "workspace archived + streamed to node" || c_no "sync step missing"
echo "$OUT" | grep -qi "building" && c_no "a remote build ran (binaries mode must NOT build)" || c_ok "no remote build ran (binaries mode)"
echo "$OUT" | grep -q "HELLO_FROM_PREBUILT_BINARY" && c_ok "prebuilt binary uploaded + executed" || c_no "binary did not run"
# the binary is gitignored; its presence in `ls` proves .gitignore was NOT applied
# (the stream prefixes each line with "[node] "). Its successful run above already
# implies upload, but assert the listing too.
echo "$OUT" | grep -qE '(^|\] )app$' && c_ok "gitignored binary WAS uploaded (binaries mode ignores .gitignore)" || c_no "binary was filtered out by .gitignore"
echo "$OUT" | grep -q "RUNUSER=pandion-run" && c_ok "ran as unprivileged pandion-run" || c_no "did not run as pandion-run"

# arch guard: the arm64 fixture (on an amd64 node) should trigger a loud warning
if [ -f "$DIST/app_arm64" ]; then
  if echo "$OUT" | grep -qi "arch mismatch" && echo "$OUT" | grep -q "app_arm64"; then
    c_ok "arch guard warned about the wrong-arch binary (app_arm64)"
  else
    c_no "arch guard did not warn about the arm64 binary on an amd64 node"
  fi
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "BINARY UPLOAD (sync mode binaries): verified" || c_no "see failures above"
