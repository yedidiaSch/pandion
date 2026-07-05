#!/usr/bin/env bash
# ============================================================================
# Pandion attach e2e: a long-running workload survives detach (runs in tmux) and
# `pandion attach` reconnects to its live output. Runs TWO phases so both entry
# points are covered:
#   A) cluster    — `up -f cluster.yaml` (worker heartbeat + crasher node)
#   B) single     — `up` (no -f), one node running the heartbeat
# Self-cleaning (tears down both clusters on exit).
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_attach.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

CID="e2e-attach"        # cluster-path id
SID="e2e-attach-single" # single-node-path id
BIN="./bin/pandion"
CLYAML="$(mktemp --suffix=.yaml)"
CRASH_CODE=42
PASS=1
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"

# a heartbeat that ticks every second and never exits (proves durable run).
# single-quoted so the e2e shell does NOT expand it — the node's shell does.
HEARTBEAT='i=0; while true; do i=$((i+1)); echo "tick $i $(date +%s)"; sleep 1; done'

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  for id in "$CID" "$SID"; do
    "$BIN" down --provider=hetzner --id "$id" --yes >/dev/null 2>&1 || true
  done
  rm -f "$CLYAML" /tmp/attach_*.log
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -cE "$CID|$SID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

# wait until the worker's stream shows ticks, then DETACH (kill our client; the
# workload stays running in its tmux session on the node).
wait_and_detach(){ # <up_log> <up_pid>
  local log=$1 pid=$2 _
  for _ in $(seq 1 40); do grep -q '\[worker\] tick' "$log" && break; sleep 5; done
  sleep 3
  kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
}

# assert the heartbeat survived detach: attach must reconnect AND show ticks
# strictly higher than where we detached (proof it kept running in tmux).
assert_survival(){ # <label> <up_log> <re_log>
  local label=$1 uplog=$2 relog=$3 last_up last_re
  last_up=$(grep -oE 'tick [0-9]+' "$uplog" | tail -1 | awk '{print $2}')
  last_re=$(grep -oE 'tick [0-9]+' "$relog" | tail -1 | awk '{print $2}')
  grep -q '\[worker\] tick' "$uplog" && c_ok "$label: up streamed (last tick=$last_up)" || { c_no "$label: up did not stream"; PASS=0; }
  grep -q '\[worker\] tick' "$relog" && c_ok "$label: attach reconnected to the stream" || { c_no "$label: attach showed no stream"; PASS=0; }
  if [ -n "$last_re" ] && [ -n "$last_up" ] && [ "$last_re" -gt "$last_up" ]; then
    c_ok "$label: workload SURVIVED detach (tick $last_up -> $last_re — kept running in tmux)"
  else
    c_no "$label: could not confirm advance past detach (up=$last_up re=$last_re)"; PASS=0
  fi
}

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
# Phase A: CLUSTER path (up -f). worker=heartbeat, crasher=exits non-zero ~8s in
# (AFTER we detach) to prove crash detection survives the tmux hand-off.
# ---------------------------------------------------------------------------
cat > "$CLYAML" <<EOF
apiVersion: pandion/v1
name: $CID
nodes:
  - name: worker
    run: '$HEARTBEAT'
  - name: crasher
    run: 'echo "crasher: up"; sleep 8; echo "crasher: exiting $CRASH_CODE"; exit $CRASH_CODE'
EOF

c_in "[A] cluster: provision + start workloads, then DETACH (~3-5 min)..."
NO_COLOR=1 "$BIN" up --provider=hetzner --id "$CID" -f "$CLYAML" >/tmp/attach_cl_up.log 2>&1 &
wait_and_detach /tmp/attach_cl_up.log $!
c_in "[A] detached. waiting 8s (workload should keep ticking in tmux)..."; sleep 8

c_in "[A] pandion attach --id $CID — reconnecting..."
NO_COLOR=1 timeout 15 "$BIN" attach --id "$CID" >/tmp/attach_cl_re.log 2>&1 || true
echo "---- [A] attach output (tail) ----"; tail -6 /tmp/attach_cl_re.log
assert_survival "[A cluster]" /tmp/attach_cl_up.log /tmp/attach_cl_re.log

# crash detection: attach must report the crasher's non-zero exit code. The crash
# fires ~8s in, AFTER we detached, so seeing it on attach proves fail-fast
# reporting survives detach (the exit code is persisted in the run log).
if grep -qE "\[crasher !\] process exited \(code $CRASH_CODE\)" /tmp/attach_cl_re.log; then
  c_ok "[A cluster]: crash DETECTED on attach (crasher exited code $CRASH_CODE — after detach)"
else
  c_no "[A cluster]: crash NOT reported on attach (expected '[crasher !] process exited (code $CRASH_CODE)')"; PASS=0
fi
if grep -qE 'process exited \(code' /tmp/attach_cl_up.log; then
  c_in "[A cluster]: note: crasher exited before detach this run (still valid — attach replayed it)"
else
  c_ok "[A cluster]: crash occurred post-detach (absent from up log, present on attach)"
fi

# ---------------------------------------------------------------------------
# Phase B: SINGLE-NODE path (up, no -f). One node named 'worker' runs the
# heartbeat — proves the single-node wiring (manifest write + tmux launch +
# stream + attach) reaches parity with the cluster path.
# ---------------------------------------------------------------------------
c_in "[B] single-node: provision + start heartbeat, then DETACH (~3-5 min)..."
NO_COLOR=1 "$BIN" up --provider=hetzner --id "$SID" --node worker -- "$HEARTBEAT" >/tmp/attach_sg_up.log 2>&1 &
wait_and_detach /tmp/attach_sg_up.log $!
c_in "[B] detached. waiting 8s (workload should keep ticking in tmux)..."; sleep 8

c_in "[B] pandion attach --id $SID — reconnecting..."
NO_COLOR=1 timeout 15 "$BIN" attach --id "$SID" >/tmp/attach_sg_re.log 2>&1 || true
echo "---- [B] attach output (tail) ----"; tail -6 /tmp/attach_sg_re.log
assert_survival "[B single]" /tmp/attach_sg_up.log /tmp/attach_sg_re.log

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "ATTACH + DURABLE RUN (cluster + single-node): verified" || c_no "see failures above"
