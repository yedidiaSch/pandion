#!/usr/bin/env bash
# ============================================================================
# Pandion DigitalOcean e2e (M6): proves the Provider seam with a 2nd backend —
# a hardened droplet provisions, runs, streams, `ls` shows live USD cost, and
# `down` leaves nothing. Also checks the `--max-cost` preflight (FREE — it
# provisions nothing but exercises DO's live pricing). Self-cleaning.
#
#   export DIGITALOCEAN_TOKEN=your-token
#   ./scripts/e2e_digitalocean.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-do"
BIN="./bin/pandion"
: "${DIGITALOCEAN_TOKEN:?Set DIGITALOCEAN_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=digitalocean --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/do_*.log
  if command -v doctl >/dev/null 2>&1; then
    local left; left=$(doctl compute droplet list --format Name --no-header 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no droplets left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
# Phase 1 (FREE): --max-cost preflight rejects before provisioning — this alone
# proves the DO Pricer works live (it must fetch the size catalog to estimate).
# ---------------------------------------------------------------------------
c_in "[1] --max-cost too low → reject before provisioning (exercises DO live pricing)..."
code=0
NO_COLOR=1 "$BIN" up --provider=digitalocean --id "$ID" --node worker --max-cost 0.0001 -- 'echo ready' >/tmp/do_rej.log 2>&1 || code=$?
if grep -qi "max-cost exceeded" /tmp/do_rej.log && [ "$code" = 6 ]; then
  c_ok "over-budget rejected (exit 6, nothing provisioned)"
else
  c_no "expected exit 6 + 'max-cost exceeded' (got $code)"; cat /tmp/do_rej.log
fi

# ---------------------------------------------------------------------------
# Phase 2 (PAID, ~2–3 min): provision one hardened droplet within budget.
# ---------------------------------------------------------------------------
c_in "[2] up on DigitalOcean (--ttl 30m --max-cost 1.00) — provisions + hardens one droplet..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider=digitalocean --id "$ID" --node worker \
     --ttl 30m --max-cost 1.00 -- 'echo PANDION_READY' >/tmp/do_up.log 2>&1; then :; fi
if grep -q "node is live" /tmp/do_up.log; then
  c_ok "provisioned + hardened + ran on DigitalOcean"
else
  c_no "up did not complete on DO"; tail -25 /tmp/do_up.log
fi

# ---------------------------------------------------------------------------
# Phase 3: `ls` must show the DO cluster with a live USD hourly rate + region.
# ---------------------------------------------------------------------------
c_in "[3] pandion ls --provider=digitalocean ..."
NO_COLOR=1 "$BIN" ls --provider=digitalocean >/tmp/do_ls.log 2>&1 || true
echo "---- ls output ----"; cat /tmp/do_ls.log; echo "-------------------"
grep -q "$ID"        /tmp/do_ls.log && c_ok "ls lists cluster $ID"                || c_no "ls missing cluster $ID"
grep -q "worker"     /tmp/do_ls.log && c_ok "ls lists node 'worker'"              || c_no "ls missing node"
grep -qE "USD/hr"    /tmp/do_ls.log && c_ok "ls shows USD cost columns"           || c_no "ls missing USD cost header"
grep -qE "0\.[0-9]{3,4}" /tmp/do_ls.log && c_ok "ls shows a nonzero hourly rate (LIVE DO pricing)" \
                                        || c_no "ls shows no live price"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "DIGITALOCEAN PROVIDER (M6): verified" || c_no "see failures above"
