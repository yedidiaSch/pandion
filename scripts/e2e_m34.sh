#!/usr/bin/env bash
# ============================================================================
# Pandion M3.4 e2e: MULTIPLEXED per-node output + per-node log files.
# Two nodes each emit several timestamped lines; output should interleave with
# [node] prefixes, and per-node logs should be written locally. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_m34.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-m34"
BIN="./bin/pandion"
CLYAML="$(mktemp --suffix=.yaml)"
LOGDIR="$HOME/.pandion/logs/$ID"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f "$CLYAML"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

# each node prints 3 lines (finite, so the stream ends on its own) — broker to
# stdout, worker mixes a stderr line to exercise the stderr marker.
cat > "$CLYAML" <<EOF
apiVersion: pandion/v1
name: $ID
nodes:
  - name: broker
    run: 'for i in 1 2 3; do echo "broker line \$i"; sleep 1; done'
  - name: worker
    run: 'for i in 1 2 3; do echo "worker line \$i"; sleep 1; done; echo "worker warning" 1>&2'
EOF

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "provisioning + streaming multiplexed output (~3-5 min)..."
# NO_COLOR so assertions match plain prefixes
OUT=$(NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" -f "$CLYAML")
echo "----------------------------------------------------------------"
echo "$OUT"
echo "----------------------------------------------------------------"

PASS=1
echo "$OUT" | grep -q "mesh verified" && c_ok "mesh verified" || { c_no "mesh"; PASS=0; }
echo "$OUT" | grep -q "\[broker\] broker line 3" && c_ok "broker output multiplexed with prefix" || { c_no "broker stream"; PASS=0; }
echo "$OUT" | grep -q "\[worker\] worker line 3" && c_ok "worker output multiplexed with prefix" || { c_no "worker stream"; PASS=0; }
echo "$OUT" | grep -q "\[worker !\] worker warning" && c_ok "stderr marked with [node !]" || c_no "stderr marker (non-fatal)"
# both nodes' lines present => interleaved multiplexing
echo "$OUT" | grep -q "\[broker\] broker line 1" && echo "$OUT" | grep -q "\[worker\] worker line 1" \
  && c_ok "both node streams interleaved into one view" || { c_no "interleave"; PASS=0; }
# per-node log files written
[ -s "$LOGDIR/broker.log" ] && grep -q "broker line 2" "$LOGDIR/broker.log" && c_ok "per-node log tee'd: $LOGDIR/broker.log" || { c_no "broker log"; PASS=0; }
[ -s "$LOGDIR/worker.log" ] && c_ok "per-node log tee'd: worker.log" || { c_no "worker log"; PASS=0; }

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "M3.4 MULTIPLEXED OUTPUT: verified" || c_no "M3.4: see failures above"
