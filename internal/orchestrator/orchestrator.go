// SPDX-License-Identifier: AGPL-3.0-or-later

// Package orchestrator is the provider-agnostic state machine + reconcile loop.
// M0 scope: single-node Up, and a Down that reconciles to empty using the
// provider as the source of truth (so it works even if local state is lost).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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
	// Progress, if set, receives provisioning progress from slow-booting
	// providers (forwarded as ServerSpec.Progress), so a multi-minute launch
	// visibly moves instead of looking hung.
	Progress provider.ProgressFunc
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
	c.Nodes[0].Launched = time.Now().UTC() // billing starts at launch, not at "ready"
	if err := o.S.Save(c); err != nil {    // journal BEFORE the create (crash-resumable)
		return nil, err
	}

	srv, err := o.createWithRetry(ctx, provider.ServerSpec{
		Name:        nodeName,
		ClusterID:   clusterID,
		Type:        spec.Type,
		Image:       spec.Image,
		RegionPref:  spec.RegionPref,
		UserData:    userData,
		LoginPubKey: loginPubKey,
		GPU:         spec.GPU,
		Progress:    o.Progress,
	})
	if err != nil {
		c.Nodes[0].Phase = state.Failed
		c.Nodes[0].ServerID = provider.LaunchedServerID(err) // the instance may exist: journal it
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

			mu.Lock()
			c.Nodes[i].Phase = state.Provisioning
			c.Nodes[i].Launched = time.Now().UTC()
			mu.Unlock()
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
				Progress:    o.Progress,
			})
			if err != nil {
				mu.Lock()
				c.Nodes[i].Phase = state.Failed
				c.Nodes[i].ServerID = provider.LaunchedServerID(err)
				mu.Unlock()
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

type ReapCandidate struct {
	ClusterID string
	Servers   int
	OldestAge time.Duration // meaningful only when AgeKnown
	AgeKnown  bool          // false when neither the provider nor the journal knows any node's launch time
}

// ReapPlan lists the Pandion clusters currently alive at the provider whose
// oldest server is at least olderThan (0 = all). It queries the provider
// directly, so it works across machines and with no local state (C4).
//
// A cluster whose age is UNKNOWN (the provider reports no creation time and no
// local journal has one — e.g. a Lambda instance launched from another machine
// that never became healthy) is never assumed old: under olderThan > 0 it is
// left out of the plan and its id is returned in skipped, so the caller can say
// so instead of silently reaping a node that may have launched a minute ago.
func (o *Orchestrator) ReapPlan(ctx context.Context, olderThan time.Duration) (plan []ReapCandidate, skipped []string, err error) {
	all, err := o.P.ListAllTagged(ctx)
	if err != nil {
		return nil, nil, err
	}
	type agg struct {
		n      int
		oldest time.Time // zero until a known launch time is seen
	}
	byCluster := map[string]*agg{}
	now := time.Now()
	for _, s := range all {
		a := byCluster[s.ClusterID]
		if a == nil {
			a = &agg{}
			byCluster[s.ClusterID] = a
		}
		a.n++
		if t := o.LaunchedAt(s); !t.IsZero() && (a.oldest.IsZero() || t.Before(a.oldest)) {
			a.oldest = t
		}
	}
	for id, a := range byCluster {
		c := ReapCandidate{ClusterID: id, Servers: a.n}
		if !a.oldest.IsZero() {
			c.OldestAge, c.AgeKnown = now.Sub(a.oldest), true
		}
		if olderThan > 0 {
			if !c.AgeKnown {
				skipped = append(skipped, id)
				continue
			}
			if c.OldestAge < olderThan {
				continue
			}
		}
		plan = append(plan, c)
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].ClusterID < plan[j].ClusterID })
	sort.Strings(skipped)
	return plan, skipped, nil
}

// LaunchedAt is the best-known launch time of a provider server: the EARLIER of
// the provider's own creation time and the launch time Pandion journaled for that
// server (matched by server id, else by node name). Zero means unknown. The
// journal covers providers that report no creation time (Lambda reports only
// first_healthy — later than launch, and null if the VM never booted).
func (o *Orchestrator) LaunchedAt(s provider.Server) time.Time {
	best := s.Created
	if o.S == nil {
		return best
	}
	c, err := o.S.Load(s.ClusterID)
	if err != nil {
		return best
	}
	for _, n := range c.Nodes {
		if n.Launched.IsZero() {
			continue
		}
		if (n.ServerID != "" && n.ServerID == s.ID) || (n.ServerID == "" && nodeNameMatches(s.Name, n.Name)) {
			if best.IsZero() || n.Launched.Before(best) {
				best = n.Launched
			}
			break
		}
	}
	return best
}

// nodeNameMatches reports whether a provider server name denotes journal node
// name: providers either use the node name verbatim or embed it as a suffix
// (Lambda: "pandion-<cluster>--<node>").
func nodeNameMatches(serverName, node string) bool {
	return serverName == node || strings.HasSuffix(serverName, "--"+node) || strings.HasSuffix(serverName, "-"+node)
}

// NodeStatus is the live view of one running node for `ls`/`status`.
type NodeStatus struct {
	Name, Type, Region, IP string
	Age                    time.Duration    // meaningful only when AgeKnown
	AgeKnown               bool             // false = provider + journal know no launch time
	Status                 string           // provider lifecycle state ("" = running; e.g. "terminating", "booting")
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
		var age time.Duration
		launched := o.LaunchedAt(s)
		if !launched.IsZero() {
			age = now.Sub(launched)
		}
		n := NodeStatus{Name: s.Name, Type: s.Type, Region: s.Region, IP: s.IP, Age: age, AgeKnown: !launched.IsZero(),
			Status: s.Status, GPU: s.GPU, GPUUtil: -1}
		if pricer != nil {
			// a per-node pricing error is non-fatal: leave that node unpriced.
			if m, perr := pricer.HourlyPrice(ctx, s.Type, s.Region); perr == nil && m.Known() {
				n.Hourly = m
				currency = m.Currency
				cs.Hourly += m.Amount
				cs.Accrued += m.Amount * age.Hours() // 0 for an unknown age — never a guess
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

// Down reconciles the cluster to empty. It lists by tag from the PROVIDER (the
// source of truth, C4) so it succeeds even with no local state, destroys with
// retry (H7), then WAITS until the provider no longer lists anything for the
// cluster before closing the state record.
//
// Teardown is asynchronous at some providers (Lambda reports "terminating" for
// ~30s normally, and an instance that never finished launching has been seen
// stuck there for 20+ minutes). An accepted terminate is NOT "gone": if servers
// are still terminating when DownWait runs out, Down returns a
// *StillTerminatingError and keeps the local state, so nothing claims the
// cluster is gone while the provider still lists it (and may bill for it).
func (o *Orchestrator) Down(ctx context.Context, clusterID string) error {
	servers, err := o.P.ListByTag(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("list %q: %w", clusterID, err)
	}

	// Also target any instance the journal knows was launched but that the
	// provider's list doesn't (yet) show — e.g. a launch that timed out before the
	// instance appeared in listings. DestroyServer is idempotent, so a stale id
	// costs one no-op call.
	ids := make([]string, 0, len(servers))
	seen := map[string]bool{}
	for _, s := range servers {
		ids = append(ids, s.ID)
		seen[s.ID] = true
	}

	// best-effort journal of the teardown intent (local state may not exist)
	if c, lerr := o.S.Load(clusterID); lerr == nil {
		for i := range c.Nodes {
			c.Nodes[i].Phase = state.TearingDown
			// journaled ids are only meaningful at the provider that issued them.
			if id := c.Nodes[i].ServerID; id != "" && !seen[id] && c.Provider == o.P.Name() {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		_ = o.S.Save(c)
	}

	// Destroy everything, including servers already terminating: re-sending a
	// terminate is idempotent and is exactly what nudges a stuck one along.
	var firstErr error
	for _, id := range ids {
		if derr := destroyWithRetry(ctx, o.P, id, 3); derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	if firstErr != nil {
		return fmt.Errorf("teardown %q: %w", clusterID, firstErr)
	}

	// verify the provider truly has nothing left for this cluster, waiting out an
	// asynchronous termination.
	left, err := o.waitGone(ctx, clusterID)
	if err != nil {
		return err
	}
	if len(left) != 0 {
		var running []provider.Server
		for _, s := range left {
			if !s.Terminating() {
				running = append(running, s)
			}
		}
		if len(running) == 0 {
			return &StillTerminatingError{ClusterID: clusterID, Servers: left}
		}
		return fmt.Errorf("teardown incomplete: %d server(s) remain for %q — safe to re-run `pandion down --id %s`; teardown is idempotent and converges", len(left), clusterID, clusterID)
	}

	// clean up cluster-scoped auxiliary resources (e.g. registered SSH keys) so
	// nothing leaks, if the provider supports it (C4).
	if reaper, ok := o.P.(provider.AuxReaper); ok {
		if rerr := reaper.ReapAux(ctx, clusterID); rerr != nil {
			// The servers are already destroyed here (billing has stopped); only an
			// auxiliary resource (key/firewall/volume) failed to reap. Re-running is
			// safe and idempotent — say so, so the operator doesn't think money leaks.
			return fmt.Errorf("servers destroyed, but reaping aux resources for %q failed: %w — servers are gone (billing stopped); re-run `pandion down --id %s` to finish (idempotent)", clusterID, rerr, clusterID)
		}
	}
	return o.S.Close(clusterID)
}

// DownWait bounds how long Down waits for an asynchronous termination to finish
// before reporting the servers as still terminating. A var so the CLI can expose
// it and tests can shrink it.
var DownWait = 3 * time.Minute

// downPollEvery is the re-list interval while waiting. A var for tests.
var downPollEvery = 5 * time.Second

// waitGone re-lists the cluster until the provider reports nothing left or
// DownWait elapses, returning whatever is still listed. Only servers still
// terminating are waited for: a server in any other state means the teardown
// didn't take, so it returns immediately for Down to report it.
func (o *Orchestrator) waitGone(ctx context.Context, clusterID string) ([]provider.Server, error) {
	deadline := time.Now().Add(DownWait)
	for {
		left, err := o.P.ListByTag(ctx, clusterID)
		if err != nil {
			return nil, err
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left, nil
		}
		for _, s := range left {
			if !s.Terminating() {
				return left, nil
			}
		}
		select {
		case <-ctx.Done():
			return left, nil
		case <-time.After(downPollEvery):
		}
	}
}

// StillTerminatingError reports that every teardown was accepted, but the
// provider still lists the servers as terminating after DownWait. The cluster is
// NOT reported gone and its local state is kept: re-run `down` to check again.
type StillTerminatingError struct {
	ClusterID string
	Servers   []provider.Server
}

func (e *StillTerminatingError) Error() string {
	ids := make([]string, len(e.Servers))
	for i, s := range e.Servers {
		ids[i] = s.ID
	}
	return fmt.Sprintf("terminate accepted, but %d server(s) of %q are still terminating at the provider (%s) — not reporting the cluster gone until the provider does; re-run `pandion down --id %s` later to re-send terminate and re-check",
		len(e.Servers), e.ClusterID, strings.Join(ids, ", "), e.ClusterID)
}

// IsStillTerminating reports whether err is (or wraps) a StillTerminatingError.
func IsStillTerminating(err error) bool {
	var st *StillTerminatingError
	return errors.As(err, &st)
}

// destroyRetryBaseDelay is the first pause between destroy attempts; it grows ~3x
// per attempt (1s, 3s, 9s…). A var so tests can drop it to zero. Mirrors
// provisionRetryDelay on the creation side.
var destroyRetryBaseDelay = 1 * time.Second

// destroyWithRetry destroys a server, retrying with exponential backoff up to
// `attempts` times. DestroyServer is contractually idempotent, so retrying after a
// partial success is safe. Unlike the old instant 3× loop it (a) backs off so a
// rate-limited (429) provider isn't hammered in milliseconds, honoring ctx, and
// (b) aggregates every attempt's error — so "429, 429, 429" is visible in the
// message instead of only the last error hiding the pattern (F7/R8).
func destroyWithRetry(ctx context.Context, p provider.Provider, id string, attempts int) error {
	var errs []error
	delay := destroyRetryBaseDelay
	for i := 0; i < attempts; i++ {
		err := p.DestroyServer(ctx, id)
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("attempt %d: %w", i+1, err))
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return errors.Join(append(errs, ctx.Err())...)
			case <-time.After(delay):
			}
			delay *= 3
		}
	}
	return errors.Join(errs...)
}
