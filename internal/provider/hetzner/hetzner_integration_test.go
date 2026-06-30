package hetzner

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/envcore/envcore/internal/provider"
)

// Opt-in real-cloud test. Requires BOTH ENVCORE_IT=1 and HCLOUD_TOKEN so it can
// never run by accident (e.g. when HCLOUD_TOKEN merely happens to be exported)
// and never runs in CI. It provisions and destroys one cheap server, with
// best-effort teardown even on assertion failure.
func TestHetzner_Integration_CreateListDestroy(t *testing.T) {
	if os.Getenv("ENVCORE_IT") != "1" {
		t.Skip("set ENVCORE_IT=1 and HCLOUD_TOKEN to run the real-cloud integration test")
	}
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		t.Skip("HCLOUD_TOKEN not set")
	}

	h := New(token)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	clusterID := "envcore-it-" + time.Now().Format("20060102-150405")

	// best-effort teardown regardless of what fails below
	defer func() {
		list, lerr := h.ListByTag(context.Background(), clusterID)
		if lerr != nil {
			t.Logf("cleanup list error: %v", lerr)
			return
		}
		for _, s := range list {
			if derr := h.DestroyServer(context.Background(), s.ID); derr != nil {
				t.Errorf("cleanup destroy %s: %v", s.ID, derr)
			}
		}
	}()

	srv, err := h.CreateServer(ctx, provider.ServerSpec{
		Name:      "envcore-it",
		ClusterID: clusterID,
		MinCores:  2,
		MinRAMGB:  2,
		UserData:  "#cloud-config\n",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created %s (%s) in %s, IP %s", srv.ID, srv.Type, srv.Region, srv.IP)

	list, err := h.ListByTag(ctx, clusterID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 server by tag, got %d", len(list))
	}

	if err := h.DestroyServer(ctx, srv.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := h.DestroyServer(ctx, srv.ID); err != nil {
		t.Fatalf("idempotent second destroy: %v", err)
	}

	left, err := h.ListByTag(ctx, clusterID)
	if err != nil {
		t.Fatalf("list after destroy: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("want 0 servers after destroy, got %d", len(left))
	}
}
