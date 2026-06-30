# EnvCore

Provisioning, hardening & orchestration CLI for C++ and distributed-IPC dev/test
environments. Design docs live in `../plan/`.

> **Status: M0 — walking skeleton.** Proves the architecture's spine (state machine +
> reconcile loop) against an in-memory **mock provider**, with no cloud and no cost.
> stdlib-only by design so it builds and tests fully offline. cobra CLI + the Hetzner
> provider arrive in M1.

## What M0 demonstrates
- `internal/provider` — the `Provider` interface (the only cloud seam).
- `internal/provider/mock` — free, offline fake; the permanent CI backbone.
- `internal/state` — journaled, atomic on-disk cluster state (a cache, not the truth).
- `internal/orchestrator` — `Up` (single node) and `Down` (reconcile-to-empty).
- `internal/harden` — cloud-init builder using `ssh_keys` host-key injection
  (validated by spike **S1**; never `write_files`).

## Build & test
```bash
export PATH="$HOME/.local/go/bin:$PATH"
go test ./...        # the authoritative M0 proof
go build ./...
go run ./cmd/envcore demo
```

## Key invariants (asserted by tests)
- **No leaks:** `Up` then `Down` leaves zero servers.
- **C4 recovery:** `Down` reconciles from the provider even if local state is lost.
- **H7 resilience:** `Down` retries a transient destroy failure and is idempotent.
- **S1 regression guard:** host keys go through `ssh_keys`, never `write_files`.

## Roadmap
See `../plan/envcore-roadmap.md`. Next: **M1** (real Hetzner provider — runtime type
discovery + region preference per S1/F3 — cloud-init hardening, host-key pinning).
