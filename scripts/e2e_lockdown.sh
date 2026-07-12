#!/usr/bin/env bash
# ============================================================================
# Pandion lockdown e2e (P1): proves the deny-all / overlay-only-SSH flip AND
# its lockout-safe design.
#
#   Phase A (safety gate)  With the overlay NOT joined, `lockdown` must REFUSE
#                          (exit 6) and change nothing — public SSH still works.
#   Phase B (real flip)    Join the overlay, `lockdown` succeeds, then:
#                            - public SSH is DEAD (scan sees only the WG port)
#                            - SSH still works over the overlay (10.99.0.x)
#
# Needs sudo (wg-quick) for Phase B. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_lockdown.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-lockdown"
BIN="./bin/pandion"
KD="$HOME/.pandion/keys/$ID"
WGCONF="$KD/wg-$ID.conf"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1
OVERLAY_UP=0

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  [ "$OVERLAY_UP" = 1 ] && sudo wg-quick down "$WGCONF" 2>/dev/null || true
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/ld_*.log "$HOME/.pandion/lock/$ID.json"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

# public-IP SSH probe (host-key not pinned; we only care whether the port answers)
SSH_PUB(){ ssh -i "$KD/login_ed25519" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15 "root@$1" "$2" 2>/dev/null; }

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "provision one node (~3-5 min)..."
timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node probe \
  --ttl 30m -- 'echo up' >/tmp/ld_up.log 2>&1 || { c_no "up failed"; tail -20 /tmp/ld_up.log; exit 1; }
IP=$(python3 -c "import json;print(json.load(open('$KD/manifest.json'))['nodes'][0]['ip'])")
c_ok "node live at $IP"
[ -f "$WGCONF" ] || { c_no "no operator overlay config at $WGCONF (was --no-overlay set?)"; exit 1; }

# baseline: public SSH works BEFORE lockdown
SSH_PUB "$IP" 'true' && c_ok "baseline: public SSH works before lockdown" || c_no "baseline public SSH failed"

# ---- Phase A: lockout-safe refusal (overlay NOT joined) --------------------
c_in "Phase A — lockdown with overlay DOWN must REFUSE..."
if NO_COLOR=1 "$BIN" lockdown --id "$ID" >/tmp/ld_a.log 2>&1; then
  c_no "lockdown SUCCEEDED without overlay — safety gate failed!"
else
  rc=$?
  [ "$rc" = 6 ] && c_ok "lockdown refused (exit 6) without overlay reachability" || c_no "refused but exit $rc (want 6)"
  grep -qi "REFUSING to lock down" /tmp/ld_a.log && c_ok "printed the lockout-safe refusal" || c_no "no refusal message"
fi
SSH_PUB "$IP" 'true' && c_ok "public SSH still works — refusal changed nothing" || c_no "public SSH broke after a REFUSED lockdown"

# ---- Phase B: real flip (overlay joined) ----------------------------------
c_in "Phase B — join overlay, then lock down for real..."
sudo wg-quick up "$WGCONF" && OVERLAY_UP=1 && c_ok "overlay joined" || { c_no "wg-quick up failed"; exit 1; }
if NO_COLOR=1 "$BIN" lockdown --id "$ID" >/tmp/ld_b.log 2>&1; then
  c_ok "lockdown applied over the overlay"
else
  c_no "lockdown failed with overlay up"; tail -10 /tmp/ld_b.log
fi

# public SSH must now be DEAD
if SSH_PUB "$IP" 'true'; then c_no "public SSH STILL WORKS after lockdown — not locked down!"
else c_ok "public SSH is dead after lockdown (deny-all public ingress)"; fi
# overlay SSH must still work
"$BIN" ssh --id "$ID" --overlay -- 'echo LOCKED_OK' 2>/dev/null | grep -q LOCKED_OK \
  && c_ok "SSH over the overlay still works (10.99.0.x)" \
  || c_no "lost SSH over the overlay after lockdown"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "LOCKDOWN: safety gate + overlay-only-SSH verified" || c_no "see failures above"
