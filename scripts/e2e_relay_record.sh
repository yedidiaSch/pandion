#!/usr/bin/env bash
# Focused relay session-recording e2e (single node) with verbose diagnostics.
# Usage:  ./scripts/e2e_relay_record.sh [provider]   (default digitalocean)
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

PROV="${1:-digitalocean}"
ID="e2e-relayrec-$PROV"
BIN="./bin/pandion"
MAN="$HOME/.pandion/keys/$ID/manifest.json"
KD="$HOME/.pandion/keys/$ID"
PROBE=""
PASS=1
c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }
teardown(){ local code=$?; echo; c_in "cleaning up..."; "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true; rm -f /tmp/relayrec_*.log; [ -n "$PROBE" ] && rm -rf "$PROBE"; rm -rf /tmp/relayrecdl; c_in "done ($code)"; }
trap teardown EXIT

c_in "build..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion; c_ok built
PROBE="$(mktemp -d ./relayrecprobe-XXXXXX)"
cat > "$PROBE/main.go" <<'GO'
package main
import ("context";"crypto/tls";"fmt";"net/http";"os";"strings";"time";"github.com/coder/websocket")
func main(){
 url,want:=os.Args[1],os.Args[2]
 ctx,cancel:=context.WithTimeout(context.Background(),25*time.Second); defer cancel()
 hc:=&http.Client{Transport:&http.Transport{TLSClientConfig:&tls.Config{InsecureSkipVerify:true}}}
 c,_,err:=websocket.Dial(ctx,url,&websocket.DialOptions{HTTPClient:hc})
 if err!=nil{fmt.Println("DIAL_FAIL",err);os.Exit(1)}
 defer c.Close(websocket.StatusNormalClosure,"done")
 time.Sleep(1500*time.Millisecond)
 _=c.Write(ctx,websocket.MessageBinary,[]byte("whoami; echo "+want+"\n"))
 var sb strings.Builder; dl:=time.Now().Add(15*time.Second)
 for time.Now().Before(dl){ _,d,e:=c.Read(ctx); if e!=nil{break}; sb.Write(d); if strings.Contains(sb.String(),want){fmt.Println("MARKER_SEEN");return} }
 fmt.Println("NO_MARKER"); os.Exit(1)
}
GO

c_in "up 1 node + relay up + share --record..."
timeout 900 env NO_COLOR=1 "$BIN" up --provider="$PROV" --id "$ID" --node relay --ttl 20m --max-cost 1 -- 'echo READY' >/tmp/relayrec_up.log 2>&1 || true
[ -f "$MAN" ] || { c_no "no manifest"; tail -15 /tmp/relayrec_up.log; exit 1; }
IP=$(python3 -c "import json;print(json.load(open('$MAN'))['nodes'][0]['ip'])")
NO_COLOR=1 "$BIN" relay up --id "$ID" --node relay >/tmp/relayrec_relayup.log 2>&1
grep -q "relay up on" /tmp/relayrec_relayup.log && c_ok "relay up" || { c_no "relay up"; cat /tmp/relayrec_relayup.log; exit 1; }
NO_COLOR=1 "$BIN" relay share --id "$ID" --node relay --record --expires 20m >/tmp/relayrec_share.log 2>&1
echo "  --- share output ---"; sed 's/^/    /' /tmp/relayrec_share.log
URL=$(grep -oE 'https://[0-9.]+:[0-9]+/s/PRLY1-[A-Za-z0-9_-]+' /tmp/relayrec_share.log | head -1)
[ -n "$URL" ] && c_ok "record share URL" || { c_no "no share URL"; exit 1; }
WSS=$(echo "$URL" | sed 's#^https://#wss://#; s#/s/#/ws/#')

c_in "drive the recorded session..."
OUT=$(go run "$PROBE/main.go" "$WSS" "REC_MARKER_OK" 2>&1 || true)
echo "  probe: $OUT"
echo "$OUT" | grep -q MARKER_SEEN && c_ok "session connected + ran" || c_no "session did not run"
sleep 3

SSH="ssh -i $KD/login_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15"
echo "  --- node state ---"
$SSH "root@$IP" 'echo "recordings:"; ls -la /var/lib/pandion-relay/recordings; echo "--- journal ---"; journalctl -u pandion-relay --no-pager | tail -20; echo "--- binary has RecordDir? ---"; strings /usr/local/bin/pandion-relay | grep -c "recording disabled"' 2>&1 | sed 's/^/    /'

c_in "fetch recordings..."
rm -rf /tmp/relayrecdl; NO_COLOR=1 "$BIN" relay recordings --id "$ID" --fetch /tmp/relayrecdl 2>&1 | sed 's/^/    /'
grep -rq "REC_MARKER_OK" /tmp/relayrecdl 2>/dev/null && c_ok "recording captured the terminal output" || c_no "recording empty/missing"

echo "================================================================"
[ "$PASS" = 1 ] && c_ok "RELAY RECORDING: verified" || { c_no "see above"; exit 1; }
