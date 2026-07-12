// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/orchestrator"
	"github.com/yedidiaSch/pandion/internal/provider"
	"github.com/yedidiaSch/pandion/internal/provider/mock"
)

func TestParseGPUFlag(t *testing.T) {
	cases := []struct {
		in        string
		wantModel string
		wantCount int
		wantErr   bool
	}{
		{"", "", 0, false},         // no GPU
		{"a100", "a100", 1, false}, // model only ⇒ count 1
		{"a100:2", "a100", 2, false},
		{" h100 : 4 ", "h100", 4, false}, // whitespace tolerated
		{"a100:0", "", 0, true},          // count must be >= 1
		{"a100:x", "", 0, true},          // non-numeric count
		{":2", "", 0, true},              // model required
	}
	for _, c := range cases {
		got, err := parseGPUFlag(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseGPUFlag(%q): want error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseGPUFlag(%q): unexpected error %v", c.in, err)
			continue
		}
		if got.Model != c.wantModel || got.Count != c.wantCount {
			t.Errorf("parseGPUFlag(%q) = %+v, want model=%q count=%d", c.in, got, c.wantModel, c.wantCount)
		}
	}
}

func TestGPULabel(t *testing.T) {
	cases := map[string]struct {
		req  provider.GPUReq
		want string
	}{
		"cpu":    {provider.GPUReq{}, "—"},
		"single": {provider.GPUReq{Model: "a100", Count: 1}, "a100"},
		"multi":  {provider.GPUReq{Model: "h100", Count: 2}, "h100×2"},
		"any":    {provider.GPUReq{Count: 1}, "any"},
	}
	for name, c := range cases {
		if got := gpuLabel(c.req); got != c.want {
			t.Errorf("%s: gpuLabel = %q, want %q", name, got, c.want)
		}
	}
}

// The dry-run table gains a GPU column: CPU nodes show "—", GPU nodes their model.
func TestRenderDryRun_GPUColumn(t *testing.T) {
	nodes := []orchestrator.DryRunNode{
		{Name: "trainer", Hourly: provider.Money{Amount: 1.10, Currency: "EUR"},
			Window: 2 * time.Hour, GPU: provider.GPUReq{Model: "a100", Count: 1}},
		{Name: "plain", Hourly: provider.Money{Amount: 0.01, Currency: "EUR"}, Window: time.Hour},
	}
	est := orchestrator.CostEstimate{Hourly: 1.11, Projected: 2.21, Currency: "EUR"}

	var b strings.Builder
	renderDryRun(&b, "mock", "ml", nodes, est)
	out := b.String()

	for _, want := range []string{"GPU", "a100", "trainer", "—" /* cpu node */} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run GPU output missing %q:\n%s", want, out)
		}
	}
}

// The `ls` table shows a GPU column: CPU nodes "—", GPU nodes their model×count.
func TestRenderStatus_GPUColumn(t *testing.T) {
	clusters := []orchestrator.ClusterStatus{{
		ClusterID: "ml",
		Nodes: []orchestrator.NodeStatus{
			{Name: "trainer", Type: "gpu_8x_h100", Region: "us-west-1", Age: time.Hour,
				Hourly: provider.Money{Amount: 23.92, Currency: "USD"},
				GPU:    provider.GPUInfo{Model: "h100", Count: 8, VRAM: 80}},
			{Name: "plain", Type: "cpx21", Region: "fsn1", Age: time.Hour,
				Hourly: provider.Money{Amount: 0.01, Currency: "USD"}},
		},
		Hourly: 23.93,
	}}
	var b strings.Builder
	renderStatus(&b, clusters, "USD", false)
	out := b.String()
	for _, want := range []string{"GPU", "h100×8", "trainer"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls GPU column missing %q:\n%s", want, out)
		}
	}
}

// M6-R2: `ls --gpu-util` adds a UTIL% column; measured values render, unknown "—".
func TestRenderStatus_GPUUtilColumn(t *testing.T) {
	clusters := []orchestrator.ClusterStatus{{
		ClusterID: "ml",
		Nodes: []orchestrator.NodeStatus{
			{Name: "busy", Type: "gpu_1x_a10", Region: "us", Age: time.Hour,
				GPU: provider.GPUInfo{Model: "a10", Count: 1}, GPUUtil: 97},
			{Name: "unreach", Type: "gpu_1x_a10", Region: "us", Age: time.Hour,
				GPU: provider.GPUInfo{Model: "a10", Count: 1}, GPUUtil: -1},
		},
	}}
	var b strings.Builder
	renderStatus(&b, clusters, "USD", true)
	out := b.String()
	for _, want := range []string{"UTIL%", "97%", "—" /* unreachable */} {
		if !strings.Contains(out, want) {
			t.Errorf("gpu-util output missing %q:\n%s", want, out)
		}
	}
	var b2 strings.Builder
	renderStatus(&b2, clusters, "USD", false)
	if strings.Contains(b2.String(), "UTIL%") {
		t.Error("UTIL column must only appear with --gpu-util")
	}
}

func TestGPUUtilLabel(t *testing.T) {
	if gpuUtilLabel(-1) != "—" || gpuUtilLabel(0) != "0%" || gpuUtilLabel(100) != "100%" {
		t.Error("gpuUtilLabel wrong")
	}
}

func TestRenderReceipt(t *testing.T) {
	// priced GPU teardown
	var b strings.Builder
	renderReceipt(&b, receipt{nodes: 1, ran: 2*time.Hour + 13*time.Minute, ranKnown: true,
		total: 5.31, currency: "USD", priced: true, gpus: []string{"a100"}})
	out := b.String()
	for _, want := range []string{"1 node(s)", "(a100)", "ran 2h13m", "total ~5.31 USD"} {
		if !strings.Contains(out, want) {
			t.Errorf("receipt missing %q: %q", want, out)
		}
	}

	// unpriced provider ⇒ "cost unknown", no bogus total
	b.Reset()
	renderReceipt(&b, receipt{nodes: 2, ran: time.Hour, ranKnown: true})
	if got := b.String(); !strings.Contains(got, "cost unknown") || strings.Contains(got, "total") {
		t.Errorf("unpriced receipt wrong: %q", got)
	}

	// no nodes ⇒ no line at all
	b.Reset()
	renderReceipt(&b, receipt{})
	if b.String() != "" {
		t.Errorf("empty receipt should print nothing, got %q", b.String())
	}
}

// buildReceipt against the mock provider: a GPU node priced from the catalog,
// with a nonzero runtime and the GPU label captured.
func TestBuildReceipt_Mock(t *testing.T) {
	m := mock.New()
	ctx := context.Background()
	if _, err := m.CreateServer(ctx, provider.ServerSpec{
		Name: "g", ClusterID: "c", GPU: provider.GPUReq{Model: "a100", Count: 1},
	}); err != nil {
		t.Fatal(err)
	}
	servers, _ := m.ListByTag(ctx, "c")
	r := buildReceipt(ctx, m, servers, time.Time{})
	if r.nodes != 1 || !r.priced || !r.ranKnown {
		t.Fatalf("receipt = %+v", r)
	}
	if len(r.gpus) != 1 || r.gpus[0] != "a100" {
		t.Fatalf("gpu label = %v", r.gpus)
	}
	if r.currency != "EUR" || r.total < 0 {
		t.Fatalf("cost = %.4f %s", r.total, r.currency)
	}
}

// M6-R1: a provider that reports NO creation time (Lambda) still gets a priced
// receipt via the lockfile fallback, instead of "cost unknown".
func TestBuildReceipt_FallbackCreated(t *testing.T) {
	m := mock.New()
	ctx := context.Background()
	// a server with a zero Created (as Lambda returns) — priced type, no timestamp.
	servers := []provider.Server{{
		ID: "i-1", Name: "n", ClusterID: "c", Type: "mock-gpu-a100", Region: "mock-dc",
		GPU: provider.GPUInfo{Model: "a100", Count: 1, VRAM: 40}, // Created is zero
	}}
	// without a fallback: unpriced (no age to multiply)
	if r := buildReceipt(ctx, m, servers, time.Time{}); r.priced || r.ranKnown {
		t.Fatalf("no timestamp should yield cost unknown: %+v", r)
	}
	// with the lockfile fallback (created 2h ago): real cost from age × hourly
	r := buildReceipt(ctx, m, servers, time.Now().Add(-2*time.Hour))
	if !r.ranKnown || !r.priced {
		t.Fatalf("fallback should produce a priced receipt: %+v", r)
	}
	if r.total < 2.0 || r.total > 2.4 { // a100 = 1.10/hr × ~2h
		t.Fatalf("expected ~2.20, got %.4f %s", r.total, r.currency)
	}
}
