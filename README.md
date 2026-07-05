<div align="center">

# ⚡ Pandion

### Hardened, mesh-networked C++/IPC test clusters — one command up, zero trace down.

Pandion is a single-binary CLI that provisions, **security-hardens**, and orchestrates
ephemeral remote dev/test environments for C++ and distributed-IPC workloads
(ZeroMQ, MQTT, …) on your own **Hetzner or DigitalOcean** account. No backend. No agents.
No secrets leave your machine.

[![CI](https://github.com/yedidiaSch/pandion/actions/workflows/ci.yml/badge.svg)](https://github.com/yedidiaSch/pandion/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yedidiaSch/pandion?sort=semver)](https://github.com/yedidiaSch/pandion/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/yedidiaSch/pandion)](go.mod)
[![License](https://img.shields.io/badge/license-BSL--1.1-blue.svg)](LICENSE)
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
| ☁️ **Five providers** | **Hetzner**, **DigitalOcean**, **Vultr**, **Linode/Akamai** and **Scaleway** behind one seam (+ an offline `mock`); spec-based size discovery, first-class tags, exact live pricing |
| 🔐 **Hardened by default** | SSH host-key pinning, key-only auth, default-deny egress, WireGuard overlay, **cloud-edge firewall**, metadata block, fail2ban, auditd, sysctl baseline, least-privilege run user, LUKS-at-rest — see [Security](#-security-first) |
| 🛡️ **Public deny-all** | Lockout-safe `pandion lockdown`: SSH becomes overlay-only, public scan sees just the WG port |
| 🧭 **Service discovery** | `$PANDION_<NODE>_IP` injected into every node — no hardcoded IPs, IPC rides the overlay |
| 📺 **Durable runs + attach** | Workloads run in tmux; `Ctrl+C` **detaches** without killing them, `pandion attach` reconnects to the live multiplexed streams |
| 💸 **Cost-aware** | `ls`/`status` live cost, `--dry-run` cost preview, `--max-cost` budget cap, `reap` orphan sweep |
| 🧪 **Reproducible** | Toolchain-version lockfile (`--lock`); **keyless-cosign-signed** releases + SBOMs |
| ♻️ **No-leak lifecycle** | Journaled state, tag-based reconcile, idempotent verified teardown, Ctrl+C auto-rollback |
| 🧰 **Native or Docker** | Polyglot toolchain on the host (`pandion-run`) or a hardened container — single node and per-node in clusters |
| 🔧 **Operator tooling** | `pandion ssh` / `pandion cp` (host-key-pinned), shell completion, offline `mock` provider |
| 🐞 **IDE debug-attach** | `pandion debug` attaches your **local** VS Code debugger to a **remote** process over the overlay — remote `gdb` driven through the pinned SSH pipe (no new port, no agent) |

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

**Verify the release** (keyless [Sigstore/cosign](https://docs.sigstore.dev/) signature — no key to trust, identity is pinned to this repo's release workflow):
```bash
# checksums.txt is cosign-signed; verify it, then let it vouch for every artifact.
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature   checksums.txt.sig \
  --certificate-identity-regexp '^https://github.com/yedidiaSch/pandion/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing   # verifies the file you downloaded
```

**From source** (Go 1.22+):
```bash
go install github.com/yedidiaSch/pandion/cmd/pandion@latest
```

---

## 🚀 Quickstart

> **Prerequisite:** a project-scoped API token for whichever provider you use —
> [Hetzner Cloud](https://console.hetzner.cloud), [DigitalOcean](https://cloud.digitalocean.com),
> [Vultr](https://my.vultr.com), [Linode/Akamai](https://cloud.linode.com) or
> [Scaleway](https://console.scaleway.com).
> ```bash
> export HCLOUD_TOKEN=your-token           # a leading space keeps it out of shell history
> # export DIGITALOCEAN_TOKEN=your-token   # for --provider=digitalocean
> # export VULTR_API_KEY=your-key          # for --provider=vultr
> # export LINODE_TOKEN=your-token         # for --provider=linode
> # Scaleway needs a triple (only the secret key is sensitive):
> # export SCW_SECRET_KEY=… SCW_ACCESS_KEY=… SCW_DEFAULT_PROJECT_ID=…
> ```
> Or store the token once in your **OS keychain** (macOS Keychain / libsecret / Windows
> Credential Manager) and skip the env var — it is never passed on the command line:
> ```bash
> pandion login --provider hetzner        # prompts (hidden), or reads $HCLOUD_TOKEN if set
> # also: --provider vultr|linode|scaleway  (Scaleway stores only the secret key)
> ```
> Resolution order is **env var first, then keychain** (so scripts/CI are unchanged).

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
| **Cloud-metadata block** | Egress to `169.254.169.254` dropped unconditionally — instance credentials can't be read even under a broad allowlist |
| **WireGuard overlay** | All management + IPC on an encrypted plane; SSH can be bound to it entirely |
| **Public deny-all** | `pandion lockdown` removes public SSH after **verifying** overlay access — you can't lock yourself out |
| **Cloud-edge firewall** | A provider firewall (SSH+WG+ICMP inbound only) in *front* of the host nftables; removed on teardown (no leak) |
| **Least privilege** | Workloads run as unprivileged `pandion-run` (or a hardened container); only declared capabilities added back |
| **fail2ban · auditd · sysctl** | SSH brute-force protection, an on-node audit trail, and a CIS-lite kernel network baseline — on by default |
| **LUKS-at-rest** | Opt-in `--encrypt-workspace`: encrypted workspace with an ephemeral RAM key — the disk yields only ciphertext |
| **Idle dead-man's-switch** | An abandoned node powers itself off (no SSH for the TTL window) |
| **Signed & no telemetry** | Keyless-cosign-signed releases + SBOMs. Zero telemetry; tokens/keys live only on your machine |

Design details, threat model, and the record of **9 findings that only surfaced against the
live cloud API** are documented in the companion design set (security architecture + risk
register).

---

## 🧠 How it works

Pandion is a **stateful controller that spends real money** — so it's built around durable
state, idempotency, and reconciliation, not fire-and-forget scripts.

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

**Lifecycle:** `PLANNED → PROVISIONING → HARDENED → MESHED → READY → RUNNING → DESTROYED`, journaled
before/after every transition so any command is resumable. The provider (queried by tag) is the
source of truth for teardown — local state is only a cache.

Under the hood: a provider seam (`hetzner`, `digitalocean`, `vultr`, `linode`, `scaleway`, and an offline `mock`), a concurrent
orchestrator with a readiness **barrier**, and a hardening pipeline of small, unit-tested packages
(`harden` → `overlay` → `firewall` → `discovery` → `stream`).

---

## 📖 Command reference

| Command | What it does |
|---|---|
| `pandion up [--provider mock\|hetzner\|digitalocean\|vultr\|linode\|scaleway] [--id ID] [flags] -- <cmd>` | Provision + harden + run a **single** node |
| `pandion up --provider … -f cluster.yaml --id ID` | Provision a **multi-node** cluster + mesh |
| `pandion attach --id ID` | Reconnect to a running cluster's live multiplexed streams |
| `pandion ssh --id ID [--node N] [--overlay] [-- CMD]` | Host-key-pinned SSH into a node |
| `pandion cp --id ID [--node N] SRC DST` | scp to/from a node (prefix a node path with `:`) |
| `pandion code --id ID [--node N] [--print]` | **IDE Tier-1:** pinned SSH config for VS Code Remote-SSH / JetBrains Gateway |
| `pandion debug --id ID [--node N] [--public] [--pid N] [--print]` | **IDE Tier-2:** attach your **local** debugger to a **remote** process over the overlay |
| `pandion ls` / `status [--json]` | List live clusters/nodes with uptime + **live cost** |
| `pandion reap [--older-than DUR] [--yes]` | Destroy orphaned Pandion nodes across clusters |
| `pandion down [--provider …] --id ID` | Idempotent, verified teardown (reconciles by tag) |
| `pandion validate [-f cluster.yaml]` | Schema-check a topology (exit 2 with a precise pointer on error) |
| `pandion lockdown --id ID` | Lockout-safe public deny-all (SSH over the overlay only) |
| `pandion completion bash\|zsh\|fish` | Shell completion script |
| `pandion demo` · `pandion version` | Offline mock lifecycle · version |

Selected `up` flags: `--dry-run` (preview plan + cost), `--max-cost N` (budget cap),
`--lock FILE` (pin toolchain versions), `--encrypt-workspace` (LUKS-at-rest),
`--engine docker`, `--no-toolchain`, `--no-firewall`, `--no-overlay`,
`--egress-allow <cidr,...>`, `--ttl DUR` / `--no-ttl`. Respects `NO_COLOR`.

---

## 💻 Platform support

| | Status |
|---|---|
| **CLI on Linux** | ✅ Built **and** e2e-validated on real cloud |
| **CLI on macOS / Windows** | ⚙️ Cross-compiled & released, **not yet validated** (see roadmap **M7**). Pure-Go core; the operator-side overlay join uses `wg-quick` (Linux/macOS) or the WireGuard app (Windows) |
| **Provisioned nodes** | 🐧 **Ubuntu Linux** (by design — cloud-init/apt/nftables/systemd) |
| **Providers** | **Hetzner Cloud**, **DigitalOcean**, **Vultr**, **Linode/Akamai** and **Scaleway** (a `mock` provider backs offline testing; AWS/GCP deferred) |

---

## 📈 Project status

**v0.3.0 — MVP complete and exceeded.** Hardened single nodes and multi-node IPC clusters on
**Hetzner, DigitalOcean, Vultr, Linode/Akamai and Scaleway**, native or containerized, with the full security posture, no-leak
lifecycle, live cost + budget controls, durable-run/`attach`, reproducibility, and operator
tooling (`ssh`/`cp`) — every cloud-facing capability proven on real cloud by a self-cleaning e2e
script (`scripts/`, see [`scripts/README.md`](scripts/README.md)). Release artifacts are
keyless-cosign-signed. See [`CHANGELOG.md`](CHANGELOG.md) for the full history.

**Still ahead:** macOS/Windows validation (M7) · off-node auditd log shipping · OS-keychain
secret store · structured audit log (`log/slog`) · more providers (AWS/GCP) — see [`TODO.md`](TODO.md).

---

## 🛠️ Development

Pandion keeps a **structured audit trail** of its own infra actions (provision,
teardown, lockdown, reap) as JSON lines at `~/.pandion/logs/audit.jsonl` — handy for
debugging a run after the fact. Set `PANDION_LOG=debug|info|warn|error` to also
stream it to stderr live (the human console output is otherwise unchanged).

```bash
make ci                       # gofmt + go vet + go test -race + build   (offline, no cloud)
go run ./cmd/pandion demo     # exercise the full lifecycle on the mock provider

# real-cloud e2e (self-cleaning; needs HCLOUD_TOKEN). Full index + costs:
#   scripts/README.md
scripts/e2e_ls_cost.sh        # ls/cost, --dry-run, --max-cost (mostly FREE)
scripts/e2e_network_hardening.sh   # cloud firewall + sysctl + no-leak teardown
```

Releases are automated by [GoReleaser](https://goreleaser.com): push a semver tag and the
pipeline builds every platform, generates checksums + SBOMs, **keyless-cosign-signs** the
checksums, and publishes a GitHub Release.
```bash
git tag v0.3.1 && git push origin v0.3.1
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
