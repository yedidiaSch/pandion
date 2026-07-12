#!/usr/bin/env bash
# ============================================================================
# Pandion workspace-sync e2e: sync a local C++ project to a node, build it
# remotely, and run it — proving Pandion can run YOUR code. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_sync.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-sync"
BIN="./bin/pandion"
SRC="$(mktemp -d)"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -rf "$SRC"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

# a tiny local C++ "project" + a build dir that MUST be excluded from the sync
mkdir -p "$SRC/build"
cat > "$SRC/hello.cpp" <<'CPP'
#include <cstdio>
int main(){ std::puts("HELLO_FROM_SYNCED_SOURCE"); return 0; }
CPP
echo "this must NOT be uploaded" > "$SRC/build/stale.o"
printf 'build/\n' > "$SRC/.pandionignore"

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "provision + sync $SRC + remote build + run as pandion-run (~2-4 min)..."
# workspace becomes the cwd; the command runs as the unprivileged pandion-run user.
OUT=$("$BIN" up --provider=hetzner --id "$ID" \
  --workspace "$SRC" \
  --build 'g++ -O2 hello.cpp -o hello' \
  -- 'echo "RUNUSER=$(whoami)"; ls; ./hello')
echo "----------------------------------------------------------------"
echo "$OUT"
echo "----------------------------------------------------------------"

PASS=1
echo "$OUT" | grep -q "syncing .* files ->" && c_ok "workspace archived + streamed to node" || { c_no "sync step"; PASS=0; }
echo "$OUT" | grep -q "HELLO_FROM_SYNCED_SOURCE" && c_ok "remote build + run of MY source succeeded" || { c_no "build/run"; PASS=0; }
echo "$OUT" | grep -q "hello.cpp" && c_ok "source file present on node" || { c_no "source missing"; PASS=0; }
echo "$OUT" | grep -q "stale.o" && { c_no ".pandionignore not honored (build/ leaked)"; PASS=0; } || c_ok ".pandionignore honored (build/ excluded)"
echo "$OUT" | grep -q "RUNUSER=pandion-run" && c_ok "least privilege: workload ran as pandion-run (NOT root)" || { c_no "workload did not run as pandion-run"; PASS=0; }

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "WORKSPACE SYNC + LEAST-PRIV: verified" || c_no "see failures above"
