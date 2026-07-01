#!/usr/bin/env bash
# ============================================================================
# EnvCore M2.2a end-to-end test: provision -> hardened bootstrap -> WireGuard
# overlay -> operator-scoped SSH, then verify and ALWAYS tear down.
#
# Usage:
#   export HCLOUD_TOKEN=your-project-scoped-token
#   ./scripts/e2e_m22.sh
#
# Costs a few cents. The node is destroyed on exit (success, failure, or Ctrl+C).
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-m22"
BIN="./bin/envcore"
KEYDIR="$HOME/.envcore/keys/$ID"
WGCONF="$KEYDIR/wg-$ID.conf"
JOINED=0

: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN to your project-scoped Hetzner token}"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?
  echo
  c_in "cleaning up..."
  # bring the local overlay down if we brought it up
  if [ "$JOINED" = 1 ]; then sudo wg-quick down "$WGCONF" >/dev/null 2>&1 || true; fi
  # destroy the cloud node (idempotent; reconciles by tag)
  "$BIN" down --provider=hetzner --id "$ID" >/dev/null 2>&1 || true
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    if [ "${left:-0}" = 0 ]; then c_ok "teardown: no servers left"; else c_no "teardown: $left server(s) remain — check 'hcloud server list'"; fi
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

# ---- build ----
c_in "building envcore..."
export PATH="$HOME/.local/go/bin:$PATH"
go build -o "$BIN" ./cmd/envcore
c_ok "built $BIN"

# ---- up: provision + overlay, and verify wg0 on the node ----
c_in "provisioning (this waits for cloud-init + toolchain; ~2-4 min)..."
OUT=$("$BIN" up --provider=hetzner --id "$ID" -- \
  'echo "=== wg show ==="; wg show; echo "=== wg0 addr ==="; ip -brief addr show wg0; echo "=== firewall ==="; nft list chain inet envcore input 2>/dev/null | grep -E "dport (22|51820)" || true')
echo "----------------------------------------------------------------"
echo "$OUT"
echo "----------------------------------------------------------------"

# ---- assertions on the node side ----
PASS=1
echo "$OUT" | grep -q "interface: wg0"           && c_ok "node: wg0 interface is up"      || { c_no "node: wg0 not up"; PASS=0; }
echo "$OUT" | grep -q "peer:"                      && c_ok "node: wireguard peer configured" || { c_no "node: no wg peer"; PASS=0; }
echo "$OUT" | grep -Eq "wg0 .*10\.99\.0\.1"        && c_ok "node: overlay IP 10.99.0.1 assigned" || { c_no "node: overlay IP missing"; PASS=0; }
echo "$OUT" | grep -q "dport 51820"                && c_ok "firewall: WG port 51820 open"   || c_no "firewall: WG port rule not found (non-fatal)"

# ---- optional: actually join the overlay from THIS machine ----
if [ -f "$WGCONF" ] && command -v wg-quick >/dev/null 2>&1; then
  c_in "joining overlay locally (needs sudo)..."
  if sudo -n true 2>/dev/null || sudo true; then
    if sudo wg-quick up "$WGCONF" >/dev/null 2>&1; then
      JOINED=1
      if ping -c2 -W3 10.99.0.1 >/dev/null 2>&1; then
        c_ok "overlay data path: reached node at 10.99.0.1 over WireGuard"
      else
        c_no "overlay data path: ping over overlay failed (likely egress rule needed — a real finding)"
      fi
    else c_no "could not bring up local overlay (wg-quick up failed)"; fi
  else c_in "sudo unavailable — skipping local overlay join"; fi
else
  c_in "skipping local overlay join (need wireguard-tools: sudo apt install -y wireguard-tools)"
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "M2.2a NODE-SIDE OVERLAY: verified" || c_no "M2.2a: node-side checks failed (see above)"
# teardown runs automatically via trap
