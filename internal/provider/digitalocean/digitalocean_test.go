// SPDX-License-Identifier: AGPL-3.0-or-later

package digitalocean

import (
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"github.com/yedidiaSch/pandion/internal/provider"
)

// compile-time: DO satisfies the core seam plus the optional capabilities.
var (
	_ provider.Provider  = (*DO)(nil)
	_ provider.Pricer    = (*DO)(nil)
	_ provider.AuxReaper = (*DO)(nil)
)

func TestSelectSizes_CheapestFirst_FiltersSpecRegionGPU(t *testing.T) {
	sizes := []sizeInfo{
		{Slug: "s-1vcpu-1gb", Vcpus: 1, MemMB: 1024, PriceHourly: 0.007, Regions: []string{"nyc3", "fra1"}, Available: true},
		{Slug: "s-2vcpu-2gb", Vcpus: 2, MemMB: 2048, PriceHourly: 0.018, Regions: []string{"nyc3", "fra1"}, Available: true},
		{Slug: "s-2vcpu-4gb", Vcpus: 2, MemMB: 4096, PriceHourly: 0.027, Regions: []string{"nyc3"}, Available: true},
		{Slug: "s-8vcpu-16gb-gpu", Vcpus: 8, MemMB: 16384, PriceHourly: 1.50, Regions: []string{"nyc3"}, Available: true, GPU: true},
		{Slug: "s-2vcpu-2gb-sold", Vcpus: 2, MemMB: 2048, PriceHourly: 0.001, Regions: []string{"nyc3"}, Available: false},
	}

	// spec: >=2 vcpu / >=2GB, any region. Excludes the 1-vcpu, the GPU, the
	// unavailable; cheapest-first ⇒ s-2vcpu-2gb before s-2vcpu-4gb.
	got := selectSizes(sizes, 2, 2048, "")
	if len(got) != 2 || got[0].Slug != "s-2vcpu-2gb" || got[1].Slug != "s-2vcpu-4gb" {
		t.Fatalf("unexpected selection: %+v", slugs(got))
	}

	// region filter: fra1 only has the 2gb (4gb is nyc3-only).
	got = selectSizes(sizes, 2, 2048, "fra1")
	if len(got) != 1 || got[0].Slug != "s-2vcpu-2gb" {
		t.Fatalf("region filter wrong: %+v", slugs(got))
	}
}

func TestOrderRegions_PreferredFirst(t *testing.T) {
	got := orderRegions([]string{"nyc3", "fra1", "sfo3"}, []string{"fra1", "lon1"})
	want := []string{"fra1", "nyc3", "sfo3"} // fra1 first (preferred+present), lon1 ignored (absent)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("orderRegions = %v, want %v", got, want)
	}
}

// The cluster id round-trips through the per-cluster tag (sanitized) so
// ListAllTagged can recover it.
func TestTagRoundTrip(t *testing.T) {
	for _, id := range []string{"pipeline", "e2e-lscost", "Build-01"} {
		tag := clusterTag(id)
		if got := recoverClusterID([]string{tagAll, tag}); got != sanitize(id) {
			t.Fatalf("round-trip %q: tag %q -> %q, want %q", id, tag, got, sanitize(id))
		}
	}
	if got := recoverClusterID([]string{"pandion"}); got != "" {
		t.Fatalf("no cluster tag should recover to empty, got %q", got)
	}
}

func TestDropletName_SanitizedAndBounded(t *testing.T) {
	if n := dropletName("pipeline", "broker"); n != "pandion-pipeline-broker" {
		t.Fatalf("name = %q", n)
	}
	long := dropletName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if len(long) > 63 {
		t.Fatalf("name not bounded to 63: %d", len(long))
	}
}

func TestToServer_ParsesFields(t *testing.T) {
	created := time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	dr := &godo.Droplet{
		ID:       12345,
		Name:     "pandion-pipeline-broker",
		SizeSlug: "s-2vcpu-2gb",
		Region:   &godo.Region{Slug: "fra1"},
		Created:  created,
		Networks: &godo.Networks{V4: []godo.NetworkV4{
			{IPAddress: "10.0.0.9", Type: "private"},
			{IPAddress: "203.0.113.7", Type: "public"},
		}},
	}
	s := toServer(dr, "pipeline")
	if s.ID != "12345" || s.Type != "s-2vcpu-2gb" || s.Region != "fra1" || s.IP != "203.0.113.7" {
		t.Fatalf("toServer mapped wrong: %+v", s)
	}
	if s.Created.IsZero() || time.Since(s.Created) < time.Hour {
		t.Fatalf("created not parsed: %v", s.Created)
	}
}

func slugs(ss []sizeInfo) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Slug
	}
	return out
}
