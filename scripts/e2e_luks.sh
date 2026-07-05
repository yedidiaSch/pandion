#!/usr/bin/env bash
# ============================================================================
# Pandion LUKS-at-rest e2e (S-E): with --encrypt-workspace, the run user's
# workspace is a LUKS-encrypted volume (ephemeral RAM key). Verified via
# `pandion ssh`. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_luks.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-luks"
BIN="./bin/pandion"
WORKDIR="/home/pandion-run/workspace"   # default run user's workspace
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/luks_*.log "$HOME/.pandion/lock/$ID.json"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "provision one node with --encrypt-workspace (~3-5 min)..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node box \
     --ttl 30m --encrypt-workspace -- 'echo PANDION_READY' >/tmp/luks_up.log 2>&1; then :; fi
grep -q "node is live" /tmp/luks_up.log && c_ok "provisioned" || { c_no "up did not complete"; tail -20 /tmp/luks_up.log; }

# verify (as root, via pandion ssh) that the workspace is a dm-crypt mount.
c_in "pandion ssh — verifying the workspace is LUKS-encrypted..."
NO_COLOR=1 "$BIN" ssh --id "$ID" --node box -- \
  "echo SRC=\$(findmnt -n -o SOURCE $WORKDIR 2>/dev/null); echo CRYPT=\$(lsblk -o TYPE -n 2>/dev/null | grep -c crypt); cryptsetup status pandion_enc 2>/dev/null | head -3" \
  >/tmp/luks_probe.log 2>&1 || true
echo "---- probe output ----"; cat /tmp/luks_probe.log; echo "----------------------"

grep -q "SRC=/dev/mapper/pandion_enc" /tmp/luks_probe.log \
  && c_ok "workspace is mounted on the LUKS device (/dev/mapper/pandion_enc)" \
  || c_no "workspace is not on the encrypted device"
grep -qE "CRYPT=[1-9]" /tmp/luks_probe.log \
  && c_ok "a dm-crypt device is present (data encrypted at rest)" \
  || c_no "no dm-crypt device found"
grep -qiE "type: *LUKS2|cipher" /tmp/luks_probe.log \
  && c_ok "cryptsetup confirms an active LUKS2 volume" \
  || c_in "note: could not read cryptsetup status"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "LUKS-AT-REST (S-E): verified" || c_no "see failures above"
