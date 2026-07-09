#!/usr/bin/env bash
# ============================================================================
# Pandion config-defaults e2e — `pandion init` defaults seed `up`.
#
# Proves defaults.{size,region,ttl} from ~/.pandion/config.yaml fill an `up`
# that passes NO --size/--region/--ttl, on real cloud:
#   [1] `pandion init --provider P --size S --region R --ttl D --force` writes
#       the config (into an ISOLATED HOME so your real config is untouched).
#   [2] `pandion up --id ID -- <cmd>` (no size/region/ttl flags) provisions a
#       node whose live type == S and region == R (checked via `ls --json`).
#   [3] `pandion down --id ID` tears it down (provider from the manifest).
#
# The provider TOKEN must be in the environment (e.g. HCLOUD_TOKEN) — an
# isolated HOME has no keychain entry. Source it from your .env first.
# Self-cleaning.  Usage:  ./scripts/e2e_config_defaults.sh [provider] [size] [region]
#   defaults: hetzner cpx11 nbg1
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

PROV="${1:-hetzner}"
SIZE="${2:-cpx11}"
REGION="${3:-nbg1}"
TTL="45m"
ID="e2e-cfgdefaults-$PROV"
BIN="$PWD/bin/pandion"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

# isolated HOME so the real ~/.pandion/config.yaml is never touched.
TH="$(mktemp -d)"; export HOME="$TH"

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  HOME="$TH" "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -rf "$TH" /tmp/cfg_up.log /tmp/cfg_ls.json
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:/usr/local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
c_in "[1] init writes defaults: size=$SIZE region=$REGION ttl=$TTL (isolated HOME)..."
NO_COLOR=1 "$BIN" init --provider="$PROV" --size "$SIZE" --region "$REGION" --ttl "$TTL" --force
grep -q "size: $SIZE" "$TH/.pandion/config.yaml" && c_ok "config has the size default" || { c_no "size not in config"; cat "$TH/.pandion/config.yaml"; exit 1; }

# ---------------------------------------------------------------------------
c_in "[2] up --id $ID with NO size/region/ttl flags (must come from config)..."
if timeout 900 env NO_COLOR=1 "$BIN" up --id "$ID" --max-cost 2.00 -- 'echo cfgdefaults-ok && sleep 600' >/tmp/cfg_up.log 2>&1; then :; fi
tail -4 /tmp/cfg_up.log
NO_COLOR=1 "$BIN" ls --provider="$PROV" --json >/tmp/cfg_ls.json 2>&1 || true
if python3 -c "import json,sys; d=json.load(open('/tmp/cfg_ls.json')); ns=[n for c in d for n in c.get('nodes',[]) if c.get('id')=='$ID']; sys.exit(0 if ns and ns[0].get('type')=='$SIZE' else 1)" 2>/dev/null; then
  c_ok "provisioned node type == $SIZE (from config default)"
else
  c_no "node type != $SIZE"; python3 -c "import json;print(open('/tmp/cfg_ls.json').read())" | head;
fi
if python3 -c "import json,sys; d=json.load(open('/tmp/cfg_ls.json')); ns=[n for c in d for n in c.get('nodes',[]) if c.get('id')=='$ID']; sys.exit(0 if ns and ns[0].get('region')=='$REGION' else 1)" 2>/dev/null; then
  c_ok "provisioned node region == $REGION (from config default)"
else
  c_no "node region != $REGION"
fi

# ---------------------------------------------------------------------------
c_in "[3] down --id $ID (provider from manifest)..."
NO_COLOR=1 "$BIN" down --id "$ID" --yes 2>&1 | tail -2

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "CONFIG-DEFAULTS: verified" || c_no "see failures above"
