# Examples

Each directory is a self-contained, runnable topology. Validate any of them with
`pandion validate -f <dir>/cluster.yaml`, then `pandion up -f <dir>/cluster.yaml`.

| Example | Use it when you want… |
|---|---|
| [single-node](single-node/) | The smallest thing that works — one node, one command. |
| [docker-engine](docker-engine/) | The workload to run inside a hardened container image. |
| [gpu](gpu/) | A GPU node (needs a GPU-capable provider; preview cost with `--dry-run`). |
| [python-setup](python-setup/) | A Python workload with pip deps via `setup:` (no C++ toolchain). |
| [zmq-cluster](zmq-cluster/) | A multi-node, source-synced C++ cluster over the overlay. |

All example configs are validated in CI. See the
[cluster.yaml reference](../docs/cluster-yaml.md) for every field.
