# Pandion GPU Extension — Design & Implementation Plan

Status: **draft / proposed**  ·  Owner: TBD  ·  Target: milestone series **G0–G4**

Extends Pandion from CPU-only ephemeral infrastructure into on-demand GPU
compute — **without a new subsystem**. GPU is modeled as a *capability filter*
over the existing provider seam, so the WireGuard overlay, eBPF/XDP `lockdown`,
`ssh`/`code`/`debug`, `reap`, TTL, `--max-cost`, lockfile, and profiles all keep
working unchanged.

---

## 1. Design principles

1. **GPU is a capability filter, not a mode.** `pandion up --gpu a100` means
   "resolve to a provider that can serve an A100, pick the SKU, inject a
   CUDA-native image." No GPU-specific branch belongs in `runUp` / the
   orchestrator; if one appears, the boundary is drawn wrong.
2. **Reuse the optional-capability pattern.** The provider seam
   (`internal/provider/provider.go`) already layers optional interfaces on a
   minimal core: `AuxReaper`, `ClusterFirewaller`, `Pricer`. GPU adds exactly
   one more — `GPUProvider` — and CPU-only backends are untouched.
3. **Safety stays in the OSS core.** `--ttl` and `--max-cost` already live in
   OSS and must keep guarding GPU nodes. The commercial layer is *org-wide
   policy* (non-overridable caps, RBAC, audit), never the individual's seatbelt.
4. **Fail closed on money.** GPU nodes cost 10–100× a CPU node. Every path that
   can't price a node must error, never silently skip the budget guard — the
   existing `EstimateSpend` contract (`orchestrator.go:276`) already does this;
   GPU pricing must honor it.
5. **Lead with private AI fine-tuning** as the public use case. Ship
   hash-cracking as a "security research" template, not the billboard (provider
   ToS + reputational reasons).

---

## 2. Where GPU touches the existing code

| Concern | Existing code | Change |
|---|---|---|
| Provider contract | `internal/provider/provider.go` | Add `GPUProvider` optional interface + `GPU` fields on `ServerSpec`/`Server` |
| Type discovery | `internal/provider/hetzner/selecttype.go` (`selectCandidates`) | New GPU-aware selector per GPU provider |
| Provisioning flow | `cmd/pandion/main.go` `runUp` → `upHetzner` | Add `--gpu` flag; thread `GPUReq` into `ServerSpec`; image resolution |
| Idle dead-man | `internal/harden/cloudinit.go` `deadmanScript` | Add GPU-utilization signal (SSH-only today — **the key correctness fix**) |
| Pricing / budget | `internal/orchestrator/orchestrator.go` (`Pricer`, `EstimateSpend`, `CheckBudget`) | GPU SKUs priced via same `Pricer`; no interface change |
| Cost visibility | `runLs` + `LsView` (`cmd/pandion/main.go`) | Add GPU util%, $/hr, time-to-reap columns |
| Teardown | orchestrator `Down` / `reap` | Emit a cost **receipt** |
| Defaults | `internal/userconfig/userconfig.go` `Defaults` | Add `gpu`, `max_cost` per-profile defaults |
| CLI help/completion | `usage()`, `completion.go` | `--gpu`, `list-gpus` |

---

## 3. Provider seam changes

### 3.1 `ServerSpec` / `Server` (`internal/provider/provider.go`)

Add a GPU request to the spec and a realized descriptor to the result. Both are
zero-valued for CPU nodes, so nothing changes for existing providers.

```go
// GPUReq is an optional GPU requirement on a ServerSpec. A zero GPUReq (Count==0)
// means "no GPU" — CPU-only providers ignore it entirely.
type GPUReq struct {
    Model   string // normalized model, e.g. "a100", "h100", "rtx4090"; "" = any
    Count   int    // number of GPUs; 0 = none
    MinVRAM int    // minimum VRAM per GPU in GB; 0 = don't care
}

// on ServerSpec:
    GPU GPUReq // optional; zero = CPU-only

// GPUInfo is the realized GPU on a provisioned Server (for `ls` + receipts).
type GPUInfo struct {
    Model string
    Count int
    VRAM  int // GB per GPU
}

// on Server:
    GPU GPUInfo // zero for CPU nodes
```

### 3.2 New optional interface: `GPUProvider`

```go
// GPUProvider is an optional Provider capability: it can serve GPU instances.
// A backend implements it only if it offers GPUs; the resolver uses it to answer
// "can this provider satisfy --gpu?" and to enumerate offerings for `list-gpus`.
type GPUProvider interface {
    // GPUOfferings lists the GPU SKUs this provider can currently serve, priced.
    // Used by `pandion list-gpus` and by --gpu resolution. Results SHOULD be
    // cached (see §6) — this may hit a live pricing/availability API.
    GPUOfferings(ctx context.Context) ([]GPUOffering, error)

    // ResolveGPUType maps a GPUReq to a concrete provider server type (the GPU
    // analog of selectCandidates). Returns the type name CreateServer will use,
    // or an error if the request can't be satisfied. MUST be deterministic and
    // cheapest-first for a given req, so --dry-run and up agree.
    ResolveGPUType(ctx context.Context, req GPUReq, regionPref []string) (typeName, region string, err error)
}

// GPUOffering is one purchasable GPU SKU.
type GPUOffering struct {
    ServerType string  // provider type name, e.g. "gpu_1x_a100"
    GPU        GPUInfo // model/count/vram
    Regions    []string
    Hourly     Money
    Image      string // recommended CUDA-native image for this SKU
}
```

`CreateServer` needs no signature change: it already receives `ServerSpec`, now
carrying `spec.GPU`. A GPU provider's `CreateServer` reads `spec.GPU` (or the
pre-resolved `spec.Type`) and picks the CUDA-native image if `spec.Image` is
empty (see §5).

### 3.3 Why not a separate `GPUServerSpec`?

Because every downstream stage — overlay join, harden, SSH, workspace sync,
`reap`, budget — operates on `ServerSpec`/`Server`. A parallel type would fork
all of them. Adding two optional fields keeps the single spine.

---

## 4. The idle-detection problem (highest-priority correctness fix)

Today's dead-man's-switch (`internal/harden/cloudinit.go:327`,
`deadmanScript`) refreshes the heartbeat **only** while an SSH connection is
established on `:22`:

```sh
if ss -Htn state established '( sport = :22 )' 2>/dev/null | grep -q .; then
  mkdir -p /run/pandion && touch "$HB"
fi
```

For a GPU node this is **wrong in both directions**:

- A training/cracking job running under `nohup`/`tmux`/systemd with **no live
  SSH** looks idle → the node powers off mid-epoch and destroys hours of work
  and money.
- A live-but-idle Jupyter kernel with an SSH tunnel open looks busy forever →
  the node never reaps and bleeds money — exactly the "$5k H100 weekend."

### Fix: GPU-utilization as a first-class liveness signal

Extend the deadman to also refresh the heartbeat when GPU utilization is above a
threshold. On a GPU node the script gains:

```sh
# GPU liveness: fresh while any GPU is doing real work.
if command -v nvidia-smi >/dev/null 2>&1; then
  util=$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits 2>/dev/null \
         | awk '{ if ($1+0 > BUSY) hot=1 } END { print hot+0 }' BUSY=$GPU_BUSY_PCT)
  [ "$util" = "1" ] && touch "$HB"
fi
```

Design points:

- **Two thresholds, one timer.** Keep the SSH refresh (interactive sessions) AND
  add the GPU-util refresh (headless jobs). Either keeps the node alive.
- **`GPU_BUSY_PCT` default 5%.** Above idle-driver noise, below any real kernel.
  Tunable via `--gpu-idle-util`.
- **Sustained, not instantaneous.** Poll stays at 1-minute cadence
  (`deadmanTimer`); the TTL window (default 60m) already provides the "sustained"
  smoothing — a job dipping to 0% between epochs for <TTL won't trip it. For
  tighter control, a future `--gpu-idle-window` can require N consecutive idle
  polls.
- **CPU nodes unchanged.** The `nvidia-smi` branch is only emitted when
  `IdleTTL > 0 && spec.GPU.Count > 0`, gated in `CloudInit` assembly. Requires a
  new `HasGPU bool` (or reuse the presence of a GPU field) on the harden config
  struct.

### Config plumbing

`internal/harden/cloudinit.go` `CloudInit` gains `GPUIdleUtil int` (percent) and
a signal that the node is a GPU node. `deadmanScript` takes both `ttlSec` and the
GPU threshold; when GPU, it emits the extended script.

---

## 5. Provider tiers & the driver strategy

The "CUDA dependency nightmare" is **not universal** — it's per tier. Do not
write one universal driver installer.

### Tier A — GPU-native clouds (VMs with pre-baked CUDA)

Lambda Labs, CoreWeave, Paperspace. The base image already has the NVIDIA driver
+ CUDA + container runtime. **Provisioning = pick the SKU, use the vendor's
GPU image, done.** The existing kernel-space WireGuard + eBPF/XDP `lockdown`
stack works unchanged (full VM, `CAP_SYS_ADMIN` available). `GPUOffering.Image`
carries the recommended image; `CreateServer` uses it when `spec.Image == ""`.

### Tier B — generic cloud + GPU SKU

Scaleway GPU instances, some AWS. VM is generic; you must **inject the NVIDIA
container runtime** at instantiation. Do it via a cloud-init snippet that pulls
`nvidia/cuda` base + installs `nvidia-container-toolkit` — **not** an ad-hoc SSH
script (brittle, slow, unversioned). This reuses the existing `--engine docker`
path (`main.go:227`): a GPU node forces `docker` engine with the NVIDIA runtime.

### Tier C — managed containers (deferred to G4)

RunPod, Vast.ai. **No kernel privileges** → eBPF/XDP and kernel WireGuard are
unavailable. This is the "container kernel trap": the agent must detect missing
`CAP_BPF`/`CAP_SYS_ADMIN` and **fall back** to userspace WireGuard
(`wireguard-go`/netstack) with services bound to `127.0.0.1` and ingress locked
at the provider API. This is a **separate, harder workstream** and must not gate
v1.

### Rollout consequence

> **Ship Lambda Labs first, alone, end-to-end.** Raw VMs + pre-baked CUDA +
> clean API = zero fallback complexity, proves the whole GPU path on the existing
> security stack. Tier B is a second provider; Tier C is a distinct milestone.

---

## 6. Cost as the trust/observability surface

Pandion is zero-backend, so there's no dashboard — turn that into the feature.
A GPU user's #1 anxiety is *"what's running and what is it costing me right
now?"*

### 6.1 `ls` becomes the FinOps panel

`runLs` / `LsView` already render `Hourly` and `Accrued` (`Money`, per node).
Add, for GPU nodes:

- `GPU` (model×count)
- `UTIL%` — last-known GPU utilization (from a lightweight node metric; MVP can
  omit and show `—`)
- `$/HR` and `SPENT` (already have the money)
- `→REAP` — time until the idle dead-man would fire (derived from TTL − idle age)

`--json` gains the same fields for scripting.

### 6.2 Teardown receipt

On `down`/`reap`, print a closing line:

```
✔ torn down cluster "ft-run" — 1×H100 ran 2h13m · €0.00 leaked · total €5.31
```

Cheap (the orchestrator already tracks create time + hourly) and
disproportionately valuable for trust. Emit from the orchestrator `Down` path so
both `down` and `reap` get it.

### 6.3 Offline pricing cache

`GPUOfferings` may hit a live pricing/availability API. Cache the offerings under
`~/.pandion/cache/<provider>-gpu.json` with a short TTL so `--dry-run` and
`list-gpus` work fast and offline (mirrors the existing `mock` offline story).
Stale cache is fine for preview; `up` re-validates availability at
`CreateServer`.

---

## 7. CLI surface

```
pandion up --gpu a100[:2] [--gpu-idle-util 5] [--engine docker] -- ./train.sh
pandion up --gpu h100 --max-cost 20 --ttl 3h -- python finetune.py
pandion list-gpus [--provider lambda] [--json]   # enumerate priced GPU SKUs
```

- `--gpu MODEL[:COUNT]` — `a100`, `h100`, `rtx4090`, …; `:2` = two GPUs. Parsed
  into `GPUReq{Model, Count}`.
- `--gpu-idle-util PCT` — GPU-utilization idle threshold (default 5).
- Resolution order (mirrors provider resolution, `resolve.go`):
  `--provider` → profile default → the only GPU-capable provider with creds →
  (on a terminal) prompt. If `--gpu` is set and the resolved provider does not
  implement `GPUProvider`, error clearly:
  `provider "hetzner" has no GPU offerings — try --provider lambda`.
- **Profile integration** (ties to the just-shipped profiles): add `gpu` and
  `max_cost` to `userconfig.Defaults`, so a `work` profile can pin
  "lambda / h100 / €20 cap" and `up --gpu` needs no other flags.

### `usage()` / completion

Add `--gpu`, `--gpu-idle-util`, and the `list-gpus` subcommand to `usage()`
(`main.go`), `completionCommands`, and the `--gpu`/`--provider` completion cases
in `completion.go`.

---

## 8. Milestones

| ID | Deliverable | Gate |
|---|---|---|
| **G0** ✅ | Seam only: `GPUReq`/`GPUInfo` on spec/server, `GPUProvider` interface, `--gpu` flag parsed and threaded to `ServerSpec` (no provider yet). `mock` grows fake GPU offerings so tests + `--dry-run` + `list-gpus` work offline. | **Done.** Unit tests green; `--dry-run --gpu a100` prints a priced plan on mock; `list-gpus` renders the offline catalog; `--max-cost` fails closed on an unpriceable GPU. |
| **G1** 🚧 | **Lambda Labs provider** (Tier A): `CreateServer` with GPU SKU + CUDA-native image, `Pricer`, `GPUProvider`. End-to-end `up --gpu` on a real A100 with overlay + `lockdown` + `reap`. | **Provider landed + wired + offline-tested** (`internal/provider/lambda`, httptest stub covers catalog/resolve/price/launch/terminate/list; CLI resolves `--provider lambda`). **Pending:** live-cloud e2e (needs `LAMBDA_API_KEY`; read-only smoke test gated behind `PANDION_IT=1`). |
| **G2** ✅ | **GPU-aware idle dead-man** (§4) + teardown **receipt** (§6.2) + `ls` GPU/cost columns (§6.1). | **Done.** Deadman treats GPU util as liveness (`--gpu-idle-util`, default 5%), gated to GPU nodes; `down` prints a cost receipt; `ls` (+`--json`) shows a GPU column. Golden-tested in `cloudinit_test.go`; receipt/ls covered in `cmd/pandion`. Live behavior (headless job survives, idle box reaps) verifies on real hardware in G1's e2e. |
| **G3** | **Tier B** provider (Scaleway GPU) via injected NVIDIA container runtime on the `--engine docker` path; offline pricing cache (§6.3). | Driver injection works from cloud-init, no SSH scripts |
| **G4** | **Tier C** (RunPod/Vast.ai): capability detection + userspace-WireGuard fallback + API-level ingress lock. | Node joins overlay with no `CAP_BPF`; services bound to loopback |

G0–G2 are the MVP: a real, safe, single-provider GPU story. G3–G4 broaden reach.

---

## 9. Edge cases & pitfalls

1. **Availability, not just price.** GPU SKUs are frequently sold out per-region.
   `ResolveGPUType` must walk a (type × region) plan like
   `hetzner/selecttype.go:searchPlan` and surface a clear
   "A100 unavailable in requested regions, available in us-west" message rather
   than a raw API error.
2. **Idle-window flapping.** A job that dips to 0% between epochs must not reap.
   The TTL window smooths this; document that `--ttl` should exceed the longest
   expected inter-epoch gap. Offer `--gpu-idle-window` later if needed.
3. **Driver/image mismatch.** Never install drivers via SSH. If Tier B injection
   fails, fail the `up` loudly with the cloud-init log tail — a half-provisioned
   GPU box still bills.
4. **Budget must fail closed for GPU.** `EstimateSpend` already errors when a
   node can't be priced (`orchestrator.go:293`). GPU pricing MUST populate
   `Money` or the `--max-cost` guard silently disappears on the most expensive
   nodes. Add a test asserting an unpriceable GPU SKU fails the budget check.
5. **Don't hardcode SKUs/regions/prices.** Drive everything from
   `GPUOfferings`; the binary ships no GPU price table (mirrors the S1/F3
   no-hardcoded-types rule in `selecttype.go`).
6. **Multi-GPU count vs. multi-node.** `--gpu a100:8` = one 8-GPU box;
   `-f cluster.yaml` with 8 single-GPU nodes = a mesh. Keep the two orthogonal;
   `GPUReq.Count` is per-node.
7. **Orchestration vs. execution.** Pandion provisions + meshes + injects; it
   does **not** write a distributed engine. Feed mesh IPs to Hashcat
   (`--skip/--limit`) or PyTorch/Ray. No custom scheduler.
8. **Cracking framing.** Ship it as a template, not the headline; several
   providers' ToS forbid it — a public "crack passwords" pitch risks account
   bans that would poison the whole project.

---

## 10. Testing strategy

- **Unit (offline, `mock`):** `mock` implements `GPUProvider` with a fixed
  offering table → tests for `--gpu` parsing, `ResolveGPUType` cheapest-first,
  budget-fails-closed on unpriceable GPU, `--dry-run` cost math, `list-gpus`
  render.
- **Cloud-init golden tests:** extend `internal/harden/cloudinit_test.go` to
  assert the GPU deadman script is present iff `HasGPU && IdleTTL>0`, and absent
  otherwise (mirrors the existing `pandion-deadman` assertions at
  `cloudinit_test.go:98,240`).
- **Provider integration (tagged, opt-in):** Lambda `CreateServer`/pricing behind
  a build tag + env creds, like `hetzner_integration_test.go`.
- **e2e (G1 gate):** real `up --gpu` → overlay reach → `lockdown` → `ssh nvidia-smi`
  → `reap` → receipt.

---

## 11. Open questions

1. **First provider — Lambda vs. CoreWeave?** Lambda: simplest API, strong AI-dev
   mindshare. CoreWeave: broader SKUs, more enterprise. Recommendation: **Lambda
   for G1**, CoreWeave as a fast follow (same Tier A path).
2. **GPU util metric in `ls`** — push (node → nowhere, zero-backend) is out;
   pull via a one-shot `ssh nvidia-smi` on `ls` is simple but adds latency.
   MVP: show `—`, add opt-in `ls --gpu-util` that SSHes.
3. **Enterprise split** — non-overridable org caps + RBAC on who may launch
   H100s + SIEM audit of GPU spend. Keep the *per-invocation* `--max-cost` in
   OSS; sell centralized *policy*.
4. **`--gpu` on `-f cluster.yaml`** — per-node `gpu:` key in the topology schema
   (`internal/config`). Design in G1, implement post-MVP.

---

## 12. Summary

The three highest-leverage moves, in order:

1. **GPU-util idle detection** (§4) — without it, GPU nodes either die mid-job or
   bleed money. This is the one non-negotiable correctness fix.
2. **Lambda-first, single-path rollout** (§5, G1) — proves the whole GPU story on
   the existing overlay + eBPF stack with zero fallback complexity.
3. **Cost as the trust surface** (§6) — `ls` FinOps panel + teardown receipt turn
   the zero-backend constraint into the thing that makes people trust running an
   H100.

Everything else rides the seam Pandion already has.
