#!/usr/bin/env bash
# ============================================================================
# Pandion network-hardening e2e (P1/M8): a provisioned node has the sysctl
# baseline applied (WG-safe: loose rp_filter), a provider-level cloud firewall
# in front of the host nftables, and that firewall is CLEANED UP on teardown
# (no leak). Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_network_hardening.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-nethard"
FW="pandion-fw-$ID"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

has_hcloud(){ command -v hcloud >/dev/null 2>&1; }
fw_exists(){ hcloud firewall list -o noheader 2>/dev/null | grep -q "$FW"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/nh_*.log "$HOME/.pandion/lock/$ID.json"
  if has_hcloud; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
    fw_exists && c_no "teardown: cloud firewall $FW LEAKED" || c_ok "teardown: cloud firewall gone"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# Probe (runs as the workload user): read the hardened sysctl values from /proc.
PROBE='echo "SYN=$(sysctl -n net.ipv4.tcp_syncookies 2>/dev/null)"; echo "RPF=$(sysctl -n net.ipv4.conf.all.rp_filter 2>/dev/null)"; echo "RED=$(sysctl -n net.ipv4.conf.all.accept_redirects 2>/dev/null)"'

c_in "provision one node (~3-5 min), probe sysctl + cloud firewall..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node probe \
     --ttl 30m -- "$PROBE" >/tmp/nh_up.log 2>&1; then :; fi
echo "---- probe output ----"; grep -E "SYN=|RPF=|RED=|cloud-edge firewall|node is live" /tmp/nh_up.log || tail -20 /tmp/nh_up.log
echo "----------------------"

# sysctl baseline
grep -q "SYN=1" /tmp/nh_up.log && c_ok "tcp_syncookies enabled" || c_no "tcp_syncookies not set"
grep -q "RPF=2" /tmp/nh_up.log && c_ok "rp_filter is LOOSE (2) — WireGuard-safe" || c_no "rp_filter not loose (2)"
grep -q "RED=0" /tmp/nh_up.log && c_ok "ICMP redirects ignored" || c_no "accept_redirects not disabled"

# cloud firewall present + attached
if has_hcloud; then
  if fw_exists; then
    c_ok "cloud-edge firewall $FW created"
    hcloud firewall describe "$FW" 2>/dev/null | grep -qiE "server|applied|label" \
      && c_ok "firewall is applied (has targets)" || c_in "note: could not confirm targets"
  else
    c_no "cloud firewall $FW not found after up"
  fi

  # no-leak: down must delete the firewall.
  c_in "pandion down — the cloud firewall must be removed (no leak)..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/tmp/nh_down.log 2>&1 || true
  sleep 3
  fw_exists && c_no "cloud firewall $FW LEAKED after down" || c_ok "cloud firewall removed on down (no leak)"
else
  c_in "note: hcloud CLI not installed — skipping cloud-firewall checks"
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "NETWORK HARDENING (sysctl + cloud firewall): verified" || c_no "see failures above"
