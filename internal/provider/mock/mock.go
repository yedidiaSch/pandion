// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mock is an in-memory Provider used for fast, free, offline tests.
// Per the roadmap it is built in M0 and KEPT FOREVER as the CI backbone — the
// orchestrator's logic is validated here without ever touching a cloud or spending money.
package mock

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// Mock is a thread-safe, in-memory provider with simple failure injection.
type Mock struct {
	mu      sync.Mutex
	seq     int
	servers map[string]provider.Server

	// FailDestroyOnce makes the next DestroyServer call fail exactly once,
	// to exercise the orchestrator's retry path (risk H7).
	FailDestroyOnce bool

	// ReapAuxCalls counts ReapAux invocations (test observability).
	ReapAuxCalls int

	// FailCreateFor makes CreateServer fail for these node names, to exercise
	// partial-cluster-failure handling (M10).
	FailCreateFor map[string]bool
	// MaxConcurrent records the peak simultaneous CreateServer calls, to assert
	// bounded concurrency (M6).
	MaxConcurrent int
	curConcurrent int
}

// ReapAux implements provider.AuxReaper so the orchestrator's cleanup branch is
// exercised offline.
func (m *Mock) ReapAux(_ context.Context, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReapAuxCalls++
	return nil
}

// mockGPUOfferings is a fixed, offline GPU catalog so the GPU seam (`list-gpus`,
// `--gpu` resolution, GPU pricing) is exercised in CI without any cloud (G0).
// Each SKU is a single GPU; multi-GPU requests scale price by GPUReq.Count.
var mockGPUOfferings = []provider.GPUOffering{
	{ServerType: "mock-gpu-a100", GPU: provider.GPUInfo{Model: "a100", Count: 1, VRAM: 40},
		Regions: []string{"mock-dc"}, Hourly: provider.Money{Amount: 1.10, Currency: "EUR"}, Image: "mock-cuda"},
	{ServerType: "mock-gpu-h100", GPU: provider.GPUInfo{Model: "h100", Count: 1, VRAM: 80},
		Regions: []string{"mock-dc"}, Hourly: provider.Money{Amount: 2.50, Currency: "EUR"}, Image: "mock-cuda"},
}

// GPUOfferings implements provider.GPUProvider: the fixed offline catalog.
func (m *Mock) GPUOfferings(_ context.Context) ([]provider.GPUOffering, error) {
	out := make([]provider.GPUOffering, len(mockGPUOfferings))
	copy(out, mockGPUOfferings)
	return out, nil
}

// ResolveGPUType implements provider.GPUProvider: cheapest offering matching the
// requested model (if any) and minimum VRAM. Deterministic so dry-run and up agree.
func (m *Mock) ResolveGPUType(_ context.Context, req provider.GPUReq, _ []string) (string, string, error) {
	off, err := matchGPUOffering(req)
	if err != nil {
		return "", "", err
	}
	return off.ServerType, off.Regions[0], nil
}

// matchGPUOffering returns the cheapest offering satisfying req (mockGPUOfferings
// is already cheapest-first). Shared by ResolveGPUType, pricing, and CreateServer.
func matchGPUOffering(req provider.GPUReq) (provider.GPUOffering, error) {
	for _, o := range mockGPUOfferings {
		if req.Model != "" && !strings.EqualFold(req.Model, o.GPU.Model) {
			continue
		}
		if req.MinVRAM > 0 && o.GPU.VRAM < req.MinVRAM {
			continue
		}
		return o, nil
	}
	return provider.GPUOffering{}, fmt.Errorf("mock: no GPU offering matches model=%q minVRAM=%d", req.Model, req.MinVRAM)
}

// HourlyPrice implements provider.Pricer with a fixed, nonzero fake price so the
// `ls`/`status` cost path and the `--max-cost` preflight are exercised offline.
func (m *Mock) HourlyPrice(_ context.Context, serverType, _ string) (provider.Money, error) {
	if serverType == "" {
		return provider.Money{}, nil
	}
	for _, o := range mockGPUOfferings {
		if o.ServerType == serverType {
			return o.Hourly, nil
		}
	}
	return provider.Money{Amount: 0.01, Currency: "EUR"}, nil
}

// EstimateHourly implements provider.Pricer: the mock always resolves to a priced
// type, so the `--max-cost` preflight is exercised offline. A --gpu request is
// priced from the GPU catalog (× Count); otherwise the flat CPU price.
func (m *Mock) EstimateHourly(_ context.Context, spec provider.ServerSpec) (provider.Money, error) {
	if spec.GPU.Wanted() {
		off, err := matchGPUOffering(spec.GPU)
		if err != nil {
			return provider.Money{}, err
		}
		n := spec.GPU.Count
		if n < 1 {
			n = 1
		}
		return provider.Money{Amount: off.Hourly.Amount * float64(n), Currency: off.Hourly.Currency}, nil
	}
	return provider.Money{Amount: 0.01, Currency: "EUR"}, nil
}

// New returns an empty mock provider.
func New() *Mock { return &Mock{servers: map[string]provider.Server{}} }

// Name implements provider.Provider.
func (m *Mock) Name() string { return "mock" }

// CreateServer records a new server and returns it.
func (m *Mock) CreateServer(_ context.Context, spec provider.ServerSpec) (provider.Server, error) {
	// enter: track concurrency peak (for M6 bounded-concurrency assertions)
	m.mu.Lock()
	m.curConcurrent++
	if m.curConcurrent > m.MaxConcurrent {
		m.MaxConcurrent = m.curConcurrent
	}
	fail := m.FailCreateFor[spec.Name]
	m.mu.Unlock()

	// stay "in flight" briefly so overlapping callers are actually observed
	time.Sleep(3 * time.Millisecond)

	m.mu.Lock()
	defer func() { m.curConcurrent--; m.mu.Unlock() }()
	if fail {
		return provider.Server{}, fmt.Errorf("mock: simulated create failure for %q", spec.Name)
	}
	m.seq++
	typ, gpu := "mock-small", provider.GPUInfo{}
	if spec.GPU.Wanted() {
		off, err := matchGPUOffering(spec.GPU)
		if err != nil {
			return provider.Server{}, err
		}
		typ = off.ServerType
		gpu = off.GPU
		if spec.GPU.Count > 1 {
			gpu.Count = spec.GPU.Count
		}
	}
	s := provider.Server{
		ID:        fmt.Sprintf("mock-%d", m.seq),
		Name:      spec.Name,
		ClusterID: spec.ClusterID,
		Type:      typ,
		Region:    "mock-dc",
		IP:        fmt.Sprintf("10.0.0.%d", m.seq),
		Created:   time.Now(),
		GPU:       gpu,
	}
	m.servers[s.ID] = s
	return s, nil
}

// DestroyServer removes a server. Idempotent: deleting an absent id is success.
func (m *Mock) DestroyServer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailDestroyOnce {
		m.FailDestroyOnce = false
		return fmt.Errorf("mock: simulated transient destroy failure for %s", id)
	}
	delete(m.servers, id)
	return nil
}

// ListByTag returns all servers for a cluster (the reconcile source of truth).
func (m *Mock) ListByTag(_ context.Context, clusterID string) ([]provider.Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]provider.Server, 0, len(m.servers))
	for _, s := range m.servers {
		if s.ClusterID == clusterID {
			out = append(out, s)
		}
	}
	return out, nil
}

// ListAllTagged returns every server (the reaper source of truth).
func (m *Mock) ListAllTagged(_ context.Context) ([]provider.Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]provider.Server, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, s)
	}
	return out, nil
}

// Count is a test helper: number of live servers.
func (m *Mock) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.servers)
}
