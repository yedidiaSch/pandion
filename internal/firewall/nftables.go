// Package firewall generates nftables rulesets for host-level network hardening.
//
// M2 increment 1 implements the egress build-window validated by spike S2:
// during provisioning the node fetches its toolchain with egress open, then
// EnvCore applies a default-deny egress policy BEFORE running the user command,
// so a compromised workload cannot exfiltrate or phone home. Ingress is also
// default-deny, keeping only SSH (and any declared ports) reachable.
//
// The generator is a pure function so it is unit-tested offline; application on
// the node happens over the pinned SSH connection.
package firewall

import (
	"fmt"
	"sort"
	"strings"
)

// Spec describes the desired host firewall.
type Spec struct {
	// SSHPort is allowed inbound so the control connection survives (default 22).
	SSHPort int
	// SSHFromCIDR, if set, restricts inbound SSH to this source (e.g. the
	// operator's IP as "203.0.113.4/32"). Empty = allow SSH from anywhere.
	SSHFromCIDR string
	// WGPort, if non-zero, allows inbound UDP for the WireGuard overlay so the
	// operator (and, later, sibling nodes) can reach it. This is the ONE public
	// port that stays open under the hardened posture.
	WGPort int
	// AllowOverlayInput accepts any traffic arriving on the wg0 interface, so
	// management/IPC over the encrypted overlay is unrestricted.
	AllowOverlayInput bool
	// IngressPorts are additional inbound TCP ports (e.g. IPC) to allow.
	IngressPorts []int
	// EgressAllowIPs are outbound-allowed IPv4 addresses/CIDRs (already resolved).
	// Empty means: no new outbound except DNS + established (see AllowDNS).
	EgressAllowIPs []string
	// AllowDNS permits outbound DNS so name resolution still works under lockdown.
	AllowDNS bool
}

func (s Spec) normalize() Spec {
	if s.SSHPort == 0 {
		s.SSHPort = 22
	}
	return s
}

// NFTables renders an atomic nftables ruleset. Applying it with `nft -f` replaces
// the whole ruleset in one transaction, so there is no window where the control
// SSH connection is dropped (established traffic is always accepted).
func NFTables(in Spec) string {
	s := in.normalize()
	var b strings.Builder
	b.WriteString("flush ruleset\n")
	b.WriteString("table inet envcore {\n")

	if len(s.EgressAllowIPs) > 0 {
		ips := append([]string(nil), s.EgressAllowIPs...)
		sort.Strings(ips)
		b.WriteString("  set egress_ok {\n")
		b.WriteString("    type ipv4_addr; flags interval;\n")
		b.WriteString("    elements = { " + strings.Join(ips, ", ") + " }\n")
		b.WriteString("  }\n")
	}

	// inbound: default drop, keep the control plane + declared ports reachable.
	b.WriteString("  chain input {\n")
	b.WriteString("    type filter hook input priority 0; policy drop;\n")
	b.WriteString("    iif \"lo\" accept\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    ip protocol icmp accept\n")
	if s.AllowOverlayInput {
		b.WriteString("    iif \"wg0\" accept\n") // trust the encrypted overlay
	}
	if s.WGPort != 0 {
		b.WriteString(fmt.Sprintf("    udp dport %d accept\n", s.WGPort)) // WireGuard
	}
	if s.SSHFromCIDR != "" {
		b.WriteString(fmt.Sprintf("    ip saddr %s tcp dport %d accept\n", s.SSHFromCIDR, s.SSHPort))
	} else {
		b.WriteString(fmt.Sprintf("    tcp dport %d accept\n", s.SSHPort))
	}
	for _, p := range s.IngressPorts {
		b.WriteString(fmt.Sprintf("    tcp dport %d accept\n", p))
	}
	b.WriteString("  }\n")

	// outbound: default drop — exfiltration protection (S2).
	b.WriteString("  chain output {\n")
	b.WriteString("    type filter hook output priority 0; policy drop;\n")
	b.WriteString("    oif \"lo\" accept\n")
	b.WriteString("    ct state established,related accept\n")
	if s.AllowDNS {
		b.WriteString("    udp dport 53 accept\n")
		b.WriteString("    tcp dport 53 accept\n")
	}
	if len(s.EgressAllowIPs) > 0 {
		b.WriteString("    ip daddr @egress_ok accept\n")
	}
	b.WriteString("  }\n")

	b.WriteString("}\n")
	return b.String()
}
