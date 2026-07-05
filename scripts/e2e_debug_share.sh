#!/usr/bin/env bash
# ============================================================================
# Pandion IDE Tier-2 SHARED-debug e2e (gdbserver design).
#
# Proves `pandion debug share` end to end WITHOUT a second machine or a GUI:
#   1. up a node running a long-lived C++ process as the unprivileged run user
#   2. `debug share --pid` → a token (+ share id)
#   3a. SSH TRANSPORT: guest key → ForceCommand → gdbserver ATTACHES over real SSH
#   3b. MEMORY READ: a gdb client drives the full wrapper→sudo→gdbserver chain and
#       pulls a REAL symbolized backtrace (spin/main) — the thing a capped non-root
#       gdb could NOT do
#   4. NEGATIVE: sharing a root-owned PID is refused (at share time)
#   5. REVOKE: `unshare` → the guest SSH now fails (key + pinned PID gone)
#   6. down → nothing left
# Self-cleaning.  export HCLOUD_TOKEN=…  ./scripts/e2e_debug_share.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."

ID="e2e-dbgshare"; NODE="worker"; PROV="${PANDION_PROVIDER:-hetzner}"; BIN="./bin/pandion"
: "${HCLOUD_TOKEN:?Set HCLOUD_TOKEN}"
PASS=1
c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }
teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/dbgshare_* /tmp/guest_key
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# --- Phase 1: up + a long-lived C++ workload owned by pandion-run --------------
c_in "[1] up on $PROV; launch a C++ workload as the unprivileged pandion-run user..."
if timeout 600 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID" --node "$NODE" \
     --ttl 30m -- 'echo PANDION_READY' >/tmp/dbgshare_up.log 2>&1; then :; fi
grep -q "node is live" /tmp/dbgshare_up.log && c_ok "node live" || { c_no "up failed"; tail -15 /tmp/dbgshare_up.log; }
KEYDIR="$HOME/.pandion/keys/$ID"
NO_COLOR=1 "$BIN" debug --id "$ID" --node "$NODE" --print >/dev/null 2>&1 || true   # materialize known_hosts
OP_PIN=(-i "$KEYDIR/login_ed25519" -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
        -o UserKnownHostsFile="$KEYDIR/known_hosts" -o BatchMode=yes)
ADDR=$(NO_COLOR=1 "$BIN" ls --provider="$PROV" --json 2>/dev/null | grep -oE '"ip": *"[^"]+"' | head -1 | cut -d'"' -f4)
SRC='#include <unistd.h>\nint spin(int n){ while(1){ sleep(1); n++; } return n; }\nint main(){ return spin(0); }'
ssh "${OP_PIN[@]}" "root@$ADDR" \
  "printf '$SRC' >/tmp/app.c && gcc -g -O0 -o /tmp/app /tmp/app.c && chmod 0755 /tmp/app && sudo -u pandion-run env HOME=/home/pandion-run tmux new-session -d -s dbgapp /tmp/app" \
  >/tmp/dbgshare_launch.log 2>&1 || true
sleep 2
WPID=$(ssh "${OP_PIN[@]}" "root@$ADDR" 'pgrep -u pandion-run -x app | head -1' 2>/dev/null || true)
[ -n "${WPID:-}" ] && c_ok "workload PID $WPID running as pandion-run" || c_no "no workload PID"

# --- Phase 2: share the PID → token + share id --------------------------------
c_in "[2] pandion debug share --pid $WPID → token..."
SHARE_OUT=$(NO_COLOR=1 "$BIN" debug share --id "$ID" --node "$NODE" --pid "$WPID" --expires 1h 2>&1 || true)
TOKEN=$(echo "$SHARE_OUT" | grep -E '^PDBG1-' | head -1 || true)
SID=$(echo "$SHARE_OUT" | grep -oE -- '--share [a-f0-9]+' | head -1 | awk '{print $2}' || true)
[ -n "$TOKEN" ] && [ -n "$SID" ] && c_ok "share issued token + id $SID" || { c_no "share failed"; echo "$SHARE_OUT"; }
# decode the guest key from the token (json in gzip+b64url)
python3 - "$TOKEN" >/dev/null 2>&1 <<'PY' || true
import sys,base64,gzip,json
b=json.loads(gzip.decompress(base64.urlsafe_b64decode(sys.argv[1].split("PDBG1-",1)[1])))
open("/tmp/guest_key","w").write(b["ssh_key"])
PY
chmod 600 /tmp/guest_key 2>/dev/null || true
GUEST_PIN=(-i /tmp/guest_key -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
           -o UserKnownHostsFile="$KEYDIR/known_hosts" -o BatchMode=yes)

# --- Phase 3a: real SSH transport → ForceCommand → gdbserver attach -----------
c_in "[3a] guest key over SSH → ForceCommand → gdbserver attaches (real transport)..."
timeout 6 ssh "${GUEST_PIN[@]}" "pandion-debug@$ADDR" >/tmp/dbgshare_ssh.log 2>&1 || true
grep -qE "Attached; pid = $WPID" /tmp/dbgshare_ssh.log \
  && c_ok "guest SSH → gdbserver attached to PID $WPID (non-root, ForceCommand)" \
  || { c_no "gdbserver did not attach over SSH"; sed -n '1,8p' /tmp/dbgshare_ssh.log; }

# --- Phase 3b: MEMORY READ via the full wrapper→sudo→gdbserver chain ----------
c_in "[3b] drive the wrapper→sudo→gdbserver chain with a gdb client → real backtrace..."
ssh "${OP_PIN[@]}" "root@$ADDR" \
  "gdb /tmp/app --batch -ex 'target remote | sudo -u pandion-debug /usr/local/bin/pandion-debug-forced $SID' -ex bt -ex detach -ex quit" \
  >/tmp/dbgshare_bt.log 2>&1 || true
echo "---- backtrace ----"; grep -E '#[0-9]+ ' /tmp/dbgshare_bt.log | head -6; echo "-------------------"
grep -qE '#[0-9]+ .*(spin|main) ' /tmp/dbgshare_bt.log \
  && c_ok "gdbserver-as-root serves workload MEMORY — real symbolized backtrace" \
  || { c_no "memory read failed"; sed -n '1,15p' /tmp/dbgshare_bt.log; }

# --- Phase 4: negative — sharing a root-owned PID is refused ------------------
c_in "[4] scope check: sharing a root-owned PID (1) must be refused..."
code=0
NO_COLOR=1 "$BIN" debug share --id "$ID" --node "$NODE" --pid 1 --print >/tmp/dbgshare_root.log 2>&1 || code=$?
grep -qi "system/root" /tmp/dbgshare_root.log && [ "$code" != 0 ] \
  && c_ok "sharing a root/system PID refused (uid<1000)" \
  || { c_no "root PID share was not refused (code $code)"; cat /tmp/dbgshare_root.log; }

# --- Phase 5: revoke — unshare, then the guest SSH fails ----------------------
c_in "[5] pandion debug unshare --all → grant revoked..."
NO_COLOR=1 "$BIN" debug unshare --id "$ID" --all >/tmp/dbgshare_unshare.log 2>&1 || true
code=0
timeout 8 ssh -o ConnectTimeout=6 "${GUEST_PIN[@]}" "pandion-debug@$ADDR" >/tmp/dbgshare_after.log 2>&1 || code=$?
[ "$code" != 0 ] && c_ok "guest SSH fails after unshare (key + pinned PID revoked)" \
                 || c_no "guest still had access after unshare!"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "IDE TIER-2 SHARED DEBUG: verified" || c_no "see failures above"
