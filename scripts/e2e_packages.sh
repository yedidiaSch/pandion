#!/usr/bin/env bash
# ============================================================================
# Pandion package-install e2e — proves the "install libraries on a node" flow:
#   [1] --packages installs the requested libraries in the build window;
#   [2] they are ADDED to the built-in toolchain (gcc/cmake still present) — the
#       old replace-footgun is gone;
#   [3] a bogus/typo'd package name is reported by a LOUD warning at `up` time
#       (a silent cloud-init failure is turned into an actionable signal).
# Uses --no-run (deploy only) so no workload is needed. Self-cleaning.
# Usage:  ./scripts/e2e_packages.sh [provider]   (default digitalocean)
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

PROV="${1:-digitalocean}"
ID="e2e-packages-$PROV"
NODE="node-a"
BIN="./bin/pandion"
KD="$HOME/.pandion/keys/$ID"
MAN="$KD/manifest.json"
PASS=1
BOGUS="definitely-not-a-real-pkg-xyz"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/pkg_up.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"
inst(){ $SSH "root@$1" "dpkg-query -W -f='\${db:Status-Status}' $2 2>/dev/null || echo missing"; }

# ---------------------------------------------------------------------------
c_in "[1] up --packages libzmq3-dev,libcurl4-openssl-dev + a bogus pkg, --no-run..."
if timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID" --node "$NODE" \
     --packages "libzmq3-dev,libcurl4-openssl-dev,$BOGUS" --no-run \
     --ttl 20m --max-cost 1.00 >/tmp/pkg_up.log 2>&1; then :; fi
if grep -qiE "node deployed|nothing started" /tmp/pkg_up.log; then
  c_ok "node deployed"
else
  c_no "up did not complete"; tail -25 /tmp/pkg_up.log; exit 1
fi
[ -f "$MAN" ] || { c_no "no manifest"; exit 1; }
IP=$(python3 -c "import json;print(json.load(open('$MAN'))['nodes'][0]['ip'])")
c_in "node ip=$IP"

# ---------------------------------------------------------------------------
c_in "[2] requested libraries are installed..."
[ "$(inst "$IP" libzmq3-dev)" = installed ] && c_ok "libzmq3-dev installed" || c_no "libzmq3-dev NOT installed"
[ "$(inst "$IP" libcurl4-openssl-dev)" = installed ] && c_ok "libcurl4-openssl-dev installed" || c_no "libcurl4-openssl-dev NOT installed"

# ---------------------------------------------------------------------------
c_in "[3] the built-in toolchain is STILL there (additive, no footgun)..."
[ "$(inst "$IP" g++)" = installed ] && c_ok "g++ (build-essential) still installed" || c_no "g++ dropped — footgun!"
[ "$(inst "$IP" cmake)" = installed ] && c_ok "cmake still installed" || c_no "cmake dropped — footgun!"
[ "$(inst "$IP" gdb)" = installed ] && c_ok "gdb still installed" || c_no "gdb dropped"

# ---------------------------------------------------------------------------
c_in "[4] the bogus package triggered a loud warning at up time..."
if grep -qi "did NOT install" /tmp/pkg_up.log && grep -q "$BOGUS" /tmp/pkg_up.log; then
  c_ok "missing-package warning named the bogus package"
else
  c_no "no warning for the bogus package"; grep -i "install" /tmp/pkg_up.log | head
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "PACKAGE INSTALL: verified" || c_no "see failures above"
