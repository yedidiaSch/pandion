// SPDX-License-Identifier: AGPL-3.0-or-later

package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// A --gpu request flows through PlanUp and is priced from the provider's GPU
// catalog (mock: a100 = €1.10/hr), scaling with the requested count.
func TestPlanUpGPU(t *testing.T) {
	o, _ := newOrch(t)
	specs := []NodeSpec{{Name: "trainer", GPU: provider.GPUReq{Model: "a100", Count: 2}}}

	nodes, est, err := o.PlanUp(context.Background(), specs, []time.Duration{time.Hour})
	if err != nil {
		t.Fatalf("planup gpu: %v", err)
	}
	if got := nodes[0].Hourly.Amount; got != 2.20 { // 1.10 × 2
		t.Fatalf("a100×2 hourly = %.4f, want 2.20", got)
	}
	if nodes[0].GPU.Model != "a100" || nodes[0].GPU.Count != 2 {
		t.Fatalf("plan lost the GPU req: %+v", nodes[0].GPU)
	}
	if est.Hourly != 2.20 || est.Projected != 2.20 {
		t.Fatalf("est = %+v", est)
	}
}

// CheckBudget must fail CLOSED for a GPU request the provider cannot price — the
// budget guard is never silently skipped on the most expensive nodes.
func TestCheckBudgetGPUUnpriceable(t *testing.T) {
	o, _ := newOrch(t)
	specs := []NodeSpec{{Name: "x", GPU: provider.GPUReq{Model: "b200", Count: 1}}} // not in mock catalog

	err := o.CheckBudget(context.Background(), specs, []time.Duration{time.Hour}, 100.0)
	if err == nil {
		t.Fatal("an unpriceable GPU node must fail the budget check, not pass it")
	}
}

// A priced GPU request over the cap is rejected before anything is created.
func TestCheckBudgetGPUOverCap(t *testing.T) {
	o, _ := newOrch(t)
	specs := []NodeSpec{{Name: "h", GPU: provider.GPUReq{Model: "h100", Count: 1}}} // €2.50/hr

	// 2h window ⇒ €5.00 projected, cap €1.00 ⇒ reject.
	if err := o.CheckBudget(context.Background(), specs, []time.Duration{2 * time.Hour}, 1.00); err == nil {
		t.Fatal("projected €5.00 over a €1.00 cap must be rejected")
	}
	// generous cap ⇒ allowed.
	if err := o.CheckBudget(context.Background(), specs, []time.Duration{2 * time.Hour}, 100.00); err != nil {
		t.Fatalf("within cap should pass: %v", err)
	}
}
