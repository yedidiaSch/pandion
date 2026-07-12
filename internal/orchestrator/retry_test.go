// SPDX-License-Identifier: AGPL-3.0-or-later

package orchestrator

import (
	"context"
	"io"
	"testing"

	"github.com/yedidiaSch/pandion/internal/provider"
	"github.com/yedidiaSch/pandion/internal/state"
)

// fastRetries removes the inter-attempt delay and silences the notice for tests.
func fastRetries(t *testing.T) {
	t.Helper()
	pd, pl := provisionRetryDelay, provisionRetryLog
	provisionRetryDelay, provisionRetryLog = 0, io.Discard
	t.Cleanup(func() { provisionRetryDelay, provisionRetryLog = pd, pl })
}

// A node whose instance dies mid-boot a couple of times must be relaunched and
// ultimately reach RUNNING — one flaky boot shouldn't fail the whole up.
func TestProvisionRetry_RecoversFromMidBootTermination(t *testing.T) {
	fastRetries(t)
	o, m := newOrch(t)
	m.TransientBootFor = map[string]int{"node-a": 2} // 2 transient failures, then success

	c, err := o.UpSpec(context.Background(), "c1", NodeSpec{Name: "node-a"}, "#cloud-config\n", "")
	if err != nil {
		t.Fatalf("expected recovery after transient boot failures, got %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("want exactly 1 live server, got %d", m.Count())
	}
	if c.Nodes[0].Phase != state.Running {
		t.Fatalf("want phase RUNNING, got %s", c.Nodes[0].Phase)
	}
	if left := m.TransientBootFor["node-a"]; left != 0 {
		t.Fatalf("want all transient failures consumed, %d left", left)
	}
}

// If boot keeps failing past the attempt bound, up fails — surfacing the
// transient error — and leaves no server behind.
func TestProvisionRetry_GivesUpAfterBound(t *testing.T) {
	fastRetries(t)
	o, m := newOrch(t)
	m.TransientBootFor = map[string]int{"node-a": maxProvisionAttempts + 2} // never recovers

	_, err := o.UpSpec(context.Background(), "c1", NodeSpec{Name: "node-a"}, "#cloud-config\n", "")
	if err == nil {
		t.Fatal("expected failure after exhausting provisioning attempts")
	}
	if !provider.IsTransientProvision(err) {
		t.Fatalf("want the transient error surfaced, got %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("no server should survive a failed provision, got %d", m.Count())
	}
	// exactly maxProvisionAttempts launches were tried
	if got := (maxProvisionAttempts + 2) - m.TransientBootFor["node-a"]; got != maxProvisionAttempts {
		t.Fatalf("want %d attempts, got %d", maxProvisionAttempts, got)
	}
}

// A NON-transient error (bad spec/quota) must fail immediately — no relaunch.
func TestProvisionRetry_NonTransientNotRetried(t *testing.T) {
	fastRetries(t)
	o, m := newOrch(t)
	m.FailCreateFor = map[string]bool{"node-a": true}

	_, err := o.UpSpec(context.Background(), "c1", NodeSpec{Name: "node-a"}, "#cloud-config\n", "")
	if err == nil {
		t.Fatal("expected a non-transient create error to fail")
	}
	if provider.IsTransientProvision(err) {
		t.Fatalf("non-transient error must not be classified transient: %v", err)
	}
}
