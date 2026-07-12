#!/usr/bin/env bash
# ============================================================================
# Pandion relay e2e (Phase 1: `relay up`) — deploy the browser-SSH relay on a
# node and prove it's serving over TLS through the one opened firewall port:
#   [1] up a node; [2] `pandion relay up`; [3] the relay serves xterm.js over
#   HTTPS and 404s unknown tokens (so the systemd service, TLS, and firewall port
#   are all working). The full browser-terminal flow (`relay share`) is verified
#   in the next slice. Self-cleaning.
# Usage:  ./scripts/e2e_relay.sh [provider]   (default digitalocean — no cloud firewall)
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

PROV="${1:-digitalocean}"
ID="e2e-relay-$PROV"
PORT=8443
BIN="./bin/pandion"
MAN="$HOME/.pandion/keys/$ID/manifest.json"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/relay_up.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "[1] up a node to host the relay..."
if timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID" --node relay \
     --ttl 20m --max-cost 1.00 -- 'echo READY' >/tmp/relay_up.log 2>&1; then :; fi
grep -q "node is live" /tmp/relay_up.log && c_ok "node up" || { c_no "node up failed"; tail -20 /tmp/relay_up.log; exit 1; }
IP=$(python3 -c "import json;print(json.load(open('$MAN'))['nodes'][0]['ip'])")
c_in "relay node ip=$IP"

c_in "[2] pandion relay up (build + upload + systemd + firewall port + start)..."
if NO_COLOR=1 "$BIN" relay up --id "$ID" --port "$PORT" 2>&1 | tee /tmp/relay_deploy.log; then :; fi
grep -q "relay up on" /tmp/relay_deploy.log && c_ok "relay deployed" || c_no "relay up did not report success"
grep -qE "TLS SHA-256: [0-9A-F:]{20,}" /tmp/relay_deploy.log && c_ok "relay reported a TLS fingerprint" || c_no "no TLS fingerprint"

c_in "[3] relay is serving over HTTPS through the opened firewall port..."
sleep 3
code=$(curl -sk --max-time 15 "https://$IP:$PORT/assets/xterm.js" -o /dev/null -w '%{http_code}' 2>/dev/null || echo 000)
[ "$code" = 200 ] && c_ok "xterm.js served over TLS (HTTP $code) — service + firewall + TLS all up" || c_no "xterm.js not served (HTTP $code)"
code=$(curl -sk --max-time 15 "https://$IP:$PORT/s/PRLY1-nope" -o /dev/null -w '%{http_code}' 2>/dev/null || echo 000)
[ "$code" = 404 ] && c_ok "unknown token 404s (no oracle)" || c_no "unknown token returned HTTP $code (want 404)"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "RELAY UP: verified" || c_no "see failures above"
