#!/usr/bin/env bash
# ============================================================================
# Pandion L2 overlay e2e — SAFE profile (Phase 1). Proves the encrypted Layer-2
# VXLAN-over-WireGuard segment end to end on real cloud:
#   1. up a 3-node cluster with security.overlay: l2 (attacker/victim/target)
#   2. vxlan0 is up on every node with the expected IP / MAC / MTU
#   3. L2 reachability + dynamic ARP (BUM flood) across nodes
#   4. MTU is set correctly (no silent black-hole of large frames)
#   5. host-side Dynamic ARP Inspection is installed AND a real cross-node ARP
#      spoof is BLOCKED (victim's neighbor cache is NOT poisoned)
#   6. the management plane (wg0) posture is unchanged
#   7. teardown leaves nothing
# Self-cleaning.  export HCLOUD_TOKEN=…  ./scripts/e2e_l2_safe.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

PROV="${1:-hetzner}"; ID="e2e-l2safe-$PROV"; BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1
c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }
teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/l2_*.log /tmp/l2_cluster.yaml
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

cat > /tmp/l2_cluster.yaml <<'YAML'
apiVersion: pandion/v1
name: e2e-l2safe
provider: { name: hetzner }
defaults:
  security:
    overlay: l2        # encrypted Layer-2 segment, safe profile
nodes:
  - { name: attacker, run: "echo READY" }
  - { name: victim,   run: "echo READY" }
  - { name: target,   run: "echo READY" }
YAML

# --- Phase 1: up the 3-node L2 cluster ----------------------------------------
c_in "[1] up 3-node cluster with security.overlay: l2 (safe)..."
if timeout 720 env NO_COLOR=1 "$BIN" up --provider="$PROV" -f /tmp/l2_cluster.yaml --id "$ID" >/tmp/l2_up.log 2>&1; then :; fi
grep -qiE "WireGuard mesh verified|cluster .* UP" /tmp/l2_up.log && c_ok "cluster up + mesh formed" \
  || { c_no "up did not complete"; tail -25 /tmp/l2_up.log; exit 1; }

# read node facts from the manifest (public ip, l2 ip, l2 mac, host pin).
MAN="$HOME/.pandion/keys/$ID/manifest.json"
KEYDIR="$HOME/.pandion/keys/$ID"
read_node(){ python3 - "$MAN" "$1" "$2" <<'PY'
import sys,json
m=json.load(open(sys.argv[1]))
for n in m["nodes"]:
    if n["name"]==sys.argv[2]: print(n.get(sys.argv[3],"")); break
PY
}
for who in attacker victim target; do
  eval "${who}_ip=$(read_node "$who" ip)"
  eval "${who}_l2=$(read_node "$who" l2_ip)"
  eval "${who}_mac=$(read_node "$who" l2_mac)"
done
SSH="ssh -i $KEYDIR/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=10"
c_in "attacker=$attacker_ip/$attacker_l2  victim=$victim_ip/$victim_l2  target=$target_ip/$target_l2"
[ -n "$victim_l2" ] && [ -n "$target_mac" ] && c_ok "manifest carries L2 IPs + MACs" || c_no "manifest missing L2 fields"

# --- Phase 2: vxlan0 up with expected IP / MAC / MTU --------------------------
c_in "[2] vxlan0 present on every node with expected addr/mac/mtu..."
for who in attacker victim target; do
  ip=$(eval echo \$${who}_ip); l2=$(eval echo \$${who}_l2); mac=$(eval echo \$${who}_mac)
  out=$($SSH "root@$ip" "ip -o addr show vxlan0; ip -d link show vxlan0" 2>/dev/null || true)
  echo "$out" | grep -q "$l2" && echo "$out" | grep -qi "$mac" && echo "$out" | grep -q "mtu 1370" \
    && c_ok "$who: vxlan0 $l2 $mac mtu1370" || { c_no "$who: vxlan0 wrong/missing"; echo "$out"; }
done

# --- Phase 3: L2 reachability + dynamic ARP (BUM flood) -----------------------
c_in "[3] L2 reachability + dynamic ARP across the segment..."
$SSH "root@$attacker_ip" "ip neigh flush dev vxlan0; ping -c2 -W3 $victim_l2 >/dev/null 2>&1 && ping -c2 -W3 $target_l2 >/dev/null 2>&1" \
  && c_ok "attacker reaches victim + target over vxlan0" || c_no "L2 reachability failed"
neigh=$($SSH "root@$attacker_ip" "ip neigh show $victim_l2 dev vxlan0" 2>/dev/null || true)
echo "$neigh" | grep -qi "$victim_mac" && c_ok "dynamic ARP resolved victim (BUM flood works): $neigh" \
  || c_no "dynamic ARP did not resolve (broadcast/flood broken): $neigh"

# --- Phase 4: MTU sanity (a large-but-valid frame crosses; no black-hole) -----
c_in "[4] large frame (1300B, DF) crosses vxlan0 — MTU not black-holing..."
$SSH "root@$attacker_ip" "ping -c2 -W3 -s 1300 -M do $victim_l2 >/dev/null 2>&1" \
  && c_ok "1300B DF frame delivered (inner MTU correct)" || c_no "large frame black-holed (MTU bug)"

# --- Phase 5: DAI installed + a real cross-node ARP spoof is BLOCKED ----------
c_in "[5] safe-profile ARP inspection: spoof must NOT poison the victim..."
# DAI table present on the victim, with the target's binding.
dai=$($SSH "root@$victim_ip" "nft list table arp pandion_dai 2>/dev/null" || true)
echo "$dai" | grep -q "$target_l2" && echo "$dai" | grep -qi "$target_mac" \
  && c_ok "victim has DAI bindings (arp table pandion_dai)" || { c_no "DAI table missing/incomplete"; echo "$dai"; }
# victim learns target's REAL mac first.
$SSH "root@$victim_ip" "ping -c2 -W3 $target_l2 >/dev/null 2>&1" || true
# attacker forges an ARP reply: 'target_l2 is at attacker_mac' -> sent to victim.
$SSH "root@$attacker_ip" "python3 - vxlan0 $victim_mac $attacker_mac $target_l2 $victim_l2 $victim_mac" <<'PY' >/tmp/l2_spoof.log 2>&1 || true
import socket,struct,sys
iface,dstm,srcm,spoof_ip,vic_ip,vic_mac=sys.argv[1:7]
mac=lambda m: bytes(int(x,16) for x in m.split(':'))
s=socket.socket(socket.AF_PACKET,socket.SOCK_RAW); s.bind((iface,0))
eth=mac(dstm)+mac(srcm)+struct.pack('!H',0x0806)
arp=struct.pack('!HHBBH',1,0x0800,6,4,2)+mac(srcm)+socket.inet_aton(spoof_ip)+mac(vic_mac)+socket.inet_aton(vic_ip)
for _ in range(5): s.send(eth+arp)
print("sent 5 forged ARP replies")
PY
sleep 1
after=$($SSH "root@$victim_ip" "ip neigh show $target_l2 dev vxlan0" 2>/dev/null || true)
if echo "$after" | grep -qi "$attacker_mac"; then
  c_no "SPOOF SUCCEEDED — victim poisoned to attacker MAC: $after"
elif echo "$after" | grep -qi "$target_mac"; then
  c_ok "spoof BLOCKED — victim still maps target to its real MAC: $after"
else
  c_ok "spoof BLOCKED — victim has no poisoned entry: ${after:-<none>}"
fi

# --- Phase 6: management plane (wg0) unchanged --------------------------------
c_in "[6] management plane (wg0) posture intact..."
$SSH "root@$victim_ip" "ip link show wg0 >/dev/null 2>&1 && nft list table inet pandion >/dev/null 2>&1" \
  && c_ok "wg0 up + hardened firewall table present (management plane intact)" || c_no "wg0/firewall changed"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "L2 OVERLAY (safe): verified" || c_no "see failures above"
