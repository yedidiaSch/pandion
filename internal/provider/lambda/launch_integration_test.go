// SPDX-License-Identifier: AGPL-3.0-or-later

package lambda

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/provider"
	"github.com/yedidiaSch/pandion/internal/sshkeys"
)

// SPENDS REAL MONEY. Gated behind PANDION_IT_LAUNCH=1 (distinct from the
// read-only PANDION_IT) so it never runs by accident or in CI. It launches the
// cheapest GPU SKU that currently has capacity, confirms it is active + listed,
// then terminates it and verifies it is gone — validating the provider WRITE path
// (CreateServer/waitRunning/ListByTag/DestroyServer/ensureLoginKey) against the
// real API. It does NOT touch the overlay/SSH/hardening (that needs a real
// operator machine, not this harness).
//
// Safety: a catch-all defer terminates EVERY instance under the test cluster tag,
// registered BEFORE launch, so an error after creation cannot leak a billed node.
func TestLambda_Integration_LaunchTerminate(t *testing.T) {
	if os.Getenv("PANDION_IT_LAUNCH") != "1" {
		t.Skip("set PANDION_IT_LAUNCH=1 (SPENDS MONEY) and LAMBDA_API_KEY to run the launch/terminate test")
	}
	key := os.Getenv("LAMBDA_API_KEY")
	if key == "" {
		t.Skip("LAMBDA_API_KEY not set")
	}
	const testCluster = "pandion-it-launch"
	l := New(key)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// SAFETY NET (registered before launch): terminate anything under the test tag,
	// even if CreateServer errors after the instance was created.
	defer func() {
		bg := context.Background()
		leftovers, err := l.ListByTag(bg, testCluster)
		if err != nil {
			t.Errorf("cleanup: could not list %q to verify teardown: %v", testCluster, err)
			return
		}
		for _, s := range leftovers {
			t.Logf("cleanup: terminating %s (%s)", s.ID, s.Name)
			if derr := l.DestroyServer(bg, s.ID); derr != nil {
				t.Errorf("!!! CLEANUP FAILED for instance %s — TERMINATE IT MANUALLY in the Lambda console: %v", s.ID, derr)
			}
		}
	}()

	// 1) cheapest offering WITH live capacity (GPUOfferings is cheapest-first).
	offs, err := l.GPUOfferings(ctx)
	if err != nil {
		t.Fatalf("offerings: %v", err)
	}
	var chosen provider.GPUOffering
	for _, o := range offs {
		if len(o.Regions) > 0 {
			chosen = o
			break
		}
	}
	if chosen.ServerType == "" {
		t.Skip("no Lambda GPU capacity available right now — cannot run a live launch")
	}
	t.Logf("launching %s (%s×%d, %dGB) in %v at $%.2f/hr",
		chosen.ServerType, chosen.GPU.Model, chosen.GPU.Count, chosen.GPU.VRAM, chosen.Regions, chosen.Hourly.Amount)

	// 2) ephemeral login key (registered with Lambda by ensureLoginKey).
	kp, err := sshkeys.Generate("pandion-it")
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	// 3) launch (blocks until active + IP, or ctx timeout).
	srv, err := l.CreateServer(ctx, provider.ServerSpec{
		Name:        "e2e",
		ClusterID:   testCluster,
		LoginPubKey: kp.PublicAuthorized,
		Type:        chosen.ServerType,
		UserData:    "#cloud-config\n",
	})
	if err != nil {
		t.Fatalf("launch: %v", err) // the safety-net defer still runs
	}
	t.Logf("LAUNCHED id=%s ip=%s type=%s gpu=%s×%d", srv.ID, srv.IP, srv.Type, srv.GPU.Model, srv.GPU.Count)

	// 4) assertions against the real instance.
	if srv.IP == "" {
		t.Errorf("active instance has no IP")
	}
	if srv.GPU.Model != chosen.GPU.Model || srv.GPU.Count != chosen.GPU.Count {
		t.Errorf("realized GPU %s×%d != requested %s×%d", srv.GPU.Model, srv.GPU.Count, chosen.GPU.Model, chosen.GPU.Count)
	}
	byTag, err := l.ListByTag(ctx, testCluster)
	if err != nil {
		t.Fatalf("list by tag: %v", err)
	}
	seen := false
	for _, s := range byTag {
		if s.ID == srv.ID {
			seen = true
		}
	}
	if !seen {
		t.Errorf("ListByTag(%q) did not return launched instance %s", testCluster, srv.ID)
	}

	// 5) explicit terminate + verify (the safety-net defer is a backstop).
	if err := l.DestroyServer(ctx, srv.ID); err != nil {
		t.Fatalf("terminate %s: %v", srv.ID, err)
	}
	// give the API a moment to reflect termination, then check it is no longer
	// listed. Termination is async on Lambda, so a still-present node is a soft
	// note (the safety-net defer is the backstop), not a hard failure.
	time.Sleep(5 * time.Second)
	left, err := l.ListByTag(ctx, testCluster)
	if err != nil {
		t.Fatalf("post-terminate list: %v", err)
	}
	still := false
	for _, s := range left {
		if s.ID == srv.ID {
			still = true
		}
	}
	if still {
		t.Logf("note: %s still listed shortly after terminate (async) — cleanup defer will confirm", srv.ID)
	} else {
		t.Logf("terminated + verified gone: %s", srv.ID)
	}
}
