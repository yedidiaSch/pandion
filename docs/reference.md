# Reference

Env vars, exit codes, configuration precedence, and the on-disk layout.

## Environment variables

| Variable | Effect |
|---|---|
| `PANDION_HOME` | Directory that **replaces** `~/.pandion` for all state (journal, keys, logs, config, lockfiles). Handy for tests, shared machines, and XDG layouts. |
| `PANDION_PROFILE` | Selects the operator profile (same as the global `--profile` flag). |
| `PANDION_LOG` | `debug\|info\|warn\|error` — tees the structured audit trail to stderr. The global `--verbose` flag forces `debug`. |
| `NO_COLOR` | When set, disables ANSI color. Color is also auto-disabled when stdout is not a TTY. |
| `HCLOUD_TOKEN`, `DIGITALOCEAN_TOKEN`, `VULTR_API_KEY`, `LINODE_TOKEN`, `SCW_SECRET_KEY` (+ `SCW_ACCESS_KEY`, `SCW_DEFAULT_PROJECT_ID`) | Provider credentials, if not stored via `pandion login`. |

## Global flags

| Flag | Effect |
|---|---|
| `--profile NAME` | Use a named operator profile's defaults. |
| `--verbose` | Tee the audit trail to stderr (= `PANDION_LOG=debug`). |
| `--quiet` | Suppress progress chatter on stdout; leave results and stderr errors. |

Global flags work before or after the command (`pandion --quiet up …` or
`pandion up --quiet …`) and are never consumed from a run command after `--`.

## Exit codes

The CLI's contract with scripts and CI (defined in `cmd/pandion/exitcodes.go`):

| Code | Meaning |
|---|---|
| 0 | Success (or a `Ctrl+C` detach that left workloads running). |
| 1 | Generic / unclassified failure. |
| 2 | Bad flags/args, a missing `--id`, or a refused non-interactive prompt. |
| 3 | Missing manifest / cluster / node / key (incl. a torn-down cluster). |
| 4 | Debug-share token assembly failure. |
| 5 | Provisioning failed and the cluster was rolled back (nothing left). |
| 6 | Budget / lockdown / safety refusal (nothing changed). |
| 7 | Post-provision setup failed; the cluster is left **UP** for triage. |
| 8 | Teardown / reap could not reconcile to empty. |

A workload `run:` command exits with **its own** code (1–255), mirroring
`pandion ssh -- cmd` — so `up`/`start`/`attach` can exit with an arbitrary
workload status when the command fails.

## Configuration precedence

Highest priority wins:

```
flag  >  env  >  cluster.yaml  >  ~/.pandion/config.yaml  >  built-in default
```

`pandion validate --show-effective` prints the effective value **and its source**
for each resolved knob (provider, region, size, ttl, engine) — the tool for
answering "why did it pick that?".

## On-disk layout (`~/.pandion`, or `$PANDION_HOME`)

```
~/.pandion/
├── config.yaml            # operator defaults (default profile)
├── profiles/<name>.yaml   # named-profile defaults
├── state/<id>.json        # per-cluster state journal (crash-resume)
├── state/<id>.lock        # per-cluster advisory lock (cross-process)
├── keys/<id>/             # manifest.json, login/host keys, wg-<id>.conf
├── logs/<id>/<node>.log   # streamed run logs (kept after teardown)
└── lock/<id>.json         # reproducibility lockfile (toolchain versions)
```

Tokens are **not** stored here — they live in your OS keychain (`pandion login`).
