#!/usr/bin/env bash
# ============================================================================
# Pandion deploy-only + `pandion start` e2e.
#
# Proves the two-phase execution model on real cloud:
#   [1] `up --no-run` DEPLOYS a 2-node cluster (provision + harden + mesh) but
#       launches NOTHING — the run tmux session is absent on every node.
#   [2] one node has no `run:` at all (deploy-only) and one has a run command;
#       the manifest persists each node's run spec.
#   [3] `pandion start` launches the run command(s) — the tmux session + run log
#       now exist and contain the expected output; the deploy-only node is skipped.
#   [4] `pandion start --node <deploy-only>` errors helpfully (nothing to run).
# Self-cleaning.  Usage:  ./scripts/e2e_deploy_start.sh [provider]   (default hetzner)
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

PROV="${1:-hetzner}"
ID="e2e-deploystart-$PROV"
BIN="./bin/pandion"
YAML="/tmp/ds_$PROV.yaml"
KD="$HOME/.pandion/keys/$ID"
MAN="$KD/manifest.json"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f "$YAML" /tmp/ds_up.log /tmp/ds_start.log /tmp/ds_err.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"
node_ip(){ python3 -c "import json;print([n['ip'] for n in json.load(open('$MAN'))['nodes'] if n['name']=='$1'][0])"; }

# worker prints a unique marker; target is deploy-only (no run:).
MARK="PANDION_STARTED_$$"
{ echo "apiVersion: pandion/v1"; echo "name: $ID"; echo "provider: { name: $PROV }"
  echo "nodes:"
  echo "  - { name: target }"
  echo "  - { name: worker, run: \"echo $MARK && sleep 600\" }"
} > "$YAML"

# ---------------------------------------------------------------------------
c_in "[1] up --no-run: DEPLOY the cluster, launch nothing..."
if timeout 1200 env NO_COLOR=1 "$BIN" up --provider="$PROV" -f "$YAML" --id "$ID" \
     --no-run --ttl 30m --max-cost 2.00 >/tmp/ds_up.log 2>&1; then :; fi
if grep -qiE "deployed .* --no-run|nothing started" /tmp/ds_up.log; then
  c_ok "up --no-run reported deploy-only"
else
  c_no "up --no-run did not complete/report"; tail -25 /tmp/ds_up.log; exit 1
fi
[ -f "$MAN" ] || { c_no "no manifest written"; exit 1; }

WIP=$(node_ip worker); TIP=$(node_ip target)
c_in "worker=$WIP  target=$TIP"

# ---------------------------------------------------------------------------
c_in "[2] manifest persists the run spec; NO run tmux session exists yet..."
python3 -c "import json;m=json.load(open('$MAN'))['nodes'];w=[n for n in m if n['name']=='worker'][0];t=[n for n in m if n['name']=='target'][0];assert w.get('run'),'worker run missing';assert not t.get('run'),'target should be deploy-only';print('ok')" \
  && c_ok "manifest: worker has run spec, target is deploy-only" || c_no "manifest run spec wrong"
if $SSH "root@$WIP" 'tmux has-session -t pandion 2>/dev/null && echo LIVE || echo NONE' | grep -q NONE; then
  c_ok "worker: no run session after --no-run (nothing launched)"
else
  c_no "worker: a run session exists but --no-run should launch nothing"
fi

# ---------------------------------------------------------------------------
c_in "[3] pandion start --detach: launch the run command(s)..."
if NO_COLOR=1 "$BIN" start --id "$ID" --detach >/tmp/ds_start.log 2>&1; then :; fi
cat /tmp/ds_start.log
grep -qi "started 1 node" /tmp/ds_start.log && c_ok "start launched exactly 1 runnable node" || c_no "start node count wrong"
grep -qi "skipped deploy-only" /tmp/ds_start.log && c_ok "start skipped the deploy-only node" || c_no "start did not skip deploy-only"
sleep 4
if $SSH "root@$WIP" "cat /var/log/pandion/run.log 2>/dev/null" | grep -q "$MARK"; then
  c_ok "worker actually ran (marker in run log)"
else
  c_no "worker run log missing the marker"; $SSH "root@$WIP" 'tmux ls; tail /var/log/pandion/run.log' 2>&1 | head
fi
if $SSH "root@$TIP" 'test -f /var/log/pandion/run.log && echo YES || echo NO' | grep -q NO; then
  c_ok "target (deploy-only) ran nothing"
else
  c_no "target should have no run log"
fi

# ---------------------------------------------------------------------------
c_in "[4] start --node target (deploy-only) errors helpfully..."
if NO_COLOR=1 "$BIN" start --id "$ID" --node target >/tmp/ds_err.log 2>&1; then
  c_no "start on a deploy-only node should have failed"
else
  grep -qi "deploy-only" /tmp/ds_err.log && c_ok "helpful deploy-only error" || { c_no "unclear error"; cat /tmp/ds_err.log; }
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "DEPLOY-ONLY + START: verified" || c_no "see failures above"
