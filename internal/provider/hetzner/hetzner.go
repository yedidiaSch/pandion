// Package hetzner implements provider.Provider against the Hetzner Cloud API
// using hcloud-go. It encodes the spike findings: server types are discovered by
// SPEC at runtime (never hardcoded — S1/F3), and creation searches type x location
// for an available combination (availability is sparse).
package hetzner

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/yedidiaSch/pandion/internal/provider"
)

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// serverName namespaces the Hetzner server name by cluster id so names are unique
// per project (Hetzner requirement) and never collide across clusters or retries
// (finding F9). Reconciliation still keys off the cluster-id LABEL, not the name.
func serverName(clusterID, node string) string {
	s := strings.ToLower(fmt.Sprintf("pandion-%s-%s", clusterID, node))
	s = nonDNS.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

// LabelClusterID tags every resource so ListByTag can reconcile from provider
// truth even if local state is lost (C4).
const LabelClusterID = "pandion-cluster-id"

// Hetzner is a provider.Provider backed by Hetzner Cloud.
type Hetzner struct {
	c          *hcloud.Client
	arch       string
	regionPref []string
	mode       SearchMode
	keyMu      sync.Mutex // serializes get-or-create of the shared login SSH key

	priceMu    sync.Mutex      // guards the pricing cache
	priceCache *hcloud.Pricing // fetched once, reused for every HourlyPrice lookup
}

// Option configures a Hetzner provider.
type Option func(*Hetzner)

// WithSearchMode sets the type/location search ordering (default RegionFirst).
func WithSearchMode(m SearchMode) Option { return func(h *Hetzner) { h.mode = m } }

// WithRegionPref overrides the preferred locations, in priority order.
func WithRegionPref(regions ...string) Option {
	return func(h *Hetzner) {
		if len(regions) > 0 {
			h.regionPref = regions
		}
	}
}

// New returns a Hetzner provider. token must be a project-scoped API token.
// Default search mode is RegionFirst (proximity over a marginal price delta).
func New(token string, opts ...Option) *Hetzner {
	h := &Hetzner{
		c:          hcloud.NewClient(hcloud.WithToken(token)),
		arch:       string(hcloud.ArchitectureX86),
		regionPref: []string{"fsn1", "nbg1", "hel1"},
		mode:       RegionFirst,
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Name implements provider.Provider.
func (h *Hetzner) Name() string { return "hetzner" }

// CreateServer discovers an available (type,location) by spec and provisions a
// tagged server with the given cloud-init user-data.
func (h *Hetzner) CreateServer(ctx context.Context, spec provider.ServerSpec) (provider.Server, error) {
	minCores, minRAM, image := spec.MinCores, spec.MinRAMGB, spec.Image
	if minCores == 0 {
		minCores = 2
	}
	if minRAM == 0 {
		minRAM = 2
	}
	if image == "" {
		image = "ubuntu-24.04"
	}

	// 1) discover server types -> candidate names by spec (S1/F3)
	sts, err := h.c.ServerType.All(ctx)
	if err != nil {
		return provider.Server{}, fmt.Errorf("list server types: %w", err)
	}
	byName := make(map[string]*hcloud.ServerType, len(sts))
	infos := make([]typeInfo, 0, len(sts))
	for _, st := range sts {
		byName[st.Name] = st
		infos = append(infos, typeInfo{
			Name: st.Name, Cores: st.Cores, MemGB: float64(st.Memory),
			Arch: string(st.Architecture), Deprecated: st.IsDeprecated(),
		})
	}
	var candidates []string
	if spec.Type != "" {
		// exact type requested (e.g. cluster.yaml `size: cpx21`)
		if _, ok := byName[spec.Type]; !ok {
			return provider.Server{}, fmt.Errorf("server type %q not found", spec.Type)
		}
		candidates = []string{spec.Type}
	} else {
		candidates = selectCandidates(infos, minCores, minRAM, h.arch)
		if len(candidates) == 0 {
			return provider.Server{}, fmt.Errorf("no server type matches spec (>=%dc/%dGB, %s)", minCores, minRAM, h.arch)
		}
	}

	// 2) discover + order locations
	locs, err := h.c.Location.All(ctx)
	if err != nil {
		return provider.Server{}, fmt.Errorf("list locations: %w", err)
	}
	locNames := make([]string, 0, len(locs))
	locByName := make(map[string]*hcloud.Location, len(locs))
	for _, l := range locs {
		locNames = append(locNames, l.Name)
		locByName[l.Name] = l
	}
	pref := h.regionPref
	if len(spec.RegionPref) > 0 {
		pref = spec.RegionPref
	}
	ordered := orderLocations(locNames, pref)

	// 3) architecture-matched image
	img, _, err := h.c.Image.GetByNameAndArchitecture(ctx, image, hcloud.Architecture(h.arch))
	if err != nil {
		return provider.Server{}, fmt.Errorf("lookup image %q: %w", image, err)
	}
	if img == nil {
		return provider.Server{}, fmt.Errorf("image %q not found for arch %s", image, h.arch)
	}

	labels := map[string]string{LabelClusterID: spec.ClusterID}

	// 3b) register the login key so it lands in root's authorized_keys reliably
	//     (validated path, spike S1 — do not rely on cloud-init default-user).
	//     Cluster nodes share ONE login key, so this is get-or-create by a
	//     deterministic per-cluster name (Hetzner rejects duplicate public keys),
	//     serialized to survive concurrent provisioning (finding F13).
	var sshKeys []*hcloud.SSHKey
	if spec.LoginPubKey != "" {
		k, err := h.ensureLoginKey(ctx, spec.ClusterID, spec.LoginPubKey, labels)
		if err != nil {
			return provider.Server{}, fmt.Errorf("register login ssh key: %w", err)
		}
		sshKeys = []*hcloud.SSHKey{k}
	}

	// 4) search type x location until one is available, ordered per mode (F8/R15)
	var lastErr error
	for _, pair := range searchPlan(candidates, ordered, h.mode) {
		tname, lname := pair[0], pair[1]
		res, _, cerr := h.c.Server.Create(ctx, hcloud.ServerCreateOpts{
			Name:             serverName(spec.ClusterID, spec.Name),
			ServerType:       byName[tname],
			Image:            img,
			Location:         locByName[lname],
			UserData:         spec.UserData,
			SSHKeys:          sshKeys,
			Labels:           labels,
			StartAfterCreate: hcloud.Ptr(true),
		})
		if cerr != nil {
			lastErr = cerr
			if isAvailabilityErr(cerr) {
				continue // try next (type,location) per the plan
			}
			return provider.Server{}, fmt.Errorf("create %s@%s: %w", tname, lname, cerr)
		}
		srv, werr := h.waitRunning(ctx, res.Server.ID)
		if werr != nil {
			return provider.Server{}, werr
		}
		return toServer(srv, spec.ClusterID), nil
	}
	return provider.Server{}, fmt.Errorf("no available type/location for spec; last error: %v", lastErr)
}

func (h *Hetzner) waitRunning(ctx context.Context, id int64) (*hcloud.Server, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		srv, _, err := h.c.Server.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if srv == nil {
			return nil, fmt.Errorf("server %d vanished during boot", id)
		}
		if srv.Status == hcloud.ServerStatusRunning && srv.PublicNet.IPv4.IP != nil {
			return srv, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("server %d not running within timeout", id)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// DestroyServer deletes by id. Idempotent: a missing server is success (H7).
func (h *Hetzner) DestroyServer(ctx context.Context, id string) error {
	sid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid server id %q: %w", id, err)
	}
	srv, _, err := h.c.Server.GetByID(ctx, sid)
	if err != nil {
		return err
	}
	if srv == nil {
		return nil // already gone
	}
	_, err = h.c.Server.Delete(ctx, srv)
	return err
}

// ListByTag returns all servers for a cluster — the reconcile source of truth (C4).
func (h *Hetzner) ListByTag(ctx context.Context, clusterID string) ([]provider.Server, error) {
	srvs, err := h.c.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: LabelClusterID + "=" + clusterID},
	})
	if err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(srvs))
	for _, s := range srvs {
		out = append(out, toServer(s, clusterID))
	}
	return out, nil
}

// ListAllTagged returns every Pandion-tagged server (any cluster) — the reaper's
// source of truth. Selects on the presence of the cluster-id label.
func (h *Hetzner) ListAllTagged(ctx context.Context) ([]provider.Server, error) {
	srvs, err := h.c.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: LabelClusterID}, // key exists
	})
	if err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(srvs))
	for _, s := range srvs {
		out = append(out, toServer(s, s.Labels[LabelClusterID]))
	}
	return out, nil
}

// HourlyPrice implements provider.Pricer: the gross hourly price for a server
// type in a region. Pricing is fetched once and cached. A type/region we can't
// price returns a zero Money (nil error) so `ls` degrades to "—" rather than
// failing the whole listing.
func (h *Hetzner) HourlyPrice(ctx context.Context, serverType, region string) (provider.Money, error) {
	pr, err := h.pricing(ctx)
	if err != nil {
		return provider.Money{}, err
	}
	for _, st := range pr.ServerTypes {
		if st.ServerType == nil || st.ServerType.Name != serverType {
			continue
		}
		// exact region match, else fall back to any location (base rate is ~equal).
		var gross string
		for _, lp := range st.Pricings {
			if lp.Location != nil && lp.Location.Name == region {
				gross = lp.Hourly.Gross
				break
			}
			if gross == "" {
				gross = lp.Hourly.Gross // fallback: first available
			}
		}
		amt, perr := strconv.ParseFloat(strings.TrimSpace(gross), 64)
		if perr != nil || amt <= 0 {
			return provider.Money{}, nil // unknown, don't error the listing
		}
		return provider.Money{Amount: amt, Currency: pr.Currency}, nil
	}
	return provider.Money{}, nil // unknown type
}

// EstimateHourly implements provider.Pricer: resolve the type a spec would
// provision (explicit `size`, else the first spec-matched candidate — mirroring
// CreateServer's selection) and price it, without creating anything.
func (h *Hetzner) EstimateHourly(ctx context.Context, spec provider.ServerSpec) (provider.Money, error) {
	serverType := spec.Type
	if serverType == "" {
		minCores, minRAM := spec.MinCores, spec.MinRAMGB
		if minCores == 0 {
			minCores = 2
		}
		if minRAM == 0 {
			minRAM = 2
		}
		sts, err := h.c.ServerType.All(ctx)
		if err != nil {
			return provider.Money{}, fmt.Errorf("list server types: %w", err)
		}
		infos := make([]typeInfo, 0, len(sts))
		for _, st := range sts {
			infos = append(infos, typeInfo{
				Name: st.Name, Cores: st.Cores, MemGB: float64(st.Memory),
				Arch: string(st.Architecture), Deprecated: st.IsDeprecated(),
			})
		}
		cands := selectCandidates(infos, minCores, minRAM, h.arch)
		if len(cands) == 0 {
			return provider.Money{}, fmt.Errorf("no server type matches spec (>=%dc/%dGB, %s)", minCores, minRAM, h.arch)
		}
		serverType = cands[0] // the type CreateServer would try first
	}
	region := ""
	switch {
	case len(spec.RegionPref) > 0:
		region = spec.RegionPref[0]
	case len(h.regionPref) > 0:
		region = h.regionPref[0]
	}
	return h.HourlyPrice(ctx, serverType, region)
}

// pricing returns the cached provider pricing, fetching it on first use. A failed
// fetch is not cached, so a transient error is retried on the next call.
func (h *Hetzner) pricing(ctx context.Context) (*hcloud.Pricing, error) {
	h.priceMu.Lock()
	defer h.priceMu.Unlock()
	if h.priceCache != nil {
		return h.priceCache, nil
	}
	p, _, err := h.c.Pricing.Get(ctx)
	if err != nil {
		return nil, err
	}
	h.priceCache = &p
	return h.priceCache, nil
}

// ensureLoginKey get-or-creates the cluster's shared login SSH key by a
// deterministic name, tolerating concurrent callers and duplicate-key errors.
func (h *Hetzner) ensureLoginKey(ctx context.Context, clusterID, pubKey string, labels map[string]string) (*hcloud.SSHKey, error) {
	name := "pandion-login-" + clusterID
	h.keyMu.Lock()
	defer h.keyMu.Unlock()

	if k, _, err := h.c.SSHKey.GetByName(ctx, name); err != nil {
		return nil, err
	} else if k != nil {
		return k, nil // already registered for this cluster
	}
	k, _, err := h.c.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{
		Name: name, PublicKey: pubKey, Labels: labels,
	})
	if err == nil {
		return k, nil
	}
	// lost a race (same name) — fetch by name.
	if k2, _, gerr := h.c.SSHKey.GetByName(ctx, name); gerr == nil && k2 != nil {
		return k2, nil
	}
	// duplicate public key under a DIFFERENT name (e.g. a leftover from a prior
	// run) — Hetzner dedupes on key material, so find and reuse it.
	if all, aerr := h.c.SSHKey.All(ctx); aerr == nil {
		want := keyMaterial(pubKey)
		for _, existing := range all {
			if keyMaterial(existing.PublicKey) == want {
				return existing, nil
			}
		}
	}
	return nil, err
}

// keyMaterial extracts the base64 body of an SSH public key ("ssh-ed25519 BODY
// comment" -> "BODY"), so keys are compared by material, ignoring name/comment.
func keyMaterial(pub string) string {
	f := strings.Fields(strings.TrimSpace(pub))
	if len(f) >= 2 {
		return f[1]
	}
	return strings.TrimSpace(pub)
}

// ReapAux deletes cluster-scoped SSH keys we registered (implements
// provider.AuxReaper) so teardown leaves nothing behind.
func (h *Hetzner) ReapAux(ctx context.Context, clusterID string) error {
	keys, err := h.c.SSHKey.AllWithOpts(ctx, hcloud.SSHKeyListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: LabelClusterID + "=" + clusterID},
	})
	if err != nil {
		return err
	}
	var firstErr error
	for _, k := range keys {
		if _, derr := h.c.SSHKey.Delete(ctx, k); derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	return firstErr
}

func toServer(s *hcloud.Server, clusterID string) provider.Server {
	ps := provider.Server{
		ID:        strconv.FormatInt(s.ID, 10),
		Name:      s.Name,
		ClusterID: clusterID,
		Created:   s.Created,
	}
	if s.ServerType != nil {
		ps.Type = s.ServerType.Name
	}
	// Prefer the location name (e.g. "fsn1"); fall back to the datacenter name
	// (e.g. "fsn1-dc14") since list responses don't always hydrate the nested
	// location — so `ls` shows a region rather than "—".
	if s.Datacenter != nil {
		if s.Datacenter.Location != nil && s.Datacenter.Location.Name != "" {
			ps.Region = s.Datacenter.Location.Name
		} else if s.Datacenter.Name != "" {
			ps.Region = s.Datacenter.Name
		}
	}
	if ip := s.PublicNet.IPv4.IP; ip != nil {
		ps.IP = ip.String()
	}
	return ps
}

// isAvailabilityErr reports a "this type isn't orderable here" error (try next),
// versus a real failure. Mirrors the spike's robust matching.
func isAvailabilityErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unsupported location") ||
		strings.Contains(m, "unavailable") ||
		strings.Contains(m, "resource_unavailable")
}

// compile-time assertion that *Hetzner satisfies the interface
var _ provider.Provider = (*Hetzner)(nil)
