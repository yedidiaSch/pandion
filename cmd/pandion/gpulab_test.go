// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// CLI-level regressions for gpu/docs/pandion_issues.md.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/lockfile"
	"github.com/yedidiaSch/pandion/internal/orchestrator"
	"github.com/yedidiaSch/pandion/internal/provider"
	"github.com/yedidiaSch/pandion/internal/state"
)

// isolatedHome points PANDION_HOME at a temp dir for the test.
func isolatedHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("PANDION_HOME", h)
	return h
}

// #4: `down --id X` must find the cluster's own provider from local records —
// including a FAILED up that never wrote a manifest (only the journal) and a
// cluster whose only trace is the lockfile — instead of falling back to the
// default provider and "reconciling" the wrong cloud.
func TestLocalClusterProvider(t *testing.T) {
	h := isolatedHome(t)

	if p, src := localClusterProvider("nothing"); p != "" || src != "" {
		t.Fatalf("unknown id: got %q/%q, want none", p, src)
	}

	// journal only (setup failed before the manifest existed, in 0.7.2)
	st, err := state.NewStore(filepath.Join(h, "state"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Save(&state.Cluster{ID: "gpu-lab", Provider: "lambda", Nodes: []state.Node{{Name: "node-a", Phase: state.Failed}}})
	if p, src := localClusterProvider("gpu-lab"); p != "lambda" || src != "journal" {
		t.Fatalf("journal: got %q/%q, want lambda/journal", p, src)
	}

	// a live manifest outranks the journal
	if err := os.MkdirAll(filepath.Join(h, "keys", "gpu-lab"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest("gpu-lab", "lambda", []nodeManifest{{Name: "node-a"}}, nil); err != nil {
		t.Fatal(err)
	}
	if p, src := localClusterProvider("gpu-lab"); p != "lambda" || src != "manifest" {
		t.Fatalf("manifest: got %q/%q", p, src)
	}

	// lockfile only
	if err := lockfile.Save(envHome(), &lockfile.Lock{ID: "old", Provider: "vultr", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if p, src := localClusterProvider("old"); p != "vultr" || src != "lockfile" {
		t.Fatalf("lockfile: got %q/%q", p, src)
	}

	// a tombstoned manifest still names the provider (idempotent re-run of down)
	_ = st.Close("gpu-lab")
	tombstoneManifest("gpu-lab")
	if p, _ := localClusterProvider("gpu-lab"); p != "lambda" {
		t.Fatalf("tombstoned manifest: got %q, want lambda", p)
	}
}

// #7: down only mentions post-mortem dirs that exist; up creates the logs dir.
func TestKeptPathsAndLogsDir(t *testing.T) {
	h := isolatedHome(t)
	if got := keptPaths("x"); len(got) != 0 {
		t.Fatalf("nothing exists yet, got %v", got)
	}
	ensureLogsDir("x")
	if fi, err := os.Stat(filepath.Join(h, "logs", "x")); err != nil || !fi.IsDir() {
		t.Fatalf("ensureLogsDir must create logs/<id>: %v", err)
	}
	if got := keptPaths("x"); len(got) != 1 || !strings.Contains(got[0], "logs") {
		t.Fatalf("want only the logs dir, got %v", got)
	}
}

// #12 + #9 in `ls`: a terminating server is listed with its state, and an
// unknown launch time shows "?" rather than a fake "0m".
func TestRenderStatus_TerminatingAndUnknownAge(t *testing.T) {
	clusters := []orchestrator.ClusterStatus{{
		ClusterID: "gpu-lab",
		Nodes: []orchestrator.NodeStatus{{
			Name: "pandion-gpu-lab--node-a", Type: "gpu_1x_rtx6000", Region: "us-south-2",
			Status: provider.StatusTerminating, Hourly: provider.Money{Amount: 0.69, Currency: "USD"},
		}},
		Hourly: 0.69,
	}}
	var b strings.Builder
	renderStatus(&b, clusters, "USD", false)
	out := b.String()
	for _, want := range []string{"[terminating]", " ? ", "launch time unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, " 0m ") {
		t.Errorf("an unknown uptime must not render as 0m:\n%s", out)
	}
}

// #4 in `ls`: several providers render one table each, and the empty ones are
// named in one line instead of being silently skipped.
func TestRenderLs_MultiProvider(t *testing.T) {
	results := []providerStatus{
		{Provider: "hetzner"},
		{Provider: "lambda", Currency: "USD", Clusters: []orchestrator.ClusterStatus{{
			ClusterID: "gpu-lab", Nodes: []orchestrator.NodeStatus{{Name: "n", Age: time.Hour, Hourly: provider.Money{Amount: 0.69, Currency: "USD"}}}, Hourly: 0.69,
		}}},
		{Provider: "vultr"},
	}
	var b strings.Builder
	renderLs(&b, results, false)
	out := b.String()
	for _, want := range []string{"== lambda ==", "gpu-lab", "no active Pandion clusters at hetzner, vultr."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Multi-provider JSON: per-cluster provider + currency + node state; mixed
// currencies are never summed.
func TestRenderStatusJSONMulti_MixedCurrency(t *testing.T) {
	results := []providerStatus{
		{Provider: "hetzner", Currency: "EUR", Clusters: []orchestrator.ClusterStatus{{
			ClusterID: "a", Nodes: []orchestrator.NodeStatus{{Name: "n1", Age: time.Hour}}, Hourly: 0.01, Accrued: 0.01}}},
		{Provider: "lambda", Currency: "USD", Clusters: []orchestrator.ClusterStatus{{
			ClusterID: "b", Nodes: []orchestrator.NodeStatus{{Name: "n2", Status: "terminating"}}, Hourly: 0.69}}},
	}
	var b strings.Builder
	if err := renderStatusJSONMulti(&b, results, true); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Currency      string   `json:"currency"`
		MixedCurrency bool     `json:"mixed_currency"`
		Providers     []string `json:"providers"`
		TotalHourly   float64  `json:"total_hourly"`
		Clusters      []struct {
			ID, Provider, Currency string
			Nodes                  []struct {
				State       string `json:"state"`
				UptimeKnown bool   `json:"uptime_known"`
			}
		}
	}
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	if !got.MixedCurrency || got.Currency != "" || got.TotalHourly != 0 {
		t.Errorf("mixed currencies must not be summed: %+v", got)
	}
	if len(got.Providers) != 2 || len(got.Clusters) != 2 || got.Clusters[1].Provider != "lambda" || got.Clusters[1].Currency != "USD" {
		t.Errorf("per-cluster provider/currency missing: %+v", got)
	}
	if got.Clusters[0].Nodes[0].State != "running" || !got.Clusters[0].Nodes[0].UptimeKnown {
		t.Errorf("node 1: %+v", got.Clusters[0].Nodes[0])
	}
	if got.Clusters[1].Nodes[0].State != "terminating" || got.Clusters[1].Nodes[0].UptimeKnown {
		t.Errorf("node 2: %+v", got.Clusters[1].Nodes[0])
	}
}

// #11: the provisioning deadline is configurable and GPU nodes get far more room
// than the old shared 15m budget (Lambda boots measured at 2–15 minutes).
func TestTimeoutDefaults(t *testing.T) {
	if got := provisionTimeoutFor(0, true); got < 20*time.Minute {
		t.Errorf("GPU provisioning default %s is too tight for Lambda boots", got)
	}
	if got := provisionTimeoutFor(0, false); got != defaultProvisionTimeoutCPU {
		t.Errorf("CPU default = %s", got)
	}
	if got := provisionTimeoutFor(42*time.Minute, true); got != 42*time.Minute {
		t.Errorf("an explicit --provision-timeout must win, got %s", got)
	}
	if got := setupTimeoutFor(0); got != defaultSetupTimeout {
		t.Errorf("setup default = %s", got)
	}
}

// #8: the lockdown message tells the operator why later network calls fail and
// which flags avoid it.
func TestEgressLockdownNote(t *testing.T) {
	note := egressLockdownNote()
	for _, want := range []string{"refused", "--packages", "--setup", "--egress-allow"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q: %s", want, note)
		}
	}
}
