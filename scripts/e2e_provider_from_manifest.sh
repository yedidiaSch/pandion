#!/usr/bin/env bash
# ============================================================================
# Pandion provider-from-manifest e2e.
#
# Proves teardown needs no --provider on real cloud:
#   [1] `up --provider P` provisions a node AND records the owning provider in
#       the cluster manifest (manifest.provider == P).
#   [2] `pandion down --id ID` (NO --provider) reads the provider from that
#       manifest and reconciles the cluster to empty.
#   [3] a control `ls --provider P` afterwards shows the cluster is gone — no
#       node left billing.
# Self-cleaning.  Usage:  ./scripts/e2e_provider_from_manifest.sh [provider]  (default hetzner)
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

PROV="${1:-hetzner}"
ID="e2e-provmanifest-$PROV"
BIN="./bin/pandion"
KD="$HOME/.pandion/keys/$ID"
MAN="$KD/manifest.json"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  # explicit --provider here so cleanup works even if the feature under test regressed
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/pm_up.log /tmp/pm_down.log /tmp/pm_ls.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
c_in "[1] up --provider=$PROV, then check manifest.provider..."
if timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID" \
     --ttl 30m --max-cost 2.00 -- 'echo provmanifest-ok && sleep 600' >/tmp/pm_up.log 2>&1; then :; fi
[ -f "$MAN" ] || { c_no "no manifest written"; tail -25 /tmp/pm_up.log; exit 1; }
if python3 -c "import json,sys; sys.exit(0 if json.load(open('$MAN')).get('provider')=='$PROV' else 1)"; then
  c_ok "manifest records provider=$PROV"
else
  c_no "manifest.provider != $PROV"; python3 -c "import json;print(json.load(open('$MAN')).get('provider'))"; exit 1
fi

# ---------------------------------------------------------------------------
c_in "[2] down --id $ID  (NO --provider — must come from the manifest)..."
if NO_COLOR=1 "$BIN" down --id "$ID" --yes >/tmp/pm_down.log 2>&1; then
  cat /tmp/pm_down.log
  # the teardown banner names the provider it resolved from the manifest
  grep -qiE "DOWN \(${PROV}\)|reconciled to empty" /tmp/pm_down.log \
    && c_ok "down resolved $PROV from the manifest and tore the cluster down" \
    || c_no "down ran but did not report a $PROV teardown"
else
  c_no "down --id (no --provider) failed"; cat /tmp/pm_down.log; exit 1
fi

# ---------------------------------------------------------------------------
c_in "[3] control: ls --provider=$PROV shows the cluster is gone..."
NO_COLOR=1 "$BIN" ls --provider="$PROV" >/tmp/pm_ls.log 2>&1 || true
if grep -q "$ID" /tmp/pm_ls.log; then
  c_no "cluster $ID still listed — node may still be billing"; cat /tmp/pm_ls.log
else
  c_ok "cluster $ID absent from ls — nothing left billing"
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "PROVIDER-FROM-MANIFEST: verified" || c_no "see failures above"
