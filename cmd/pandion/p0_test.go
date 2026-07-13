// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestLocalClusterIDs covers the source of `down`'s missing-id resolution: with
// exactly one local cluster it auto-picks (forced prompt), with zero or several
// it requires an explicit --id (P0.3).
func TestLocalClusterIDs(t *testing.T) {
	seed := func(t *testing.T, ids ...string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home) // Windows envHome path
		dir := filepath.Join(home, ".pandion", "state")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// a stray non-journal file must be ignored.
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no state dir returns nothing", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if got := localClusterIDs(); len(got) != 0 {
			t.Fatalf("want none, got %v", got)
		}
	})

	t.Run("single cluster (auto-pick source)", func(t *testing.T) {
		seed(t, "pipeline")
		got := localClusterIDs()
		if len(got) != 1 || got[0] != "pipeline" {
			t.Fatalf("want [pipeline], got %v", got)
		}
	})

	t.Run("several clusters, journals only", func(t *testing.T) {
		seed(t, "alpha", "beta", "gamma")
		got := localClusterIDs()
		sort.Strings(got)
		if len(got) != 3 || got[0] != "alpha" || got[2] != "gamma" {
			t.Fatalf("want [alpha beta gamma], got %v", got)
		}
	})
}

// TestTombstoneManifest covers the P0.2 lifecycle: a live manifest loads, and
// after tombstoning, loadManifest fast-fails with a *tornDownError carrying the id.
func TestTombstoneManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".pandion", "keys", "pipeline"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest("pipeline", "hetzner", []nodeManifest{{Name: "n1", IP: "1.2.3.4"}}, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := loadManifest("pipeline"); err != nil {
		t.Fatalf("live manifest should load, got %v", err)
	}

	tombstoneManifest("pipeline")

	_, err := loadManifest("pipeline")
	var td *tornDownError
	if !errors.As(err, &td) {
		t.Fatalf("want *tornDownError after tombstone, got %v", err)
	}
	if td.id != "pipeline" {
		t.Fatalf("tornDownError.id = %q, want pipeline", td.id)
	}
	if msg := err.Error(); !strings.Contains(msg, "was torn down") || !strings.Contains(msg, "pipeline") {
		t.Fatalf("message %q missing expected text", msg)
	}

	// tombstoning again must be idempotent (no crash, still torn down).
	tombstoneManifest("pipeline")
	if _, err := loadManifest("pipeline"); !errors.As(err, &td) {
		t.Fatalf("still want tornDownError, got %v", err)
	}
}

// TestParseExitMarker covers the run-log sentinel parsing that drives workload
// exit-code propagation (P0.1): a clean exit is 0, a crash is its own code, and a
// malformed/absent marker never silently reads as success.
func TestParseExitMarker(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantCode int
		wantOK   bool
	}{
		{"clean exit", runExitMarker + "0", 0, true},
		{"crash code 3", runExitMarker + "3", 3, true},
		{"crash code 137", runExitMarker + " 137 ", 137, true},
		{"malformed code is generic failure", runExitMarker + "oops", codeError, true},
		{"empty code is generic failure", runExitMarker, codeError, true},
		{"ordinary log line", "hello world", 0, false},
		{"line merely containing marker text", "log: " + runExitMarker + "0", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, ok := parseExitMarker(tc.line)
			if ok != tc.wantOK || code != tc.wantCode {
				t.Fatalf("parseExitMarker(%q) = (%d, %v), want (%d, %v)", tc.line, code, ok, tc.wantCode, tc.wantOK)
			}
		})
	}
}

// TestExitAggregator asserts the multi-node aggregator keeps the WORST (highest)
// exit code, so one crashed node makes the whole `up` exit non-zero (P0.1).
func TestExitAggregator(t *testing.T) {
	var a exitAggregator
	if a.worst() != 0 {
		t.Fatalf("zero value should be 0, got %d", a.worst())
	}
	a.record(0)
	a.record(3)
	a.record(1)
	if got := a.worst(); got != 3 {
		t.Fatalf("worst() = %d, want 3", got)
	}
}

// TestColorEnabledFor covers the TTY-aware color predicate (P1.4): ANSI only when
// NO_COLOR is unset AND stdout is a terminal.
func TestColorEnabledFor(t *testing.T) {
	cases := []struct {
		noColor string
		tty     bool
		want    bool
	}{
		{"", true, true},    // terminal, NO_COLOR unset -> color
		{"", false, false},  // piped -> no color even without NO_COLOR
		{"1", true, false},  // NO_COLOR set -> never color
		{"1", false, false}, // NO_COLOR set + piped -> no color
	}
	for _, c := range cases {
		if got := colorEnabledFor(c.noColor, c.tty); got != c.want {
			t.Errorf("colorEnabledFor(%q, %v) = %v, want %v", c.noColor, c.tty, got, c.want)
		}
	}
}

// TestIdleTTLNotice covers the P2.6 poweroff notice: a positive TTL names the
// window and the knobs; zero says the dead-man's-switch is disabled.
func TestIdleTTLNotice(t *testing.T) {
	if s := idleTTLNotice(0); !strings.Contains(s, "disabled") {
		t.Errorf("zero TTL should say disabled, got %q", s)
	}
	s := idleTTLNotice(90 * 60 * 1e9) // 90m
	for _, want := range []string{"powers off", "--ttl", "--no-ttl"} {
		if !strings.Contains(s, want) {
			t.Errorf("notice %q missing %q", s, want)
		}
	}
}

// TestDeadlineHintText covers P4.1: a context-deadline error yields a per-stage
// hint naming the stage and the retry/teardown commands; any other error yields "".
func TestDeadlineHintText(t *testing.T) {
	s := deadlineHintText("cloud-init readiness", context.DeadlineExceeded, "hetzner", "demo")
	for _, want := range []string{"cloud-init readiness", "left up", "pandion start --id demo", "pandion down"} {
		if !strings.Contains(s, want) {
			t.Errorf("hint %q missing %q", s, want)
		}
	}
	if got := deadlineHintText("x", errors.New("some other error"), "p", "i"); got != "" {
		t.Errorf("non-deadline error should give no hint, got %q", got)
	}
}

// TestStartHeartbeatQuiet asserts the heartbeat is a no-op (non-nil stop func) when
// --quiet is set, so it never writes and stopping is safe.
func TestStartHeartbeatQuiet(t *testing.T) {
	logQuiet = true
	defer func() { logQuiet = false }()
	stop := startHeartbeat("x")
	if stop == nil {
		t.Fatal("stop func must not be nil")
	}
	stop()
	stop() // idempotent-safe
}
