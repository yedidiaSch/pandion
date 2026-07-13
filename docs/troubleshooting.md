# Troubleshooting

## "cluster ... was torn down on ..." on attach/ssh/start/lockdown

The cluster was `down`ed (or `reap`ed). Its manifest is tombstoned so reconnect
commands fail fast instead of hanging against dead IPs. Post-mortem logs are still
under `~/.pandion/logs/<id>/`. Bring a fresh cluster up with the same id to reuse it.

## `lockdown` refuses: "cannot SSH ... over the overlay"

`lockdown` is lockout-safe: it verifies it can reach **every** node over the
WireGuard overlay before removing public ingress. Join the overlay first:

```console
$ sudo wg-quick up ~/.pandion/keys/<id>/wg-<id>.conf
```

then retry. (This is why the overlay config path is printed at the end of `up`.)

## `up` exits non-zero but the cluster is still up

Exit **7** means a *post-provision* step failed (cloud-init readiness, mesh setup,
firewall, or a build-window command) and the cluster was **left up on purpose** for
triage. SSH in, fix or inspect, then tear down:

```console
$ pandion ssh --id <id> -- 'journalctl -xe | tail'
$ pandion down --id <id>
```

A non-zero exit that is **not** 7 and comes from `up`/`start`/`attach` is usually
your workload's own exit code (a crashed `run:` command) — the node is left up for
GDB/SSH.

## "budget cap exceeded" / `--max-cost`

The projected spend (hourly × TTL) exceeded `--max-cost`. Raise the cap, pick a
smaller `--size`, or lower `--ttl`. Unpriceable providers or `--no-ttl` with a cap
are a hard error by design — the message names exactly which knob to change.

## Provider token errors (401/403)

The token is missing, invalid, or lacks write scope. Store it in your keychain with
`pandion login --provider <name>`, or export the provider's env var (see
[reference.md](reference.md#environment-variables)). **Scaleway** needs three
values: `SCW_SECRET_KEY`, `SCW_ACCESS_KEY`, and `SCW_DEFAULT_PROJECT_ID` — the
preflight names whichever is missing.

## Escape codes in a captured log / pipe

Color is auto-disabled when stdout is not a terminal, and `NO_COLOR` forces it off.
If you still see codes, you're likely reading a raw streamed log file (those are
tee'd verbatim by design) rather than the console output.

## Windows

Use **WSL2**. Pandion drives the OpenSSH client and standard POSIX tooling; the
cross-platform smoke test runs on Windows, but day-to-day use expects a Linux/WSL2
shell.

## A "valid" config field seems ignored

Some schema fields are accepted but not yet wired to a backend (e.g. the top-level
`firewall:` block). `pandion validate` and `up` print a `warning:` for each such
field so it's never silently dropped — the hardened defaults are used instead.
