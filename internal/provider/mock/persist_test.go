// SPDX-License-Identifier: AGPL-3.0-or-later

package mock

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// TestPersistentMock_SurvivesAcrossInstances covers R10/F9: a persistent mock
// backed by a file is visible to a *separate* Mock instance (i.e. a separate
// `pandion` process), so re-up collision / teardown can be tested offline.
func TestPersistentMock_SurvivesAcrossInstances(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mock-state.json")

	// process 1: create a server for cluster "demo".
	m1 := NewPersistent(path)
	if _, err := m1.CreateServer(ctx, provider.ServerSpec{Name: "node-a", ClusterID: "demo"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// process 2: a fresh instance loads the same file and sees it.
	m2 := NewPersistent(path)
	got, err := m2.ListByTag(ctx, "demo")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 server visible to a fresh instance, got %d", len(got))
	}

	// destroy through m2, then a third instance sees it gone.
	if err := m2.DestroyServer(ctx, got[0].ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	m3 := NewPersistent(path)
	if got, _ := m3.ListByTag(ctx, "demo"); len(got) != 0 {
		t.Fatalf("want 0 servers after destroy, got %d", len(got))
	}
}

// TestInMemoryMock_IsNotPersisted guards the default: New() never writes state,
// so two in-memory instances are independent (repeated CLI `up --provider=mock`
// stays free and collision-free).
func TestInMemoryMock_IsNotPersisted(t *testing.T) {
	ctx := context.Background()
	m1 := New()
	if _, err := m1.CreateServer(ctx, provider.ServerSpec{Name: "n", ClusterID: "demo"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := New().ListByTag(ctx, "demo"); len(got) != 0 {
		t.Fatalf("in-memory mock must not share state, a fresh instance saw %d", len(got))
	}
}
