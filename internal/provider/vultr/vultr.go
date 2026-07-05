// Package vultr implements provider.Provider against the Vultr API using govultr.
// It mirrors the DigitalOcean backend's discipline — plans are discovered by SPEC
// at runtime (never hardcoded), creation searches plan x region for an available
// combination — proving the Provider seam with a third backend.
//
// Vultr's model is close to DigitalOcean's: tags are first-class (so reconcile is
// a tag query), each plan lists the regions where it is orderable (Plan.Locations,
// so availability is known up front) and carries a price. The one wrinkle is that
// Vultr prices are MONTHLY (USD) and the OS is chosen by a numeric os_id rather
// than an image slug, so we resolve Ubuntu's id from the OS catalog once.
package vultr

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vultr/govultr/v3"
	"github.com/yedidiaSch/pandion/internal/provider"
	"golang.org/x/oauth2"
)

const (
	// tagAll marks every Pandion instance (ListAllTagged / reaper source of truth).
	tagAll = "pandion"
	// tagCIDPrefix + sanitized cluster id is the per-cluster tag (ListByTag).
	tagCIDPrefix = "pandion-cid-"
	loginKeyName = "pandion-login-" // + sanitized cluster id
	// defaultOSID is Vultr's "Ubuntu 24.04 LTS x64" os_id, used as a fallback when
	// the OS catalog lookup can't resolve it (ids are stable in practice).
	defaultOSID = 2284
)

// Vultr tag/label chars: keep to lowercase letters, digits, dashes.
var nonTag = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitize(s string) string {
	return strings.Trim(nonTag.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func clusterTag(clusterID string) string { return tagCIDPrefix + sanitize(clusterID) }

// instanceLabel namespaces the instance by cluster id so labels never collide
// across clusters/retries. Reconciliation keys off the TAG, not the label (C4).
func instanceLabel(clusterID, node string) string {
	s := sanitize("pandion-" + clusterID + "-" + node)
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

// recoverClusterID reads the (sanitized) cluster id back out of an instance's tags.
func recoverClusterID(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, tagCIDPrefix) {
			return strings.TrimPrefix(t, tagCIDPrefix)
		}
	}
	return ""
}

// Vultr is a provider.Provider backed by Vultr.
type Vultr struct {
	c          *govultr.Client
	regionPref []string

	keyMu     sync.Mutex // serializes get-or-create of the shared login key
	planMu    sync.Mutex // guards the plan cache
	planCache []planInfo
	osMu      sync.Mutex // guards the resolved OS id
	osID      int
}

// Option configures a Vultr provider.
type Option func(*Vultr)

// WithRegionPref overrides the preferred regions, in priority order.
func WithRegionPref(regions ...string) Option {
	return func(v *Vultr) {
		if len(regions) > 0 {
			v.regionPref = regions
		}
	}
}

// New returns a Vultr provider. token must be a Vultr API key with write access.
func New(token string, opts ...Option) *Vultr {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	v := &Vultr{
		c:          govultr.NewClient(oauth2.NewClient(context.Background(), ts)),
		regionPref: []string{"ewr", "fra", "ams", "sea"},
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

// Name implements provider.Provider.
func (v *Vultr) Name() string { return "vultr" }

// CreateServer discovers an available (plan,region) by spec and provisions a
// tagged instance with the given cloud-init user-data.
func (v *Vultr) CreateServer(ctx context.Context, spec provider.ServerSpec) (provider.Server, error) {
	minCores, minRAM := spec.MinCores, spec.MinRAMGB
	if minCores == 0 {
		minCores = 2
	}
	if minRAM == 0 {
		minRAM = 2
	}

	osID, err := v.resolveOS(ctx, spec.Image)
	if err != nil {
		return provider.Server{}, err
	}

	plans, err := v.plans(ctx)
	if err != nil {
		return provider.Server{}, err
	}

	// candidate plans (cheapest-first)
	var cands []planInfo
	if spec.Type != "" {
		for _, p := range plans {
			if p.ID == spec.Type {
				cands = []planInfo{p}
				break
			}
		}
		if len(cands) == 0 {
			return provider.Server{}, fmt.Errorf("plan %q not found", spec.Type)
		}
	} else {
		cands = selectPlans(plans, minCores, minRAM*1024, "")
		if len(cands) == 0 {
			return provider.Server{}, fmt.Errorf("no plan matches spec (>=%dvcpu/%dGB)", minCores, minRAM)
		}
	}

	pref := v.regionPref
	if len(spec.RegionPref) > 0 {
		pref = spec.RegionPref
	}
	regions := orderRegions(regionsOf(cands), pref)

	// register the login key so it lands in root's authorized_keys.
	var sshKeys []string
	if spec.LoginPubKey != "" {
		id, err := v.ensureLoginKey(ctx, spec.ClusterID, spec.LoginPubKey)
		if err != nil {
			return provider.Server{}, fmt.Errorf("register login ssh key: %w", friendlyErr(err))
		}
		sshKeys = []string{id}
	}
	tags := []string{tagAll, clusterTag(spec.ClusterID)}
	// Vultr requires user_data base64-encoded (unlike DO, which takes it raw);
	// the SDK passes the field through verbatim, so we encode it here.
	var userData string
	if spec.UserData != "" {
		userData = base64.StdEncoding.EncodeToString([]byte(spec.UserData))
	}

	// search (region, plan): RegionFirst — exhaust a region's cheapest plans
	// before moving on (keeps nodes close; proximity over a marginal price delta).
	var lastErr error
	for _, region := range regions {
		for _, p := range cands {
			if !contains(p.Regions, region) {
				continue
			}
			inst, _, cerr := v.c.Instance.Create(ctx, &govultr.InstanceCreateReq{
				Region:   region,
				Plan:     p.ID,
				Label:    instanceLabel(spec.ClusterID, spec.Name),
				Hostname: instanceLabel(spec.ClusterID, spec.Name),
				OsID:     osID,
				SSHKeys:  sshKeys,
				Tags:     tags,
				UserData: userData,
			})
			if cerr != nil {
				lastErr = cerr
				if isAvailabilityErr(cerr) {
					continue // try the next (region,plan)
				}
				return provider.Server{}, fmt.Errorf("create %s@%s: %w", p.ID, region, friendlyErr(cerr))
			}
			return v.waitActive(ctx, inst.ID, spec.ClusterID)
		}
	}
	return provider.Server{}, fmt.Errorf("no available plan/region for spec; last error: %v", lastErr)
}

func (v *Vultr) waitActive(ctx context.Context, id, clusterID string) (provider.Server, error) {
	deadline := time.Now().Add(4 * time.Minute)
	for {
		inst, _, err := v.c.Instance.Get(ctx, id)
		if err != nil {
			return provider.Server{}, err
		}
		if inst.Status == "active" && inst.MainIP != "" && inst.MainIP != "0.0.0.0" {
			return toServer(inst, clusterID), nil
		}
		if time.Now().After(deadline) {
			return provider.Server{}, fmt.Errorf("instance %s not active within timeout", id)
		}
		select {
		case <-ctx.Done():
			return provider.Server{}, ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
}

// DestroyServer deletes by id. Idempotent: a missing instance is success (H7).
func (v *Vultr) DestroyServer(ctx context.Context, id string) error {
	if err := v.c.Instance.Delete(ctx, id); err != nil {
		if isNotFoundErr(err) {
			return nil // already gone
		}
		return err
	}
	return nil
}

// ListByTag returns all instances for a cluster — the reconcile source of truth (C4).
func (v *Vultr) ListByTag(ctx context.Context, clusterID string) ([]provider.Server, error) {
	insts, err := v.instancesByTag(ctx, clusterTag(clusterID))
	if err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(insts))
	for i := range insts {
		out = append(out, toServer(&insts[i], clusterID))
	}
	return out, nil
}

// ListAllTagged returns every Pandion-tagged instance (any cluster) — the reaper's
// source of truth. Recovers each instance's cluster id from its per-cluster tag.
func (v *Vultr) ListAllTagged(ctx context.Context) ([]provider.Server, error) {
	insts, err := v.instancesByTag(ctx, tagAll)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(insts))
	for i := range insts {
		out = append(out, toServer(&insts[i], recoverClusterID(insts[i].Tags)))
	}
	return out, nil
}

// ReapAux deletes the cluster's shared login key (Vultr keys carry no tags, so we
// match by our deterministic per-cluster name). Implements provider.AuxReaper.
func (v *Vultr) ReapAux(ctx context.Context, clusterID string) error {
	name := loginKeyName + sanitize(clusterID)
	keys, err := v.allKeys(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for i := range keys {
		if keys[i].Name == name {
			if derr := v.c.SSHKey.Delete(ctx, keys[i].ID); derr != nil && firstErr == nil {
				firstErr = derr
			}
		}
	}
	return firstErr
}

// HourlyPrice implements provider.Pricer: Vultr plans carry a MONTHLY price (USD),
// which we convert to the gross hourly rate. region is irrelevant (price is
// uniform per plan across regions). Unknown plan -> zero Money (nil error).
func (v *Vultr) HourlyPrice(ctx context.Context, planID, _ string) (provider.Money, error) {
	plans, err := v.plans(ctx)
	if err != nil {
		return provider.Money{}, err
	}
	for _, p := range plans {
		if p.ID == planID {
			if p.MonthlyCost <= 0 {
				return provider.Money{}, nil
			}
			return provider.Money{Amount: p.MonthlyCost / hoursPerMonth, Currency: "USD"}, nil
		}
	}
	return provider.Money{}, nil
}

// EstimateHourly implements provider.Pricer: price the plan a spec would
// provision (explicit, else the cheapest spec-match) without creating anything.
func (v *Vultr) EstimateHourly(ctx context.Context, spec provider.ServerSpec) (provider.Money, error) {
	plans, err := v.plans(ctx)
	if err != nil {
		return provider.Money{}, err
	}
	id := spec.Type
	if id == "" {
		minCores, minRAM := spec.MinCores, spec.MinRAMGB
		if minCores == 0 {
			minCores = 2
		}
		if minRAM == 0 {
			minRAM = 2
		}
		region := ""
		if len(spec.RegionPref) > 0 {
			region = spec.RegionPref[0]
		}
		cands := selectPlans(plans, minCores, minRAM*1024, region)
		if len(cands) == 0 {
			return provider.Money{}, nil
		}
		id = cands[0].ID
	}
	return v.HourlyPrice(ctx, id, "")
}

// ensureLoginKey get-or-creates the cluster's shared login key by a deterministic
// name, tolerating concurrent callers and Vultr's dedup-by-material.
func (v *Vultr) ensureLoginKey(ctx context.Context, clusterID, pubKey string) (string, error) {
	name := loginKeyName + sanitize(clusterID)
	v.keyMu.Lock()
	defer v.keyMu.Unlock()

	if id := v.findKey(ctx, name, pubKey); id != "" {
		return id, nil
	}
	k, _, cerr := v.c.SSHKey.Create(ctx, &govultr.SSHKeyReq{Name: name, SSHKey: pubKey})
	if cerr == nil {
		return k.ID, nil
	}
	// lost a race, or the material already exists — refetch.
	if id := v.findKey(ctx, name, pubKey); id != "" {
		return id, nil
	}
	return "", cerr
}

// findKey returns the id of a registered key matching our name or the key material.
func (v *Vultr) findKey(ctx context.Context, name, pubKey string) string {
	keys, err := v.allKeys(ctx)
	if err != nil {
		return ""
	}
	want := keyMaterial(pubKey)
	for i := range keys {
		if keys[i].Name == name || keyMaterial(keys[i].SSHKey) == want {
			return keys[i].ID
		}
	}
	return ""
}

// plans returns the Vultr plan catalog (as planInfo), fetched once and cached.
func (v *Vultr) plans(ctx context.Context) ([]planInfo, error) {
	v.planMu.Lock()
	defer v.planMu.Unlock()
	if v.planCache != nil {
		return v.planCache, nil
	}
	var out []planInfo
	opt := &govultr.ListOptions{PerPage: 500}
	for {
		page, meta, _, err := v.c.Plan.List(ctx, "", opt)
		if err != nil {
			return nil, err
		}
		for _, p := range page {
			out = append(out, planInfo{
				ID: p.ID, VCPU: p.VCPUCount, RAMMB: p.RAM,
				MonthlyCost: float64(p.MonthlyCost), Regions: p.Locations,
				GPU: p.GPUType != "" || p.GPUVRAM > 0,
			})
		}
		if meta == nil || meta.Links == nil || meta.Links.Next == "" {
			break
		}
		opt.Cursor = meta.Links.Next
	}
	v.planCache = out
	return out, nil
}

// resolveOS returns the os_id to install. image may be a numeric os_id (used
// verbatim), or empty to auto-select Ubuntu 24.04 x64 from the OS catalog.
func (v *Vultr) resolveOS(ctx context.Context, image string) (int, error) {
	if image != "" {
		if id, err := strconv.Atoi(strings.TrimSpace(image)); err == nil {
			return id, nil
		}
		return 0, fmt.Errorf("vultr image must be a numeric os_id, got %q", image)
	}
	v.osMu.Lock()
	defer v.osMu.Unlock()
	if v.osID != 0 {
		return v.osID, nil
	}
	opt := &govultr.ListOptions{PerPage: 500}
	for {
		page, meta, _, err := v.c.OS.List(ctx, opt)
		if err != nil {
			// network hiccup shouldn't strand the caller — fall back to the constant.
			return defaultOSID, nil //nolint:nilerr
		}
		for _, o := range page {
			n := strings.ToLower(o.Name)
			if o.Family == "ubuntu" && strings.Contains(n, "24.04") && o.Arch == "x64" {
				v.osID = o.ID
				return o.ID, nil
			}
		}
		if meta == nil || meta.Links == nil || meta.Links.Next == "" {
			break
		}
		opt.Cursor = meta.Links.Next
	}
	v.osID = defaultOSID
	return defaultOSID, nil
}

// instancesByTag lists all Pandion instances carrying tag (client-side filtered,
// so it is robust to Vultr's list-filter deprecations).
func (v *Vultr) instancesByTag(ctx context.Context, tag string) ([]govultr.Instance, error) {
	var out []govultr.Instance
	opt := &govultr.ListOptions{PerPage: 500}
	for {
		page, meta, _, err := v.c.Instance.List(ctx, opt)
		if err != nil {
			return nil, friendlyErr(err)
		}
		for i := range page {
			if contains(page[i].Tags, tag) {
				out = append(out, page[i])
			}
		}
		if meta == nil || meta.Links == nil || meta.Links.Next == "" {
			break
		}
		opt.Cursor = meta.Links.Next
	}
	return out, nil
}

func (v *Vultr) allKeys(ctx context.Context) ([]govultr.SSHKey, error) {
	var all []govultr.SSHKey
	opt := &govultr.ListOptions{PerPage: 500}
	for {
		page, meta, _, err := v.c.SSHKey.List(ctx, opt)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if meta == nil || meta.Links == nil || meta.Links.Next == "" {
			break
		}
		opt.Cursor = meta.Links.Next
	}
	return all, nil
}

func toServer(inst *govultr.Instance, clusterID string) provider.Server {
	ps := provider.Server{
		ID:        inst.ID,
		Name:      inst.Label,
		ClusterID: clusterID,
		Type:      inst.Plan,
		Region:    inst.Region,
		IP:        inst.MainIP,
	}
	if t, err := time.Parse(time.RFC3339, inst.DateCreated); err == nil {
		ps.Created = t
	}
	return ps
}

// isAvailabilityErr reports a "not orderable here / sold out" error (try the next
// region,plan), versus a real failure.
func isAvailabilityErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not available") ||
		strings.Contains(m, "unavailable") ||
		strings.Contains(m, "sold out") ||
		strings.Contains(m, "no capacity") ||
		strings.Contains(m, "out of stock")
}

// isNotFoundErr reports that a resource is already gone (idempotent destroy).
func isNotFoundErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not found") ||
		strings.Contains(m, "does not exist") ||
		strings.Contains(m, "invalid instance") ||
		strings.Contains(m, "unable to find")
}

// friendlyErr turns Vultr's opaque "Unauthorized IP address: <ip>" 401 into
// actionable guidance. Vultr's optional API Access Control allowlist rejects
// account-scoped calls from non-allowlisted IPs — an easy trap, especially on a
// dual-stack host where the SDK may egress over an unlisted IPv6. Public catalog
// endpoints (plans/regions) are unaffected, so pricing can work while provisioning
// fails, which is doubly confusing without this hint.
func friendlyErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "Unauthorized IP address") {
		return fmt.Errorf("%w\n"+
			"  → this host's IP is not allowed by Vultr's API Access Control.\n"+
			"    Add it (allow BOTH your IPv4 and IPv6) under Vultr console →\n"+
			"    Account → API → Access Control, or toggle 'Allow All'", err)
	}
	return err
}

// keyMaterial extracts the base64 body of an SSH public key so keys compare by
// material, ignoring name/comment.
func keyMaterial(pub string) string {
	if f := strings.Fields(strings.TrimSpace(pub)); len(f) >= 2 {
		return f[1]
	}
	return strings.TrimSpace(pub)
}
