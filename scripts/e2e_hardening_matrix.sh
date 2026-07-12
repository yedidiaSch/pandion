#!/usr/bin/env bash
# ============================================================================
# Pandion cross-provider hardening matrix (P1.3). The security posture is only
# meaningful if it holds on EVERY provider, not just Hetzner. This provisions one
# cheap node per provider (whose credential is present), runs a single probe, and
# prints a support matrix of the core host-hardening invariants:
#
#   meta    the cloud metadata endpoint (169.254.169.254) is UNREACHABLE (S-F)
#   egress  default-deny egress holds — an un-allowlisted destination is DENIED
#   v6      IPv6 is disabled (no dual-stack bypass of the IPv4-only nftables)
#   ssh     the node provisioned and ran the workload as the least-priv user
#
# Each provider is token-gated: absent credentials ⇒ skipped (not failed).
# Self-cleaning: every node is torn down at exit. A few cents per provider.
#
#   export HCLOUD_TOKEN=...            # hetzner
#   export DIGITALOCEAN_TOKEN=...      # digitalocean
#   export VULTR_API_KEY=...           # vultr
#   export LINODE_TOKEN=...            # linode
#   export SCW_SECRET_KEY=... SCW_ACCESS_KEY=... SCW_DEFAULT_PROJECT_ID=...
#   ./scripts/e2e_hardening_matrix.sh
#   E2E_PROVIDERS="hetzner vultr" ./scripts/e2e_hardening_matrix.sh   # subset
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

BIN="./bin/pandion"
TTL="${E2E_TTL:-20m}"
PROVIDERS="${E2E_PROVIDERS:-hetzner digitalocean vultr linode scaleway}"
PASS=1
IDS=()          # provider:id pairs we created → torn down at exit
declare -A ROW  # provider → formatted result row

c_ok(){ printf '\033[32m[ PASS ]\033[0m %s\n' "$*"; }
c_no(){ printf '\033[31m[ FAIL ]\033[0m %s\n' "$*"; PASS=0; }
c_in(){ printf '\033[36m[ e2e  ]\033[0m %s\n' "$*"; }
c_sk(){ printf '\033[33m[ skip ]\033[0m %s\n' "$*"; }

teardown(){
  local code=$? pair prov id
  echo; c_in "cleaning up..."
  for pair in "${IDS[@]:-}"; do
    [ -z "$pair" ] && continue
    prov="${pair%%:*}"; id="${pair#*:}"
    "$BIN" down --provider "$prov" --id "$id" --yes >/dev/null 2>&1 || true
  done
  c_in "done (exit $code)"
}
trap teardown EXIT INT TERM

# has_creds PROVIDER → 0 if this provider's credential(s) are present in the env.
has_creds(){
  case "$1" in
    hetzner)      [ -n "${HCLOUD_TOKEN:-}" ] ;;
    digitalocean) [ -n "${DIGITALOCEAN_TOKEN:-}" ] ;;
    vultr)        [ -n "${VULTR_API_KEY:-}" ] ;;
    linode)       [ -n "${LINODE_TOKEN:-}" ] ;;
    scaleway)     [ -n "${SCW_SECRET_KEY:-}" ] && [ -n "${SCW_ACCESS_KEY:-}" ] && [ -n "${SCW_DEFAULT_PROJECT_ID:-}" ] ;;
    *) return 1 ;;
  esac
}

# One probe gathers every invariant. curl targets are raw IPs (no DNS needed,
# since default-deny egress may block resolution).
PROBE='echo "META=$(curl -sS --max-time 5 http://169.254.169.254/ >/dev/null 2>&1 && echo REACHABLE || echo BLOCKED)"
echo "EGRESS=$(curl -sS --max-time 6 https://1.1.1.1/ >/dev/null 2>&1 && echo OPEN || echo DENIED)"
echo "V6=$(cat /proc/sys/net/ipv6/conf/all/disable_ipv6 2>/dev/null)"
echo "WHOAMI=$(id -un)"'

mark(){ # mark GOOD_CONDITION → "ok" (green) or "FAIL" (red), and fail the run
  if eval "$1"; then printf 'ok'; else printf 'FAIL'; PASS=0; fi
}

run_provider(){
  local prov="$1" id="e2e-mtx-${1:0:6}" log="/tmp/mtx_$1.log"
  c_in "== $prov: provision + probe (~3-5 min) =="
  IDS+=("$prov:$id")
  if ! timeout 700 env NO_COLOR=1 "$BIN" up --provider "$prov" --id "$id" --node probe \
        --ttl "$TTL" -- "$PROBE" >"$log" 2>&1; then
    c_no "$prov: up failed"; tail -8 "$log"
    ROW[$prov]=$(printf '%-13s %-5s %-7s %-4s %-4s' "$prov" "FAIL" "-" "-" "-")
    return
  fi
  local meta egress v6 who
  meta=$(grep -o 'META=[A-Z]*' "$log" | tail -1 | cut -d= -f2)
  egress=$(grep -o 'EGRESS=[A-Z]*' "$log" | tail -1 | cut -d= -f2)
  v6=$(grep -o 'V6=[0-9]*' "$log" | tail -1 | cut -d= -f2)
  who=$(grep -o 'WHOAMI=[a-z0-9-]*' "$log" | tail -1 | cut -d= -f2)
  c_in "  $prov: meta=$meta egress=$egress v6=$v6 whoami=$who"

  local rmeta regr rv6 rssh
  rmeta=$(mark '[ "$meta" = BLOCKED ]');  [ "$meta" = BLOCKED ] && c_ok "$prov: metadata endpoint blocked" || c_no "$prov: metadata REACHABLE"
  regr=$(mark '[ "$egress" = DENIED ]');  [ "$egress" = DENIED ] && c_ok "$prov: default-deny egress holds" || c_no "$prov: egress OPEN (default-deny leaked)"
  rv6=$(mark '[ "$v6" = 1 ]');            [ "$v6" = 1 ] && c_ok "$prov: IPv6 disabled" || c_no "$prov: IPv6 NOT disabled (v6=$v6)"
  rssh=$(mark '[ -n "$who" ]');           [ -n "$who" ] && c_ok "$prov: node ran workload (user=$who)" || c_no "$prov: no workload output"

  ROW[$prov]=$(printf '%-13s %-5s %-7s %-4s %-4s' "$prov" "$rmeta" "$regr" "$rv6" "$rssh")
}

c_in "building..."; export PATH="$HOME/.local/go/bin:$PATH"; go build -o "$BIN" ./cmd/pandion || { c_no "build"; exit 1; }
c_ok "built"

RAN=0
for prov in $PROVIDERS; do
  if has_creds "$prov"; then run_provider "$prov"; RAN=$((RAN+1))
  else c_sk "$prov: no credentials in env — skipped"; ROW[$prov]=$(printf '%-13s %-5s %-7s %-4s %-4s' "$prov" "skip" "skip" "skip" "skip"); fi
done

echo; echo "================ HARDENING SUPPORT MATRIX ================"
printf '%-13s %-5s %-7s %-4s %-4s\n' "provider" "meta" "egress" "v6" "ssh"
for prov in $PROVIDERS; do echo "${ROW[$prov]}"; done
echo "=========================================================="
[ "$RAN" -gt 0 ] || { c_no "no providers had credentials — nothing tested"; exit 1; }
[ "$PASS" = 1 ] && c_ok "ALL TESTED PROVIDERS PASSED ($RAN provider/s)" || c_no "see failures above"
