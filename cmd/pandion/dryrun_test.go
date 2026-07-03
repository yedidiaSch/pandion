package main

import (
	"strings"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/orchestrator"
	"github.com/yedidiaSch/pandion/internal/provider"
)

func TestRenderDryRun_TableTotalsAndAuto(t *testing.T) {
	nodes := []orchestrator.DryRunNode{
		{Name: "broker", Size: "cpx21", Region: "fsn1", Hourly: provider.Money{Amount: 0.008, Currency: "EUR"}, Window: 2 * time.Hour},
		{Name: "worker", Size: "", Region: "", Hourly: provider.Money{Amount: 0.008, Currency: "EUR"}, Window: 30 * time.Minute},
	}
	est := orchestrator.CostEstimate{Hourly: 0.016, Projected: 0.020, Currency: "EUR"}

	var b strings.Builder
	renderDryRun(&b, "hetzner", "pipeline", nodes, est)
	out := b.String()

	for _, want := range []string{
		"DRY RUN", "nothing will be created", "provider=hetzner", "pipeline",
		"broker", "cpx21", "fsn1", "2h00m",
		"worker", "auto", // empty size/region render as "auto"
		"0.0080",        // per-node hourly
		"2 node(s)",     // total line
		"0.0160 EUR/hr", // aggregate hourly
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderDryRun_UnboundedAndTtlLabels(t *testing.T) {
	nodes := []orchestrator.DryRunNode{
		{Name: "a", Hourly: provider.Money{Amount: 0.01, Currency: "EUR"}, Window: 0}, // no TTL
	}
	est := orchestrator.CostEstimate{Hourly: 0.01, Currency: "EUR", Unbounded: true}

	var b strings.Builder
	renderDryRun(&b, "mock", "x", nodes, est)
	out := b.String()
	if !strings.Contains(out, "unbounded") {
		t.Errorf("expected 'unbounded' for a no-TTL node:\n%s", out)
	}
	if !strings.Contains(out, "none") { // TTL column for a no-TTL node
		t.Errorf("expected TTL 'none':\n%s", out)
	}
}
