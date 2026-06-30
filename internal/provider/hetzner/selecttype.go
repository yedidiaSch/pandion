package hetzner

import "sort"

// typeInfo is a provider-agnostic view of a server type, used so the selection
// logic can be unit-tested without the hcloud SDK or any network.
type typeInfo struct {
	Name       string
	Cores      int
	MemGB      float64
	Arch       string // "x86" | "arm"
	Deprecated bool
}

// selectCandidates implements spike S1 finding F3: never hardcode server-type
// names (they rotate — cpx11 is retired). Instead, pick types by SPEC:
//   - architecture must match
//   - skip deprecated types
//   - meet minimum cores and RAM
//   - cheapest-first (smallest cores, then least RAM, then name for stability)
//
// It returns an ordered list of type names to try; the caller then searches
// type x location for an available combination (availability is sparse).
func selectCandidates(types []typeInfo, minCores, minRAMGB int, arch string) []string {
	var ok []typeInfo
	for _, t := range types {
		if t.Arch != arch || t.Deprecated {
			continue
		}
		if t.Cores < minCores || t.MemGB < float64(minRAMGB) {
			continue
		}
		ok = append(ok, t)
	}
	sort.Slice(ok, func(i, j int) bool {
		if ok[i].Cores != ok[j].Cores {
			return ok[i].Cores < ok[j].Cores
		}
		if ok[i].MemGB != ok[j].MemGB {
			return ok[i].MemGB < ok[j].MemGB
		}
		return ok[i].Name < ok[j].Name
	})
	names := make([]string, len(ok))
	for i, t := range ok {
		names[i] = t.Name
	}
	return names
}

// orderLocations puts preferred regions first (in the given order), then the rest.
func orderLocations(all, preferred []string) []string {
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
