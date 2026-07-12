#!/usr/bin/env bash
# ============================================================================
# Pandion DigitalOcean GPU Droplet e2e — the real thing, on a real GPU
# (SPENDS MONEY). DO GPU Droplets are the successor to Paperspace and use your
# existing DO token — no new provider or key.
#
# Proves the hardened GPU flow end to end on DO GPU Droplets:
#   [1] list-gpus shows the live, priced catalog (which SKUs have capacity).
#   [2] `up --gpu MODEL` provisions a real GPU droplet (CUDA-native "AI/ML Ready"
#       base image), host-key-pinned, and runs `nvidia-smi` ON it.
#   [3] (optional, needs sudo) join the WireGuard overlay + `lockdown`, then
#       `ssh --overlay nvidia-smi`.
#   [4] teardown: destroy + POLL until the droplet is truly gone.
#
# NOTE: DO gates GPU Droplets behind an account quota (new accounts default to
# 0). If you see "exceed your GPU limit", request GPU access in the DO console.
#
# Safety: a trap tears down on ANY exit (error / Ctrl-C); `--ttl` is a second net.
#
# Usage:
#   ./scripts/e2e_do_gpu.sh [MODEL]              # MODEL default: h100
#   E2E_OVERLAY=1 ./scripts/e2e_do_gpu.sh h100   # also test overlay+lockdown
#
# Requires: DIGITALOCEAN_TOKEN in the environment (or ./.env); sudo for --overlay.
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -z "${DIGITALOCEAN_TOKEN:-}" ] && [ -f ./.env ]; then set -a; . ./.env; set +a; fi
if [ -z "${DIGITALOCEAN_TOKEN:-}" ]; then echo "DIGITALOCEAN_TOKEN not set (export it or put it in ./.env)"; exit 2; fi

MODEL="${1:-h100}"                # GPU model; must currently have capacity
ID="e2e-do-gpu-$MODEL"
TTL="${E2E_TTL:-20m}"
MAXCOST="${E2E_MAXCOST:-6.00}"    # H100 is ~$3.39/hr; refuse if projected > this
BIN="$PWD/bin/pandion"
KD="$HOME/.pandion/keys/$ID"
MAN="$KD/manifest.json"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

# --- bulletproof teardown: destroy, then POLL until the cluster is gone -------
teardown(){
  local code=$? i n
  echo; c_in "tearing down (exit $code) — destroying + polling until gone..."
  "$BIN" down --provider digitalocean --id "$ID" --yes >/dev/null 2>&1 || true
  for i in $(seq 1 24); do
    n=$("$BIN" ls --provider digitalocean 2>/dev/null | grep -c "$ID" || true)
    if [ "$n" = 0 ]; then c_in "confirmed gone — nothing left billing"; break; fi
    c_in "  still destroying ($n) — waiting 15s [$i/24]"; sleep 15
  done
  [ "${n:-1}" = 0 ] || c_no "!!! droplet MAY STILL BE BILLING — check https://cloud.digitalocean.com and destroy '$ID' manually"
}
trap teardown EXIT INT TERM

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:/usr/local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
c_in "[1] list-gpus — live capacity (REGIONS column = launchable now)..."
"$BIN" list-gpus --provider digitalocean 2>&1 | head -20 || true

# ---------------------------------------------------------------------------
c_in "[2] up --gpu $MODEL --id $ID --ttl $TTL -- 'nvidia-smi'  (real GPU droplet)..."
if ! timeout 1200 env NO_COLOR=1 "$BIN" up --provider digitalocean --gpu "$MODEL" --id "$ID" \
      --ttl "$TTL" --max-cost "$MAXCOST" --no-toolchain -- 'nvidia-smi -L && nvidia-smi' \
      >/tmp/do_gpu_up.log 2>&1; then
  c_no "up failed:"; tail -20 /tmp/do_gpu_up.log
  grep -qi "gpu limit\|gpu quota" /tmp/do_gpu_up.log && \
    c_in "  -> your DO account has no GPU quota yet; request GPU Droplet access in the DO console, then retry."
  exit 1
fi
[ -f "$MAN" ] || { c_no "no manifest — provisioning failed"; tail -20 /tmp/do_gpu_up.log; exit 1; }
IP=$(python3 -c "import json;print(json.load(open('$MAN'))['nodes'][0]['ip'])")
c_ok "provisioned GPU droplet at $IP"

SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"
c_in "waiting for the nvidia-smi banner in the run log..."
ok=0
for _ in $(seq 1 20); do
  if $SSH "root@$IP" "cat /var/log/pandion/run.log 2>/dev/null" | grep -qiE "NVIDIA-SMI|CUDA Version|$MODEL"; then ok=1; break; fi
  sleep 6
done
if [ "$ok" = 1 ]; then
  c_ok "GPU is live — nvidia-smi ran on the droplet:"
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
[ "$PASS" = 1 ] && c_ok "DO GPU e2e: verified (teardown runs next)" || c_no "see failures above (teardown runs next)"
