# TODO — Activation checklist

Everything is built and merged. These are the **manual steps only you can do** to
switch the remaining install channels live. Nothing here needs code changes.

---

## 1. Make the repo public
Settings ▸ General ▸ Danger Zone ▸ **Change visibility → Public**.

Why it's required: release `.deb`/`.rpm` assets and GitHub Pages must be publicly
reachable for `apt`/`dnf`/badges to work.

- [ ] Repo is public
- [ ] Confirm the badges in the README now render (CI, release, license)

---

## 2. Turn on Homebrew  →  `brew install yedidiaSch/tap/envcore`
The `homebrew-tap` repo already exists and is wired; it just needs a push token.

1. Create a **fine-grained PAT**: https://github.com/settings/tokens?type=beta
   - Repository access: **only** `yedidiaSch/homebrew-tap`
   - Permissions: **Contents → Read and write**
2. Add it as a secret on the `envcore` repo:
   ```bash
   gh secret set HOMEBREW_TAP_GITHUB_TOKEN --repo yedidiaSch/envcore   # paste the PAT
   ```
3. Publish the cask for the current release (or wait for the next tag):
   ```bash
   git tag v0.1.1 && git push origin v0.1.1     # release workflow pushes the cask
   ```

- [ ] PAT created (scoped to `homebrew-tap`, contents:write)
- [ ] `HOMEBREW_TAP_GITHUB_TOKEN` secret set
- [ ] Tagged a release; cask appears in `yedidiaSch/homebrew-tap`
- [ ] `brew install yedidiaSch/tap/envcore` works

---

## 3. Turn on APT/YUM  →  `apt install envcore` / `dnf install envcore`
The signing key + publish workflow are already in place (`GPG_PRIVATE_KEY` secret
is set; public key in `packaging/`).

1. Enable GitHub Pages: Settings ▸ Pages ▸ Source **Deploy from a branch** →
   branch **`gh-pages`**, folder `/ (root)`.
   (The `gh-pages` branch is created by the workflow's first run — do step 2 first,
   then set the source, then it serves.)
2. Run the publisher for the existing release:
   Actions ▸ **Publish package repo** ▸ *Run workflow* ▸ tag `v0.1.0`.
3. Verify the repo is live:
   ```bash
   curl -fsSL https://yedidiaSch.github.io/envcore/gpg.key | head -1   # PGP PUBLIC KEY
   ```
4. Smoke-test on a Debian/Ubuntu box (or container):
   ```bash
   curl -fsSL https://yedidiaSch.github.io/envcore/gpg.key | sudo gpg --dearmor -o /usr/share/keyrings/envcore.gpg
   echo "deb [signed-by=/usr/share/keyrings/envcore.gpg] https://yedidiaSch.github.io/envcore/deb stable main" | sudo tee /etc/apt/sources.list.d/envcore.list
   sudo apt update && sudo apt install envcore && envcore version
   ```

- [ ] Publish workflow ran green for `v0.1.0`
- [ ] Pages source set to `gh-pages`
- [ ] `gpg.key` reachable over HTTPS
- [ ] `apt install envcore` works on a clean box
- [ ] `dnf install envcore` works on a clean box

> First real run of the repo-assembly step is this workflow dispatch (reprepro /
> createrepo_c only run on the GitHub runner). If it errors, paste the log — it's
> a quick iterate, same as the e2e scripts.

---

## Notes / caveats
- **Signing key:** fingerprint `8A19045A2466ED368AA23868988E9FA033620E2F`
  (`EnvCore Packages`). Private half is the `GPG_PRIVATE_KEY` secret; rotate per
  `packaging/README.md`.
- **macOS/Windows CLI** is built + released but **unvalidated** — that's roadmap
  **M7** (`../plan/envcore-roadmap.md` in the design set), not part of this checklist.
- After all three are green, every channel is live: `brew` · `apt` · `dnf` ·
  direct download · `go install`. Then delete this section.

---

# Remaining implementation backlog (from the design & plan)

What the MVP (v0.1.0) delivered vs. what the design (`../plan/`) still calls for.
Grouped by priority. IDs reference the design review findings / roadmap milestones.

> **Read this first — the one gap that blocks real use:**
> **Workspace sync is not implemented (P0).** Nodes run your `run:` command, but
> EnvCore never copies your source/binaries to them. Today only commands that need
> no local code work (the e2e used `echo`/`ping`). A real C++ workload needs the
> workspace synced (and built) on the node first. Until P0-1 lands, `run:
> ./build/app` assumes `./build/app` already exists on the node — it won't.

## P0 — makes it usable for real C++/IPC workloads
- [ ] **Workspace synchronization** (design §4, H3, L5) — rsync-over-SSH of the
      local workspace to each node before running; `--sync=source` (build remotely)
      vs `--sync=binaries` (validate target arch/libc, H3); `.envcoreignore`
      (fallback `.gitignore`). *Without this the tool can't run user code.*
- [ ] **Apply `cluster.yaml` fields that are currently parsed-but-ignored** — the
      config layer reads them, but `upClusterHetzner` only uses `name` + `run`.
      Wire up per-node: `size`, `image`, `region`, `engine`, `sync`, `ttl`,
      `ipc_ports` (→ firewall), `needs_caps`/`privileged_ports` (→ least-priv),
      `egress_allow`, and the `security:` overrides. Plus `defaults:` inheritance.
- [ ] **Docker engine** (`--engine=docker`, spec §2) — run the toolchain/workload
      in a hardened container (non-root, seccomp/AppArmor, no docker.sock, read-only
      rootfs). Only the native/host path exists today.
- [ ] **Local target** (`--target=local`, spec §2) — run on the workstation; keep
      the `local+native` guardrail (`--allow-local-native`, Linux-only, H4).

## P1 — security hardening the design promises (M1.x + security architecture)
- [ ] **Least-privilege run user** (S-C) — commands currently run as **root**. Add a
      dedicated `envcore-run` user, dropped caps, `NoNewPrivileges`, add-back only
      declared `needs_caps`/`privileged_ports`.
- [ ] **Encrypted volumes at rest** (LUKS, S-E).
- [ ] **Cloud metadata block** (`169.254.169.254`, S-F) post-provision.
- [ ] **auditd** with off-node, tamper-evident log shipping (S-F).
- [ ] **Secret keychain** (H6) — token/keys via OS keychain (macOS Keychain /
      libsecret / Windows Credential Manager); today the token is env-only and keys
      are `0600` files.
- [ ] **Provider-level Cloud Firewall** (M8) — Hetzner network firewall in front of
      host nftables (defense in depth).
- [ ] **fail2ban** as secondary defense (M7-review), `unattended-upgrades` on
      longer-lived nodes.
- [ ] **Reproducibility** (H2) — pin toolchain versions + record a per-cluster
      lockfile (`~/.envcore/lock/<id>.json`). Toolchain is currently unpinned.
- [ ] **Signed releases** — add cosign signing to goreleaser (checksums + SBOM exist;
      artifact signatures don't).

## P2 — lifecycle & cost (roadmap M4)
- [ ] **TTL dead-man's-switch** (C4/A5) — server-side systemd idle-timeout that
      self-destroys a node with no heartbeat; the real leak-prevention when the
      laptop dies. `--ttl`, `--no-ttl`.
- [ ] **`envcore ls` / `status`** (L1) — list active clusters, nodes, uptime, and
      **live cost**.
- [ ] **`envcore reap`** (C4) — sweep orphaned tagged resources across all clusters
      (today `down --id` reconciles one cluster).
- [ ] **`envcore attach`** (L6) — reconnect to a running cluster's multiplexed
      streams. *Foundation exists:* the manifest persisted for `lockdown`.
- [ ] **Budget controls** (L2) — `--max-cost`, idle auto-stop (built on the TTL).

## P3 — providers, portability, polish
- [ ] **DigitalOcean provider** (M6) — prove the `Provider` seam with a 2nd backend.
- [ ] **macOS/Windows CLI validation** (M7) — run tests + a real e2e on each; document
      the per-OS operator overlay join; consider userspace `wireguard-go` so the
      operator side needs no admin install.
- [ ] **`--dry-run`** (L4) — preview the plan without creating anything.
- [ ] **Structured logging / audit trail** (L3) — `log/slog` for EnvCore's own infra
      actions (today it's plain `fmt` prints).
- [ ] **Config precedence + profiles** (CLI spec) — `flags > env > cluster.yaml >
      ~/.envcore/config.yaml > defaults`; named credential/config profiles.
- [ ] **Shell completion** (`envcore completion …`), richer `--help` examples.

## Explicitly NOT planned (don't "implement" these)
- **Auto-restart of crashed processes** — fail-fast is a deliberate design choice
  (§5): freeze, alert, leave the node up for GDB. Do not add a supervisor.
- **AWS / GCP providers** — deferred until there's a concrete need (see
  `envcore-provider-comparison.md`); the `Provider` seam is ready if so.
- **Non-Ubuntu / non-Linux nodes** — provisioned environments are Ubuntu-by-design.

---

### Suggested order
P0-1 (workspace sync) and P0-2 (apply cluster.yaml) first — they unblock real
usage. Then P1 least-privilege run user (biggest security delta: root → scoped).
Then P2 TTL (closes the last real leak vector). Everything else as needed.
