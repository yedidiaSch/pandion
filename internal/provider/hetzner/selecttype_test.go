// SPDX-License-Identifier: AGPL-3.0-or-later

package hetzner

import (
	"reflect"
	"strings"
	"testing"
)

func TestSelectCandidates_BySpec_SmallestFirst_SkipsDeprecatedAndArch(t *testing.T) {
	types := []typeInfo{
		{Name: "cpx11", Cores: 2, MemGB: 2, Arch: "x86", Deprecated: true}, // retired -> skip
		{Name: "cpx12", Cores: 2, MemGB: 4, Arch: "x86"},                   // ok
		{Name: "cpx21", Cores: 3, MemGB: 4, Arch: "x86"},                   // ok (bigger)
		{Name: "cx23", Cores: 2, MemGB: 4, Arch: "x86"},                    // ok, tie on cores+mem -> name order
		{Name: "cax11", Cores: 2, MemGB: 4, Arch: "arm"},                   // wrong arch -> skip
		{Name: "tiny", Cores: 1, MemGB: 1, Arch: "x86"},                    // too small -> skip
	}

	got := selectCandidates(types, 2, 2, "x86")
	want := []string{"cpx12", "cx23", "cpx21"} // 2c/4g (name order cpx12<cx23), then 3c/4g
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectCandidates = %v, want %v", got, want)
	}
}

func TestSelectCandidates_RespectsMinima(t *testing.T) {
	types := []typeInfo{
		{Name: "small", Cores: 2, MemGB: 4, Arch: "x86"},
		{Name: "big", Cores: 8, MemGB: 16, Arch: "x86"},
	}
	got := selectCandidates(types, 4, 8, "x86")
	if !reflect.DeepEqual(got, []string{"big"}) {
		t.Fatalf("got %v, want [big]", got)
	}
}

func TestSearchPlan_RegionFirstVsCheapestType(t *testing.T) {
	types := []string{"small", "big"} // smallest-first
	locs := []string{"fsn1", "ash"}   // preference-first

	// RegionFirst: exhaust fsn1 (small THEN big) before touching ash —
	// stays in-region even if it means a bigger type.
	region := searchPlan(types, locs, RegionFirst)
	wantRegion := [][2]string{{"small", "fsn1"}, {"big", "fsn1"}, {"small", "ash"}, {"big", "ash"}}
	if !reflect.DeepEqual(region, wantRegion) {
		t.Fatalf("RegionFirst = %v, want %v", region, wantRegion)
	}

	// CheapestType: try the small type in every region before the big one.
	cheap := searchPlan(types, locs, CheapestType)
	wantCheap := [][2]string{{"small", "fsn1"}, {"small", "ash"}, {"big", "fsn1"}, {"big", "ash"}}
	if !reflect.DeepEqual(cheap, wantCheap) {
		t.Fatalf("CheapestType = %v, want %v", cheap, wantCheap)
	}
}

func TestServerName_NamespacedAndDNSSafe(t *testing.T) {
	cases := map[string][2]string{
		"pandion-e2e-node-a":     {"e2e", "node-a"},
		"pandion-team1-broker":   {"team1", "broker"},
		"pandion-a-b-c-worker-1": {"a.b.c", "worker_1"}, // dots/underscores -> hyphens
	}
	for want, in := range cases {
		if got := serverName(in[0], in[1]); got != want {
			t.Errorf("serverName(%q,%q)=%q, want %q", in[0], in[1], got, want)
		}
	}
	// different clusters, same node name -> different server names (no collision)
	if serverName("c1", "node-a") == serverName("c2", "node-a") {
		t.Fatal("same node name in different clusters must not collide")
	}
	// bounded length, no leading/trailing hyphen
	long := serverName("verylongclusteridentifier0123456789", "verylongnodename0123456789abcdef")
	if len(long) > 63 {
		t.Fatalf("server name too long: %d", len(long))
	}
	if strings.HasPrefix(long, "-") || strings.HasSuffix(long, "-") {
		t.Fatalf("server name has stray hyphen: %q", long)
	}
}

func TestOrderLocations_PrefersThenRest(t *testing.T) {
	all := []string{"sin", "ash", "fsn1", "nbg1", "hil", "hel1"}
	got := orderLocations(all, []string{"fsn1", "nbg1", "hel1"})
	want := []string{"fsn1", "nbg1", "hel1", "sin", "ash", "hil"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderLocations = %v, want %v", got, want)
	}
}
