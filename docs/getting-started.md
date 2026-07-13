# Getting started with Pandion

Pandion provisions hardened, ephemeral cloud dev/CI nodes, runs your command on
them, streams the output, and tears everything down — no control plane, no agents.

This walks the whole loop: install → configure → scaffold → validate → up →
attach/ssh → down.

## 1. Install

See the [README Install section](../README.md#install) for the binary/`go install`
options. Verify it runs:

```console
$ pandion --version
pandion dev
$ pandion --help          # top-level command list (also: pandion help)
```

Every command has its own help with a synopsis and an example:

```console
$ pandion up -h
```

## 2. Configure an operator profile

`pandion init` stores a default provider (and optional region/size/TTL) so bare
commands work without flags. Tokens go in your OS keychain, never in a file.

```console
$ pandion init --provider hetzner        # prompts for a token on a terminal
```

Prefer to try it with zero cost and no cloud account? Use the offline mock:

```console
$ pandion init --provider mock
$ pandion demo                            # full lifecycle, offline
```

## 3. Scaffold and validate a topology

```console
$ pandion init --cluster                  # writes a commented cluster.yaml
$ pandion validate -f cluster.yaml        # check it against the schema
$ pandion validate -f cluster.yaml --show-effective   # see which value wins per knob
```

Validation errors point at the exact YAML line and suggest fixes:

```
cluster.yaml:5:5: nodes[0].runn: unknown field "runn" (did you mean "run"?)
```

The full field reference is generated from the schema: [cluster-yaml.md](cluster-yaml.md).

## 4. Bring it up

```console
$ pandion up -f cluster.yaml --id my-cluster
```

`up` provisions each node, applies the hardened cloud-init, forms the WireGuard
mesh, syncs + builds your workspace, then runs each node's `run:` command and
streams the multiplexed output. The first status line names the **idle-poweroff
TTL** — an idle node powers *itself* off (default 60m) unless you pass `--no-ttl`.

`Ctrl+C` **detaches** (workloads keep running); reconnect with `pandion attach`.

A single node needs no file at all:

```console
$ pandion up --id quick -- 'echo hello from the cloud && uname -a'
```

## 5. Work with a running cluster

```console
$ pandion attach --id my-cluster          # reconnect to the live streams
$ pandion ssh --id my-cluster --node server -- uptime
$ pandion cp  --id my-cluster ./data :/tmp/data
$ pandion ls                              # every live cluster + cost (add --json)
```

## 6. Tear it down

```console
$ pandion down --id my-cluster            # reconcile the provider to empty
```

After `down`, the cluster is tombstoned locally: `attach`/`ssh`/`start` on that id
fail fast with "was torn down" instead of hanging against dead IPs. Post-mortem
logs are kept under `~/.pandion/logs/<id>/`.

## Where to next

- [cluster-yaml.md](cluster-yaml.md) — every field, generated from the schema.
- [reference.md](reference.md) — env vars, exit codes, config precedence, layout.
- [troubleshooting.md](troubleshooting.md) — common errors and fixes.
- [examples/](../examples/) — single-node, Docker, GPU, and Python setups.
