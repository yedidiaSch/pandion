package orchestrator

import (
	"context"
	"testing"

	"github.com/envcore/envcore/internal/provider/mock"
	"github.com/envcore/envcore/internal/state"
)

func newOrch(t *testing.T) (*Orchestrator, *mock.Mock) {
	t.Helper()
	st, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	m := mock.New()
	return New(m, st), m
}

// M0 spine: up provisions exactly one server and reaches RUNNING; down leaves zero.
func TestM0_UpThenDown_NoLeaks(t *testing.T) {
	o, m := newOrch(t)
	ctx := context.Background()

	c, err := o.Up(ctx, "test-cluster", "node-a", "#cloud-config\n", "")
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("want 1 server after up, got %d", m.Count())
	}
	if c.Nodes[0].Phase != state.Running {
		t.Fatalf("want phase RUNNING, got %s", c.Nodes[0].Phase)
	}

	if err := o.Down(ctx, "test-cluster"); err != nil {
		t.Fatalf("down: %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("want 0 servers after down, got %d", m.Count())
	}
	// AuxReaper must be invoked during teardown so registered SSH keys don't leak.
	if m.ReapAuxCalls != 1 {
		t.Fatalf("want ReapAux called once, got %d", m.ReapAuxCalls)
	}
}

// C4: down must reconcile from provider truth even when local state is gone.
func TestM0_Down_RecoversWithoutLocalState(t *testing.T) {
	o, m := newOrch(t)
	ctx := context.Background()

	if _, err := o.Up(ctx, "c2", "n", "", ""); err != nil {
		t.Fatalf("up: %v", err)
	}
	// simulate a lost/deleted local state file (e.g. CLI crashed, laptop wiped)
	if err := o.S.Close("c2"); err != nil {
		t.Fatalf("close state: %v", err)
	}
	if err := o.Down(ctx, "c2"); err != nil {
		t.Fatalf("down without local state: %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("want 0 servers, got %d", m.Count())
	}
}

// H7: down survives a transient destroy failure (retry) and is idempotent.
func TestM0_Down_Idempotent_And_RetriesTransientFailure(t *testing.T) {
	o, m := newOrch(t)
	ctx := context.Background()

	if _, err := o.Up(ctx, "c3", "n", "", ""); err != nil {
		t.Fatalf("up: %v", err)
	}
	m.FailDestroyOnce = true // first destroy attempt fails; retry should succeed

	if err := o.Down(ctx, "c3"); err != nil {
		t.Fatalf("down with transient failure: %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("want 0 servers, got %d", m.Count())
	}
	// re-running down is a clean no-op
	if err := o.Down(ctx, "c3"); err != nil {
		t.Fatalf("second down (idempotent): %v", err)
	}
}
