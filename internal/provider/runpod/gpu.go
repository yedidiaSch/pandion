// SPDX-License-Identifier: AGPL-3.0-or-later

package runpod

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// gpuType is one entry of GET /v2/catalog/gpus?include=AVAILABILITY&product=POD.
type gpuType struct {
	ID           string `json:"id"`   // "NVIDIA RTX A4000" — used to create pods
	Name         string `json:"name"` // "RTX A4000"
	Manufacturer string `json:"manufacturer"`
	Memory       int    `json:"memory"` // VRAM GB
	Secure       bool   `json:"secure"`
	Community    bool   `json:"community"`
	Price        struct {
		Secure    float64 `json:"secure"`
		Community float64 `json:"community"`
	} `json:"price"` // USD/hr for ONE GPU
	MaxCount struct {
		Secure    int `json:"secure"`
		Community int `json:"community"`
	} `json:"maxCount"`
	Availability string `json:"availability"` // NONE | LOW | MEDIUM | HIGH
	DataCenters  []struct {
		ID           string `json:"id"`
		Availability string `json:"availability"`
	} `json:"dataCenters"`
}

// catalogTTL bounds how long a catalog answer is reused within one command.
const catalogTTL = time.Minute

// gpuTypes returns the catalog for pods of count GPUs on the selected cloud tier,
// with live availability. Cached briefly so dry-run/resolve/create agree.
func (r *RunPod) gpuTypes(ctx context.Context, count int) ([]gpuType, error) {
	count = max(count, 1)
	r.catMu.Lock()
	if e, ok := r.catalog[count]; ok && time.Since(e.at) < catalogTTL {
		r.catMu.Unlock()
		return e.gpus, nil
	}
	r.catMu.Unlock()
	q := url.Values{}
	q.Set("include", "AVAILABILITY")
	q.Set("product", "POD")
	q.Set("cloud", r.cloud)
	q.Set("count", strconv.Itoa(count))
	var resp struct {
		GPUs []gpuType `json:"gpus"`
	}
	if err := r.do(ctx, http.MethodGet, "/catalog/gpus?"+q.Encode(), nil, &resp); err != nil {
		return nil, fmt.Errorf("runpod: GPU catalog: %w", err)
	}
	r.catMu.Lock()
	r.catalog[count] = catalogEntry{at: time.Now(), gpus: resp.GPUs}
	r.catMu.Unlock()
	return resp.GPUs, nil
}

// onTier reports whether g is offered on the selected tier, its per-GPU price
// there, and the most GPUs one pod can have.
func (r *RunPod) onTier(g gpuType) (ok bool, price float64, maxCount int) {
	if r.cloud == CloudCommunity {
		return g.Community, g.Price.Community, g.MaxCount.Community
	}
	return g.Secure, g.Price.Secure, g.MaxCount.Secure
}

func available(a string) bool { return a != "" && !strings.EqualFold(a, "NONE") }

// GPUOfferings implements provider.GPUProvider: single-GPU pod types on the
// selected tier that have capacity now, cheapest first. Regions are the data
// centers reporting availability.
func (r *RunPod) GPUOfferings(ctx context.Context) ([]provider.GPUOffering, error) {
	gpus, err := r.gpuTypes(ctx, 1)
	if err != nil {
		return nil, err
	}
	var out []provider.GPUOffering
	for _, g := range gpus {
		if o, ok := r.offering(g, 1); ok {
			out = append(out, o)
		}
	}
	sortOfferings(out)
	return out, nil
}

// offering builds the priced offering for count GPUs of type g, if available.
func (r *RunPod) offering(g gpuType, count int) (provider.GPUOffering, bool) {
	ok, price, maxCount := r.onTier(g)
	if !ok || price <= 0 || !available(g.Availability) || (maxCount > 0 && count > maxCount) {
		return provider.GPUOffering{}, false
	}
	var regions []string
	for _, dc := range g.DataCenters {
		if available(dc.Availability) {
			regions = append(regions, dc.ID)
		}
	}
	sort.Strings(regions)
	return provider.GPUOffering{
		ServerType: typeName(g.ID, count),
		GPU:        provider.GPUInfo{Model: normModel(g.ID), Count: count, VRAM: g.Memory},
		Regions:    regions,
		Hourly:     provider.Money{Amount: price * float64(count), Currency: "USD"},
		Image:      DefaultImage,
	}, true
}

func sortOfferings(o []provider.GPUOffering) {
	sort.Slice(o, func(i, j int) bool {
		if o[i].Hourly.Amount != o[j].Hourly.Amount {
			return o[i].Hourly.Amount < o[j].Hourly.Amount
		}
		return o[i].ServerType < o[j].ServerType
	})
}

// ResolveGPUType implements provider.GPUProvider: the cheapest available type
// matching req on the selected tier (deterministic, so --dry-run and up agree).
// An explicit region list is honored strictly.
func (r *RunPod) ResolveGPUType(ctx context.Context, req provider.GPUReq, regionPref []string) (string, string, error) {
	count := max(req.Count, 1)
	gpus, err := r.gpuTypes(ctx, count)
	if err != nil {
		return "", "", err
	}
	var cands []provider.GPUOffering
	for _, g := range gpus {
		if req.Model != "" && !modelMatches(req.Model, g.ID) {
			continue
		}
		if req.MinVRAM > 0 && g.Memory < req.MinVRAM {
			continue
		}
		if o, ok := r.offering(g, count); ok {
			cands = append(cands, o)
		}
	}
	sortOfferings(cands)
	for _, o := range cands {
		if len(regionPref) == 0 {
			return o.ServerType, "", nil // let RunPod's scheduler place it
		}
		for _, want := range regionPref {
			for _, have := range o.Regions {
				if strings.EqualFold(want, have) {
					return o.ServerType, have, nil
				}
			}
		}
	}
	where := ""
	if len(regionPref) > 0 {
		where = " in " + strings.Join(regionPref, ",")
	}
	return "", "", fmt.Errorf("runpod: no %s-cloud GPU with capacity matches model=%q count=%d%s right now (try another model, --cloud community, or `pandion list-gpus --provider runpod`)",
		strings.ToLower(r.cloud), req.Model, count, where)
}

// HourlyPrice implements provider.Pricer: the rate RunPod reported for a live pod
// of this type/region, else the catalog list price on the selected tier.
func (r *RunPod) HourlyPrice(ctx context.Context, serverType, region string) (provider.Money, error) {
	r.catMu.Lock()
	if c, ok := r.observed[serverType+"|"+region]; ok {
		r.catMu.Unlock()
		return provider.Money{Amount: c, Currency: "USD"}, nil
	}
	r.catMu.Unlock()
	slug, count := splitType(serverType)
	gpus, err := r.gpuTypes(ctx, count)
	if err != nil {
		return provider.Money{}, nil // unknown price never breaks a listing
	}
	for _, g := range gpus {
		if typeSlug(g.ID) == slug {
			if _, price, _ := r.onTier(g); price > 0 {
				return provider.Money{Amount: price * float64(count), Currency: "USD"}, nil
			}
		}
	}
	return provider.Money{}, nil
}

// EstimateHourly implements provider.Pricer for the --max-cost / --dry-run preflight.
func (r *RunPod) EstimateHourly(ctx context.Context, spec provider.ServerSpec) (provider.Money, error) {
	typeName := spec.Type
	if typeName == "" {
		if !spec.GPU.Wanted() {
			return provider.Money{}, fmt.Errorf("runpod: cannot price a non-GPU request (pass --gpu)")
		}
		var err error
		if typeName, _, err = r.ResolveGPUType(ctx, spec.GPU, spec.RegionPref); err != nil {
			return provider.Money{}, err
		}
	}
	return r.HourlyPrice(ctx, typeName, "")
}

// --- type names + model matching ----------------------------------------------

// vendorWords are dropped from GPU ids when deriving slugs/models.
var vendorWords = map[string]bool{"nvidia": true, "geforce": true, "amd": true, "radeon": true}

// typeSlug turns a RunPod GPU id into Pandion's type name: "NVIDIA RTX A4000" →
// "rtx-a4000", "NVIDIA GeForce RTX 4090" → "rtx-4090".
func typeSlug(gpuID string) string {
	var parts []string
	for _, f := range strings.Fields(strings.ToLower(gpuID)) {
		if !vendorWords[f] {
			parts = append(parts, f)
		}
	}
	return sanitize(strings.Join(parts, "-"))
}

// typeName is the Pandion server type for count GPUs of a type: the slug, with an
// "xN" suffix for multi-GPU pods ("rtx-4090x2").
func typeName(gpuID string, count int) string {
	if count > 1 {
		return fmt.Sprintf("%sx%d", typeSlug(gpuID), count)
	}
	return typeSlug(gpuID)
}

// splitType inverts typeName.
func splitType(t string) (slug string, count int) {
	t = strings.ToLower(strings.TrimSpace(t))
	if i := strings.LastIndex(t, "x"); i > 0 && i < len(t)-1 {
		if n, err := strconv.Atoi(t[i+1:]); err == nil && n > 1 {
			return t[:i], n
		}
	}
	return t, 1
}

// normModel is the compact model token for a GPU id ("rtxa4000", "rtx4090", "a40").
func normModel(s string) string { return strings.ReplaceAll(typeSlug(s), "-", "") }

// modelMatches reports whether a --gpu model names this RunPod GPU id, matching
// whole leading name tokens: "rtx4090"/"4090" ⇒ "RTX 4090", "a4000" ⇒ "RTX A4000",
// "a100" ⇒ "A100 80GB PCIe" and "A100-SXM4-80GB", "h100" ⇒ every H100 — but "a40"
// never matches "A4000" and "a10" never matches "A100".
func modelMatches(want, gpuID string) bool {
	w := normModel(want)
	toks := strings.Split(typeSlug(gpuID), "-")
	if w == "" || len(toks) == 0 || toks[0] == "" {
		return false
	}
	try := [][]string{toks}
	if toks[0] == "rtx" && len(toks) > 1 {
		try = append(try, toks[1:])
	}
	for _, t := range try {
		acc := ""
		for _, tok := range t {
			acc += tok
			if acc == w {
				return true
			}
		}
	}
	return false
}
