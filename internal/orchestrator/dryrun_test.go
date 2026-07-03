package orchestrator

import (
	"context"
	"testing"
	"time"
)

// PlanUp previews per-node plan + rolled-up cost, and flags an unbounded node
// (no TTL) without erroring — the mock prices every node at €0.01/hr.
func TestPlanUp(t *testing.T) {
	o, _ := newOrch(t)
	specs := []NodeSpec{{Name: "broker", Type: "cpx21"}, {Name: "worker"}}
	windows := []time.Duration{2 * time.Hour, 0} // worker has no TTL

	nodes, est, err := o.PlanUp(context.Background(), specs, windows)
	if err != nil {
		t.Fatalf("planup: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(nodes))
	}
	if nodes[0].Size != "cpx21" || nodes[1].Size != "" { // explicit vs auto
		t.Fatalf("sizes: %q, %q", nodes[0].Size, nodes[1].Size)
	}
	if !nodes[0].Hourly.Known() || nodes[0].Hourly.Amount != 0.01 {
		t.Fatalf("node price: %+v", nodes[0].Hourly)
	}
	if est.Hourly != 0.02 || est.Currency != "EUR" {
		t.Fatalf("aggregate hourly: %.4f %s", est.Hourly, est.Currency)
	}
	// projected counts only the TTL'd node: 0.01 × 2h = 0.02; worker is unbounded.
	if est.Projected < 0.0199 || est.Projected > 0.0201 {
		t.Fatalf("projected = %.4f, want ~0.02", est.Projected)
	}
	if !est.Unbounded {
		t.Fatal("a node without TTL must set Unbounded")
	}
}
