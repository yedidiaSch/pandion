package firewall

import (
	"strings"
	"testing"
)

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
