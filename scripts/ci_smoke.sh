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

echo "[smoke] OK on ${RUNNER_OS:-$(uname -s)}"
