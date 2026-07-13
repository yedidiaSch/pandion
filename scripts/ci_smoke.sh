#!/usr/bin/env bash
# ============================================================================
# Offline, cross-platform CLI smoke — proves the `pandion` binary actually RUNS
# on this OS (macOS / Windows / Linux), not just compiles. No cloud, no SSH, no
# secrets. Exercises config validation, mock provisioning, dry-run pricing,
# deploy-only parsing, and completion — i.e. path handling, state I/O and flag
# wiring on the host OS. Run by CI on every push (the cross-platform matrix) and
# usable by hand anywhere.
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f ./.env ] && { set -a; . ./.env; set +a; }   # auto-load provider creds from .env

run() { go run ./cmd/pandion "$@"; }
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t pandion)"
trap 'rm -rf "$tmp"' EXIT

echo "[smoke] version"
run version

echo "[smoke] validate — a valid cluster (incl. a deploy-only node)"
cat > "$tmp/ok.yaml" <<'YAML'
apiVersion: pandion/v1
name: smoke
nodes:
  - { name: n1, run: "echo hi" }
  - { name: n2 }
YAML
run validate -f "$tmp/ok.yaml"

echo "[smoke] validate — an invalid cluster must be REJECTED"
cat > "$tmp/bad.yaml" <<'YAML'
apiVersion: pandion/v1
name: "Bad Name!"
nodes: [ { name: n1, run: x } ]
YAML
if run validate -f "$tmp/bad.yaml"; then
  echo "[smoke] FAIL: invalid config was accepted"; exit 1
fi

echo "[smoke] mock provision (creates nothing, runs no SSH — exercises paths + state)"
run up --provider=mock --id ci-smoke -- 'echo hi'

echo "[smoke] dry-run preview (offline pricing)"
run up --provider=mock --id ci-dry --dry-run -- 'echo hi'

echo "[smoke] shell completion renders"
run completion bash > /dev/null

echo "[smoke] init writes a config + bare 'up' resolves to it (isolated home)"
# build once with the normal home (so Go's cache isn't created under the isolated
# home), then run the binary with an isolated home (Linux/macOS: HOME; Windows:
# USERPROFILE). Glob the output name so it works whether or not Go appended .exe.
go build -o "$tmp/pcli" ./cmd/pandion
BIN="$(ls "$tmp"/pcli* | head -1)"
H="$tmp/home"; mkdir -p "$H"
HOME="$H" USERPROFILE="$H" "$BIN" init --provider mock > /dev/null
if ! HOME="$H" USERPROFILE="$H" "$BIN" up --id cfg-smoke -- 'echo hi' | grep -q 'UP (mock)'; then
  echo "[smoke] FAIL: bare 'up' did not resolve to the configured default provider"; exit 1
fi

echo "[smoke] down/start/relay require --id on a real provider (P0.3 — no destructive default)"
# a clean home with NO local clusters, so 'down' can't auto-pick a sole cluster.
# NOTE: these commands exit non-zero, so capture the output (|| true) rather than
# piping into grep — under `set -o pipefail` the pipeline would inherit exit 2.
HE="$tmp/empty-home"; mkdir -p "$HE"
for cmd in "down --provider=hetzner" "start" "relay status"; do
  out="$(HOME="$HE" USERPROFILE="$HE" "$BIN" $cmd 2>&1 || true)"
  case "$out" in
    *"--id is required"*) : ;;
    *) echo "[smoke] FAIL: 'pandion $cmd' did not refuse a missing --id (got: $out)"; exit 1 ;;
  esac
done

echo "[smoke] down --provider=mock still targets the demo id (P0.3 escape hatch)"
out="$(HOME="$HE" USERPROFILE="$HE" "$BIN" down --provider=mock 2>&1 || true)"
case "$out" in *'"demo"'*) : ;; *) echo "[smoke] FAIL: bare mock 'down' no longer resolves the demo id"; exit 1 ;; esac

echo "[smoke] mock 'up' note no longer references shipped milestones (P0.6)"
out="$(HOME="$HE" USERPROFILE="$HE" "$BIN" up --provider=mock --id ci-note -- 'echo hi' 2>&1 || true)"
case "$out" in *M3.2b*) echo "[smoke] FAIL: stale milestone note still printed"; exit 1 ;; esac

echo "[smoke] PANDION_HOME relocates all state (P2.5)"
PH="$tmp/pandion-home"; mkdir -p "$PH"
PANDION_HOME="$PH" HOME="$HE" USERPROFILE="$HE" "$BIN" up --provider=mock --id ph -- 'echo hi' > /dev/null
[ -f "$PH/state/ph.json" ] || { echo "[smoke] FAIL: PANDION_HOME did not hold the state journal"; exit 1; }

echo "[smoke] init --cluster scaffolds a cluster.yaml that validates (P2.3)"
SC="$tmp/scaffold"; mkdir -p "$SC"
HOME="$HE" USERPROFILE="$HE" "$BIN" init --cluster "$SC/cluster.yaml" > /dev/null
HOME="$HE" USERPROFILE="$HE" "$BIN" validate -f "$SC/cluster.yaml" | grep -q "valid" || { echo "[smoke] FAIL: scaffolded cluster.yaml did not validate"; exit 1; }
# refuses to clobber without --force
if HOME="$HE" USERPROFILE="$HE" "$BIN" init --cluster "$SC/cluster.yaml" >/dev/null 2>&1; then
  echo "[smoke] FAIL: init --cluster overwrote an existing file without --force"; exit 1
fi

echo "[smoke] help/version discovery gestures exit 0 on stdout (P1.1)"
for g in "--help" "-h" "help" "--version" "-V"; do
  if ! HOME="$HE" USERPROFILE="$HE" "$BIN" $g >/dev/null 2>&1; then
    echo "[smoke] FAIL: 'pandion $g' did not exit 0"; exit 1
  fi
done
# `pandion up -h` shows the registry synopsis + example on stdout (P1.1/P1.2).
out="$(HOME="$HE" USERPROFILE="$HE" "$BIN" up -h 2>/dev/null || true)"
case "$out" in *"example: pandion up"*) : ;; *) echo "[smoke] FAIL: 'up -h' missing synopsis/example"; exit 1 ;; esac
# command-aware completion: ls offers --json but not the up-only --ttl (P1.2).
out="$(HOME="$HE" USERPROFILE="$HE" "$BIN" completion bash 2>/dev/null)"
case "$out" in *"ls) flags="*"--json"*) : ;; *) echo "[smoke] FAIL: completion not command-aware for ls"; exit 1 ;; esac

echo "[smoke] OK on ${RUNNER_OS:-$(uname -s)}"
