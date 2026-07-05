package scaleway

import "sort"

// typeInfo is a provider-agnostic view of a Scaleway commercial instance type,
// so the selection logic is unit-testable without the SDK or any network.
//
// Scaleway server types are per-ZONE (a type may exist in fr-par but not pl-waw),
// so — like Hetzner — selection happens against a single zone's catalog and
// creation falls back across zones. Prices are in EUR/hour.
type typeInfo struct {
	Name      string // commercial type, e.g. "PLAY2-NANO", "PRO2-XS"
	NCPUs     int
	RAMMB     int
	HourlyEUR float64
	GPU       bool
	EOS       bool // end of service — never provision onto these
}

// selectTypes picks types meeting the spec, CHEAPEST-FIRST. GPU and end-of-service
// types are excluded (GPU costs far more and needs a different hardened profile —
// roadmap MX; EOS types are being retired).
func selectTypes(types []typeInfo, minCores, minRAMMB int) []typeInfo {
	var ok []typeInfo
	for _, t := range types {
		if t.GPU || t.EOS {
			continue
		}
		if t.NCPUs < minCores || t.RAMMB < minRAMMB {
			continue
		}
		ok = append(ok, t)
	}
	sort.Slice(ok, func(i, j int) bool {
		if ok[i].HourlyEUR != ok[j].HourlyEUR {
			return ok[i].HourlyEUR < ok[j].HourlyEUR
		}
		return ok[i].Name < ok[j].Name
	})
	return ok
}

// orderZones puts preferred zones first (in the given order), then the rest.
func orderZones(all, preferred []string) []string {
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
