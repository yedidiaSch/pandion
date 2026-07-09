// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lambda implements provider.Provider + provider.GPUProvider +
// provider.Pricer for Lambda Cloud (https://cloud.lambdalabs.com) — a Tier-A GPU
// cloud (see docs/gpu-design.md §5): full VMs whose base image (Lambda Stack) is
// already CUDA-native, so Pandion's kernel-space overlay + eBPF/XDP lockdown work
// unchanged and no driver injection is needed.
//
// Two Lambda-specific shapes drive the design:
//   - Lambda has NO server labels/tags. Cluster membership (C4 reconcile) is
//     encoded in the instance NAME ("pandion-<cluster>--<node>") and recovered by
//     splitting on the "--" delimiter, since both halves are hyphen-sanitized.
//   - Lambda only offers GPU instances and the OS image is fixed (Lambda Stack),
//     so CreateServer requires a GPU request and ignores spec.Image.
package lambda

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// defaultBaseURL is the Lambda Cloud API v1 root. Overridable in tests.
const defaultBaseURL = "https://cloud.lambdalabs.com/api/v1"

var nonDNS = regexp.MustCompile(`[^a-z0-9]+`)

// namePrefix tags every Pandion instance (there are no labels to select on).
const namePrefix = "pandion"

// nameSep separates the cluster id from the node in an instance name. Both halves
// are sanitized so no other "--" can appear, making the split unambiguous.
const nameSep = "--"

// serverName encodes cluster membership into the instance name, since Lambda has
// no labels. sanitize collapses any run of non-[a-z0-9] to a single "-", so the
// only "--" in the result is nameSep.
func serverName(clusterID, node string) string {
	return fmt.Sprintf("%s-%s%s%s", namePrefix, sanitize(clusterID), nameSep, sanitize(node))
}

// clusterOf recovers the cluster id from an instance name, or "" if the name is
// not a Pandion-managed instance.
func clusterOf(name string) string {
	rest, ok := strings.CutPrefix(name, namePrefix+"-")
	if !ok {
		return ""
	}
	cid, _, ok := strings.Cut(rest, nameSep)
	if !ok {
		return ""
	}
	return cid
}

// sanitize lowercases and reduces to [a-z0-9-] with single hyphens, no edges.
func sanitize(s string) string {
	s = nonDNS.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

// Lambda is a provider.Provider backed by Lambda Cloud.
type Lambda struct {
	apiKey     string
	baseURL    string
	http       *http.Client
	regionPref []string
}

// Option configures a Lambda provider.
type Option func(*Lambda)

// WithBaseURL overrides the API root (tests point this at an httptest server).
func WithBaseURL(u string) Option { return func(l *Lambda) { l.baseURL = strings.TrimRight(u, "/") } }

// WithHTTPClient overrides the HTTP client (tests inject a stub transport).
func WithHTTPClient(c *http.Client) Option { return func(l *Lambda) { l.http = c } }

// WithRegionPref sets preferred regions, in priority order.
func WithRegionPref(regions ...string) Option {
	return func(l *Lambda) {
		if len(regions) > 0 {
			l.regionPref = regions
		}
	}
}

// New returns a Lambda provider. apiKey is a Lambda Cloud API key (Basic auth,
// key as the username).
func New(apiKey string, opts ...Option) *Lambda {
	l := &Lambda{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Name implements provider.Provider.
func (l *Lambda) Name() string { return "lambda" }

// CreateServer launches a GPU instance. Lambda is GPU-only: the request must
// carry a GPU (or an exact type). The CUDA-native image is inherent (Lambda
// Stack), so spec.Image is ignored. spec.UserData is passed as cloud-init.
func (l *Lambda) CreateServer(ctx context.Context, spec provider.ServerSpec) (provider.Server, error) {
	if !spec.GPU.Wanted() && spec.Type == "" {
		return provider.Server{}, fmt.Errorf("lambda only provisions GPU instances — pass --gpu MODEL (see `pandion list-gpus --provider lambda`)")
	}

	// resolve the (type, region) to launch: an explicit --size wins, else discover
	// the cheapest available SKU satisfying the GPU request.
	typeName, region := spec.Type, ""
	if typeName == "" {
		var err error
		typeName, region, err = l.ResolveGPUType(ctx, spec.GPU, spec.RegionPref)
		if err != nil {
			return provider.Server{}, err
		}
	} else {
		off, err := l.offeringFor(ctx, typeName)
		if err != nil {
			return provider.Server{}, err
		}
		region = pickRegion(off.Regions, spec.RegionPref)
		if region == "" {
			return provider.Server{}, fmt.Errorf("lambda: type %q has no region with capacity", typeName)
		}
	}

	// ensure a registered SSH key holds the login public key (launch needs a name).
	keyNames, err := l.ensureLoginKey(ctx, spec.ClusterID, spec.LoginPubKey)
	if err != nil {
		return provider.Server{}, err
	}

	body := launchReq{
		RegionName:       region,
		InstanceTypeName: typeName,
		SSHKeyNames:      keyNames,
		Name:             serverName(spec.ClusterID, spec.Name),
		Quantity:         1,
		UserData:         spec.UserData,
	}
	var lr struct {
		Data struct {
			InstanceIDs []string `json:"instance_ids"`
		} `json:"data"`
	}
	if err := l.do(ctx, http.MethodPost, "/instance-operations/launch", body, &lr); err != nil {
		return provider.Server{}, fmt.Errorf("launch: %w", err)
	}
	if len(lr.Data.InstanceIDs) == 0 {
		return provider.Server{}, fmt.Errorf("lambda: launch returned no instance id")
	}
	return l.waitRunning(ctx, lr.Data.InstanceIDs[0], spec.ClusterID)
}

// waitRunning polls until the instance is active with an IP, then returns it.
func (l *Lambda) waitRunning(ctx context.Context, id, clusterID string) (provider.Server, error) {
	for {
		inst, err := l.getInstance(ctx, id)
		if err != nil {
			return provider.Server{}, err
		}
		if strings.EqualFold(inst.Status, "active") && inst.IP != "" {
			return toServer(inst, clusterID), nil
		}
		if strings.EqualFold(inst.Status, "terminated") || strings.EqualFold(inst.Status, "error") {
			return provider.Server{}, fmt.Errorf("lambda: instance %s entered status %q while booting", id, inst.Status)
		}
		select {
		case <-ctx.Done():
			return provider.Server{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// DestroyServer terminates an instance by id. Idempotent: an already-absent
// instance is success (Lambda returns the id in "terminated_instances" or a
// not-found error, both treated as gone).
func (l *Lambda) DestroyServer(ctx context.Context, id string) error {
	err := l.do(ctx, http.MethodPost, "/instance-operations/terminate",
		terminateReq{InstanceIDs: []string{id}}, nil)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

// ListByTag returns all instances for a cluster — the reconcile source of truth
// (C4). Lambda has no labels, so it filters live instances by the name prefix.
func (l *Lambda) ListByTag(ctx context.Context, clusterID string) ([]provider.Server, error) {
	insts, err := l.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(insts))
	for _, in := range insts {
		if clusterOf(in.Name) == sanitize(clusterID) {
			out = append(out, toServer(in, clusterID))
		}
	}
	return out, nil
}

// ListAllTagged returns every Pandion-managed instance (any cluster) — the
// reaper's source of truth. Selects on the name prefix.
func (l *Lambda) ListAllTagged(ctx context.Context) ([]provider.Server, error) {
	insts, err := l.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(insts))
	for _, in := range insts {
		if cid := clusterOf(in.Name); cid != "" {
			out = append(out, toServer(in, cid))
		}
	}
	return out, nil
}

// toServer maps a Lambda instance to a provider.Server, deriving the GPU from the
// instance type name/description.
func toServer(in instance, clusterID string) provider.Server {
	gpu, _ := parseGPU(in.InstanceType.Name, in.InstanceType.Description)
	return provider.Server{
		ID:        in.ID,
		Name:      in.Name,
		ClusterID: clusterID,
		Type:      in.InstanceType.Name,
		Region:    in.Region.Name,
		IP:        in.IP,
		GPU:       gpu,
	}
}

// pickRegion returns the first preferred region present in avail, else the first
// available region.
func pickRegion(avail, pref []string) string {
	for _, p := range pref {
		for _, a := range avail {
			if a == p {
				return a
			}
		}
	}
	if len(avail) > 0 {
		return avail[0]
	}
	return ""
}

// --- low-level API ---------------------------------------------------------

type launchReq struct {
	RegionName       string   `json:"region_name"`
	InstanceTypeName string   `json:"instance_type_name"`
	SSHKeyNames      []string `json:"ssh_key_names"`
	Name             string   `json:"name,omitempty"`
	Quantity         int      `json:"quantity,omitempty"`
	UserData         string   `json:"user_data,omitempty"`
}

type terminateReq struct {
	InstanceIDs []string `json:"instance_ids"`
}

// instance is a Lambda instance as returned by GET /instances[/{id}].
type instance struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IP           string `json:"ip"`
	Status       string `json:"status"`
	Region       region `json:"region"`
	InstanceType itype  `json:"instance_type"`
}

type region struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type itype struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	PriceCentsPerHr int    `json:"price_cents_per_hour"`
	Specs           specs  `json:"specs"`
}

type specs struct {
	VCPUs     int `json:"vcpus"`
	MemoryGiB int `json:"memory_gib"`
	GPUs      int `json:"gpus"`
}

func (l *Lambda) getInstance(ctx context.Context, id string) (instance, error) {
	var r struct {
		Data instance `json:"data"`
	}
	if err := l.do(ctx, http.MethodGet, "/instances/"+id, nil, &r); err != nil {
		return instance{}, err
	}
	return r.Data, nil
}

func (l *Lambda) listInstances(ctx context.Context) ([]instance, error) {
	var r struct {
		Data []instance `json:"data"`
	}
	if err := l.do(ctx, http.MethodGet, "/instances", nil, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

// apiError is a non-2xx Lambda API error body.
type apiError struct {
	Status int
	Code   string
	Msg    string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("lambda api %d (%s): %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("lambda api %d: %s", e.Status, e.Msg)
}

func isNotFound(err error) bool {
	var ae *apiError
	if e, ok := err.(*apiError); ok {
		ae = e
	}
	if ae == nil {
		return false
	}
	return ae.Status == http.StatusNotFound || strings.Contains(ae.Code, "not-found")
}

// do performs an authenticated JSON request. body (if non-nil) is sent as JSON;
// out (if non-nil) receives the decoded response. Auth is HTTP Basic with the
// API key as the username (Lambda's documented scheme).
func (l *Lambda) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.SetBasicAuth(l.apiKey, "")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &apiError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(data))}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &body) == nil && body.Error.Message != "" {
			ae.Code, ae.Msg = body.Error.Code, body.Error.Message
		}
		return ae
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

var _ provider.Provider = (*Lambda)(nil)
var _ provider.GPUProvider = (*Lambda)(nil)
var _ provider.Pricer = (*Lambda)(nil)
