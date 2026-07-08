// SPDX-License-Identifier: AGPL-3.0-or-later

package orchestrator

import (
	"context"
	"testing"
)

// Status groups running nodes by cluster, sorts deterministically, and (via the
// mock's Pricer) rolls up live cost — €0.01/hr per node in the mock.
func TestStatus_GroupsAndPrices(t *testing.T) {
	o, _ := newOrch(t)
	ctx := context.Background()

	// two clusters, provisioned out of alphabetical order to prove sorting.
	if _, err := o.Up(ctx, "zeta", "node-a", "#cloud-config\n", ""); err != nil {
		t.Fatalf("up zeta: %v", err)
	}
	if _, err := o.UpCluster(ctx, "alpha",
		[]NodeSpec{{Name: "broker"}, {Name: "worker"}}, DefaultMaxConcurrency); err != nil {
		t.Fatalf("up alpha: %v", err)
	}

	clusters, currency, err := o.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if currency != "EUR" {
		t.Fatalf("want currency EUR, got %q", currency)
	}
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters, got %d", len(clusters))
	}

	// sorted by cluster id: alpha before zeta.
	if clusters[0].ClusterID != "alpha" || clusters[1].ClusterID != "zeta" {
		t.Fatalf("clusters not sorted: %q, %q", clusters[0].ClusterID, clusters[1].ClusterID)
	}

	// alpha has 2 nodes, sorted by name (broker, worker), each priced.
	alpha := clusters[0]
	if len(alpha.Nodes) != 2 {
		t.Fatalf("want 2 nodes in alpha, got %d", len(alpha.Nodes))
	}
	if alpha.Nodes[0].Name != "broker" || alpha.Nodes[1].Name != "worker" {
		t.Fatalf("nodes not sorted by name: %q, %q", alpha.Nodes[0].Name, alpha.Nodes[1].Name)
	}
	for _, n := range alpha.Nodes {
		if !n.Hourly.Known() || n.Hourly.Amount != 0.01 {
			t.Fatalf("node %q unpriced: %+v", n.Name, n.Hourly)
		}
	}
	// cluster hourly = sum of node hourly = 2 × 0.01.
	if alpha.Hourly != 0.02 {
		t.Fatalf("want alpha hourly 0.02, got %v", alpha.Hourly)
	}
	// accrued is age×rate and must be non-negative (nodes just created ⇒ ~0).
	if alpha.Accrued < 0 {
		t.Fatalf("accrued must be >= 0, got %v", alpha.Accrued)
	}
}

// An empty provider yields no clusters and no currency — the `ls` "nothing
// running" path.
func TestStatus_EmptyIsClean(t *testing.T) {
	o, _ := newOrch(t)
	clusters, currency, err := o.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(clusters) != 0 {
		t.Fatalf("want 0 clusters, got %d", len(clusters))
	}
	if currency != "" {
		t.Fatalf("want empty currency, got %q", currency)
	}
}
