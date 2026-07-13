#!/usr/bin/env bash
# ============================================================================
# Pandion UX e2e — the P0/P1 acceptance criteria that need a REAL node.
#
# Most of the UX upgrade plan (docs/ux-upgrade-plan.md) is proved offline by
# scripts/ci_smoke.sh. Three items only mean something against live cloud,
# because the mock provider runs no SSH and creates no resources:
#   [1] P0.1  a workload `run:` command's exit code propagates through `up`
#             (up -- 'exit 7' must make the process exit 7, node left up).
#   [2] P1.5  `ls --json` / `down --json` emit the documented envelopes for a
#             live cluster.
#   [3] P0.2  after `down`, reconnect commands (attach/ssh/start) fast-fail
#             with the "was torn down" message instead of dialing a dead IP.
#
# The provider TOKEN must be in the environment (e.g. HCLOUD_TOKEN) — source
# your .env first. Self-cleaning (tears the node down on any exit).
#
# Usage:  ./scripts/e2e_ux.sh [provider] [size] [region]
#   defaults: hetzner cpx11 nbg1
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."

PROV="${1:-hetzner}"
SIZE="${2:-cpx11}"
REGION="${3:-nbg1}"
TTL="30m"
ID="e2e-ux-$PROV"
BIN="$PWD/bin/pandion"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

# Build with the AMBIENT home so the module cache is reused (an isolated HOME
# would re-download every module read-only and then fight `rm` at cleanup).
c_in "building..."; export PATH="${PATH}:/usr/local/go/bin"
go build -o "$BIN" ./cmd/pandion || { c_no "build failed"; exit 1; }; c_ok "built"

# isolated HOME so the real ~/.pandion is never touched (set AFTER the build).
TH="$(mktemp -d)"; export HOME="$TH"

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  HOME="$TH" "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -rf "$TH" /tmp/ux_up.log /tmp/ux_ls.json /tmp/ux_down.json /tmp/ux_attach.log
  c_in "done (exit $code)"
}
trap teardown EXIT INT TERM

# ---------------------------------------------------------------------------
# [1] P0.1 — workload exit code propagates; the node is left up afterward.
WANT=7
c_in "[1] up --id $ID -- 'exit $WANT' — expect the process to exit $WANT (P0.1)..."
timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --size "$SIZE" --region "$REGION" \
  --ttl "$TTL" --id "$ID" --max-cost 2.00 -- "echo ux-e2e-ran && exit $WANT" >/tmp/ux_up.log 2>&1
GOT=$?
tail -5 /tmp/ux_up.log
if [ "$GOT" = "$WANT" ]; then c_ok "up propagated the workload exit code ($GOT)"; else c_no "up exited $GOT, wanted $WANT"; fi
grep -q "ux-e2e-ran" /tmp/ux_up.log && c_ok "workload actually ran on the node" || c_no "workload output missing"

# ---------------------------------------------------------------------------
# [2] P1.5 — ls --json envelope for the (still-up) cluster.
c_in "[2] ls --json shows the live node (P1.5)..."
NO_COLOR=1 "$BIN" ls --provider="$PROV" --json >/tmp/ux_ls.json 2>&1 || true
# ls --json envelope: {"provider":...,"clusters":[{"id":...,"nodes":[...]}]}
if python3 -c "import json,sys; d=json.load(open('/tmp/ux_ls.json')); ns=[n for c in d.get('clusters',[]) for n in c.get('nodes',[]) if c.get('id')=='$ID']; sys.exit(0 if ns else 1)" 2>/dev/null; then
  c_ok "ls --json parses and lists the cluster"
else
  c_no "ls --json missing the cluster"; head -c 400 /tmp/ux_ls.json; echo
fi

# ---------------------------------------------------------------------------
# [3] P1.5 + P0.2 — down --json receipt, then reconnect commands fast-fail.
c_in "[3] down --id $ID --json — expect a machine receipt (P1.5)..."
NO_COLOR=1 "$BIN" down --id "$ID" --json --yes >/tmp/ux_down.json 2>&1
DOWN_RC=$?
if [ "$DOWN_RC" = 0 ] && python3 -c "import json; json.load(open('/tmp/ux_down.json'))" 2>/dev/null; then
  c_ok "down --json emitted valid JSON (rc 0)"
else
  c_no "down --json bad (rc $DOWN_RC)"; head -c 400 /tmp/ux_down.json; echo
fi

c_in "[3b] after down, attach must fast-fail 'torn down' — not hang on a dead IP (P0.2)..."
timeout 30 env NO_COLOR=1 "$BIN" attach --id "$ID" >/tmp/ux_attach.log 2>&1
ATT_RC=$?
tail -2 /tmp/ux_attach.log
if grep -qi "torn down" /tmp/ux_attach.log; then
  c_ok "attach fast-failed with the 'torn down' message"
else
  c_no "attach did not report 'torn down' (rc $ATT_RC)"
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "UX E2E: verified" || { c_no "see failures above"; exit 1; }
