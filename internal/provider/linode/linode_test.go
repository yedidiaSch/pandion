// SPDX-License-Identifier: AGPL-3.0-or-later

package linode

import (
	"net"
	"testing"
	"time"

	"github.com/linode/linodego"
	"github.com/yedidiaSch/pandion/internal/provider"
)

// compile-time: Linode satisfies the core seam plus the Pricer. It intentionally
// does NOT implement AuxReaper — the login key is installed inline (AuthorizedKeys)
// so there is no auxiliary resource to reap.
var (
	_ provider.Provider = (*Linode)(nil)
	_ provider.Pricer   = (*Linode)(nil)
)

func TestSelectTypes_CheapestFirst_FiltersSpecGPU(t *testing.T) {
	types := []typeInfo{
		{ID: "g6-nanode-1", VCPU: 1, MemMB: 1024, HourlyUSD: 0.0075},
		{ID: "g6-standard-2", VCPU: 2, MemMB: 4096, HourlyUSD: 0.036},
		{ID: "g6-standard-1", VCPU: 1, MemMB: 2048, HourlyUSD: 0.018},
		{ID: "g6-standard-4", VCPU: 4, MemMB: 8192, HourlyUSD: 0.072},
		{ID: "g1-gpu-rtx", VCPU: 8, MemMB: 32768, HourlyUSD: 1.5, GPU: true},
	}

	// spec: >=2 vcpu / >=2GB. Excludes the 1-vcpu ones and the GPU; cheapest-first
	// ⇒ g6-standard-2 before g6-standard-4.
	got := selectTypes(types, 2, 2048)
	if len(got) != 2 || got[0].ID != "g6-standard-2" || got[1].ID != "g6-standard-4" {
		t.Fatalf("unexpected selection: %+v", ids(got))
	}
}

func TestHourlyIn_RegionOverride(t *testing.T) {
	ti := typeInfo{ID: "g6-standard-2", HourlyUSD: 0.036, RegionPrice: map[string]float64{"eu-central": 0.041}}
	if got := ti.hourlyIn("eu-central"); got != 0.041 {
		t.Fatalf("region override = %v, want 0.041", got)
	}
	if got := ti.hourlyIn("us-east"); got != 0.036 {
		t.Fatalf("base price = %v, want 0.036", got)
	}
}

func TestOrderRegions_PreferredFirst(t *testing.T) {
	got := orderRegions([]string{"us-east", "eu-central", "us-west"}, []string{"eu-central", "ap-south"}, false)
	want := []string{"eu-central", "us-east", "us-west"} // eu-central first, ap-south ignored (absent)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("orderRegions = %v, want %v", got, want)
	}
}

// strict (explicit --region) must not append other regions as fallback.
func TestOrderRegions_StrictOnlyPreferred(t *testing.T) {
	all := []string{"us-east", "eu-central", "us-west"}
	if got := orderRegions(all, []string{"eu-central"}, true); len(got) != 1 || got[0] != "eu-central" {
		t.Fatalf("strict orderRegions(eu-central) = %v, want [eu-central]", got)
	}
	if got := orderRegions(all, []string{"ap-south"}, true); len(got) != 0 {
		t.Fatalf("strict orderRegions(absent) = %v, want empty", got)
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
	if len(long) > 64 {
		t.Fatalf("label not bounded to 64: %d", len(long))
	}
}

func TestPublicIP_SkipsPrivate(t *testing.T) {
	priv := net.ParseIP("192.168.135.7")
	pub := net.ParseIP("203.0.113.7")
	if got := publicIP([]*net.IP{&priv, &pub}); got != "203.0.113.7" {
		t.Fatalf("publicIP = %q, want the routable one", got)
	}
	if got := publicIP([]*net.IP{&priv}); got != "" {
		t.Fatalf("publicIP with only private = %q, want empty", got)
	}
}

func TestToServer_ParsesFields(t *testing.T) {
	created := time.Now().Add(-90 * time.Minute).UTC()
	pub := net.ParseIP("203.0.113.7")
	inst := &linodego.Instance{
		ID:      12345,
		Label:   "pandion-pipeline-broker",
		Type:    "g6-standard-2",
		Region:  "eu-central",
		IPv4:    []*net.IP{&pub},
		Created: &created,
	}
	s := toServer(inst, "pipeline")
	if s.ID != "12345" || s.Type != "g6-standard-2" || s.Region != "eu-central" || s.IP != "203.0.113.7" {
		t.Fatalf("toServer mapped wrong: %+v", s)
	}
	if s.Created.IsZero() || time.Since(s.Created) < time.Hour {
		t.Fatalf("created not parsed: %v", s.Created)
	}
}

func TestRandomRootPass_StrongAndUnique(t *testing.T) {
	a, err := randomRootPass()
	if err != nil || len(a) != 32 {
		t.Fatalf("rootpass a=%q err=%v", a, err)
	}
	b, _ := randomRootPass()
	if a == b {
		t.Fatal("two root passwords collided — not random")
	}
}

func TestIsNotFoundErr(t *testing.T) {
	if !isNotFoundErr(&linodego.Error{Code: 404, Message: "Not found"}) {
		t.Fatal("404 should be not-found")
	}
	if isNotFoundErr(&linodego.Error{Code: 429, Message: "rate limited"}) {
		t.Fatal("429 is not not-found")
	}
}

func ids(ts []typeInfo) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
