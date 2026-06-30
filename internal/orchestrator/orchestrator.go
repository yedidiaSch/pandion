// Package orchestrator is the provider-agnostic state machine + reconcile loop.
// M0 scope: single-node Up, and a Down that reconciles to empty using the
// provider as the source of truth (so it works even if local state is lost).
package orchestrator

import (
	"context"
	"fmt"

	"github.com/envcore/envcore/internal/provider"
	"github.com/envcore/envcore/internal/state"
)

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

// Down reconciles the cluster to empty. It lists by tag from the PROVIDER (the
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
