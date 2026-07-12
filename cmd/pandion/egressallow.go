// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

// resolveEgressAllow expands an egress allowlist that may contain hostnames into
// concrete IPv4 addresses/CIDRs for the nftables egress set (which is ipv4_addr).
// IPv4 addresses and IPv4 CIDRs pass through unchanged; a hostname is resolved to
// its IPv4 address(es) at provision time. Unresolvable names are dropped with a
// warning (fail-open on the one name, not the whole firewall apply). IPv6 is
// intentionally ignored — nodes disable it and the egress set is IPv4-only.
// Results are de-duplicated and sorted.
//
// NOTE: this resolves ONCE, at provision time. If a name's IPs later rotate (as
// CDNs do), the rule goes stale — periodic on-node re-resolution is the follow-up
// (see docs/security-next-steps.md P2).
func resolveEgressAllow(entries []string) []string {
	return resolveEgressAllowWith(entries, net.LookupIP)
}

// resolveEgressAllowWith is resolveEgressAllow with an injectable lookup, for tests.
func resolveEgressAllowWith(entries []string, lookup func(string) ([]net.IP, error)) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		// IPv4 CIDR (a.b.c.d/n) — pass through; reject IPv6 CIDRs (set is ipv4).
		if ip, _, err := net.ParseCIDR(e); err == nil {
			if ip.To4() != nil {
				add(e)
			} else {
				fmt.Fprintf(os.Stderr, "egress-allow: ignoring IPv6 CIDR %q (nodes are IPv4-only)\n", e)
			}
			continue
		}
		// bare IP
		if ip := net.ParseIP(e); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				add(ip4.String())
			} else {
				fmt.Fprintf(os.Stderr, "egress-allow: ignoring IPv6 address %q (nodes are IPv4-only)\n", e)
			}
			continue
		}
		// hostname → resolve to IPv4(s) at provision time
		ips, err := lookup(e)
		if err != nil {
			fmt.Fprintf(os.Stderr, "egress-allow: could not resolve %q: %v (skipped)\n", e, err)
			continue
		}
		n := 0
		for _, ip := range ips {
			if ip4 := ip.To4(); ip4 != nil {
				add(ip4.String())
				n++
			}
		}
		if n == 0 {
			fmt.Fprintf(os.Stderr, "egress-allow: %q resolved to no IPv4 address (skipped)\n", e)
		}
	}
	sort.Strings(out)
	return out
}
