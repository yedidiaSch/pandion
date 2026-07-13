<div align="center">

# Pandion

**Hardened, mesh-networked C++/IPC test clusters — one command up, zero trace down.**

[![CI](https://github.com/yedidiaSch/pandion/actions/workflows/ci.yml/badge.svg)](https://github.com/yedidiaSch/pandion/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yedidiaSch/pandion?sort=semver)](https://github.com/yedidiaSch/pandion/releases)
[![License](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/yedidiaSch/pandion)](go.mod)

</div>

Pandion is a single-binary CLI that provisions, security-hardens, and orchestrates ephemeral
remote dev/test environments for C++ and distributed-IPC workloads (ZeroMQ, MQTT, and the like)
on your own cloud account — Hetzner, DigitalOcean, Vultr, Linode/Akamai, or Scaleway. No backend,
no agents, no secrets leaving your machine.

```console
$ pandion up --provider=hetzner -f cluster.yaml
UP cluster "pipeline": provisioning 2 hardened nodes (concurrent)...
  broker   ip=178.105.197.227  overlay=10.99.0.1
  worker   ip=188.245.229.15   overlay=10.99.0.2
forming WireGuard mesh (wg set peers)...
verifying mesh reachability...
  broker -> worker (10.99.0.2): OK
  worker -> broker (10.99.0.1): OK
injecting service discovery...   # each node learns $PANDION_<NODE>_IP
streaming 2 node command(s) (Ctrl+C detaches; logs: ~/.pandion/logs/pipeline)
----------------------------------------------------------------
[broker] listening on tcp://*:5557
[worker] connected to broker at 10.99.0.1:5557 over the overlay
[worker] processed batch 1
[broker] dispatched 1 result

$ pandion down --id pipeline                      # provider read from the cluster manifest
DOWN (hetzner): cluster "pipeline" reconciled to empty.   # nothing left billing
```

## Overview

Standing up a realistic multi-node C++ IPC test bed usually means yak-shaving: spin up VMs,
lock them down, install a toolchain, wire the network, hand-edit IPs, babysit logs, and remember
to delete everything before the bill lands. Pandion collapses that into one command that is
secure by default and leaves nothing behind.

- **One command, whole cluster.** Describe a topology in `cluster.yaml`; get N provisioned,
  hardened, mesh-networked nodes with the toolchain installed and your commands streaming.
- **Security is the default, not a checklist.** Every node boots with SSH host-key pinning,
  key-only auth, default-deny egress, and an encrypted WireGuard overlay — optionally
  public-deny-all, so a scanner sees nothing.
- **No hardcoded IPs.** Nodes discover each other by name (`$PANDION_BROKER_IP`) and talk over
  the encrypted overlay.
- **No leaks, ever.** State is journaled, every resource is tagged, and teardown is idempotent
  and reconciles against the provider — an interrupted run or a lost laptop cannot orphan a
  billing node.
- **Your money, your machine.** Pandion runs no service and stores no user credentials; the only
  cloud bill is your own.

## Capabilities

| Capability | Detail |
|---|---|
| One-command clusters | `cluster.yaml` drives concurrent provisioning plus a readiness barrier — every node is ready before any command runs |
| Five providers | Hetzner, DigitalOcean, Vultr, Linode/Akamai, and Scaleway behind one seam (plus an offline `mock`), with spec-based size discovery, first-class tags, and exact live pricing |
| Hardened by default | Host-key pinning, key-only auth, default-deny egress, WireGuard overlay, cloud-edge firewall, metadata block, fail2ban, auditd, sysctl baseline, least-privilege run user, LUKS-at-rest — see [Security](#security) |
| Public deny-all | Lockout-safe `pandion lockdown` makes SSH overlay-only; a public scan sees just the WireGuard port |
| Service discovery | `$PANDION_<NODE>_IP` is injected into every node — no hardcoded IPs, and IPC rides the overlay |
| Dependencies | Built-in C++ toolchain, plus apt `packages:` and arbitrary `setup:` commands (pip/npm/curl) installed in the build window |
| Durable runs | Workloads run under tmux; `Ctrl+C` detaches without killing them, and `pandion attach` reconnects to the live multiplexed streams |
| Deploy / run split | `--no-run` deploys without launching; `pandion start` runs the workloads on demand |
| Cost-aware | Live cost in `ls`/`status`, `--dry-run` cost preview, `--max-cost` budget cap, and a `reap` orphan sweep |
| Reproducible & signed | Toolchain-version lockfile (`--lock`); keyless-cosign-signed releases with per-archive SBOMs |
| Native or Docker | Toolchain on the host (as `pandion-run`) or a hardened container — single node and per node in clusters |
| IDE debug-attach | `pandion debug` attaches your local VS Code debugger to a remote process over the overlay — remote `gdb` driven through the pinned SSH pipe, no new port or agent |
| Shared debugging | `pandion debug share` grants a teammate one scoped, expiring, revocable remote-debug token — a root `gdbserver` pinned to one non-root PID over the pinned SSH pipe; no shell, no port, no root RCE |
| Encrypted L2 overlay | `security.overlay: l2` adds a VXLAN-over-WireGuard Layer-2 segment with an orchestrator-managed static FDB, isolated from the provider LAN. `safe` is spoof-resistant (host-side ARP inspection); `lab` is a contained, attackable cyber-range for authorized labs/CTF |

## Install

Homebrew (macOS/Linux):

```bash
brew install yedidiaSch/tap/pandion
```

Debian/Ubuntu, from the signed APT repo:

```bash
curl -fsSL https://yedidiaSch.github.io/pandion/gpg.key | sudo gpg --dearmor -o /usr/share/keyrings/pandion.gpg
echo "deb [signed-by=/usr/share/keyrings/pandion.gpg] https://yedidiaSch.github.io/pandion/deb stable main" | sudo tee /etc/apt/sources.list.d/pandion.list
sudo apt update && sudo apt install pandion
```

RHEL/Fedora, from the signed YUM repo:

```bash
sudo curl -fsSL https://yedidiaSch.github.io/pandion/pandion.repo -o /etc/yum.repos.d/pandion.repo
sudo dnf install pandion
```

From source (Go 1.22+):

```bash
go install github.com/yedidiaSch/pandion/cmd/pandion@latest
```

Prebuilt archives (linux/macOS/windows, amd64/arm64) ship with `checksums.txt` and per-archive
SBOMs on the [releases page](https://github.com/yedidiaSch/pandion/releases/latest). Each
release's checksums are signed with a keyless [Sigstore/cosign](https://docs.sigstore.dev/)
signature whose identity is pinned to this repository's release workflow:

```bash
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature   checksums.txt.sig \
  --certificate-identity-regexp '^https://github.com/yedidiaSch/pandion/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing
```

### Shell completion

Pandion generates completion scripts for bash, zsh, and fish (command- and
flag-aware). Install the one for your shell:

```bash
pandion completion bash | sudo tee /etc/bash_completion.d/pandion    # bash
pandion completion zsh  > "${fpath[1]}/_pandion"                     # zsh (then restart)
pandion completion fish > ~/.config/fish/completions/pandion.fish    # fish
```

Run `pandion completion <shell>` on a terminal and it also prints the matching
install one-liner (on stderr, so redirecting stdout to a file stays clean).

## Documentation

- [Getting started](docs/getting-started.md) — install → configure → up → down.
- [cluster.yaml reference](docs/cluster-yaml.md) — every field (generated from the schema).
- [Reference](docs/reference.md) — env vars, exit codes, config precedence, `~/.pandion` layout.
- [Troubleshooting](docs/troubleshooting.md) — common errors and fixes.
- [Examples](examples/) — single-node, Docker engine, GPU, and Python `setup:` workloads.

## Quickstart

The quickest way to get set up is the interactive wizard — it picks a default provider, stores
your token in the OS keychain, and writes `~/.pandion/config.yaml` so bare commands work with no
flags:

```bash
pandion init
```

`init` also records optional `defaults.{region,size,ttl}`, which seed any `up` that omits
`--region`/`--size`/`--ttl` (an explicit flag always wins). After that, `pandion up -- ./app`
needs no `--provider`, and no size/region either. Prefer to do it by hand (or automate)?
Authenticate with a project-scoped API token — an environment variable, or the OS keychain:

```bash
export HCLOUD_TOKEN=your-token          # or DIGITALOCEAN_TOKEN / VULTR_API_KEY / LINODE_TOKEN / LAMBDA_API_KEY
pandion login --provider hetzner        # alternatively, store it in the OS keychain
```

For GPU work, `pandion up --gpu MODEL[:N]` provisions an on-demand GPU node — hardened,
overlay-meshed, and torn down like any other. Browse the live, priced catalog with
`pandion list-gpus --provider <name>`, then e.g. `pandion up --gpu a100 -- ./train.sh`. Two
providers today, both with CUDA-native images (no driver setup):

- **Lambda Cloud** (`--provider lambda`) — A10 → B200, GPU-only.
- **DigitalOcean GPU Droplets** (`--provider digitalocean`) — H100/H200/L40S/RTX/MI300X, using
  your existing DO token (the successor to Paperspace; needs GPU-Droplet quota on your DO account).

On a GPU node the idle dead-man's-switch also treats **GPU utilization** as liveness: a
headless training job (no SSH session) keeps the node alive, while a truly idle box still
powers off after `--ttl` — tune the threshold with `--gpu-idle-util` (default 5%). Teardown
(`pandion down`) prints a cost **receipt** (`ran 2h13m · total ~5.31 USD`).

For **multi-node GPU clusters**, add `gpu:` to nodes in a `cluster.yaml` (or `--gpu` as the
cluster-wide default) and `pandion up -f cluster.yaml` stands up a WireGuard-meshed GPU fleet.
Each node gets rendezvous env for distributed frameworks —
`MASTER_ADDR=$PANDION_MASTER_ADDR torchrun --nnodes $PANDION_WORLD_SIZE --node-rank $PANDION_RANK …`
— so training groups form over the overlay with no hardcoded IPs.

Scaleway uses a triple (`SCW_SECRET_KEY`, `SCW_ACCESS_KEY`, `SCW_DEFAULT_PROJECT_ID`); only the
secret key is sensitive. Provider resolution follows `--provider` flag → `~/.pandion/config.yaml`
default → the only provider you have credentials for → (on a terminal) a prompt; automation stays
explicit and never prompts.

Run the full lifecycle offline, at zero cost, with the `mock` provider:

```bash
pandion demo
```

See a real cluster before writing any code — a ready-to-run ZeroMQ broker with two
workers lives in [`examples/zmq-cluster`](examples/zmq-cluster) (a few cents,
self-cleaning):

```bash
cd examples/zmq-cluster && pandion up --provider=hetzner -f cluster.yaml --id zmq-demo
```

A single hardened cloud box:

```bash
pandion up --provider=hetzner --id build -- 'gcc --version && cmake --version'
pandion down --provider=hetzner --id build
```

Add dependencies. Pandion installs a built-in C++ toolchain; declare extra apt packages with
`--packages` (or `toolchain.packages` in a cluster), and non-apt software with `setup:` commands
that run in the egress-open build window:

```bash
pandion up --provider=hetzner --id build --packages libzmq3-dev,libboost-dev \
  --setup 'pip3 install -r requirements.txt' -- ./app
```

A multi-node IPC cluster — describe it in `cluster.yaml`:

```yaml
apiVersion: pandion/v1
name: pipeline
nodes:
  - name: broker
    run: ./build/broker --frontend tcp://*:5557
  - name: worker
    run: ./build/worker --connect tcp://$PANDION_BROKER_IP:5557   # discovered, over the overlay
```

```bash
pandion validate -f cluster.yaml
pandion up --provider=hetzner -f cluster.yaml --id pipeline
pandion down --provider=hetzner --id pipeline
```

Go fully dark — public deny-all, SSH over the overlay only:

```bash
sudo wg-quick up ~/.pandion/keys/pipeline/wg-pipeline.conf   # join the overlay
pandion lockdown --id pipeline                               # verifies overlay reach, then denies public
```

## Profiles

Use `--profile NAME` (or `$PANDION_PROFILE`) to keep separate configs and credentials for
different accounts or projects — for example, a personal Hetzner account and a work
DigitalOcean account side by side:

```bash
pandion --profile work init --provider digitalocean
pandion --profile work login --provider digitalocean   # stored as "work@digitalocean" in keychain

pandion --profile personal init --provider hetzner
pandion --profile personal login --provider hetzner   # stored as "personal@hetzner"

pandion profiles          # list all profiles; * = active
pandion --profile work up -- './app'
```

Named profiles store their defaults in `~/.pandion/profiles/<name>.yaml`; the default
profile (no `--profile`) continues to use `~/.pandion/config.yaml` and bare keychain
entries, so existing setups are unchanged.

## Running your code

You don't ship a machine image or a container to run your own program — Pandion gets
your code onto each node for you. There are three ways, depending on what you have:

> **Shortcut — `pandion build [dir]`.** For a standard project, skip the flags: `pandion build`
> auto-detects the toolchain (CMake, Meson, Cargo, Go, npm, Python, or Make), uploads the
> directory, and builds it in the cloud. Add `-- <cmd>` to run the result, or omit it to
> build-only and `pandion ssh`/`start`/`debug` afterwards. It's sugar over way **2** below;
> any `up` flag (`--provider`, `--size`, `--id`, …) passes straight through.
>
> ```bash
> pandion build ./my-app -- ./build/app     # detect + upload + build + run
> ```

**1. A command that needs no code of yours.** Anything already on the node (the toolchain,
your `--packages`, whatever `setup:` installed) just runs:

```bash
pandion up --provider=hetzner -- 'gcc --version && cmake --version'
```

**2. Build a project on the node** (`sync` mode `source`, the default). Point Pandion at a
local directory; it uploads it, runs your build command **on the node** (in the egress-open
window, so the build can fetch dependencies), then runs your program:

```bash
pandion up --provider=hetzner --workspace . --build 'cmake -B build && cmake --build build' -- ./build/app
```

```yaml
# cluster.yaml — same thing, per node or under defaults:
sync:
  path: .                                   # local dir to upload (honors .pandionignore / .gitignore)
  build: cmake -B build && cmake --build build
run: ./build/app
```

**3. Upload a prebuilt binary** (`sync` mode `binaries`). Build locally, ship the artifact
as-is — no remote build. Unlike source mode, `binaries` does **not** apply `.gitignore`, so
build output (e.g. `./dist`, `./build`) is included rather than filtered out:

```bash
pandion up --provider=hetzner --workspace ./dist --sync-mode binaries -- ./dist/app
```

```yaml
sync:
  mode: binaries
  path: ./dist        # uploaded verbatim (only .pandionignore is applied; .git is always excluded)
run: ./dist/app       # build for the node's architecture — Linux, usually amd64
```

### Options

| `sync:` key | Meaning |
|---|---|
| `mode` | `source` (default — build on the node) or `binaries` (upload prebuilt, no build) |
| `path` | Local directory to upload (default `./`) |
| `build` | Command run on the node after upload (source mode only) |
| `remote_path` | Where the workspace lands on the node (defaults to the run user's workspace) |

The `run:` value (or the single-node `-- <cmd>`) is a normal shell command run from the
workspace directory as the unprivileged `pandion-run` user, with service-discovery variables
in scope — so `run: ./worker --broker $PANDION_BROKER_IP` works with no hardcoded IPs. Chain
steps with `&&`, set env inline, or run a script. Single-node flags: `--workspace`, `--build`,
`--sync-mode`, `--remote-path`, `--run-as`, `--cap-add`. See [`examples/zmq-cluster`](examples/zmq-cluster)
for a complete sync + build + run cluster.

> Anything uploaded is built/run on **Ubuntu Linux** — compile prebuilt binaries for the node's
> architecture (`linux/amd64` unless you chose arm64 sizes), or use `source` mode to build there.
> In `binaries` mode Pandion checks each binary's architecture against the node's and **warns on a
> mismatch** before you hit a runtime `Exec format error`.

## Security

Pandion treats every cloud host as hostile until hardened, and hardens it at provisioning
time — there is no window in which a fresh node is exposed.

| Control | What it does |
|---|---|
| Provision-time hardening | Hardened `sshd` and keys injected via cloud-init; the host boots already locked down |
| SSH host-key pinning | Pandion generates the host key, so it knows the fingerprint and pins it — MITM is rejected |
| Key-only access | Login key registered with the provider; password auth disabled |
| Default-deny egress | nftables output lockdown after the toolchain fetch — a compromised workload cannot phone home |
| Cloud-metadata block | Egress to `169.254.169.254` dropped unconditionally, so instance credentials cannot be read |
| WireGuard overlay | All management and IPC on an encrypted plane; SSH can be bound to it entirely |
| Public deny-all | `pandion lockdown` removes public SSH only after verifying overlay access — you cannot lock yourself out |
| Cloud-edge firewall | A provider firewall (SSH, WireGuard, ICMP inbound only) in front of the host nftables; removed on teardown |
| Least privilege | Workloads run as unprivileged `pandion-run`, or in a hardened container; only declared capabilities are added back |
| fail2ban, auditd, sysctl | SSH brute-force protection, an on-node audit trail, and a CIS-lite kernel network baseline — on by default |
| LUKS-at-rest | Opt-in `--encrypt-workspace`: encrypted workspace with an ephemeral RAM key; the disk yields only ciphertext |
| Idle dead-man's-switch | An abandoned node powers itself off after no SSH for the TTL window |
| Signed, no telemetry | Keyless-cosign-signed releases with SBOMs; zero telemetry, and tokens/keys stay on your machine |

## How it works

Pandion is a stateful controller that spends real money, so it is built around durable state,
idempotency, and reconciliation rather than fire-and-forget scripts.

```
        your machine                                   cloud (your account)
  ┌───────────────────────┐                        ┌──────────────────────────┐
  │  pandion (one binary) │                        │   node: broker (Ubuntu)  │
  │                       │   1. CreateServer +     │   • pinned host key      │
  │  orchestrator ─┐      │──── cloud-init  ───────▶│   • toolchain + nftables │
  │  (state machine│      │                        │   • wg0  10.99.0.1        │
  │   + reconcile) │      │   2. pinned SSH         │           ▲              │
  │                │      │◀─── (over overlay) ────▶│           │ WireGuard    │
  │  provider ◀────┘      │                        │           ▼   mesh       │
  │  (hetzner|do|vultr|   │   3. wg set peers,      │   node: worker (Ubuntu)  │
  │   linode|scw|mock)    │      inject $PANDION_*  │   • wg0  10.99.0.2        │
  │  journaled state ─────┼── tag-based reconcile ─▶│   • $PANDION_BROKER_IP   │
  └───────────────────────┘   (source of truth)    └──────────────────────────┘
```

The lifecycle — `PLANNED → PROVISIONING → HARDENED → MESHED → READY → RUNNING → DESTROYED` — is
journaled before and after every transition, so any command is resumable. The provider, queried
by tag, is the source of truth for teardown; local state is only a cache. Under the hood: a
provider seam (`hetzner`, `digitalocean`, `vultr`, `linode`, `scaleway`, and an offline `mock`),
a concurrent orchestrator with a readiness barrier, and a hardening pipeline of small,
unit-tested packages (`harden` → `overlay` → `firewall` → `discovery` → `stream`).

## Command reference

| Command | What it does |
|---|---|
| `pandion [--profile NAME] <cmd>` | Run any command under a named profile (`$PANDION_PROFILE` also accepted) |
| `pandion init` | First-run setup: pick a default provider, log in, write `~/.pandion/config.yaml` (or `~/.pandion/profiles/<name>.yaml` with `--profile`) |
| `pandion build [dir] [flags] [-- <cmd>]` | Auto-detect the toolchain, upload the project, and build it in the cloud |
| `pandion up [--provider …] [--id ID] [flags] -- <cmd>` | Provision, harden, and run a single node |
| `pandion up --provider … -f cluster.yaml --id ID` | Provision a multi-node cluster and mesh |
| `pandion up … --no-run` | Deploy only: provision, sync, and build, but do not launch the run command |
| `pandion start --id ID [--node N] [--detach]` | Launch the run command(s) on a deployed cluster/node |
| `pandion attach --id ID` | Reconnect to a running cluster's live multiplexed streams |
| `pandion ssh --id ID [--node N] [--overlay] [-- CMD]` | Host-key-pinned SSH into a node |
| `pandion cp --id ID [--node N] SRC DST` | Copy to/from a node (prefix a node path with `:`) |
| `pandion code --id ID [--node N] [--print]` | Pinned SSH config for VS Code Remote-SSH / JetBrains Gateway |
| `pandion debug --id ID [--node N] [--pid N] [--print]` | Attach your local debugger to a remote process over the overlay |
| `pandion debug share --id ID [--node N] [--expires 2h]` | Grant a teammate a scoped, expiring, revocable remote-debug token |
| `pandion debug join <token>` · `unshare --id ID --all` | Accept a shared grant · revoke it |
| `pandion ls` / `status [--json]` | List live clusters/nodes with uptime and live cost |
| `pandion reap [--older-than DUR] [--yes]` | Destroy orphaned Pandion nodes across clusters |
| `pandion down --id ID` | Idempotent, verified teardown (provider read from the manifest; `--provider` optional) |
| `pandion validate [-f cluster.yaml]` | Schema-check a topology |
| `pandion lockdown --id ID` | Lockout-safe public deny-all (SSH over the overlay only) |
| `pandion login \| logout [--provider …]` | Store/remove a provider's API token in the OS keychain |
| `pandion profiles` | List configured profiles; `*` marks the active one |
| `pandion completion bash\|zsh\|fish` | Shell completion script |
| `pandion demo` · `pandion version` | Offline mock lifecycle · version |

Selected `up` flags: `--packages`, `--setup`, `--no-run`, `--dry-run`, `--max-cost N`,
`--lock FILE`, `--encrypt-workspace`, `--engine docker`, `--no-toolchain`, `--no-firewall`,
`--no-overlay`, `--egress-allow <cidr,...>`, `--ttl DUR` / `--no-ttl`. Respects `NO_COLOR`.

## Platform support

| Target | Status |
|---|---|
| CLI on Linux | Built, unit-tested, and e2e-validated on real cloud |
| CLI on macOS | Built, unit-tested, and offline-smoke-tested on every push (macOS CI runner). The full cloud and overlay-join loop is validated on a real Mac with `scripts/mac_smoke.sh` |
| CLI on Windows | Built, unit-tested, and offline-smoke-tested in CI. Recommended path: WSL2 (native `ssh` and `wg-quick`). The native `.exe` works for provisioning, SSH, and debug; the overlay join uses the WireGuard Windows app |
| Provisioned nodes | Ubuntu Linux (by design — cloud-init, apt, nftables, systemd), whatever your operator OS |
| Providers | Hetzner Cloud, DigitalOcean, Vultr, Linode/Akamai, Scaleway (plus a `mock` provider for offline testing; AWS/GCP deferred) |

## Project status

Pandion is at **v0.5.0**. Hardened single nodes and multi-node IPC clusters run on all five
providers, native or containerized, with the full security posture, no-leak lifecycle, live cost
and budget controls, durable-run/`attach`, reproducibility, IDE and shared debugging, and an
encrypted Layer-2 overlay (`safe` and `lab` profiles). Every cloud-facing capability is proven on
real cloud by a self-cleaning end-to-end test (`scripts/`, see [`scripts/README.md`](scripts/README.md)),
and release artifacts are keyless-cosign-signed. See [`CHANGELOG.md`](CHANGELOG.md) for the full
history and [`TODO.md`](TODO.md) for what is ahead.

## Development

```bash
make ci                       # gofmt, go vet, go test -race, build (offline, no cloud)
go run ./cmd/pandion demo     # exercise the full lifecycle on the mock provider
bash scripts/ci_smoke.sh      # offline CLI smoke (also runs on the macOS/Windows CI runners)
```

Real-cloud, self-cleaning end-to-end tests live in `scripts/` (index and costs in
[`scripts/README.md`](scripts/README.md)); each provisions on your own account and tears
everything down on exit. Pandion also keeps a structured JSON audit trail of its own infra
actions at `~/.pandion/logs/audit.jsonl`; set `PANDION_LOG=debug|info|warn|error` to also stream
it to stderr.

Releases are automated by [GoReleaser](https://goreleaser.com): pushing a semver tag builds every
platform, generates checksums and SBOMs, keyless-cosign-signs the checksums, and publishes a
GitHub Release.

## Contributing

Pull requests are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). Contributions require agreeing
to the [CLA](CLA.md), which keeps Pandion's dual-licensing possible while you retain copyright of
your work.

## License

Pandion is free and open source under the [GNU Affero General Public License v3.0](LICENSE)
(AGPL-3.0). Run, study, modify, and share it freely; if you distribute a modified Pandion — or
offer one to others over a network — the AGPL asks you to share your changes under the same terms.
Running the CLI as-is against your own infrastructure carries no such obligation, at any scale.

Want to embed Pandion in a proprietary product or a modified hosted service without the AGPL's
source-sharing terms? A [commercial license](COMMERCIAL.md) is available. Contributions are made
under the [CLA](CLA.md), which keeps this dual-licensing possible while you retain copyright of
your work. © 2026 Yedidya Schwartz.
