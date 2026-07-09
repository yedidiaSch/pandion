// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/orchestrator"
	"github.com/yedidiaSch/pandion/internal/provider"
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
