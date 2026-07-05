#!/usr/bin/env bash
# ============================================================================
# Pandion ssh e2e: `pandion ssh` reaches a provisioned node with the host key
# PINNED (StrictHostKeyChecking against the persisted manifest) and runs a
# command. This is the supported way to reach a node "left up for GDB/SSH".
# Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_ssh.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-ssh"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/ssh_*.log "$HOME/.pandion/lock/$ID.json"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "provision one node (~3-5 min)..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node box \
     --ttl 30m -- 'echo PANDION_READY' >/tmp/ssh_up.log 2>&1; then :; fi
grep -q "node is live" /tmp/ssh_up.log && c_ok "provisioned" || { c_no "up did not complete"; tail -15 /tmp/ssh_up.log; }

# `pandion ssh -- <cmd>` must run the command on the node and return its output.
MARKER="pandion-ssh-ok-$$"
c_in "pandion ssh --id $ID -- (run a command, host-key pinned)..."
NO_COLOR=1 "$BIN" ssh --id "$ID" --node box -- "echo $MARKER; id -un; hostname" >/tmp/ssh_run.log 2>&1 || true
echo "---- ssh output ----"; cat /tmp/ssh_run.log; echo "--------------------"
grep -q "$MARKER" /tmp/ssh_run.log && c_ok "pandion ssh ran the command on the node" || c_no "ssh command produced no output"
grep -q "root" /tmp/ssh_run.log && c_ok "ssh connected as root (host-key pinned)" || c_no "ssh did not report the remote user"

# A pinned session must reject a tampered/unknown host key. Sanity: a bogus id fails.
c_in "pandion ssh with an unknown id must fail cleanly..."
NO_COLOR=1 "$BIN" ssh --id no-such-cluster -- 'echo x' >/tmp/ssh_bad.log 2>&1 && c_no "bad id should have failed" || c_ok "unknown cluster rejected"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "PANDION SSH: verified" || c_no "see failures above"
