#!/usr/bin/env bash
# ============================================================================
# Pandion operator-UX e2e: `down --dry-run` previews without destroying, and
# `pandion code` emits a host-key-pinned SSH config that actually connects
# (validated with `ssh -F`). Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_operator.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-operator"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/op_*.log /tmp/op_*.config "$HOME/.pandion/lock/$ID.json" "$HOME/.pandion/ssh/$ID-"*.config
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

srv_count(){ hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true; }

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "provision one node (~3-5 min)..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node box \
     --ttl 30m -- 'echo PANDION_READY' >/tmp/op_up.log 2>&1; then :; fi
grep -q "node is live" /tmp/op_up.log && c_ok "provisioned" || { c_no "up did not complete"; tail -15 /tmp/op_up.log; }

# ---- down --dry-run: previews, destroys nothing ----
c_in "down --dry-run should list the node but NOT destroy it..."
NO_COLOR=1 "$BIN" down --provider=hetzner --id "$ID" --dry-run >/tmp/op_dry.log 2>&1 || true
cat /tmp/op_dry.log
grep -qi "to destroy" /tmp/op_dry.log && grep -qi "nothing destroyed" /tmp/op_dry.log \
  && c_ok "dry-run previewed the teardown" || c_no "dry-run banner missing"
[ "$(srv_count)" -ge 1 ] && c_ok "dry-run left the server intact" || c_no "dry-run destroyed the server!"

# ---- pandion code: generated pinned config actually connects ----
c_in "pandion code --print → connect with the generated pinned SSH config..."
NO_COLOR=1 "$BIN" code --id "$ID" --node box --print >/tmp/op_code.config 2>/tmp/op_code.err || true
echo "---- generated config ----"; sed 's/^/  /' /tmp/op_code.config; echo "--------------------------"
grep -q "Host pandion-$ID-box" /tmp/op_code.config && c_ok "config block generated" || c_no "no Host block"
if ssh -F /tmp/op_code.config -o ConnectTimeout=15 -o BatchMode=yes "pandion-$ID-box" 'echo CODE_SSH_OK; hostname' >/tmp/op_ssh.log 2>&1; then
  grep -q "CODE_SSH_OK" /tmp/op_ssh.log && c_ok "IDE SSH config connected (host-key pinned)" || c_no "connected but no output"
else
  c_no "ssh -F generated config failed to connect"; cat /tmp/op_ssh.log
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "OPERATOR UX (down --dry-run + pandion code): verified" || c_no "see failures above"
