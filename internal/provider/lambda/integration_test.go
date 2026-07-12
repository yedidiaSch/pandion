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

// Opt-in ($0, no instance): calls EnsureClusterFirewall against the real Lambda
// account firewall and verifies udp/51820 is present afterward. This also OPENS
// the WireGuard port for real (account-wide, idempotent) so the overlay works.
func TestLambda_Integration_EnsureFirewall(t *testing.T) {
	if os.Getenv("PANDION_IT") != "1" {
		t.Skip("set PANDION_IT=1 and LAMBDA_API_KEY to run the firewall integration test")
	}
	key := os.Getenv("LAMBDA_API_KEY")
	if key == "" {
		t.Skip("LAMBDA_API_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l := New(key)
	if err := l.EnsureClusterFirewall(ctx, "it", 51820); err != nil {
		t.Fatalf("ensure firewall: %v", err)
	}
	var cur struct {
		Data []fwRule `json:"data"`
	}
	if err := l.do(ctx, "GET", "/firewall-rules", nil, &cur); err != nil {
		t.Fatalf("re-list: %v", err)
	}
	ok := false
	for _, r := range cur.Data {
		if r.Protocol == "udp" && len(r.PortRange) == 2 && r.PortRange[0] <= 51820 && 51820 <= r.PortRange[1] {
			ok = true
		}
		t.Logf("rule: %s %v %s", r.Protocol, r.PortRange, r.Description)
	}
	if !ok {
		t.Fatal("udp/51820 not present after EnsureClusterFirewall")
	}
}
