// SPDX-License-Identifier: AGPL-3.0-or-later

package mock

import (
	"context"
	"testing"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// The mock implements the optional GPUProvider seam so the GPU catalog,
// resolution, and pricing are exercised offline (G0).
func TestMockIsGPUProvider(t *testing.T) {
	var _ provider.GPUProvider = New()
}

func TestGPUOfferings(t *testing.T) {
	offs, err := New().GPUOfferings(context.Background())
	if err != nil {
		t.Fatalf("offerings: %v", err)
	}
	if len(offs) == 0 {
		t.Fatal("expected a non-empty GPU catalog")
	}
	for _, o := range offs {
		if o.ServerType == "" || o.GPU.Model == "" || !o.Hourly.Known() {
			t.Fatalf("incomplete offering: %+v", o)
		}
	}
}

func TestResolveGPUType(t *testing.T) {
	m := New()
	ctx := context.Background()

	// exact model resolves to its SKU
	typ, region, err := m.ResolveGPUType(ctx, provider.GPUReq{Model: "h100", Count: 1}, nil)
	if err != nil {
		t.Fatalf("resolve h100: %v", err)
	}
	if typ != "mock-gpu-h100" || region == "" {
		t.Fatalf("resolve h100 = %q / %q", typ, region)
	}

	// any-model (cheapest-first) picks the a100 SKU
	typ, _, err = m.ResolveGPUType(ctx, provider.GPUReq{Count: 1}, nil)
	if err != nil {
		t.Fatalf("resolve any: %v", err)
	}
	if typ != "mock-gpu-a100" {
		t.Fatalf("cheapest-first want mock-gpu-a100, got %q", typ)
	}

	// unsatisfiable request errors
	if _, _, err := m.ResolveGPUType(ctx, provider.GPUReq{Model: "b200", Count: 1}, nil); err == nil {
		t.Fatal("unknown model must error")
	}

	// minVRAM filters out the smaller card
	typ, _, err = m.ResolveGPUType(ctx, provider.GPUReq{MinVRAM: 80, Count: 1}, nil)
	if err != nil || typ != "mock-gpu-h100" {
		t.Fatalf("minVRAM=80 want mock-gpu-h100, got %q (err %v)", typ, err)
	}
}

func TestEstimateHourlyGPU(t *testing.T) {
	m := New()
	ctx := context.Background()

	// CPU node keeps the flat price
	cpu, _ := m.EstimateHourly(ctx, provider.ServerSpec{Name: "n"})
	if cpu.Amount != 0.01 {
		t.Fatalf("cpu price = %.4f, want 0.01", cpu.Amount)
	}

	// GPU node is priced from the catalog
	one, err := m.EstimateHourly(ctx, provider.ServerSpec{Name: "g", GPU: provider.GPUReq{Model: "a100", Count: 1}})
	if err != nil || one.Amount != 1.10 {
		t.Fatalf("a100 price = %.4f (err %v), want 1.10", one.Amount, err)
	}

	// multi-GPU scales by count
	two, _ := m.EstimateHourly(ctx, provider.ServerSpec{Name: "g", GPU: provider.GPUReq{Model: "a100", Count: 2}})
	if two.Amount != 2.20 {
		t.Fatalf("a100×2 price = %.4f, want 2.20", two.Amount)
	}

	// an unpriceable GPU request errors (so --max-cost fails closed, never skipped)
	if _, err := m.EstimateHourly(ctx, provider.ServerSpec{GPU: provider.GPUReq{Model: "b200", Count: 1}}); err == nil {
		t.Fatal("unpriceable GPU must error")
	}
}

func TestCreateServerGPU(t *testing.T) {
	m := New()
	ctx := context.Background()

	// CPU node: no GPU on the result
	cpu, _ := m.CreateServer(ctx, provider.ServerSpec{Name: "c"})
	if cpu.GPU.Present() {
		t.Fatalf("cpu node should have no GPU: %+v", cpu.GPU)
	}

	// GPU node: realized GPU + SKU type
	g, err := m.CreateServer(ctx, provider.ServerSpec{Name: "g", GPU: provider.GPUReq{Model: "h100", Count: 2}})
	if err != nil {
		t.Fatalf("create gpu: %v", err)
	}
	if g.Type != "mock-gpu-h100" || g.GPU.Model != "h100" || g.GPU.Count != 2 || g.GPU.VRAM != 80 {
		t.Fatalf("realized GPU = type %q, %+v", g.Type, g.GPU)
	}
}
