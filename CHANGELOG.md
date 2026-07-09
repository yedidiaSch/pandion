# Changelog

All notable changes to Pandion. Format follows [Keep a Changelog](https://keepachangelog.com);
versions follow [SemVer](https://semver.org). Each released version's artifacts are
**keyless-cosign-signed** (verify per the README).

## [Unreleased]

### Added
- **`pandion build [dir]` — one-liner "upload this project and build it in the cloud"** — auto-
  detects the toolchain (CMake, Meson, Cargo, Go, npm, Python, or Make), syncs the directory, and
  builds it on a hardened node; add `-- <cmd>` to run the result, or omit it to build-only. It's a
  thin wrapper over `up` (way 2 / `sync: source`) — every `up` flag passes through, and detected
  knobs only fill what you didn't set. Non-C++ stacks skip the built-in C++ toolchain for a faster
  boot.
- **`up` honors `defaults.{region,size,ttl}` from `~/.pandion/config.yaml`** — a new `--size`
  and `--region` on `up` (joining `--ttl`) plus config seeding: an `up` that omits these fills
  them from your `pandion init` defaults (an explicit flag always wins; `--no-ttl` still
  disables the dead-man's-switch). `pandion init` now prompts for / accepts `--size` and
  `--ttl` alongside `--region`.

### Changed
- **`down` reads the provider from the cluster manifest** — teardown (`pandion down --id ID`)
  no longer needs `--provider`; it uses the provider recorded when the cluster was created,
  falling back to the `pandion init` default / credential inference (`--provider` still wins if
  given). `up` now records the owning provider in the manifest; `ls`/`reap` also honor the config
  default instead of defaulting to Hetzner.

## [0.6.0] — 2026-07-09

The headline is the **Relay** — self-hosted, zero-install browser terminals to your nodes — and
a **relicense to AGPL-3.0**, making Pandion genuinely open source. Plus a friendlier on-ramp
(`pandion init` + a defaults config), first-class dependency and prebuilt-binary handling, a
runnable example, deploy/run separation, a reliable Scaleway multi-node fix, and macOS/Windows CI
coverage. Every cloud-facing capability is proven on real cloud by a self-cleaning e2e; releases
are keyless-cosign-signed.

### Added
- **Relay — zero-install browser terminals to your nodes (`pandion relay …`)** — a self-hosted
  bridge that turns a clickable `https://` link into a live, in-browser shell on a Pandion node,
  with no WireGuard, SSH client, or install for the participant — built for cyber-ranges,
  training, CTF, and collaboration. `relay up` deploys a hardened, non-root `systemd` service on
  a node you designate (one public port; everything else stays default-deny); `relay share --node
  T` mints a scoped, expiring, revocable link to one node as a restricted non-root user; the
  participant opens it and the relay bridges a WebSocket to a host-key-pinned SSH PTY over the
  overlay (embedded xterm.js). The scoped key stays server-side; tokens are 256-bit, hash-looked-
  up (no oracle), and rate-limited. TLS is **browser-trusted via Let's Encrypt** with `--domain`,
  else self-signed with a printed fingerprint. Also: `--read-only` (view-only), `--record` +
  `relay recordings --fetch` (session capture), `relay unshare`/`status`. No Pandion service in
  the path — it runs on your own node. Every path proven live on DigitalOcean (incl. a real
  Let's Encrypt issuance validated with system roots). See `docs/pandion-relay.pdf`.
- **`pandion init` + a defaults config (`~/.pandion/config.yaml`) — bare one-liners that just
  work.** A first-run wizard picks a default provider, logs you in (token → OS keychain, with the
  disclosed signup pointer), and writes your defaults, so `pandion up -- ./app` needs no
  `--provider`. Provider resolution is now: `--provider` flag → config default → the only provider
  you have credentials for → (on a terminal) a prompt. Automation is unaffected — nothing ever
  prompts without a TTY; non-interactive callers get a clear error naming the flag/env to set.
  `init` is fully scriptable too (`--provider`/`--token`/`--region`). Verified offline (incl. the
  macOS/Windows CI smoke): `init` writes the config and a bare `up` resolves to it.
- **Architecture guard for prebuilt binaries** — when uploading with `sync.mode: binaries`,
  Pandion now checks each ELF binary's CPU architecture (parsed locally via `debug/elf`)
  against the node's (`uname -m`) and prints a loud warning naming any mismatch — turning a
  later, cryptic `Exec format error` into an actionable message (e.g. "this node is amd64,
  but you're uploading app (built for arm64); rebuild for linux/amd64"). Best-effort and
  non-blocking (source mode builds on the node, so it's unaffected). Verified by
  `scripts/e2e_binaries.sh` (a cross-built arm64 binary on an amd64 node trips the warning).
- **Deploy prebuilt binaries — `sync.mode: binaries` now works (+ `--sync-mode`)** — you can
  ship a prebuilt artifact to nodes as-is, with no remote build: `sync: { mode: binaries, path:
  ./dist }` in `cluster.yaml`, or `pandion up --workspace ./dist --sync-mode binaries -- ./dist/app`
  single-node. Unlike `source` mode, `binaries` does **not** apply `.gitignore` — so build output
  (`dist/`, `build/`) is uploaded rather than filtered out (`.pandionignore` still applies; `.git`
  is always excluded). Previously `mode: binaries` was silently a no-op (nothing synced). A new
  README "Running your code" section documents all three execution paths (a bare command, build a
  project on the node, upload a prebuilt binary) with the full `sync:`/`run:` options. Verified live
  by `scripts/e2e_binaries.sh` (DigitalOcean): a locally-built, gitignored binary ships and runs
  with no remote build.

### Changed
- **Relicensed from BSL 1.1 to AGPL-3.0 (open source), with a commercial dual-license.**
  Pandion is now free and open source under the **GNU Affero General Public License v3.0**
  (an OSI-approved license) instead of the source-available Business Source License. Running
  the CLI as-is against your own infrastructure carries no obligation, at any scale; the AGPL's
  copyleft applies only if you distribute a modified Pandion or offer a modified one over a
  network. A **commercial license** (see `COMMERCIAL.md`) remains available for embedding in
  proprietary products or closed hosted services — the CLA is retained so this dual-licensing
  stays possible. SPDX `AGPL-3.0-or-later` headers were added to all source files; the vestigial
  BSL change-license artifact was removed. Versions already published under BSL 1.1 keep their
  original terms (including their four-year Apache-2.0 conversion); everything from here is AGPL.

### Added
- **Runnable example — `examples/zmq-cluster` (broker + 2 workers)** — a ready-to-run
  ZeroMQ ventilator/worker cluster so a newcomer can see Pandion work on real cloud
  *before writing any code*: `cd examples/zmq-cluster && pandion up -f cluster.yaml`.
  It exercises the whole tool in one command — `libzmq3-dev` via `toolchain.packages`,
  workspace `sync` + `build: make`, service discovery (`$PANDION_BROKER_IP`,
  `$PANDION_SELF_NAME`), the WireGuard mesh, multiplexed streaming, and no-leak
  teardown — and you watch the load split across both workers. Kept honest by
  `scripts/e2e_example.sh` (verified live on DigitalOcean: 15/15 task split, clean
  exit). The workers check in on a readiness socket so the broker splits work
  reliably regardless of connect order (and any number of workers).
- **macOS/Windows CLI validation (M7, partial)** — CI now runs an **offline CLI smoke**
  (`scripts/ci_smoke.sh`) on the `macos-latest` and `windows-latest` runners in addition to
  build + unit tests: config validation, mock provisioning, dry-run pricing, and completion —
  proving the binary *runs* on each OS (path handling + state I/O), not just compiles. A new
  **`scripts/mac_smoke.sh`** covers the real-hardware bits on a Mac (the macOS Keychain, the
  openssh shell-out, and an opt-in real cloud + `wg-quick` overlay-join loop). Documented the
  operator platform story: **macOS is first-class native; on Windows use WSL2** (native `ssh`
  + `wg-quick`), with the native `.exe` supported for the core provision/SSH/debug flow.
- **`setup:` — install non-apt software declaratively** — a list of shell commands run
  on a node (as root) in the egress-open build window, **after** apt packages and **before**
  your build, so software apt can't install (pip/npm/cargo, a vendor repo, a curl'd binary)
  lands while the network is still reachable. In `cluster.yaml` per node or under `defaults:`
  (additive — defaults' setup runs first, then the node's); single-node via
  `--setup '<command>'`. Fail-fast: a failing setup command reports the error, leaves the
  node up for debugging, and exits non-zero so scripts/CI notice. Verified live by
  `scripts/e2e_setup.sh` (DigitalOcean): a real pip install + network fetch land on the node,
  and a failing setup command aborts `up`.
- **Install libraries on a node — `--packages`, additive `toolchain.packages`, and
  install verification** — declaring libraries is now first-class and safe. Single-node
  `up` gains **`--packages libzmq3-dev,libboost-dev`** (comma-separated apt packages), and
  a cluster's `toolchain.packages` (per node or under `defaults:`) now **adds to** the
  built-in C++ toolchain instead of replacing it — previously, declaring a library silently
  dropped `gcc`/`clang`/`cmake`/`gdb` (footgun). Set `toolchain.no_default: true` (or
  `--no-toolchain`) for a minimal node with only your packages. And a requested package that
  doesn't install (typo'd or unavailable name) — which cloud-init logs but boots past, so the
  node looks healthy while the library is missing — is now caught by a **loud warning at `up`
  time**, checked in the egress-open build window. Verified live by `scripts/e2e_packages.sh`
  (DigitalOcean): requested libs install, the built-in toolchain is preserved, and a bogus
  package name is reported.
- **Account-signup pointer for newcomers (with disclosed DigitalOcean referral)** — when
  a command needs a provider token but none is found, and on `pandion login` with no token
  entered, Pandion now points first-timers at how to open an account. For DigitalOcean this
  can use a **referral link** — shown only ever with a clear **affiliate disclosure**
  (*"referral link — helps support Pandion's development"*) and the sign-up credit, per FTC
  guidance and to keep trust. Until a referral code is configured it falls back to
  DigitalOcean's plain signup page with **no** referral claim. Other providers get no such
  pointer. Set the code via the `doRefcode` constant (`cmd/pandion/referral.go`) or the
  `PANDION_DO_REFCODE` env var. Purely a suggestion — nothing is sent anywhere.
- **Deploy-only nodes + `pandion start` — separate "deploy" from "run"** — `run:` is now
  **optional** in `cluster.yaml`: a node without it is *deployed* (provisioned + hardened +
  workspace synced + built) but nothing is launched. `up --no-run` does the same for a whole
  cluster or single node. Launch the workloads later, on demand, with **`pandion start --id ID
  [--node NAME] [--detach]`** — it works entirely from the persisted manifest (which now records
  each node's run spec), streams like `attach`, and skips deploy-only nodes with a helpful error
  if one is named explicitly. Good for staged cyber-ranges (stand up the nodes, start exercises
  when ready) and iterative dev (sync + build once, then run/re-run over SSH). Verified live by
  `scripts/e2e_deploy_start.sh` (DigitalOcean): `--no-run` launches nothing, `start` runs exactly
  the runnable node and skips the deploy-only one.

### Fixed
- **Scaleway multi-node clusters now provision reliably** — multi-node `up` used to
  time out with *"login key not yet on root"*: Scaleway received the login key only
  through the (large) cloud-init user-data, which did not land on root reliably at
  concurrent multi-node scale (the longer provisioning window did not fix it). The
  Scaleway provider now registers the login key as a **project-scoped IAM SSH key**
  before boot, so Scaleway's metadata datasource injects it into root early and
  independently of cloud-init timing; the key is deleted on teardown (`ReapAux`) so
  nothing leaks. Verified live by `scripts/e2e_scaleway_cluster.sh` (a 3-node cluster
  forms its mesh, root login succeeds on all three nodes, and the IAM key is reaped
  on `down`).

## [0.5.0] — 2026-07-06

An encrypted **Layer-2 overlay** for clusters — a real, isolated Ethernet broadcast
domain riding the WireGuard mesh — in two profiles: `safe` (spoof-resistant, Phase 1)
and `lab` (a deliberately attackable, contained cyber-range, Phase 2). Both were
proven on real cloud (DigitalOcean) by self-cleaning e2es, including a live
cross-node **MITM that the `safe` profile blocks and the `lab` profile allows —
while staying contained to the private overlay**.

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

[0.6.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.6.0
[0.5.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.5.0
[0.4.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.4.0
[0.3.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.3.0
[0.2.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.2.0
[0.1.0]: https://github.com/yedidiaSch/pandion/releases/tag/v0.1.0
