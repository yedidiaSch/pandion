// SPDX-License-Identifier: AGPL-3.0-or-later

package orchestrator

// Regression tests for the issues found running the GPU lab on Lambda
// (gpu/docs/pandion_issues.md): a launch that times out leaving a billing
// instance (#11), a terminate that is accepted but never finishes (#12), and
// receipts/uptime computed from the wrong start time (#9).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/provider"
	"github.com/yedidiaSch/pandion/internal/state"
)

func fastDown(t *testing.T) {
	t.Helper()
	prevWait, prevPoll := DownWait, downPollEvery
	DownWait, downPollEvery = 30*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { DownWait, downPollEvery = prevWait, prevPoll })
}

// #11: a launch accepted by the provider but never ready must be journaled with
// its server id, so Down destroys it even though `up` failed.
func TestUpSpec_LaunchedThenFailed_IsJournaledAndTornDown(t *testing.T) {
	o, m := newOrch(t)
	fastDown(t)
	m.LaunchThenFailFor = map[string]bool{"node-a": true}
	ctx := context.Background()

	c, err := o.UpSpec(ctx, "gpu-lab", NodeSpec{Name: "node-a"}, "#cloud-config\n", "")
	if err == nil {
		t.Fatal("want a provisioning error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error should wrap the deadline: %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("the launched instance exists at the provider: want 1 server, got %d", m.Count())
	}
	if c.Nodes[0].Phase != state.Failed || c.Nodes[0].ServerID == "" {
		t.Fatalf("journal must record FAILED + the launched server id, got %+v", c.Nodes[0])
	}
	if c.Nodes[0].Launched.IsZero() {
		t.Error("journal must record the launch time")
	}
	if err := o.Down(ctx, "gpu-lab"); err != nil {
		t.Fatalf("down: %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("down must destroy the half-launched instance, %d left", m.Count())
	}
}

// #12: an accepted terminate that never finishes must NOT be reported as gone,
// and the local state must survive so the cluster is still visible.
func TestDown_StuckTerminating_IsNotReportedGone(t *testing.T) {
	o, m := newOrch(t)
	fastDown(t)
	ctx := context.Background()
	if _, err := o.Up(ctx, "gpu-lab", "node-a", "#cloud-config\n", ""); err != nil {
		t.Fatal(err)
	}
	m.StuckTerminating = true

	err := o.Down(ctx, "gpu-lab")
	if !IsStillTerminating(err) {
		t.Fatalf("want StillTerminatingError, got %v", err)
	}
	if !strings.Contains(err.Error(), "still terminating") || !strings.Contains(err.Error(), "pandion down --id gpu-lab") {
		t.Errorf("message should say it is still terminating and how to re-check: %v", err)
	}
	if _, lerr := o.S.Load("gpu-lab"); lerr != nil {
		t.Errorf("local state must be KEPT while the provider still lists the server: %v", lerr)
	}
	if m.ReapAuxCalls != 0 {
		t.Errorf("aux resources (the SSH key) must not be reaped before the server is gone")
	}

	// once the provider finishes, a re-run converges and closes the state.
	m.StuckTerminating = false
	if err := o.Down(ctx, "gpu-lab"); err != nil {
		t.Fatalf("re-run down: %v", err)
	}
	if _, lerr := o.S.Load("gpu-lab"); lerr == nil {
		t.Error("state should be closed once the provider reports nothing")
	}
}

// A provider that hands back no creation time (Lambda) must not show 0m uptime
// or an age of "forever": the journaled launch time is used instead.
func TestStatus_UsesJournaledLaunchTime(t *testing.T) {
	o, m := newOrch(t)
	m.NoCreatedTime = true
	ctx := context.Background()
	if _, err := o.Up(ctx, "gpu-lab", "node-a", "#cloud-config\n", ""); err != nil {
		t.Fatal(err)
	}
	// pretend the launch happened 10 minutes ago
	c, _ := o.S.Load("gpu-lab")
	c.Nodes[0].Launched = time.Now().Add(-10 * time.Minute)
	if err := o.S.Save(c); err != nil {
		t.Fatal(err)
	}

	clusters, _, err := o.Status(ctx)
	if err != nil || len(clusters) != 1 {
		t.Fatalf("status: %v %+v", err, clusters)
	}
	n := clusters[0].Nodes[0]
	if !n.AgeKnown || n.Age < 9*time.Minute || n.Age > 11*time.Minute {
		t.Fatalf("want ~10m known age from the journal, got known=%v age=%s", n.AgeKnown, n.Age)
	}
}

// The provider's own creation time and the journal disagree: the EARLIER wins,
// because billing starts at launch.
func TestLaunchedAt_EarlierOfProviderAndJournal(t *testing.T) {
	o, _ := newOrch(t)
	early := time.Now().Add(-2 * time.Hour).UTC()
	late := time.Now().Add(-1 * time.Hour).UTC()
	_ = o.S.Save(&state.Cluster{ID: "c", Provider: "mock", Nodes: []state.Node{{Name: "n", ServerID: "s1", Launched: early}}})

	if got := o.LaunchedAt(provider.Server{ID: "s1", ClusterID: "c", Created: late}); !got.Equal(early) {
		t.Errorf("want the earlier journal time %v, got %v", early, got)
	}
	if got := o.LaunchedAt(provider.Server{ID: "other", ClusterID: "c", Created: late}); !got.Equal(late) {
		t.Errorf("a server the journal doesn't know keeps the provider time, got %v", got)
	}
	if got := o.LaunchedAt(provider.Server{ID: "x", ClusterID: "nojournal"}); !got.IsZero() {
		t.Errorf("no provider time + no journal = unknown (zero), got %v", got)
	}
}

// reap --older-than must never treat an unknown age as old (that would reap a
// node launched a minute ago from another machine); it is reported as skipped.
func TestReapPlan_UnknownAgeIsSkippedNotReaped(t *testing.T) {
	o, m := newOrch(t)
	m.NoCreatedTime = true
	ctx := context.Background()
	if _, err := m.CreateServer(ctx, provider.ServerSpec{Name: "n", ClusterID: "orphan"}); err != nil {
		t.Fatal(err)
	}
	plan, skipped, err := o.ReapPlan(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 || len(skipped) != 1 || skipped[0] != "orphan" {
		t.Fatalf("want orphan skipped (unknown age), got plan=%+v skipped=%v", plan, skipped)
	}
	// without an age filter it IS a reap candidate, flagged as unknown age.
	plan, skipped, _ = o.ReapPlan(ctx, 0)
	if len(plan) != 1 || plan[0].AgeKnown || len(skipped) != 0 {
		t.Fatalf("want 1 unknown-age candidate, got plan=%+v skipped=%v", plan, skipped)
	}
}
