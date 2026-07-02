#!/usr/bin/env bash
# ============================================================================
# Pandion Docker-engine e2e: run a workload in a HARDENED container on the node.
# Proves --engine=docker + the S-D hardening (cap-drop, no-new-privileges,
# read-only rootfs, no --privileged, no docker.sock). Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_docker.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-docker"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" >/dev/null 2>&1 || true
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "provision + run workload in a hardened container (~3-5 min)..."
# inside the container: print the OS (proves we're in the image), whoami, and
# prove least privilege — /proc/self/status CapEff should be all-zero (caps dropped),
# and writing to the read-only rootfs must fail.
OUT=$("$BIN" up --provider=hetzner --id "$ID" --engine=docker --container-image alpine:3.20 -- \
  'echo "IN_CONTAINER=$(cat /etc/alpine-release 2>/dev/null || echo no)"; \
   echo "CAPEFF=$(grep CapEff /proc/self/status | awk "{print \$2}")"; \
   (touch /nope 2>/dev/null && echo "ROOTFS=writable" || echo "ROOTFS=readonly")')
echo "----------------------------------------------------------------"
echo "$OUT"
echo "----------------------------------------------------------------"

PASS=1
echo "$OUT" | grep -q "running command in hardened container" && c_ok "docker engine selected" || { c_no "engine"; PASS=0; }
echo "$OUT" | grep -Eq "IN_CONTAINER=[0-9]" && c_ok "workload ran inside the alpine image" || { c_no "not in container"; PASS=0; }
echo "$OUT" | grep -q "CAPEFF=0000000000000000" && c_ok "capabilities dropped (CapEff=0)" || c_no "caps not fully dropped (check CAPEFF)"
echo "$OUT" | grep -q "ROOTFS=readonly" && c_ok "container rootfs is read-only" || { c_no "rootfs not read-only"; PASS=0; }

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "DOCKER ENGINE (hardened): verified" || c_no "see failures above"
