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

### 1. IPv6 is a semantic blind spot — **CONFIRMED LIVE**
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

**Decision required (forks the implementation):**
- **(a) Disable IPv6 on the node** via sysctl (`disable_ipv6 = 1`) — fail-closed,
  smallest change, matches the zero-trust default. Recommended MVP stance.
- **(b) Full dual-stack** — `ip6` allowlist set, IPv6 metadata endpoint block,
  `icmpv6` ND/RA accept in the ingress chain. More work; keeps IPv6 usable.

→ *e2e:* assert an IPv6 egress to a public host is denied under lockdown, and
that the chosen posture holds.

---

## P1 — Prove the posture where it isn't proven

### 2. Dedicated lockdown e2e (`e2e_lockdown.sh`)
The full deny-all / overlay-only-SSH flip is the strongest claim and has **no
self-cleaning e2e** of its own (unlike metadata/network/node). It should assert:
public SSH dead, overlay SSH works, egress denied, metadata blocked, and —
critically — the **lockout-safe refusal** fires when the overlay is unreachable.

### 3. Cross-provider hardening matrix
Every hardening e2e is Hetzner-only. The lockdown posture is exactly what breaks
per-provider (Lambda's firewall is account-wide and already special-cased).
Parameterize the hardening e2e across **DO, Lambda, Vultr, Scaleway, Linode** and
record a support matrix. Highest-value use of available runway: breadth-of-proof
beats new mechanism.

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
3. **→ Decide the IPv6 stance (P0.1) and ship it + its e2e.**  ← next
4. Lockdown e2e (P1.2), then the cross-provider matrix (P1.3).
5. DNS allowlist + dry-run mode (P2).
6. Park eBPF behind demand validation.

**Headline risk:** the posture is only proven on one provider. Spend the runway
on breadth-of-proof (P0–P1), not new mechanism.
