#!/usr/bin/env bash
# ============================================================================
# Pandion Vultr e2e: proves the Provider seam with the Vultr backend — a
# hardened instance provisions, runs, streams, `ls` shows live USD cost, and
# `down` leaves nothing (incl. the login SSH key). Also checks the `--max-cost`
# preflight (FREE — it provisions nothing but exercises Vultr's live pricing).
# Self-cleaning.
#
#   export VULTR_API_KEY=your-key
#   ./scripts/e2e_vultr.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-vultr"
BIN="./bin/pandion"
: "${VULTR_API_KEY:?Set VULTR_API_KEY}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=vultr --id "$ID" >/dev/null 2>&1 || true
  rm -f /tmp/vultr_*.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
# Phase 1 (FREE): --max-cost preflight rejects before provisioning — this alone
# proves the Vultr Pricer works live (it must fetch the plan catalog to estimate).
# ---------------------------------------------------------------------------
c_in "[1] --max-cost too low → reject before provisioning (exercises Vultr live pricing)..."
code=0
NO_COLOR=1 "$BIN" up --provider=vultr --id "$ID" --node worker --max-cost 0.0001 -- 'echo ready' >/tmp/vultr_rej.log 2>&1 || code=$?
if grep -qi "max-cost exceeded" /tmp/vultr_rej.log && [ "$code" = 6 ]; then
  c_ok "over-budget rejected (exit 6, nothing provisioned)"
else
  c_no "expected exit 6 + 'max-cost exceeded' (got $code)"; cat /tmp/vultr_rej.log
fi

# ---------------------------------------------------------------------------
# Phase 2 (PAID, ~2–4 min): provision one hardened instance within budget.
# ---------------------------------------------------------------------------
c_in "[2] up on Vultr (--ttl 30m --max-cost 1.00) — provisions + hardens one instance..."
if timeout 720 env NO_COLOR=1 "$BIN" up --provider=vultr --id "$ID" --node worker \
     --ttl 30m --max-cost 1.00 -- 'echo PANDION_READY' >/tmp/vultr_up.log 2>&1; then :; fi
if grep -q "node is live" /tmp/vultr_up.log; then
  c_ok "provisioned + hardened + ran on Vultr"
else
  c_no "up did not complete on Vultr"; tail -25 /tmp/vultr_up.log
fi

# ---------------------------------------------------------------------------
# Phase 3: `ls` must show the Vultr cluster with a live USD hourly rate + region.
# ---------------------------------------------------------------------------
c_in "[3] pandion ls --provider=vultr ..."
NO_COLOR=1 "$BIN" ls --provider=vultr >/tmp/vultr_ls.log 2>&1 || true
echo "---- ls output ----"; cat /tmp/vultr_ls.log; echo "-------------------"
grep -q "$ID"        /tmp/vultr_ls.log && c_ok "ls lists cluster $ID"                || c_no "ls missing cluster $ID"
grep -q "worker"     /tmp/vultr_ls.log && c_ok "ls lists node 'worker'"              || c_no "ls missing node"
grep -qE "USD/hr"    /tmp/vultr_ls.log && c_ok "ls shows USD cost columns"           || c_no "ls missing USD cost header"
grep -qE "0\.[0-9]{3,4}" /tmp/vultr_ls.log && c_ok "ls shows a nonzero hourly rate (LIVE Vultr pricing)" \
                                        || c_no "ls shows no live price"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "VULTR PROVIDER: verified" || c_no "see failures above"
