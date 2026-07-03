#!/usr/bin/env bash
# ============================================================================
# Pandion node-hardening e2e (P1): a provisioned node comes up with fail2ban
# active (SSH brute-force protection) and runs workloads as the unprivileged
# run user (S-C). Both are checked by a probe workload. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_node_hardening.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-harden"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" >/dev/null 2>&1 || true
  rm -f /tmp/hd_*.log "$HOME/.pandion/lock/$ID.json"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# Probe (runs as the workload user): report fail2ban state + our uid name.
PROBE='echo "F2B=$(systemctl is-active fail2ban 2>/dev/null)"; echo "WHOAMI=$(id -un)"'

c_in "provision one node, then probe hardening (~3-5 min)..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node probe \
     --ttl 30m -- "$PROBE" >/tmp/hd_up.log 2>&1; then :; fi
echo "---- probe output ----"; grep -E "F2B=|WHOAMI=|node is live" /tmp/hd_up.log || tail -20 /tmp/hd_up.log
echo "----------------------"

grep -q "F2B=active" /tmp/hd_up.log \
  && c_ok "fail2ban is ACTIVE on the node (SSH brute-force protection)" \
  || c_no "fail2ban not active"
grep -q "WHOAMI=pandion-run" /tmp/hd_up.log \
  && c_ok "workload runs as the unprivileged 'pandion-run' user (S-C)" \
  || c_no "workload not running as the least-privilege user"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "NODE HARDENING (fail2ban + least-priv): verified" || c_no "see failures above"
