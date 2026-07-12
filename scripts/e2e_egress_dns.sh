#!/usr/bin/env bash
# ============================================================================
# Pandion egress-allow-by-hostname e2e (P2.1): `--egress-allow` now accepts
# hostnames, resolved to IPv4 at provision time and fed into the nftables egress
# set. This proves it end-to-end: a node allowlisted for `one.one.one.one`
# (→ 1.1.1.1) can reach 1.1.1.1, but an un-allowlisted host (8.8.8.8) stays
# DENIED by default-deny egress. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token   # or put it in ./.env
#   ./scripts/e2e_egress_dns.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-egress-dns"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/edns_*.log "$HOME/.pandion/lock/$ID.json"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# Probe: 1.1.1.1 is the resolved allowlist target (must be reachable); 8.8.8.8 is
# NOT allowlisted (must be denied by default-deny egress). Raw IPs, no DNS needed.
PROBE='echo "ALLOWED=$(curl -sS --max-time 6 https://1.1.1.1/ >/dev/null 2>&1 && echo OK || echo BLOCKED)"
echo "DENIED=$(curl -sS --max-time 6 https://8.8.8.8/ >/dev/null 2>&1 && echo OPEN || echo BLOCKED)"'

c_in "provision with --egress-allow one.one.one.one (~3-5 min)..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node probe \
     --egress-allow one.one.one.one --ttl 30m -- "$PROBE" >/tmp/edns_up.log 2>&1; then :; fi
echo "---- probe output ----"; grep -E "ALLOWED=|DENIED=|node is live" /tmp/edns_up.log || tail -20 /tmp/edns_up.log
echo "----------------------"

grep -q "ALLOWED=OK" /tmp/edns_up.log \
  && c_ok "resolved hostname is reachable (egress to 1.1.1.1 allowed)" \
  || c_no "allowlisted host unreachable — hostname resolution/allow failed"
grep -q "DENIED=BLOCKED" /tmp/edns_up.log \
  && c_ok "un-allowlisted host denied (default-deny egress holds)" \
  || c_no "un-allowlisted host reachable — egress deny leaked"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "EGRESS-ALLOW BY HOSTNAME: verified" || c_no "see failures above"
