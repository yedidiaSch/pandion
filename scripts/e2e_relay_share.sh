#!/usr/bin/env bash
# ============================================================================
# Pandion relay share e2e (Phase 1 complete) — the zero-install browser terminal,
# end to end: up a 2-node cluster, deploy the relay on node A, `relay share` node
# B, then drive the resulting token URL over a real WebSocket (as a browser would)
# and confirm we get an interactive shell AS THE SCOPED USER on node B. Then
# `unshare` and confirm the link is dead. Self-cleaning.
# Usage:  ./scripts/e2e_relay_share.sh [provider]   (default digitalocean)
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

PROV="${1:-digitalocean}"
ID="e2e-relayshare-$PROV"
BIN="./bin/pandion"
YAML="/tmp/relayshare.yaml"
PROBE=""
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f "$YAML" /tmp/relayshare_up.log /tmp/relayshare_relayup.log /tmp/relayshare_share.log /tmp/relayshare_ro.log /tmp/relayshare_unshare.log
  [ -n "$PROBE" ] && rm -rf "$PROBE"
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# a tiny WebSocket probe (a "browser" in Go), built inside the module so it reuses
# the coder/websocket dep. It opens the token URL, sends a command, and reports
# what the PTY streamed back.
PROBE="$(mktemp -d ./relayprobe-XXXXXX)"
cat > "$PROBE/main.go" <<'GO'
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	url, want := os.Args[1], os.Args[2]
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		fmt.Println("DIAL_FAIL", err)
		os.Exit(1)
	}
	defer c.Close(websocket.StatusNormalClosure, "done")
	time.Sleep(1500 * time.Millisecond)
	_ = c.Write(ctx, websocket.MessageBinary, []byte("whoami; echo "+want+"\n"))
	var sb strings.Builder
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, data, rerr := c.Read(ctx)
		if rerr != nil {
			break
		}
		sb.Write(data)
		if strings.Contains(sb.String(), want) {
			fmt.Println("MARKER_SEEN")
			if strings.Contains(sb.String(), "pandion-lab") {
				fmt.Println("USER_OK")
			}
			return
		}
	}
	fmt.Println("NO_MARKER")
	os.Exit(1)
}
GO

# ---------------------------------------------------------------------------
c_in "[1] up a 2-node cluster (relay + victim)..."
{ echo "apiVersion: pandion/v1"; echo "name: $ID"; echo "provider: { name: $PROV }"; echo "nodes:"
  echo "  - { name: relay,  run: \"echo READY\" }"
  echo "  - { name: victim, run: \"echo READY\" }"; } > "$YAML"
if timeout 1200 env NO_COLOR=1 "$BIN" up --provider="$PROV" -f "$YAML" --id "$ID" --ttl 30m --max-cost 2.00 >/tmp/relayshare_up.log 2>&1; then :; fi
grep -qi "mesh verif" /tmp/relayshare_up.log && c_ok "cluster up + mesh formed" || { c_no "cluster up failed"; tail -20 /tmp/relayshare_up.log; exit 1; }

c_in "[2] deploy the relay on node 'relay'..."
NO_COLOR=1 "$BIN" relay up --id "$ID" --node relay --port 8443 >/tmp/relayshare_relayup.log 2>&1
grep -q "relay up on" /tmp/relayshare_relayup.log && c_ok "relay deployed" || { c_no "relay up failed"; cat /tmp/relayshare_relayup.log; exit 1; }

c_in "[3] relay share --node victim → clickable URL..."
NO_COLOR=1 "$BIN" relay share --id "$ID" --node victim --expires 30m >/tmp/relayshare_share.log 2>&1
URL=$(grep -oE 'https://[0-9.]+:[0-9]+/s/PRLY1-[A-Za-z0-9_-]+' /tmp/relayshare_share.log | head -1)
[ -n "$URL" ] && c_ok "got a share URL" || { c_no "no share URL"; cat /tmp/relayshare_share.log; exit 1; }
WSS=$(echo "$URL" | sed 's#^https://#wss://#; s#/s/#/ws/#')
c_in "url=$URL"

c_in "[4] drive the URL over WebSocket — interactive shell as the scoped user on node B..."
OUT=$(go run "$PROBE/main.go" "$WSS" "RELAY_MARKER_OK" 2>&1 || true)
echo "$OUT" | sed 's/^/    /'
echo "$OUT" | grep -q "MARKER_SEEN" && c_ok "browser terminal bridged to the node (PTY round-trip)" || c_no "no PTY round-trip over the relay"
echo "$OUT" | grep -q "USER_OK" && c_ok "logged in as the scoped non-root user (pandion-lab) on node B" || c_no "not the scoped user"

c_in "[4b] read-only share: participant connects + sees output but cannot type..."
NO_COLOR=1 "$BIN" relay share --id "$ID" --node victim --read-only --expires 30m >/tmp/relayshare_ro.log 2>&1
URL_RO=$(grep -oE 'https://[0-9.]+:[0-9]+/s/PRLY1-[A-Za-z0-9_-]+' /tmp/relayshare_ro.log | head -1)
WSS_RO=$(echo "$URL_RO" | sed 's#^https://#wss://#; s#/s/#/ws/#')
OUT_RO=$(go run "$PROBE/main.go" "$WSS_RO" "RO_MARKER_OK" 2>&1 || true)
if echo "$OUT_RO" | grep -q "DIAL_FAIL"; then
  c_no "read-only link failed to connect: $OUT_RO"
elif echo "$OUT_RO" | grep -q "NO_MARKER"; then
  c_ok "read-only: connected, but keystrokes were dropped (command never ran)"
else
  c_no "read-only session accepted input (should be view-only): $OUT_RO"
fi

c_in "[4c] rate-limit: flooding unknown tokens gets throttled (429)..."
BASE="${URL%/s/*}"
got429=no
for i in $(seq 1 30); do
  code=$(curl -sk --max-time 8 "$BASE/s/PRLY1-bruteforce$i" -o /dev/null -w '%{http_code}' 2>/dev/null || echo 000)
  [ "$code" = 429 ] && { got429=yes; break; }
done
[ "$got429" = yes ] && c_ok "unknown-token flood throttled with 429" || c_no "no 429 after 30 bad-token requests"

c_in "[5] unshare → the link is dead..."
NO_COLOR=1 "$BIN" relay unshare --id "$ID" --all >/tmp/relayshare_unshare.log 2>&1
sleep 2
OUT2=$(go run "$PROBE/main.go" "$WSS" "RELAY_MARKER_OK" 2>&1 || true)
echo "$OUT2" | grep -q "DIAL_FAIL" && c_ok "revoked link is rejected (session gone)" || c_no "revoked link still works: $OUT2"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "RELAY SHARE (browser terminal): verified" || { c_no "see failures above"; exit 1; }
