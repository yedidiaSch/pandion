package hetzner

import (
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/yedidiaSch/pandion/internal/provider"
)

// compile-time: Hetzner offers the optional cloud-firewall capability.
var _ provider.ClusterFirewaller = (*Hetzner)(nil)

func TestClusterFirewallRules_OnlySshWgIcmpInbound(t *testing.T) {
	rules := clusterFirewallRules(51820)
	if len(rules) != 3 {
		t.Fatalf("want 3 inbound rules, got %d", len(rules))
	}
	// all inbound (egress is left to the host)
	for _, r := range rules {
		if r.Direction != hcloud.FirewallRuleDirectionIn {
			t.Fatalf("rule not inbound: %+v", r)
		}
		if len(r.SourceIPs) != 2 { // v4 + v6 any
			t.Fatalf("expected v4+v6 source, got %d", len(r.SourceIPs))
		}
	}
	// SSH on 22, WG on the given port, plus ICMP
	var haveSSH, haveWG, haveICMP bool
	for _, r := range rules {
		switch r.Protocol {
		case hcloud.FirewallRuleProtocolTCP:
			haveSSH = r.Port != nil && *r.Port == "22"
		case hcloud.FirewallRuleProtocolUDP:
			haveWG = r.Port != nil && *r.Port == "51820"
		case hcloud.FirewallRuleProtocolICMP:
			haveICMP = true
		}
	}
	if !haveSSH || !haveWG || !haveICMP {
		t.Fatalf("missing rule: ssh=%v wg=%v icmp=%v", haveSSH, haveWG, haveICMP)
	}
}
