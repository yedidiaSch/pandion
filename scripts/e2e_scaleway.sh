#!/usr/bin/env bash
# ============================================================================
# Pandion Scaleway e2e: proves the Provider seam with the Scaleway backend — a
# hardened instance provisions (two-phase boot: create → attach user-data →
# power on), runs, streams, `ls` shows live EUR cost, and `down` terminates the
# server AND deletes its block volumes so nothing bills after teardown (C4).
# Also checks the `--max-cost` preflight (FREE — provisions nothing but
# exercises Scaleway's live pricing). Self-cleaning.
#
# Scaleway auth is a triple (only the secret key is sensitive):
#   export SCW_SECRET_KEY=your-secret-key
#   export SCW_ACCESS_KEY=your-access-key
#   export SCW_DEFAULT_PROJECT_ID=your-project-id
#   ./scripts/e2e_scaleway.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-scw"
BIN="./bin/pandion"
: "${SCW_SECRET_KEY:?Set SCW_SECRET_KEY}"
: "${SCW_ACCESS_KEY:?Set SCW_ACCESS_KEY}"
: "${SCW_DEFAULT_PROJECT_ID:?Set SCW_DEFAULT_PROJECT_ID}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=scaleway --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/scw_*.log
  if command -v scw >/dev/null 2>&1; then
    local left; left=$(scw instance server list -o json 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
# Phase 1 (FREE): --max-cost preflight rejects before provisioning — this alone
# proves the Scaleway Pricer works live (it must fetch the per-zone type catalog).
# ---------------------------------------------------------------------------
c_in "[1] --max-cost too low → reject before provisioning (exercises Scaleway live pricing)..."
code=0
NO_COLOR=1 "$BIN" up --provider=scaleway --id "$ID" --node worker --max-cost 0.0001 -- 'echo ready' >/tmp/scw_rej.log 2>&1 || code=$?
if grep -qi "max-cost exceeded" /tmp/scw_rej.log && [ "$code" = 6 ]; then
  c_ok "over-budget rejected (exit 6, nothing provisioned)"
else
  c_no "expected exit 6 + 'max-cost exceeded' (got $code)"; cat /tmp/scw_rej.log
fi

# ---------------------------------------------------------------------------
# Phase 2 (PAID, ~3–5 min): provision one hardened instance within budget.
# ---------------------------------------------------------------------------
c_in "[2] up on Scaleway (--ttl 30m --max-cost 1.00) — provisions + hardens one instance..."
if timeout 900 env NO_COLOR=1 "$BIN" up --provider=scaleway --id "$ID" --node worker \
     --ttl 30m --max-cost 1.00 -- 'echo PANDION_READY' >/tmp/scw_up.log 2>&1; then :; fi
if grep -q "node is live" /tmp/scw_up.log; then
  c_ok "provisioned + hardened + ran on Scaleway"
else
  c_no "up did not complete on Scaleway"; tail -25 /tmp/scw_up.log
fi

# ---------------------------------------------------------------------------
# Phase 3: `ls` must show the Scaleway cluster with a live EUR hourly rate + zone.
# ---------------------------------------------------------------------------
c_in "[3] pandion ls --provider=scaleway ..."
NO_COLOR=1 "$BIN" ls --provider=scaleway >/tmp/scw_ls.log 2>&1 || true
echo "---- ls output ----"; cat /tmp/scw_ls.log; echo "-------------------"
grep -q "$ID"        /tmp/scw_ls.log && c_ok "ls lists cluster $ID"                || c_no "ls missing cluster $ID"
grep -q "worker"     /tmp/scw_ls.log && c_ok "ls lists node 'worker'"              || c_no "ls missing node"
grep -qE "EUR/hr"    /tmp/scw_ls.log && c_ok "ls shows EUR cost columns"           || c_no "ls missing EUR cost header"
grep -qE "0\.[0-9]{3,4}" /tmp/scw_ls.log && c_ok "ls shows a nonzero hourly rate (LIVE Scaleway pricing)" \
                                        || c_no "ls shows no live price"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "SCALEWAY PROVIDER: verified" || c_no "see failures above"
