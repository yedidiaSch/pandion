#!/usr/bin/env bash
# ============================================================================
# Pandion idempotency e2e — the `up` existence guard (F1/R1) on a live node.
#
# `up` must refuse to provision onto an id that already names a live cluster,
# instead of duplicating/orphaning servers or overwriting the first cluster's
# keys+manifest. Only meaningful against a real provider (the mock creates no
# servers and its ListByTag is in-memory per process):
#   [1] up --id X            -> provisions one node, left up
#   [2] up --id X (again)    -> REFUSED, exit 2, names the existing cluster
#   [3] still exactly ONE server for X (the original was untouched)
#   [4] down --id X          -> reconciles to empty
#
# Provider TOKEN must be in the environment (e.g. HCLOUD_TOKEN) — source .env.
# Self-cleaning.  Usage:  ./scripts/e2e_idempotency.sh [provider] [size] [region]
#   defaults: hetzner cpx11 fsn1
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."

PROV="${1:-hetzner}"
SIZE="${2:-cpx11}"
REGION="${3:-auto}"   # "auto" = omit --region and let the provider place the node
TTL="30m"
region_args=(); [ "$REGION" != "auto" ] && region_args=(--region "$REGION")
ID="e2e-idem-$PROV"
BIN="$PWD/bin/pandion"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

c_in "building..."; export PATH="${PATH}:/usr/local/go/bin"
go build -o "$BIN" ./cmd/pandion || { c_no "build failed"; exit 1; }; c_ok "built"

TH="$(mktemp -d)"; export HOME="$TH"
teardown(){
  local code=$?; echo; c_in "cleaning up..."
  HOME="$TH" "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -rf "$TH" /tmp/idem_up1.log /tmp/idem_up2.log /tmp/idem_ls.json
  c_in "done (exit $code)"
}
trap teardown EXIT INT TERM

count_servers(){ # nodes tagged for $ID, via ls --json
  NO_COLOR=1 "$BIN" ls --provider="$PROV" --json >/tmp/idem_ls.json 2>/dev/null || true
  python3 -c "import json,sys;d=json.load(open('/tmp/idem_ls.json'));print(sum(len(c.get('nodes',[])) for c in d.get('clusters',[]) if c.get('id')=='$ID'))" 2>/dev/null || echo 0
}

# ---------------------------------------------------------------------------
c_in "[1] first up --id $ID (provisions one node, left up)..."
timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --size "$SIZE" "${region_args[@]}" \
  --ttl "$TTL" --id "$ID" --max-cost 2.00 -- 'echo idem-first' >/tmp/idem_up1.log 2>&1
UP1=$?
tail -3 /tmp/idem_up1.log
[ "$UP1" = 0 ] && c_ok "first up succeeded" || { c_no "first up failed (rc $UP1)"; exit 1; }
n1=$(count_servers); c_in "server count after first up: $n1"

# ---------------------------------------------------------------------------
c_in "[2] second up --id $ID — must be REFUSED (F1/R1)..."
timeout 120 env NO_COLOR=1 "$BIN" up --provider="$PROV" --size "$SIZE" "${region_args[@]}" \
  --ttl "$TTL" --id "$ID" --max-cost 2.00 -- 'echo idem-second' >/tmp/idem_up2.log 2>&1
UP2=$?
tail -3 /tmp/idem_up2.log
if [ "$UP2" = 2 ]; then c_ok "second up exited 2 (refused)"; else c_no "second up exited $UP2, wanted 2"; fi
grep -qi "already exists" /tmp/idem_up2.log && c_ok "refusal names the existing cluster" || c_no "refusal message missing 'already exists'"

# ---------------------------------------------------------------------------
c_in "[3] the original cluster must be untouched — still exactly one server..."
n2=$(count_servers); c_in "server count after refused up: $n2"
if [ "$n1" = 1 ] && [ "$n2" = 1 ]; then c_ok "still exactly one server (no duplicate/orphan)"; else c_no "server count changed: $n1 -> $n2"; fi

# ---------------------------------------------------------------------------
c_in "[4] down --id $ID..."
NO_COLOR=1 "$BIN" down --id "$ID" --yes 2>&1 | tail -2

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "IDEMPOTENCY E2E: verified" || { c_no "see failures above"; exit 1; }
