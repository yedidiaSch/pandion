#!/usr/bin/env bash
# ============================================================================
# Pandion egress-allow re-resolution e2e (P2.1 follow-up): a hostname in
# --egress-allow is resolved ONCE at provision, but CDN IPs rotate — so Pandion
# installs an on-node systemd timer that periodically re-resolves the hostnames
# and re-adds their current IPv4s to the nftables egress set.
#
# This proves the mechanism deterministically (no waiting for a real rotation):
#   1. the timer + refresher + hosts file are installed,
#   2. FLUSH the egress set, run the refresher, and confirm it REPOPULATES the
#      set from DNS — i.e. re-resolution actually works.
# Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token   # or put it in ./.env
#   ./scripts/e2e_egress_refresh.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-egress-refresh"
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
  rm -f /tmp/erf_*.log "$HOME/.pandion/lock/$ID.json"
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

c_in "provision with --egress-allow one.one.one.one (~3-5 min)..."
timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node probe \
  --egress-allow one.one.one.one --ttl 30m -- 'echo ready' >/tmp/erf_up.log 2>&1 \
  || { c_no "up failed"; tail -20 /tmp/erf_up.log; exit 1; }
grep -q "on-node re-resolution installed" /tmp/erf_up.log \
  && c_ok "up reported the re-resolution timer was installed" \
  || c_no "up did not report installing the re-resolution timer"
IP=$(python3 -c "import json;print(json.load(open('$KD/manifest.json'))['nodes'][0]['ip'])")
c_ok "node live at $IP"

# On the node: verify install, then FLUSH + refresh + confirm the set repopulates.
PROBE='T=$(systemctl is-enabled pandion-egress-refresh.timer 2>/dev/null)
X=$(test -x /usr/local/sbin/pandion-egress-refresh && echo yes || echo no)
H=$(grep -c one.one.one.one /etc/pandion/egress-hosts 2>/dev/null || echo 0)
nft flush set inet pandion egress_ok 2>/dev/null || true
/usr/local/sbin/pandion-egress-refresh 2>/dev/null || true
R=$(nft list set inet pandion egress_ok 2>/dev/null | grep -cE "1\.1\.1\.1|1\.0\.0\.1")
echo "TIMER=$T SCRIPT=$X HOSTS=$H REPOP=$R"'
OUT=$(SSH "$IP" "$PROBE")
echo "---- node check ----"; echo "$OUT"; echo "--------------------"

echo "$OUT" | grep -q "TIMER=enabled" && c_ok "systemd timer is enabled" || c_no "timer not enabled"
echo "$OUT" | grep -q "SCRIPT=yes"    && c_ok "refresher script installed"  || c_no "refresher script missing"
echo "$OUT" | grep -qE "HOSTS=[1-9]"  && c_ok "hostname recorded in /etc/pandion/egress-hosts" || c_no "hostname not recorded"
echo "$OUT" | grep -qE "REPOP=[1-9]"  && c_ok "re-resolution REPOPULATED the flushed egress set from DNS" || c_no "set was not repopulated — re-resolution broken"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "EGRESS-ALLOW RE-RESOLUTION: verified" || c_no "see failures above"
