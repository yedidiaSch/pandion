# ZeroMQ cluster demo — broker + 2 workers

A ready-to-run cluster so you can see Pandion work **before writing any code**. It
provisions three hardened nodes, installs ZeroMQ, builds this C++ code on each, and
runs a classic ventilator/worker pipeline over the encrypted overlay:

- **broker** dispatches 30 tasks and collects the results, printing which worker did each;
- **worker-1** and **worker-2** pull tasks over the overlay, process them, and push results back.

It exercises the whole tool in one command: the toolchain + `libzmq3-dev`, workspace
**sync + build**, **service discovery** (`$PANDION_BROKER_IP`, `$PANDION_SELF_NAME`),
the **WireGuard mesh**, multiplexed streaming, and no-leak teardown.

## Run it

You need a provider API token (see the main README). This provisions three small
nodes for a couple of minutes — a few cents, and `down` removes everything.

```bash
cd examples/zmq-cluster

pandion validate -f cluster.yaml                              # free schema check
pandion up --provider=hetzner -f cluster.yaml --id zmq-demo   # or digitalocean|vultr|linode|scaleway
```

Watch the streamed output: both workers connect, the broker dispatches, the two
workers process tasks in parallel, and the broker prints the final split — e.g.

```
[worker-1] checked in with broker; waiting for tasks
[worker-2] checked in with broker; waiting for tasks
[broker] 2 worker(s) ready; dispatching 30 tasks over the overlay
[worker-1] processed task 2
[worker-2] processed task 1
[broker] result  1/30: task 1 done by worker-2
...
[broker] all 30 tasks complete. distribution:
[broker]   worker-1 handled 15 task(s)
[broker]   worker-2 handled 15 task(s)
```

The workers exit a few seconds after the tasks stop, so `up` returns on its own.
Then tear it down:

```bash
pandion down --provider=hetzner --id zmq-demo
```

## What to change next

- Add a `worker-3` node — it joins the mesh and shares the load, no code change.
- Point the workers at real work in `worker.cpp`; `pandion up` re-syncs and rebuilds.
- Swap in your own topology: this `cluster.yaml` is a normal Pandion cluster file.
