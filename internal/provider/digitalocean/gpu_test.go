// SPDX-License-Identifier: AGPL-3.0-or-later

package digitalocean

import (
	"testing"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// realistic GPU catalog (from the live DO API): most sold out (no regions),
// only h100x1 has capacity; plus a CPU size that must be ignored.
func gpuFixture() []sizeInfo {
	return []sizeInfo{
		{Slug: "s-2vcpu-4gb", Vcpus: 2, MemMB: 4096, PriceHourly: 0.03, Regions: []string{"nyc1"}, Available: true},
		{Slug: "gpu-h100x1-80gb", GPU: true, GPUModel: "nvidia_h100", GPUCount: 1, GPUVRAM: 80, PriceHourly: 3.39, Regions: []string{"tor1"}, Available: true},
		{Slug: "gpu-h100x8-640gb", GPU: true, GPUModel: "nvidia_h100", GPUCount: 8, GPUVRAM: 80, PriceHourly: 23.92, Regions: []string{}, Available: true},
		{Slug: "gpu-l40sx1-48gb", GPU: true, GPUModel: "nvidia_l40s", GPUCount: 1, GPUVRAM: 48, PriceHourly: 1.57, Regions: []string{}, Available: true},
		{Slug: "gpu-mi300x1-192gb", GPU: true, GPUModel: "amd_mi300x", GPUCount: 1, GPUVRAM: 192, PriceHourly: 1.99, Regions: []string{}, Available: true},
	}
}

func TestNormalizeGPUModel(t *testing.T) {
	cases := map[string]string{"nvidia_h100": "h100", "amd_mi300x": "mi300x", "nvidia_rtx6000_ada": "rtx6000_ada", "h100": "h100"}
	for in, want := range cases {
		if got := normalizeGPUModel(in); got != want {
			t.Errorf("normalizeGPUModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelMatches(t *testing.T) {
	if !modelMatches("h100", "nvidia_h100") {
		t.Error("h100 should match nvidia_h100")
	}
	if !modelMatches("rtx6000", "nvidia_rtx6000_ada") {
		t.Error("bare family rtx6000 should match rtx6000_ada")
	}
	if modelMatches("h100", "nvidia_h200") {
		t.Error("h100 must not match h200")
	}
}

func TestGPUImageFor(t *testing.T) {
	cases := []struct {
		s    sizeInfo
		want string
	}{
		{sizeInfo{GPUModel: "nvidia_h100", GPUCount: 1}, "gpu-h100x1-base"},
		{sizeInfo{GPUModel: "nvidia_h100", GPUCount: 8}, "gpu-h100x8-base"},
		{sizeInfo{GPUModel: "amd_mi300x", GPUCount: 1}, "gpu-amd-base"},
		{sizeInfo{GPUModel: "nvidia_l40s", GPUCount: 1}, "gpu-h100x1-base"},
	}
	for _, c := range cases {
		if got := gpuImageFor(c.s); got != c.want {
			t.Errorf("gpuImageFor(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestVramGB(t *testing.T) {
	if vramGB(80, "gib") != 80 || vramGB(80, "GB") != 80 || vramGB(81920, "mib") != 80 {
		t.Error("vramGB conversion wrong")
	}
}

func TestSelectGPUSizes_CheapestFirstAndFilters(t *testing.T) {
	sizes := gpuFixture()

	// any GPU, cheapest-first → l40s (1.57) before mi300x (1.99) before h100 (3.39)
	got := selectGPUSizes(sizes, provider.GPUReq{Count: 1}, "")
	if len(got) == 0 || got[0].Slug != "gpu-l40sx1-48gb" {
		t.Fatalf("cheapest-first wrong: %+v", got)
	}

	// model filter (normalized): --gpu h100 matches nvidia_h100 only
	got = selectGPUSizes(sizes, provider.GPUReq{Model: "h100", Count: 1}, "")
	if len(got) != 1 || got[0].Slug != "gpu-h100x1-80gb" {
		t.Fatalf("h100:1 filter wrong: %+v", got)
	}

	// count filter: h100:8 → the x8 size
	got = selectGPUSizes(sizes, provider.GPUReq{Model: "h100", Count: 8}, "")
	if len(got) != 1 || got[0].Slug != "gpu-h100x8-640gb" {
		t.Fatalf("h100:8 filter wrong: %+v", got)
	}

	// region filter: only h100x1 has tor1 capacity
	got = selectGPUSizes(sizes, provider.GPUReq{Count: 1}, "tor1")
	if len(got) != 1 || got[0].Slug != "gpu-h100x1-80gb" {
		t.Fatalf("region filter wrong: %+v", got)
	}

	// minVRAM
	got = selectGPUSizes(sizes, provider.GPUReq{MinVRAM: 100, Count: 1}, "")
	if len(got) != 1 || got[0].Slug != "gpu-mi300x1-192gb" {
		t.Fatalf("minVRAM filter wrong: %+v", got)
	}
}

func TestGPUOfferingSatisfies(t *testing.T) {
	o := provider.GPUOffering{GPU: provider.GPUInfo{Model: "h100", Count: 1, VRAM: 80}}
	if !gpuOfferingSatisfies(o, provider.GPUReq{Model: "h100", Count: 1}) {
		t.Error("should satisfy exact match")
	}
	if gpuOfferingSatisfies(o, provider.GPUReq{Count: 8}) {
		t.Error("count mismatch must not satisfy")
	}
	if gpuOfferingSatisfies(o, provider.GPUReq{MinVRAM: 141}) {
		t.Error("minVRAM over the card must not satisfy")
	}
}

func TestPickRegion(t *testing.T) {
	if pickRegion([]string{"tor1", "nyc1"}, []string{"nyc1"}) != "nyc1" {
		t.Error("preferred region should win")
	}
	if pickRegion([]string{"tor1"}, []string{"nyc1"}) != "tor1" {
		t.Error("fallback to first available")
	}
	if pickRegion(nil, nil) != "" {
		t.Error("no regions → empty")
	}
}
