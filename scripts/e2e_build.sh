#!/usr/bin/env bash
# ============================================================================
# Pandion `pandion build` e2e — auto-detect + build a project in the cloud.
#
# Proves the one-liner on real cloud:
#   [1] a tiny CMake C++ project is scaffolded locally.
#   [2] `pandion build <dir> --id ID -- ./build/app` auto-detects CMake, uploads
#       the dir, builds it on the node, and runs the resulting binary — its
#       marker shows up in the run log.
#   [3] a Go project is detected as Go (build banner) via --dry-run (no spend).
#   [4] `pandion down --id ID` tears the cluster down (provider from manifest).
# Self-cleaning.  Usage:  ./scripts/e2e_build.sh [provider]   (default hetzner)
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

PROV="${1:-hetzner}"
ID="e2e-build-$PROV"
BIN="$PWD/bin/pandion"
KD="$HOME/.pandion/keys/$ID"
MAN="$KD/manifest.json"
PROJ="$(mktemp -d)"
GOPROJ="$(mktemp -d)"
MARK="PANDION_BUILT_$$"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -rf "$PROJ" "$GOPROJ" /tmp/build_up.log /tmp/build_dry.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:/usr/local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"

# ---------------------------------------------------------------------------
c_in "[1] scaffold a tiny CMake C++ project..."
cat > "$PROJ/CMakeLists.txt" <<EOF
cmake_minimum_required(VERSION 3.10)
project(hello CXX)
add_executable(app main.cpp)
EOF
cat > "$PROJ/main.cpp" <<EOF
#include <cstdio>
int main(){ printf("$MARK\\n"); return 0; }
EOF
c_ok "scaffolded"

# ---------------------------------------------------------------------------
c_in "[2] pandion build <dir> --id $ID -- ./build/app  (detect CMake, upload, build, run)..."
if timeout 1200 env NO_COLOR=1 "$BIN" build "$PROJ" --provider="$PROV" --id "$ID" \
     --ttl 30m --max-cost 2.00 -- './build/app' >/tmp/build_up.log 2>&1; then :; fi
grep -qi "detected CMake" /tmp/build_up.log && c_ok "auto-detected CMake" || { c_no "did not detect CMake"; tail -20 /tmp/build_up.log; }
[ -f "$MAN" ] || { c_no "no manifest — provisioning failed"; tail -25 /tmp/build_up.log; exit 1; }
IP=$(python3 -c "import json;print(json.load(open('$MAN'))['nodes'][0]['ip'])")
c_in "node=$IP; waiting for the build+run to produce the marker..."
ok=0
for _ in $(seq 1 20); do
  if $SSH "root@$IP" "cat /var/log/pandion/run.log 2>/dev/null" | grep -q "$MARK"; then ok=1; break; fi
  sleep 6
done
[ "$ok" = 1 ] && c_ok "built on the node and ran (marker in run log)" || { c_no "marker never appeared"; $SSH "root@$IP" 'tail -30 /var/log/pandion/run.log' 2>&1 | head; }

# ---------------------------------------------------------------------------
c_in "[3] Go project is detected as Go (offline --dry-run)..."
echo 'module hello' > "$GOPROJ/go.mod"
printf 'package main\nfunc main(){}\n' > "$GOPROJ/main.go"
NO_COLOR=1 "$BIN" build "$GOPROJ" --provider="$PROV" --dry-run --id "${ID}-go" >/tmp/build_dry.log 2>&1 || true
grep -qi "detected Go" /tmp/build_dry.log && c_ok "auto-detected Go" || { c_no "did not detect Go"; cat /tmp/build_dry.log; }

# ---------------------------------------------------------------------------
c_in "[4] down --id $ID (provider from manifest)..."
NO_COLOR=1 "$BIN" down --id "$ID" --yes 2>&1 | tail -2

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "BUILD: verified" || c_no "see failures above"
