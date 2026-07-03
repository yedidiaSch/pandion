package digitalocean

import "sort"

// sizeInfo is a provider-agnostic view of a DigitalOcean size, so the selection
// logic is unit-testable without the godo SDK or any network.
type sizeInfo struct {
	Slug        string
	Vcpus       int
	MemMB       int
	PriceHourly float64
	Regions     []string // region slugs where this size is orderable
	Available   bool
	GPU         bool
}

// selectSizes picks sizes meeting the spec, CHEAPEST-FIRST. Unlike Hetzner, DO
// exposes a real hourly price and per-size region availability, so we order by
// price directly and can pre-filter to a region. GPU sizes are excluded (they
// cost ~100x and need a different hardened profile — roadmap MX), and
// unavailable sizes are skipped. region == "" means "any".
func selectSizes(sizes []sizeInfo, minCores, minRAMMB int, region string) []sizeInfo {
	var ok []sizeInfo
	for _, s := range sizes {
		if !s.Available || s.GPU {
			continue
		}
		if s.Vcpus < minCores || s.MemMB < minRAMMB {
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

// orderRegions puts preferred regions first (in the given order), then the rest.
func orderRegions(all, preferred []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range preferred {
		for _, a := range all {
			if a == p && !seen[a] {
				out = append(out, a)
				seen[a] = true
			}
		}
	}
	for _, a := range all {
		if !seen[a] {
			out = append(out, a)
			seen[a] = true
		}
	}
	return out
}

// regionsOf is the union of regions across the given sizes (candidate regions to
// search, before preference ordering).
func regionsOf(sizes []sizeInfo) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sizes {
		for _, r := range s.Regions {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
