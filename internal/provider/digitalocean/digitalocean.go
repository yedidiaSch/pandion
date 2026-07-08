// SPDX-License-Identifier: AGPL-3.0-or-later

// Package digitalocean implements provider.Provider against the DigitalOcean API
// using godo. It mirrors the Hetzner backend's discipline — sizes are discovered
// by SPEC at runtime (never hardcoded), creation searches size x region for an
// available combination — proving the Provider seam with a second backend (M6).
//
// DO's model is cleaner than Hetzner's in three ways this code leans on: tags are
// first-class (so reconcile is a tag query, not a label selector), each Size
// carries its hourly price (so the Pricer is exact), and each Size lists the
// regions where it is orderable (so availability is mostly known up front).
package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/godo"
	"github.com/yedidiaSch/pandion/internal/provider"
)

const (
	// tagAll marks every Pandion droplet (ListAllTagged / reaper source of truth).
	tagAll = "pandion"
	// tagCIDPrefix + sanitized cluster id is the per-cluster tag (ListByTag).
	tagCIDPrefix = "pandion-cid-"
	defaultImage = "ubuntu-24-04-x64"
	loginKeyName = "pandion-login-" // + sanitized cluster id
)

// DO tag/name chars: lowercase letters, digits, dashes.
var nonTag = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitize(s string) string {
	return strings.Trim(nonTag.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func clusterTag(clusterID string) string { return tagCIDPrefix + sanitize(clusterID) }

// dropletName namespaces the droplet by cluster id so names never collide across
// clusters/retries. Reconciliation keys off the TAG, not the name (C4).
func dropletName(clusterID, node string) string {
	s := sanitize("pandion-" + clusterID + "-" + node)
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

// recoverClusterID reads the (sanitized) cluster id back out of a droplet's tags.
func recoverClusterID(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, tagCIDPrefix) {
			return strings.TrimPrefix(t, tagCIDPrefix)
		}
	}
	return ""
}

// DO is a provider.Provider backed by DigitalOcean.
type DO struct {
	c          *godo.Client
	regionPref []string

	keyMu     sync.Mutex // serializes get-or-create of the shared login key
	sizeMu    sync.Mutex // guards the size cache
	sizeCache []sizeInfo
}

// Option configures a DO provider.
type Option func(*DO)

// WithRegionPref overrides the preferred regions, in priority order.
func WithRegionPref(regions ...string) Option {
	return func(d *DO) {
		if len(regions) > 0 {
			d.regionPref = regions
		}
	}
}

// New returns a DigitalOcean provider. token must be a personal access token
// with write scope.
func New(token string, opts ...Option) *DO {
	d := &DO{
		c:          godo.NewFromToken(token),
		regionPref: []string{"nyc3", "fra1", "ams3", "sfo3"},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Name implements provider.Provider.
func (d *DO) Name() string { return "digitalocean" }

// CreateServer discovers an available (size,region) by spec and provisions a
// tagged droplet with the given cloud-init user-data.
func (d *DO) CreateServer(ctx context.Context, spec provider.ServerSpec) (provider.Server, error) {
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

	sizes, err := d.sizes(ctx)
	if err != nil {
		return provider.Server{}, err
	}

	// candidate sizes (cheapest-first)
	var cands []sizeInfo
	if spec.Type != "" {
		for _, s := range sizes {
			if s.Slug == spec.Type {
				cands = []sizeInfo{s}
				break
			}
		}
		if len(cands) == 0 {
			return provider.Server{}, fmt.Errorf("size %q not found", spec.Type)
		}
	} else {
		cands = selectSizes(sizes, minCores, minRAM*1024, "")
		if len(cands) == 0 {
			return provider.Server{}, fmt.Errorf("no size matches spec (>=%dvcpu/%dGB)", minCores, minRAM)
		}
	}

	pref := d.regionPref
	if len(spec.RegionPref) > 0 {
		pref = spec.RegionPref
	}
	regions := orderRegions(regionsOf(cands), pref)

	// register the login key so it lands in root's authorized_keys.
	var sshKeys []godo.DropletCreateSSHKey
	if spec.LoginPubKey != "" {
		k, err := d.ensureLoginKey(ctx, spec.ClusterID, spec.LoginPubKey)
		if err != nil {
			return provider.Server{}, fmt.Errorf("register login ssh key: %w", err)
		}
		sshKeys = []godo.DropletCreateSSHKey{{ID: k.ID}}
	}
	tags := []string{tagAll, clusterTag(spec.ClusterID)}

	// search (region, size): RegionFirst — exhaust a region's cheapest sizes
	// before moving on (keeps nodes close; proximity over a marginal price delta).
	var lastErr error
	for _, region := range regions {
		for _, s := range cands {
			if !contains(s.Regions, region) {
				continue
			}
			dr, _, cerr := d.c.Droplets.Create(ctx, &godo.DropletCreateRequest{
				Name:     dropletName(spec.ClusterID, spec.Name),
				Region:   region,
				Size:     s.Slug,
				Image:    godo.DropletCreateImage{Slug: image},
				SSHKeys:  sshKeys,
				Tags:     tags,
				UserData: spec.UserData,
			})
			if cerr != nil {
				lastErr = cerr
				if isAvailabilityErr(cerr) {
					continue // try the next (region,size)
				}
				return provider.Server{}, fmt.Errorf("create %s@%s: %w", s.Slug, region, cerr)
			}
			return d.waitActive(ctx, dr.ID, spec.ClusterID)
		}
	}
	return provider.Server{}, fmt.Errorf("no available size/region for spec; last error: %v", lastErr)
}

func (d *DO) waitActive(ctx context.Context, id int, clusterID string) (provider.Server, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		dr, _, err := d.c.Droplets.Get(ctx, id)
		if err != nil {
			return provider.Server{}, err
		}
		if ip, _ := dr.PublicIPv4(); dr.Status == "active" && ip != "" {
			return toServer(dr, clusterID), nil
		}
		if time.Now().After(deadline) {
			return provider.Server{}, fmt.Errorf("droplet %d not active within timeout", id)
		}
		select {
		case <-ctx.Done():
			return provider.Server{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// DestroyServer deletes by id. Idempotent: a missing droplet (404) is success (H7).
func (d *DO) DestroyServer(ctx context.Context, id string) error {
	did, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("invalid droplet id %q: %w", id, err)
	}
	if _, derr := d.c.Droplets.Delete(ctx, did); derr != nil {
		var er *godo.ErrorResponse
		if errors.As(derr, &er) && er.Response != nil && er.Response.StatusCode == 404 {
			return nil // already gone
		}
		return derr
	}
	return nil
}

// ListByTag returns all droplets for a cluster — the reconcile source of truth (C4).
func (d *DO) ListByTag(ctx context.Context, clusterID string) ([]provider.Server, error) {
	drs, err := d.dropletsByTag(ctx, clusterTag(clusterID))
	if err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(drs))
	for i := range drs {
		out = append(out, toServer(&drs[i], clusterID))
	}
	return out, nil
}

// ListAllTagged returns every Pandion-tagged droplet (any cluster) — the reaper's
// source of truth. Recovers each droplet's cluster id from its per-cluster tag.
func (d *DO) ListAllTagged(ctx context.Context) ([]provider.Server, error) {
	drs, err := d.dropletsByTag(ctx, tagAll)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(drs))
	for i := range drs {
		out = append(out, toServer(&drs[i], recoverClusterID(drs[i].Tags)))
	}
	return out, nil
}

// ReapAux deletes the cluster's shared login key (DO keys carry no tags, so we
// match by our deterministic per-cluster name). Implements provider.AuxReaper.
func (d *DO) ReapAux(ctx context.Context, clusterID string) error {
	name := loginKeyName + sanitize(clusterID)
	keys, err := d.allKeys(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for i := range keys {
		if keys[i].Name == name {
			if _, derr := d.c.Keys.DeleteByID(ctx, keys[i].ID); derr != nil && firstErr == nil {
				firstErr = derr
			}
		}
	}
	return firstErr
}

// HourlyPrice implements provider.Pricer: DO sizes carry the hourly price
// directly (in USD), so this is exact. region is irrelevant (price is uniform
// per size across regions). Unknown size -> zero Money (nil error).
func (d *DO) HourlyPrice(ctx context.Context, sizeSlug, _ string) (provider.Money, error) {
	sizes, err := d.sizes(ctx)
	if err != nil {
		return provider.Money{}, err
	}
	for _, s := range sizes {
		if s.Slug == sizeSlug {
			if s.PriceHourly <= 0 {
				return provider.Money{}, nil
			}
			return provider.Money{Amount: s.PriceHourly, Currency: "USD"}, nil
		}
	}
	return provider.Money{}, nil
}

// EstimateHourly implements provider.Pricer: price the size a spec would
// provision (explicit, else the cheapest spec-match) without creating anything.
func (d *DO) EstimateHourly(ctx context.Context, spec provider.ServerSpec) (provider.Money, error) {
	sizes, err := d.sizes(ctx)
	if err != nil {
		return provider.Money{}, err
	}
	slug := spec.Type
	if slug == "" {
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
		cands := selectSizes(sizes, minCores, minRAM*1024, region)
		if len(cands) == 0 {
			return provider.Money{}, nil
		}
		slug = cands[0].Slug
	}
	return d.HourlyPrice(ctx, slug, "")
}

// ensureLoginKey get-or-creates the cluster's shared login key by a deterministic
// name, tolerating concurrent callers and DO's dedup-by-material (a duplicate
// public key returns an error, so we refetch and match).
func (d *DO) ensureLoginKey(ctx context.Context, clusterID, pubKey string) (*godo.Key, error) {
	name := loginKeyName + sanitize(clusterID)
	d.keyMu.Lock()
	defer d.keyMu.Unlock()

	if k := d.findKey(ctx, name, pubKey); k != nil {
		return k, nil
	}
	k, _, cerr := d.c.Keys.Create(ctx, &godo.KeyCreateRequest{Name: name, PublicKey: pubKey})
	if cerr == nil {
		return k, nil
	}
	// lost a race, or the key material already exists under another name — refetch.
	if k := d.findKey(ctx, name, pubKey); k != nil {
		return k, nil
	}
	return nil, cerr
}

// findKey returns a registered key matching either our name or the key material.
func (d *DO) findKey(ctx context.Context, name, pubKey string) *godo.Key {
	keys, err := d.allKeys(ctx)
	if err != nil {
		return nil
	}
	want := keyMaterial(pubKey)
	for i := range keys {
		if keys[i].Name == name || keyMaterial(keys[i].PublicKey) == want {
			return &keys[i]
		}
	}
	return nil
}

// sizes returns the DO size catalog (as sizeInfo), fetched once and cached.
func (d *DO) sizes(ctx context.Context) ([]sizeInfo, error) {
	d.sizeMu.Lock()
	defer d.sizeMu.Unlock()
	if d.sizeCache != nil {
		return d.sizeCache, nil
	}
	var all []godo.Size
	opt := &godo.ListOptions{PerPage: 200}
	for {
		page, resp, err := d.c.Sizes.List(ctx, opt)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		cur, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, err
		}
		opt.Page = cur + 1
	}
	out := make([]sizeInfo, 0, len(all))
	for _, s := range all {
		out = append(out, sizeInfo{
			Slug: s.Slug, Vcpus: s.Vcpus, MemMB: s.Memory, PriceHourly: s.PriceHourly,
			Regions: s.Regions, Available: s.Available, GPU: s.GPUInfo != nil,
		})
	}
	d.sizeCache = out
	return out, nil
}

func (d *DO) dropletsByTag(ctx context.Context, tag string) ([]godo.Droplet, error) {
	var all []godo.Droplet
	opt := &godo.ListOptions{PerPage: 200}
	for {
		page, resp, err := d.c.Droplets.ListByTag(ctx, tag, opt)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		cur, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, err
		}
		opt.Page = cur + 1
	}
	return all, nil
}

func (d *DO) allKeys(ctx context.Context) ([]godo.Key, error) {
	var all []godo.Key
	opt := &godo.ListOptions{PerPage: 200}
	for {
		page, resp, err := d.c.Keys.List(ctx, opt)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		cur, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, err
		}
		opt.Page = cur + 1
	}
	return all, nil
}

func toServer(dr *godo.Droplet, clusterID string) provider.Server {
	ps := provider.Server{
		ID:        strconv.Itoa(dr.ID),
		Name:      dr.Name,
		ClusterID: clusterID,
		Type:      dr.SizeSlug,
	}
	if dr.Region != nil {
		ps.Region = dr.Region.Slug
	}
	if ip, err := dr.PublicIPv4(); err == nil {
		ps.IP = ip
	}
	if t, err := time.Parse(time.RFC3339, dr.Created); err == nil {
		ps.Created = t
	}
	return ps
}

// isAvailabilityErr reports a "not orderable here / sold out" error (try the next
// region,size), versus a real failure.
func isAvailabilityErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not available") ||
		strings.Contains(m, "unavailable") ||
		strings.Contains(m, "sold out") ||
		strings.Contains(m, "no capacity")
}

// keyMaterial extracts the base64 body of an SSH public key so keys compare by
// material, ignoring name/comment.
func keyMaterial(pub string) string {
	if f := strings.Fields(strings.TrimSpace(pub)); len(f) >= 2 {
		return f[1]
	}
	return strings.TrimSpace(pub)
}
