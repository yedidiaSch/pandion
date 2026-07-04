# Changelog

All notable changes to Pandion. Format follows [Keep a Changelog](https://keepachangelog.com);
versions follow [SemVer](https://semver.org). Each released version's artifacts are
**keyless-cosign-signed** (verify per the README).

## [0.3.0] — 2026-07-04

A second cloud provider, a full security-hardening pass, operator tooling, and
supply-chain signing. Every cloud-facing change was proven on real Hetzner via a
self-cleaning e2e script.

### Added
- **DigitalOcean provider** — a 2nd backend proving the `Provider` seam, incl. an
  exact `Pricer` (droplet sizes carry hourly price). Select with `--provider=digitalocean`.
- **`pandion ssh`** and **`pandion cp`** — host-key-pinned shell + file transfer to
  any node via the persisted manifest (the supported "left up for GDB/SSH" path).
- **`pandion ls` / `status`** — live fleet view with uptime + real per-node cost;
  `--json` for automation.
- **`pandion completion bash|zsh|fish`** — shell completion.
- **`up --dry-run`** — preview the plan + projected cost, creating nothing.
- **`up --max-cost`** — projected-total budget preflight (Σ hourly × idle-TTL);
  refuses to provision over budget (fail-closed).
- **Reproducibility lockfile (H2)** — `up` records resolved toolchain versions to
  `~/.pandion/lock/<id>.json`; `up --lock <file>` pins them.
- **Cloud-metadata block (S-F)** — egress to `169.254.169.254` dropped unconditionally.
- **Provider cloud-edge firewall (M8)** — Hetzner firewall (SSH+WG+ICMP inbound only)
  in front of host nftables; removed on teardown (no leak).
- **fail2ban** (SSH brute-force protection), **auditd** on-node audit trail (S-F),
  and a **sysctl** network-hardening baseline (WireGuard-safe) — on by default.
- **LUKS-at-rest (S-E)** — opt-in `--encrypt-workspace` / `security.encrypt_volumes`,
  ephemeral RAM key (disk yields only ciphertext).
- **Per-node `engine: docker` in clusters** — completes docker support (`container_image`).
- **Keyless cosign signing** of release checksums (Sigstore OIDC) + verify instructions.

### Fixed
- `ls` region column was empty after Hetzner dropped the deprecated `datacenter`
  API field (2026-07-01) — now reads `Server.Location`.
- Cloud firewall could leak on teardown (Hetzner rejects deleting a firewall while
  a server is still applied) — `ReapAux` now detaches resources, then deletes.

### Changed
- `up` (single node and cluster) now run **durably in tmux** — `Ctrl+C` detaches
  without killing the workload; `pandion attach` reconnects to the live streams.
- All previously parsed-but-ignored `cluster.yaml` fields are wired (P0-2 complete):
  `egress_allow`, and the `security:` overrides with `defaults:` inheritance.

## [0.2.0] — 2026-07-02

- Renamed **EnvCore → Pandion**; relicensed to **BSL 1.1** (source-available) + CLA.
- Ran workloads as an unprivileged **`pandion-run`** user (S-C) with **capability
  add-back** (`needs_caps`/`privileged_ports`).
- **Hardened Docker engine** (`--engine=docker`): cap-drop, no-new-privileges,
  read-only rootfs, no docker.sock.
- **Idle dead-man's-switch** (on-node TTL poweroff) and **`pandion reap`**
  (client-side orphan sweep) for billing-leak prevention.
- **Workspace sync** + remote build; applied `cluster.yaml` node settings
  (size/image/region/toolchain).
- Multi-node clusters + WireGuard mesh, service discovery, multiplexed output,
  lockout-safe public deny-all (`lockdown`).

## [0.1.0] — 2026-07-01

- Initial MVP: hardened single-node provision → run → verified teardown on Hetzner;
  SSH host-key pinning, default-deny egress build-window, WireGuard overlay.
- Release pipeline: GoReleaser, signed APT/YUM repos, Homebrew cask, `.deb`/`.rpm`/
  `.apk`, per-archive SBOMs, cross-compiled binaries.

[0.3.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.3.0
[0.2.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.2.0
[0.1.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.1.0
