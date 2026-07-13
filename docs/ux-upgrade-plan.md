# Pandion UX Review & Upgrade Plan

*A full-depth review of the user experience — CLI surface, output, configuration,
initialization, lifecycle, docs — with a phased, actionable work plan.*

**Status:** proposed · **Scope:** UX only (no new features, no provider work) ·
**Date:** 2026-07-13

---

## How to read this plan

Work is grouped into five phases, ordered by user impact:

| Phase | Theme | Why this order |
|---|---|---|
| **P0** | Trust & correctness | Places where the CLI *lies* (exit codes, stale state, overclaiming messages). Cosmetics don't matter while these exist. |
| **P1** | CLI surface consistency & discoverability | `--help`/`--version`, flag conventions, color/TTY, JSON coverage. |
| **P2** | Configuration & initialization | Validation feedback, scaffolding, silently ignored fields, precedence. |
| **P3** | Documentation, onboarding & examples | User docs are absent today; examples cover one path of many. |
| **P4** | Long-running operation polish | Progress feel, error translation, log retention, cost rendering. |

Each item lists **Problem → Evidence → Fix → Effort → Acceptance**.
Effort: **S** ≤ half a day · **M** ≈ 1–2 days · **L** ≈ 3+ days.

### What the review found is *good* (preserve these)

- **Lockout-safe `lockdown`** — verifies overlay SSH to every node *before* changing
  anything, refuses with an actionable message otherwise (`cmd/pandion/lockdown.go:71-78`).
- **Phase-aware Ctrl+C** — rollback during setup, detach-not-destroy once streaming
  (`cmd/pandion/cluster.go:581-602`); workloads survive in tmux, `pandion attach` reconnects.
- **Fail-closed `--max-cost`** — unpriceable provider or `--no-ttl` is an error, with
  messages that tell you exactly which knob to turn (`internal/orchestrator/orchestrator.go:358-374`).
- **Reconcile-based teardown/reap** — provider tags are the source of truth; retries,
  verifies emptiness, reaps aux resources (`internal/orchestrator/orchestrator.go:216-248, 439-490`).
- **Actionable credential errors** — missing-token messages name the env var *and* the
  `pandion login` alternative (`cmd/pandion/login.go:41-55`); the Scaleway triple-credential
  preflight names exactly which value is missing (`cmd/pandion/main.go:159-175`).
- **Flag help strings** are unusually clear and example-rich (e.g. `--gpu`, `--setup`).
- **Loud-signal touches** — apt install misses surfaced (`warnMissingPackages`), binary
  arch mismatch caught before "Exec format error" (`warnBinaryArchMismatch`).

None of the items below should regress these.

---

## P0 — Trust & correctness

> The theme: **what Pandion tells the user (or a script) must be true.** Every item
> here is a case where output, exit status, or local state disagrees with reality.

### P0.1 Exit codes lie on failure — the biggest scripting footgun

- **Problem:** `pandion up` frequently exits **0** on failure. Two classes:
  1. *Post-provision failures* — cloud-init readiness, mesh setup, service discovery —
     print an error, leave the cluster up (correct, by design), but then `return` from
     the flow, so the process exits 0.
  2. *Workload failures* — a crashed `run:` command is reported in the stream
     ("process exited (code N) — node left up for GDB/SSH") but never propagated.
     `pandion up … -- ./flaky_test && deploy` happily deploys.
- **Evidence:**
  - Cluster path: cloud-init failure `cmd/pandion/cluster.go:750-753`, mesh failure
    `:857`, per-node firewall failure warns only `:918`, mesh-verify soft-fail `:985` —
    all `return`/warn with exit 0. Single-node path: readiness `cmd/pandion/main.go:579`,
    firewall `:695` — same pattern.
  - Workload exit: `tailLog` (`cmd/pandion/cluster.go:1040-1061`) parses the
    `__pandion_exit__ N` sentinel, prints it, discards it. Contrast `pandion ssh -- cmd`,
    which *does* propagate openssh's exit code (`cmd/pandion/sshcmd.go:112-122`).
- **Fix:**
  1. Define the exit-code contract in one place (`cmd/pandion/exitcodes.go` with named
     constants; today's de-facto scheme — 2 usage, 3 missing manifest, 5 rollback,
     6 budget/lockdown-refused, 8 reap failure — is ~99 scattered `os.Exit` magic numbers).
     Add: **7 = infra degraded (cluster left up)**, and make `up`/`start`/`attach`
     exit with the **workload's own code** when a run command fails (mirroring `ssh`).
  2. Thread a status value out of `upClusterHetzner`/`upHetzner`/`streamCluster`
     instead of bare `return`s.
  3. Document the contract (see P3.1) and in `pandion up -h`.
- **Effort:** M
- **Acceptance:** `pandion up --provider=mock … -- 'exit 3'`-style test exits 3;
  a simulated post-provision failure exits 7; `scripts/ci_smoke.sh` asserts both;
  exit codes table exists in docs and matches `exitcodes.go`.

### P0.2 `down` leaves stale local state; dependent commands then dial dead IPs

- **Problem:** Teardown removes the state journal but **not**
  `~/.pandion/keys/<id>/` (manifest.json, login/host keys, `wg-<id>.conf`) or
  `~/.pandion/logs/<id>/`. Afterwards `attach`/`ssh`/`start`/`lockdown --id X`
  load the stale manifest and hang against dead IPs instead of saying "that
  cluster was torn down". Worse, `down`'s own message overclaims: *"teardown will
  also reap keys/firewall/state"*.
- **Evidence:** `runDown` (`cmd/pandion/main.go:1330-1395`) → `o.Down` →
  `o.S.Close(id)` removes only `~/.pandion/state/<id>.json`
  (`internal/orchestrator/orchestrator.go:479`). The overclaiming string:
  `cmd/pandion/main.go:1366`. Manifest readers: `cmd/pandion/cluster.go:396-430`
  (loaders), `start.go:45`, `cluster.go:1110-1152` (attach).
- **Fix:**
  1. On successful `down`/`reap`, either delete `~/.pandion/keys/<id>/` and
     `~/.pandion/logs/<id>/`, **or** (safer, preserves post-mortem logs) write a
     `destroyed_at` tombstone into the manifest and keep logs.
  2. Teach the manifest loader to reject tombstoned manifests with:
     `cluster "X" was torn down on <date>; nothing to attach to (logs kept in ~/.pandion/logs/X)`.
  3. Fix the `down` message to state exactly what is and isn't removed.
- **Effort:** M (tombstone approach; touchpoints are all behind the shared manifest loader)
- **Acceptance:** mock e2e — `up`, `down`, then `attach`/`ssh`/`start`/`lockdown`
  each fail fast with the "was torn down" message and exit 3; `down` output matches
  actual cleanup behavior.

### P0.3 Destructive `--id` default: `pandion down` with no id targets "demo"

- **Problem:** `down`, `start`, and all `relay *` subcommands default `--id` to
  `"demo"`, while `ssh`/`cp`/`code`/`debug`/`attach`/`lockdown` require it and error.
  A user who runs bare `pandion down` while owning a real cluster named "demo"
  destroys it; a user who forgets `--id` gets inconsistent behavior across commands.
- **Evidence:** `cmd/pandion/main.go:1333` (`down`), `cmd/pandion/start.go:28`,
  `cmd/pandion/relay.go:55,147,275,320,390` — default `"demo"`. Required-with-error:
  `cmd/pandion/sshcmd.go`, `debug.go`, `main.go:784-796` (attach).
- **Fix:** Require `--id` on `down`/`start`/`relay` like the rest. Two ergonomic
  escape hatches: (a) if exactly **one** live cluster exists (from local state),
  offer it in the confirmation prompt — `no --id given; destroy the only cluster
  "pipeline"? [y/N]`; (b) keep bare `pandion down` working for the `demo` id **only
  under the mock provider** so `pandion demo` flows stay one-liners.
- **Effort:** S
- **Acceptance:** `pandion down` with no id and no/multiple clusters exits 2 with
  a message listing live ids; single-cluster prompt path covered by a unit test;
  `ci_smoke.sh` mock flow still passes.

### P0.4 `reap` and `down` disagree on non-interactive confirmation

- **Problem:** `down` prompts only on a TTY (scripts proceed); `reap` prompts
  unconditionally — a piped/CI `reap` reads empty stdin and aborts. The two
  destructive commands behave oppositely under automation.
- **Evidence:** `down`: `if !*yes && len(servers) > 0 && isTTY()`
  (`cmd/pandion/main.go:1372-1381`). `reap`: no `isTTY()` gate
  (`cmd/pandion/main.go:820-826`).
- **Fix:** Add the `isTTY()` gate to `reap` (matching `down`), or — stricter and
  arguably safer for the *fleet-wide* destroyer — keep requiring `--yes` when
  non-interactive but say so explicitly: `non-interactive: pass --yes to confirm`.
  Recommendation: the stricter variant for `reap` (it can destroy every cluster),
  the permissive variant stays on `down`. Either way both must print *why*.
- **Effort:** S
- **Acceptance:** piped `reap` without `--yes` exits 2 with the explicit hint;
  piped `down` behavior unchanged; both documented in command help.

### P0.5 No cross-process lock on `~/.pandion` state

- **Problem:** Two concurrent invocations touching the same cluster id (`up` racing
  `reap`, double `up --id x`) interleave writes to `state/<id>.json` and
  `keys/<id>/manifest.json`. The only guard is an in-process mutex.
- **Evidence:** `internal/orchestrator/orchestrator.go:156-193` (`var mu sync.Mutex`
  around a shared `*state.Cluster`); no flock/O_EXCL anywhere in the state path
  (`internal/lockfile` is toolchain reproducibility, not a mutex). Atomic
  write-rename in `internal/state/state.go:57-68` protects single writes only.
- **Fix:** Per-cluster advisory lock file `~/.pandion/state/<id>.lock`
  (`syscall.Flock` on Unix, `LockFileEx` fallback or create-excl on Windows —
  keep it in a tiny `internal/flock` package). Acquire non-blocking in `up`,
  `down`, `start`, `lockdown`, `reap`(per id); on contention:
  `another pandion command is operating on "X" (pid N) — retry when it finishes`.
- **Effort:** M (Windows path is what makes it M; CI smoke already runs on Windows)
- **Acceptance:** unit test acquires the lock and asserts a second acquire fails
  with the message; `go test -race` clean; `ci_smoke.sh` passes on all three OSes.

### P0.6 Small lies and swallowed errors

- **Problem / Evidence / Fix (batch of one-liners):**
  1. Stale mock message *"real mesh + IPC land in M3.2b/M3.3"*
     (`cmd/pandion/main.go:409`) — milestones long since shipped. Reword to what
     mock actually does/doesn't do.
  2. `_ = o.S.Save(c)` swallows journal-write failures during cluster provisioning
     (`internal/orchestrator/orchestrator.go:157`) — a broken state dir silently
     produces no journal. Warn once to stderr on first failure.
  3. `fmt.Scanln` errors ignored on confirmation prompts
     (`cmd/pandion/main.go:1376, 824`) — EOF reads as "abort", which is safe but
     should *say* `no confirmation received — aborted`.
- **Effort:** S (all three together)
- **Acceptance:** messages updated; a failing-state-dir unit test sees the warning.

---

## P1 — CLI surface consistency & discoverability

### P1.1 `--help`, `-h`, `help`, and `--version` don't exist at the top level

- **Problem:** `pandion --help`, `pandion -h`, `pandion help`, and
  `pandion --version` all fall through to `default:` → usage on **stderr** +
  **exit 2** — an error, for the most common discovery gestures in any CLI.
  Only the `version` subcommand works. Per-command help is stdlib flag-dump only;
  just `debug` has a real synopsis.
- **Evidence:** dispatch switch `cmd/pandion/main.go:52-109`; `usage()`
  `main.go:1441-1468` (stderr); the lone custom `fs.Usage` at
  `cmd/pandion/debug.go:126-131`.
- **Fix:** Handle `help`, `-h`, `--help` (usage → **stdout**, exit 0) and
  `--version`/`-V` (alias of `version`) before the switch. Give every command a
  `fs.Usage` with a one-line synopsis + one example (the registry in P1.2 makes
  this table-driven rather than 20 hand-written closures).
- **Effort:** S (given P1.2) · **Acceptance:** `pandion --help` exits 0 on stdout;
  `pandion up -h` shows synopsis + example; smoke-tested in `ci_smoke.sh`.

### P1.2 One command registry instead of three hand-maintained parallel lists

- **Problem:** The command set is declared three times — the dispatch switch, the
  static `usage()` block, and `completionCommands` — with a comment admitting the
  completion list is deliberately separate. Nothing ties them together; they *will*
  drift. Completion also offers a flat list of ~20 flags for **every** command
  (`pandion ls --<TAB>` suggests `--ttl`).
- **Evidence:** `cmd/pandion/main.go:60-108` (switch), `main.go:1441-1468`
  (usage), `cmd/pandion/completion.go:13-17` (list; "Kept here (not derived from
  the main switch)"), `completion.go:62` (flat flag list).
- **Fix:** A single table `var commands = []command{{name, synopsis, example,
  flags, handler}, …}` that drives (a) dispatch, (b) `usage()`, (c) per-command
  `fs.Usage`, (d) completion — commands *and* per-command flags. Add a drift test
  asserting every registered command completes and appears in usage.
  **Deliberately not a cobra migration:** the codebase is consistently
  stdlib/hand-rolled and this is the final stretch; a registry gets the same wins
  (single source of truth, generated help/completion) without a big-bang diff or
  a new dependency.
- **Effort:** M · **Acceptance:** drift test green; `pandion ls --<TAB>` only
  offers `ls` flags; `usage()` output generated, not hand-edited.

### P1.3 Flag-convention cleanup

- **Problem / Evidence:**
  1. `-f` is the lone flag breaking the `--long` convention — registered as `f`
     on `up` and `validate` (`cmd/pandion/main.go:243, 771`); `--file` doesn't exist.
  2. `--overlay`/`--public` polarity flip: `ssh`/`cp`/`code` default public and
     take `--overlay` (`cmd/pandion/sshcmd.go:36`); `debug` defaults overlay and
     takes `--public` (`cmd/pandion/debug.go:122`). Intentional (per code comment)
     but a cognitive tax.
  3. Provider aliases (`do`, `scw`, `akamai`, `lambdalabs`) are accepted
     (`main.go:131-182`, `login.go:18-36`) but advertised nowhere.
  4. `--node` means three things: node-to-create (`up`), node-selector
     (`ssh`/`cp`/`code`/`debug`), node-filter (`start`).
- **Fix:** (1) register `--file` as an alias everywhere `-f` exists (keep `-f`).
  (2) accept **both** `--overlay` and `--public` on all five commands as an
  explicit override of each command's default; state the default in help text.
  (3) list aliases in `--provider` help + completion. (4) per-command help text
  spells out the meaning (free with P1.2's registry).
- **Effort:** S · **Acceptance:** `pandion validate --file x.yaml` works;
  `pandion ssh --public …` and `pandion debug --overlay …` are no-ops that
  parse; help text updated.

### P1.4 Color is not TTY-aware — ANSI leaks into pipes and files

- **Problem:** `colorEnabled()` only checks `NO_COLOR`; piping `pandion up` to a
  file captures escape codes. (Stream-log *files* are already teed raw — only the
  console writer misbehaves.)
- **Evidence:** `cmd/pandion/cluster.go:1154-1156`; ANSI emission in
  `internal/stream/stream.go:80-85`.
- **Fix:** `colorEnabled() = NO_COLOR unset && stdout is a terminal`
  (`golang.org/x/term.IsTerminal` — x/term is already a direct dependency via the
  no-echo token prompt; no new module).
- **Effort:** S · **Acceptance:** unit test on the predicate; piped output byte-clean.

### P1.5 `--json` coverage for automation

- **Problem:** Only `ls`/`status` and `list-gpus` speak JSON. Teardown results,
  reap sweeps, and dry-run plans — exactly the things CI wants to assert on —
  are print-only.
- **Evidence:** `cmd/pandion/main.go:846` and `:1087` (existing `--json`);
  `renderDryRun` `main.go:1170-1191`; receipt `main.go:1254-1307`; `runReap`
  `main.go:798+` — no JSON paths.
- **Fix:** Add `--json` to `down` (receipt: nodes, runtime, estimated cost),
  `reap` (per-cluster destroyed counts), and `up --dry-run` (the plan table +
  projected totals). Follow the existing stable-schema pattern of
  `renderStatusJSON` (`main.go:947-999`) — always emit the envelope even when empty.
- **Effort:** M · **Acceptance:** golden-file unit tests per schema (mirroring
  `lsjson_test.go`); documented in help.

### P1.6 Verbosity is an undiscoverable env var

- **Problem:** The only verbosity knob is `PANDION_LOG` (tees the audit trail to
  stderr) — absent from help, usage, and completion.
- **Evidence:** `internal/audit/audit.go:37` (`LevelFromEnv`), wired at
  `cmd/pandion/main.go:1428-1436`; documented only in README:430.
- **Fix:** Global `--verbose` (= `PANDION_LOG=debug`) and `--quiet` (suppress
  progress chatter on stdout, keep errors on stderr) parsed alongside
  `--profile` in `initProfile`-style pre-parse (`cmd/pandion/profile.go:26-50`
  is the pattern). Keep the env var; flags win.
- **Effort:** M · **Acceptance:** `pandion --verbose up …` shows the audit
  stream; `--quiet` leaves only results + errors; both in usage/completion.

---

## P2 — Configuration & initialization

### P2.1 Validation errors are raw jsonschema output

- **Problem:** `pandion validate` (and every `Load`) surfaces the validator's
  message verbatim: JSON-pointer paths (`/nodes/0`), no YAML **line numbers**, no
  "did you mean" for typos. For the tool's primary config artifact, feedback
  quality is the UX.
- **Evidence:** `internal/config/config.go:296-340` (`return err // jsonschema.ValidationError
  has a detailed message`); `cmd/pandion/main.go:769-778` prints `invalid: %v`, exit 2.
- **Fix:** A translation layer in `internal/config`:
  1. Map the instance JSON pointer back to a YAML line/column (`gopkg.in/yaml.v3`
     `yaml.Node` — walk the node tree by pointer segments; the yaml.v3 dependency
     already exists).
  2. For `additionalProperties` violations, Levenshtein-suggest the nearest legal
     key from the schema at that path: `unknown field "tootlchain" (did you mean
     "toolchain"?)`.
  3. Format: `cluster.yaml:12:5: nodes[0]: unknown field "runn" (did you mean "run"?)`.
- **Effort:** M–L (the pointer→line walk is fiddly; test-drive against
  `internal/config/testdata/invalid_*.yaml` and new cases)
- **Acceptance:** golden tests for ≥6 error shapes (unknown field top-level/node/
  security, bad apiVersion, bad name pattern, bad port spec) each showing
  file:line + suggestion; `validate` output uses it.

### P2.2 Schema-valid fields that are silently ignored

- **Problem:** Several fields validate against `schema.json` but have **no
  consumer** (some not even a struct field), so users configure them, get no
  effect and no warning: top-level `firewall:` block, `provider.private_network`,
  `security.capabilities`. This is the worst kind of config bug — the file is
  "valid" and wrong.
- **Evidence:** `internal/config/schema.json` (`firewall` at lines 195-220,
  `private_network` at 44-50, `capabilities` at ~100) vs the typed structs in
  `internal/config/config.go` (`Security` struct :104-112 has no capabilities
  field; no firewall struct is loaded/consumed).
- **Fix:** For each field decide: **wire it** (if the backend exists — e.g.
  `security.capabilities` overlaps the implemented `needs_caps` add-back),
  **warn** (`cluster.yaml: "firewall:" is accepted but not yet applied — the
  hardened defaults are used instead`), or **remove from the schema** so
  validation honestly rejects it. Default to *remove or warn* — never silent.
  Add a CI guard: a test that walks schema properties and asserts each is either
  consumed by the loader or on an explicit allow-list with a warning.
- **Effort:** M · **Acceptance:** no schema property is silently droppable; the
  drift test fails if a future schema field lands without a consumer/warning.

### P2.3 No way to scaffold a cluster.yaml

- **Problem:** `pandion init` configures the operator (provider/token/defaults)
  but there is no generator for the *topology* file users must hand-write; the
  schema isn't user-visible either. Today the path is "copy the zmq example and
  guess".
- **Evidence:** `cmd/pandion/init.go` (writes `~/.pandion/config.yaml` only);
  no `new`/scaffold command in the dispatch (`main.go:60-108`).
- **Fix:** `pandion init --cluster [PATH]` (or `pandion new`, decide once) that
  writes a commented starter `cluster.yaml` — 2 nodes, `defaults.sync`,
  `$PANDION_<NODE>_IP` discovery in a comment, pointers to `validate` and docs.
  Source it from a `//go:embed` template distilled from
  `examples/zmq-cluster/cluster.yaml` (already excellent). Refuse to overwrite
  without `--force`. Print next steps (`pandion validate`, `pandion up -f`).
- **Effort:** S–M · **Acceptance:** scaffolded file passes `pandion validate`;
  `ci_smoke.sh` scaffolds + validates on all OSes; README quickstart uses it.

### P2.4 Finish config precedence + document it

- **Problem:** The intended precedence `flags > env > cluster.yaml >
  ~/.pandion/config.yaml > defaults` is stated in code comments and partially
  wired (`applyUpDefaults` covers flag-vs-config for a few knobs); the TODO
  backlog lists it as open. Users can't predict which value wins.
- **Evidence:** `internal/userconfig/userconfig.go:5-9` (the contract),
  `cmd/pandion/main.go:193-209` (`applyUpDefaults`), `TODO.md:268`.
- **Fix:** Audit each `up`/`build` knob (provider, region, size, ttl, engine,
  sync) against the contract; route them through one resolution helper that also
  powers a `pandion config` / `validate --show-effective` printout of the
  *effective* value per knob **and its source** (`region = fsn1 (from
  ~/.pandion/config.yaml)`). That printout doubles as the debugging tool for
  "why did it pick that?".
- **Effort:** M · **Acceptance:** table-driven test over the precedence matrix;
  `--show-effective` output covered by a golden test; docs section (P3.1).

### P2.5 `PANDION_HOME` override

- **Problem:** All state/keys/logs/config hang off `os.UserHomeDir()` with no
  override — painful for tests, shared machines, and XDG purists.
- **Evidence:** `envHome()` `cmd/pandion/main.go:1419-1422`; call sites via
  `filepath.Join(envHome(), ".pandion", …)`.
- **Fix:** `envHome()` honors `PANDION_HOME` (pointing at the directory that
  *replaces* `~/.pandion`). One function change + docs; `ci_smoke.sh` already
  isolates `$HOME` and can switch to it.
- **Effort:** S · **Acceptance:** smoke test runs entirely under
  `PANDION_HOME=$(mktemp -d)`; documented in the env-var reference.

### P2.6 Surface the silent defaults

- **Problem:** Consequential defaults are invisible until they bite: TTL 60m
  (node powers off!), engine `native`, container image `ubuntu:24.04`, run user
  `pandion-run`, auto-selected size/region.
- **Evidence:** schema defaults in `internal/config/schema.json`;
  `harden.DefaultIdleTTL`; size/region auto-selection in the provider layer.
- **Fix:** (a) `--dry-run` already shows size/region/TTL per node — extend it to
  show engine/image and mark auto-chosen values (`size=cpx21 (auto)`); (b) the
  first status line of a real `up` names the TTL: `nodes idle-poweroff after 60m
  (--ttl to change, --no-ttl to disable)`; (c) defaults table in docs (P3.1).
- **Effort:** S · **Acceptance:** dry-run golden test updated; `up` line present
  in smoke output.

---

## P3 — Documentation, onboarding & examples

### P3.1 Create user documentation (docs/ is internal-only today)

- **Problem:** `docs/` contains two contributor design notes (`gpu-design.md` —
  a draft, `security-next-steps.md`). There is **no** user-facing documentation
  beyond the (dense, single-page) README: no cluster.yaml reference, no
  troubleshooting, no env-var list, no exit codes.
- **Evidence:** `ls docs/`; the JSON Schema carries per-field `description`s
  (`internal/config/schema.json`) that are never rendered for humans.
- **Fix:** Add, in priority order:
  1. `docs/getting-started.md` — install → `init` → scaffold → `validate` →
     `up` → `attach`/`ssh` → `down`, with expected output.
  2. `docs/cluster-yaml.md` — **generated from `schema.json`** (a small
     `go generate` tool walking properties/descriptions/defaults/patterns), so
     it can't drift.
  3. `docs/reference.md` — env vars (incl. `PANDION_LOG`, `NO_COLOR`,
     `PANDION_PROFILE`, `PANDION_HOME`), config precedence, exit codes, the
     `~/.pandion` layout.
  4. `docs/troubleshooting.md` — lockdown's `wg-quick up` prerequisite (today
     discoverable only via the refusal message, `cmd/pandion/lockdown.go:71-78`),
     "attach says cluster torn down", budget errors, provider token issues,
     Scaleway's three credentials, Windows = WSL2.
  Move the two design notes to `docs/design/` to keep the top level user-facing.
- **Effort:** L (the generator makes #2 an M on its own)
- **Acceptance:** README links the four docs; schema-doc generator run is in CI
  (`git diff --exit-code` after `go generate`); every env var in the code appears
  in the reference (greppable test).

### P3.2 Examples: one path covered out of many

- **Problem:** `examples/` has exactly one entry (zmq-cluster: multi-node,
  source-sync, C++). Headline features — single node, Docker engine, GPU,
  `setup:`-based (pip/npm) workloads — have no runnable example, and there's no
  index.
- **Evidence:** `ls examples/` → `zmq-cluster` only; README links it 3×.
- **Fix:** Add `examples/single-node/` (smallest possible yaml + one command),
  `examples/docker-engine/`, `examples/gpu/` (with `--dry-run` cost note),
  `examples/python-setup/` (`setup:` + no C++ toolchain), each with the same
  README shape as zmq-cluster (validate → up → expected output → down), plus
  `examples/README.md` as an index with a "which example do I want" table.
- **Effort:** M · **Acceptance:** every example's yaml passes `pandion validate`
  in CI; index linked from README.

### P3.3 Completion install instructions are buried in a code comment

- **Problem / Evidence:** The three install one-liners exist only as a comment
  (`cmd/pandion/completion.go:22-26`); README never mentions completion.
- **Fix:** README "Shell completion" subsection under Install; also print the
  matching one-liner as a trailing comment when `pandion completion <shell>`
  runs on a TTY (to stderr so redirection stays clean).
- **Effort:** S · **Acceptance:** README section exists;
  `pandion completion bash > f` produces a clean script with the hint on stderr.

### P3.4 `debug share` prints a live bearer credential with no warning

- **Problem:** The `PDBG1-…` token embeds a private SSH key + WireGuard config;
  whoever holds it gets the grant until expiry. It's printed with only
  "# send that token to your teammate" — nothing marks it as a secret.
- **Evidence:** token assembly `cmd/pandion/debugshare.go:69` (`SSHKeyPEM`),
  print site `:183-188`.
- **Fix:** One line above the token: `# ⚠ this token IS the access — anyone
  holding it can attach until <expiry>. share over a private channel; revoke
  with: pandion debug unshare …`. Keep stdout machine-clean by putting the
  warning on stderr.
- **Effort:** S · **Acceptance:** warning present on TTY runs; token remains the
  only stdout line (script-safe), covered by `debugshare_test.go`.

---

## P4 — Long-running operation polish

### P4.1 Progress heartbeat for multi-minute waits

- **Problem:** `up` prints a stage banner, then can sit silent for many minutes
  inside a 25-minute overall ceiling ("waiting for cloud-init on all nodes...").
  Users can't tell hung from slow; when the deadline blows, the error is a raw
  wrapped `context deadline exceeded`.
- **Evidence:** single 25-min budget `cmd/pandion/cluster.go:578`; stage prints
  `:720, 745, 845, 884, 905, 951`; single-node budgets `main.go:479-482`.
- **Fix:** (a) a lightweight elapsed ticker on TTYs — reprint
  `  … still waiting (2m10s, node worker-1 pending)` every ~30s on stderr
  (plain lines, no spinner dependency; suppressed when not a TTY or `--quiet`);
  (b) wrap deadline errors per stage: `provisioning exceeded the 25m budget
  during "cloud-init readiness" — the cluster is left up; retry with pandion
  start, or tear down: pandion down --id X`.
- **Effort:** M · **Acceptance:** ticker visible on TTY, absent when piped;
  deadline error names the stage; no change to machine-read stdout.

### P4.2 Translate raw SDK/remote errors on failure paths

- **Problem:** Provider SDK errors (godo/hcloud/linodego HTTP errors) and raw
  remote `nft` stderr reach the user verbatim through `must()` and the `up`
  failure prints.
- **Evidence:** `must()` `cmd/pandion/main.go:1470-1475`; readiness/firewall
  failure prints `main.go:579, 695` (the `%s` dumps remote stderr);
  `sshcmd.go:119` shows the pattern done right ("is the openssh client installed?").
- **Fix:** A small `friendlyErr(err)` classifier used by `must()` and the up
  paths: 401/403 → "token invalid or lacks write scope — re-run pandion login";
  429 → "provider rate limit — retry shortly"; quota/capacity errors → named as
  such with the size/region that failed; anything else falls through verbatim
  (never *hide* the underlying error — append it after the hint).
- **Effort:** M · **Acceptance:** table-driven unit tests mapping sample SDK
  errors to hints; original error text always still present.

### P4.3 Local stream logs are truncated on every reconnect

- **Problem:** `~/.pandion/logs/<id>/<node>.log` opens with `O_TRUNC`, so the
  `attach` you run *to inspect a crash* destroys the evidence the previous
  session captured.
- **Evidence:** `internal/stream/stream.go:62-63`.
- **Fix:** Open `O_APPEND` with a session separator line
  (`----- attached 2026-07-13T10:42Z -----`); guard growth with a simple
  size-based rotation (`<node>.log.1`, keep one generation).
- **Effort:** S · **Acceptance:** stream unit test shows two sessions preserved
  with separator; rotation kicks in past the threshold.

### P4.4 Cost rendering consistency

- **Problem:** Unpriced providers render currency `?` and `—` cells that read as
  *broken* rather than *unpriced*; `ls` prints money at 4 decimals but the
  teardown receipt at 2.
- **Evidence:** `renderStatus` currency fallback `cmd/pandion/main.go:1004`;
  `money` `%.4f` vs receipt `%.2f` (`main.go:1254-1307`).
- **Fix:** Replace `?`-currency output with an explicit `(unpriced)` footer note
  and blank cost columns; single `money()` helper used by ls/receipt/dry-run
  (2 decimals ≥ 0.01, 4 below, one rule everywhere).
- **Effort:** S · **Acceptance:** golden tests for `ls` and receipt updated; the
  word "unpriced" appears instead of `?`.

---

## Cross-cutting: verification strategy

- **Unit:** registry drift test (P1.2), exit-code propagation (P0.1), flock
  contention (P0.5), validation-error golden tests (P2.1), schema-consumer drift
  test (P2.2), precedence matrix (P2.4), JSON schema goldens (P1.5), stream
  append/rotation (P4.3).
- **Offline smoke (`scripts/ci_smoke.sh`, runs on Linux/macOS/Windows):** add
  `--help`/`--version` exit-0 checks; mock `up`→`down`→`attach` asserting the
  "torn down" fast-fail (P0.2); piped `down`/`reap` symmetry (P0.4); scaffold →
  validate round-trip (P2.3); `PANDION_HOME` isolation (P2.5); piped output has
  no ANSI (P1.4).
- **Docs CI:** `go generate` for the schema reference is diff-clean (P3.1);
  every example validates (P3.2).
- **Non-goals (explicit in TODO.md — do not "fix" these):** no auto-restart of
  crashed workloads (fail-fast for GDB is a design choice), no AWS/GCP
  providers, no non-Ubuntu nodes, no cobra migration (see P1.2).

## Suggested execution order & sizing

| Order | Items | Size | Rationale |
|---|---|---|---|
| 1 | P0.1, P0.3, P0.4, P0.6 | ~3 days | Stops the CLI lying; small blast radius |
| 2 | P0.2, P0.5 | ~3 days | State lifecycle correctness; unlocks the "torn down" UX |
| 3 | P1.2 → P1.1 → P1.3 → P1.4 | ~3 days | Registry first — help/completion/consistency fall out of it |
| 4 | P2.1, P2.2, P2.3 | ~4 days | Config feedback loop: scaffold → validate-with-line-numbers → no silent fields |
| 5 | P3.1, P3.2, P3.3, P3.4 | ~4 days | Docs & examples once behavior above is settled (don't document twice) |
| 6 | P1.5, P1.6, P2.4, P2.5, P2.6 | ~4 days | Automation & precedence polish |
| 7 | P4.1–P4.4 | ~3 days | Feel & polish last |

Total ≈ 4–5 focused weeks. Phases 1–2 alone remove every identified footgun;
phases 1–4 are the recommended bar for a public release.
