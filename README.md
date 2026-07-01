<div align="center">

# ⚡ EnvCore

### Hardened, mesh-networked C++/IPC test clusters — one command up, zero trace down.

EnvCore is a single-binary CLI that provisions, **security-hardens**, and orchestrates
ephemeral remote dev/test environments for C++ and distributed-IPC workloads
(ZeroMQ, MQTT, …) on your own cloud account. No backend. No agents. No secrets leave your machine.

[![CI](https://github.com/yedidiaSch/envcore/actions/workflows/ci.yml/badge.svg)](https://github.com/yedidiaSch/envcore/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yedidiaSch/envcore?sort=semver)](https://github.com/yedidiaSch/envcore/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/yedidiaSch/envcore)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Platforms](https://img.shields.io/badge/binaries-linux%20%7C%20macOS%20%7C%20windows-informational)](#platform-support)

[Quickstart](#-quickstart) · [Features](#-features) · [Security](#-security-first) · [How it works](#-how-it-works) · [Commands](#-command-reference) · [Roadmap](#-project-status)

</div>

---

```console
$ envcore up --provider=hetzner -f cluster.yaml
UP cluster "pipeline": provisioning 2 hardened nodes (concurrent)...
  broker   ip=178.105.197.227  overlay=10.99.0.1
  worker   ip=188.245.229.15   overlay=10.99.0.2
forming WireGuard mesh (wg set peers)...
verifying mesh reachability...
  broker -> worker (10.99.0.2): OK
  worker -> broker (10.99.0.1): OK
injecting service discovery...   # each node learns $ENVCORE_<NODE>_IP
streaming 2 node command(s) (Ctrl+C detaches; logs: ~/.envcore/logs/pipeline)
----------------------------------------------------------------
[broker] listening on tcp://*:5557
[worker] connected to broker at 10.99.0.1:5557 over the overlay
[worker] processed batch 1
[broker] dispatched 1 result

$ envcore down --provider=hetzner --id pipeline
DOWN (hetzner): cluster "pipeline" reconciled to empty.   # nothing left billing
```

---

## Why EnvCore

Standing up a realistic multi-node C++ IPC test bed usually means yak-shaving: spin up VMs,
lock them down, install a toolchain, wire the network, hand-edit IPs, babysit logs, and
remember to delete everything before the bill lands. EnvCore collapses that into **one command
that is secure by default and leaves nothing behind.**

- **One command, whole cluster.** Describe a topology in `cluster.yaml`; get N provisioned,
  hardened, mesh-networked nodes with the toolchain installed and your commands streaming.
- **Security is the default, not a checklist.** Every node comes up with SSH host-key pinning,
  key-only auth, default-deny egress, and an encrypted WireGuard overlay — optionally
  **public-deny-all** so a scanner sees nothing.
- **No hardcoded IPs.** Nodes discover each other by name (`$ENVCORE_BROKER_IP`) and talk over
  the encrypted overlay.
- **No leaks, ever.** State is journaled, every resource is tagged, teardown is idempotent and
  reconciles against the provider — so an interrupted run or a lost laptop can't orphan a
  billing node.
- **Your money, your machine.** EnvCore is a local tool. It runs no service, stores no user
  credentials, and the only cloud bill is your own.

---

## ✨ Features

| | |
|---|---|
| 🚀 **One-command clusters** | `cluster.yaml` → concurrent provisioning + a synchronization **barrier** (all nodes ready before any command runs) |
| 🔐 **SSH host-key pinning** | Keys are generated locally and injected via cloud-init — **MITM/TOFU impossible**, never `StrictHostKeyChecking=no` |
| 🧱 **Default-deny firewall** | nftables egress lockdown (exfiltration-proof) with a build-window for the toolchain fetch |
| 🕸️ **WireGuard overlay + mesh** | Encrypted management + IPC plane; single-node or full node-to-node mesh |
| 🛡️ **Public deny-all** | Lockout-safe `envcore lockdown`: SSH becomes overlay-only, public scan sees just the WG port |
| 🧭 **Service discovery** | `$ENVCORE_<NODE>_IP` injected into every node — no hardcoded IPs, IPC rides the overlay |
| 📺 **Multiplexed output** | Color-coded, per-node-prefixed live streams, tee'd to per-node log files |
| ♻️ **No-leak lifecycle** | Journaled state, tag-based reconcile, idempotent teardown, Ctrl+C auto-rollback / detach-not-destroy |
| 🧰 **C++ toolchain, ready** | `gcc`/`clang`/`cmake`/`gdb`/`tmux` installed and verified before your command runs |
| 🧪 **Offline by default** | A `mock` provider runs the whole flow with no cloud and no cost — the CI backbone |

---

## 📦 Install

**Homebrew (macOS/Linux)**
```bash
brew install yedidiaSch/tap/envcore
```

**Debian / Ubuntu** — from the signed APT repo:
```bash
curl -fsSL https://yedidiaSch.github.io/envcore/gpg.key | sudo gpg --dearmor -o /usr/share/keyrings/envcore.gpg
echo "deb [signed-by=/usr/share/keyrings/envcore.gpg] https://yedidiaSch.github.io/envcore/deb stable main" | sudo tee /etc/apt/sources.list.d/envcore.list
sudo apt update && sudo apt install envcore
```

**RHEL / Fedora** — from the signed YUM repo:
```bash
sudo curl -fsSL https://yedidiaSch.github.io/envcore/envcore.repo -o /etc/yum.repos.d/envcore.repo
sudo dnf install envcore
```

Or grab a single `.deb`/`.rpm` from the [latest release](https://github.com/yedidiaSch/envcore/releases/latest) and `dpkg -i` / `rpm -i` it directly.

**Prebuilt archive** (linux/macOS/windows · amd64/arm64), shipped with `checksums.txt` and per-archive SBOMs:
```bash
tar -xzf envcore_<version>_<os>_<arch>.tar.gz && ./envcore version
```

**From source** (Go 1.22+):
```bash
go install github.com/envcore/envcore/cmd/envcore@latest
```

---

## 🚀 Quickstart

> **Prerequisite:** a project-scoped [Hetzner Cloud](https://console.hetzner.cloud) API token.
> ```bash
> export HCLOUD_TOKEN=your-token     # a leading space keeps it out of shell history
> ```

**Try it with zero cost first** — the `mock` provider runs the full lifecycle offline:
```bash
envcore demo
```

**A single hardened cloud box:**
```bash
envcore up --provider=hetzner --id build -- 'gcc --version && cmake --version'
# ... provisions, hardens, installs the toolchain, runs your command ...
envcore down --provider=hetzner --id build
```

**A multi-node IPC cluster** — write `cluster.yaml`:
```yaml
apiVersion: envcore/v1
name: pipeline
nodes:
  - name: broker
    run: ./build/broker --frontend tcp://*:5557
  - name: worker
    run: ./build/worker --connect tcp://$ENVCORE_BROKER_IP:5557   # discovered, over the overlay
```
```bash
envcore validate -f cluster.yaml          # schema-check before spending a cent
envcore up --provider=hetzner -f cluster.yaml --id pipeline
envcore down --provider=hetzner --id pipeline
```

**Go fully dark** (public deny-all, SSH over the overlay only):
```bash
sudo wg-quick up ~/.envcore/keys/pipeline/wg-pipeline.conf   # join the overlay
envcore lockdown --id pipeline                               # verifies overlay reach, THEN denies public
```

---

## 🔒 Security-first

EnvCore treats every cloud host as hostile until hardened, and hardens it **at provisioning
time** — there is no window in which a fresh node is exposed.

| Control | What it does |
|---|---|
| **Provision-time hardening** | Hardened `sshd` + keys injected via cloud-init `ssh_keys` — the host boots already locked down |
| **SSH host-key pinning** | EnvCore generates the host key, so it *knows* the fingerprint and pins it — MITM is rejected |
| **Key-only, root-scoped access** | Login key registered with the provider; password auth off |
| **Default-deny egress** | nftables output lockdown after the toolchain fetch — a compromised workload can't phone home |
| **WireGuard overlay** | All management + IPC on an encrypted plane; SSH can be bound to it entirely |
| **Public deny-all** | `envcore lockdown` removes public SSH after **verifying** overlay access — you can't lock yourself out |
| **No-secret telemetry** | Zero telemetry. Tokens/keys live only on your machine (never in remote config, argv, or logs) |
| **Ephemeral by default** | Fast create/destroy denies attackers persistence; teardown is verified and leak-free |

Design details, threat model, and the record of **9 findings that only surfaced against the
live cloud API** are documented in the companion design set (security architecture + risk
register).

---

## 🧠 How it works

EnvCore is a **stateful controller that spends real money** — so it's built around durable
state, idempotency, and reconciliation, not fire-and-forget scripts.

```
        your machine                                   Hetzner (your account)
  ┌───────────────────────┐                        ┌──────────────────────────┐
  │  envcore (one binary) │                        │   node: broker (Ubuntu)  │
  │                       │   1. CreateServer +     │   • pinned host key      │
  │  orchestrator ─┐      │──── cloud-init  ───────▶│   • toolchain + nftables │
  │  (state machine│      │                        │   • wg0  10.99.0.1        │
  │   + reconcile) │      │   2. pinned SSH         │           ▲              │
  │                │      │◀─── (over overlay) ────▶│           │ WireGuard    │
  │  provider ◀────┘      │                        │           ▼   mesh       │
  │  (hetzner | mock)     │   3. wg set peers,      │   node: worker (Ubuntu)  │
  │                       │      inject $ENVCORE_*  │   • wg0  10.99.0.2        │
  │  journaled state ─────┼── tag-based reconcile ─▶│   • $ENVCORE_BROKER_IP   │
  └───────────────────────┘   (source of truth)    └──────────────────────────┘
```

**Lifecycle:** `PLANNED → PROVISIONING → HARDENED → MESHED → READY → RUNNING → DESTROYED`, journaled
before/after every transition so any command is resumable. The provider (queried by tag) is the
source of truth for teardown — local state is only a cache.

Under the hood: a provider seam (`hetzner` + an offline `mock`), a concurrent orchestrator with a
readiness **barrier**, and a hardening pipeline of small, unit-tested packages
(`harden` → `overlay` → `firewall` → `discovery` → `stream`).

---

## 📖 Command reference

| Command | What it does |
|---|---|
| `envcore up [--provider mock\|hetzner] [--id ID] [flags] -- <cmd>` | Provision + harden + run a **single** node |
| `envcore up --provider hetzner -f cluster.yaml --id ID` | Provision a **multi-node** cluster + mesh |
| `envcore down [--provider …] --id ID` | Idempotent, verified teardown (reconciles by tag) |
| `envcore validate [-f cluster.yaml]` | Schema-check a topology (exit 2 with a precise pointer on error) |
| `envcore lockdown --id ID` | Lockout-safe public deny-all (SSH over the overlay only) |
| `envcore demo` · `envcore version` | Offline mock lifecycle · version |

Selected `up` flags: `--no-toolchain`, `--no-firewall`, `--no-overlay`,
`--egress-allow <cidr,...>`. Respects `NO_COLOR`.

---

## 💻 Platform support

| | Status |
|---|---|
| **CLI on Linux** | ✅ Built **and** e2e-validated on real cloud |
| **CLI on macOS / Windows** | ⚙️ Cross-compiled & released, **not yet validated** (see roadmap **M7**). Pure-Go core; the operator-side overlay join uses `wg-quick` (Linux/macOS) or the WireGuard app (Windows) |
| **Provisioned nodes** | 🐧 **Ubuntu Linux** (by design — cloud-init/apt/nftables/systemd) |
| **Providers** | Hetzner Cloud (a `mock` provider backs offline testing; DigitalOcean is planned) |

---

## 📈 Project status

**v0.1.0 — MVP.** The full core of the design is implemented and validated on real cloud:
hardened single nodes and multi-node IPC clusters, WireGuard mesh, service discovery,
multiplexed output, public deny-all, and no-leak teardown. Backed by offline CI and self-cleaning
real-cloud e2e scripts (`scripts/`).

**On the roadmap:**
`envcore attach` · macOS/Windows validation (M7) · LUKS-at-rest + least-privilege run user ·
native engine · DigitalOcean provider · live cost tracking + TTL dead-man's-switch ·
signed APT/YUM repos.

---

## 🛠️ Development

```bash
make ci                       # gofmt + go vet + go test -race + build   (offline, no cloud)
go run ./cmd/envcore demo     # exercise the full lifecycle on the mock provider

# real-cloud e2e (a few cents each, self-cleaning) — needs HCLOUD_TOKEN
scripts/e2e_m32b.sh           # 2-node cluster + WireGuard mesh
scripts/e2e_m33.sh            # service discovery + IPC over the overlay
scripts/e2e_m34.sh            # multiplexed output + per-node logs
```

Releases are automated by [GoReleaser](https://goreleaser.com): push a semver tag and the
pipeline builds every platform, generates checksums + SBOMs, and publishes a GitHub Release.
```bash
git tag v0.1.1 && git push origin v0.1.1
```

---

## 📄 License

[Apache-2.0](LICENSE) © EnvCore authors. See [NOTICE](NOTICE).

<div align="center">
<sub>Built with a spike-before-you-build, verify-before-you-merge discipline — every cloud-facing slice proven on real infrastructure before it shipped.</sub>
</div>
