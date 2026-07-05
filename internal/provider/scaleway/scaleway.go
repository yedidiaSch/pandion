// Package scaleway implements provider.Provider against the Scaleway Instances
// API. It mirrors the DigitalOcean/Hetzner backends' discipline — commercial types
// are discovered by SPEC at runtime (never hardcoded), creation searches type x
// zone for an available combination — proving the Provider seam with a fourth
// backend and a first European home for the H6 payment-flexible providers.
//
// Scaleway is the most different of Pandion's backends, and this file absorbs the
// differences so the rest of the system doesn't have to:
//
//   - Credentials are an access-key + secret-key + project-id triple (not a single
//     token). New() takes all three; the secret key is the sensitive one that flows
//     through `pandion login`, the other two are non-secret identifiers from env.
//   - Locations are ZONES (fr-par-1, nl-ams-1, pl-waw-1), surfaced through the seam
//     as regions. Types and prices are per-zone, so we cache per zone.
//   - Boot is two-phase: CreateServer yields a STOPPED server, then we attach the
//     cloud-init user-data and power it on. The login key needs no provider-native
//     registration — it already rides the cloud-init user-data (ssh_authorized_keys).
//   - Teardown must be leak-free (C4): the `terminate` action deletes local volumes
//     but only DETACHES block (sbs) volumes, so we explicitly delete the server's
//     leftover volumes afterwards. Dynamic IPs are released automatically on delete.
package scaleway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"github.com/yedidiaSch/pandion/internal/provider"
)

// ConfigFileExists reports whether a Scaleway CLI config file is present. The
// SDK's WithEnv also loads credentials from that file, so a caller can use this
// to decide whether missing SCW_* environment variables are actually fatal
// (env-only setups) or fine (the config file supplies them).
func ConfigFileExists() bool {
	if p := strings.TrimSpace(os.Getenv("SCW_CONFIG_PATH")); p != "" {
		_, err := os.Stat(p)
		return err == nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".config", "scw", "config.yaml"))
	return err == nil
}

const (
	// tagAll marks every Pandion instance (ListAllTagged / reaper source of truth).
	tagAll = "pandion"
	// tagCIDPrefix + sanitized cluster id is the per-cluster tag (ListByTag).
	tagCIDPrefix = "pandion-cid-"
	// defaultImage is a marketplace label; the SDK resolves it to a per-zone image.
	defaultImage = "ubuntu_noble" // Ubuntu 24.04 LTS
)

// Scaleway tag/name chars: keep to lowercase letters, digits, dashes.
var nonTag = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitize(s string) string {
	return strings.Trim(nonTag.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func clusterTag(clusterID string) string { return tagCIDPrefix + sanitize(clusterID) }

// instanceName namespaces the instance by cluster id so names never collide across
// clusters/retries. Reconciliation keys off the TAG, not the name (C4).
func instanceName(clusterID, node string) string {
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

// Scaleway is a provider.Provider backed by the Scaleway Instances API.
type Scaleway struct {
	api   *instance.API
	zones []string

	typeMu    sync.Mutex // guards the per-zone type cache
	typeCache map[string][]typeInfo
}

// Option configures a Scaleway provider.
type Option func(*Scaleway)

// WithZones overrides the preferred zones, in priority order.
func WithZones(zones ...string) Option {
	return func(s *Scaleway) {
		if len(zones) > 0 {
			s.zones = zones
		}
	}
}

// New returns a Scaleway provider. secretKey is the (sensitive) API secret key;
// accessKey and projectID are non-secret identifiers. Any of them may be empty to
// fall back to the standard Scaleway env/config (SCW_*), which WithEnv loads.
func New(secretKey, accessKey, projectID string, opts ...Option) (*Scaleway, error) {
	copts := []scw.ClientOption{scw.WithEnv()}
	if accessKey != "" && secretKey != "" {
		copts = append(copts, scw.WithAuth(accessKey, secretKey))
	}
	if projectID != "" {
		copts = append(copts, scw.WithDefaultProjectID(projectID))
	}
	client, err := scw.NewClient(copts...)
	if err != nil {
		return nil, fmt.Errorf("scaleway client: %w", err)
	}
	s := &Scaleway{
		api:       instance.NewAPI(client),
		zones:     []string{"fr-par-1", "nl-ams-1", "pl-waw-1"},
		typeCache: map[string][]typeInfo{},
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Name implements provider.Provider.
func (s *Scaleway) Name() string { return "scaleway" }

// CreateServer discovers an available (type,zone) by spec, provisions a stopped
// instance, attaches the cloud-init user-data, powers it on and waits for its IP.
func (s *Scaleway) CreateServer(ctx context.Context, spec provider.ServerSpec) (provider.Server, error) {
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

	pref := s.zones
	if len(spec.RegionPref) > 0 {
		pref = spec.RegionPref
	}
	zones := orderZones(s.zones, pref)
	tags := []string{tagAll, clusterTag(spec.ClusterID)}

	// search (zone, type): ZoneFirst — exhaust a zone's cheapest types before
	// moving on (keeps nodes close; proximity over a marginal price delta).
	var lastErr error
	for _, z := range zones {
		zone := scw.Zone(z)
		types, err := s.zoneTypes(ctx, zone)
		if err != nil {
			lastErr = err
			continue
		}
		var cands []typeInfo
		if spec.Type != "" {
			for _, t := range types {
				if t.Name == spec.Type {
					cands = []typeInfo{t}
					break
				}
			}
		} else {
			cands = selectTypes(types, minCores, minRAM*1024)
		}
		for _, t := range cands {
			srv, cerr := s.api.CreateServer(&instance.CreateServerRequest{
				Zone:           zone,
				Name:           instanceName(spec.ClusterID, spec.Name),
				CommercialType: t.Name,
				Image:          scw.StringPtr(image),
				Tags:           tags,
			})
			if cerr != nil {
				lastErr = cerr
				if isAvailabilityErr(cerr) {
					continue // try the next (zone,type)
				}
				return provider.Server{}, fmt.Errorf("create %s@%s: %w", t.Name, z, cerr)
			}
			return s.finishBoot(ctx, zone, srv.Server.ID, spec)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no type matches spec (>=%dvcpu/%dGB) in any zone", minCores, minRAM)
	}
	return provider.Server{}, fmt.Errorf("no available type/zone for spec; last error: %v", lastErr)
}

// finishBoot attaches the cloud-init user-data to a freshly-created (stopped)
// server, powers it on, and waits until it is running with a public IP.
func (s *Scaleway) finishBoot(ctx context.Context, zone scw.Zone, id string, spec provider.ServerSpec) (provider.Server, error) {
	if spec.UserData != "" {
		if err := s.api.SetServerUserData(&instance.SetServerUserDataRequest{
			Zone:     zone,
			ServerID: id,
			Key:      "cloud-init",
			Content:  bytes.NewReader([]byte(spec.UserData)),
		}); err != nil {
			return provider.Server{}, fmt.Errorf("set user-data: %w", err)
		}
	}
	timeout := 5 * time.Minute
	if err := s.api.ServerActionAndWait(&instance.ServerActionAndWaitRequest{
		Zone:     zone,
		ServerID: id,
		Action:   instance.ServerActionPoweron,
		Timeout:  &timeout,
	}); err != nil {
		return provider.Server{}, fmt.Errorf("power on: %w", err)
	}
	// After power-on the server is running; fetch it once more for the assigned IP.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := s.api.GetServer(&instance.GetServerRequest{Zone: zone, ServerID: id})
		if err != nil {
			return provider.Server{}, err
		}
		if ps := toServer(resp.Server); ps.IP != "" {
			return ps, nil
		}
		if time.Now().After(deadline) {
			return provider.Server{}, fmt.Errorf("server %s has no public IP within timeout", id)
		}
		select {
		case <-ctx.Done():
			return provider.Server{}, ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
}

// DestroyServer terminates the instance and deletes any leftover block volumes so
// nothing bills after teardown (C4). Idempotent: an already-absent server (across
// all zones) is success (H7).
func (s *Scaleway) DestroyServer(ctx context.Context, id string) error {
	zone, srv, found := s.locate(ctx, id)
	if !found {
		return nil // already gone
	}
	// Remember the volumes before termination — terminate deletes local (l_ssd)
	// volumes but only DETACHES block (sbs/b_ssd) ones, which would keep billing.
	leftovers := blockVolumeIDs(srv)

	timeout := 5 * time.Minute
	if err := s.api.ServerActionAndWait(&instance.ServerActionAndWaitRequest{
		Zone:     zone,
		ServerID: id,
		Action:   instance.ServerActionTerminate,
		Timeout:  &timeout,
	}); err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("terminate %s: %w", id, err)
	}
	var firstErr error
	for _, vid := range leftovers {
		if derr := s.api.DeleteVolume(&instance.DeleteVolumeRequest{Zone: zone, VolumeID: vid}); derr != nil && !isNotFoundErr(derr) && firstErr == nil {
			firstErr = derr
		}
	}
	return firstErr
}

// ListByTag returns all instances for a cluster — the reconcile source of truth (C4).
func (s *Scaleway) ListByTag(ctx context.Context, clusterID string) ([]provider.Server, error) {
	return s.listTagged(ctx, clusterTag(clusterID))
}

// ListAllTagged returns every Pandion-tagged instance (any cluster) — the reaper's
// source of truth. Recovers each instance's cluster id from its per-cluster tag.
func (s *Scaleway) ListAllTagged(ctx context.Context) ([]provider.Server, error) {
	return s.listTagged(ctx, tagAll)
}

// listTagged scans every zone for servers carrying tag.
func (s *Scaleway) listTagged(ctx context.Context, tag string) ([]provider.Server, error) {
	var out []provider.Server
	for _, z := range s.zones {
		zone := scw.Zone(z)
		page := int32(1)
		perPage := uint32(100)
		for {
			resp, err := s.api.ListServers(&instance.ListServersRequest{
				Zone: zone, Tags: []string{tag}, Page: &page, PerPage: &perPage,
			})
			if err != nil {
				return nil, err
			}
			for _, srv := range resp.Servers {
				if contains(srv.Tags, tag) {
					out = append(out, toServer(srv))
				}
			}
			if len(resp.Servers) < int(perPage) {
				break
			}
			page++
		}
	}
	return out, nil
}

// HourlyPrice implements provider.Pricer: Scaleway prices are per-zone EUR/hour.
// region is the zone. Unknown type -> zero Money (nil error).
func (s *Scaleway) HourlyPrice(ctx context.Context, commercialType, region string) (provider.Money, error) {
	zone := scw.Zone(region)
	if region == "" {
		zone = scw.Zone(s.zones[0])
	}
	types, err := s.zoneTypes(ctx, zone)
	if err != nil {
		return provider.Money{}, err
	}
	for _, t := range types {
		if t.Name == commercialType {
			if t.HourlyEUR <= 0 {
				return provider.Money{}, nil
			}
			return provider.Money{Amount: t.HourlyEUR, Currency: "EUR"}, nil
		}
	}
	return provider.Money{}, nil
}

// EstimateHourly implements provider.Pricer: price the type a spec would provision
// (explicit, else the cheapest spec-match) without creating anything.
func (s *Scaleway) EstimateHourly(ctx context.Context, spec provider.ServerSpec) (provider.Money, error) {
	region := s.zones[0]
	if len(spec.RegionPref) > 0 {
		region = spec.RegionPref[0]
	}
	types, err := s.zoneTypes(ctx, scw.Zone(region))
	if err != nil {
		return provider.Money{}, err
	}
	name := spec.Type
	if name == "" {
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
		name = cands[0].Name
	}
	return s.HourlyPrice(ctx, name, region)
}

// zoneTypes returns a zone's commercial type catalog (as typeInfo), cached per zone.
func (s *Scaleway) zoneTypes(ctx context.Context, zone scw.Zone) ([]typeInfo, error) {
	s.typeMu.Lock()
	defer s.typeMu.Unlock()
	if c, ok := s.typeCache[string(zone)]; ok {
		return c, nil
	}
	var out []typeInfo
	page := int32(1)
	perPage := uint32(100)
	for {
		resp, err := s.api.ListServersTypes(&instance.ListServersTypesRequest{
			Zone: zone, Page: &page, PerPage: &perPage,
		})
		if err != nil {
			return nil, err
		}
		for name, st := range resp.Servers {
			out = append(out, toTypeInfo(name, st))
		}
		if len(resp.Servers) < int(perPage) {
			break
		}
		page++
	}
	s.typeCache[string(zone)] = out
	return out, nil
}

// toTypeInfo maps a Scaleway ServerType to the provider-agnostic typeInfo.
func toTypeInfo(name string, st *instance.ServerType) typeInfo {
	ti := typeInfo{
		Name:      name,
		NCPUs:     int(st.Ncpus),
		RAMMB:     int(st.RAM / (1024 * 1024)),
		HourlyEUR: float64(st.HourlyPrice),
		EOS:       st.EndOfService,
	}
	if st.Gpu != nil && *st.Gpu > 0 {
		ti.GPU = true
	}
	return ti
}

// locate finds the zone and current state of a server by id, scanning all zones.
func (s *Scaleway) locate(ctx context.Context, id string) (scw.Zone, *instance.Server, bool) {
	for _, z := range s.zones {
		zone := scw.Zone(z)
		resp, err := s.api.GetServer(&instance.GetServerRequest{Zone: zone, ServerID: id})
		if err != nil {
			continue // not in this zone (or transient) — keep scanning
		}
		if resp != nil && resp.Server != nil {
			return zone, resp.Server, true
		}
	}
	return "", nil, false
}

func toServer(srv *instance.Server) provider.Server {
	ps := provider.Server{
		ID:        srv.ID,
		Name:      srv.Name,
		ClusterID: recoverClusterID(srv.Tags),
		Type:      srv.CommercialType,
		Region:    string(srv.Zone),
		IP:        publicIP(srv),
	}
	if srv.CreationDate != nil {
		ps.Created = *srv.CreationDate
	}
	return ps
}

// publicIP returns the server's routable IPv4 (dynamic or flexible).
func publicIP(srv *instance.Server) string {
	if srv.PublicIP != nil && len(srv.PublicIP.Address) > 0 {
		return srv.PublicIP.Address.String()
	}
	for _, ip := range srv.PublicIPs {
		if ip != nil && len(ip.Address) > 0 && ip.Family == instance.ServerIPIPFamilyInet {
			return ip.Address.String()
		}
	}
	return ""
}

// blockVolumeIDs returns the ids of a server's block (sbs/b_ssd) volumes, which
// survive `terminate` and must be deleted separately to avoid billing leaks.
func blockVolumeIDs(srv *instance.Server) []string {
	var out []string
	for _, v := range srv.Volumes {
		if v == nil {
			continue
		}
		if v.VolumeType == instance.VolumeServerVolumeTypeSbsVolume || v.VolumeType == instance.VolumeServerVolumeTypeBSSD {
			out = append(out, v.ID)
		}
	}
	return out
}

// isAvailabilityErr reports a "not orderable here / out of stock" error (try the
// next zone,type), versus a real failure.
func isAvailabilityErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "out of stock") ||
		strings.Contains(m, "no capacity") ||
		strings.Contains(m, "not available") ||
		strings.Contains(m, "unavailable") ||
		strings.Contains(m, "insufficient") ||
		strings.Contains(m, "shortage")
}

// isNotFoundErr reports that a resource is already gone (idempotent destroy).
func isNotFoundErr(err error) bool {
	var nf *scw.ResourceNotFoundError
	if errors.As(err, &nf) {
		return true
	}
	var re *scw.ResponseError
	if errors.As(err, &re) && re.StatusCode == 404 {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}
