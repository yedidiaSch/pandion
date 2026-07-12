#!/usr/bin/env bash
# ============================================================================
# Pandion Scaleway MULTI-NODE e2e — the regression proof for the IAM SSH-key fix.
#
# Before the fix, a multi-node Scaleway `up` failed with "login key not yet on
# root": the login key rode only the (large) cloud-init user-data, which did not
# reliably land on root at multi-node scale. The fix registers the login key as a
# project-scoped IAM SSH key, which Scaleway's metadata datasource injects into
# root early. This test provisions a 3-node cluster (the exact scenario that
# failed), asserts the mesh forms and every node is reachable as root (⇒ the key
# landed), and that teardown reaps the IAM key so nothing leaks (C4).
#
# Scaleway auth is a triple (only the secret key is sensitive):
#   export SCW_SECRET_KEY=... SCW_ACCESS_KEY=... SCW_DEFAULT_PROJECT_ID=...
#   ./scripts/e2e_scaleway_cluster.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

ID="e2e-scw-cluster"
BIN="./bin/pandion"
: "${SCW_SECRET_KEY:?Set SCW_SECRET_KEY}"
: "${SCW_ACCESS_KEY:?Set SCW_ACCESS_KEY}"
: "${SCW_DEFAULT_PROJECT_ID:?Set SCW_DEFAULT_PROJECT_ID}"
KEYNAME="pandion-login-$ID"          # must match loginKeyName + sanitize(ID)
YAML="/tmp/scw_cluster.yaml"
MAN="$HOME/.pandion/keys/$ID/manifest.json"
KD="$HOME/.pandion/keys/$ID"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

# IAM SSH-key helpers via the Scaleway REST API (no scw CLI dependency).
iam_keys_named(){ # -> count of project keys named $KEYNAME
  curl -fsS -H "X-Auth-Token: $SCW_SECRET_KEY" \
    "https://api.scaleway.com/iam/v1alpha1/ssh-keys?project_id=$SCW_DEFAULT_PROJECT_ID&page_size=100" 2>/dev/null \
    | python3 -c "import sys,json;print(sum(1 for k in json.load(sys.stdin).get('ssh_keys',[]) if k.get('name')=='$1'))" 2>/dev/null || echo 0
}

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=scaleway --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f "$YAML" /tmp/scw_cl_*.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
# [1] up a 3-node Scaleway cluster — the previously-failing multi-node scenario.
# ---------------------------------------------------------------------------
{ echo "apiVersion: pandion/v1"; echo "name: $ID"; echo "provider: { name: scaleway }"
  echo "nodes:"
  echo "  - { name: n1, run: \"echo PANDION_READY\" }"
  echo "  - { name: n2, run: \"echo PANDION_READY\" }"
  echo "  - { name: n3, run: \"echo PANDION_READY\" }"
} > "$YAML"

c_in "[1] up 3-node Scaleway cluster (--ttl 40m --max-cost 3.00)..."
if timeout 1800 env NO_COLOR=1 "$BIN" up --provider=scaleway -f "$YAML" --id "$ID" \
     --ttl 40m --max-cost 3.00 >/tmp/scw_cl_up.log 2>&1; then :; fi
if grep -qiE "mesh verif" /tmp/scw_cl_up.log; then
  c_ok "3-node cluster up + mesh formed (login key landed on root — fix works)"
else
  c_no "multi-node up did not complete"; tail -30 /tmp/scw_cl_up.log
fi

# ---------------------------------------------------------------------------
# [2] the IAM login key was actually registered (the mechanism of the fix).
# ---------------------------------------------------------------------------
c_in "[2] project IAM SSH key '$KEYNAME' registered..."
if [ "$(iam_keys_named "$KEYNAME")" -ge 1 ]; then
  c_ok "IAM login key present in project (Scaleway injects it into root at boot)"
else
  c_no "IAM login key not found — fix mechanism not engaged"
fi

# ---------------------------------------------------------------------------
# [3] every node reachable as root over its pinned key (⇒ key on root on ALL nodes).
# ---------------------------------------------------------------------------
c_in "[3] all 3 nodes reachable as root (multi-node key injection)..."
if [ -f "$MAN" ]; then
  mapfile -t IPS < <(python3 -c "import json;[print(n['ip']) for n in json.load(open('$MAN'))['nodes']]" 2>/dev/null)
  SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"
  ok=0
  for ip in "${IPS[@]}"; do
    $SSH "root@$ip" 'id -u' 2>/dev/null | grep -qx 0 && ok=$((ok+1))
  done
  [ "$ok" -eq 3 ] && c_ok "root login succeeded on all 3 nodes ($ok/3)" \
                  || c_no "root login only on $ok/3 nodes (key injection unreliable)"
else
  c_no "no manifest — cluster did not reach saveManifest"
fi

# ---------------------------------------------------------------------------
# [4] teardown reaps the IAM key (leak-free, C4) — checked live before the trap.
# ---------------------------------------------------------------------------
c_in "[4] down reaps the IAM login key (no leak)..."
"$BIN" down --provider=scaleway --id "$ID" --yes >/tmp/scw_cl_down.log 2>&1 || true
if [ "$(iam_keys_named "$KEYNAME")" -eq 0 ]; then
  c_ok "IAM login key deleted on down (ReapAux — nothing leaks)"
else
  c_no "IAM login key still present after down (leak)"
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "SCALEWAY MULTI-NODE: verified (IAM key fix)" || c_no "see failures above"
