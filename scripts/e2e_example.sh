#!/usr/bin/env bash
# ============================================================================
# Pandion example e2e — runs the shipped examples/zmq-cluster demo end to end so
# the onboarding example can never bit-rot: provision broker + 2 workers, install
# libzmq, sync + build the C++ on each node, run the ZeroMQ ventilator/worker
# pipeline over the overlay, and assert the broker completed with BOTH workers
# sharing the load. Self-cleaning.
# Usage:  ./scripts/e2e_example.sh [provider]   (default digitalocean)
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

PROV="${1:-digitalocean}"
ID="e2e-example-$PROV"
BIN="$(pwd)/bin/pandion"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  c_in "done (exit $code) — full streamed run kept at /tmp/example_up.log"
}
trap teardown EXIT

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# validate the shipped example (free)
( cd examples/zmq-cluster && NO_COLOR=1 "$BIN" validate -f cluster.yaml ) \
  && c_ok "example cluster.yaml validates" || { c_no "example cluster.yaml invalid"; exit 1; }

# run it exactly as a user would: from the example dir, streaming to completion
# (workers self-exit a few seconds after the broker finishes, so `up` returns).
c_in "[1] up broker + 2 workers on $PROV (installs libzmq, builds, runs)..."
( cd examples/zmq-cluster && timeout 1200 env NO_COLOR=1 "$BIN" up --provider="$PROV" \
    -f cluster.yaml --id "$ID" --ttl 30m --max-cost 3.00 ) >/tmp/example_up.log 2>&1

c_in "----- streamed output (tail) -----"; tail -20 /tmp/example_up.log

# assertions on the streamed run
grep -q "mesh verif" /tmp/example_up.log && c_ok "cluster came up + mesh verified" || c_no "cluster/mesh did not form"
grep -q "all 30 tasks complete" /tmp/example_up.log && c_ok "broker completed all tasks" || c_no "broker did not finish"
grep -q "worker-1 handled" /tmp/example_up.log && c_ok "worker-1 processed tasks" || c_no "worker-1 did no work"
grep -q "worker-2 handled" /tmp/example_up.log && c_ok "worker-2 processed tasks" || c_no "worker-2 did no work"
# both workers sharing load = service discovery + overlay + build all worked
if grep -q "worker-1 handled" /tmp/example_up.log && grep -q "worker-2 handled" /tmp/example_up.log; then
  c_ok "load split across BOTH workers (discovery + overlay + build verified end-to-end)"
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "EXAMPLE (zmq-cluster): verified" || c_no "see failures above"
