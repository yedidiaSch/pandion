package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/provider/mock"
	"github.com/yedidiaSch/pandion/internal/state"
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

func specs(names ...string) []NodeSpec {
	out := make([]NodeSpec, len(names))
	for i, n := range names {
		out[i] = NodeSpec{Name: n, UserData: "#cloud-config\n"}
	}
	return out
}

// M3.2: UpCluster provisions all nodes and the barrier holds (all RUNNING on return).
func TestUpCluster_AllNodesRunning_BarrierHolds(t *testing.T) {
	o, m := newOrch(t)
	c, err := o.UpCluster(context.Background(), "cl", specs("a", "b", "c"), 5)
	if err != nil {
		t.Fatalf("upcluster: %v", err)
	}
	if m.Count() != 3 {
		t.Fatalf("want 3 servers, got %d", m.Count())
	}
	for _, n := range c.Nodes {
		if n.Phase != state.Running {
			t.Fatalf("barrier violated: node %s in phase %s (want RUNNING)", n.Name, n.Phase)
		}
		if n.IP == "" || n.ServerID == "" {
			t.Fatalf("node %s missing IP/ServerID after provisioning", n.Name)
		}
	}
}

// M6: concurrency is bounded by maxConc.
func TestUpCluster_BoundedConcurrency(t *testing.T) {
	o, m := newOrch(t)
	if _, err := o.UpCluster(context.Background(), "cl", specs("a", "b", "c", "d", "e", "f"), 2); err != nil {
		t.Fatalf("upcluster: %v", err)
	}
	if m.MaxConcurrent > 2 {
		t.Fatalf("concurrency exceeded bound: peak=%d, max=2", m.MaxConcurrent)
	}
	if m.MaxConcurrent < 2 {
		t.Logf("note: peak concurrency was %d (<2); scheduling-dependent", m.MaxConcurrent)
	}
}

// M10: a node failure fails the whole UpCluster; the caller can then roll back.
func TestUpCluster_PartialFailure_ThenRollback(t *testing.T) {
	o, m := newOrch(t)
	m.FailCreateFor = map[string]bool{"b": true}
	ctx := context.Background()

	_, err := o.UpCluster(ctx, "cl", specs("a", "b", "c"), 5)
	if err == nil {
		t.Fatal("expected UpCluster to fail when a node fails")
	}
	// caller rolls back the partial cluster
	if derr := o.Down(ctx, "cl"); derr != nil {
		t.Fatalf("rollback down: %v", derr)
	}
	if m.Count() != 0 {
		t.Fatalf("rollback left %d servers", m.Count())
	}
}

func TestReap_DestroysAllTaggedClusters(t *testing.T) {
	o, m := newOrch(t)
	ctx := context.Background()
	// two separate clusters provisioned
	if _, err := o.UpCluster(ctx, "cl-a", specs("a1", "a2"), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Up(ctx, "cl-b", "b1", "", ""); err != nil {
		t.Fatal(err)
	}
	if m.Count() != 3 {
		t.Fatalf("want 3 servers, got %d", m.Count())
	}
	plan, err := o.ReapPlan(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("want 2 reap candidates, got %d", len(plan))
	}
	n, err := o.Reap(ctx, plan)
	if err != nil || n != 2 {
		t.Fatalf("reap n=%d err=%v", n, err)
	}
	if m.Count() != 0 {
		t.Fatalf("reap left %d servers", m.Count())
	}
}

func TestReapPlan_OlderThanFiltersOutYoungClusters(t *testing.T) {
	o, _ := newOrch(t)
	ctx := context.Background()
	if _, err := o.Up(ctx, "fresh", "n", "", ""); err != nil {
		t.Fatal(err)
	}
	// just-created cluster is younger than 1h -> excluded
	plan, err := o.ReapPlan(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Fatalf("young cluster should be filtered out, got %d", len(plan))
	}
}
