// SPDX-License-Identifier: AGPL-3.0-or-later

package firewall

import (
	"strings"
	"testing"
)

func TestNFTables_BlockMetadata(t *testing.T) {
	// off by default
	if strings.Contains(NFTables(Spec{}), "169.254.169.254") {
		t.Fatal("metadata drop should be absent unless BlockMetadata is set")
	}

	// on: the drop is present and UNCONDITIONAL — it must come BEFORE the
	// egress-allow accept, so even a wide-open allowlist can't reach metadata.
	out := NFTables(Spec{BlockMetadata: true, EgressAllowIPs: []string{"0.0.0.0/0"}})
	drop := strings.Index(out, "ip daddr 169.254.169.254 drop")
	allow := strings.Index(out, "ip daddr @egress_ok accept")
	if drop < 0 {
		t.Fatalf("metadata drop missing:\n%s", out)
	}
	if allow < 0 || drop > allow {
		t.Fatalf("metadata drop must precede egress-allow accept (drop=%d allow=%d):\n%s", drop, allow, out)
	}
}

func TestNFTables_DefaultDenyBothChains_SSHOpen(t *testing.T) {
	out := NFTables(Spec{AllowDNS: true})

	// both hooks default-drop (ingress + egress locked down)
	if strings.Count(out, "policy drop;") != 2 {
		t.Fatalf("expected policy drop on both chains:\n%s", out)
	}
	// control plane preserved
	for _, want := range []string{
		"flush ruleset",
		"ct state established,related accept",
		"tcp dport 22 accept", // default SSH port
		"udp dport 53 accept", // DNS allowed for resolution
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset missing %q", want)
		}
	}
	// with no allowlist, there must be no egress set nor @egress_ok reference
	if strings.Contains(out, "@egress_ok") || strings.Contains(out, "set egress_ok") {
		t.Errorf("empty allowlist must not emit an egress set:\n%s", out)
	}
}

func TestNFTables_EgressAllowlistAndIngressPorts(t *testing.T) {
	out := NFTables(Spec{
		SSHPort:        2222,
		IngressPorts:   []int{5557, 5558},
		EgressAllowIPs: []string{"185.12.64.1", "185.12.64.2"},
		AllowDNS:       true,
	})
	for _, want := range []string{
		"tcp dport 2222 accept", // custom SSH port
		"tcp dport 5557 accept", // IPC ingress
		"tcp dport 5558 accept",
		"set egress_ok {",
		"185.12.64.1, 185.12.64.2", // sorted allowlist elements
		"ip daddr @egress_ok accept",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset missing %q\n%s", want, out)
		}
	}
}

func TestNFTables_OverlayAndOperatorRestrictedSSH(t *testing.T) {
	out := NFTables(Spec{
		SSHFromCIDR:       "203.0.113.4/32",
		WGPort:            51820,
		AllowOverlayInput: true,
		AllowDNS:          true,
	})
	for _, want := range []string{
		"iif \"wg0\" accept",                          // trust the overlay (ingress)
		"oif \"wg0\" accept",                          // node-initiated overlay egress (F14)
		"udp dport 51820 accept",                      // WireGuard port (both chains)
		"ip saddr 203.0.113.4/32 tcp dport 22 accept", // SSH only from operator
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ruleset missing %q\n%s", want, out)
		}
	}
	// overlay egress must be in the OUTPUT chain, not just input (the F14 bug)
	oi := strings.Index(out, "chain output")
	if oi < 0 || !strings.Contains(out[oi:], "oif \"wg0\" accept") {
		t.Errorf("output chain must allow overlay egress (oif wg0):\n%s", out)
	}
	// with a restricted source, there must be NO unrestricted "tcp dport 22 accept"
	if strings.Contains(out, "\n    tcp dport 22 accept\n") {
		t.Errorf("SSH must be restricted to the operator source, found unrestricted rule:\n%s", out)
	}
}

func TestNFTables_L2Overlay(t *testing.T) {
	out := NFTables(Spec{WGPort: 51820, AllowOverlayInput: true, AllowL2Input: true, AllowDNS: true})
	// decapsulated L2 frames accepted on vxlan0 (input).
	if !strings.Contains(out, "iifname \"vxlan0\" accept") {
		t.Errorf("input must accept decapsulated vxlan0 frames:\n%s", out)
	}
	// VXLAN egress allowed but SCOPED to the overlay subnet (no internet hole).
	oi := strings.Index(out, "chain output")
	if oi < 0 || !strings.Contains(out[oi:], "ip daddr 10.99.0.0/24 udp dport 4789 accept") {
		t.Errorf("output must allow scoped VXLAN egress (10.99.0.0/24 udp 4789):\n%s", out)
	}
	// it must NOT be an unscoped udp 4789 accept (that would allow exfil to any host).
	if strings.Contains(out, "\n    udp dport 4789 accept\n") {
		t.Errorf("VXLAN egress must be scoped to the overlay, found unscoped rule:\n%s", out)
	}
	// disabled by default (no L2 => no vxlan/4789 rules).
	off := NFTables(Spec{WGPort: 51820, AllowOverlayInput: true})
	if strings.Contains(off, "vxlan0") || strings.Contains(off, "4789") {
		t.Errorf("L2 rules must not appear when AllowL2Input is false:\n%s", off)
	}
}

// The exfiltration-protection property from S2: with no allowlist and DNS off,
// the only permitted outbound is loopback + established — no new connections.
func TestNFTables_NoArbitraryEgress(t *testing.T) {
	out := NFTables(Spec{})
	oi := strings.Index(out, "chain output")
	egress := out[oi:]
	if strings.Contains(egress, "@egress_ok") || strings.Contains(egress, "dport 53") {
		t.Fatalf("locked-down egress must not allow allowlist/DNS when unset:\n%s", egress)
	}
	if !strings.Contains(egress, "policy drop;") {
		t.Fatalf("egress must default-drop:\n%s", egress)
	}
}

func TestNFTables_NoPublicSSH_OverlayOnly(t *testing.T) {
	out := NFTables(Spec{NoPublicSSH: true, WGPort: 51820, AllowOverlayInput: true, AllowDNS: true})
	// no public SSH rule at all (neither open nor operator-restricted)
	if strings.Contains(out, "tcp dport 22 accept") {
		t.Errorf("NoPublicSSH must emit NO public SSH rule:\n%s", out)
	}
	// but the overlay is still trusted (so SSH-over-overlay works) and WG is open
	if !strings.Contains(out, "iif \"wg0\" accept") || !strings.Contains(out, "udp dport 51820 accept") {
		t.Errorf("overlay + WG must remain reachable:\n%s", out)
	}
}
