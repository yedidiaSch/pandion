# EnvCore

Provision, harden & orchestrate ephemeral C++ / distributed-IPC dev & test
environments from one CLI — single nodes or multi-node clusters — with a
security-first posture, on your own Hetzner account. No backend, no service to
run, no secrets leave your machine.

> **Status: MVP complete (M0–M3.5)** — feature-complete core, validated on real
> cloud. Design docs live in `../plan/`; roadmap in `../plan/envcore-roadmap.md`;
> summary in `../plan/envcore-mvp-summary.md`.

## Highlights
- **Security-first:** provision-time hardening, **SSH host-key pinning** (MITM-proof),
  default-deny egress, **WireGuard overlay**, and lockout-safe **public deny-all**.
- **Clusters:** `cluster.yaml` topology, concurrent provisioning + barrier, full
  **WireGuard mesh**, service discovery (`$ENVCORE_<NODE>_IP`), IPC over the overlay,
  **multiplexed** per-node output + logs.
- **Safe by construction:** journaled state, orphan reconcile by tag, idempotent
  teardown, fail-fast crash handling, Ctrl+C auto-rollback / detach-not-destroy.

## Commands
```
envcore up   [--provider mock|hetzner] [--id ID] [flags] -- <cmd>   # single node
envcore up   --provider hetzner -f cluster.yaml --id ID             # cluster
envcore down [--provider ...] --id ID                               # verified teardown
envcore validate [-f cluster.yaml]                                  # schema check
envcore lockdown --id ID                                            # public deny-all (overlay-only)
envcore demo | version
```

## Install
Prebuilt binaries (linux/macOS/windows, amd64/arm64) are attached to each
[GitHub Release](https://github.com/yedidiaSch/envcore/releases) with a
`checksums.txt` and per-archive SBOMs. Download, verify, extract:
```bash
tar -xzf envcore_<version>_<os>_<arch>.tar.gz
./envcore version
```
Or build from source: `go install github.com/envcore/envcore/cmd/envcore@latest`.

## Build & test
```bash
export PATH="$HOME/.local/go/bin:$PATH"
make ci                 # gofmt + vet + go test -race + build  (offline, no cloud)
go run ./cmd/envcore demo
```

## Releasing (maintainers)
Releases are automated by [goreleaser](https://goreleaser.com) via
`.github/workflows/release.yml`: push a semver tag and the pipeline builds every
platform, generates checksums + SBOMs, writes the changelog, and publishes a
GitHub Release.
```bash
git tag v0.1.0 && git push origin v0.1.0     # -> Release workflow runs
```

## Real-cloud e2e (a few cents each, self-cleaning)
Set `HCLOUD_TOKEN` (project-scoped) first.
```bash
scripts/e2e_m22.sh    # overlay + operator-scoped SSH
scripts/e2e_m32b.sh   # 2-node cluster + WireGuard mesh
scripts/e2e_m33.sh    # service discovery + IPC over overlay
scripts/e2e_m34.sh    # multiplexed output + logs
```

## Layout
```
cmd/envcore/            # CLI: up / down / validate / lockdown / demo
internal/
  provider/{hetzner,mock}   # the cloud seam (mock = offline CI backbone)
  orchestrator/         # state machine + reconcile loop; Up / UpCluster (barrier)
  harden/  overlay/  firewall/  discovery/  stream/   # the hardening pipeline
  config/  ssh/  sshkeys/  state/
scripts/                # self-cleaning real-cloud e2e
```

## Design & findings
The `../plan/` folder holds the full design (review, requirements, security
architecture, provider comparison, risk register, roadmap) and the record of
**9 findings** that only surfaced against the live cloud API. Licensed Apache-2.0.
