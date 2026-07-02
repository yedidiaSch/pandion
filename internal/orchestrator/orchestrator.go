// Package orchestrator is the provider-agnostic state machine + reconcile loop.
// M0 scope: single-node Up, and a Down that reconciles to empty using the
// provider as the source of truth (so it works even if local state is lost).
package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/yedidiaSch/pandion/internal/provider"
	"github.com/yedidiaSch/pandion/internal/state"
)

// DefaultMaxConcurrency bounds concurrent provisioning to respect provider rate
// limits (risk M6).
const DefaultMaxConcurrency = 5

// NodeSpec describes one node to provision in a cluster.
type NodeSpec struct {
	Name        string
	UserData    string
	LoginPubKey string
	Type        string // exact provider type (from cluster.yaml `size`); empty = auto
	Image       string
	RegionPref  []string
}

// Orchestrator drives clusters through their lifecycle.
type Orchestrator struct {
	P provider.Provider
	S *state.Store
}

// New wires a provider and a state store.
func New(p provider.Provider, s *state.Store) *Orchestrator {
	return &Orchestrator{P: p, S: s}
}

// Up provisions a single-node cluster, journaling each transition before/after.
// loginPubKey (optional) is registered with the provider for root SSH access.
func (o *Orchestrator) Up(ctx context.Context, clusterID, nodeName, userData, loginPubKey string) (*state.Cluster, error) {
	c := &state.Cluster{
		ID:       clusterID,
		Provider: o.P.Name(),
		Nodes:    []state.Node{{Name: nodeName, Phase: state.Planned}},
	}
	if err := o.S.Save(c); err != nil { // journal: PLANNED
		return nil, err
	}

	c.Nodes[0].Phase = state.Provisioning
	if err := o.S.Save(c); err != nil { // journal BEFORE the create (crash-resumable)
		return nil, err
	}

	srv, err := o.P.CreateServer(ctx, provider.ServerSpec{
		Name:        nodeName,
		ClusterID:   clusterID,
		UserData:    userData,
		LoginPubKey: loginPubKey,
	})
	if err != nil {
		c.Nodes[0].Phase = state.Failed
		_ = o.S.Save(c)
		return c, fmt.Errorf("provision %q: %w", nodeName, err)
	}

	c.Nodes[0].ServerID = srv.ID
	c.Nodes[0].IP = srv.IP
	c.Nodes[0].Phase = state.Running
	if err := o.S.Save(c); err != nil { // journal: RUNNING
		return nil, err
	}
	return c, nil
}

// UpCluster provisions a multi-node cluster concurrently (bounded by maxConc; 0 =
// default), journaling each node's transitions. It implements the provisioning
// BARRIER (C5): it returns only after ALL nodes reach RUNNING — later slices form
// the WG mesh and inject discovery only after this returns.
//
// Fail-fast (M10): if any node fails, the group context is cancelled (aborting
// in-flight creates), the partial cluster is returned with the error, and the
// caller decides rollback (default) vs --keep-on-failure.
func (o *Orchestrator) UpCluster(ctx context.Context, clusterID string, specs []NodeSpec, maxConc int) (*state.Cluster, error) {
	if maxConc <= 0 {
		maxConc = DefaultMaxConcurrency
	}
	c := &state.Cluster{ID: clusterID, Provider: o.P.Name(), Nodes: make([]state.Node, len(specs))}
	for i, s := range specs {
		c.Nodes[i] = state.Node{Name: s.Name, Phase: state.Planned}
	}

	var mu sync.Mutex
	save := func() { mu.Lock(); defer mu.Unlock(); _ = o.S.Save(c) }
	setPhase := func(i int, p state.Phase) { mu.Lock(); c.Nodes[i].Phase = p; mu.Unlock() }
	save() // journal: all PLANNED

	sem := semaphore.NewWeighted(int64(maxConc))
	g, gctx := errgroup.WithContext(ctx)
	for i := range specs {
		i := i
		g.Go(func() error {
			if err := sem.Acquire(gctx, 1); err != nil {
				return err
			}
			defer sem.Release(1)

			setPhase(i, state.Provisioning)
			save()
			srv, err := o.P.CreateServer(gctx, provider.ServerSpec{
				Name:        specs[i].Name,
				ClusterID:   clusterID,
				UserData:    specs[i].UserData,
				LoginPubKey: specs[i].LoginPubKey,
				Type:        specs[i].Type,
				Image:       specs[i].Image,
				RegionPref:  specs[i].RegionPref,
			})
			if err != nil {
				setPhase(i, state.Failed)
				save()
				return fmt.Errorf("provision %q: %w", specs[i].Name, err)
			}
			mu.Lock()
			c.Nodes[i].ServerID = srv.ID
			c.Nodes[i].IP = srv.IP
			c.Nodes[i].Phase = state.Running
			mu.Unlock()
			save()
			return nil
		})
	}

	// BARRIER: block until every node is RUNNING (or one fails).
	if err := g.Wait(); err != nil {
		return c, err
	}
	return c, nil
}

// Down reconciles the cluster to empty. It lists by tag from the PROVIDER (the
// ReapCandidate is a cluster the reaper would destroy.
type ReapCandidate struct {
	ClusterID string
	Servers   int
	OldestAge time.Duration
}

// ReapPlan lists the Pandion clusters currently alive at the provider whose
// oldest server is at least olderThan (0 = all). It queries the provider
// directly, so it works across machines and with no local state (C4).
func (o *Orchestrator) ReapPlan(ctx context.Context, olderThan time.Duration) ([]ReapCandidate, error) {
	all, err := o.P.ListAllTagged(ctx)
	if err != nil {
		return nil, err
	}
	type agg struct {
		n      int
		oldest time.Time
	}
	byCluster := map[string]*agg{}
	now := time.Now()
	for _, s := range all {
		a := byCluster[s.ClusterID]
		if a == nil {
			a = &agg{oldest: s.Created}
			byCluster[s.ClusterID] = a
		}
		a.n++
		if !s.Created.IsZero() && s.Created.Before(a.oldest) {
			a.oldest = s.Created
		}
	}
	var out []ReapCandidate
	for id, a := range byCluster {
		age := now.Sub(a.oldest)
		if olderThan > 0 && age < olderThan {
			continue
		}
		out = append(out, ReapCandidate{ClusterID: id, Servers: a.n, OldestAge: age})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClusterID < out[j].ClusterID })
	return out, nil
}

// Reap destroys the given clusters (reusing Down: verified teardown + aux reap +
// state close). Returns the number successfully reaped.
func (o *Orchestrator) Reap(ctx context.Context, candidates []ReapCandidate) (int, error) {
	n := 0
	var firstErr error
	for _, c := range candidates {
		if err := o.Down(ctx, c.ClusterID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// source of truth, C4) so it succeeds even with no local state, destroys with
// retry (H7), then VERIFIES nothing remains before closing the state record.
func (o *Orchestrator) Down(ctx context.Context, clusterID string) error {
	servers, err := o.P.ListByTag(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("list %q: %w", clusterID, err)
	}

	// best-effort journal of the teardown intent (local state may not exist)
	if c, lerr := o.S.Load(clusterID); lerr == nil {
		for i := range c.Nodes {
			c.Nodes[i].Phase = state.TearingDown
		}
		_ = o.S.Save(c)
	}

	var firstErr error
	for _, s := range servers {
		if derr := destroyWithRetry(ctx, o.P, s.ID, 3); derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	if firstErr != nil {
		return fmt.Errorf("teardown %q: %w", clusterID, firstErr)
	}

	// verify the provider truly has nothing left for this cluster
	left, err := o.P.ListByTag(ctx, clusterID)
	if err != nil {
		return err
	}
	if len(left) != 0 {
		return fmt.Errorf("teardown incomplete: %d server(s) remain for %q", len(left), clusterID)
	}

	// clean up cluster-scoped auxiliary resources (e.g. registered SSH keys) so
	// nothing leaks, if the provider supports it (C4).
	if reaper, ok := o.P.(provider.AuxReaper); ok {
		if rerr := reaper.ReapAux(ctx, clusterID); rerr != nil {
			return fmt.Errorf("reap aux resources for %q: %w", clusterID, rerr)
		}
	}
	return o.S.Close(clusterID)
}

func destroyWithRetry(ctx context.Context, p provider.Provider, id string, attempts int) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = p.DestroyServer(ctx, id); err == nil {
			return nil
		}
	}
	return err
}
