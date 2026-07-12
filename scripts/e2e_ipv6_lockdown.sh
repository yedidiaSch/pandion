#!/usr/bin/env bash
# ============================================================================
# Pandion IPv6-lockdown e2e (P0.1): the nftables lockdown filters IPv4 only, so
# a dual-stack node would let a workload egress over IPv6 past the allowlist +
# metadata block. Pandion closes that by DISABLING IPv6 on the node. This proves
# it on a real node: IPv6 is off, there is no global IPv6 address, and an IPv6
# egress attempt fails. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_ipv6_lockdown.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-ipv6"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/v6_*.log "$HOME/.pandion/lock/$ID.json"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# Probe: report the IPv6 sysctl, any global IPv6 address, and whether an IPv6
# egress actually leaves (public DNS over IPv6). All three must show IPv6 is off.
PROBE='echo "V6DIS=$(cat /proc/sys/net/ipv6/conf/all/disable_ipv6 2>/dev/null)"
echo "V6ADDR=$(ip -6 addr show scope global 2>/dev/null | grep -c inet6)"
if curl -6 -sS --max-time 5 "https://[2606:4700:4700::1111]/" >/dev/null 2>&1; then echo "V6EGRESS=OPEN"; else echo "V6EGRESS=BLOCKED"; fi'

c_in "provision one node, then probe IPv6 state (~3-5 min)..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node probe \
     --ttl 30m -- "$PROBE" >/tmp/v6_up.log 2>&1; then :; fi
echo "---- probe output ----"; grep -E "V6DIS=|V6ADDR=|V6EGRESS=|node is live" /tmp/v6_up.log || tail -20 /tmp/v6_up.log
echo "----------------------"

grep -q "V6DIS=1" /tmp/v6_up.log \
  && c_ok "IPv6 is DISABLED at the kernel (disable_ipv6=1)" \
  || c_no "IPv6 not disabled (disable_ipv6 != 1)"
grep -q "V6ADDR=0" /tmp/v6_up.log \
  && c_ok "node has NO global IPv6 address" \
  || c_no "node still has a global IPv6 address"
grep -q "V6EGRESS=BLOCKED" /tmp/v6_up.log \
  && c_ok "IPv6 egress fails — no path around the IPv4 allowlist/metadata block" \
  || c_no "IPv6 egress SUCCEEDED — lockdown bypass over IPv6!"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "IPv6 LOCKDOWN: verified (no dual-stack egress bypass)" || c_no "see failures above"
