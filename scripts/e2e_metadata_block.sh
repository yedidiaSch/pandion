#!/usr/bin/env bash
# ============================================================================
# Pandion metadata-block e2e (S-F): the cloud metadata endpoint (169.254.169.254)
# is UNCONDITIONALLY unreachable from a workload — even when the operator
# EXPLICITLY allowlists it with --egress-allow. That proves the block is
# defense-in-depth, not just a side effect of default-deny egress. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_metadata_block.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-metablock"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/mb_*.log
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# Probe (runs as the unprivileged run user): try a TCP connect to the metadata
# service on :80 using bash /dev/tcp (no curl dependency). A dropped packet hangs
# until the timeout -> META_BLOCKED; a reachable service connects -> META_REACHED.
PROBE='if timeout 4 bash -c ":</dev/tcp/169.254.169.254/80" 2>/dev/null; then echo META_REACHED; else echo META_BLOCKED; fi'

# The kicker: allowlist the metadata IP itself. Under plain default-deny this
# would be REACHABLE; the unconditional S-F drop must still block it.
c_in "provision one node with --egress-allow 169.254.169.254/32, probe metadata (~3-5 min)..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node probe \
     --ttl 30m --egress-allow 169.254.169.254/32 -- "$PROBE" >/tmp/mb_up.log 2>&1; then :; fi
echo "---- probe output ----"; grep -E "META_|node is live" /tmp/mb_up.log || tail -20 /tmp/mb_up.log
echo "----------------------"

if grep -q "META_REACHED" /tmp/mb_up.log; then
  c_no "metadata was REACHABLE — the S-F block FAILED (creds exposed!)"
elif grep -q "META_BLOCKED" /tmp/mb_up.log; then
  c_ok "metadata BLOCKED despite an explicit allowlist entry (S-F defense-in-depth)"
else
  c_no "probe produced no result"; tail -20 /tmp/mb_up.log
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "METADATA BLOCK (S-F): verified" || c_no "see failures above"
