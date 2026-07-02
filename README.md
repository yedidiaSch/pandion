<div align="center">

# ⚡ Pandion

### Hardened, mesh-networked C++/IPC test clusters — one command up, zero trace down.

Pandion is a single-binary CLI that provisions, **security-hardens**, and orchestrates
ephemeral remote dev/test environments for C++ and distributed-IPC workloads
(ZeroMQ, MQTT, …) on your own cloud account. No backend. No agents. No secrets leave your machine.

[![CI](https://github.com/yedidiaSch/pandion/actions/workflows/ci.yml/badge.svg)](https://github.com/yedidiaSch/pandion/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yedidiaSch/pandion?sort=semver)](https://github.com/yedidiaSch/pandion/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/yedidiaSch/pandion)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Platforms](https://img.shields.io/badge/binaries-linux%20%7C%20macOS%20%7C%20windows-informational)](#platform-support)

[Quickstart](#-quickstart) · [Features](#-features) · [Security](#-security-first) · [How it works](#-how-it-works) · [Commands](#-command-reference) · [Roadmap](#-project-status)

</div>

---

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

$ pandion down --provider=hetzner --id pipeline
DOWN (hetzner): cluster "pipeline" reconciled to empty.   # nothing left billing
```

---

## Why Pandion

Standing up a realistic multi-node C++ IPC test bed usually means yak-shaving: spin up VMs,
lock them down, install a toolchain, wire the network, hand-edit IPs, babysit logs, and
remember to delete everything before the bill lands. Pandion collapses that into **one command
that is secure by default and leaves nothing behind.**

- **One command, whole cluster.** Describe a topology in `cluster.yaml`; get N provisioned,
  hardened, mesh-networked nodes with the toolchain installed and your commands streaming.
- **Security is the default, not a checklist.** Every node comes up with SSH host-key pinning,
  key-only auth, default-deny egress, and an encrypted WireGuard overlay — optionally
  **public-deny-all** so a scanner sees nothing.
- **No hardcoded IPs.** Nodes discover each other by name (`$PANDION_BROKER_IP`) and talk over
  the encrypted overlay.
- **No leaks, ever.** State is journaled, every resource is tagged, teardown is idempotent and
  reconciles against the provider — so an interrupted run or a lost laptop can't orphan a
  billing node.
- **Your money, your machine.** Pandion is a local tool. It runs no service, stores no user
  credentials, and the only cloud bill is your own.

---

## ✨ Features

| | |
|---|---|
| 🚀 **One-command clusters** | `cluster.yaml` → concurrent provisioning + a synchronization **barrier** (all nodes ready before any command runs) |
| 🔐 **SSH host-key pinning** | Keys are generated locally and injected via cloud-init — **MITM/TOFU impossible**, never `StrictHostKeyChecking=no` |
| 🧱 **Default-deny firewall** | nftables egress lockdown (exfiltration-proof) with a build-window for the toolchain fetch |
| 🕸️ **WireGuard overlay + mesh** | Encrypted management + IPC plane; single-node or full node-to-node mesh |
| 🛡️ **Public deny-all** | Lockout-safe `pandion lockdown`: SSH becomes overlay-only, public scan sees just the WG port |
| 🧭 **Service discovery** | `$PANDION_<NODE>_IP` injected into every node — no hardcoded IPs, IPC rides the overlay |
| 📺 **Multiplexed output** | Color-coded, per-node-prefixed live streams, tee'd to per-node log files |
| ♻️ **No-leak lifecycle** | Journaled state, tag-based reconcile, idempotent teardown, Ctrl+C auto-rollback / detach-not-destroy |
| 🧰 **C++ toolchain, ready** | `gcc`/`clang`/`cmake`/`gdb`/`tmux` installed and verified before your command runs |
| 🧪 **Offline by default** | A `mock` provider runs the whole flow with no cloud and no cost — the CI backbone |

---

## 📦 Install

**Homebrew (macOS/Linux)**
```bash
brew install yedidiaSch/tap/pandion
```

**Debian / Ubuntu** — from the signed APT repo:
```bash
curl -fsSL https://yedidiaSch.github.io/pandion/gpg.key | sudo gpg --dearmor -o /usr/share/keyrings/pandion.gpg
echo "deb [signed-by=/usr/share/keyrings/pandion.gpg] https://yedidiaSch.github.io/pandion/deb stable main" | sudo tee /etc/apt/sources.list.d/pandion.list
sudo apt update && sudo apt install pandion
```

**RHEL / Fedora** — from the signed YUM repo:
```bash
sudo curl -fsSL https://yedidiaSch.github.io/pandion/pandion.repo -o /etc/yum.repos.d/pandion.repo
sudo dnf install pandion
```

Or grab a single `.deb`/`.rpm` from the [latest release](https://github.com/yedidiaSch/pandion/releases/latest) and `dpkg -i` / `rpm -i` it directly.

**Prebuilt archive** (linux/macOS/windows · amd64/arm64), shipped with `checksums.txt` and per-archive SBOMs:
```bash
tar -xzf pandion_<version>_<os>_<arch>.tar.gz && ./pandion version
```

**From source** (Go 1.22+):
```bash
go install github.com/pandion/pandion/cmd/pandion@latest
```

---

## 🚀 Quickstart

> **Prerequisite:** a project-scoped [Hetzner Cloud](https://console.hetzner.cloud) API token.
> ```bash
> export HCLOUD_TOKEN=your-token     # a leading space keeps it out of shell history
> ```

**Try it with zero cost first** — the `mock` provider runs the full lifecycle offline:
```bash
pandion demo
```

**A single hardened cloud box:**
```bash
pandion up --provider=hetzner --id build -- 'gcc --version && cmake --version'
# ... provisions, hardens, installs the toolchain, runs your command ...
pandion down --provider=hetzner --id build
```

**A multi-node IPC cluster** — write `cluster.yaml`:
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
pandion validate -f cluster.yaml          # schema-check before spending a cent
pandion up --provider=hetzner -f cluster.yaml --id pipeline
pandion down --provider=hetzner --id pipeline
```

**Go fully dark** (public deny-all, SSH over the overlay only):
```bash
sudo wg-quick up ~/.pandion/keys/pipeline/wg-pipeline.conf   # join the overlay
pandion lockdown --id pipeline                               # verifies overlay reach, THEN denies public
```

---

## 🔒 Security-first

Pandion treats every cloud host as hostile until hardened, and hardens it **at provisioning
time** — there is no window in which a fresh node is exposed.

| Control | What it does |
|---|---|
| **Provision-time hardening** | Hardened `sshd` + keys injected via cloud-init `ssh_keys` — the host boots already locked down |
| **SSH host-key pinning** | Pandion generates the host key, so it *knows* the fingerprint and pins it — MITM is rejected |
| **Key-only, root-scoped access** | Login key registered with the provider; password auth off |
| **Default-deny egress** | nftables output lockdown after the toolchain fetch — a compromised workload can't phone home |
| **WireGuard overlay** | All management + IPC on an encrypted plane; SSH can be bound to it entirely |
| **Public deny-all** | `pandion lockdown` removes public SSH after **verifying** overlay access — you can't lock yourself out |
| **No-secret telemetry** | Zero telemetry. Tokens/keys live only on your machine (never in remote config, argv, or logs) |
| **Ephemeral by default** | Fast create/destroy denies attackers persistence; teardown is verified and leak-free |

Design details, threat model, and the record of **9 findings that only surfaced against the
live cloud API** are documented in the companion design set (security architecture + risk
register).

---

## 🧠 How it works

Pandion is a **stateful controller that spends real money** — so it's built around durable
state, idempotency, and reconciliation, not fire-and-forget scripts.

```
        your machine                                   Hetzner (your account)
  ┌───────────────────────┐                        ┌──────────────────────────┐
  │  pandion (one binary) │                        │   node: broker (Ubuntu)  │
  │                       │   1. CreateServer +     │   • pinned host key      │
  │  orchestrator ─┐      │──── cloud-init  ───────▶│   • toolchain + nftables │
  │  (state machine│      │                        │   • wg0  10.99.0.1        │
  │   + reconcile) │      │   2. pinned SSH         │           ▲              │
  │                │      │◀─── (over overlay) ────▶│           │ WireGuard    │
  │  provider ◀────┘      │                        │           ▼   mesh       │
  │  (hetzner | mock)     │   3. wg set peers,      │   node: worker (Ubuntu)  │
  │                       │      inject $PANDION_*  │   • wg0  10.99.0.2        │
  │  journaled state ─────┼── tag-based reconcile ─▶│   • $PANDION_BROKER_IP   │
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
| `pandion up [--provider mock\|hetzner] [--id ID] [flags] -- <cmd>` | Provision + harden + run a **single** node |
| `pandion up --provider hetzner -f cluster.yaml --id ID` | Provision a **multi-node** cluster + mesh |
| `pandion down [--provider …] --id ID` | Idempotent, verified teardown (reconciles by tag) |
| `pandion validate [-f cluster.yaml]` | Schema-check a topology (exit 2 with a precise pointer on error) |
| `pandion lockdown --id ID` | Lockout-safe public deny-all (SSH over the overlay only) |
| `pandion demo` · `pandion version` | Offline mock lifecycle · version |

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
`pandion attach` · macOS/Windows validation (M7) · LUKS-at-rest + least-privilege run user ·
native engine · DigitalOcean provider · live cost tracking + TTL dead-man's-switch ·
signed APT/YUM repos.

---

## 🛠️ Development

```bash
make ci                       # gofmt + go vet + go test -race + build   (offline, no cloud)
go run ./cmd/pandion demo     # exercise the full lifecycle on the mock provider

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

## 🤝 Contributing

PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). Contributions require
agreeing to the [CLA](CLA.md) (it keeps Pandion's dual-licensing possible while
you retain copyright of your work).

## 📄 License

Pandion is **source-available** under the [Business Source License 1.1](LICENSE):
**free** for individuals, non-commercial use, small organizations (< $1M/yr), and
internal evaluation/development/testing by anyone. Larger organizations using it
commercially need a [commercial license](COMMERCIAL.md). Each version becomes
**Apache-2.0** four years after its release. © 2026 Yedidya Schwartz.

<div align="center">
<sub>Built with a spike-before-you-build, verify-before-you-merge discipline — every cloud-facing slice proven on real infrastructure before it shipped.</sub>
</div>
