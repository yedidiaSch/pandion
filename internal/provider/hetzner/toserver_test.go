package hetzner

import (
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// toServer must read the region from the top-level Location — the nested
// Datacenter was phased out of API responses after 2026-07-01, which is what
// left `ls` showing "—".
func TestToServer_RegionFromLocation(t *testing.T) {
	s := &hcloud.Server{
		ID:         42,
		Name:       "pandion-x-worker",
		ServerType: &hcloud.ServerType{Name: "cpx22"},
		Location:   &hcloud.Location{Name: "fsn1"},
	}
	got := toServer(s, "x")
	if got.Region != "fsn1" {
		t.Fatalf("region = %q, want fsn1 (from Location)", got.Region)
	}
	if got.Type != "cpx22" || got.ID != "42" {
		t.Fatalf("unexpected mapping: %+v", got)
	}
}

// Fallback: an older response with only the deprecated Datacenter still yields a
// region (so we don't regress older API behavior).
func TestToServer_RegionFallbackDatacenter(t *testing.T) {
	s := &hcloud.Server{
		ID:         7,
		Datacenter: &hcloud.Datacenter{Location: &hcloud.Location{Name: "nbg1"}},
	}
	if got := toServer(s, "x"); got.Region != "nbg1" {
		t.Fatalf("region = %q, want nbg1 (from deprecated Datacenter)", got.Region)
	}
}
