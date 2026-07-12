#!/usr/bin/env bash
# ============================================================================
# Pandion firewall audit/dry-run e2e (P2.2): `up --firewall-audit` applies the
# firewall in AUDIT mode — nothing is enforced, but every packet a real lockdown
# would drop is logged (prefix "pandion-audit-out"). This proves both halves on a
# real node: an un-allowlisted egress SUCCEEDS (not enforced), and that same
# traffic shows up in the kernel audit log. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token   # or put it in ./.env
#   ./scripts/e2e_firewall_audit.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-fw-audit"
BIN="./bin/pandion"
KD="$HOME/.pandion/keys/$ID"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/fwa_*.log "$HOME/.pandion/lock/$ID.json"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

SSH(){ ssh -i "$KD/login_ed25519" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15 "root@$1" "$2" 2>/dev/null; }

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# Probe: hit an un-allowlisted host. In AUDIT mode this MUST succeed (nothing
# enforced) — under a real lockdown it would be denied.
PROBE='echo "EGRESS=$(curl -sS --max-time 6 https://8.8.8.8/ >/dev/null 2>&1 && echo OPEN || echo BLOCKED)"'

c_in "provision with --firewall-audit (~3-5 min)..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node probe \
     --firewall-audit --ttl 30m -- "$PROBE" >/tmp/fwa_up.log 2>&1; then :; fi
echo "---- probe output ----"; grep -E "EGRESS=|AUDIT mode|node is live" /tmp/fwa_up.log || tail -20 /tmp/fwa_up.log
echo "----------------------"
IP=$(python3 -c "import json;print(json.load(open('$KD/manifest.json'))['nodes'][0]['ip'])" 2>/dev/null || true)

grep -q "AUDIT mode" /tmp/fwa_up.log \
  && c_ok "up reported AUDIT mode (nothing enforced)" \
  || c_no "up did not report audit mode"
grep -q "EGRESS=OPEN" /tmp/fwa_up.log \
  && c_ok "un-allowlisted egress SUCCEEDED — audit mode enforces nothing" \
  || c_no "egress was BLOCKED — audit mode wrongly enforced"

# the same would-be-denied traffic must be LOGGED in the kernel audit log
if [ -n "${IP:-}" ]; then
  n=$(SSH "$IP" 'dmesg 2>/dev/null | grep -c pandion-audit-out || journalctl -k --no-pager 2>/dev/null | grep -c pandion-audit-out || echo 0')
  [ "${n:-0}" -gt 0 ] \
    && c_ok "kernel audit log recorded would-be-drops (pandion-audit-out ×$n)" \
    || c_no "no pandion-audit-out entries in the kernel log"
else
  c_no "could not read node IP to check the audit log"
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "FIREWALL AUDIT MODE: non-enforcing + logging verified" || c_no "see failures above"
