// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"net"
	"reflect"
	"testing"
)

func TestResolveEgressAllow(t *testing.T) {
	// deterministic stub resolver
	table := map[string][]net.IP{
		"github.com":     {net.ParseIP("140.82.112.3"), net.ParseIP("140.82.112.4")},
		"dual.example":   {net.ParseIP("2606:4700::1"), net.ParseIP("1.2.3.4")}, // v6 dropped, v4 kept
		"v6only.example": {net.ParseIP("2606:4700::1")},
	}
	lookup := func(h string) ([]net.IP, error) {
		if ips, ok := table[h]; ok {
			return ips, nil
		}
		return nil, fmt.Errorf("no such host")
	}

	got := resolveEgressAllowWith([]string{
		"10.0.0.5",       // bare IPv4 → kept
		"192.168.1.0/24", // IPv4 CIDR → kept
		"github.com",     // hostname → two IPv4s
		"dual.example",   // mixed → only the IPv4
		"v6only.example", // no IPv4 → dropped (warned)
		"nope.invalid",   // unresolvable → dropped (warned)
		"  10.0.0.5  ",   // dup after trim
		"::1",            // IPv6 literal → dropped
		"2001:db8::/32",  // IPv6 CIDR → dropped
		"",               // empty → skipped
	}, lookup)

	// sorted + de-duplicated, IPv4-only
	want := []string{
		"1.2.3.4",
		"10.0.0.5",
		"140.82.112.3",
		"140.82.112.4",
		"192.168.1.0/24",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveEgressAllow mismatch\n got: %v\nwant: %v", got, want)
	}
}

// A pure IP/CIDR list must be returned as-is (sorted), never touching the resolver.
func TestResolveEgressAllow_NoLookupForLiterals(t *testing.T) {
	called := false
	lookup := func(string) ([]net.IP, error) { called = true; return nil, nil }
	got := resolveEgressAllowWith([]string{"203.0.113.9", "198.51.100.0/24"}, lookup)
	if called {
		t.Error("resolver must not be called for literal IPs/CIDRs")
	}
	want := []string{"198.51.100.0/24", "203.0.113.9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
