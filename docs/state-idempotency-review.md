# Pandion Review: State Management & Idempotency

*A deep, code-level review of how Pandion tracks cluster state across its
lifecycle — what is journaled where, which operations are safely re-runnable,
what happens on crashes/re-runs/races — with concrete findings and
prioritized recommendations.*

**Status:** review report · **Scope:** state stores, lifecycle idempotency,
reconciliation, crash recovery · **Date:** 2026-07-13

---

## 1. Executive summary

Pandion's **teardown half is architecturally excellent**: the provider (queried
by tag) is the single source of truth, `down`/`reap` are true reconcile loops —
list, destroy-with-retry, **verify empty**, reap auxiliary resources — and every
provider's `DestroyServer` honors an explicit idempotency contract ("destroying
an already-absent server is success"). Teardown is re-entrant, works with zero
local state, and works from a different machine. This is the hard part, and it
is done right.

The **creation half has no idempotency story at all**: `up` never checks whether
the cluster id already exists — locally or at the provider — before it
provisions, overwrites the state journal, and **overwrites the SSH keys and
manifest of any live cluster with the same id**. Combined with the fact that
local artifacts are never cleaned on `down` (except the journal and debug
shares) and that nothing locks `~/.pandion` against concurrent invocations,
the local state layer can silently diverge from reality in several directions.

A second headline finding: the idle **TTL "dead-man's switch" powers the node
off — it does not destroy it** — and powered-off servers keep billing on every
supported provider. The budget math in `--max-cost` (hourly × TTL) and some
project docs assume spend stops at the TTL; it does not. The *actual*
leak-stopper is `reap`, which is manual.

Severity-ranked findings are in §4; the per-command idempotency matrix is §5;
prioritized recommendations are §6.

---

## 2. State inventory — who writes what, who reads it, who cleans it

Pandion has **one authoritative store and five local caches**. The design
comment in `internal/state/state.go:3-4` states the philosophy: *"It is a
CACHE, not the source of truth — the provider (queried by tag) is
authoritative (C4)."*

### 2.1 The source of truth: provider tags

Every server is created with a `pandion-cluster-id` label/tag
(`internal/provider/hetzner/hetzner.go:38-40, 155`; equivalents in
digitalocean/vultr/linode/scaleway/lambda). Two queries define all
reconciliation:

- `ListByTag(clusterID)` — everything belonging to one cluster
  (`internal/provider/provider.go:98-100`), used by `down`, `ls`, `status`.
- `ListAllTagged()` — every Pandion server of any cluster
  (`provider.go:101-104`), used by `reap` — the cross-machine,
  lost-laptop recovery path.

Auxiliary provider resources (registered SSH login keys, the cluster cloud
firewall, Scaleway IAM keys / detached volumes) are also cluster-labeled and
cleaned by the optional `AuxReaper`/`ClusterFirewaller` seams
(`provider.go:109-124`).

### 2.2 The local stores

| Artifact | Path | Written by | Read by | Cleaned by |
|---|---|---|---|---|
| **State journal** | `~/.pandion/state/<id>.json` | `orchestrator.UpSpec/UpCluster` (every phase transition) | nothing at runtime today (cache) | `Store.Close` on successful `down`/`reap` (`state.go:84-90`) |
| **Cluster manifest** | `~/.pandion/keys/<id>/manifest.json` | `saveManifest` at the end of cluster `up` (`cmd/pandion/cluster.go:972-978`) | `attach`, `ssh`, `cp`, `code`, `debug`, `start`, `lockdown`, `down` (provider inference via `manifestProvider`, `cluster.go:432-441`) | **never** |
| **SSH keys + WG conf** | `~/.pandion/keys/<id>/{host,login}_ed25519*`, `wg-<id>.conf` | `up` (`main.go:565-568`, `cluster.go:740-742`, `main.go:673`) | every reconnect command; `lockdown` | **never** |
| **Reproducibility lock** | `~/.pandion/lock/<id>.json` | `up` (`writeLock`) | `up --lock`, `down` receipt fallback (`main.go:1384-1387`) | **never** |
| **Run logs** | `~/.pandion/logs/<id>/<node>.log` | stream tee (`internal/stream/stream.go:62-63`, `O_TRUNC`) | the user | **never** |
| **Debug-share records** | `~/.pandion/state/…shares…` (`sharesDir`) | `debug share` | `unshare`, `down` | `reapShares` on `down` (`debugshare.go:351-363`) — the **only** extra artifact `down` cleans |
| **Operator config/profiles** | `~/.pandion/config.yaml`, `profiles/` | `init` | every command | user-managed (correctly so) |

Two structural observations before the findings:

1. **All local stores are keyed by bare cluster id** — not by
   `(provider, id)`. The journal records the provider *inside* the file
   (`state.go:36-41`), and the manifest does too, but the *filenames* collide:
   one `demo` per machine, across all providers (§4, F5).
2. **The journal is well-formed but write-only.** `Save` is atomic
   (write-temp-then-rename, `state.go:57-68`), phases exist
   (PLANNED → PROVISIONING → RUNNING → TEARING_DOWN → DESTROYED → FAILED,
   `state.go:18-25`), and transitions are journaled *before* the provider call
   ("crash-resumable", `orchestrator.go:110-113`) — but no code path ever
   *reads* a journal to resume, adopt, or even warn. The crash-resumability the
   journal was built for is unrealized (§4, F6). `DESTROYED` is never assigned
   anywhere; a fully-destroyed cluster is represented by file *absence*
   ("Absent == success", `state.go:83`).

---

## 3. What is done right (and must not regress)

### 3.1 The destroy contract is explicit and honored six times

`provider.Provider.DestroyServer` documents the contract at the seam: *"MUST
be idempotent: destroying an already-absent server is success (enables safe
retry + re-run)"* (`provider.go:95-97`). All six backends implement it with a
get-then-treat-missing-as-success or a 404-is-success check:

- Hetzner `hetzner.go:225-240` (`if srv == nil { return nil }`)
- DigitalOcean `digitalocean.go:229` (404 → success)
- Linode `linode.go:234, 449` · Vultr `vultr.go:225, 499`
- Scaleway `scaleway.go:289` (also idempotent across its two-phase
  terminate-plus-delete-volumes teardown)
- Lambda `lambda.go:222` · Mock `mock.go:188-198`

### 3.2 `Down` is a verified, re-entrant reconcile — not a best-effort delete

`orchestrator.Down` (`orchestrator.go:439-480`) is the strongest sequence in
the codebase:

1. `ListByTag` from the provider (works with zero local state);
2. best-effort journal of intent (TEARING_DOWN) — tolerates a missing journal;
3. `destroyWithRetry` per server (3 attempts);
4. **re-list and verify `len(left) == 0`** — teardown that didn't converge is
   an *error* ("teardown incomplete: %d server(s) remain"), never a false
   success;
5. `ReapAux` for keys/firewalls/volumes so nothing non-server leaks;
6. only then `Store.Close`.

Every step is safe to repeat: a failed run leaves the journal in place, and a
re-run lists whatever actually remains and converges. `reap` reuses `Down`
per cluster (`orchestrator.go:422-435`), inheriting all of this, and
`ReapPlan` works from `ListAllTagged` (`orchestrator.go:216-248`) so it
recovers clusters created by other machines or lost state.

### 3.3 Creation-side retries distinguish transient from permanent

`createWithRetry` (`orchestrator.go:69-94`) relaunches a *fresh* instance up to
3 times, but **only** for errors a provider explicitly wraps in
`TransientProvisionError` (boot-time capacity reclaim, `provider.go:21-35`) —
bad specs, quota, and auth fail immediately. The retry notices go to stderr so
a waiting operator sees the recovery. The mock provider can inject both
failure classes (`mock.go:31-37`) and the concurrency bound (`MaxConcurrent`,
semaphore at `orchestrator.go:161`) is asserted in tests.

### 3.4 Cluster-scoped "ensure" patterns, not "create"

The auxiliary resources use find-or-create semantics, so partial re-runs don't
duplicate: Hetzner `ensureLoginKey` (`hetzner.go:364-399`, keyed by name +
material), `EnsureClusterFirewall` ("create-or-confirms … Idempotent",
`provider.go:119-124`, `hetzner.go:478-505`), Scaleway's project-scoped IAM
login key with an explicit mutex serializing find-or-create across concurrent
node provisioning (`scaleway.go:115, 319`), Lambda's fingerprint-named keys
("re-runs are idempotent", `lambda/gpu.go:219`). On-node scripts are written
idempotently too (tmux re-start guard `start.go:58`, L2 bring-up `ip link del
… || true` `overlay/l2.go:72`, nft rules re-applied atomically as a whole
table `cluster.go:929`).

### 3.5 Fail-fast provisioning with rollback, phase-aware interrupt

Multi-node `UpCluster` is a true barrier: it returns only when *all* nodes are
RUNNING, and any node's failure cancels the in-flight group
(`orchestrator.go:147-203`). The CLI rolls the partial cluster back by default
(`cluster.go:721-726`) and Ctrl+C during setup does the same (`cluster.go:581-602`)
— an aborted creation never orphans a billing node. This is exactly the right
default for ephemeral infra.

---

## 4. Findings

Ordered by severity. Each: problem → concrete scenario → evidence →
recommendation (cross-referenced to §6).

### F1 — `up` is not idempotent and has no existence guard (CRITICAL)

**Problem.** `runUp` → `UpSpec`/`UpCluster` never asks the provider (or the
local journal) whether cluster `<id>` already exists. The flow immediately:
(1) **overwrites the journal** with a fresh PLANNED record
(`orchestrator.go:101-113, 151-159`); (2) provisions new tagged servers;
(3) **overwrites `~/.pandion/keys/<id>/`** — new login/host keys, new
`wg-<id>.conf`, new manifest (`main.go:565-568`, `cluster.go:740-742, 972-978`).

**Scenario A (provider allows duplicate names — DigitalOcean, Scaleway):**
`pandion up --id demo` twice → **two sets of servers tagged
`pandion-cluster-id=demo`**. The manifest/keys now describe only the second
set; the first set is unreachable by any Pandion command except `down`/`reap`
(which will destroy *both*, since they reconcile by tag). Billing continues on
machines the operator can no longer see per-node.

**Scenario B (provider enforces unique names — Hetzner):** the second create
fails on the name collision (`serverName` namespaces to
`pandion-<id>-<node>`, unique per project, `hetzner.go:22-35`) — but by then
the journal has already been rewritten: a healthy RUNNING cluster's journal
now reads FAILED (`orchestrator.go:124-127`), and in the cluster path the
rollback branch (`cluster.go:721-726`) responds to the failed re-run by
**calling `o.Down(id)` — destroying the healthy original cluster** the user
never asked to touch.

**Scenario C (mock):** the default `--id demo` makes accidental reuse the
*most likely* invocation, not an edge case.

**Evidence.** No `ListByTag`/`Load` call anywhere in `runUp` before
provisioning (`cmd/pandion/main.go:211-330`); key overwrite sites above.

**Recommendation.** R1: a cheap preflight — `ListByTag(id)` non-empty OR a
local journal/manifest exists → refuse with "cluster 'demo' already exists
(N servers, provider X) — choose another --id, or run `pandion down --id demo`
first". Scenario B's rollback must also become "roll back only what *this run*
created" (track the server IDs returned by this run's creates, not the tag).

### F2 — The TTL dead-man's switch powers off; billing continues (HIGH)

**Problem.** The on-node dead-man reaps an idle node with `systemctl poweroff`
(`internal/harden/cloudinit.go:368-400`). A powered-off server **keeps
billing** on Hetzner, DigitalOcean, Vultr, Linode and Scaleway (compute/disk
reservation). Three places assume otherwise:

1. `TODO.md:180` sells it as a node that "**self-destroys**" and calls it "the
   real leak-prevention when the laptop dies";
2. `--max-cost` projects total spend as Σ hourly × TTL — "spend before
   self-stop" (`orchestrator.go:315-321`) — i.e. the budget guard's model says
   spend *stops* at the TTL. In reality a forgotten node keeps accruing after
   poweroff until a manual `down`/`reap`;
3. the receipt/estimate copy inherits the same model.

**What it actually is:** an abuse-stopper (a compromised or runaway node goes
quiet) and a *partial* cost brake — genuinely valuable, just not what the
budget math or the roadmap text claims.

**Recommendation.** R2: pick one of (a) honest copy — rename to "idle
power-off", document that billing continues, and make `--max-cost` messaging
say "projected spend *until idle power-off*; the node still bills until
`down`/`reap`"; or (b) close the loop — the dead-man calls the provider's
delete API… which requires credentials on the node (rejected by the security
model), so realistically (a) plus R6 (an opt-in `reap --older-than` cron
suggestion or a `pandion reap --auto` hint printed at `up`).

### F3 — `down` cleans only 2 of 6 local artifacts; the rest go stale (HIGH)

**Problem.** A successful teardown removes the state journal (`Store.Close`)
and the debug-share records (`reapShares`, `main.go:1391`) — and nothing else.
Left behind forever: `~/.pandion/keys/<id>/` (manifest + keys + WG conf),
`~/.pandion/logs/<id>/`, `~/.pandion/lock/<id>.json`. Consequences:

- `attach`/`ssh`/`start`/`lockdown --id X` after a `down` load the stale
  manifest and hang dialing dead IPs instead of "cluster was torn down"
  (manifest readers listed in §2.2).
- **Stale lockfile skews the next receipt:** `down` falls back to the
  lockfile's `Created` for providers that report none (`main.go:1384-1387`);
  reuse the id later (which F1 permits) and a receipt can be computed against
  the *previous* cluster's creation time.
- The `down` message overclaims: *"teardown will also reap keys/firewall/
  state"* (`main.go:1366`) — keys are precisely what it does **not** reap.

**Recommendation.** R3 (same fix as UX-plan P0.2): tombstone or delete
`keys/<id>` and `lock/<id>.json` on verified teardown (keep logs, they're the
post-mortem); make manifest loaders fail fast on tombstones; fix the message.

### F4 — No cross-process lock over `~/.pandion` (HIGH)

**Problem.** Nothing prevents two Pandion processes from operating on the same
id concurrently. `up --id x` racing `down --id x` (or a second `up`, or `reap`)
interleaves: journal writes are individually atomic (`state.go:57-68`) but the
read-modify-write cycle across a whole command is not; `manifest.json`,
`login_ed25519`, and `wg-<id>.conf` are plain `os.WriteFile` overwrites. The
in-process mutex in `UpCluster` (`orchestrator.go:156-158`) protects goroutines,
not processes. `internal/lockfile` is *toolchain reproducibility*, not a mutex
— the name invites confusion (`lockfile.go:4`).

**Concrete race:** `reap` lists candidates, the user confirms; meanwhile an
`up` for a new cluster with a reused id completes; `reap`'s `Down(id)` now
destroys servers the plan never showed.

**Recommendation.** R4: per-id advisory flock
(`~/.pandion/state/<id>.lock`) held for the duration of `up`/`down`/`start`/
`lockdown` and per-candidate inside `reap`; non-blocking acquire with
"another pandion command is operating on 'x' (pid N)". Consider renaming
`internal/lockfile` → `internal/repro` while touching this area.

### F5 — Local state is keyed by id, not (provider, id) (MEDIUM)

**Problem.** `state/<id>.json`, `keys/<id>/`, `lock/<id>.json` collide across
providers. `up --id x --provider=hetzner` then `up --id x
--provider=digitalocean` is accepted (no guard, F1), and the second run
overwrites the journal, keys, and manifest — including the manifest's
`provider` field that `down` uses for inference (`manifestProvider`,
`cluster.go:432-441`). The Hetzner cluster keeps running and billing, but
`down --id x` now reconciles against DigitalOcean; the Hetzner side is
invisible until a Hetzner-scoped `ls`/`reap`.

**Recommendation.** R5: cheapest fix is the F1 preflight *including a
provider-mismatch check* against the existing journal/manifest ("id 'x' is
already a hetzner cluster — pick a new id or tear it down first"). A full
re-keying of the directory layout is not worth the migration cost.

### F6 — The journal is crash-*consistent* but nothing ever resumes from it (MEDIUM)

**Problem.** Transitions are journaled before provider mutations precisely so
"a crash is always resumable" (`state.go:5`, `orchestrator.go:111`). But no
command reads the journal: after a mid-`up` crash (or the post-provision
failures that leave "cluster left up", `cluster.go:750-753, 857`), the only
recovery is `down`/`reap` — destroy and start over. There is no `up --resume`,
no adopt-and-continue, and `ls` doesn't even flag "journal says PROVISIONING
since 40 minutes ago". PLANNED/FAILED journals for clusters that never created
a server also linger forever (only `Down` closes journals, and only after a
provider round-trip).

**Recommendation.** R7 (pragmatic, not a state-machine rewrite): (a) `ls
--local` (or a `pandion doctor`) that walks `~/.pandion/state` + `keys/` and
reports divergence from provider truth — stale journals, tombstones,
manifest/journal mismatches — with the fix command per line; (b) `start`
already re-launches workloads on a deployed cluster, document it as the
"resume" for the streaming phase; (c) garbage-collect journals whose cluster
has no provider servers during `reap`.

### F7 — `destroyWithRetry` retries instantly, 3×, and reports only the last error (LOW)

**Problem.** `orchestrator.go:482-490`: three back-to-back attempts with no
delay and no jitter. A rate-limited (429) provider will burn all three
attempts in milliseconds and surface a failure the user must manually re-run;
intermediate errors are discarded (only the last is returned), hiding a
pattern like "429, 429, 429" vs "500, timeout, 404-now-gone".

**Recommendation.** R8: small exponential backoff (e.g. 1s/3s/9s honoring
`ctx`), keep the retry count; wrap with attempt context. One function, no
seam change. (`createWithRetry` already does this correctly with
`provisionRetryDelay` — mirror it.)

### F8 — Journal write failures are silently swallowed in the cluster path (LOW)

**Problem.** `save := func() { …; _ = o.S.Save(c) }` (`orchestrator.go:157`).
A broken state dir (permissions, disk full) produces a cluster with **no
journal at all** while provisioning proceeds normally — the exact situation
where you later want the journal. The single-node path returns these errors
(`orchestrator.go:106-113`); the cluster path doesn't.

**Recommendation.** R9: warn once to stderr on the first failed save (keep
provisioning — provider truth still covers teardown), consistent with the
"cache, not truth" philosophy but no longer silent.

### F9 — The mock provider can't exercise cross-process state logic (LOW, test gap)

**Problem.** Mock state is in-memory per process (`mock.go:18-22, 128`).
Consequences: `pandion up --provider=mock` then `pandion ls --provider=mock`
(new process) shows nothing; `down` "reconciles to empty" trivially — so the
CI smoke can never catch regressions in exactly the areas of this report
(re-up collision, stale-manifest behavior, journal divergence, flock). The
orchestrator's excellent unit tests (failure injection, H7 retry, bounded
concurrency) all run in-process.

**Recommendation.** R10: optional file-backed mock state
(`PANDION_MOCK_STATE=dir` or automatic under `~/.pandion/mock/`), keeping the
in-memory default for unit tests. This single change makes F1/F3/F4/F6
regression-testable offline in `ci_smoke.sh`.

### F10 — No schema version in journal or manifest (LOW, future-proofing)

**Problem.** Neither `state.Cluster` (`state.go:36-41`) nor `clusterManifest`
(`cluster.go:396-401`) carries a version field. Compatibility is already
handled ad hoc — `manifestProvider` tolerates "a manifest from before this
field existed" (`cluster.go:433`). Each future field addition repeats that
guesswork; a field *rename* breaks silently (JSON zero values).

**Recommendation.** R11: add `"v": 1` to both on write; readers treat absent
as v0. Two lines now, a migration seam forever.

### F11 — Aux-reap failure leaves an ambiguous "half-down" (INFO)

**Observation, not a bug:** if `ReapAux` fails after all servers are destroyed,
`Down` errors out *before* `Store.Close` (`orchestrator.go:474-479`) — correct
(a leaked key/firewall is still a leak), and a re-run converges because every
step is idempotent. But the error the user sees ("reap aux resources for X:
…") doesn't say the important part: *servers are already gone, money has
stopped, just re-run `down`*. Same for "teardown incomplete: N server(s)
remain". **Recommendation** R12: append "safe to re-run `pandion down --id X`
— teardown is idempotent" to both errors.

---

## 5. Idempotency matrix (current behavior)

"Re-run" = running the same command again after success; "crash" = process
killed midway. ✅ safe/converges · ⚠ partial · ❌ unsafe.

| Command | Re-run after success | Crash midway | Notes |
|---|---|---|---|
| `up --id X` | ❌ duplicates servers (DO/Scaleway) or corrupts journal + may roll back the healthy cluster (Hetzner) — **F1** | ⚠ setup-phase Ctrl+C rolls back cleanly (`cluster.go:581-602`); a hard kill leaves tagged servers recoverable only via `down`/`reap` (journal never read, **F6**) | keys/manifest overwritten before the new run is known-good |
| `down --id X` | ✅ re-lists, destroys nothing, verifies, re-reaps aux — fully re-entrant | ✅ journal keeps TEARING_DOWN; re-run converges | leaves keys/lock/logs stale — **F3** |
| `reap` | ✅ (reuses `Down` per cluster) | ✅ per-cluster granularity; `firstErr` reported, rest continue | plan can go stale between list and confirm — **F4** |
| `start --id X` | ✅ tmux launch guarded ("a re-start is idempotent", `start.go:58`) | ✅ | fails confusingly on a stale manifest — F3 |
| `attach --id X` | ✅ read-only vs infra | ✅ | truncates prior local log on reconnect (`stream.go:62`); stale manifest hangs — F3 |
| `lockdown --id X` | ✅ nft rules re-applied as a whole atomic table (`cluster.go:929`); verify-first design | ✅ nothing changed unless all nodes verified | prereq: operator already on the overlay |
| `debug share` / `unshare` | ✅ provision script idempotent (`debugshare.go:152`); unshare of absent share is a no-op message | ✅ | share records are the one artifact `down` cleans |
| `init` / `login` / `logout` | ✅ keychain set/overwrite; init prompts before reconfigure | ✅ | |
| provider `DestroyServer` | ✅ contractually, all 6 backends | — | `provider.go:95-97` |
| provider `CreateServer` | ❌ no idempotency key/uniqueness contract at the seam — name collisions are provider-dependent (Hetzner unique, DO not) | — | root enabler of F1 scenario A |

---

## 6. Recommendations, prioritized

| # | Change | Fixes | Effort | Where |
|---|---|---|---|---|
| **R1** | `up` existence preflight: refuse when `ListByTag(id)` ≠ ∅ or a live local journal/manifest exists (include provider-mismatch check); scope failure rollback to the servers created by *this run* | F1, F5 | **S–M** | `cmd/pandion/main.go` (runUp), `cluster.go:721-726` |
| **R2** | Honest TTL semantics: rename copy to "idle power-off", state that billing continues until `down`/`reap`, adjust `--max-cost` and TODO/README wording; print a reap hint at `up` for long TTLs | F2 | **S** | `harden/cloudinit.go` copy, `orchestrator.go` budget messages, README/TODO |
| **R3** | Clean or tombstone `keys/<id>` + `lock/<id>.json` on verified teardown; manifest loaders fail fast on tombstone; fix the `down` message | F3 | **M** | `runDown`, `loadManifest`, `orchestrator.Down` |
| **R4** | Per-id advisory flock around mutating commands; rename `internal/lockfile` → `internal/repro` to free the name | F4 | **M** | new `internal/flock`; call sites in cmd/pandion |
| **R8** | Backoff in `destroyWithRetry` (mirror `provisionRetryDelay`), keep intermediate errors | F7 | **S** | `orchestrator.go:482-490` |
| **R9** | Warn (once) on failed journal saves in the cluster path | F8 | **S** | `orchestrator.go:157` |
| **R12** | Append "safe to re-run" to `Down`'s two partial-failure errors | F11 | **S** | `orchestrator.go:460, 469, 476` |
| **R7** | `pandion doctor` (or `ls --local`): report local-state divergence from provider truth with per-line fix commands; GC orphaned journals during `reap` | F6 | **M** | new cmd; reuses existing loaders |
| **R10** | Optional file-backed mock state so `ci_smoke.sh` can regression-test F1/F3/F4/F6 offline | F9 | **M** | `internal/provider/mock` |
| **R11** | `"v": 1` schema version in journal + manifest JSON | F10 | **S** | `state.go`, `cluster.go` |

Suggested order: **R1 + R2 + R3** (they close the three ways money or access
is silently lost), then **R4 + R8 + R9 + R12** (one small hardening PR), then
**R7 + R10 + R11** (observability + testability).

Overlap note: R3 and R4 correspond to items P0.2 and P0.5 of
`docs/ux-upgrade-plan.md`; this report supplies the state-layer design detail
for them. R1, R2 and the rest are new findings not covered by the UX plan.

---

## 7. Test coverage of state behavior — current vs missing

**Covered today (good):** destroy-retry on transient failure (H7,
`FailDestroyOnce`), partial-cluster fail-fast (`FailCreateFor`), transient
boot relaunch (`TransientBootFor`), bounded concurrency (`MaxConcurrent`) —
all via the mock's failure injection (`mock.go:24-41`); teardown-verification
and AuxReap invocation counts; `ci_smoke.sh` exercises journal/state I/O
paths on Linux/macOS/Windows.

**Not covered anywhere (all reproducible offline once R10 lands):**
re-`up` of an existing id (F1); post-`down` stale-manifest behavior (F3);
concurrent-invocation interleaving (F4 — needs R4's flock to have something
to assert); journal divergence reporting (F6); receipt-vs-stale-lockfile
(F3); provider-mismatch on a reused id (F5).

The e2e scripts (`scripts/e2e_*.sh`) all assert the *happy* reconcile — every
script's EXIT trap runs `down` and asserts no leak (`scripts/README.md:43-50`)
— which is why the teardown half is solid and the re-entry/collision paths,
which no script exercises, are where every finding in §4 lives.
