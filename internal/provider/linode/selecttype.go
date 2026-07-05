package linode

import "sort"

// typeInfo is a provider-agnostic view of a Linode type, so the selection logic
// is unit-testable without the linodego SDK or any network.
//
// Unlike DigitalOcean/Vultr, Linode types are not tied to a per-type region list
// (standard types are orderable in every standard region), so selection does not
// filter by region — creation attempts region x type and falls back on sold-out,
// like the Hetzner backend. Price, however, CAN vary by region, so we keep the
// per-region overrides for the Pricer.
type typeInfo struct {
	ID          string
	VCPU        int
	MemMB       int
	HourlyUSD   float64            // base gross hourly price (USD)
	RegionPrice map[string]float64 // region id -> hourly override (USD)
	GPU         bool
}

// hourlyIn returns the price for a region, honoring a per-region override.
func (t typeInfo) hourlyIn(region string) float64 {
	if p, ok := t.RegionPrice[region]; ok && p > 0 {
		return p
	}
	return t.HourlyUSD
}

// selectTypes picks types meeting the spec, CHEAPEST-FIRST (by base hourly).
// GPU types are excluded (they cost far more and need a different hardened
// profile — roadmap MX).
func selectTypes(types []typeInfo, minCores, minRAMMB int) []typeInfo {
	var ok []typeInfo
	for _, t := range types {
		if t.GPU {
			continue
		}
		if t.VCPU < minCores || t.MemMB < minRAMMB {
			continue
		}
		ok = append(ok, t)
	}
	sort.Slice(ok, func(i, j int) bool {
		if ok[i].HourlyUSD != ok[j].HourlyUSD {
			return ok[i].HourlyUSD < ok[j].HourlyUSD
		}
		return ok[i].ID < ok[j].ID
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

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
