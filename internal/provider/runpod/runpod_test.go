// SPDX-License-Identifier: AGPL-3.0-or-later

package runpod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/harden"
	"github.com/yedidiaSch/pandion/internal/provider"
)

// fakeAPI is a minimal in-memory RunPod REST v2.
type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	pods     map[string]*pod
	created  []createPodReq
	polls    int
	bootPoll int    // polls before a created pod reports RUNNING with ssh
	failWith string // status a created pod reports instead (ERROR/EXITED)
	catalog  []gpuType
	catQuery []string
	deleted  []string
	authSeen string
}

func newFake(t *testing.T) (*fakeAPI, *RunPod) {
	t.Helper()
	f := &fakeAPI{t: t, pods: map[string]*pod{}, bootPoll: 1}
	f.catalog = []gpuType{
		gpu("NVIDIA RTX A4000", "RTX A4000", 16, 0.25, "HIGH", "EU-RO-1"),
		gpu("NVIDIA GeForce RTX 4090", "RTX 4090", 24, 0.69, "MEDIUM", "US-TX-3"),
		gpu("NVIDIA GeForce RTX 3090", "RTX 3090", 24, 0.22, "NONE"),
		gpu("NVIDIA A40", "A40", 48, 0.40, "LOW", "US-KS-2"),
		gpu("NVIDIA A100 80GB PCIe", "A100 PCIe", 80, 1.64, "HIGH", "US-KS-2"),
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	r, err := New("rp_test_key", WithBaseURL(srv.URL), WithMinGap(0))
	if err != nil {
		t.Fatal(err)
	}
	return f, r
}

func gpu(id, name string, mem int, price float64, avail string, dcs ...string) gpuType {
	g := gpuType{ID: id, Name: name, Memory: mem, Secure: true, Community: true, Availability: avail}
	g.Price.Secure, g.Price.Community = price, price*0.8
	g.MaxCount.Secure, g.MaxCount.Community = 8, 8
	for _, d := range dcs {
		g.DataCenters = append(g.DataCenters, struct {
			ID           string `json:"id"`
			Availability string `json:"availability"`
		}{d, avail})
	}
	return g
}

func (f *fakeAPI) serve(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authSeen = req.Header.Get("Authorization")
	path := strings.TrimPrefix(req.URL.Path, "/")
	switch {
	case req.Method == http.MethodGet && path == "catalog/gpus":
		f.catQuery = append(f.catQuery, req.URL.RawQuery)
		_ = json.NewEncoder(w).Encode(map[string]any{"gpus": f.catalog})
	case req.Method == http.MethodPost && path == "pods":
		var body createPodReq
		b, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.created = append(f.created, body)
		id := "pod" + string(rune('a'+len(f.created)))
		p := &pod{ID: id, Name: body.Name, Status: "PROVISIONING", DataCenterID: "EU-RO-1", Cost: 0.25,
			CreatedAt: "2026-11-26T10:00:00Z"}
		p.GPU = &struct {
			ID    string `json:"id"`
			Count int    `json:"count"`
		}{body.GPU.ID, max(body.GPU.Count, 1)}
		f.pods[id] = p
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(p)
	case req.Method == http.MethodGet && path == "pods":
		var list []pod
		for _, p := range f.pods {
			c := *p
			c.SSH.Direct = nil // list omits the live ssh mapping
			list = append(list, c)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pods": list, "pagination": map[string]any{"hasNextPage": false}})
	case strings.HasPrefix(path, "pods/") && strings.HasSuffix(path, "/logs"):
		_, _ = io.WriteString(w, `"apt-get: unable to locate package nope"`)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "pods/"):
		p, ok := f.pods[strings.TrimPrefix(path, "pods/")]
		if !ok {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"title":"Not Found","detail":"resource not found"}`)
			return
		}
		f.polls++
		if f.polls > f.bootPoll {
			if f.failWith != "" {
				p.Status = f.failWith
			} else {
				p.Status = "RUNNING"
				p.SSH.Direct = &sshEndpoint{Host: "203.0.113.7", Port: 40123, Username: "root"}
			}
		}
		_ = json.NewEncoder(w).Encode(p)
	case req.Method == http.MethodDelete && strings.HasPrefix(path, "pods/"):
		id := strings.TrimPrefix(path, "pods/")
		if _, ok := f.pods[id]; !ok {
			w.WriteHeader(404)
			return
		}
		delete(f.pods, id)
		f.deleted = append(f.deleted, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected %s %s", req.Method, req.URL)
		w.WriteHeader(500)
	}
}

func fastPolls(t *testing.T) {
	old := pollEvery
	pollEvery = time.Millisecond
	t.Cleanup(func() { pollEvery = old })
}

func TestCreateServer_PodWithMappedSSH(t *testing.T) {
	fastPolls(t)
	f, r := newFake(t)
	script := harden.PodBootstrap(harden.CloudInit{HostPubKey: "ssh-ed25519 AAAAhost", HostPrivKeyPEM: "PRIV", LoginPubKey: "ssh-ed25519 AAAAlogin"})
	var progress []string
	srv, err := r.CreateServer(context.Background(), provider.ServerSpec{
		Name: "node-a", ClusterID: "gpu-lab", GPU: provider.GPUReq{Model: "a4000", Count: 1},
		UserData: script, Progress: func(s string) { progress = append(progress, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if srv.IP != "203.0.113.7" || srv.SSHPort != 40123 || srv.SSHAddr() != "203.0.113.7:40123" {
		t.Fatalf("ssh endpoint = %s:%d", srv.IP, srv.SSHPort)
	}
	if srv.Type != "rtx-a4000" || srv.GPU.Model != "rtxa4000" || srv.GPU.VRAM != 16 || srv.Created.IsZero() {
		t.Fatalf("server = %+v", srv)
	}
	if f.authSeen != "Bearer rp_test_key" {
		t.Errorf("auth header = %q", f.authSeen)
	}
	c := f.created[0]
	if c.Name != "pandion-gpu-lab--node-a" || c.GPU.ID != "NVIDIA RTX A4000" || c.Cloud != CloudSecure {
		t.Errorf("create body = %+v", c)
	}
	if c.Image != DefaultImage || c.GPU.MinCudaVersion != defaultMinCUDA || len(c.Ports) != 1 || c.Ports[0] != "22/tcp" {
		t.Errorf("image/cuda/ports = %q %q %v", c.Image, c.GPU.MinCudaVersion, c.Ports)
	}
	dec, err := base64.StdEncoding.DecodeString(c.Env[harden.PodBootEnv])
	if err != nil || string(dec) != script {
		t.Errorf("bootstrap not carried intact in %s", harden.PodBootEnv)
	}
	if len(progress) == 0 || !strings.Contains(progress[0], "provisioning") {
		t.Errorf("progress = %q", progress)
	}
}

// A pod that dies while starting was CREATED and bills: the error must carry its
// id (so up's rollback deletes it) and the container log tail (so the operator
// sees why).
func TestCreateServer_FailedBootIsLaunchedError(t *testing.T) {
	fastPolls(t)
	f, r := newFake(t)
	f.failWith = "EXITED"
	_, err := r.CreateServer(context.Background(), provider.ServerSpec{
		Name: "n", ClusterID: "c", GPU: provider.GPUReq{Model: "rtx4090", Count: 1},
	})
	if provider.LaunchedServerID(err) == "" {
		t.Fatalf("want a LaunchedError, got %v", err)
	}
	if !strings.Contains(err.Error(), "EXITED") || !strings.Contains(err.Error(), "unable to locate package") {
		t.Errorf("error should name the status and the log tail: %v", err)
	}
}

func TestCustomImageSkipsCudaConstraint(t *testing.T) {
	fastPolls(t)
	f, r := newFake(t)
	if _, err := r.CreateServer(context.Background(), provider.ServerSpec{
		Name: "n", ClusterID: "c", Image: "runpod/pytorch:latest", GPU: provider.GPUReq{Model: "a40", Count: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if c := f.created[0]; c.Image != "runpod/pytorch:latest" || c.GPU.MinCudaVersion != "" || c.GPU.ID != "NVIDIA A40" {
		t.Errorf("create = %+v / %+v", c, c.GPU)
	}
}

func TestListDestroyAndStatus(t *testing.T) {
	fastPolls(t)
	f, r := newFake(t)
	f.pods["x"] = &pod{ID: "x", Name: "pandion-gpu-lab--node-a", Status: "EXITED", CreatedAt: "2026-11-26T09:00:00Z"}
	f.pods["y"] = &pod{ID: "y", Name: "someone-elses-pod", Status: "RUNNING"}
	f.pods["z"] = &pod{ID: "z", Name: "pandion-other--n", Status: "TERMINATED"}
	got, err := r.ListByTag(context.Background(), "gpu-lab")
	if err != nil || len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("ListByTag = %+v, %v", got, err)
	}
	if got[0].Status != "stopped" {
		t.Errorf("an EXITED pod still exists and is listed as stopped, got %q", got[0].Status)
	}
	all, _ := r.ListAllTagged(context.Background())
	if len(all) != 1 {
		t.Errorf("ListAllTagged must skip foreign + terminated pods: %+v", all)
	}
	if err := r.DestroyServer(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if err := r.DestroyServer(context.Background(), "x"); err != nil {
		t.Fatalf("destroy must be idempotent: %v", err)
	}
	if len(f.deleted) != 1 {
		t.Errorf("want exactly one DELETE, got %v", f.deleted)
	}
}

func TestResolveAndPrice(t *testing.T) {
	_, r := newFake(t)
	ctx := context.Background()
	// cheapest available, never an unavailable one (3090 is cheaper but NONE)
	typ, _, err := r.ResolveGPUType(ctx, provider.GPUReq{Count: 1}, nil)
	if err != nil || typ != "rtx-a4000" {
		t.Fatalf("cheapest = %q, %v", typ, err)
	}
	if _, _, err := r.ResolveGPUType(ctx, provider.GPUReq{Model: "3090", Count: 1}, nil); err == nil {
		t.Error("an unavailable GPU must not resolve")
	}
	// strict region
	if typ, reg, err := r.ResolveGPUType(ctx, provider.GPUReq{Model: "4090", Count: 1}, []string{"us-tx-3"}); err != nil || typ != "rtx-4090" || reg != "US-TX-3" {
		t.Errorf("region resolve = %q %q %v", typ, reg, err)
	}
	if _, _, err := r.ResolveGPUType(ctx, provider.GPUReq{Model: "4090", Count: 1}, []string{"EU-RO-1"}); err == nil {
		t.Error("an explicit region with no capacity must fail, not move")
	}
	// multi-GPU type + price
	typ, _, err = r.ResolveGPUType(ctx, provider.GPUReq{Model: "a100", Count: 2}, nil)
	if err != nil || typ != "a100-80gb-pciex2" {
		t.Fatalf("2x a100 = %q, %v", typ, err)
	}
	m, _ := r.EstimateHourly(ctx, provider.ServerSpec{GPU: provider.GPUReq{Model: "a100", Count: 2}})
	if m.Currency != "USD" || m.Amount < 3.27 || m.Amount > 3.29 {
		t.Errorf("2x a100 price = %+v", m)
	}
	offs, _ := r.GPUOfferings(ctx)
	if len(offs) != 4 || offs[0].ServerType != "rtx-a4000" {
		t.Errorf("offerings = %+v", offs)
	}
}

func TestCommunityTier(t *testing.T) {
	f, _ := newFake(t)
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	r, err := New("k", WithBaseURL(srv.URL), WithMinGap(0), WithCloud("community"))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := r.HourlyPrice(context.Background(), "rtx-a4000", "")
	if m.Amount != 0.2 {
		t.Errorf("community price = %v", m.Amount)
	}
	if !strings.Contains(f.catQuery[len(f.catQuery)-1], "cloud=COMMUNITY") {
		t.Errorf("catalog query = %q", f.catQuery)
	}
	if _, err := New("k", WithCloud("cheap")); err == nil {
		t.Error("an unknown tier must be rejected")
	}
}

func TestModelMatching(t *testing.T) {
	cases := []struct {
		want, id string
		ok       bool
	}{
		{"a4000", "NVIDIA RTX A4000", true},
		{"rtxa4000", "NVIDIA RTX A4000", true},
		{"rtx-a4000", "NVIDIA RTX A4000", true},
		{"4090", "NVIDIA GeForce RTX 4090", true},
		{"rtx4090", "NVIDIA GeForce RTX 4090", true},
		{"a100", "NVIDIA A100 80GB PCIe", true},
		{"a100", "NVIDIA A100-SXM4-80GB", true},
		{"h100", "NVIDIA H100 80GB HBM3", true},
		{"a40", "NVIDIA A40", true},
		{"a40", "NVIDIA RTX A4000", false},
		{"a10", "NVIDIA A100 80GB PCIe", false},
		{"3090", "NVIDIA GeForce RTX 4090", false},
	}
	for _, c := range cases {
		if got := modelMatches(c.want, c.id); got != c.ok {
			t.Errorf("modelMatches(%q, %q) = %v, want %v", c.want, c.id, got, c.ok)
		}
	}
	for _, typ := range []string{"rtx-a4000", "rtx-4090x2", "a100-80gb-pcie", "mi300x"} {
		slug, n := splitType(typ)
		if typeName(map[string]string{"rtx-a4000": "NVIDIA RTX A4000", "rtx-4090x2": "NVIDIA GeForce RTX 4090",
			"a100-80gb-pcie": "NVIDIA A100 80GB PCIe", "mi300x": "AMD Instinct MI300X"}[typ], n) == "" || slug == "" {
			t.Errorf("splitType(%q) = %q,%d", typ, slug, n)
		}
	}
	if s, n := splitType("rtx-4090x2"); s != "rtx-4090" || n != 2 {
		t.Errorf("splitType = %q %d", s, n)
	}
	if s, n := splitType("mi300x"); s != "mi300x" || n != 1 {
		t.Errorf("splitType(mi300x) = %q %d", s, n)
	}
}
