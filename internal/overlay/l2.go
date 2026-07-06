package overlay

import (
	"fmt"
	"strings"
)

// Layer-2 overlay: a VXLAN segment stacked on the WireGuard L3 mesh, giving nodes
// a real encrypted Ethernet broadcast domain. WireGuard carries no multicast, so
// the orchestrator manages a static Forwarding Database (FDB): unicast MAC→VTEP
// entries plus per-peer BUM-flood entries so broadcast/ARP still propagate. The
// "safe" profile additionally enforces the IP↔MAC binding with an nftables ARP
// filter (host-side Dynamic ARP Inspection), making the segment spoof-resistant.
//
// These renderers are pure strings/commands — unit-tested offline, no `ip`/`nft`.

const (
	// L2Dev is the VXLAN interface name; L2DstPort the VXLAN UDP port (IANA 4789).
	L2Dev     = "vxlan0"
	L2DstPort = 4789
	// vxlanOverhead is the bytes VXLAN adds on top of the underlay (outer IPv4 +
	// UDP + VXLAN header). Subtracted from the wg0 MTU for the inner vxlan0 MTU.
	vxlanOverhead = 50
	// l2MTUFloor is the smallest inner MTU we will set (IPv6 minimum), a guard
	// against a pathologically small underlay MTU.
	l2MTUFloor = 1280
	// DefaultWGMTU is wg-quick's default WireGuard MTU, the underlay for vxlan0.
	DefaultWGMTU = 1420
)

// L2NodeSpec describes one node's VXLAN interface (peer-independent; the FDB is
// injected separately at the barrier once every node's MAC + wg IP is known).
type L2NodeSpec struct {
	VNI     int    // VXLAN network identifier
	LocalWG string // this node's wg0 IP (the VXLAN "local" underlay address)
	Addr    string // this node's L2 address, e.g. 192.168.66.3/24
	MAC     string // deterministic locally-administered MAC, e.g. 02:00:00:00:00:03
	MTU     int    // inner MTU (see MTUFor)
	Profile string // "safe" | "lab"
}

// L2Binding is one node's IP↔MAC↔VTEP identity, used to build the FDB and DAI.
type L2Binding struct {
	L2IP string // e.g. 192.168.66.2 (no prefix)
	MAC  string
	WGIP string // the peer's wg0 IP (VXLAN destination / VTEP)
}

// MTUFor computes the vxlan0 inner MTU from the wg0 underlay MTU, subtracting the
// VXLAN overhead and clamping to the floor — so large frames don't silently
// black-hole (the classic stacked-tunnel failure).
func MTUFor(wgMTU int) int {
	if wgMTU <= 0 {
		wgMTU = DefaultWGMTU
	}
	m := wgMTU - vxlanOverhead
	if m < l2MTUFloor {
		return l2MTUFloor
	}
	return m
}

// VXLANBringUp renders the ordered shell commands to create and configure vxlan0
// on one node — run at boot AFTER wg-quick@wg0 (the interface rides wg0). It is
// peer-independent: the FDB is injected later, at the barrier. The "safe" profile
// keeps the hardened posture; "lab" relaxes reverse-path filtering and enables
// promiscuous mode ON vxlan0 ONLY (never wg0/eth0).
func VXLANBringUp(s L2NodeSpec) []string {
	cmds := []string{
		fmt.Sprintf("ip link del %s 2>/dev/null || true", L2Dev), // idempotent
		fmt.Sprintf("ip link add %s type vxlan id %d dstport %d dev wg0 local %s nolearning",
			L2Dev, s.VNI, L2DstPort, s.LocalWG),
		fmt.Sprintf("ip link set %s address %s", L2Dev, s.MAC),
		fmt.Sprintf("ip link set %s mtu %d", L2Dev, s.MTU),
		fmt.Sprintf("ip addr add %s dev %s", s.Addr, L2Dev),
		fmt.Sprintf("ip link set %s up", L2Dev),
	}
	if s.Profile == "lab" {
		// lab range: let a MITM forward spoofed-source frames and allow sniffing —
		// scoped to vxlan0. (No DAI: ARP is unprotected by design.)
		cmds = append(cmds,
			fmt.Sprintf("sysctl -w net.ipv4.conf.%s.rp_filter=0", L2Dev),
			fmt.Sprintf("ip link set %s promisc on", L2Dev),
		)
	}
	return cmds
}

// FDBInject renders the FDB entries pointing this node at one peer VTEP: a unicast
// MAC→VTEP entry and a BUM-flood entry (all-zeros MAC) so broadcast/ARP/multicast
// reach that peer (head-end replication — required because WireGuard has no
// multicast). Injected over SSH at the barrier, like SetPeerCommand.
func FDBInject(peerMAC, peerWGIP string) []string {
	return []string{
		fmt.Sprintf("bridge fdb append %s dev %s dst %s", peerMAC, L2Dev, peerWGIP),
		fmt.Sprintf("bridge fdb append 00:00:00:00:00:00 dev %s dst %s", L2Dev, peerWGIP),
	}
}

// DAIRules renders an nftables ruleset (arp family) that enforces the IP↔MAC
// binding on vxlan0 — a host-side Dynamic ARP Inspection. Only ARP whose sender
// IP+MAC match a known binding is accepted; every other ARP on vxlan0 is dropped,
// so a forged ARP reply cannot poison a neighbor's cache. Used by the "safe"
// profile only. Applied with `nft -f` alongside the existing firewall table.
func DAIRules(bindings []L2Binding) string {
	var b strings.Builder
	b.WriteString("table arp pandion_dai {\n")
	b.WriteString("  chain input {\n")
	b.WriteString("    type filter hook input priority 0; policy accept;\n")
	// only police ARP arriving on the L2 segment.
	for _, bd := range bindings {
		fmt.Fprintf(&b, "    meta iifname \"%s\" arp saddr ip %s arp saddr ether %s accept\n",
			L2Dev, bd.L2IP, bd.MAC)
	}
	// any other ARP on vxlan0 (a spoof — sender IP/MAC not a known binding) is
	// dropped; ARP on every other interface is untouched (policy accept).
	fmt.Fprintf(&b, "    meta iifname \"%s\" arp operation reply drop\n", L2Dev)
	fmt.Fprintf(&b, "    meta iifname \"%s\" arp operation request drop\n", L2Dev)
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}
