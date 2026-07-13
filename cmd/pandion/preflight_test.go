// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/yedidiaSch/pandion/internal/provider"
	"github.com/yedidiaSch/pandion/internal/provider/mock"
)

// TestUpPreflight covers the `up` idempotency guard (F1/R1): refuse when the id
// already names a live cluster — at the provider, or locally under a different
// provider — and allow otherwise.
func TestUpPreflight(t *testing.T) {
	ctx := context.Background()

	t.Run("empty provider, no local manifest -> allowed", func(t *testing.T) {
		if msg := upPreflight(ctx, mock.New(), "fresh", "mock", ""); msg != "" {
			t.Fatalf("want allow (empty), got refusal: %q", msg)
		}
	})

	t.Run("provider already has servers for the id -> refused", func(t *testing.T) {
		m := mock.New()
		if _, err := m.CreateServer(ctx, provider.ServerSpec{Name: "node-a", ClusterID: "demo"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		msg := upPreflight(ctx, m, "demo", "mock", "")
		if msg == "" {
			t.Fatal("want refusal for an existing cluster, got allow")
		}
		for _, want := range []string{"demo", "already exists", "pandion down"} {
			if !strings.Contains(msg, want) {
				t.Errorf("refusal message missing %q:\n%s", want, msg)
			}
		}
	})

	t.Run("live local manifest for a different provider -> refused even if target empty", func(t *testing.T) {
		// target provider (mock) has NO servers, but the id is locally a hetzner
		// cluster whose servers may still bill — must still refuse (F5).
		msg := upPreflight(ctx, mock.New(), "x", "mock", "hetzner")
		if msg == "" {
			t.Fatal("want refusal on provider mismatch, got allow")
		}
		if !strings.Contains(msg, "hetzner") || !strings.Contains(msg, "another --id") {
			t.Errorf("mismatch message unexpected:\n%s", msg)
		}
	})

	t.Run("same provider recorded locally but nothing running -> allowed (re-up after out-of-band teardown)", func(t *testing.T) {
		if msg := upPreflight(ctx, mock.New(), "x", "mock", "mock"); msg != "" {
			t.Fatalf("want allow, got refusal: %q", msg)
		}
	})
}
