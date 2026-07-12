#!/usr/bin/env bash
# ============================================================================
# Pandion relay Let's Encrypt e2e (Phase 2) — prove `relay up --domain` gets a
# real, BROWSER-TRUSTED cert (no self-signed warning). Uses nip.io magic DNS so
# the node's public IP has a DNS name Let's Encrypt can validate:
#   [1] up a node; [2] `relay up --domain <ip>.nip.io` (:443, TLS-ALPN-01);
#   [3] a VALIDATING HTTPS client (system roots, no -k) fetches over the LE cert;
#   [4] `relay share` + a VALIDATING WebSocket opens the terminal — trusted TLS,
#       end to end. Self-cleaning.
# Usage:  ./scripts/e2e_relay_le.sh [provider]   (default digitalocean)
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

PROV="${1:-digitalocean}"
ID="e2e-relayle-$PROV"
BIN="./bin/pandion"
MAN="$HOME/.pandion/keys/$ID/manifest.json"
PROBE=""
PASS=1

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$?; echo; c_in "cleaning up..."
  "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
  rm -f /tmp/relayle_relayup.log /tmp/relayle_share.log
  [ -n "$PROBE" ] && rm -rf "$PROBE"
  c_in "done (exit $code)"
}
trap teardown EXIT

c_in "building pandion..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok "built"

# a VALIDATING WebSocket probe (default system roots — no InsecureSkipVerify), so a
# successful connection PROVES the Let's Encrypt cert is genuinely browser-trusted.
PROBE="$(mktemp -d ./relayleprobe-XXXXXX)"
cat > "$PROBE/main.go" <<'GO'
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	url, want := os.Args[1], os.Args[2]
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil) // default TLS: validates the chain
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

c_in "[1] up a node..."
if timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID" --node relay --ttl 25m --max-cost 1.00 -- 'echo READY' >/dev/null 2>&1; then :; fi
[ -f "$MAN" ] || { c_no "no manifest"; exit 1; }
IP=$(python3 -c "import json;print(json.load(open('$MAN'))['nodes'][0]['ip'])")
DOMAIN="${IP}.nip.io"
c_in "node ip=$IP  domain=$DOMAIN"

c_in "[2] relay up --domain $DOMAIN (Let's Encrypt, :443)..."
NO_COLOR=1 "$BIN" relay up --id "$ID" --node relay --domain "$DOMAIN" >/tmp/relayle_relayup.log 2>&1
grep -q "Let's Encrypt" /tmp/relayle_relayup.log && c_ok "relay deployed with --domain" || { c_no "relay up --domain failed"; cat /tmp/relayle_relayup.log; exit 1; }

c_in "[3] wait for the browser-TRUSTED cert (validating HTTPS, no -k)..."
code=000
for i in $(seq 1 30); do
  code=$(curl -sS --max-time 10 "https://$DOMAIN/assets/xterm.js" -o /dev/null -w '%{http_code}' 2>/dev/null || echo 000)
  [ "$code" = 200 ] && break
  sleep 5
done
if [ "$code" = 200 ]; then
  c_ok "Let's Encrypt cert issued + browser-trusted (validated HTTPS, HTTP $code)"
else
  c_no "no trusted cert after wait (HTTP $code)"
  KD="$HOME/.pandion/keys/$ID"
  SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"
  echo "---- relay diagnostics ----"
  $SSH "root@$IP" 'systemctl status pandion-relay --no-pager -l 2>&1 | head -15; echo "--- journal ---"; journalctl -u pandion-relay --no-pager 2>&1 | tail -30; echo "--- listening ---"; ss -tlnp 2>/dev/null | grep -E ":443" || echo "NOT listening on :443"; echo "--- fw ---"; nft list chain inet pandion input 2>/dev/null | grep 443 || echo "no :443 firewall rule"' 2>&1 | sed 's/^/    /'
  exit 1
fi

c_in "[4] relay share + a VALIDATING WebSocket terminal over the trusted cert..."
NO_COLOR=1 "$BIN" relay share --id "$ID" --node relay --expires 20m >/tmp/relayle_share.log 2>&1
URL=$(grep -oE "https://$DOMAIN/s/PRLY1-[A-Za-z0-9_-]+" /tmp/relayle_share.log | head -1)
[ -n "$URL" ] && c_ok "share URL uses the domain: $URL" || { c_no "no domain share URL"; cat /tmp/relayle_share.log; exit 1; }
WSS=$(echo "$URL" | sed 's#^https://#wss://#; s#/s/#/ws/#')
OUT=$(go run "$PROBE/main.go" "$WSS" "RELAY_MARKER_OK" 2>&1 || true)
echo "$OUT" | sed 's/^/    /'
echo "$OUT" | grep -q "MARKER_SEEN" && c_ok "browser terminal over Let's Encrypt TLS (validated, PTY round-trip)" || c_no "no validated PTY round-trip"
echo "$OUT" | grep -q "USER_OK" && c_ok "scoped user (pandion-lab) shell" || c_no "not the scoped user"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "RELAY LET'S ENCRYPT: verified" || { c_no "see failures above"; exit 1; }
