#!/usr/bin/env bash
# ============================================================================
# Pandion cp e2e: `pandion cp` copies files to/from a node (host-key pinned),
# round-trip verified. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_cp.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-cp"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/cp_*.txt /tmp/cp_*.log "$HOME/.pandion/lock/$ID.json"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "provision one node (~3-5 min)..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node box \
     --ttl 30m -- 'echo PANDION_READY' >/tmp/cp_up.log 2>&1; then :; fi
grep -q "node is live" /tmp/cp_up.log && c_ok "provisioned" || { c_no "up did not complete"; tail -15 /tmp/cp_up.log; }

TOKEN="pandion-cp-$$-$RANDOM"
echo "$TOKEN" > /tmp/cp_local.txt

# upload local -> node
c_in "upload: pandion cp ./file :/tmp/uploaded.txt ..."
NO_COLOR=1 "$BIN" cp --id "$ID" --node box /tmp/cp_local.txt :/tmp/uploaded.txt >/tmp/cp_up2.log 2>&1 \
  && c_ok "upload succeeded" || { c_no "upload failed"; cat /tmp/cp_up2.log; }

# confirm on the node via pandion ssh
NO_COLOR=1 "$BIN" ssh --id "$ID" --node box -- 'cat /tmp/uploaded.txt' >/tmp/cp_cat.log 2>&1 || true
grep -q "$TOKEN" /tmp/cp_cat.log && c_ok "uploaded file present + correct on the node" || c_no "uploaded file wrong/missing on node"

# download node -> local (round-trip)
c_in "download: pandion cp :/tmp/uploaded.txt ./back.txt ..."
NO_COLOR=1 "$BIN" cp --id "$ID" --node box :/tmp/uploaded.txt /tmp/cp_back.txt >/tmp/cp_dn.log 2>&1 \
  && c_ok "download succeeded" || { c_no "download failed"; cat /tmp/cp_dn.log; }
[ -f /tmp/cp_back.txt ] && grep -q "$TOKEN" /tmp/cp_back.txt \
  && c_ok "round-trip content matches" || c_no "round-trip content mismatch"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "PANDION CP: verified" || c_no "see failures above"
