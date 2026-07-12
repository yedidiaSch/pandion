// SPDX-License-Identifier: AGPL-3.0-or-later

package digitalocean

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// DigitalOcean GPU Droplets are Tier-A (docs/gpu-design.md §5): full droplets
// whose base image ("NVIDIA AI/ML Ready", slug gpu-h100x1-base / gpu-h100x8-base
// / gpu-amd-base) ships NVIDIA drivers + CUDA, so no driver injection is needed
// and the overlay + lockdown work unchanged. The DO API reports GPU model, count,
// and VRAM directly in each size's gpu_info — no string parsing.

// GPUOfferings implements provider.GPUProvider: the priced GPU-droplet catalog.
func (d *DO) GPUOfferings(ctx context.Context) ([]provider.GPUOffering, error) {
	sizes, err := d.sizes(ctx)
	if err != nil {
		return nil, err
	}
	var out []provider.GPUOffering
	for _, s := range sizes {
		if !s.GPU || !s.Available {
			continue
		}
		out = append(out, provider.GPUOffering{
			ServerType: s.Slug,
			GPU:        provider.GPUInfo{Model: normalizeGPUModel(s.GPUModel), Count: s.GPUCount, VRAM: s.GPUVRAM},
			Regions:    s.Regions,
			Hourly:     provider.Money{Amount: s.PriceHourly, Currency: "USD"},
			Image:      gpuImageFor(s),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hourly.Amount != out[j].Hourly.Amount {
			return out[i].Hourly.Amount < out[j].Hourly.Amount
		}
		return out[i].ServerType < out[j].ServerType
	})
	return out, nil
}

// ResolveGPUType implements provider.GPUProvider: the cheapest GPU size that
// satisfies req AND has capacity in an acceptable region. Deterministic.
func (d *DO) ResolveGPUType(ctx context.Context, req provider.GPUReq, regionPref []string) (string, string, error) {
	offs, err := d.GPUOfferings(ctx)
	if err != nil {
		return "", "", err
	}
	pref := d.regionPref
	if len(regionPref) > 0 {
		pref = regionPref
	}
	for _, o := range offs {
		if !gpuOfferingSatisfies(o, req) {
			continue
		}
		if r := pickRegion(o.Regions, pref); r != "" {
			return o.ServerType, r, nil
		}
	}
	return "", "", fmt.Errorf("digitalocean: no GPU size with capacity matches model=%q count=%d minVRAM=%d (try `pandion list-gpus --provider digitalocean`)",
		req.Model, req.Count, req.MinVRAM)
}

// gpuOfferingSatisfies reports whether an offering meets a GPU request.
func gpuOfferingSatisfies(o provider.GPUOffering, req provider.GPUReq) bool {
	if req.Model != "" && !modelMatches(req.Model, o.GPU.Model) {
		return false
	}
	if req.MinVRAM > 0 && o.GPU.VRAM < req.MinVRAM {
		return false
	}
	if req.Count > 0 && o.GPU.Count != req.Count {
		return false
	}
	return true
}

// selectGPUSizes returns GPU sizes matching a GPUReq, cheapest-first (for
// CreateServer when no exact --size was given).
func selectGPUSizes(sizes []sizeInfo, req provider.GPUReq, region string) []sizeInfo {
	var ok []sizeInfo
	for _, s := range sizes {
		if !s.GPU || !s.Available {
			continue
		}
		if req.Model != "" && !modelMatches(req.Model, normalizeGPUModel(s.GPUModel)) {
			continue
		}
		if req.Count > 0 && s.GPUCount != req.Count {
			continue
		}
		if req.MinVRAM > 0 && s.GPUVRAM < req.MinVRAM {
			continue
		}
		if region != "" && !contains(s.Regions, region) {
			continue
		}
		ok = append(ok, s)
	}
	sort.Slice(ok, func(i, j int) bool {
		if ok[i].PriceHourly != ok[j].PriceHourly {
			return ok[i].PriceHourly < ok[j].PriceHourly
		}
		return ok[i].Slug < ok[j].Slug
	})
	return ok
}

// gpuImageFor returns the CUDA/ROCm-ready base image slug for a GPU size. DO
// ships per-family "AI/ML Ready" base images; NVIDIA multi-GPU NVLink configs use
// the x8 base, AMD uses the AMD base, everything else the single-GPU NVIDIA base.
func gpuImageFor(s sizeInfo) string {
	m := strings.ToLower(s.GPUModel)
	switch {
	case strings.HasPrefix(m, "amd"):
		return "gpu-amd-base"
	case s.GPUCount >= 8:
		return "gpu-h100x8-base"
	default:
		return "gpu-h100x1-base"
	}
}

// normalizeGPUModel strips DO's vendor prefix so `--gpu h100` matches
// "nvidia_h100" and `--gpu mi300x` matches "amd_mi300x".
func normalizeGPUModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	for _, p := range []string{"nvidia_", "amd_"} {
		if strings.HasPrefix(m, p) {
			return strings.TrimPrefix(m, p)
		}
	}
	return m
}

// modelMatches compares a user-supplied model against a normalized catalog model,
// tolerating the vendor prefix on either side and a bare-family request (e.g.
// "rtx6000" matching "rtx6000_ada").
func modelMatches(want, have string) bool {
	want, have = normalizeGPUModel(want), normalizeGPUModel(have)
	return want == have || strings.HasPrefix(have, want+"_") || strings.HasPrefix(want, have+"_")
}

// vramGB converts a DO VRAM amount+unit to whole GB.
func vramGB(amount int, unit string) int {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "mib", "mb":
		return amount / 1024
	default: // gib, gb, ""
		return amount
	}
}

// pickRegion returns the first preferred region present in avail, else the first
// available region.
func pickRegion(avail, pref []string) string {
	for _, p := range pref {
		for _, a := range avail {
			if a == p {
				return a
			}
		}
	}
	if len(avail) > 0 {
		return avail[0]
	}
	return ""
}

var _ provider.GPUProvider = (*DO)(nil)
