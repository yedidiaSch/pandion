// SPDX-License-Identifier: AGPL-3.0-or-later

// Package orchestrator is the provider-agnostic state machine + reconcile loop.
// M0 scope: single-node Up, and a Down that reconciles to empty using the
// provider as the source of truth (so it works even if local state is lost).
package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
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
	GPU         provider.GPUReq // optional; zero = CPU-only
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
	return o.UpSpec(ctx, clusterID, NodeSpec{Name: nodeName}, userData, loginPubKey)
}

// maxProvisionAttempts bounds how many times createWithRetry will (re)launch a
// node whose instance dies DURING BOOT (a transient cloud failure — e.g. Lambda
// terminating a VM mid-boot). Each attempt launches a fresh instance.
const maxProvisionAttempts = 3

// provisionRetryDelay is the pause between provisioning attempts. A var so tests
// can drop it to zero.
var provisionRetryDelay = 5 * time.Second

// provisionRetryLog receives the "relaunching" notices; stderr in production so a
// waiting operator sees the recovery, overridable in tests.
var provisionRetryLog io.Writer = os.Stderr

// createWithRetry launches a server, retrying TRANSIENT boot failures with a
// fresh instance up to maxProvisionAttempts. Non-transient errors (bad spec,
// quota, auth) fail immediately — no point relaunching those.
func (o *Orchestrator) createWithRetry(ctx context.Context, spec provider.ServerSpec) (provider.Server, error) {
	var lastErr error
	for attempt := 1; attempt <= maxProvisionAttempts; attempt++ {
		srv, err := o.P.CreateServer(ctx, spec)
		if err == nil {
			return srv, nil
		}
		if !provider.IsTransientProvision(err) {
			return provider.Server{}, err
		}
		lastErr = err
		if attempt < maxProvisionAttempts {
			fmt.Fprintf(provisionRetryLog, "provision %q: %v — relaunching a fresh instance (attempt %d/%d)\n",
				spec.Name, err, attempt+1, maxProvisionAttempts)
			select {
			case <-ctx.Done():
				return provider.Server{}, ctx.Err()
			case <-time.After(provisionRetryDelay):
			}
		}
	}
	return provider.Server{}, lastErr
}

// UpSpec is Up with an explicit node spec, so callers can pin the size (Type) and
// region (RegionPref) — e.g. from `pandion init` defaults. Empty fields keep the
// provider's auto-selection.
func (o *Orchestrator) UpSpec(ctx context.Context, clusterID string, spec NodeSpec, userData, loginPubKey string) (*state.Cluster, error) {
	nodeName := spec.Name
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

	srv, err := o.createWithRetry(ctx, provider.ServerSpec{
		Name:        nodeName,
		ClusterID:   clusterID,
		Type:        spec.Type,
		RegionPref:  spec.RegionPref,
		UserData:    userData,
		LoginPubKey: loginPubKey,
		GPU:         spec.GPU,
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
	var saveWarned bool
	save := func() {
		mu.Lock()
		defer mu.Unlock()
		if err := o.S.Save(c); err != nil && !saveWarned {
			// A broken state dir would otherwise silently produce no journal for
			// the whole provision. Warn once so an operator can fix it; the
			// provision itself still proceeds (the provider is the source of truth).
			fmt.Fprintf(provisionRetryLog, "warning: could not journal cluster state: %v (crash-resume will be unavailable)\n", err)
			saveWarned = true
		}
	}
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
			srv, err := o.createWithRetry(gctx, provider.ServerSpec{
				Name:        specs[i].Name,
				ClusterID:   clusterID,
				UserData:    specs[i].UserData,
				LoginPubKey: specs[i].LoginPubKey,
				Type:        specs[i].Type,
				Image:       specs[i].Image,
				RegionPref:  specs[i].RegionPref,
				GPU:         specs[i].GPU,
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

// NodeStatus is the live view of one running node for `ls`/`status`.
type NodeStatus struct {
	Name, Type, Region, IP string
	Age                    time.Duration
	Hourly                 provider.Money   // zero = price unknown
	GPU                    provider.GPUInfo // zero = CPU node
	GPUUtil                int              // live GPU utilization %; -1 = not measured/unknown
}

// ClusterStatus aggregates a cluster's running nodes and its rolled-up cost.
type ClusterStatus struct {
	ClusterID string
	Nodes     []NodeStatus
	Hourly    float64       // sum of per-node hourly (in Currency); 0 if unpriced
	Accrued   float64       // Σ hourly × age — an ESTIMATE of spend so far
	Oldest    time.Duration // age of the oldest node
}

// Status lists every Pandion cluster alive at the provider, grouped, with uptime
// and (if the provider implements provider.Pricer) live cost. Queries the
// provider directly, so it works with no local state and across machines (C4).
// Returns the clusters and the billing currency ("" if unpriced/unknown).
func (o *Orchestrator) Status(ctx context.Context) ([]ClusterStatus, string, error) {
	all, err := o.P.ListAllTagged(ctx)
	if err != nil {
		return nil, "", err
	}
	pricer, _ := o.P.(provider.Pricer)
	now := time.Now()
	byCluster := map[string]*ClusterStatus{}
	var currency string
	for _, s := range all {
		cs := byCluster[s.ClusterID]
		if cs == nil {
			cs = &ClusterStatus{ClusterID: s.ClusterID}
			byCluster[s.ClusterID] = cs
		}
		age := now.Sub(s.Created)
		if s.Created.IsZero() {
			age = 0
		}
		n := NodeStatus{Name: s.Name, Type: s.Type, Region: s.Region, IP: s.IP, Age: age, GPU: s.GPU, GPUUtil: -1}
		if pricer != nil {
			// a per-node pricing error is non-fatal: leave that node unpriced.
			if m, perr := pricer.HourlyPrice(ctx, s.Type, s.Region); perr == nil && m.Known() {
				n.Hourly = m
				currency = m.Currency
				cs.Hourly += m.Amount
				cs.Accrued += m.Amount * age.Hours()
			}
		}
		if age > cs.Oldest {
			cs.Oldest = age
		}
		cs.Nodes = append(cs.Nodes, n)
	}
	out := make([]ClusterStatus, 0, len(byCluster))
	for _, cs := range byCluster {
		sort.Slice(cs.Nodes, func(i, j int) bool { return cs.Nodes[i].Name < cs.Nodes[j].Name })
		out = append(out, *cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClusterID < out[j].ClusterID })
	return out, currency, nil
}

// CostEstimate is the `--max-cost` preflight projection.
type CostEstimate struct {
	Hourly    float64 // Σ per-node gross hourly (in Currency)
	Projected float64 // Σ per-node hourly × its idle-TTL window. NOTE: the idle TTL
	// powers a node OFF, it does not destroy it — billing continues after power-off
	// until `down`/`reap`. So this is spend over the active window, not a cap on the
	// total a forgotten cluster can accrue (F2).
	Currency  string
	Unbounded bool // a node has no TTL ⇒ projected spend is infinite
}

// EstimateSpend prices each spec (without provisioning) and projects total spend
// over each node's idle-TTL window (windows[i]; 0 = no TTL ⇒ unbounded). It fails
// CLOSED: an unpriceable node or a provider without pricing is an error, so a
// budget guard is never silently skipped.
func (o *Orchestrator) EstimateSpend(ctx context.Context, specs []NodeSpec, windows []time.Duration) (CostEstimate, error) {
	pricer, ok := o.P.(provider.Pricer)
	if !ok {
		return CostEstimate{}, fmt.Errorf("provider %q does not support pricing (cannot honor --max-cost)", o.P.Name())
	}
	var est CostEstimate
	for i, s := range specs {
		m, err := pricer.EstimateHourly(ctx, provider.ServerSpec{
			Name: s.Name, Type: s.Type, Image: s.Image, RegionPref: s.RegionPref, GPU: s.GPU,
		})
		if err != nil {
			return CostEstimate{}, err
		}
		if !m.Known() {
			return CostEstimate{}, fmt.Errorf("could not price node %q (cannot honor --max-cost)", s.Name)
		}
		est.Currency = m.Currency
		est.Hourly += m.Amount
		if windows[i] <= 0 {
			est.Unbounded = true
		} else {
			est.Projected += m.Amount * windows[i].Hours()
		}
	}
	return est, nil
}

// CheckBudget enforces a projected-total `--max-cost` (in the provider's
// currency) before any server is created. maxCost <= 0 disables the check. A node
// with no TTL makes the projection unbounded, which is an error under a cap.
// Returns nil (pass) or a descriptive, actionable error.
func (o *Orchestrator) CheckBudget(ctx context.Context, specs []NodeSpec, windows []time.Duration, maxCost float64) error {
	if maxCost <= 0 {
		return nil // disabled
	}
	est, err := o.EstimateSpend(ctx, specs, windows)
	if err != nil {
		return err
	}
	if est.Unbounded {
		return fmt.Errorf("--max-cost %.2f %s needs a bounded run, but a node has no TTL (--no-ttl) — projected spend is unbounded; set --ttl", maxCost, est.Currency)
	}
	if est.Projected > maxCost {
		return fmt.Errorf("--max-cost exceeded: projected %.2f %s (%.4f %s/hr × TTL) > cap %.2f %s — pin a smaller size, shorten --ttl, or raise --max-cost",
			est.Projected, est.Currency, est.Hourly, est.Currency, maxCost, est.Currency)
	}
	return nil
}

// DryRunNode is one node's `--dry-run` preview line: what would be provisioned
// and its price, without creating anything. Size/Region are "" when auto-selected.
type DryRunNode struct {
	Name, Size, Region string
	Hourly             provider.Money
	Window             time.Duration   // idle-TTL; 0 = none
	GPU                provider.GPUReq // zero = CPU-only
}

// PlanUp previews an `up`: the per-node plan and the rolled-up projected cost,
// creating nothing. Lenient (unlike CheckBudget): a node that can't be priced is
// shown unpriced rather than erroring, so the preview always renders.
func (o *Orchestrator) PlanUp(ctx context.Context, specs []NodeSpec, windows []time.Duration) ([]DryRunNode, CostEstimate, error) {
	pricer, ok := o.P.(provider.Pricer)
	if !ok {
		return nil, CostEstimate{}, fmt.Errorf("provider %q does not support pricing (cannot preview cost)", o.P.Name())
	}
	nodes := make([]DryRunNode, 0, len(specs))
	var est CostEstimate
	for i, s := range specs {
		m, err := pricer.EstimateHourly(ctx, provider.ServerSpec{
			Name: s.Name, Type: s.Type, Image: s.Image, RegionPref: s.RegionPref, GPU: s.GPU,
		})
		if err != nil {
			return nil, CostEstimate{}, err
		}
		region := ""
		if len(s.RegionPref) > 0 {
			region = s.RegionPref[0]
		}
		nodes = append(nodes, DryRunNode{Name: s.Name, Size: s.Type, Region: region, Hourly: m, Window: windows[i], GPU: s.GPU})
		if m.Known() {
			est.Currency = m.Currency
			est.Hourly += m.Amount
			if windows[i] <= 0 {
				est.Unbounded = true
			} else {
				est.Projected += m.Amount * windows[i].Hours()
			}
		}
	}
	return nodes, est, nil
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
