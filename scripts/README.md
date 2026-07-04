# Pandion end-to-end tests

Real-cloud, **self-cleaning** integration tests. Each provisions on your own
account, asserts behavior, and tears everything down on exit (success, failure,
or Ctrl+C).

```bash
export HCLOUD_TOKEN=your-project-scoped-token   # (+ DIGITALOCEAN_TOKEN for the DO one)
./scripts/e2e_ls_cost.sh
```

Offline unit tests (`make ci` / `go test ./...`) cover the pure logic; these
scripts cover the parts that only manifest against a live cloud API + SSH. Most
cost a few cents (a node for a few minutes); the FREE ones provision nothing.

## Index

| Script | Proves | Cost |
|---|---|---|
| `e2e_m22.sh` | Provision-time hardening + WireGuard overlay + operator-scoped SSH | paid (1 node) |
| `e2e_m32b.sh` | 2-node cluster + WireGuard mesh + mutual reachability | paid (2 nodes) |
| `e2e_m33.sh` | Cluster + service discovery (`$PANDION_<NODE>_IP` over the overlay) | paid (2 nodes) |
| `e2e_m34.sh` | Multiplexed per-node output + per-node log files | paid (2 nodes) |
| `e2e_sync.sh` | Workspace sync → remote build → run (your code on the node) | paid (1 node) |
| `e2e_docker.sh` | `--engine=docker` single node (hardened container: cap-drop, no-new-privs, ro-rootfs) | paid (1 node) |
| `e2e_caps.sh` | Capability add-back on the least-privilege baseline (P1b), both engines | paid |
| `e2e_attach.sh` | Durable tmux runs, detach≠destroy, `attach` reconnect, crash detection — cluster + single-node | paid (2+1 nodes) |
| `e2e_ls_cost.sh` | `ls`/cost, `--dry-run`, `--max-cost` (reject over-budget/no-ttl, all FREE), region, reproducibility lockfile | mixed (1 node) |
| `e2e_metadata_block.sh` | Cloud-metadata egress blocked even when explicitly allowlisted (S-F) | paid (1 node) |
| `e2e_node_hardening.sh` | fail2ban + auditd active; workload runs as `pandion-run` (S-C) | paid (1 node) |
| `e2e_network_hardening.sh` | sysctl baseline (WG-safe rp_filter) + cloud-edge firewall + firewall no-leak on teardown (M8) | paid (1 node) |
| `e2e_luks.sh` | LUKS-at-rest workspace (`--encrypt-workspace`), verified via `pandion ssh` (S-E) | paid (1 node) |
| `e2e_ssh.sh` | `pandion ssh` — host-key-pinned command on a node | paid (1 node) |
| `e2e_cp.sh` | `pandion cp` — pinned scp round-trip to/from a node | paid (1 node) |
| `e2e_docker_cluster.sh` | Per-node `engine: docker` in a cluster (runs in the container, proven with an Alpine image) | paid (1 node) |
| `e2e_digitalocean.sh` | DigitalOcean provider: `--max-cost` preflight (FREE, exercises live pricing) + `ls` | mixed (1 droplet) |

> **Naming:** `e2e_m22/m32b/m33/m34` are milestone-era (they predate the current
> naming convention) but exercise still-current foundational behavior. The
> feature-named scripts were each run on live Hetzner as their feature merged.

## Conventions (for new scripts)

- `set -euo pipefail`; a `teardown` EXIT trap that runs `pandion down` and
  asserts nothing leaked (servers, and for M8, the cloud firewall).
- `c_ok` / `c_no` (sets `PASS=0`) / `c_in` helpers; final PASS/FAIL summary.
- Prefer **FREE** preflight/negative cases first (they provision nothing yet hit
  the live API), then a single cheap paid provision for the happy path.
- Verify with `bash -n` before running.
