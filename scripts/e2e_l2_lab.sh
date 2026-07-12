#!/usr/bin/env bash
# ============================================================================
# Pandion L2 overlay e2e — LAB profile (Phase 2, the attackable cyber-range).
# The mirror image of e2e_l2_safe.sh: here the ARP spoof must SUCCEED (and be
# contained). Proves on real cloud:
#   1. up a 3-node lab cluster (attacker/victim/target) — LOUD warning + audit
#   2. lab relaxations present: rp_filter=0 + promisc on vxlan0, and NO DAI table
#   3. baseline L2 reachability works
#   4. MITM WORKS — attacker poisons the victim's ARP for the target (victim's
#      neighbor cache flips to the attacker's MAC) AND the attacker intercepts the
#      victim's target-bound traffic (tcpdump proof)
#   5. CONTAINMENT — the attack never leaks to the public NIC; wg0 stays hardened
# Self-cleaning.  export HCLOUD_TOKEN or PANDION_PROVIDER + token.
#   ./scripts/e2e_l2_lab.sh [provider]
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

PROV="${1:-hetzner}"
ID="e2e-l2lab-$PROV"
BIN="./bin/pandion"
PASS=1
c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ %-9s ]\033[0m %s\n' "$PROV" "$*"; }
WORK="/tmp/l2lab-$PROV"; rm -rf "$WORK"; mkdir -p "$WORK"
teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -rf "$WORK" "/tmp/l2lab_$PROV.yaml"
  c_in "done (exit $code)"
}
trap teardown EXIT

export PATH="$HOME/.local/go/bin:$PATH"
c_in "building..."; go build -o "$BIN" ./cmd/pandion >/dev/null 2>&1 && c_ok "built" || { c_no build; exit 1; }

cat > "/tmp/l2lab_$PROV.yaml" <<YAML
apiVersion: pandion/v1
name: $ID
provider: { name: $PROV }
defaults:
  security:
    overlay:
      l2:
        profile: lab
nodes:
  - { name: attacker, run: "echo READY" }
  - { name: victim,   run: "echo READY" }
  - { name: target,   run: "echo READY" }
YAML

# --- Phase 1: up + LOUD warning ----------------------------------------------
c_in "[1] up 3-node LAB cluster (security.overlay.l2.profile: lab)..."
if timeout 1200 env NO_COLOR=1 "$BIN" up --provider="$PROV" -f "/tmp/l2lab_$PROV.yaml" --id "$ID" >"$WORK/up.log" 2>&1; then :; fi
grep -qiE "mesh verif" "$WORK/up.log" && c_ok "cluster up + mesh formed" || { c_no "up did not complete"; tail -25 "$WORK/up.log"; exit 1; }
grep -q "L2 LAB PROFILE" "$WORK/up.log" && c_ok "loud lab warning printed on up" || c_no "lab warning missing (UX)"

MAN="$HOME/.pandion/keys/$ID/manifest.json"; KD="$HOME/.pandion/keys/$ID"
[ -f "$MAN" ] || { c_no "no manifest (up errored)"; tail -25 "$WORK/up.log"; exit 1; }
j(){ python3 -c "import json;print([n.get('$2','') for n in json.load(open('$MAN'))['nodes'] if n['name']=='$1'][0])"; }
for who in attacker victim target; do eval "${who}_ip=$(j $who ip)"; eval "${who}_l2=$(j $who l2_ip)"; eval "${who}_mac=$(j $who l2_mac)"; done
SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"
c_in "attacker=$attacker_l2/$attacker_mac victim=$victim_l2 target=$target_l2/$target_mac"

# `ls` should tag it as a lab (retry: the provider's server list is eventually
# consistent right after provisioning).
tag_ok=no
for _ in 1 2 3 4 5; do
  NO_COLOR=1 "$BIN" ls --provider="$PROV" 2>/dev/null | grep -q "L2-LAB" && { tag_ok=yes; break; }
  sleep 3
done
[ "$tag_ok" = yes ] && c_ok "ls tags the cluster L2-LAB" || c_no "ls missing L2-LAB tag"

# --- Phase 2: lab relaxations present; DAI ABSENT ----------------------------
c_in "[2] lab relaxations on vxlan0; DAI must be ABSENT..."
rp=$($SSH "root@$attacker_ip" "cat /proc/sys/net/ipv4/conf/vxlan0/rp_filter 2>/dev/null" || echo x)
pr=$($SSH "root@$attacker_ip" "ip -d link show vxlan0 2>/dev/null | grep -c -i promisc" || echo 0)
[ "$rp" = 0 ] && c_ok "rp_filter=0 on vxlan0 (MITM can forward)" || c_no "rp_filter not relaxed (got '$rp')"
$SSH "root@$attacker_ip" "ip link show vxlan0 | grep -qi PROMISC" && c_ok "promiscuous mode on vxlan0" || c_no "promisc not set"
$SSH "root@$victim_ip" "nft list table arp pandion_dai >/dev/null 2>&1" && c_no "DAI table present (lab must NOT have it)" || c_ok "no DAI table (segment is attackable, as intended)"

# --- Phase 3: baseline L2 reachability ---------------------------------------
c_in "[3] baseline L2 reachability..."
$SSH "root@$victim_ip" "ping -c2 -W3 $target_l2 >/dev/null 2>&1" && c_ok "victim reaches target over vxlan0" || c_no "L2 reachability failed"

# --- Phase 4: MITM WORKS (spoof succeeds + interception) ----------------------
c_in "[4] ARP-spoof MITM: victim must be poisoned + attacker intercepts..."
# deploy the agent for the raw ARP-spoof
AGENT_B64=$(base64 -w0 scripts/l2agent.py)
$SSH "root@$attacker_ip" "echo $AGENT_B64 | base64 -d > /usr/local/bin/l2agent.py && chmod 0755 /usr/local/bin/l2agent.py" >/dev/null 2>&1
spoof(){ $SSH "root@$attacker_ip" "python3 /usr/local/bin/l2agent.py spoof vxlan0 $victim_mac $attacker_mac $target_l2 $victim_l2 $victim_mac" >/dev/null 2>&1 || true; }
# victim learns target's real MAC, then the attacker starts capturing traffic it
# should only ever see if it has successfully intercepted (dst target, src victim).
$SSH "root@$victim_ip" "ping -c1 -W2 $target_l2 >/dev/null 2>&1" || true
$SSH "root@$attacker_ip" "tmux kill-session -t cap 2>/dev/null; tmux new-session -d -s cap 'timeout 14 tcpdump -ni vxlan0 icmp and dst host $target_l2 and src host $victim_l2 -c 3 > /tmp/mitm.txt 2>&1'" >/dev/null 2>&1
sleep 2
# poison, then drive victim traffic -> it flows to the attacker (intercepted).
spoof
$SSH "root@$victim_ip" "ping -c3 -W2 $target_l2 >/dev/null 2>&1" || true
sleep 3
cap=$($SSH "root@$attacker_ip" "wc -l < /tmp/mitm.txt 2>/dev/null" || echo 0)
[ "${cap:-0}" -ge 1 ] && c_ok "attacker INTERCEPTED victim->target traffic ($cap frames) — MITM confirmed" \
  || { c_no "attacker did not intercept victim->target traffic"; $SSH "root@$attacker_ip" "cat /tmp/mitm.txt 2>/dev/null" | tail -4; }
# re-poison immediately before the snapshot (the pings above may have re-resolved
# the real MAC), then read the cache with no legit ARP in between.
spoof; sleep 1
poisoned=$($SSH "root@$victim_ip" "ip neigh show $target_l2 dev vxlan0" 2>/dev/null || true)
echo "$poisoned" | grep -qi "$attacker_mac" \
  && c_ok "victim POISONED — target now maps to the attacker's MAC: $poisoned" \
  || c_no "spoof failed to poison the victim (lab should allow it): $poisoned"

# --- Phase 5: CONTAINMENT — no leak to the public NIC; wg0 intact ------------
c_in "[5] containment: attack does not leak to the public NIC; wg0 hardened..."
PUBIF=$($SSH "root@$victim_ip" "ip route get 1.1.1.1 2>/dev/null | grep -oE 'dev [^ ]+' | awk '{print \$2}' | head -1" 2>/dev/null); PUBIF="${PUBIF:-eth0}"
$SSH "root@$victim_ip" "rm -f /tmp/pub.txt; tmux new-session -d -s pub 'timeout 8 tcpdump -ni $PUBIF arp or icmp -c 20 > /tmp/pub.txt 2>&1'" >/dev/null 2>&1
sleep 2
$SSH "root@$attacker_ip" "python3 /usr/local/bin/l2agent.py spoof vxlan0 $victim_mac $attacker_mac $target_l2 $victim_l2 $victim_mac" >/dev/null 2>&1 || true
$SSH "root@$victim_ip" "ping -c2 -W2 $target_l2 >/dev/null 2>&1" || true
sleep 8
# the forged ARP (192.168.66.x) must NOT appear on the public NIC.
# (grep -c prints its count AND exits 1 on no match, so fold + take the last line.)
pub=$($SSH "root@$victim_ip" "grep -c '192.168.66' /tmp/pub.txt 2>/dev/null || echo 0" | tail -1 | tr -cd '0-9')
[ "${pub:-0}" = 0 ] && c_ok "no overlay/attack frames on the public NIC $PUBIF (contained)" || c_no "$pub overlay frames leaked to $PUBIF!"
$SSH "root@$victim_ip" "ip link show wg0 >/dev/null 2>&1 && nft list table inet pandion >/dev/null 2>&1" \
  && c_ok "wg0 up + hardened firewall intact (management plane unaffected)" || c_no "management plane changed"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "L2 LAB (cyber-range) on $PROV: VERIFIED" || c_no "see failures above"
[ "$PASS" = 1 ]
