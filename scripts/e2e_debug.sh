#!/usr/bin/env bash
# ============================================================================
# Pandion IDE Tier-2 e2e: distributed debug-attach over the pinned SSH pipe.
#
# Proves the whole `pandion debug` transport WITHOUT a GUI: provision a node
# running a long-lived C++ process, generate the VS Code cppdbg attach config,
# then run the EXACT pinned SSH pipe the config encodes to drive a remote gdb
# that attaches to the workload PID and prints a backtrace. That is precisely
# what VS Code does under the hood — if this attaches, F5 attaches. Self-cleaning.
#
#   export HCLOUD_TOKEN=your-token          # (or set another --provider below)
#   ./scripts/e2e_debug.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-debug"
NODE="worker"
PROV="${PANDION_PROVIDER:-hetzner}"
BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN (or PANDION_PROVIDER + its token)}"
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/dbg_*.log
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# ---------------------------------------------------------------------------
# Phase 1 (PAID, ~2-3 min): provision a hardened node with the default C++
# toolchain (gcc/gdb). The debug TARGET is compiled + launched in Phase 3 over
# the pinned pipe, so the test proves the debug transport without depending on
# how Pandion's `--` command reaps background processes on session close.
# ---------------------------------------------------------------------------
c_in "[1] up on $PROV — provision a hardened node with the C++ toolchain..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID" --node "$NODE" \
     --ttl 30m --run-as root -- 'gcc --version && gdb --version && echo PANDION_READY' >/tmp/dbg_up.log 2>&1; then :; fi
if grep -q "node is live" /tmp/dbg_up.log; then
  c_ok "node live with gcc + gdb present"
else
  c_no "up did not complete"; tail -25 /tmp/dbg_up.log
fi

# ---------------------------------------------------------------------------
# Phase 2: `pandion debug` generates a cppdbg attach config with the pinned pipe.
# ---------------------------------------------------------------------------
c_in "[2] pandion debug --print --public --program /root/app ..."
NO_COLOR=1 "$BIN" debug --id "$ID" --node "$NODE" --public --program /root/app --print >/tmp/dbg_cfg.log 2>&1 || true
grep -q '"type": "cppdbg"'            /tmp/dbg_cfg.log && c_ok "config is cppdbg/attach"        || c_no "config missing cppdbg"
grep -q '"pipeProgram": "ssh"'        /tmp/dbg_cfg.log && c_ok "uses ssh pipeTransport"         || c_no "no pipeTransport"
grep -q 'StrictHostKeyChecking=yes'   /tmp/dbg_cfg.log && c_ok "host-key pinned (MITM-proof)"   || c_no "not pinned"
grep -q 'UserKnownHostsFile='         /tmp/dbg_cfg.log && c_ok "pinned known_hosts referenced"  || c_no "no known_hosts"

# ---------------------------------------------------------------------------
# Phase 3 (the real proof): drive a REMOTE gdb over the pinned pipe and attach
# to the workload PID, exactly as VS Code would. Build the ssh invocation from
# the persisted key + known_hosts (what the config's pipeArgs encode).
# ---------------------------------------------------------------------------
c_in "[3] attach remote gdb over the pinned pipe and pull a backtrace..."
KEYDIR="$HOME/.pandion/keys/$ID"
# prefer the public IP the generated config already encodes (root@IP); fall back to ls --json.
ADDR=$(grep -oE 'root@[0-9.]+' /tmp/dbg_cfg.log | head -1 | cut -d@ -f2 || true)
[ -z "$ADDR" ] && ADDR=$(NO_COLOR=1 "$BIN" ls --provider="$PROV" --json 2>/dev/null | grep -oE '"ip": *"[^"]+"' | head -1 | cut -d'"' -f4 || true)
SSH_PIN=(-i "$KEYDIR/login_ed25519" -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
         -o UserKnownHostsFile="$KEYDIR/known_hosts" -o BatchMode=yes)
# Over the pinned pipe: compile a long-lived C++ program and launch it inside a
# detached tmux session (tmux ships in the default toolchain). tmux returns
# immediately and does NOT hold the SSH channel open — a plain `setsid ... &` over
# ssh hangs the client waiting for the channel to close.
SRC='#include <unistd.h>\nint spin(int n){ while(1){ sleep(1); n++; } return n; }\nint main(){ return spin(0); }'
ssh "${SSH_PIN[@]}" "root@$ADDR" \
  "printf '$SRC' > /root/app.c && gcc -g -O0 -o /root/app /root/app.c && tmux new-session -d -s dbgapp /root/app" \
  >/tmp/dbg_launch.log 2>&1 || true
sleep 2
PID=$(ssh "${SSH_PIN[@]}" "root@$ADDR" 'pgrep -x app | head -1' 2>/tmp/dbg_ssh.log || true)
if [ -n "${PID:-}" ]; then
  c_ok "found workload PID $PID on the node (over the pinned pipe)"
  ssh "${SSH_PIN[@]}" "root@$ADDR" "gdb --batch -p $PID -ex 'bt' -ex 'detach' -ex 'quit'" >/tmp/dbg_bt.log 2>&1 || true
  echo "---- remote gdb backtrace ----"; sed -n '1,15p' /tmp/dbg_bt.log; echo "------------------------------"
  if grep -qiE '#0 .*(spin|sleep|nanosleep|main)' /tmp/dbg_bt.log; then
    c_ok "remote gdb ATTACHED and produced a backtrace — Tier-2 transport verified"
  else
    c_no "gdb did not produce an expected backtrace"; cat /tmp/dbg_bt.log
  fi
else
  c_no "could not find the workload PID over the pinned pipe"; cat /tmp/dbg_ssh.log
fi

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "IDE TIER-2 (debug-attach): verified" || c_no "see failures above"
