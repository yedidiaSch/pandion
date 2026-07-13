# TODO — Activation checklist

Status verified 2026-07-13. Most channels are **already live**; the one real gap is
that the hosted apt/yum repo is serving the wrong project's packages (see §3).

---

## 1. Make the repo public — ✅ DONE
Repo is **public**, and releases up to **v0.7.1** are published with signed assets
(`.deb`/`.rpm`/`.apk` × amd64/arm64, tar.gz/zip, SBOMs, cosign `.sig`+`.pem`). README
badges render.

---

## 2. Homebrew  →  `brew install yedidiaSch/tap/pandion` — ✅ DONE
`HOMEBREW_TAP_GITHUB_TOKEN` is set; the `pandion.rb` cask is published in
`yedidiaSch/homebrew-tap` and the release workflow pushes it on each new tag.

---

## 3. APT/YUM  →  `apt install pandion` / `dnf install pandion` — ⚠️ NEEDS ONE RUN
Infrastructure is live: repo public, `GPG_PRIVATE_KEY` set, Pages built from
`gh-pages`, `gpg.key` served over HTTPS. **But the apt/yum index currently serves
`envcore`, not `pandion`** — the last publish dispatch (2026-07-09) ran with the old
default input `v0.1.0`, and this repo's `v0.1.0` is a pre-rename **envcore** release,
so it published envcore's packages.

Fixed in the workflow (this branch): the dispatch input now defaults to **`latest`**
(resolved at run time) instead of the stale `v0.1.0`, and a guard **fails the run** if
the release contains any non-`pandion_*` package. To go live:

1. Cut/confirm a real pandion release exists (v0.7.1 already does; or tag the next one
   — see §5).
2. Re-run the publisher against it:
   ```bash
   gh workflow run packages-repo.yml -R yedidiaSch/pandion -f tag=latest
   ```
   (The `release: published` event does **not** auto-trigger this — GitHub suppresses
   workflow triggers from goreleaser's default `GITHUB_TOKEN` — so this dispatch is
   required after each release, or wire a `workflow_run` chain / PAT later.)
3. Verify pandion (not envcore) is now indexed:
   ```bash
   curl -fsSL https://yedidiaSch.github.io/pandion/deb/dists/stable/main/binary-amd64/Packages | grep '^Package:'
   ```
4. Smoke-test on a clean Debian/Ubuntu box:
   ```bash
   curl -fsSL https://yedidiaSch.github.io/pandion/gpg.key | sudo gpg --dearmor -o /usr/share/keyrings/pandion.gpg
   echo "deb [signed-by=/usr/share/keyrings/pandion.gpg] https://yedidiaSch.github.io/pandion/deb stable main" | sudo tee /etc/apt/sources.list.d/pandion.list
   sudo apt update && sudo apt install pandion && pandion version
   ```

- [ ] Publish workflow re-run against a real pandion release (`tag=latest`)
- [ ] apt/yum index lists `pandion` (not `envcore`)
- [ ] `apt install pandion` works on a clean box
- [ ] `dnf install pandion` works on a clean box

---

## 4. Add the DigitalOcean referral code  →  turns the signup pointer into a referral
The signup-suggestion flow is wired and disclosed (shown on a missing token / empty
`pandion login`). Until a code is set it points at DigitalOcean's plain signup page
with **no** referral claim.

1. Get your DigitalOcean referral code (Account ▸ Refer & Earn / affiliate program).
2. Set it: edit `doRefcode` in `cmd/pandion/referral.go` (or ship `PANDION_DO_REFCODE`).
3. Verify: `env -u DIGITALOCEAN_TOKEN PANDION_DO_REFCODE=<code> pandion up --provider=do --id x -- true`
   should print the `?refcode=<code>` URL **with** the "referral link" disclosure.

- [ ] Referral code obtained
- [ ] `doRefcode` set (or `PANDION_DO_REFCODE` shipped) and disclosure verified

---

## Notes / caveats
- **Signing key:** fingerprint `8A19045A2466ED368AA23868988E9FA033620E2F`
  (`Pandion Packages`). Private half is the `GPG_PRIVATE_KEY` secret; rotate per
  `packaging/README.md`.
- **macOS/Windows CLI** is built, unit-tested and **offline-smoke-tested in CI** on both
  OSes every push; the real-hardware bits are covered by `scripts/mac_smoke.sh` (run once on
  a Mac). Windows operators: **use WSL2**. Full details in the README "Platform support".
- After all three are green, every channel is live: `brew` · `apt` · `dnf` ·
  direct download · `go install`. Then delete this section.

---

# Remaining implementation backlog (from the design & plan)

What the MVP (v0.1.0) delivered vs. what the design (`../plan/`) still calls for.
Grouped by priority. IDs reference the design review findings / roadmap milestones.

> **Read this first — the one gap that blocks real use:**
> **Workspace sync is not implemented (P0).** Nodes run your `run:` command, but
> Pandion never copies your source/binaries to them. Today only commands that need
> no local code work (the e2e used `echo`/`ping`). A real C++ workload needs the
> workspace synced (and built) on the node first. Until P0-1 lands, `run:
> ./build/app` assumes `./build/app` already exists on the node — it won't.

## P0 — makes it usable for real C++/IPC workloads
- [ ] **Workspace synchronization** (design §4, H3, L5) — rsync-over-SSH of the
      local workspace to each node before running; `--sync=source` (build remotely)
      vs `--sync=binaries` (validate target arch/libc, H3); `.pandionignore`
      (fallback `.gitignore`). *Without this the tool can't run user code.*
- [~] **Apply `cluster.yaml` fields that are currently parsed-but-ignored** —
      wired: `size`/`image`/`region`/`ttl`/`sync`/`toolchain`/`needs_caps`/
      `privileged_ports` (earlier), and now **`egress_allow`** (node + `security:` +
      defaults union → per-node firewall) and the **`security:` overrides**
      (`block_metadata_service`, `audit_log`, `run_as`) with `defaults:` inheritance
      via `config.Effective`. `ipc_ports` intentionally NOT opened publicly — IPC
      rides the encrypted overlay (all wg0 traffic is already accepted); public
      exposure would be a security regression. **P0-2 complete** — per-node `engine:
      docker` + `container_image` now wired for clusters too. *(this branch)*
- [x] **Docker engine** (`--engine=docker`, spec §2) — hardened container
      (cap-drop, no-new-privileges, read-only rootfs, no docker.sock). Single-node
      (#24) AND per-node in clusters (`engine: docker` + `container_image`, this
      branch): docker.io installed, image pulled in the build window, workload run
      via `dockerRun` in the durable tmux.
- [ ] **Local target** (`--target=local`, spec §2) — run on the workstation; keep
      the `local+native` guardrail (`--allow-local-native`, Linux-only, H4).

## P1 — security hardening the design promises (M1.x + security architecture)
- [x] **Least-privilege run user** (S-C) — dedicated `pandion-run` user, dropped
      caps, add-back only declared `needs_caps`/`privileged_ports`. *(done: #21, #25)*
- [x] **Encrypted volumes at rest** (LUKS, S-E) — opt-in (`--encrypt-workspace` /
      `security.encrypt_volumes`): a LUKS2 volume with an EPHEMERAL tmpfs (RAM) key
      is mounted at the run user's workspace, so synced code + build artifacts are
      encrypted at rest and the disk yields only ciphertext (unrecoverable after
      reboot — fine for ephemeral nodes). *(done: this branch)*
- [x] **Cloud metadata block** (`169.254.169.254`, S-F) — unconditional egress drop
      of the metadata endpoint, placed BEFORE any egress-allow, so a compromised
      workload can't read instance credentials even if the operator opens a broad
      allowlist. In `up` (single + cluster) and `lockdown`. *(done: this branch)*
- [~] **auditd** (S-F) — installed with a baseline ruleset (identity files, sshd
      config, priv-esc binaries) + enabled on every node by default; honors
      `security.audit_log`. Off-node, tamper-evident log SHIPPING still TODO (the
      on-node trail lands now). *(done: this branch)*
- [~] **Secret keychain** (H6) — provider **API tokens** in the OS keychain
      (macOS Keychain / libsecret / Windows Cred Manager) via `pandion login`/`logout`
      (`internal/secret` on go-keyring); resolution is env-first then keychain, no
      token in argv, graceful on headless. *(done: this branch)* SSH keys stay `0600`
      files for now (moving them to the keychain is a larger follow-up).
- [x] **Provider-level Cloud Firewall** (M8) — per-cluster Hetzner firewall (label
      selector → auto-applies to every node) allowing only SSH + WireGuard + ICMP
      inbound, in front of host nftables; created on `up`, deleted on teardown via
      ReapAux (no leak). Provider seam `ClusterFirewaller`. *(done: this branch)*
- [x] **Kernel/sysctl network hardening** — CIS-lite baseline (loose rp_filter for
      WireGuard-safety, ignore ICMP redirects / source routing, SYN-cookies, log
      martians), applied at boot. *(done: this branch)*
- [~] **fail2ban** as secondary defense (M7-review) — installed + sshd jail
      enabled (systemd backend) on every node by default. *(done: this branch)*
      `unattended-upgrades` on longer-lived nodes still TODO (skipped to avoid
      apt-lock contention during the provisioning window).
- [x] **Reproducibility** (H2) — `up` records the resolved toolchain versions
      (per node: package versions + OS/kernel) to `~/.pandion/lock/<id>.json`;
      `up --lock <file>` pins them so a re-provision reproduces the environment.
      Single-node + cluster. Best-effort (declared pkgs, not transitive deps; needs
      the versions still in-mirror). *(done: this branch)*
- [x] **Signed releases** — keyless cosign (Sigstore OIDC) signs `checksums.txt`
      → `checksums.txt.sig` + `.pem` on each release; `id-token: write` + cosign
      installed in the release workflow; verify snippet in the README. *(done: this
      branch; takes effect on the next `vX.Y.Z` tag.)*

## P2 — lifecycle & cost (roadmap M4)
- [x] **TTL dead-man's-switch** (C4/A5) — server-side systemd idle-timeout that
      powers a node OFF with no heartbeat (an abuse/liveness brake). Note: power-off
      is not teardown — a stopped node keeps billing until `pandion down`/`reap`, so
      `reap` is the real leak-stopper when the laptop dies. `--ttl`, `--no-ttl`.
      *(done: #23; F2 copy corrected)*
- [x] **`pandion ls` / `status`** (L1) — list active clusters, nodes, uptime, and
      **live cost** (provider.Pricer seam; grouped over the reconcile source of
      truth). *(done: this branch)*
- [x] **`pandion reap`** (C4) — sweep orphaned tagged resources across all clusters
      (today `down --id` reconciles one cluster). *(done: #22)*
- [x] **`pandion attach`** (L6) — reconnect to a running cluster's multiplexed
      streams; workloads run in tmux so detach != destroy, and crashes stay visible
      across reattach. Covers both single-node and cluster paths. *(done: #27)*
- [x] **Budget controls** (L2) — `--max-cost` projected-total preflight (Σ hourly ×
      TTL; fail-closed; `--no-ttl` ⇒ unbounded ⇒ error) + idle auto-stop on the TTL.
      *(done: this branch)* → **P2 complete.**

## P3 — providers, portability, polish
- [~] **DigitalOcean provider** (M6) — prove the `Provider` seam with a 2nd backend.
      *Implemented (this branch):* full `Provider` + `Pricer` + `AuxReaper` via `godo`
      (tag-based reconcile, size discovery by spec, exact per-size pricing); wired into
      `up`/`down`/`ls`/`reap`/`validate` and the hardened `up` flow (now provider-agnostic).
      Unit-tested; `scripts/e2e_digitalocean.sh` added. **Pending live e2e** (the DO
      hardened-provision path is unverified without a `DIGITALOCEAN_TOKEN`).
- [~] **Vultr / Linode / Scaleway providers** (H6 payment-flexibility follow-up) —
      three more backends behind the same `Provider` seam, taking Pandion to **five
      providers + mock** (PR #48). *Implemented:* full `Provider` + live `Pricer` each
      (spec-based size discovery, first-class tags, no-leak teardown). Vultr adds
      `AuxReaper` (login-key cleanup); Linode installs the login key inline
      (`AuthorizedKeys`) and filters regions to the `Metadata` capability for
      cloud-init; Scaleway is zone-based with a two-phase boot and terminates **plus
      deletes detached block volumes** so nothing keeps billing (C4). Vultr/Linode
      use a single token (`VULTR_API_KEY` / `LINODE_TOKEN`); Scaleway uses the
      `SCW_SECRET_KEY` + `SCW_ACCESS_KEY` + `SCW_DEFAULT_PROJECT_ID` triple (only the
      secret key is keychain-stored). Unit-tested; `scripts/e2e_{vultr,linode,scaleway}.sh`
      added. **Pending live e2e** — the hardened-provision paths are unverified until
      each account's payment method clears and a token is available. Likely first-run
      tweaks: image labels, Scaleway commercial-type/volume pairing, zone defaults.
- [~] **IDE Tier-2 — distributed debug-attach over the overlay** (the deliberate moat bet).
      *Implemented:* `pandion debug` generates a VS Code `cppdbg` attach config that drives a
      remote `gdb` through the pinned SSH pipe (`pipeTransport`) at the node's overlay IP —
      no new port/gdbserver/agent, nothing installed on the node (root login bypasses ptrace).
      Merges into `./.vscode/launch.json` (JSONC-tolerant, dedupes, preserves other configs).
      Unit-tested; `scripts/e2e_debug.sh` proves the transport (remote gdb attaches to a real
      workload PID over the pinned pipe and prints a backtrace) — **verified live** on Hetzner.
      Tier-1 (`pandion code`, Remote-SSH) already shipped.
    - **Shared debugging** (`pandion debug share`/`join`/`unshare`) — send a teammate one
      token for a scoped, expiring, revocable remote-debug attach. On the node: `gdbserver
      --attach` (stdio) run as root ONLY via a ForceCommand wrapper bound to one pinned
      non-root PID; guest gdb connects `target remote | ssh` (pinned, no port). Non-root
      `pandion-debug` grant + scoped WG peer; revoked by `unshare`/`down`; audited. **Verified
      live** — `scripts/e2e_debug_share.sh`: real symbolized backtrace over the guest grant,
      root-PID refused, post-`unshare` access gone. Remaining: a **GUI smoke test** with real
      VS Code over the overlay. Next: a relay for a zero-install clickable URL, collaborative
      same-session (shared-tmux / Delve multi-client), `--lang` for Python/Go.
- [x] **Encrypted Layer-2 overlay** (`security.overlay: l2`) — VXLAN-over-WireGuard broadcast
      domain; orchestrator-managed static FDB (unicast + BUM flood) injected at the mesh
      barrier; auto inner-MTU; VXLAN egress scoped to the overlay subnet; management plane
      stays hardened. **Both profiles done + live-verified.**
    - **safe** (`scripts/e2e_l2_safe.sh`): reachability, dynamic ARP, large-MTU frame, host
      DAI blocks a real cross-node ARP spoof, wg0 intact. Cross-provider **stress harness**
      (`scripts/l2agent.py` + `scripts/e2e_l2_stress.sh`) — unicast full-mesh, broadcast/
      multicast fan-out, MTU boundary, every-node spoof matrix, isolation, concurrent flood —
      5 nodes on DigitalOcean + Vultr, 3 on Hetzner.
    - **lab** (attackable cyber-range, `scripts/e2e_l2_lab.sh`): rp_filter=0/promisc, no DAI,
      loud warning + `ls` L2-LAB tag + audit; e2e proves **MITM works** (victim poisoned +
      attacker intercepts) and is **contained**; wg0 intact. Verified on DigitalOcean.
      Later: gateway topology, container/VM nested MACs, multicast-app validation, a wider
      cross-provider lab sweep.
- [x] **Scaleway multi-node provisioning** — was timing out with "login key not yet on
      root" (Scaleway injected the login key ONLY via cloud-init user-data, which didn't land
      reliably at concurrent multi-node scale; the longer window didn't fix it). **Fixed:** the
      Scaleway provider now registers the login key as a **project-scoped IAM SSH key** before
      boot (`ensureLoginKey`, idempotent across nodes) so Scaleway's metadata datasource injects
      it into root early, independent of cloud-init timing; `ReapAux` deletes the key on teardown
      (no leak). **Live-verified** by `scripts/e2e_scaleway_cluster.sh` — a 3-node cluster comes
      up + mesh forms, root login succeeds on all 3/3 nodes, and the IAM key is reaped on `down`.
- [~] **macOS/Windows CLI validation** (M7) — CI now builds, unit-tests **and offline-smoke-
      tests** (`scripts/ci_smoke.sh`) the CLI on `macos-latest` + `windows-latest` every push
      (config validation, mock provisioning, dry-run, completion — path handling + state I/O).
      `scripts/mac_smoke.sh` validates the real-hardware bits on a Mac (Keychain, ssh, and an
      opt-in cloud + `wg-quick` overlay loop). **Windows: WSL2 is the recommended operator
      path** (native `ssh` + `wg-quick`); the native `.exe` works for provision/SSH/debug, the
      overlay join uses the WireGuard app. Remaining: run `mac_smoke.sh --cloud` on a real Mac
      once; optionally `wireguard-go` (userspace) so the operator side needs no admin install.
- [x] **`--dry-run`** (L4) — preview the plan **+ projected cost** (per-node size/region/
      TTL, rolled-up hourly & over-TTL spend) and exit; creates nothing. Works on any
      pricing provider incl. mock (offline). *(done: this branch)*
- [x] **Structured logging / audit trail** (L3) — `internal/audit` on `log/slog`
      emits a JSON trail of Pandion's own infra actions (provision, up.complete,
      down, lockdown, reap) to `~/.pandion/logs/audit.jsonl`; `PANDION_LOG` also
      tees it to stderr. Human console + workload streams unchanged. *(done: this branch)*
- [ ] **Config precedence + profiles** (CLI spec) — `flags > env > cluster.yaml >
      ~/.pandion/config.yaml > defaults`; named credential/config profiles.
- [ ] **Shell completion** (`pandion completion …`), richer `--help` examples.

## Explicitly NOT planned (don't "implement" these)
- **Auto-restart of crashed processes** — fail-fast is a deliberate design choice
  (§5): freeze, alert, leave the node up for GDB. Do not add a supervisor.
- **AWS / GCP providers** — deferred until there's a concrete need (see
  `pandion-provider-comparison.md`); the `Provider` seam is proven across **five
  backends** (Hetzner, DigitalOcean, Vultr, Linode/Akamai, Scaleway), so adding
  another is a self-contained driver + one `newProvider` case + an e2e script.
- **Non-Ubuntu / non-Linux nodes** — provisioned environments are Ubuntu-by-design.

---

### Suggested order
Done so far: P0-1 workspace sync (#19), P0-2 cluster.yaml fields — partial (#20),
Docker engine (#24), P1 least-privilege run user + capability add-back (#21, #25),
**all of P2 (M4)**: reap (#22) / TTL (#23) / attach (#27) / ls-status + `--max-cost`
(this branch). The pre-MVP lifecycle+cost story is complete.

Next up (the live frontier):
1. **DigitalOcean provider (M6, P3)** — proves the `Provider` seam with a 2nd
   backend and (see internal strategy notes) unlocks a recurring affiliate channel.
   Also the natural home for a provider-specific Pricer alongside Hetzner's.
2. **Finish P0-2** (remaining cluster.yaml fields: `ipc_ports`, `needs_caps`,
   `egress_allow`, `security:` overrides, `defaults:` inheritance) — completeness.
3. Remaining **P1 security hardening** (LUKS-at-rest, metadata block, auditd,
   secret keychain, signed releases) as the security story demands.
Then M7 cross-platform validation before a 1.0.
