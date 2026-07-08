// SPDX-License-Identifier: AGPL-3.0-or-later

package vultr

import "sort"

// planInfo is a provider-agnostic view of a Vultr plan, so the selection logic is
// unit-testable without the govultr SDK or any network.
type planInfo struct {
	ID          string
	VCPU        int
	RAMMB       int
	MonthlyCost float64  // USD/month (Vultr bills monthly; we derive hourly)
	Regions     []string // region ids where this plan is orderable (Plan.Locations)
	GPU         bool
}

// hoursPerMonth converts Vultr's monthly price to the gross hourly rate Pandion
// reports. Vultr itself bills hourly capped at the monthly figure, using 730h.
const hoursPerMonth = 730.0

// selectPlans picks plans meeting the spec, CHEAPEST-FIRST. Like DigitalOcean,
// Vultr exposes a real price and per-plan region availability (Plan.Locations),
// so we order by price directly and can pre-filter to a region. GPU plans are
// excluded (they cost far more and need a different hardened profile — roadmap
// MX). region == "" means "any".
func selectPlans(plans []planInfo, minCores, minRAMMB int, region string) []planInfo {
	var ok []planInfo
	for _, p := range plans {
		if p.GPU {
			continue
		}
		if p.VCPU < minCores || p.RAMMB < minRAMMB {
			continue
		}
		if region != "" && !contains(p.Regions, region) {
			continue
		}
		ok = append(ok, p)
	}
	sort.Slice(ok, func(i, j int) bool {
		if ok[i].MonthlyCost != ok[j].MonthlyCost {
			return ok[i].MonthlyCost < ok[j].MonthlyCost
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

// regionsOf is the union of regions across the given plans (candidate regions to
// search, before preference ordering).
func regionsOf(plans []planInfo) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range plans {
		for _, r := range p.Regions {
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
