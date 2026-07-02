#!/usr/bin/env bash
# ============================================================================
# Pandion attach e2e: a long-running cluster workload survives detach (runs in
# tmux), and `pandion attach` reconnects to its live output. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token
#   ./scripts/e2e_attach.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-attach"
BIN="./bin/pandion"
CLYAML="$(mktemp --suffix=.yaml)"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider=hetzner --id "$ID" >/dev/null 2>&1 || true
  rm -f "$CLYAML"
  if command -v hcloud >/dev/null 2>&1; then
    local left; left=$(hcloud server list -o noheader 2>/dev/null | grep -c "$ID" || true)
    [ "${left:-0}" = 0 ] && c_ok "teardown: no servers left" || c_no "teardown: $left left"
  fi
  c_in "done (exit $code)"
}
trap teardown EXIT

# two workloads:
#   worker  — a heartbeat every second (never exits): proves durable run + reattach
#   crasher — prints, then exits non-zero ~8s in (AFTER we detach): proves crash
#             detection survives the tmux hand-off and is reported on `attach`
CRASH_CODE=42
cat > "$CLYAML" <<EOF
apiVersion: pandion/v1
name: $ID
nodes:
  - name: worker
    run: 'i=0; while true; do i=\$((i+1)); echo "tick \$i \$(date +%s)"; sleep 1; done'
  - name: crasher
    run: 'echo "crasher: up"; sleep 8; echo "crasher: exiting $CRASH_CODE"; exit $CRASH_CODE'
EOF

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# `up` streams; we background it and kill our client after a few ticks (= detach).
c_in "provision + start long-running workload, then DETACH (~3-5 min)..."
NO_COLOR=1 "$BIN" up --provider=hetzner --id "$ID" -f "$CLYAML" >/tmp/attach_up.log 2>&1 &
UP_PID=$!
# wait until we see streaming ticks, then detach (kill our client; workload stays in tmux)
for _ in $(seq 1 40); do grep -q '\[worker\] tick' /tmp/attach_up.log && break; sleep 5; done
sleep 3
kill "$UP_PID" 2>/dev/null || true; wait "$UP_PID" 2>/dev/null || true
LAST_UP=$(grep -oE 'tick [0-9]+' /tmp/attach_up.log | tail -1 | awk '{print $2}')
grep -q '\[worker\] tick' /tmp/attach_up.log && c_ok "up streamed the workload (last tick=$LAST_UP)" || { c_no "up did not stream"; PASS=0; }
c_in "detached. waiting 8s (workload should keep ticking in tmux)..."; sleep 8

# `attach` should reconnect and show ticks HIGHER than where we detached (proof it kept running)
c_in "pandion attach — reconnecting..."
NO_COLOR=1 timeout 15 "$BIN" attach --id "$ID" >/tmp/attach_re.log 2>&1 || true
echo "---- attach output (tail) ----"; tail -6 /tmp/attach_re.log
LAST_RE=$(grep -oE 'tick [0-9]+' /tmp/attach_re.log | tail -1 | awk '{print $2}')

PASS=1
grep -q '\[worker\] tick' /tmp/attach_re.log && c_ok "attach reconnected to the stream" || { c_no "attach showed no stream"; PASS=0; }
if [ -n "${LAST_RE:-}" ] && [ -n "${LAST_UP:-}" ] && [ "$LAST_RE" -gt "$LAST_UP" ]; then
  c_ok "workload SURVIVED detach (tick $LAST_UP -> $LAST_RE — kept running in tmux)"
else
  c_no "could not confirm the workload advanced past detach (up=$LAST_UP re=$LAST_RE)"; PASS=0
fi

# crash detection: attach must report the crasher's non-zero exit code. The crash
# fires ~8s in, AFTER we detached, so seeing it on attach proves fail-fast reporting
# survives the detach/reattach hand-off (the exit code is persisted in the run log).
if grep -qE "\[crasher !\] process exited \(code $CRASH_CODE\)" /tmp/attach_re.log; then
  c_ok "crash DETECTED on attach (crasher exited code $CRASH_CODE — reported after detach)"
else
  c_no "crash NOT reported on attach (expected '[crasher !] process exited (code $CRASH_CODE)')"; PASS=0
fi
# stronger: the crash happened after we detached, so `up` should NOT have seen it.
if grep -qE 'process exited \(code' /tmp/attach_up.log; then
  c_in "note: crasher exited before detach this run (still valid — attach replayed the exit)"
else
  c_ok "crash occurred post-detach (absent from up log, present on attach)"
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "ATTACH + DURABLE RUN: verified" || c_no "see failures above"
rm -f /tmp/attach_up.log /tmp/attach_re.log
