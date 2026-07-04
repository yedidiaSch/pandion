#!/usr/bin/env bash
# ============================================================================
# Pandion docker-in-cluster e2e (P0-2): a cluster node with `engine: docker`
# runs its workload inside a hardened container. Using an ALPINE image (vs the
# Ubuntu host) proves it really ran in the container, not on the host.
# Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_docker_cluster.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-dockcluster"
BIN="./bin/pandion"
CLYAML="$(mktemp --suffix=.yaml)"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" >/dev/null 2>&1 || true
  rm -f "$CLYAML" /tmp/dc_*.log "$HOME/.pandion/lock/$ID.json"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
    hcloud firewall list -o noheader 2>/dev/null | grep -q "$ID" && c_no "cloud firewall leaked" || c_ok "teardown: cloud firewall gone"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

cat > "$CLYAML" <<EOF
apiVersion: pandion/v1
name: $ID
provider:
  name: hetzner
  region: fsn1
nodes:
  - name: app
    engine: docker
    container_image: alpine:3.20
    run: 'grep -i alpine /etc/os-release | head -1; test -f /.dockerenv && echo IN_DOCKER || echo ON_HOST'
EOF

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

c_in "provision a docker-engine cluster node (pull alpine, run in container) ~4-6 min..."
if timeout 700 env NO_COLOR=1 "$BIN" up --provider=hetzner -f "$CLYAML" --id "$ID" >/tmp/dc_up.log 2>&1; then :; fi
echo "---- run output ([app] lines) ----"; grep -E "\[app" /tmp/dc_up.log || tail -25 /tmp/dc_up.log
echo "-----------------------------------"

grep -qi "\[app\].*alpine" /tmp/dc_up.log \
  && c_ok "workload ran in the ALPINE container (not the Ubuntu host)" \
  || c_no "no alpine marker — workload may have run on the host"
grep -q "IN_DOCKER" /tmp/dc_up.log \
  && c_ok "/.dockerenv present — confirmed running inside a container" \
  || c_no "not running inside a container"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "DOCKER-IN-CLUSTER (P0-2): verified" || c_no "see failures above"
