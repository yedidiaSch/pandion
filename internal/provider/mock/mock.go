// Package mock is an in-memory Provider used for fast, free, offline tests.
// Per the roadmap it is built in M0 and KEPT FOREVER as the CI backbone — the
// orchestrator's logic is validated here without ever touching a cloud or spending money.
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/envcore/envcore/internal/provider"
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
	s := provider.Server{
		ID:        fmt.Sprintf("mock-%d", m.seq),
		Name:      spec.Name,
		ClusterID: spec.ClusterID,
		Type:      "mock-small",
		Region:    "mock-dc",
		IP:        fmt.Sprintf("10.0.0.%d", m.seq),
		Created:   time.Now(),
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
