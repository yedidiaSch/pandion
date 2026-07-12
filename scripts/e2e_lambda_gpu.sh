#!/usr/bin/env bash
# ============================================================================
# Pandion Lambda GPU e2e — the real thing, on a real GPU (SPENDS MONEY).
#
# Proves the hardened GPU flow end to end on Lambda Cloud:
#   [1] list-gpus shows the live, priced catalog (which SKUs have capacity).
#   [2] `up --gpu MODEL` provisions a real GPU node, host-key-pinned, and runs
#       `nvidia-smi` ON the node — the GPU banner shows up in the run log.
#   [3] (optional, needs sudo) join the WireGuard overlay + `lockdown` so the
#       node is reachable ONLY over the overlay, then `ssh --overlay nvidia-smi`.
#   [4] teardown: terminate + POLL until the node is truly gone (Lambda
#       terminates asynchronously), so nothing is left billing.
#
# Safety: a trap tears down on ANY exit (error / Ctrl-C). `--ttl` is a second
# net — the node self-powers-off if this script dies before teardown.
#
# Usage:
#   ./scripts/e2e_lambda_gpu.sh [MODEL]      # MODEL default: a10 (cheapest)
#   E2E_OVERLAY=1 ./scripts/e2e_lambda_gpu.sh a100   # also test overlay+lockdown
#
# Requires: LAMBDA_API_KEY in the environment (or ./.env), and — for the
# optional overlay step — sudo (to `wg-quick up`).
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

# pull LAMBDA_API_KEY from ./.env if present and not already exported
if [ -z "${LAMBDA_API_KEY:-}" ] && [ -f ./.env ]; then set -a; . ./.env; set +a; fi
if [ -z "${LAMBDA_API_KEY:-}" ]; then echo "LAMBDA_API_KEY not set (export it or put it in ./.env)"; exit 2; fi

MODEL="${1:-a10}"          # GPU model to request; must currently have capacity
ID="e2e-gpu-$MODEL"
TTL="${E2E_TTL:-20m}"      # dead-man safety net
MAXCOST="${E2E_MAXCOST:-6.00}"   # refuse to launch if projected spend exceeds this
BIN="$PWD/bin/pandion"
KD="$HOME/.pandion/keys/$ID"
MAN="$KD/manifest.json"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

# --- bulletproof teardown: terminate, then POLL until the cluster is gone -----
teardown(){
  local code=$? i n
  echo; c_in "tearing down (exit $code) — terminating + polling until gone..."
  "$BIN" down --provider lambda --id "$ID" --yes >/dev/null 2>&1 || true
  for i in $(seq 1 24); do
    n=$("$BIN" ls --provider lambda 2>/dev/null | grep -c "$ID" || true)
    if [ "$n" = 0 ]; then c_in "confirmed gone — nothing left billing"; break; fi
    c_in "  still terminating ($n) — waiting 15s [$i/24]"; sleep 15
  done
  [ "${n:-1}" = 0 ] || c_no "!!! node MAY STILL BE BILLING — check https://cloud.lambda.ai and terminate '$ID' manually"
}
trap teardown EXIT INT TERM

c_in "building pandion (from current tree, incl. the async-terminate fix)..."
export PATH="$HOME/.local/go/bin:/usr/local/go/bin:$PATH"
go build -o "$BIN" ./cmd/pandion
c_ok "built"

# ---------------------------------------------------------------------------
c_in "[1] list-gpus — live capacity (REGIONS column = launchable now)..."
"$BIN" list-gpus --provider lambda 2>&1 | head -25 || true

# ---------------------------------------------------------------------------
c_in "[2] up --gpu $MODEL --id $ID --ttl $TTL -- 'nvidia-smi'  (real GPU node)..."
if ! timeout 1200 env NO_COLOR=1 "$BIN" up --provider lambda --gpu "$MODEL" --id "$ID" \
      --ttl "$TTL" --max-cost "$MAXCOST" --no-toolchain -- 'nvidia-smi -L && nvidia-smi' \
      >/tmp/gpu_up.log 2>&1; then
  c_no "up failed:"; tail -30 /tmp/gpu_up.log; exit 1
fi
[ -f "$MAN" ] || { c_no "no manifest — provisioning failed"; tail -30 /tmp/gpu_up.log; exit 1; }
IP=$(python3 -c "import json;print(json.load(open('$MAN'))['nodes'][0]['ip'])")
c_ok "provisioned GPU node at $IP"

SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"
c_in "waiting for the nvidia-smi banner in the run log..."
ok=0
for _ in $(seq 1 20); do
  if $SSH "root@$IP" "cat /var/log/pandion/run.log 2>/dev/null" | grep -qiE "NVIDIA-SMI|CUDA Version|$MODEL"; then ok=1; break; fi
  sleep 6
done
if [ "$ok" = 1 ]; then
  c_ok "GPU is live — nvidia-smi ran on the node:"
  $SSH "root@$IP" 'cat /var/log/pandion/run.log' 2>/dev/null | grep -iE "NVIDIA-SMI|CUDA Version|GPU" | head -6
else
  c_no "no GPU banner in the run log"; $SSH "root@$IP" 'tail -30 /var/log/pandion/run.log' 2>&1 | head
fi

# ---------------------------------------------------------------------------
if [ "${E2E_OVERLAY:-0}" = 1 ]; then
  CONF="$KD/wg-$ID.conf"
  if [ -f "$CONF" ]; then
    c_in "[3] joining the WireGuard overlay (needs sudo) + lockdown..."
    sudo wg-quick up "$CONF" && c_ok "overlay up" || c_no "wg-quick up failed"
    NO_COLOR=1 "$BIN" lockdown --id "$ID" 2>&1 | tail -3 && c_ok "lockdown applied (public deny-all)" || c_no "lockdown failed"
    if "$BIN" ssh --id "$ID" --overlay -- 'nvidia-smi -L' 2>&1 | grep -qi "GPU"; then
      c_ok "reached the GPU over the overlay AFTER lockdown"
    else
      c_no "could not reach the node over the overlay"
    fi
    sudo wg-quick down "$CONF" 2>/dev/null || true
  else
    c_no "no operator WG config at $CONF (was --overlay disabled on up?)"
  fi
else
  c_in "[3] overlay+lockdown skipped (set E2E_OVERLAY=1, needs sudo, to test it)"
fi

# ---------------------------------------------------------------------------
echo "================================================================"
[ "$PASS" = 1 ] && c_ok "LAMBDA GPU e2e: verified (teardown runs next)" || c_no "see failures above (teardown runs next)"
