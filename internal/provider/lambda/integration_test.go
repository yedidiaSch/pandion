// SPDX-License-Identifier: AGPL-3.0-or-later

package lambda

import (
	"context"
	"os"
	"testing"
	"time"
)

// Opt-in, READ-ONLY real-API smoke test. Requires BOTH PANDION_IT=1 and
// LAMBDA_API_KEY so it never runs by accident or in CI. It only queries the GPU
// catalog (GET /instance-types) — it launches NOTHING, so it costs $0. It exists
// to calibrate the assumed JSON shapes against the live API before G1 e2e; a full
// launch/lockdown/reap loop is a manual, credentialed step (docs/gpu-design.md G1).
func TestLambda_Integration_GPUOfferings(t *testing.T) {
	if os.Getenv("PANDION_IT") != "1" {
		t.Skip("set PANDION_IT=1 and LAMBDA_API_KEY to run the read-only Lambda smoke test")
	}
	key := os.Getenv("LAMBDA_API_KEY")
	if key == "" {
		t.Skip("LAMBDA_API_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	offs, err := New(key).GPUOfferings(ctx)
	if err != nil {
		t.Fatalf("live GPUOfferings: %v", err)
	}
	if len(offs) == 0 {
		t.Fatal("live catalog is empty — the /instance-types shape may have drifted")
	}
	for _, o := range offs {
		if o.ServerType == "" || o.GPU.Model == "" || !o.Hourly.Known() {
			t.Errorf("live offering incompletely parsed (shape drift?): %+v", o)
		}
		t.Logf("%-22s %s×%d %dGB  $%.2f/hr  regions=%v",
			o.ServerType, o.GPU.Model, o.GPU.Count, o.GPU.VRAM, o.Hourly.Amount, o.Regions)
	}
}
