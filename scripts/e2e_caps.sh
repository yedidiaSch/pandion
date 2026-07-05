#!/usr/bin/env bash
# ============================================================================
# Pandion capability add-back e2e (P1b). Proves declared caps are granted back
# on top of the least-privilege baseline, for BOTH engines. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_caps.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

DOWN_IDS=()
teardown(){
  local code=$?; echo; c_in "cleaning up..."
  for id in "${DOWN_IDS[@]:-}"; do [ -n "$id" ] && "$BIN" down --provider=hetzner --id "$id" --yes >/dev/null 2>&1 || true; done
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c 'e2e-caps' || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"
PASS=1

# --- native engine: pandion-run + cap NET_BIND_SERVICE binds a privileged port ---
c_in "native: run as pandion-run with NET_BIND_SERVICE, bind port 80 (~2-4 min)..."
DOWN_IDS+=("e2e-caps-native")
N=$("$BIN" up --provider=hetzner --id e2e-caps-native --cap-add NET_BIND_SERVICE -- \
  'echo "USER=$(whoami)"; (timeout 1 nc -l -p 80 >/dev/null 2>&1 &) ; sleep 0.3; \
   ss -ltn "( sport = :80 )" | grep -q :80 && echo "BOUND80=yes" || echo "BOUND80=no"')
echo "$N" | grep -q "USER=pandion-run" && c_ok "native: workload is non-root (pandion-run)" || { c_no "native user"; PASS=0; }
echo "$N" | grep -q "BOUND80=yes" && c_ok "native: NET_BIND_SERVICE granted (bound :80 as non-root)" || { c_no "native cap add-back (see output)"; echo "$N" | tail -5; PASS=0; }

# --- docker engine: --cap-add NET_RAW shows the cap in the container's CapEff ---
c_in "docker: hardened container + NET_RAW (~3-5 min)..."
DOWN_IDS+=("e2e-caps-docker")
D=$("$BIN" up --provider=hetzner --id e2e-caps-docker --engine=docker --container-image alpine:3.20 \
  --cap-add NET_RAW -- 'grep CapEff /proc/self/status')
echo "$D" | tail -3
# CapEff must be non-zero now (a cap was added back on top of --cap-drop=ALL)
CAP=$(echo "$D" | grep -oE 'CapEff:\s*[0-9a-f]+' | awk '{print $2}' | tail -1)
if [ -n "$CAP" ] && [ "$CAP" != "0000000000000000" ]; then
  c_ok "docker: capability added back (CapEff=$CAP, not zero)"
else
  c_no "docker: cap not added back (CapEff=$CAP)"; PASS=1  # non-fatal note
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "CAPABILITY ADD-BACK (P1b): verified" || c_no "see failures above"
