// SPDX-License-Identifier: AGPL-3.0-or-later

package lambda

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// instanceTypesJSON is a realistic GET /instance-types body: two GPU SKUs (one
// with capacity, a100 cheaper than h100) and one CPU SKU that must be excluded.
const instanceTypesJSON = `{"data":{
  "gpu_1x_a100_sxm4":{"instance_type":{"name":"gpu_1x_a100_sxm4","description":"1x A100 (40 GB SXM4)","price_cents_per_hour":110,"specs":{"vcpus":30,"memory_gib":200,"gpus":1}},
    "regions_with_capacity_available":[{"name":"us-east-1","description":"Virginia"}]},
  "gpu_8x_h100_sxm5":{"instance_type":{"name":"gpu_8x_h100_sxm5","description":"8x H100 (80 GB SXM5)","price_cents_per_hour":2392,"specs":{"vcpus":208,"memory_gib":1800,"gpus":8}},
    "regions_with_capacity_available":[{"name":"us-west-1","description":"California"}]},
  "cpu_4x_general":{"instance_type":{"name":"cpu_4x_general","description":"4x vCPU","price_cents_per_hour":36,"specs":{"vcpus":4,"memory_gib":16,"gpus":0}},
    "regions_with_capacity_available":[{"name":"us-east-1","description":"Virginia"}]}
}}`

// stubServer returns an httptest server emulating the Lambda API, plus a pointer
// to a slice capturing every launch body seen (for request-shape assertions).
func stubServer(t *testing.T) (*Lambda, *[]launchReq, *[]terminateReq) {
	t.Helper()
	var launches []launchReq
	var terminates []terminateReq
	created := map[string]instance{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instance-types", func(w http.ResponseWriter, r *http.Request) {
		if u, _, _ := r.BasicAuth(); u == "" {
			t.Errorf("missing basic-auth key on %s", r.URL.Path)
		}
		io.WriteString(w, instanceTypesJSON)
	})
	mux.HandleFunc("/api/v1/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"data":[]}`) // none yet ⇒ force a register
			return
		}
		var k sshKey
		json.NewDecoder(r.Body).Decode(&k)
		json.NewEncoder(w).Encode(map[string]sshKey{"data": {ID: "k1", Name: k.Name, PublicKey: k.PublicKey}})
	})
	mux.HandleFunc("/api/v1/instance-operations/launch", func(w http.ResponseWriter, r *http.Request) {
		var lr launchReq
		json.NewDecoder(r.Body).Decode(&lr)
		launches = append(launches, lr)
		id := "i-123"
		created[id] = instance{
			ID: id, Name: lr.Name, IP: "203.0.113.7", Status: "active",
			Region:       region{Name: lr.RegionName},
			InstanceType: itype{Name: lr.InstanceTypeName, Description: "1x A100 (40 GB SXM4)", Specs: specs{GPUs: 1}},
		}
		io.WriteString(w, `{"data":{"instance_ids":["`+id+`"]}}`)
	})
	mux.HandleFunc("/api/v1/instance-operations/terminate", func(w http.ResponseWriter, r *http.Request) {
		var tr terminateReq
		json.NewDecoder(r.Body).Decode(&tr)
		terminates = append(terminates, tr)
		for _, id := range tr.InstanceIDs {
			delete(created, id)
		}
		io.WriteString(w, `{"data":{"terminated_instances":[]}}`)
	})
	mux.HandleFunc("/api/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		list := make([]instance, 0, len(created))
		for _, in := range created {
			list = append(list, in)
		}
		b, _ := json.Marshal(map[string][]instance{"data": list})
		w.Write(b)
	})
	mux.HandleFunc("/api/v1/instances/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/instances/")
		in, ok := created[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"instance-not-found","message":"no such instance"}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]instance{"data": in})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	l := New("test-key", WithBaseURL(srv.URL+"/api/v1"), WithHTTPClient(srv.Client()))
	return l, &launches, &terminates
}

func TestImplementsInterfaces(t *testing.T) {
	var _ provider.Provider = New("k")
	var _ provider.GPUProvider = New("k")
	var _ provider.Pricer = New("k")
}

func TestGPUOfferings(t *testing.T) {
	l, _, _ := stubServer(t)
	offs, err := l.GPUOfferings(context.Background())
	if err != nil {
		t.Fatalf("offerings: %v", err)
	}
	if len(offs) != 2 {
		t.Fatalf("want 2 GPU offerings (CPU excluded), got %d: %+v", len(offs), offs)
	}
	// cheapest-first: a100 (1.10) before h100 (23.92)
	if offs[0].ServerType != "gpu_1x_a100_sxm4" || offs[1].ServerType != "gpu_8x_h100_sxm5" {
		t.Fatalf("not cheapest-first: %s then %s", offs[0].ServerType, offs[1].ServerType)
	}
	a := offs[0]
	if a.GPU.Model != "a100" || a.GPU.Count != 1 || a.GPU.VRAM != 40 {
		t.Fatalf("a100 parse wrong: %+v", a.GPU)
	}
	if a.Hourly.Amount != 1.10 || a.Hourly.Currency != "USD" {
		t.Fatalf("a100 price wrong: %+v", a.Hourly)
	}
	h := offs[1]
	if h.GPU.Model != "h100" || h.GPU.Count != 8 || h.GPU.VRAM != 80 {
		t.Fatalf("h100 parse wrong: %+v", h.GPU)
	}
}

func TestResolveGPUType(t *testing.T) {
	l, _, _ := stubServer(t)
	ctx := context.Background()

	typ, region, err := l.ResolveGPUType(ctx, provider.GPUReq{Model: "a100", Count: 1}, nil)
	if err != nil || typ != "gpu_1x_a100_sxm4" || region != "us-east-1" {
		t.Fatalf("resolve a100 = %q/%q err=%v", typ, region, err)
	}
	// 8-GPU request must match the h100 SKU (count-aware)
	typ, _, err = l.ResolveGPUType(ctx, provider.GPUReq{Model: "h100", Count: 8}, nil)
	if err != nil || typ != "gpu_8x_h100_sxm5" {
		t.Fatalf("resolve h100:8 = %q err=%v", typ, err)
	}
	// a single-GPU h100 does not exist in the catalog ⇒ error
	if _, _, err := l.ResolveGPUType(ctx, provider.GPUReq{Model: "h100", Count: 1}, nil); err == nil {
		t.Fatal("h100:1 should not resolve (only an 8x SKU exists)")
	}
	// unknown model ⇒ error
	if _, _, err := l.ResolveGPUType(ctx, provider.GPUReq{Model: "b200", Count: 1}, nil); err == nil {
		t.Fatal("unknown model must error")
	}
}

func TestEstimateHourly(t *testing.T) {
	l, _, _ := stubServer(t)
	m, err := l.EstimateHourly(context.Background(), provider.ServerSpec{GPU: provider.GPUReq{Model: "a100", Count: 1}})
	if err != nil || m.Amount != 1.10 {
		t.Fatalf("estimate a100 = %+v err=%v", m, err)
	}
	// a non-GPU spec cannot be priced on a GPU-only cloud ⇒ error (budget fails closed)
	if _, err := l.EstimateHourly(context.Background(), provider.ServerSpec{}); err == nil {
		t.Fatal("non-GPU estimate must error on lambda")
	}
}

func TestCreateAndDestroy(t *testing.T) {
	l, launches, terminates := stubServer(t)
	ctx := context.Background()

	srv, err := l.CreateServer(ctx, provider.ServerSpec{
		Name: "node-a", ClusterID: "ml-run", LoginPubKey: "ssh-ed25519 AAAAC3xyz user@host",
		UserData: "#cloud-config\n", GPU: provider.GPUReq{Model: "a100", Count: 1},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if srv.IP != "203.0.113.7" || srv.Type != "gpu_1x_a100_sxm4" || srv.GPU.Model != "a100" {
		t.Fatalf("bad server: %+v", srv)
	}
	if srv.ClusterID != "ml-run" {
		t.Fatalf("cluster id lost: %q", srv.ClusterID)
	}

	// launch request shape
	if len(*launches) != 1 {
		t.Fatalf("want 1 launch, got %d", len(*launches))
	}
	lr := (*launches)[0]
	if lr.InstanceTypeName != "gpu_1x_a100_sxm4" || lr.RegionName != "us-east-1" {
		t.Fatalf("launch type/region: %+v", lr)
	}
	if lr.Name != "pandion-ml-run--node-a" {
		t.Fatalf("name-encoded cluster wrong: %q", lr.Name)
	}
	if lr.UserData != "#cloud-config\n" {
		t.Fatalf("cloud-init not forwarded: %q", lr.UserData)
	}
	if len(lr.SSHKeyNames) != 1 {
		t.Fatalf("ssh key not attached: %+v", lr.SSHKeyNames)
	}

	// ListByTag / ListAllTagged recover the cluster from the name
	byTag, _ := l.ListByTag(ctx, "ml-run")
	if len(byTag) != 1 || byTag[0].ClusterID != "ml-run" {
		t.Fatalf("ListByTag: %+v", byTag)
	}
	all, _ := l.ListAllTagged(ctx)
	if len(all) != 1 || all[0].ClusterID != "ml-run" {
		t.Fatalf("ListAllTagged: %+v", all)
	}

	// destroy
	if err := l.DestroyServer(ctx, srv.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if len(*terminates) != 1 || (*terminates)[0].InstanceIDs[0] != srv.ID {
		t.Fatalf("terminate shape: %+v", *terminates)
	}
	// idempotent: destroying an absent id is success
	if err := l.DestroyServer(ctx, srv.ID); err != nil {
		t.Fatalf("second destroy should be nil, got %v", err)
	}
	if got, _ := l.ListAllTagged(ctx); len(got) != 0 {
		t.Fatalf("still listed after destroy: %+v", got)
	}
}

// Regression for the live-e2e finding: Lambda keeps returning terminating/
// terminated instances for a while, and reconciliation must treat them as gone —
// else `pandion down`'s verify-empty check fails after a successful terminate.
func TestListExcludesTerminating(t *testing.T) {
	var draining []instance
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string][]instance{"data": draining})
		w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	l := New("k", WithBaseURL(srv.URL+"/api/v1"), WithHTTPClient(srv.Client()))

	draining = []instance{
		{ID: "a", Name: "pandion-c--n1", Status: "active", InstanceType: itype{Name: "gpu_1x_a10"}},
		{ID: "b", Name: "pandion-c--n2", Status: "terminating", InstanceType: itype{Name: "gpu_1x_a10"}},
		{ID: "d", Name: "pandion-c--n3", Status: "terminated", InstanceType: itype{Name: "gpu_1x_a10"}},
	}
	got, err := l.ListByTag(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("want only the active node, got %+v", got)
	}
}

func TestCreateRequiresGPU(t *testing.T) {
	l, _, _ := stubServer(t)
	_, err := l.CreateServer(context.Background(), provider.ServerSpec{Name: "n", ClusterID: "c"})
	if err == nil || !strings.Contains(err.Error(), "GPU") {
		t.Fatalf("want GPU-required error, got %v", err)
	}
}

func TestNameRoundTrip(t *testing.T) {
	// cluster and node with awkward characters still round-trip unambiguously
	name := serverName("ML Run/2", "node_b.1")
	if got := clusterOf(name); got != "ml-run-2" {
		t.Fatalf("clusterOf(%q) = %q", name, got)
	}
	if clusterOf("not-ours-x") != "" {
		t.Fatal("foreign instance name must yield empty cluster")
	}
}
