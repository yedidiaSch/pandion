#!/usr/bin/env bash
# ============================================================================
# Pandion ls/cost e2e (M4): the `--max-cost` budget preflight and `pandion ls`
# live cost, against real Hetzner. Self-cleaning.
#
# Phases 1–2 provision NOTHING (they fail the preflight) so they are FREE, yet
# they still exercise the live pricing API. Phase 3 provisions ONE cheap node to
# prove `ls` shows it with a real hourly rate, then tears it down.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_ls_cost.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-lscost"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" >/dev/null 2>&1 || true
  rm -f /tmp/lc_*.log
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

# run `up` expecting the --max-cost preflight to REJECT it (exit 6, no server).
# usage: expect_reject <log> <needle> <human-label> -- <up args...>
expect_reject(){
  local log=$1 needle=$2 label=$3; shift 3; [ "$1" = "--" ] && shift
  local code=0
  NO_COLOR=1 "$BIN" "$@" >"$log" 2>&1 || code=$?
  if grep -qi "$needle" "$log" && [ "$code" = 6 ]; then
    c_ok "$label (exit 6, nothing provisioned)"
  else
    c_no "$label — expected exit 6 + '$needle', got exit $code"; echo "---"; cat "$log"; echo "---"
  fi
  # a rejected up must not have created a server
  if command -v hcloud >/dev/null 2>&1; then
    local n; n=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${n:-0}" = 0 ] || c_no "$label leaked $n server(s)"
  fi
}

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
# Phase 0 (FREE): --dry-run previews plan + cost from LIVE pricing, creates nothing.
# ---------------------------------------------------------------------------
c_in "[0] --dry-run previews cost without provisioning (exercises live pricing)..."
NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node worker --ttl 30m --dry-run >/tmp/lc_dry.log 2>&1 || true
echo "---- dry-run ----"; cat /tmp/lc_dry.log; echo "-----------------"
grep -qi "DRY RUN" /tmp/lc_dry.log && grep -qi "nothing will be created" /tmp/lc_dry.log \
  && c_ok "dry-run previewed the plan" || c_no "dry-run banner missing"
grep -qE "0\.[0-9]{3,4}" /tmp/lc_dry.log && c_ok "dry-run shows a live hourly rate" || c_no "dry-run shows no live price"
if command -v hcloud >/dev/null 2>&1; then
  n=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
  [ "${n:-0}" = 0 ] && c_ok "dry-run created no server" || c_no "dry-run leaked $n server(s)"
fi

# ---------------------------------------------------------------------------
# Phase 1 (FREE): a tiny --max-cost must abort BEFORE provisioning.
# ---------------------------------------------------------------------------
c_in "[1] --max-cost too low → reject before provisioning (exercises live pricing)..."
expect_reject /tmp/lc_rej.log "max-cost exceeded" "over-budget rejected" -- \
  up --provider=hetzner --id "$ID" --node worker --max-cost 0.0001 -- 'echo ready'

# ---------------------------------------------------------------------------
# Phase 2 (FREE): --no-ttl under a cap is an unbounded projection → reject.
# ---------------------------------------------------------------------------
c_in "[2] --no-ttl with --max-cost → rejected as unbounded..."
expect_reject /tmp/lc_unb.log "unbounded" "no-TTL under a cap rejected" -- \
  up --provider=hetzner --id "$ID" --node worker --no-ttl --max-cost 1 -- 'echo ready'

# ---------------------------------------------------------------------------
# Phase 3 (PAID, ~3–5 min): provision ONE node within budget, then `ls`.
# ---------------------------------------------------------------------------
c_in "[3] up within budget (--max-cost 1.00 --ttl 30m) — provisions one node..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" --node worker \
     --ttl 30m --max-cost 1.00 -- 'echo PANDION_READY' >/tmp/lc_up.log 2>&1; then :; fi
if grep -q "node is live" /tmp/lc_up.log; then
  c_ok "provisioned within budget (preflight passed, node up)"
else
  c_no "up did not complete"; tail -20 /tmp/lc_up.log
fi

c_in "[4] pandion ls — must show the cluster, node, and a live hourly rate..."
NO_COLOR=1 "$BIN" ls --provider=hetzner >/tmp/lc_ls.log 2>&1 || true
echo "---- ls output ----"; cat /tmp/lc_ls.log; echo "-------------------"
grep -q "$ID"     /tmp/lc_ls.log && c_ok "ls lists cluster $ID"                 || c_no "ls missing cluster $ID"
grep -q "worker"  /tmp/lc_ls.log && c_ok "ls lists node 'worker'"               || c_no "ls missing node"
grep -qE "[A-Z]{3}/hr" /tmp/lc_ls.log && c_ok "ls shows the cost columns"        || c_no "ls missing cost header"
grep -qE "0\.[0-9]{3,4}" /tmp/lc_ls.log && c_ok "ls shows a nonzero hourly rate (LIVE pricing works)" \
                                        || c_no "ls shows no live price"
# region must be populated (not "—"): regression guard for the Location field
# (Hetzner dropped the deprecated datacenter object from API responses 2026-07-01).
region=$(awk '/pandion-'"$ID"'-worker/{print $4; exit}' /tmp/lc_ls.log)
if [ -n "$region" ] && [ "$region" != "—" ] && [ "$region" != "-" ]; then
  c_ok "ls shows region '$region' (Location field populated)"
else
  c_no "ls region empty/— (Location not mapped): '$region'"
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "LS + BUDGET CAP (M4): verified" || c_no "see failures above"
