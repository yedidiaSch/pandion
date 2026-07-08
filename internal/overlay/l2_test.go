// SPDX-License-Identifier: AGPL-3.0-or-later

package overlay

import (
	"strings"
	"testing"
)

func TestMTUFor_SubtractsOverheadWithFloor(t *testing.T) {
	if got := MTUFor(1420); got != 1370 {
		t.Fatalf("MTUFor(1420) = %d, want 1370", got)
	}
	if got := MTUFor(0); got != 1370 { // default wg MTU
		t.Fatalf("MTUFor(0) = %d, want 1370 (default)", got)
	}
	if got := MTUFor(1300); got != l2MTUFloor { // 1300-50=1250 < floor
		t.Fatalf("MTUFor(1300) = %d, want floor %d", got, l2MTUFloor)
	}
}

func TestVXLANBringUp_OrderFlagsAndSafe(t *testing.T) {
	cmds := VXLANBringUp(L2NodeSpec{
		VNI: 100, LocalWG: "10.99.0.3", Addr: "192.168.66.3/24",
		MAC: "02:00:00:00:00:03", MTU: 1370, Profile: "safe",
	})
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{
		"ip link add vxlan0 type vxlan id 100 dstport 4789 dev wg0 local 10.99.0.3 nolearning",
		"ip link set vxlan0 address 02:00:00:00:00:03",
		"ip link set vxlan0 mtu 1370",
		"ip addr add 192.168.66.3/24 dev vxlan0",
		"ip link set vxlan0 up",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("bring-up missing %q:\n%s", want, joined)
		}
	}
	// the create must come before addressing/up (ordering matters).
	if idxAdd, idxUp := indexOf(cmds, "type vxlan"), indexOf(cmds, "vxlan0 up"); idxAdd == -1 || idxUp < idxAdd {
		t.Fatalf("interface must be created before it is brought up: %v", cmds)
	}
	// safe profile must NOT relax anything.
	if strings.Contains(joined, "rp_filter=0") || strings.Contains(joined, "promisc on") {
		t.Fatalf("safe profile must not relax rp_filter/promisc:\n%s", joined)
	}
}

func TestVXLANBringUp_LabRelaxationsScopedToVxlan(t *testing.T) {
	cmds := VXLANBringUp(L2NodeSpec{VNI: 100, LocalWG: "10.99.0.1", Addr: "192.168.66.1/24",
		MAC: "02:00:00:00:00:01", MTU: 1370, Profile: "lab"})
	joined := strings.Join(cmds, "\n")
	if !strings.Contains(joined, "net.ipv4.conf.vxlan0.rp_filter=0") {
		t.Error("lab must disable rp_filter on vxlan0")
	}
	if !strings.Contains(joined, "ip link set vxlan0 promisc on") {
		t.Error("lab must enable promiscuous mode on vxlan0")
	}
	// the crucial safety invariant: no relaxation may touch wg0 or eth0.
	for _, forbidden := range []string{"wg0.rp_filter", "eth0.rp_filter", "wg0 promisc", "eth0 promisc"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("lab relaxation leaked to the management/public plane: %q", forbidden)
		}
	}
}

func TestFDBInject_UnicastAndBUMFlood(t *testing.T) {
	got := FDBInject("02:00:00:00:00:02", "10.99.0.2")
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "bridge fdb append 02:00:00:00:00:02 dev vxlan0 dst 10.99.0.2") {
		t.Error("missing unicast MAC->VTEP entry")
	}
	if !strings.Contains(joined, "bridge fdb append 00:00:00:00:00:00 dev vxlan0 dst 10.99.0.2") {
		t.Error("missing BUM-flood entry (ARP/broadcast won't propagate)")
	}
}

func TestDAIRules_BindingEnforcedThenDrop(t *testing.T) {
	rs := DAIRules([]L2Binding{
		{L2IP: "192.168.66.1", MAC: "02:00:00:00:00:01", WGIP: "10.99.0.1"},
		{L2IP: "192.168.66.2", MAC: "02:00:00:00:00:02", WGIP: "10.99.0.2"},
	})
	// each known binding is accepted on vxlan0…
	if !strings.Contains(rs, `arp saddr ip 192.168.66.1 arp saddr ether 02:00:00:00:00:01 accept`) ||
		!strings.Contains(rs, `arp saddr ip 192.168.66.2 arp saddr ether 02:00:00:00:00:02 accept`) {
		t.Fatalf("bindings not accepted:\n%s", rs)
	}
	// …and any other ARP on vxlan0 (a spoof) is dropped.
	if !strings.Contains(rs, `meta iifname "vxlan0" arp operation reply drop`) ||
		!strings.Contains(rs, `meta iifname "vxlan0" arp operation request drop`) {
		t.Fatalf("catch-all ARP drop missing:\n%s", rs)
	}
	// the accept must be scoped to vxlan0 (never global).
	if strings.Contains(rs, "accept\n    arp") || !strings.Contains(rs, `meta iifname "vxlan0" arp saddr`) {
		t.Fatalf("DAI accept must be scoped to vxlan0:\n%s", rs)
	}
}

func indexOf(ss []string, sub string) int {
	for i, s := range ss {
		if strings.Contains(s, sub) {
			return i
		}
	}
	return -1
}
