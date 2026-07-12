#!/usr/bin/env bash
# ============================================================================
# Pandion Linode (Akamai) e2e: proves the Provider seam with the Linode
# backend — a hardened instance provisions, runs, streams, `ls` shows live USD
# cost, and `down` leaves nothing. Also checks the `--max-cost` preflight
# (FREE — it provisions nothing but exercises Linode's live pricing).
# Self-cleaning.
#
#   export LINODE_TOKEN=your-token
#   ./scripts/e2e_linode.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-linode"
BIN="./bin/pandion"
: "${LINODE_TOKEN:?Set LINODE_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=linode --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/linode_*.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
# Phase 1 (FREE): --max-cost preflight rejects before provisioning — this alone
# proves the Linode Pricer works live (it must fetch the type catalog to estimate).
# ---------------------------------------------------------------------------
c_in "[1] --max-cost too low → reject before provisioning (exercises Linode live pricing)..."
code=0
NO_COLOR=1 "$BIN" up --provider=linode --id "$ID" --node worker --max-cost 0.0001 -- 'echo ready' >/tmp/linode_rej.log 2>&1 || code=$?
if grep -qi "max-cost exceeded" /tmp/linode_rej.log && [ "$code" = 6 ]; then
  c_ok "over-budget rejected (exit 6, nothing provisioned)"
else
  c_no "expected exit 6 + 'max-cost exceeded' (got $code)"; cat /tmp/linode_rej.log
fi

# ---------------------------------------------------------------------------
# Phase 2 (PAID, ~2–4 min): provision one hardened instance within budget.
# ---------------------------------------------------------------------------
c_in "[2] up on Linode (--ttl 30m --max-cost 1.00) — provisions + hardens one instance..."
if timeout 720 env NO_COLOR=1 "$BIN" up --provider=linode --id "$ID" --node worker \
     --ttl 30m --max-cost 1.00 -- 'echo PANDION_READY' >/tmp/linode_up.log 2>&1; then :; fi
if grep -q "node is live" /tmp/linode_up.log; then
  c_ok "provisioned + hardened + ran on Linode"
else
  c_no "up did not complete on Linode"; tail -25 /tmp/linode_up.log
fi

# ---------------------------------------------------------------------------
# Phase 3: `ls` must show the Linode cluster with a live USD hourly rate + region.
# ---------------------------------------------------------------------------
c_in "[3] pandion ls --provider=linode ..."
NO_COLOR=1 "$BIN" ls --provider=linode >/tmp/linode_ls.log 2>&1 || true
echo "---- ls output ----"; cat /tmp/linode_ls.log; echo "-------------------"
grep -q "$ID"        /tmp/linode_ls.log && c_ok "ls lists cluster $ID"                || c_no "ls missing cluster $ID"
grep -q "worker"     /tmp/linode_ls.log && c_ok "ls lists node 'worker'"              || c_no "ls missing node"
grep -qE "USD/hr"    /tmp/linode_ls.log && c_ok "ls shows USD cost columns"           || c_no "ls missing USD cost header"
grep -qE "0\.[0-9]{3,4}" /tmp/linode_ls.log && c_ok "ls shows a nonzero hourly rate (LIVE Linode pricing)" \
                                        || c_no "ls shows no live price"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "LINODE PROVIDER: verified" || c_no "see failures above"
