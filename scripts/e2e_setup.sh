#!/usr/bin/env bash
# ============================================================================
# Pandion `setup:` e2e — non-apt software provisioning in the build window.
#   [1] up --setup runs a shell command on the node (as root), with egress open:
#       install a pip package + drop a marker file + fetch something over the net.
#   [2] the effects are present on the node (pip module importable, marker exists).
#   [3] a FAILING setup command aborts `up` (fail-fast; node left up for debugging).
# Uses --no-run (deploy only). Self-cleaning.
# Usage:  ./scripts/e2e_setup.sh [provider]   (default digitalocean)
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

PROV="${1:-digitalocean}"
ID="e2e-setup-$PROV"
ID2="e2e-setupfail-$PROV"
NODE="node-a"
BIN="./bin/pandion"
KD="$HOME/.pandion/keys/$ID"
MAN="$KD/manifest.json"
PASS=1
MARK="PANDION_SETUP_$$"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID"  --yes >/dev/null 2>&1 || true
  "$BIN" down --provider="$PROV" --id "$ID2" --yes >/dev/null 2>&1 || true
  rm -f /tmp/setup_up.log /tmp/setup_fail.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"

# ---------------------------------------------------------------------------
# [1] setup: install a pip package (non-apt), fetch over the network, drop a marker.
#     python3-pip comes via --packages; the pip install proves egress is open.
# ---------------------------------------------------------------------------
SETUP="pip3 install --break-system-packages --quiet requests && python3 -c 'import requests' && curl -fsSL https://example.com -o /tmp/net.html && echo $MARK > /root/setup.marker"
c_in "[1] up --packages python3-pip --setup '<pip install + net fetch + marker>' --no-run..."
if timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID" --node "$NODE" \
     --packages python3-pip --setup "$SETUP" --no-run --ttl 20m --max-cost 1.00 >/tmp/setup_up.log 2>&1; then :; fi
if grep -qiE "node deployed|nothing started" /tmp/setup_up.log; then
  c_ok "node deployed (setup ran without aborting)"
else
  c_no "up did not complete"; tail -25 /tmp/setup_up.log; exit 1
fi
grep -q "setup:" /tmp/setup_up.log && c_ok "setup step was reported at up time" || c_no "no setup step logged"
IP=$(python3 -c "import json;print(json.load(open('$MAN'))['nodes'][0]['ip'])")
c_in "node ip=$IP"

# ---------------------------------------------------------------------------
c_in "[2] setup effects present on the node..."
[ "$($SSH root@$IP "cat /root/setup.marker 2>/dev/null")" = "$MARK" ] && c_ok "marker file written by setup" || c_no "marker missing"
$SSH "root@$IP" "python3 -c 'import requests; print(requests.__version__)'" >/dev/null 2>&1 \
  && c_ok "pip-installed module importable (non-apt install worked)" || c_no "pip module not importable"
$SSH "root@$IP" "test -s /tmp/net.html" && c_ok "network fetch succeeded in the build window" || c_no "network fetch missing"

# ---------------------------------------------------------------------------
# [3] a failing setup command must ABORT up (fail-fast), leaving nothing "ready".
# ---------------------------------------------------------------------------
c_in "[3] a failing setup command aborts up..."
if timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID2" --node "$NODE" \
     --setup "false # this fails on purpose" --no-run --ttl 20m --max-cost 1.00 >/tmp/setup_fail.log 2>&1; then
  c_no "up should have failed on the bad setup command"
else
  grep -qi "setup command failed" /tmp/setup_fail.log && c_ok "up aborted with a clear setup error" \
    || { c_no "aborted but without a clear setup error"; tail -15 /tmp/setup_fail.log; }
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "SETUP HOOK: verified" || c_no "see failures above"
