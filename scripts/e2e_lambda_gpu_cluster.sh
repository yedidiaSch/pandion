#!/usr/bin/env bash
# ============================================================================
# Pandion multi-node GPU cluster e2e (M5) — real 2-node GPU mesh on Lambda.
# SPENDS MONEY (2× the single-node cost).
#
# Proves `up --gpu -f cluster.yaml` end to end:
#   [1] provision a 2-node GPU cluster (defaults.gpu: a10), hardened + WireGuard-
#       meshed, each node running nvidia-smi + printing its rendezvous env.
#   [2] each node has a live GPU (nvidia-smi) and the rendezvous env
#       (PANDION_RANK / WORLD_SIZE / MASTER_ADDR) from the discovery file.
#   [3] the MESH works: each node reaches the rank-0 master over the overlay.
#   [4] teardown: destroy all nodes + POLL until gone.
#
# Safety: teardown trap on ANY exit; per-node --ttl is a second net.
# Usage:  ./scripts/e2e_lambda_gpu_cluster.sh [MODEL]   (MODEL default: a10)
# Requires: LAMBDA_API_KEY (env or ./.env).
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -z "${LAMBDA_API_KEY:-}" ] && [ -f ./.env ]; then set -a; . ./.env; set +a; fi
if [ -z "${LAMBDA_API_KEY:-}" ]; then echo "LAMBDA_API_KEY not set"; exit 2; fi

MODEL="${1:-a10}"
ID="e2e-gpu-mesh"
BIN="$PWD/bin/pandion"
KD="$HOME/.pandion/keys/$ID"
MAN="$KD/manifest.json"
YAML="$(mktemp --suffix=.yaml)"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$? i n
  echo; c_in "tearing down (exit $code) — destroying + polling until gone..."
  "$BIN" down --provider lambda --id "$ID" --yes >/dev/null 2>&1 || true
  for i in $(seq 1 24); do
    n=$("$BIN" ls --provider lambda 2>/dev/null | grep -c "$ID" || true)
    [ "$n" = 0 ] && { c_in "confirmed gone — nothing left billing"; break; }
    c_in "  still terminating ($n) [$i/24]"; sleep 15
  done
  rm -f "$YAML"
  [ "${n:-1}" = 0 ] || c_no "!!! nodes MAY STILL BE BILLING — check https://cloud.lambda.ai and destroy '$ID'"
}
trap teardown EXIT INT TERM

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:/usr/local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# 2-node GPU topology. The run command prints the rendezvous env and pings the
# rank-0 master over the overlay — a real mesh check.
RUN='nvidia-smi -L; . /etc/profile.d/pandion.sh; echo "PANDION_RDV rank=$PANDION_RANK world=$PANDION_WORLD_SIZE master=$PANDION_MASTER_ADDR"; ping -c2 -W3 "$PANDION_MASTER_ADDR" >/dev/null 2>&1 && echo PANDION_MESH_OK || echo PANDION_MESH_FAIL'
cat > "$YAML" <<EOF
apiVersion: pandion/v1
name: $ID
provider:
  name: lambda
defaults:
  gpu: $MODEL
  ttl: 20m
nodes:
  - name: node-0
    run: '$RUN'
  - name: node-1
    run: '$RUN'
EOF

c_in "[1] up -f (2-node GPU mesh, defaults.gpu: $MODEL)..."
if ! timeout 1500 env NO_COLOR=1 "$BIN" up --provider lambda -f "$YAML" --id "$ID" --max-cost 8 >/tmp/gpu_mesh_up.log 2>&1; then
  c_no "up failed:"; tail -25 /tmp/gpu_mesh_up.log; exit 1
fi
[ -f "$MAN" ] || { c_no "no manifest — provisioning failed"; tail -25 /tmp/gpu_mesh_up.log; exit 1; }
mapfile -t IPS < <(python3 -c "import json;[print(n['ip']) for n in json.load(open('$MAN'))['nodes']]")
c_ok "provisioned ${#IPS[@]} nodes: ${IPS[*]}"

SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15"

c_in "[2]/[3] per-node: GPU live + rendezvous env + mesh reachability..."
for ip in "${IPS[@]}"; do
  log=""
  for _ in $(seq 1 20); do
    log=$($SSH "root@$ip" 'cat /var/log/pandion/run.log 2>/dev/null' 2>/dev/null || true)
    echo "$log" | grep -q "PANDION_RDV" && break
    sleep 6
  done
  echo "$log" | grep -qi "NVIDIA $MODEL\|NVIDIA-SMI\|GPU 0" && c_ok "$ip: GPU live" || c_no "$ip: no GPU banner"
  rdv=$(echo "$log" | grep -o "PANDION_RDV.*" | head -1)
  echo "$rdv" | grep -qE "rank=[0-9]+ world=2 master=10\." && c_ok "$ip: rendezvous env ($rdv)" || c_no "$ip: bad rendezvous env ($rdv)"
  echo "$log" | grep -q "PANDION_MESH_OK" && c_ok "$ip: reached master over the overlay (mesh works)" || c_no "$ip: mesh check failed"
done

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "MULTI-NODE GPU MESH: verified (teardown next)" || c_no "see failures above (teardown next)"
