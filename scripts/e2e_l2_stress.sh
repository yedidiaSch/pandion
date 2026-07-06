#!/usr/bin/env bash
# ============================================================================
# Pandion L2 overlay STRESS + conformance e2e (safe profile). Provisions an
# N-node cluster (default 5) with security.overlay: l2, deploys an on-node raw-
# Ethernet test agent, and pushes the segment to its limits:
#   A. delivery matrix — unicast full-mesh (N*(N-1)) + broadcast + multicast
#      fan-out to ALL peers (BUM head-end replication), over a size sweep
#   B. precise MTU boundary at L2 (<=inner MTU crosses; larger is dropped) + IP DF
#   C. ARP-spoof matrix — every node attacks; host DAI must block every forgery
#   D. isolation — L2 frames never leak to eth0; wg0 posture unchanged
#   E. concurrent load — all-pairs flood ping; assert low loss under stress
# Runs against ANY provider:  ./scripts/e2e_l2_stress.sh <provider> [N]
# Self-cleaning.  Requires the provider's token in the env (source .env).
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."

PROV="${1:?usage: e2e_l2_stress.sh <provider> [N]}"
N="${2:-5}"
ID="e2e-l2s-$PROV"
BIN="./bin/pandion"
PASS=1
c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ %-9s ]\033[0m %s\n' "$PROV" "$*"; }
WORK="/tmp/l2s-$PROV"; rm -rf "$WORK"; mkdir -p "$WORK"
teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -rf "$WORK" "/tmp/l2s_$PROV.yaml"
  c_in "done (exit $code)"
}
trap teardown EXIT

export PATH="$HOME/.local/go/bin:$PATH"
c_in "building..."; go build -o "$BIN" ./cmd/pandion >/dev/null 2>&1 && c_ok "built" || { c_no "build"; exit 1; }

# generate an N-node cluster.yaml
{ echo "apiVersion: pandion/v1"; echo "name: $ID"; echo "provider: { name: $PROV }"
  echo "defaults: { security: { overlay: l2 } }"; echo "nodes:"
  for i in $(seq 1 "$N"); do echo "  - { name: n$i, run: \"echo READY\" }"; done
} > "/tmp/l2s_$PROV.yaml"

c_in "[UP] $N-node L2 cluster on $PROV..."
if timeout 1200 env NO_COLOR=1 "$BIN" up --provider="$PROV" -f "/tmp/l2s_$PROV.yaml" --id "$ID" >"$WORK/up.log" 2>&1; then :; fi
# match the real success marker (avoid a false positive on "(cluster left up)").
grep -qiE "mesh verif" "$WORK/up.log" && c_ok "$N nodes up + mesh formed" \
  || { c_no "up did not complete"; tail -30 "$WORK/up.log"; exit 1; }

MAN="$HOME/.pandion/keys/$ID/manifest.json"; KD="$HOME/.pandion/keys/$ID"
SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"
# node arrays from the manifest, in a stable order
mapfile -t NAMES < <(python3 -c "import json;[print(n['name']) for n in json.load(open('$MAN'))['nodes']]" 2>/dev/null)
if [ "${#NAMES[@]}" -eq 0 ]; then
  c_no "no manifest / no nodes — cluster up errored after mesh-verify (before saveManifest)"
  echo "--- up.log tail ---"; tail -30 "$WORK/up.log"; exit 1
fi
declare -A IP L2 MAC
for nm in "${NAMES[@]}"; do
  IP[$nm]=$(python3 -c "import json;print([n['ip'] for n in json.load(open('$MAN'))['nodes'] if n['name']=='$nm'][0])")
  L2[$nm]=$(python3 -c "import json;print([n['l2_ip'] for n in json.load(open('$MAN'))['nodes'] if n['name']=='$nm'][0])")
  MAC[$nm]=$(python3 -c "import json;print([n['l2_mac'] for n in json.load(open('$MAN'))['nodes'] if n['name']=='$nm'][0])")
done
c_in "nodes: $(for nm in "${NAMES[@]}"; do printf '%s=%s ' "$nm" "${L2[$nm]}"; done)"

# deploy the agent to every node (base64; egress is locked so no fetch)
AGENT_B64=$(base64 -w0 scripts/l2agent.py)
for nm in "${NAMES[@]}"; do
  $SSH "root@${IP[$nm]}" "echo $AGENT_B64 | base64 -d > /usr/local/bin/l2agent.py && chmod 0755 /usr/local/bin/l2agent.py" >/dev/null 2>&1 &
done; wait
c_ok "agent deployed to all nodes"

# ---------------------------------------------------------------------------
# A. DELIVERY MATRIX — unicast full-mesh + broadcast + multicast fan-out
# ---------------------------------------------------------------------------
c_in "[A] delivery matrix: unicast mesh + broadcast/multicast fan-out (size sweep)..."
# start detached listeners (tmux) on every node
for nm in "${NAMES[@]}"; do
  $SSH "root@${IP[$nm]}" "tmux kill-session -t l2l 2>/dev/null; tmux new-session -d -s l2l 'python3 /usr/local/bin/l2agent.py listen vxlan0 16 /tmp/l2seen.txt'" >/dev/null 2>&1 &
done; wait
sleep 3
# every node sends its battery (unicast to each peer + broadcast + multicast)
for nm in "${NAMES[@]}"; do
  pj="["
  for pn in "${NAMES[@]}"; do [ "$pn" = "$nm" ] && continue; pj+="{\"id\":\"$pn\",\"mac\":\"${MAC[$pn]}\"},"; done
  pj="${pj%,}]"
  $SSH "root@${IP[$nm]}" "python3 /usr/local/bin/l2agent.py send vxlan0 $nm '$pj'" >/dev/null 2>&1 &
done; wait
sleep 15  # let the 16s listen windows finish + write files
# collect
for nm in "${NAMES[@]}"; do
  $SSH "root@${IP[$nm]}" "cat /tmp/l2seen.txt 2>/dev/null" > "$WORK/$nm.seen" 2>/dev/null || true
done
# analyze the matrix
python3 - "$WORK" "${NAMES[@]}" > "$WORK/analysis.txt" <<'PY'
import sys,os,collections
work=sys.argv[1]; names=sys.argv[2:]
# recv[node] = set of (kind, src, size)
recv=collections.defaultdict(set)
for nm in names:
    p=os.path.join(work,nm+".seen")
    if not os.path.exists(p): continue
    for line in open(p):
        f=line.split()
        if len(f)>=4: recv[nm].add((f[0],f[1],f[3]))
ok_sizes={"64","512","1300","1370"}; big_sizes={"1372","1450"}
# unicast full mesh: every ordered pair A->B delivered at all ok sizes
uni_missing=[];
for a in names:
  for b in names:
    if a==b: continue
    for sz in ok_sizes:
      if ("U",a,sz) not in recv[b]: uni_missing.append((a,b,sz))
# broadcast fan-out: every node's B reached all peers
bc_missing=[(a,b) for a in names for b in names if a!=b and not any(("B",a,s) in recv[b] for s in ("64","1300"))]
# multicast fan-out
mc_missing=[(a,b) for a in names for b in names if a!=b and not any(("M",a,s) in recv[b] for s in ("64","1300"))]
# MTU boundary: NO oversize frame should ever arrive
mtu_leak=[(k,s,sz) for s in recv for (k,src,sz) in recv[s] if sz in big_sizes]
tot=len(names)*(len(names)-1)
print("UNICAST_MESH", tot*len(ok_sizes)-len(uni_missing), "of", tot*len(ok_sizes), "missing", uni_missing[:6])
print("BROADCAST", tot-len(bc_missing), "of", tot, "missing", bc_missing[:6])
print("MULTICAST", tot-len(mc_missing), "of", tot, "missing", mc_missing[:6])
print("MTU_OVERSIZE_LEAK", len(mtu_leak), mtu_leak[:4])
PY
cat "$WORK/analysis.txt"
au=$(grep UNICAST_MESH "$WORK/analysis.txt"); grep -q "missing \[\]" <<<"$au" && c_ok "unicast full-mesh: $au" || c_no "unicast mesh incomplete: $au"
ab=$(grep '^BROADCAST' "$WORK/analysis.txt"); grep -q "missing \[\]" <<<"$ab" && c_ok "broadcast fan-out to ALL peers: $ab" || c_no "broadcast fan-out incomplete: $ab"
am=$(grep '^MULTICAST' "$WORK/analysis.txt"); grep -q "missing \[\]" <<<"$am" && c_ok "multicast fan-out: $am" || c_no "multicast fan-out incomplete: $am"
al=$(grep MTU_OVERSIZE "$WORK/analysis.txt"); grep -q "LEAK 0 " <<<"$al" && c_ok "MTU boundary: no oversize frame crossed" || c_no "MTU boundary breached: $al"

# ---------------------------------------------------------------------------
# B. MTU via IP DF (complementary to the raw-frame boundary above)
# ---------------------------------------------------------------------------
n1=${NAMES[0]}; n2=${NAMES[1]}
c_in "[B] IP MTU boundary (DF) n1->n2..."
$SSH "root@${IP[$n1]}" "ping -c2 -W3 -M do -s 1342 ${L2[$n2]} >/dev/null 2>&1" && sub=ok || sub=no
$SSH "root@${IP[$n1]}" "ping -c1 -W3 -M do -s 1500 ${L2[$n2]} >/dev/null 2>&1" && over=ok || over=no
[ "$sub" = ok ] && [ "$over" = no ] && c_ok "IP DF: 1342B crosses, 1500B rejected (MTU enforced)" || c_no "IP MTU boundary wrong (sub=$sub over=$over)"

# ---------------------------------------------------------------------------
# C. ARP-SPOOF MATRIX — every node attacks; DAI must block every forgery
# ---------------------------------------------------------------------------
c_in "[C] ARP-spoof matrix: each node forges, victim must never be poisoned..."
spoof_fail=0
for idx in "${!NAMES[@]}"; do
  atk=${NAMES[$idx]}; vic=${NAMES[$(( (idx+1)%N ))]}; imp=${NAMES[$(( (idx+2)%N ))]}
  [ "$vic" = "$imp" ] && continue
  # victim learns imp's real MAC, then attacker forges 'imp is at attacker mac'
  $SSH "root@${IP[$vic]}" "ping -c1 -W2 ${L2[$imp]} >/dev/null 2>&1" || true
  $SSH "root@${IP[$atk]}" "python3 /usr/local/bin/l2agent.py spoof vxlan0 ${MAC[$vic]} ${MAC[$atk]} ${L2[$imp]} ${L2[$vic]} ${MAC[$vic]}" >/dev/null 2>&1 || true
  sleep 0.5
  got=$($SSH "root@${IP[$vic]}" "ip neigh show ${L2[$imp]} dev vxlan0" 2>/dev/null || true)
  if echo "$got" | grep -qi "${MAC[$atk]}"; then spoof_fail=$((spoof_fail+1)); echo "  POISONED: $atk spoofed $imp on $vic -> $got"; fi
done
[ "$spoof_fail" = 0 ] && c_ok "DAI blocked every ARP forgery ($N attackers)" || c_no "$spoof_fail spoof(s) succeeded — DAI breached"

# ---------------------------------------------------------------------------
# D. ISOLATION — L2 probes must NOT leak to eth0; wg0 posture intact
# ---------------------------------------------------------------------------
c_in "[D] isolation: no L2 leak to the public NIC; management plane intact..."
# the public interface name varies by provider (eth0/ens3/...) — detect the one
# carrying the default route so the leak check is genuine on every provider.
PUBIF=$($SSH "root@${IP[$n2]}" "ip route get 1.1.1.1 2>/dev/null | grep -oE 'dev [^ ]+' | awk '{print \$2}' | head -1" 2>/dev/null)
PUBIF="${PUBIF:-eth0}"
$SSH "root@${IP[$n2]}" "rm -f /tmp/eth.txt; tmux new-session -d -s l2e 'python3 /usr/local/bin/l2agent.py listen $PUBIF 8 /tmp/eth.txt'" >/dev/null 2>&1
sleep 2
$SSH "root@${IP[$n1]}" "python3 /usr/local/bin/l2agent.py send vxlan0 $n1 '[{\"id\":\"$n2\",\"mac\":\"${MAC[$n2]}\"}]'" >/dev/null 2>&1 || true
sleep 8
leak=$($SSH "root@${IP[$n2]}" "test -f /tmp/eth.txt && wc -l < /tmp/eth.txt || echo MISSING" 2>/dev/null || echo MISSING)
if [ "$leak" = "MISSING" ]; then c_no "isolation listener on $PUBIF did not run (inconclusive)"
elif [ "${leak:-1}" = 0 ]; then c_ok "no L2 probe frames on the public NIC $PUBIF (segment contained)"
else c_no "$leak L2 frames leaked to $PUBIF!"; fi
$SSH "root@${IP[$n1]}" "ip link show wg0 >/dev/null 2>&1 && nft list table inet pandion >/dev/null 2>&1" \
  && c_ok "wg0 up + hardened firewall intact" || c_no "management plane changed"

# ---------------------------------------------------------------------------
# E. CONCURRENT LOAD — all nodes flood-ping their next peer at once
# ---------------------------------------------------------------------------
c_in "[E] concurrent load: all $N nodes flood-ping simultaneously..."
for idx in "${!NAMES[@]}"; do
  a=${NAMES[$idx]}; b=${NAMES[$(( (idx+1)%N ))]}
  $SSH "root@${IP[$a]}" "ping -f -c 800 -W1 ${L2[$b]} 2>/dev/null | grep -oE '[0-9]+% packet loss' | grep -oE '^[0-9]+'" > "$WORK/$a.loss" 2>/dev/null &
done; wait
worst=0
for nm in "${NAMES[@]}"; do l=$(cat "$WORK/$nm.loss" 2>/dev/null || echo 100); [ "${l:-100}" -gt "$worst" ] && worst=$l; done
[ "$worst" -le 5 ] && c_ok "concurrent flood: worst-node loss ${worst}% (<=5%)" || c_no "concurrent flood: worst-node loss ${worst}% (>5%)"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "L2 STRESS on $PROV ($N nodes): VERIFIED" || c_no "L2 STRESS on $PROV: FAILURES above"
[ "$PASS" = 1 ]
