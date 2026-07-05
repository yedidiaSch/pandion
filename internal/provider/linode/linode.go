// Package linode implements provider.Provider against the Linode (Akamai) API
// using linodego. It mirrors the DigitalOcean backend's discipline — types are
// discovered by SPEC at runtime (never hardcoded), creation searches type x region
// for an available combination — proving the Provider seam with a third backend.
//
// Linode differs from DigitalOcean/Vultr in three ways this code handles: the
// login public key is installed inline at create time (AuthorizedKeys) so there
// is NO separate SSH-key resource to register or reap; cloud-init user-data rides
// the instance Metadata field (base64) and so requires a region with the
// "Metadata" capability; and a root password is mandatory when deploying from an
// image, so we generate a strong random one and throw it away (login is key-only).
package linode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linode/linodego"
	"github.com/yedidiaSch/pandion/internal/provider"
)

const (
	// tagAll marks every Pandion instance (ListAllTagged / reaper source of truth).
	tagAll = "pandion"
	// tagCIDPrefix + sanitized cluster id is the per-cluster tag (ListByTag).
	tagCIDPrefix = "pandion-cid-"
	defaultImage = "linode/ubuntu24.04"
	// capLinodes / capMetadata are the region capability strings we require.
	capLinodes  = "Linodes"
	capMetadata = "Metadata"
)

// Linode label chars: letters, digits, dashes, underscores, dots; must start
// alphanumeric. We keep to lowercase letters, digits, dashes for tags/labels.
var nonTag = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitize(s string) string {
	return strings.Trim(nonTag.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func clusterTag(clusterID string) string { return tagCIDPrefix + sanitize(clusterID) }

// instanceLabel namespaces the instance by cluster id so labels never collide
// across clusters/retries. Reconciliation keys off the TAG, not the label (C4).
// Linode labels must be 3-64 chars and start with a letter — we prefix "pandion".
func instanceLabel(clusterID, node string) string {
	s := sanitize("pandion-" + clusterID + "-" + node)
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-")
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

// Linode is a provider.Provider backed by Linode (Akamai).
type Linode struct {
	c          linodego.Client
	regionPref []string

	typeMu    sync.Mutex // guards the type cache
	typeCache []typeInfo
	regMu     sync.Mutex // guards the region cache
	regCache  []linodego.Region
}

// Option configures a Linode provider.
type Option func(*Linode)

// WithRegionPref overrides the preferred regions, in priority order.
func WithRegionPref(regions ...string) Option {
	return func(l *Linode) {
		if len(regions) > 0 {
			l.regionPref = regions
		}
	}
}

// New returns a Linode provider. token must be a Linode personal access token
// with read/write on Linodes.
func New(token string, opts ...Option) *Linode {
	c := linodego.NewClient(nil)
	c.SetToken(token)
	l := &Linode{
		c:          c,
		regionPref: []string{"us-east", "eu-central", "nl-ams", "us-west"},
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Name implements provider.Provider.
func (l *Linode) Name() string { return "linode" }

// CreateServer discovers an available (type,region) by spec and provisions a
// tagged instance with the given cloud-init user-data.
func (l *Linode) CreateServer(ctx context.Context, spec provider.ServerSpec) (provider.Server, error) {
	minCores, minRAM := spec.MinCores, spec.MinRAMGB
	if minCores == 0 {
		minCores = 2
	}
	if minRAM == 0 {
		minRAM = 2
	}
	image := spec.Image
	if image == "" {
		image = defaultImage
	}

	types, err := l.types(ctx)
	if err != nil {
		return provider.Server{}, err
	}

	// candidate types (cheapest-first)
	var cands []typeInfo
	if spec.Type != "" {
		for _, t := range types {
			if t.ID == spec.Type {
				cands = []typeInfo{t}
				break
			}
		}
		if len(cands) == 0 {
			return provider.Server{}, fmt.Errorf("type %q not found", spec.Type)
		}
	} else {
		cands = selectTypes(types, minCores, minRAM*1024)
		if len(cands) == 0 {
			return provider.Server{}, fmt.Errorf("no type matches spec (>=%dvcpu/%dGB)", minCores, minRAM)
		}
	}

	// cloud-init user-data rides Metadata, which needs the "Metadata" capability.
	needMetadata := spec.UserData != ""
	regions, err := l.candidateRegions(ctx, needMetadata)
	if err != nil {
		return provider.Server{}, err
	}
	pref := l.regionPref
	if len(spec.RegionPref) > 0 {
		pref = spec.RegionPref
	}
	regions = orderRegions(regions, pref)

	var authKeys []string
	if spec.LoginPubKey != "" {
		authKeys = []string{strings.TrimSpace(spec.LoginPubKey)}
	}
	rootPass, err := randomRootPass()
	if err != nil {
		return provider.Server{}, err
	}
	var meta *linodego.InstanceMetadataOptions
	if needMetadata {
		meta = &linodego.InstanceMetadataOptions{
			UserData: base64.StdEncoding.EncodeToString([]byte(spec.UserData)),
		}
	}
	tags := []string{tagAll, clusterTag(spec.ClusterID)}

	// search (region, type): RegionFirst — try a region's cheapest types before
	// moving on (keeps nodes close; proximity over a marginal price delta).
	var lastErr error
	for _, region := range regions {
		for _, t := range cands {
			inst, cerr := l.c.CreateInstance(ctx, linodego.InstanceCreateOptions{
				Region:         region,
				Type:           t.ID,
				Label:          instanceLabel(spec.ClusterID, spec.Name),
				Image:          image,
				RootPass:       rootPass,
				AuthorizedKeys: authKeys,
				Tags:           tags,
				Metadata:       meta,
				Booted:         boolPtr(true),
			})
			if cerr != nil {
				lastErr = cerr
				if isAvailabilityErr(cerr) {
					continue // try the next (region,type)
				}
				return provider.Server{}, fmt.Errorf("create %s@%s: %w", t.ID, region, cerr)
			}
			return l.waitRunning(ctx, inst.ID, spec.ClusterID)
		}
	}
	return provider.Server{}, fmt.Errorf("no available type/region for spec; last error: %v", lastErr)
}

func (l *Linode) waitRunning(ctx context.Context, id int, clusterID string) (provider.Server, error) {
	deadline := time.Now().Add(4 * time.Minute)
	for {
		inst, err := l.c.GetInstance(ctx, id)
		if err != nil {
			return provider.Server{}, err
		}
		if inst.Status == linodego.InstanceRunning && publicIP(inst.IPv4) != "" {
			return toServer(inst, clusterID), nil
		}
		if time.Now().After(deadline) {
			return provider.Server{}, fmt.Errorf("instance %d not running within timeout", id)
		}
		select {
		case <-ctx.Done():
			return provider.Server{}, ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
}

// DestroyServer deletes by id. Idempotent: a missing instance (404) is success (H7).
func (l *Linode) DestroyServer(ctx context.Context, id string) error {
	lid, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("invalid linode id %q: %w", id, err)
	}
	if derr := l.c.DeleteInstance(ctx, lid); derr != nil {
		if isNotFoundErr(derr) {
			return nil // already gone
		}
		return derr
	}
	return nil
}

// ListByTag returns all instances for a cluster — the reconcile source of truth (C4).
func (l *Linode) ListByTag(ctx context.Context, clusterID string) ([]provider.Server, error) {
	insts, err := l.instancesByTag(ctx, clusterTag(clusterID))
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
func (l *Linode) ListAllTagged(ctx context.Context) ([]provider.Server, error) {
	insts, err := l.instancesByTag(ctx, tagAll)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(insts))
	for i := range insts {
		out = append(out, toServer(&insts[i], recoverClusterID(insts[i].Tags)))
	}
	return out, nil
}

// HourlyPrice implements provider.Pricer: Linode type prices can vary by region,
// so we honor the per-region override. Unknown type -> zero Money (nil error).
func (l *Linode) HourlyPrice(ctx context.Context, typeID, region string) (provider.Money, error) {
	types, err := l.types(ctx)
	if err != nil {
		return provider.Money{}, err
	}
	for _, t := range types {
		if t.ID == typeID {
			h := t.hourlyIn(region)
			if h <= 0 {
				return provider.Money{}, nil
			}
			return provider.Money{Amount: h, Currency: "USD"}, nil
		}
	}
	return provider.Money{}, nil
}

// EstimateHourly implements provider.Pricer: price the type a spec would
// provision (explicit, else the cheapest spec-match) without creating anything.
func (l *Linode) EstimateHourly(ctx context.Context, spec provider.ServerSpec) (provider.Money, error) {
	types, err := l.types(ctx)
	if err != nil {
		return provider.Money{}, err
	}
	region := ""
	if len(spec.RegionPref) > 0 {
		region = spec.RegionPref[0]
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
		cands := selectTypes(types, minCores, minRAM*1024)
		if len(cands) == 0 {
			return provider.Money{}, nil
		}
		id = cands[0].ID
	}
	return l.HourlyPrice(ctx, id, region)
}

// types returns the Linode type catalog (as typeInfo), fetched once and cached.
func (l *Linode) types(ctx context.Context) ([]typeInfo, error) {
	l.typeMu.Lock()
	defer l.typeMu.Unlock()
	if l.typeCache != nil {
		return l.typeCache, nil
	}
	all, err := l.c.ListTypes(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]typeInfo, 0, len(all))
	for _, t := range all {
		ti := typeInfo{
			ID: t.ID, VCPU: t.VCPUs, MemMB: t.Memory,
			GPU: t.GPUs > 0 || t.Class == "gpu" || t.AcceleratedDevices > 0,
		}
		if t.Price != nil {
			ti.HourlyUSD = float64(t.Price.Hourly)
		}
		if len(t.RegionPrices) > 0 {
			ti.RegionPrice = make(map[string]float64, len(t.RegionPrices))
			for _, rp := range t.RegionPrices {
				ti.RegionPrice[rp.ID] = float64(rp.Hourly)
			}
		}
		out = append(out, ti)
	}
	l.typeCache = out
	return out, nil
}

// candidateRegions returns the ids of regions that can host a Linode (and support
// Metadata, when cloud-init user-data is in play), fetched once and cached.
func (l *Linode) candidateRegions(ctx context.Context, needMetadata bool) ([]string, error) {
	regs, err := l.regions(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range regs {
		if r.Status != "ok" || !contains(r.Capabilities, capLinodes) {
			continue
		}
		if needMetadata && !contains(r.Capabilities, capMetadata) {
			continue
		}
		out = append(out, r.ID)
	}
	if len(out) == 0 {
		return nil, errors.New("no region supports the required capabilities")
	}
	return out, nil
}

func (l *Linode) regions(ctx context.Context) ([]linodego.Region, error) {
	l.regMu.Lock()
	defer l.regMu.Unlock()
	if l.regCache != nil {
		return l.regCache, nil
	}
	regs, err := l.c.ListRegions(ctx, nil)
	if err != nil {
		return nil, err
	}
	l.regCache = regs
	return regs, nil
}

// instancesByTag lists all Pandion instances carrying tag (client-side filtered).
func (l *Linode) instancesByTag(ctx context.Context, tag string) ([]linodego.Instance, error) {
	all, err := l.c.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []linodego.Instance
	for i := range all {
		if contains(all[i].Tags, tag) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

func toServer(inst *linodego.Instance, clusterID string) provider.Server {
	ps := provider.Server{
		ID:        strconv.Itoa(inst.ID),
		Name:      inst.Label,
		ClusterID: clusterID,
		Type:      inst.Type,
		Region:    inst.Region,
		IP:        publicIP(inst.IPv4),
	}
	if inst.Created != nil {
		ps.Created = *inst.Created
	}
	return ps
}

// publicIP returns the first routable (non-private) IPv4 of an instance.
func publicIP(ips []*net.IP) string {
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		return ip.String()
	}
	return ""
}

// isAvailabilityErr reports a "not orderable here / sold out" error (try the next
// region,type), versus a real failure.
func isAvailabilityErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not available") ||
		strings.Contains(m, "unavailable") ||
		strings.Contains(m, "sold out") ||
		strings.Contains(m, "no capacity") ||
		strings.Contains(m, "out of capacity") ||
		strings.Contains(m, "not currently accepting")
}

// isNotFoundErr reports that a resource is already gone (idempotent destroy).
func isNotFoundErr(err error) bool {
	var le *linodego.Error
	if errors.As(err, &le) && le.Code == 404 {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

// randomRootPass returns a strong random root password. It is never used for
// login (SSH key only) but Linode requires one when deploying from an image.
func randomRootPass() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*-_"
	b := make([]byte, 32)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b), nil
}

func boolPtr(b bool) *bool { return &b }
