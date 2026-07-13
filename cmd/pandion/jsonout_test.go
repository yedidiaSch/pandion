// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/orchestrator"
	"github.com/yedidiaSch/pandion/internal/provider"
)

// TestRenderReceiptJSON checks the teardown-receipt schema (P1.5): every field is
// always present, dry-run zeroes the destroyed count, and gpus is [] not null.
func TestRenderReceiptJSON(t *testing.T) {
	servers := []provider.Server{{Name: "n1", Type: "cpx21", Region: "fsn1", IP: "1.2.3.4"}}
	r := receipt{nodes: 1, ran: 90 * time.Minute, ranKnown: true, total: 0.42, currency: "EUR", priced: true}

	var buf bytes.Buffer
	if err := renderReceiptJSON(&buf, r, "demo", "hetzner", servers, false); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, k := range []string{"id", "provider", "dry_run", "destroyed", "servers", "ran_seconds", "ran_known", "priced", "total_cost", "currency", "gpus"} {
		if _, ok := got[k]; !ok {
			t.Errorf("receipt JSON missing key %q", k)
		}
	}
	if got["destroyed"].(float64) != 1 {
		t.Errorf("destroyed = %v, want 1", got["destroyed"])
	}
	if _, isArray := got["gpus"].([]any); !isArray {
		t.Errorf("gpus must be a JSON array, got %T", got["gpus"])
	}

	// dry-run: destroyed must be 0 even with servers present.
	buf.Reset()
	if err := renderReceiptJSON(&buf, receipt{}, "demo", "hetzner", servers, true); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(buf.Bytes(), &got)
	if got["destroyed"].(float64) != 0 || got["dry_run"].(bool) != true {
		t.Errorf("dry-run receipt should show destroyed=0, dry_run=true; got %v", got)
	}
}

// TestRenderReapJSON checks the reap sweep schema and an empty-plan envelope.
func TestRenderReapJSON(t *testing.T) {
	plan := []orchestrator.ReapCandidate{{ClusterID: "old", Servers: 2, OldestAge: time.Hour}}
	var buf bytes.Buffer
	if err := renderReapJSON(&buf, "hetzner", plan, 1); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Provider string `json:"provider"`
		Planned  int    `json:"planned"`
		Reaped   int    `json:"reaped"`
		Clusters []struct {
			ID              string `json:"id"`
			Servers         int    `json:"servers"`
			OldestAgeSecond int64  `json:"oldest_age_seconds"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Planned != 1 || got.Reaped != 1 || len(got.Clusters) != 1 || got.Clusters[0].ID != "old" {
		t.Fatalf("unexpected reap JSON: %+v", got)
	}

	// empty plan still emits a valid envelope with an array.
	buf.Reset()
	if err := renderReapJSON(&buf, "hetzner", nil, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"clusters": []`)) {
		t.Errorf("empty reap JSON should carry clusters: [], got %s", buf.String())
	}
}

// TestRenderDryRunJSON checks the dry-run plan schema, incl. the auto flags.
func TestRenderDryRunJSON(t *testing.T) {
	nodes := []orchestrator.DryRunNode{{
		Name: "node-a", Size: "", Region: "", Window: time.Hour,
		Hourly: provider.Money{Amount: 0.01, Currency: "EUR"},
	}}
	est := orchestrator.CostEstimate{Currency: "EUR", Hourly: 0.01, Projected: 0.01}
	var buf bytes.Buffer
	if err := renderDryRunJSON(&buf, "mock", "demo", nodes, est); err != nil {
		t.Fatal(err)
	}
	var got struct {
		DryRun bool `json:"dry_run"`
		Nodes  []struct {
			Size       string `json:"size"`
			SizeAuto   bool   `json:"size_auto"`
			RegionAuto bool   `json:"region_auto"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.DryRun || len(got.Nodes) != 1 {
		t.Fatalf("unexpected dry-run JSON: %+v", got)
	}
	if got.Nodes[0].Size != "auto" || !got.Nodes[0].SizeAuto || !got.Nodes[0].RegionAuto {
		t.Errorf("auto flags wrong: %+v", got.Nodes[0])
	}
}
