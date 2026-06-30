package hetzner

import (
	"reflect"
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

func TestOrderLocations_PrefersThenRest(t *testing.T) {
	all := []string{"sin", "ash", "fsn1", "nbg1", "hil", "hel1"}
	got := orderLocations(all, []string{"fsn1", "nbg1", "hel1"})
	want := []string{"fsn1", "nbg1", "hel1", "sin", "ash", "hil"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderLocations = %v, want %v", got, want)
	}
}
