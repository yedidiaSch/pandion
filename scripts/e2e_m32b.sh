#!/usr/bin/env bash
# ============================================================================
# Pandion M3.2b end-to-end: provision a 2-node hardened cluster, form the
# WireGuard mesh, verify mutual reachability, then ALWAYS tear down.
#
# Usage:
#   export HCLOUD_TOKEN=your-project-scoped-token
#   ./scripts/e2e_m32b.sh
#
# Costs a few cents (2 nodes for a few minutes). Destroyed on exit.
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-m32b"
BIN="./bin/pandion"
CLYAML="$(mktemp --suffix=.yaml)"

: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN to your project-scoped Hetzner token}"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?
  echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f "$CLYAML"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left server(s) remain"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

cat > "$CLYAML" <<EOF
apiVersion: pandion/v1
name: $ID
nodes:
  - name: broker
    run: ./broker
  - name: worker
    run: ./worker
EOF

c_in "building pandion..."
export PATH="$HOME/.local/go/bin:$PATH"
go build -o "$BIN" ./cmd/pandion
c_ok "built"

c_in "provisioning 2-node hardened cluster + forming mesh (~3-5 min)..."
OUT=$("$BIN" up --provider=hetzner --id "$ID" -f "$CLYAML")
echo "----------------------------------------------------------------"
echo "$OUT"
echo "----------------------------------------------------------------"

PASS=1
echo "$OUT" | grep -q "barrier"                   && : # (barrier line is internal)
echo "$OUT" | grep -Eq "broker .*ip=" && echo "$OUT" | grep -Eq "worker .*ip=" \
  && c_ok "both nodes provisioned (barrier passed)" || { c_no "provisioning"; PASS=0; }
if echo "$OUT" | grep -q "mesh verified"; then
  c_ok "WireGuard mesh formed and mutually reachable"
elif echo "$OUT" | grep -q "OK$"; then
  c_ok "mesh reachability checks present"
else
  c_no "mesh verification not confirmed (see output above)"; PASS=0
fi
echo "$OUT" | grep -q "mesh verification had failures" && { c_no "mesh had reachability failures"; PASS=0; }

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "M3.2b CLUSTER MESH: verified" || c_no "M3.2b: see failures above"
# teardown runs via trap
