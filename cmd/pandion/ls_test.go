package main

import (
	"strings"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/orchestrator"
	"github.com/yedidiaSch/pandion/internal/provider"
)

func TestRenderStatus_TableAndTotals(t *testing.T) {
	clusters := []orchestrator.ClusterStatus{{
		ClusterID: "pipeline",
		Nodes: []orchestrator.NodeStatus{
			{Name: "broker", Type: "cpx21", Region: "nbg1", Age: 2*time.Hour + 13*time.Minute,
				Hourly: provider.Money{Amount: 0.008, Currency: "EUR"}},
			{Name: "worker", Type: "cpx21", Region: "nbg1", Age: 2*time.Hour + 13*time.Minute,
				Hourly: provider.Money{Amount: 0.008, Currency: "EUR"}},
		},
		Hourly: 0.016, Accrued: 0.0354,
	}}

	var b strings.Builder
	renderStatus(&b, clusters, "EUR")
	out := b.String()

	for _, want := range []string{
		"CLUSTER", "NODE", "UPTIME", "EUR/hr", // header
		"pipeline", "broker", "worker", // cluster labeled once, both nodes
		"cpx21", "nbg1", "2h13m", // per-node fields
		"0.0080", // hourly
		"2 node(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// the cluster id should be printed once, not on every node row.
	if strings.Count(out, "pipeline") != 1 {
		t.Errorf("cluster id should appear once, got %d:\n%s", strings.Count(out, "pipeline"), out)
	}
}

// An unpriced node renders "—" rather than a bogus 0, and shortDur handles ranges.
func TestRenderStatus_UnpricedAndDurations(t *testing.T) {
	if got := shortDur(0); got != "0m" {
		t.Errorf("shortDur(0)=%q", got)
	}
	if got := shortDur(49 * time.Hour); got != "2d1h" {
		t.Errorf("shortDur(49h)=%q, want 2d1h", got)
	}
	if got := money(provider.Money{}); got != "—" {
		t.Errorf("unpriced money=%q, want —", got)
	}
}
