// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/orchestrator"
	"github.com/yedidiaSch/pandion/internal/provider"
)

func TestRenderStatusJSON_ShapeAndTotals(t *testing.T) {
	clusters := []orchestrator.ClusterStatus{{
		ClusterID: "pipeline",
		Nodes: []orchestrator.NodeStatus{
			{Name: "broker", Type: "cpx21", Region: "fsn1", IP: "1.2.3.4",
				Age: 2 * time.Hour, Hourly: provider.Money{Amount: 0.008, Currency: "EUR"}},
		},
		Hourly: 0.008, Accrued: 0.016,
	}}

	var b strings.Builder
	if err := renderStatusJSON(&b, clusters, "EUR"); err != nil {
		t.Fatalf("render: %v", err)
	}

	// must be valid JSON with the documented shape
	var got struct {
		Currency     string  `json:"currency"`
		NodeCount    int     `json:"node_count"`
		TotalHourly  float64 `json:"total_hourly"`
		TotalAccrued float64 `json:"total_accrued"`
		Clusters     []struct {
			ID    string `json:"id"`
			Nodes []struct {
				Name          string  `json:"name"`
				Region        string  `json:"region"`
				UptimeSeconds int64   `json:"uptime_seconds"`
				Hourly        float64 `json:"hourly"`
				Accrued       float64 `json:"accrued"`
			} `json:"nodes"`
			Hourly float64 `json:"hourly_total"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, b.String())
	}
	if got.Currency != "EUR" || got.NodeCount != 1 || got.TotalHourly != 0.008 {
		t.Fatalf("totals wrong: %+v", got)
	}
	n := got.Clusters[0].Nodes[0]
	if n.Region != "fsn1" || n.UptimeSeconds != 7200 || n.Hourly != 0.008 {
		t.Fatalf("node fields wrong: %+v", n)
	}
	// accrued = hourly × 2h
	if n.Accrued < 0.0159 || n.Accrued > 0.0161 {
		t.Fatalf("accrued = %v, want ~0.016", n.Accrued)
	}
}

// Empty fleet still emits the stable shape (not a bare message), so consumers
// can always parse it.
func TestRenderStatusJSON_EmptyIsValid(t *testing.T) {
	var b strings.Builder
	if err := renderStatusJSON(&b, nil, ""); err != nil {
		t.Fatalf("render: %v", err)
	}
	var got struct {
		Clusters  []any `json:"clusters"`
		NodeCount int   `json:"node_count"`
	}
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("empty output not valid JSON: %v\n%s", err, b.String())
	}
	if got.Clusters == nil {
		t.Fatal("clusters should be [] not null")
	}
	if got.NodeCount != 0 {
		t.Fatalf("node_count = %d, want 0", got.NodeCount)
	}
}
