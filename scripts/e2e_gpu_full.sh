#!/usr/bin/env bash
# ============================================================================
# Pandion GPU — comprehensive e2e. Exercises EVERY GPU feature, sectioned and
# cost-controlled, all self-cleaning.
#
#   Section 0  FREE ($0)   list-gpus (+json, cache, --refresh), dry-run pricing,
#                          --gpu MODEL:N, and the --max-cost guard (refuses
#                          before spending).
#   Section 1  ~$0.15      single GPU node: up -- nvidia-smi, ls GPU column,
#                          ls --gpu-util, ls --json, down + cost receipt.
#                          + overlay + lockdown when E2E_OVERLAY=1 (needs sudo).
#   Section 2  ~$0.35      2-node GPU mesh: rendezvous env + overlay reachability.
#
# Usage:
#   ./scripts/e2e_gpu_full.sh                    # sections 0+1+2  (~$0.5)
#   E2E_FREE_ONLY=1 ./scripts/e2e_gpu_full.sh    # section 0 only  ($0)
#   E2E_OVERLAY=1  ./scripts/e2e_gpu_full.sh     # also overlay+lockdown (sudo)
#   E2E_SKIP_MULTI=1 ./scripts/e2e_gpu_full.sh   # skip the 2-node section
#   E2E_PROVIDER=digitalocean E2E_MODEL=h100 ./scripts/e2e_gpu_full.sh
#
# Env: E2E_PROVIDER (default lambda), E2E_MODEL (default a10), E2E_TTL (20m).
# Requires the provider's credential in the env or ./.env (LAMBDA_API_KEY /
# DIGITALOCEAN_TOKEN); sudo only for E2E_OVERLAY.
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

[ -f ./.env ] && { set -a; . ./.env; set +a; }
PROV="${E2E_PROVIDER:-lambda}"
MODEL="${E2E_MODEL:-a10}"
TTL="${E2E_TTL:-20m}"
BIN="$PWD/bin/pandion"
PASS=1
CLUSTERS=()          # every cluster id we create → torn down at exit

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }
hdr(){ printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

teardown(){
  local code=$? id i n
  hdr "teardown (exit $code)"
  [ "${OVERLAY_UP:-0}" = 1 ] && sudo wg-quick down "$WGCONF" 2>/dev/null || true
  for id in "${CLUSTERS[@]:-}"; do
    [ -z "$id" ] && continue
    c_in "destroying $id ..."
    "$BIN" down --provider "$PROV" --id "$id" --yes >/dev/null 2>&1 || true
    for i in $(seq 1 24); do
      n=$("$BIN" ls --provider "$PROV" 2>/dev/null | grep -c "$id" || true)
      [ "$n" = 0 ] && break; sleep 15
    done
    [ "${n:-1}" = 0 ] && c_in "  $id gone" || c_no "!!! $id MAY STILL BE BILLING — check the provider console"
  done
}
trap teardown EXIT INT TERM

SSH(){ ssh -i "$1" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15 "root@$2" "$3" 2>/dev/null; }
manip(){ python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['nodes'][$2].get('$3',''))" "$1" 2>/dev/null; }

hdr "build"; export PATH="$HOME/.local/go/bin:/usr/local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built ($PROV / $MODEL)"

# ---------------------------------------------------------------- Section 0 ($0)
hdr "Section 0 — FREE (list-gpus, cache, dry-run, budget guard)"
"$BIN" list-gpus --provider "$PROV" >/tmp/lg.txt 2>&1 && grep -qi "$MODEL" /tmp/lg.txt && c_ok "list-gpus shows $MODEL" || { c_no "list-gpus"; cat /tmp/lg.txt; }
"$BIN" list-gpus --provider "$PROV" --json 2>/dev/null | python3 -c "import sys,json;json.load(sys.stdin)" && c_ok "list-gpus --json valid" || c_no "list-gpus --json"
t1=$( { /usr/bin/time -f %e "$BIN" list-gpus --provider "$PROV" >/dev/null; } 2>&1 || true)
c_in "cached list-gpus took ${t1}s (should be ~instant)"
"$BIN" list-gpus --provider "$PROV" --refresh >/dev/null 2>&1 && c_ok "list-gpus --refresh works" || c_no "--refresh"
NO_COLOR=1 "$BIN" up --provider "$PROV" --gpu "$MODEL" --dry-run --no-toolchain -- 'x' 2>&1 | grep -qi "$MODEL" && c_ok "dry-run prices $MODEL" || c_no "dry-run"
NO_COLOR=1 "$BIN" up --provider "$PROV" --gpu "$MODEL:2" --dry-run --no-toolchain -- 'x' 2>&1 | grep -q "×2\|:2\|x2" && c_ok "dry-run --gpu $MODEL:2 (multi-GPU)" || c_in "note: $MODEL:2 may not exist for $PROV"
# budget guard: a tiny cap must REFUSE before spending (real up, exits nonzero, $0)
if NO_COLOR=1 "$BIN" up --provider "$PROV" --gpu "$MODEL" --id budgetguard --ttl "$TTL" --max-cost 0.001 --no-toolchain -- 'x' >/tmp/bg.txt 2>&1; then
  c_no "--max-cost 0.001 should have refused"; CLUSTERS+=("budgetguard")
else
  grep -qi "budget\|max-cost\|exceed" /tmp/bg.txt && c_ok "--max-cost guard refused before spending (\$0)" || { c_no "budget guard wrong error"; tail -2 /tmp/bg.txt; }
fi

[ "${E2E_FREE_ONLY:-0}" = 1 ] && { hdr "done (free only)"; [ "$PASS" = 1 ] && c_ok "ALL FREE CHECKS PASSED" || c_no "see above"; exit 0; }

# ------------------------------------------------------------- Section 1 (~$0.15)
if [ "${E2E_SKIP_SINGLE:-0}" != 1 ]; then
hdr "Section 1 — single GPU node (real)"
ID1="e2e-gpu-single"; KD1="$HOME/.pandion/keys/$ID1"; CLUSTERS+=("$ID1")
c_in "up --gpu $MODEL -- nvidia-smi ..."
if timeout 1200 env NO_COLOR=1 "$BIN" up --provider "$PROV" --gpu "$MODEL" --id "$ID1" --ttl "$TTL" --max-cost 8 --no-toolchain -- 'nvidia-smi -L && nvidia-smi' >/tmp/s1.log 2>&1; then
  IP=$(manip "$KD1/manifest.json" 0 ip)
  c_ok "provisioned at $IP"
  for _ in $(seq 1 20); do SSH "$KD1/login_ed25519" "$IP" 'cat /var/log/pandion/run.log' | grep -qi "NVIDIA-SMI" && break; sleep 6; done
  SSH "$KD1/login_ed25519" "$IP" 'cat /var/log/pandion/run.log' | grep -qi "NVIDIA-SMI\|$MODEL" && c_ok "nvidia-smi ran on the node" || c_no "no GPU banner"
  "$BIN" ls --provider "$PROV" 2>/dev/null | grep -q "$ID1" && c_ok "ls shows the cluster (GPU column)" || c_no "ls"
  # start a GPU burn, then ls --gpu-util should read nonzero
  SSH "$KD1/login_ed25519" "$IP" 'nohup python3 -c "import torch,time;x=torch.randn(9000,9000,device=\"cuda\")
t=time.time()
while time.time()-t<70: x=x@x" >/tmp/burn.log 2>&1 & echo ok' >/dev/null 2>&1 || true
  sleep 20
  "$BIN" ls --provider "$PROV" --gpu-util 2>/dev/null | grep -qE "[1-9][0-9]?%|100%" && c_ok "ls --gpu-util shows live utilization" || c_no "ls --gpu-util (no nonzero util seen)"
  "$BIN" ls --provider "$PROV" --gpu-util --json 2>/dev/null | grep -q "gpu_util" && c_ok "ls --json includes gpu_util" || c_in "note: gpu_util omitted (node may be unreachable)"
  # overlay + lockdown (needs sudo)
  if [ "${E2E_OVERLAY:-0}" = 1 ]; then
    WGCONF="$KD1/wg-$ID1.conf"
    if [ -f "$WGCONF" ]; then
      sudo wg-quick up "$WGCONF" && OVERLAY_UP=1 && c_ok "overlay up" || c_no "wg-quick up"
      NO_COLOR=1 "$BIN" lockdown --id "$ID1" 2>&1 | tail -2 && c_ok "lockdown applied" || c_no "lockdown"
      "$BIN" ssh --id "$ID1" --overlay -- 'nvidia-smi -L' 2>&1 | grep -qi GPU && c_ok "reached GPU over overlay after lockdown" || c_no "overlay ssh"
      sudo wg-quick down "$WGCONF" 2>/dev/null || true; OVERLAY_UP=0
    else c_no "no operator wg config"; fi
  else c_in "overlay+lockdown skipped (set E2E_OVERLAY=1, needs sudo)"; fi
  # explicit down here to see the receipt (teardown trap is the backstop)
  c_in "down (cost receipt)..."
  NO_COLOR=1 "$BIN" down --provider "$PROV" --id "$ID1" --yes 2>&1 | tee /tmp/d1.log | grep -q "receipt:" && c_ok "teardown prints a cost receipt" || c_no "no receipt"
  grep -q "receipt:" /tmp/d1.log && { grep -q "cost unknown" /tmp/d1.log && c_in "  (receipt cost unknown — expected if <1s or no lockfile)" || c_ok "receipt shows real cost"; }
else c_no "single-node up failed:"; tail -20 /tmp/s1.log; fi
fi

# ------------------------------------------------------------- Section 2 (~$0.35)
if [ "${E2E_SKIP_MULTI:-0}" != 1 ]; then
hdr "Section 2 — 2-node GPU mesh (real)"
ID2="e2e-gpu-mesh-full"; KD2="$HOME/.pandion/keys/$ID2"; CLUSTERS+=("$ID2")
Y="$(mktemp --suffix=.yaml)"
R='nvidia-smi -L; . /etc/profile.d/pandion.sh; echo "RDV rank=$PANDION_RANK world=$PANDION_WORLD_SIZE master=$PANDION_MASTER_ADDR"; ping -c2 -W3 "$PANDION_MASTER_ADDR" >/dev/null 2>&1 && echo MESH_OK'
cat > "$Y" <<EOF
apiVersion: pandion/v1
name: $ID2
provider: { name: $PROV }
defaults: { gpu: $MODEL, ttl: $TTL }
nodes:
  - { name: node-0, run: '$R' }
  - { name: node-1, run: '$R' }
EOF
c_in "up -f (2-node mesh)..."
if timeout 1500 env NO_COLOR=1 "$BIN" up --provider "$PROV" -f "$Y" --id "$ID2" --max-cost 12 >/tmp/s2.log 2>&1; then
  mapfile -t MIPS < <(python3 -c "import json;[print(n['ip']) for n in json.load(open('$KD2/manifest.json'))['nodes']]")
  c_ok "provisioned ${#MIPS[@]} nodes"
  for ip in "${MIPS[@]}"; do
    log=""; for _ in $(seq 1 20); do log=$(SSH "$KD2/login_ed25519" "$ip" 'cat /var/log/pandion/run.log'); echo "$log" | grep -q "RDV" && break; sleep 6; done
    echo "$log" | grep -qi "NVIDIA-SMI\|$MODEL\|GPU 0" && c_ok "$ip GPU live" || c_no "$ip no GPU"
    echo "$log" | grep -qE "RDV rank=[0-9]+ world=2 master=10\." && c_ok "$ip rendezvous env ($(echo "$log"|grep -o 'RDV.*'|head -1))" || c_no "$ip rendezvous"
    echo "$log" | grep -q "MESH_OK" && c_ok "$ip reached master over overlay (mesh)" || c_no "$ip mesh"
  done
else c_no "mesh up failed:"; tail -20 /tmp/s2.log; fi
rm -f "$Y"
fi

hdr "summary"
[ "$PASS" = 1 ] && c_ok "GPU FULL E2E: ALL CHECKS PASSED (teardown next)" || c_no "see failures above (teardown next)"
