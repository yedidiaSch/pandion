#!/usr/bin/env bash
# ============================================================================
# macOS operator validation — the ~20% CI can't cover, run ONCE on a real Mac.
#
# CI (scripts/ci_smoke.sh, on every push) already proves the CLI builds, unit-
# tests, and runs offline on macOS. This adds the parts that need real hardware:
#   * the real macOS Keychain (go-keyring) — CI uses an in-memory mock;
#   * shelling out to the openssh client;
#   * (opt-in) a real cloud provision + WireGuard overlay join + SSH-over-overlay
#     + teardown, i.e. the full operator loop from a Mac.
#
# Usage:
#   ./scripts/mac_smoke.sh                     # offline smoke + keychain + ssh presence
#   PROVIDER=digitalocean ./scripts/mac_smoke.sh --cloud   # + real provision/overlay/teardown
#     (needs the provider token in your env/keychain and `brew install wireguard-tools`;
#      incurs a small, self-cleaning cloud cost)
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env
ok(){ printf '\033[32m[ OK ]\033[0m %s\n' "$*"; }
no(){ printf '\033[31m[FAIL]\033[0m %s\n' "$*"; FAIL=1; }
inf(){ printf '\033[36m[ .. ]\033[0m %s\n' "$*"; }
FAIL=0

[ "$(uname -s)" = "Darwin" ] || { echo "This script is for macOS."; exit 1; }
BIN="$(mktemp -t pandion)"; go build -o "$BIN" ./cmd/pandion || { echo "build failed"; exit 1; }

inf "1) offline CLI smoke (same as CI)"
if bash scripts/ci_smoke.sh >/dev/null 2>&1; then ok "offline smoke passed"; else no "offline smoke"; fi

inf "2) openssh client present (pandion shells out to it)"
if command -v ssh >/dev/null; then ok "ssh found: $(ssh -V 2>&1)"; else no "no ssh client on PATH"; fi

inf "3) real macOS Keychain round-trip (login -> use -> logout)"
if echo "mac-smoke-token-$$" | "$BIN" login --provider hetzner >/dev/null 2>&1; then
  "$BIN" logout --provider hetzner >/dev/null 2>&1 && ok "Keychain store + remove works" || no "Keychain remove failed"
else
  no "Keychain store failed (is the login keychain unlocked?)"
fi

if [ "${1:-}" = "--cloud" ]; then
  PROV="${PROVIDER:-digitalocean}"
  ID="mac-smoke-$$"
  inf "4) real provision on $PROV (small self-cleaning cost)"
  command -v wg-quick >/dev/null || no "wg-quick missing — brew install wireguard-tools (overlay step will be skipped)"
  if "$BIN" up --provider="$PROV" --id "$ID" --ttl 20m --max-cost 1.00 -- 'echo PANDION_READY'; then
    ok "provisioned + hardened + ran from macOS"
    conf="$HOME/.pandion/keys/$ID/wg-$ID.conf"
    if command -v wg-quick >/dev/null && [ -f "$conf" ]; then
      inf "5) overlay join (sudo wg-quick up) + SSH over the overlay"
      if sudo wg-quick up "$conf"; then
        "$BIN" ssh --id "$ID" --overlay -- 'echo OVERLAY_OK' | grep -q OVERLAY_OK \
          && ok "overlay join + SSH-over-overlay works" || no "SSH over overlay failed"
        sudo wg-quick down "$conf" >/dev/null 2>&1 || true
      else no "wg-quick up failed"; fi
    fi
  else
    no "provision failed (token set? budget ok?)"
  fi
  inf "teardown"; "$BIN" down --provider="$PROV" --id "$ID" --yes >/dev/null 2>&1 || true
fi

echo "================================================================"
[ "$FAIL" = 0 ] && ok "MAC SMOKE: all good" || { no "see failures above"; exit 1; }
