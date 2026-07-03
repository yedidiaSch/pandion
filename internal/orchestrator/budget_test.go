package orchestrator

import (
	"context"
	"testing"
	"time"
)

// CheckBudget enforces projected spend = Σ(hourly × per-node TTL) against the cap.
// The mock prices every node at €0.01/hr.
func TestCheckBudget(t *testing.T) {
	o, _ := newOrch(t)
	ctx := context.Background()
	two := []NodeSpec{{Name: "a"}, {Name: "b"}} // 2 × €0.01/hr = €0.02/hr

	t.Run("disabled when maxCost<=0", func(t *testing.T) {
		if err := o.CheckBudget(ctx, two, []time.Duration{time.Hour, time.Hour}, 0); err != nil {
			t.Fatalf("cap 0 should disable the check, got %v", err)
		}
	})

	t.Run("under cap passes", func(t *testing.T) {
		// 0.02/hr × 2h = 0.04 <= 1.00
		w := []time.Duration{2 * time.Hour, 2 * time.Hour}
		if err := o.CheckBudget(ctx, two, w, 1.00); err != nil {
			t.Fatalf("should pass under cap, got %v", err)
		}
	})

	t.Run("over cap fails", func(t *testing.T) {
		// 0.02/hr × 100h = 2.00 > 1.00
		w := []time.Duration{100 * time.Hour, 100 * time.Hour}
		if err := o.CheckBudget(ctx, two, w, 1.00); err == nil {
			t.Fatal("should fail over cap")
		}
	})

	t.Run("no-TTL is unbounded under a cap", func(t *testing.T) {
		w := []time.Duration{time.Hour, 0} // node b has no TTL
		err := o.CheckBudget(ctx, two, w, 1.00)
		if err == nil {
			t.Fatal("no-TTL under a cap must error (unbounded projection)")
		}
	})
}

// EstimateSpend rolls up hourly and projected spend and flags unbounded nodes.
func TestEstimateSpend(t *testing.T) {
	o, _ := newOrch(t)
	specs := []NodeSpec{{Name: "a"}, {Name: "b"}}
	est, err := o.EstimateSpend(context.Background(), specs, []time.Duration{2 * time.Hour, 3 * time.Hour})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.Currency != "EUR" || est.Hourly != 0.02 {
		t.Fatalf("want 0.02 EUR/hr, got %.4f %s", est.Hourly, est.Currency)
	}
	// projected = 0.01×2 + 0.01×3 = 0.05
	if est.Projected < 0.0499 || est.Projected > 0.0501 {
		t.Fatalf("want projected ~0.05, got %.4f", est.Projected)
	}
	if est.Unbounded {
		t.Fatal("should not be unbounded with two TTLs set")
	}
}
