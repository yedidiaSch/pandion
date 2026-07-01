#!/usr/bin/env bash
# ============================================================================
# EnvCore M3.3 e2e: cluster + mesh + SERVICE DISCOVERY. Each node reaches its
# sibling via the injected $ENVCORE_<NODE>_IP over the overlay. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_m33.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-m33"
BIN="./bin/envcore"
CLYAML="$(mktemp --suffix=.yaml)"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" >/dev/null 2>&1 || true
  rm -f "$CLYAML"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

# broker prints the worker's discovered IP; worker pings the broker over the
# overlay using its discovered IP — proving discovery + IPC-over-overlay.
cat > "$CLYAML" <<EOF
apiVersion: envcore/v1
name: $ID
nodes:
  - name: broker
    run: 'echo "broker sees worker at \$ENVCORE_WORKER_IP"'
  - name: worker
    run: 'ping -c2 -W3 \$ENVCORE_BROKER_IP >/dev/null 2>&1 && echo "worker reached broker at \$ENVCORE_BROKER_IP via overlay" || echo FAIL'
EOF

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/envcore; c_ok "built"

c_in "provisioning cluster + mesh + discovery (~3-5 min)..."
OUT=$("$BIN" up --provider=hetzner --id "$ID" -f "$CLYAML")
echo "----------------------------------------------------------------"
echo "$OUT"
echo "----------------------------------------------------------------"

PASS=1
echo "$OUT" | grep -q "mesh verified" && c_ok "mesh verified" || { c_no "mesh"; PASS=0; }
echo "$OUT" | grep -q "broker sees worker at 10.99.0.2" \
  && c_ok "discovery: broker resolved \$ENVCORE_WORKER_IP to overlay IP" || { c_no "discovery var not resolved"; PASS=0; }
echo "$OUT" | grep -q "worker reached broker at 10.99.0.1 via overlay" \
  && c_ok "IPC-over-overlay: worker reached broker via discovered IP" || { c_no "worker could not reach broker via discovery"; PASS=0; }

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "M3.3 DISCOVERY + IPC: verified" || c_no "M3.3: see failures above"
