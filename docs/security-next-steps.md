# Pandion Security — Next Steps (nftables path)

**Decision of record:** stay with **nftables** for host-level network enforcement.
eBPF is deferred behind demand validation (see below). This document is the
prioritized plan for hardening and *proving* the current posture.

> Related: `internal/firewall/nftables.go` (the ruleset generator),
> `internal/harden/cloudinit.go` (sysctl/fail2ban/unprivileged-run baseline),
> `cmd/pandion/lockdown.go` (lockout-safe deny-all flip).

---

## Where things stand

**Shipping & tested:** default-deny ingress+egress, metadata-SSRF block,
lockout-safe lockdown, sysctl/fail2ban/unprivileged-run hardening. Proven by
`e2e_metadata_block.sh`, `e2e_network_hardening.sh`, `e2e_node_hardening.sh` —
**but only on Hetzner** (`HCLOUD_TOKEN`).

**In-flight branches:** ✅ resolved. `fix/e2e-lockdown-flag`,
`fix/lambda-overlay-firewall`, and `fix/provider-e2e-hardening` were all found
patch-equivalent to commits already in `main` (`git cherry` confirms) — i.e.
already landed. The stale local branches have been deleted; the matching
`origin/*` branches can be pruned.

**Loose end:** ✅ the Lambda 429 retry/throttle fix is committed on
`fix/lambda-429-backoff`.

---

## P0 — Correctness gaps that undermine the security claim

### 1. IPv6 is a semantic blind spot — ✅ **CLOSED (disable-on-node stance)**
`internal/firewall/nftables.go` uses `table inet` with `policy drop`, so IPv6 is
default-denied — not a gaping hole. But every *allow/deny* rule is IPv4-only:
the egress allowlist (`ip daddr @egress_ok`), the metadata block
(`ip daddr 169.254.169.254`), and ICMP (`ip protocol icmp` = v4 only; no
`icmpv6`/neighbor-discovery). Meanwhile nodes are **dual-stack**:
`internal/harden/cloudinit.go` *hardens* IPv6 sysctls but leaves IPv6 enabled,
and `internal/provider/vultr/vultr.go` already carries a comment about "a
dual-stack host where the SDK may egress over an unlisted IPv6."

Net effect under lockdown: IPv6 is effectively unusable *and* the
allowlist/metadata semantics silently don't extend to it.

**Resolved:** chose **(a) disable IPv6 on the node** (`disable_ipv6 = 1` in
`sysctlHardening`) — fail-closed, matches the zero-trust default, overlay is
IPv4 so no loss of function. Covered by `TestBuild_Sysctl*` and the real-node
`scripts/e2e_ipv6_lockdown.sh` (asserts IPv6 off, no global v6 address, v6
egress fails). Full dual-stack nftables remains the fallback if IPv6 egress is
ever required.

---

## P1 — Prove the posture where it isn't proven

### 2. Dedicated lockdown e2e — 🚧 **written, pending hand-run** (`scripts/e2e_lockdown.sh`, PR #105)
Asserts both halves of the deny-all / overlay-only-SSH flip: the **lockout-safe
refusal** (overlay down ⇒ `lockdown` exits 6, changes nothing, public SSH
survives) and the real flip (overlay up ⇒ public SSH dead, overlay SSH lives).

### 3. Cross-provider hardening matrix — 🚧 **written, pending hand-run** (`scripts/e2e_hardening_matrix.sh`)
Every hardening e2e was Hetzner-only. This runs one probe per provider (whose
credential is present) and prints a support matrix of the core invariants —
`meta` (metadata blocked), `egress` (default-deny holds), `v6` (IPv6 disabled),
`ssh` (workload ran least-priv) — across **Hetzner, DO, Vultr, Linode,
Scaleway**. Token-gated (absent creds ⇒ skip), self-cleaning. Highest-value use
of runway: breadth-of-proof beats new mechanism.

---

## P2 — Capability improvements (still nftables)

### 4. DNS-name egress allowlisting
Today's allowlist is *resolved IPs*; "allow `github.com`" goes stale as CDN IPs
rotate. Add periodic re-resolution so name-based egress rules stay correct.

### 5. Audit / dry-run mode
A "log what *would* be dropped, don't enforce" mode. Lowers the friction of
adopting default-deny egress and pairs with the existing `internal/audit` trail.
Cheap in nftables (a `log` rule variant).

---

## P3 — Deferred by decision

**eBPF / process-identity egress** stays a *validate-first* item, not scheduled
work. The performance angle (XDP) is overkill for compute-bound GPU nodes.
The identity angle ("only the sanctioned binary may egress") is the only thing
that would justify eBPF — revisit **only** if user conversations confirm demand.
See `ebpf.md` for the full rationale.

---

## Execution order

1. ✅ Commit the Lambda 429 fix.
2. ✅ Confirm the three in-flight branches are already merged; delete the stale ones.
3. ✅ IPv6 stance (P0.1): disable-on-node, shipped with e2e.
4. 🚧 Lockdown e2e (P1.2) + cross-provider matrix (P1.3): written; **pending a hand-run against real cloud** before merge.
5. **→ DNS allowlist + dry-run mode (P2).**  ← next

6. Park eBPF behind demand validation.

**Headline risk:** the posture is only proven on one provider. Spend the runway
on breadth-of-proof (P0–P1), not new mechanism.
