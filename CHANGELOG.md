# Changelog

All notable changes to Pandion. Format follows [Keep a Changelog](https://keepachangelog.com);
versions follow [SemVer](https://semver.org). Each released version's artifacts are
**keyless-cosign-signed** (verify per the README).

## [Unreleased]

### Added
- **L2 overlay `lab` profile — an attackable, isolated cyber-range (Phase 2)** —
  `security.overlay: { l2: { profile: lab } }` provisions the same encrypted L2
  segment but *deliberately attackable*: `rp_filter=0` + promiscuous mode on `vxlan0`
  and **no ARP inspection**, so ARP-spoof/MITM, Responder-style poisoning, etc. work —
  for authorized labs/training/CTF. It is **loud** (a boxed warning on `up`, an
  `L2-LAB` tag in `ls`, an audit event) and **contained**: attacks ride the private
  overlay only (never the provider LAN or the internet), and the management plane
  (`wg0`) stays fully hardened. Verified live by `scripts/e2e_l2_lab.sh` (real
  cross-node **MITM: victim poisoned + attacker intercepts traffic**, then asserts
  containment + `wg0` intact) — the exact mirror of the `safe` e2e where the same
  spoof is blocked. Lab requires an explicit `profile: lab` (default stays `safe`).
- **Encrypted Layer-2 overlay — `security.overlay: l2` (safe profile, Phase 1)** — an
  opt-in VXLAN-over-WireGuard segment gives a cluster a real, encrypted, isolated
  Ethernet broadcast domain on top of the L3 mesh. Because WireGuard carries no
  multicast, the orchestrator manages a static FDB (unicast MAC→VTEP + per-peer
  BUM-flood) injected at the same barrier that forms the WireGuard mesh; the `safe`
  profile adds host-side Dynamic ARP Inspection (an nftables `arp` table pinning each
  IP↔MAC binding) so the segment is **spoof-resistant**. Inner MTU is computed from
  the wg0 underlay (no black-holing); VXLAN egress is scoped to the overlay subnet
  (no internet hole); the management plane (`wg0`) stays fully hardened. One line in
  `cluster.yaml` (`security.overlay: l2`) — the operator never touches VXLAN/FDB/MAC.
  Verified live by `scripts/e2e_l2_safe.sh` (L2 reachability + dynamic ARP + large-MTU
  frame + a real cross-node ARP spoof **blocked** + `wg0` posture intact). The `lab`
  cyber-range profile (deliberately attackable) lands in Phase 2. Design +
  implementation plan in `docs/l2-overlay-design.pdf` / `docs/l2-overlay-implementation-plan.md`.
- **Shared debugging — `pandion debug share` / `join` / `unshare`** — hand a teammate
  **one token** that grants a scoped, expiring, revocable remote-debug attach to a running
  process, with no backend and without giving up root or your overlay. On the node the
  debugger is `gdbserver --once --attach - <PID>` (stdio), run as root **only** through a
  ForceCommand wrapper bound to one pinned PID; the guest's local gdb connects with `target
  remote | ssh …`, so the gdb protocol rides the host-key-pinned SSH channel — no open port,
  no forwarding, passes the deny-all firewall. gdbserver-as-root reads memory correctly yet
  only ever proxies that one non-root process (no shell, no other process, no code-exec as
  root; sharing a root/system PID is refused). The grant is a locked-down non-root
  `pandion-debug` user (`restrict` + native `expiry-time`) plus a **scoped WireGuard peer**
  (AllowedIPs = only the target node). `join` brings up the peer + writes `launch.json`;
  `unshare` (and `down`) revoke the key + pinned PID + peer; every step is audit-logged.
  `gdbserver` now ships in the default toolchain. **Verified live** by
  `scripts/e2e_debug_share.sh` (real symbolized backtrace over the guest grant; root-PID
  refused; post-`unshare` access gone).

### Changed
- **Longer cluster-provisioning window** for slow-boot providers — the multi-node
  readiness ceiling is 25 min (was 12). It only bounds failures; fast providers still
  finish early. (Note: this alone does not fix Scaleway multi-node clusters, whose
  login key is injected only via cloud-init and does not reliably land on root at
  scale — a deeper Scaleway-provider fix, tracked in TODO, is registering the SSH key
  with Scaleway IAM like the other providers.)

## [0.4.0] — 2026-07-05

Three more cloud providers (five total), a new **IDE Tier-2** debug-attach that
puts your local debugger on a remote process over the overlay, and the fixes those
first real-cloud runs shook out. Every cloud-facing capability is proven on real
cloud by a self-cleaning e2e; DigitalOcean, Vultr and Scaleway were verified live.

### Added
- **IDE Tier-2 — distributed debug-attach over the overlay (`pandion debug`)** — attach
  your **local** VS Code debugger to a **running process on a remote node**, across the
  mesh. Generates a `cppdbg` *attach* config that drives a remote `gdb` through the same
  host-key-pinned SSH pipe Pandion already uses (VS Code `pipeTransport`), pointed at the
  node's overlay IP — no new port, no `gdbserver`, no agent, and nothing installed on the
  node (`gdb` ships in the toolchain; root login bypasses ptrace limits). Merges into the
  project's `./.vscode/launch.json` (JSONC-tolerant, dedupes by name, preserves other
  configs) so F5 just works; `--public`, `--pid`, `--program`, `--print` flags. Backed by
  `scripts/e2e_debug.sh`, which proves the transport by driving remote `gdb` over the pinned
  pipe to a real workload PID and pulling a backtrace.
- **Three more providers — Vultr, Linode/Akamai and Scaleway** — behind the same
  `Provider` seam, each with spec-based size discovery, first-class tags, an exact
  live `Pricer`, and no-leak teardown (C4). Select with `--provider=vultr`,
  `--provider=linode`, or `--provider=scaleway`; store tokens via `pandion login`.
  Payment-flexibility motivation (H6 follow-up): Vultr (PayPal), Scaleway EU billing.
  - Vultr & Linode authenticate with a single API token (`VULTR_API_KEY`,
    `LINODE_TOKEN`). Scaleway uses an access-key + secret-key + project-id triple
    (`SCW_ACCESS_KEY` / `SCW_SECRET_KEY` / `SCW_DEFAULT_PROJECT_ID`); only the secret
    key is sensitive and stored in the keychain.
  - Scaleway teardown terminates the server **and** deletes its detached block
    volumes so nothing keeps billing after `down`.
  - Each backed by a self-cleaning real-cloud e2e: `scripts/e2e_vultr.sh`,
    `scripts/e2e_linode.sh`, `scripts/e2e_scaleway.sh`.

### Fixed
- **Vultr provisioning** rejected raw cloud-init (`400 Invalid user_data`) — the
  Vultr API requires `user_data` base64-encoded; the driver now encodes it.
- **e2e teardown could leak** — the scripts' teardown `down` lacked `--yes`, so run
  by hand (stdin is a TTY) it silently aborted and left resources billing. `--yes`
  added across all 21 e2e scripts; teardown is now unconditional.
- **Clearer credential errors** — Vultr's opaque `401 Unauthorized IP` now points to
  the API Access Control allowlist (add your IPv4+IPv6); Scaleway names exactly which
  of `SCW_ACCESS_KEY` / `SCW_DEFAULT_PROJECT_ID` is missing.

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

[0.4.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.4.0
[0.3.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.3.0
[0.2.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.2.0
[0.1.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.1.0
