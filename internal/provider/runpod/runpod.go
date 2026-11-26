// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runpod implements provider.Provider + provider.GPUProvider +
// provider.Pricer + provider.ContainerHost for RunPod GPU pods, through RunPod's
// REST API v2 (https://api.runpod.io/v2; spec: /v2/openapi.json).
//
// RunPod is unlike the VM providers in three ways that shape this backend:
//   - A pod is a CONTAINER. There is no cloud-init or systemd, so the node is
//     bootstrapped by a shell script run as the container command
//     (harden.PodBootstrap), and the VM hardening (nftables, WireGuard, idle
//     poweroff) is unavailable — `up` says so.
//   - sshd is reached at the pod's public IP on a MAPPED port (the pod's
//     ssh.direct block), not IP:22. Pandion still pins the host key: it generates
//     the key and the bootstrap installs it.
//   - Pods have no labels on create, so cluster membership is encoded in the pod
//     NAME ("pandion-<cluster>--<node>"), as for Lambda.
//
// Billing is per second while a pod exists. `stop` keeps the pod (and any volume
// disk, which bills); Pandion never stops — DestroyServer DELETEs (terminates).
package runpod

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yedidiaSch/pandion/internal/harden"
	"github.com/yedidiaSch/pandion/internal/provider"
)

// defaultBaseURL is the RunPod REST API root. Overridable in tests.
const defaultBaseURL = "https://api.runpod.io/v2"

// DefaultImage is the container image used when none is given: the CUDA devel
// image (nvcc, compute-sanitizer, cuda libs) on Ubuntu 24.04. Hosts are
// constrained to a driver that supports it (defaultMinCUDA).
const DefaultImage = "nvidia/cuda:12.8.1-devel-ubuntu24.04"

// defaultMinCUDA is the lowest host CUDA (driver) version DefaultImage runs on.
const defaultMinCUDA = "12.8"

// defaultDiskGB is the container disk: the CUDA devel image plus the toolchain
// and a workspace build fit comfortably.
const defaultDiskGB = 60

// Cloud tiers.
const (
	CloudSecure    = "SECURE"    // RunPod-owned data-center hardware (default)
	CloudCommunity = "COMMUNITY" // community-hosted hardware
)

const (
	namePrefix = "pandion"
	nameSep    = "--"
)

var nonDNS = regexp.MustCompile(`[^a-z0-9]+`)

func serverName(clusterID, node string) string {
	return fmt.Sprintf("%s-%s%s%s", namePrefix, sanitize(clusterID), nameSep, sanitize(node))
}

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

func sanitize(s string) string {
	return strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// RunPod is a provider.Provider backed by RunPod pods.
type RunPod struct {
	apiKey  string
	baseURL string
	http    *http.Client
	cloud   string

	mu          sync.Mutex
	lastReq     time.Time
	minGap      time.Duration
	maxRetry    int
	backoffBase time.Duration
	backoffMax  time.Duration

	catMu   sync.Mutex
	catalog map[int]catalogEntry // by GPU count
	// observed holds the hourly rate RunPod reports for live pods (pod.cost),
	// keyed by type|region, so `ls`/receipts use the rate actually billed.
	observed map[string]float64
}

type catalogEntry struct {
	at   time.Time
	gpus []gpuType
}

// Option configures a RunPod provider.
type Option func(*RunPod)

// WithBaseURL overrides the API root (tests point this at an httptest server).
func WithBaseURL(u string) Option { return func(r *RunPod) { r.baseURL = strings.TrimRight(u, "/") } }

// WithMinGap sets the minimum spacing between API requests (0 in tests).
func WithMinGap(d time.Duration) Option { return func(r *RunPod) { r.minGap = d } }

// WithCloud selects the cloud tier: "secure" (default) or "community".
func WithCloud(tier string) Option {
	return func(r *RunPod) {
		if t := strings.ToUpper(strings.TrimSpace(tier)); t != "" {
			r.cloud = t
		}
	}
}

// New returns a RunPod provider. apiKey is a RunPod API key (Bearer auth).
func New(apiKey string, opts ...Option) (*RunPod, error) {
	r := &RunPod{
		apiKey:      apiKey,
		baseURL:     defaultBaseURL,
		http:        &http.Client{Timeout: 30 * time.Second},
		cloud:       CloudSecure,
		minGap:      200 * time.Millisecond,
		maxRetry:    5,
		backoffBase: time.Second,
		backoffMax:  8 * time.Second,
		catalog:     map[int]catalogEntry{},
		observed:    map[string]float64{},
	}
	for _, o := range opts {
		o(r)
	}
	if r.cloud != CloudSecure && r.cloud != CloudCommunity {
		return nil, fmt.Errorf("runpod: unknown cloud tier %q (use secure or community)", r.cloud)
	}
	return r, nil
}

// Name implements provider.Provider.
func (r *RunPod) Name() string { return "runpod" }

// Cloud returns the selected cloud tier (SECURE or COMMUNITY).
func (r *RunPod) Cloud() string { return r.cloud }

// ContainerNodes implements provider.ContainerHost: every RunPod node is a container.
func (r *RunPod) ContainerNodes() bool { return true }

// CreateServer launches a GPU pod and waits until its sshd port is published.
// spec.UserData is the bootstrap SCRIPT (harden.PodBootstrap), not cloud-config.
func (r *RunPod) CreateServer(ctx context.Context, spec provider.ServerSpec) (provider.Server, error) {
	if !spec.GPU.Wanted() && spec.Type == "" {
		return provider.Server{}, fmt.Errorf("runpod: GPU pods only — pass --gpu MODEL (see `pandion list-gpus --provider runpod`)")
	}
	gpuID, count, err := r.gpuForSpec(ctx, spec)
	if err != nil {
		return provider.Server{}, err
	}
	image := strings.TrimSpace(spec.Image)
	gpu := createGPU{ID: gpuID, Count: count}
	if image == "" {
		image = DefaultImage
		gpu.MinCudaVersion = defaultMinCUDA
	}
	body := createPodReq{
		Name:       serverName(spec.ClusterID, spec.Name),
		Image:      image,
		GPU:        &gpu,
		Cloud:      r.cloud,
		Disk:       defaultDiskGB,
		Ports:      []string{"22/tcp"},
		Env:        map[string]string{harden.PodBootEnv: base64.StdEncoding.EncodeToString([]byte(spec.UserData))},
		Entrypoint: harden.PodEntrypoint(),
		Cmd:        []string{},
	}
	if len(spec.RegionPref) > 0 {
		body.DataCenterIDs = spec.RegionPref
	}
	var p pod
	if err := r.do(ctx, http.MethodPost, "/pods", body, &p); err != nil {
		return provider.Server{}, fmt.Errorf("runpod: create pod (%s ×%d, %s cloud): %w", gpuID, count, strings.ToLower(r.cloud), err)
	}
	if p.ID == "" {
		return provider.Server{}, fmt.Errorf("runpod: create pod returned no id")
	}
	srv, err := r.waitRunning(ctx, p.ID, spec.ClusterID, spec.Progress)
	if err != nil && !provider.IsTransientProvision(err) {
		// The create was ACCEPTED: the pod exists and bills per second even though
		// it never became reachable. Carry its id so the caller can delete it.
		return provider.Server{}, &provider.LaunchedError{
			ServerID: p.ID,
			Err:      fmt.Errorf("runpod pod %s was created but never became reachable: %w", p.ID, err),
		}
	}
	return srv, err
}

// gpuForSpec resolves the RunPod GPU type id + count for a spec: an explicit
// --size (a type slug like "rtx-a4000" or "rtx-4090x2") or a --gpu request.
func (r *RunPod) gpuForSpec(ctx context.Context, spec provider.ServerSpec) (string, int, error) {
	typeName := spec.Type
	if typeName == "" {
		var err error
		typeName, _, err = r.ResolveGPUType(ctx, spec.GPU, spec.RegionPref)
		if err != nil {
			return "", 0, err
		}
	}
	slug, count := splitType(typeName)
	gpus, err := r.gpuTypes(ctx, count)
	if err != nil {
		return "", 0, err
	}
	for _, g := range gpus {
		if typeSlug(g.ID) == slug {
			return g.ID, count, nil
		}
	}
	return "", 0, fmt.Errorf("runpod: unknown GPU type %q (see `pandion list-gpus --provider runpod`)", typeName)
}

var (
	progressEvery = time.Minute
	pollEvery     = 5 * time.Second
)

// waitRunning polls the pod until it is RUNNING with a published sshd port.
func (r *RunPod) waitRunning(ctx context.Context, id, clusterID string, progress provider.ProgressFunc) (provider.Server, error) {
	start := time.Now()
	lastStatus, lastReport := "", time.Time{}
	for {
		p, err := r.getPod(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return provider.Server{}, ctx.Err()
			}
			return provider.Server{}, err
		}
		st := strings.ToLower(p.Status)
		if st == "running" && p.SSH.Direct == nil {
			st = "running (waiting for the ssh port)"
		}
		if progress != nil && (st != lastStatus || time.Since(lastReport) >= progressEvery) {
			progress(fmt.Sprintf("runpod pod %s: %s (%s elapsed)", id, st, time.Since(start).Round(time.Second)))
			lastStatus, lastReport = st, time.Now()
		}
		switch p.Status {
		case "RUNNING":
			if p.SSH.Direct != nil && p.SSH.Direct.Host != "" && p.SSH.Direct.Port > 0 {
				return r.toServer(p, clusterID), nil
			}
		case "ERROR", "EXITED":
			// the container failed or exited during boot (bad image, bootstrap
			// crash). Not transient: a fresh pod would do the same.
			return provider.Server{}, fmt.Errorf("pod entered status %s while starting%s", p.Status, r.logTail(ctx, id))
		case "TERMINATED":
			return provider.Server{}, &provider.TransientProvisionError{
				Err: fmt.Errorf("runpod: pod %s was terminated while starting", id),
			}
		}
		select {
		case <-ctx.Done():
			return provider.Server{}, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// logTail fetches the last container log lines, for a boot-failure message.
func (r *RunPod) logTail(ctx context.Context, id string) string {
	var raw json.RawMessage
	if err := r.do(ctx, http.MethodGet, "/pods/"+id+"/logs", nil, &raw); err != nil || len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 1500 {
		s = "…" + s[len(s)-1500:]
	}
	return "; container log: " + s
}

// DestroyServer TERMINATES (deletes) a pod. Idempotent: a missing pod is success.
// Never `stop`: a stopped pod keeps its disk and can keep billing.
func (r *RunPod) DestroyServer(ctx context.Context, id string) error {
	err := r.do(ctx, http.MethodDelete, "/pods/"+url.PathEscape(id), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// ListByTag returns the cluster's pods (matched by name — pods carry no labels).
func (r *RunPod) ListByTag(ctx context.Context, clusterID string) ([]provider.Server, error) {
	pods, err := r.listPods(ctx)
	if err != nil {
		return nil, err
	}
	var out []provider.Server
	for _, p := range pods {
		if clusterOf(p.Name) == sanitize(clusterID) {
			out = append(out, r.toServer(p, clusterID))
		}
	}
	return out, nil
}

// ListAllTagged returns every Pandion-managed pod (the reaper's source of truth).
func (r *RunPod) ListAllTagged(ctx context.Context) ([]provider.Server, error) {
	pods, err := r.listPods(ctx)
	if err != nil {
		return nil, err
	}
	var out []provider.Server
	for _, p := range pods {
		if cid := clusterOf(p.Name); cid != "" {
			out = append(out, r.toServer(p, cid))
		}
	}
	return out, nil
}

// toServer maps a pod to a provider.Server. Created is RunPod's own createdAt
// (billing starts at create), so receipts and uptime use RunPod's clock.
func (r *RunPod) toServer(p pod, clusterID string) provider.Server {
	s := provider.Server{
		ID: p.ID, Name: p.Name, ClusterID: clusterID, Region: p.DataCenterID,
		Status: podStatus(p.Status),
	}
	if t, err := time.Parse(time.RFC3339, p.CreatedAt); err == nil {
		s.Created = t
	}
	if p.GPU != nil {
		count := max(p.GPU.Count, 1)
		s.Type = typeName(p.GPU.ID, count)
		s.GPU = provider.GPUInfo{Model: normModel(p.GPU.ID), Count: count, VRAM: r.vramOf(p.GPU.ID)}
	}
	if p.SSH.Direct != nil && p.Status == "RUNNING" {
		s.IP, s.SSHPort = p.SSH.Direct.Host, p.SSH.Direct.Port
	}
	if p.Cost > 0 && s.Type != "" {
		r.catMu.Lock()
		r.observed[s.Type+"|"+s.Region] = p.Cost
		r.catMu.Unlock()
	}
	return s
}

// podStatus maps a pod status onto provider.Server.Status ("" = running).
func podStatus(st string) string {
	switch st {
	case "RUNNING":
		return ""
	case "PROVISIONING", "STARTING":
		return "booting"
	case "EXITED":
		return "stopped" // still exists; a volume disk would keep billing
	case "ERROR":
		return "error"
	default:
		return strings.ToLower(st)
	}
}

func (r *RunPod) vramOf(gpuID string) int {
	r.catMu.Lock()
	defer r.catMu.Unlock()
	for _, e := range r.catalog {
		for _, g := range e.gpus {
			if g.ID == gpuID {
				return g.Memory
			}
		}
	}
	return 0
}

func (r *RunPod) getPod(ctx context.Context, id string) (pod, error) {
	var p pod
	err := r.do(ctx, http.MethodGet, "/pods/"+url.PathEscape(id), nil, &p)
	return p, err
}

// listPods returns every non-terminated pod, following pagination.
func (r *RunPod) listPods(ctx context.Context) ([]pod, error) {
	var out []pod
	cursor := ""
	for page := 0; page < 50; page++ {
		path := "/pods"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		var resp struct {
			Pods       []pod `json:"pods"`
			Pagination struct {
				NextCursor  string `json:"nextCursor"`
				HasNextPage bool   `json:"hasNextPage"`
			} `json:"pagination"`
		}
		if err := r.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		for _, p := range resp.Pods {
			if p.Status != "TERMINATED" {
				out = append(out, p)
			}
		}
		if !resp.Pagination.HasNextPage || resp.Pagination.NextCursor == "" {
			break
		}
		cursor = resp.Pagination.NextCursor
	}
	return out, nil
}

// --- API types ---------------------------------------------------------------

type createGPU struct {
	ID             string `json:"id"`
	Count          int    `json:"count,omitempty"`
	MinCudaVersion string `json:"minCudaVersion,omitempty"`
}

type createPodReq struct {
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	GPU           *createGPU        `json:"gpu,omitempty"`
	Cloud         string            `json:"cloud,omitempty"`
	Disk          int               `json:"disk,omitempty"`
	Ports         []string          `json:"ports,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Entrypoint    []string          `json:"entrypoint,omitempty"`
	Cmd           []string          `json:"cmd"`
	DataCenterIDs []string          `json:"dataCenterIds,omitempty"`
}

type sshEndpoint struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}

type pod struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	DataCenterID string  `json:"dataCenterId"`
	Cost         float64 `json:"cost"`
	CreatedAt    string  `json:"createdAt"`
	GPU          *struct {
		ID    string `json:"id"`
		Count int    `json:"count"`
	} `json:"gpu"`
	SSH struct {
		Direct *sshEndpoint `json:"direct"`
		Proxy  *sshEndpoint `json:"proxy"`
	} `json:"ssh"`
}

// --- HTTP --------------------------------------------------------------------

// apiError is a non-2xx RunPod API response (problem+json or plain text).
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("runpod api %d: %s", e.Status, e.Msg) }

func isNotFound(err error) bool {
	ae, ok := err.(*apiError)
	return ok && ae.Status == http.StatusNotFound
}

var errWriter io.Writer = os.Stderr

func (r *RunPod) do(ctx context.Context, method, path string, body, out any) error {
	for attempt := 0; ; attempt++ {
		if err := r.throttle(ctx); err != nil {
			return err
		}
		err, retryAfter := r.doOnce(ctx, method, path, body, out)
		if err == nil {
			return nil
		}
		ae, ok := err.(*apiError)
		// retry rate limits and 5xx; a POST /pods 5xx might have created a pod, so
		// only a 429 (rejected, never processed) is retried for creates.
		retry := ok && (ae.Status == http.StatusTooManyRequests || (ae.Status >= 500 && method != http.MethodPost))
		if !retry || attempt >= r.maxRetry {
			return err
		}
		wait := min(r.backoffBase<<attempt, r.backoffMax)
		wait += time.Duration(rand.Int63n(int64(wait/2) + 1))
		wait = max(wait, retryAfter)
		fmt.Fprintf(errWriter, "runpod: api %d, retrying in %s (attempt %d/%d)\n", ae.Status, wait.Round(time.Millisecond), attempt+1, r.maxRetry)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (r *RunPod) throttle(ctx context.Context) error {
	if r.minGap <= 0 {
		return nil
	}
	r.mu.Lock()
	now := time.Now()
	wait := max(r.minGap-now.Sub(r.lastReq), 0)
	r.lastReq = now.Add(wait)
	r.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

func (r *RunPod) doOnce(ctx context.Context, method, path string, body, out any) (error, time.Duration) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err, 0
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, rdr)
	if err != nil {
		return err, 0
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err, 0
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &apiError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(data))}
		var prob struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(data, &prob) == nil && (prob.Detail != "" || prob.Title != "") {
			ae.Msg = strings.TrimSpace(strings.Trim(prob.Title+": "+prob.Detail, ": "))
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			ae.Msg += " (check RUNPOD_API_KEY / `pandion login --provider runpod`; the key needs pods read+write)"
		}
		ra := time.Duration(0)
		if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n >= 0 {
			ra = time.Duration(n) * time.Second
		}
		return ae, ra
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		return json.Unmarshal(data, out), 0
	}
	return nil, 0
}

var (
	_ provider.Provider      = (*RunPod)(nil)
	_ provider.GPUProvider   = (*RunPod)(nil)
	_ provider.Pricer        = (*RunPod)(nil)
	_ provider.ContainerHost = (*RunPod)(nil)
)
