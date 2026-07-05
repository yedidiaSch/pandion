package vultr

import (
	"testing"
	"time"

	"github.com/vultr/govultr/v3"
	"github.com/yedidiaSch/pandion/internal/provider"
)

// compile-time: Vultr satisfies the core seam plus the optional capabilities.
var (
	_ provider.Provider  = (*Vultr)(nil)
	_ provider.Pricer    = (*Vultr)(nil)
	_ provider.AuxReaper = (*Vultr)(nil)
)

func TestSelectPlans_CheapestFirst_FiltersSpecRegionGPU(t *testing.T) {
	plans := []planInfo{
		{ID: "vc2-1c-1gb", VCPU: 1, RAMMB: 1024, MonthlyCost: 5, Regions: []string{"ewr", "fra"}},
		{ID: "vc2-2c-2gb", VCPU: 2, RAMMB: 2048, MonthlyCost: 12, Regions: []string{"ewr", "fra"}},
		{ID: "vc2-2c-4gb", VCPU: 2, RAMMB: 4096, MonthlyCost: 24, Regions: []string{"ewr"}},
		{ID: "vcg-8c-gpu", VCPU: 8, RAMMB: 16384, MonthlyCost: 900, Regions: []string{"ewr"}, GPU: true},
	}

	// spec: >=2 vcpu / >=2GB, any region. Excludes the 1-vcpu and the GPU;
	// cheapest-first ⇒ vc2-2c-2gb before vc2-2c-4gb.
	got := selectPlans(plans, 2, 2048, "")
	if len(got) != 2 || got[0].ID != "vc2-2c-2gb" || got[1].ID != "vc2-2c-4gb" {
		t.Fatalf("unexpected selection: %+v", ids(got))
	}

	// region filter: fra only has the 2gb (4gb is ewr-only).
	got = selectPlans(plans, 2, 2048, "fra")
	if len(got) != 1 || got[0].ID != "vc2-2c-2gb" {
		t.Fatalf("region filter wrong: %+v", ids(got))
	}
}

func TestOrderRegions_PreferredFirst(t *testing.T) {
	got := orderRegions([]string{"ewr", "fra", "sea"}, []string{"fra", "lon"})
	want := []string{"fra", "ewr", "sea"} // fra first (preferred+present), lon ignored (absent)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("orderRegions = %v, want %v", got, want)
	}
}

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

func TestInstanceLabel_SanitizedAndBounded(t *testing.T) {
	if n := instanceLabel("pipeline", "broker"); n != "pandion-pipeline-broker" {
		t.Fatalf("label = %q", n)
	}
	long := instanceLabel("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if len(long) > 63 {
		t.Fatalf("label not bounded to 63: %d", len(long))
	}
}

func TestToServer_ParsesFields(t *testing.T) {
	created := time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	inst := &govultr.Instance{
		ID:          "abc-123",
		Label:       "pandion-pipeline-broker",
		Plan:        "vc2-2c-2gb",
		Region:      "fra",
		MainIP:      "203.0.113.7",
		DateCreated: created,
	}
	s := toServer(inst, "pipeline")
	if s.ID != "abc-123" || s.Type != "vc2-2c-2gb" || s.Region != "fra" || s.IP != "203.0.113.7" {
		t.Fatalf("toServer mapped wrong: %+v", s)
	}
	if s.Created.IsZero() || time.Since(s.Created) < time.Hour {
		t.Fatalf("created not parsed: %v", s.Created)
	}
}

func TestClassifyErrors(t *testing.T) {
	if !isAvailabilityErr(errStr("plan is not available in this region")) {
		t.Fatal("expected availability error")
	}
	if !isNotFoundErr(errStr("Instance not found")) {
		t.Fatal("expected not-found error")
	}
	if isNotFoundErr(errStr("rate limit exceeded")) {
		t.Fatal("rate limit is not not-found")
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }

func ids(ps []planInfo) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}
