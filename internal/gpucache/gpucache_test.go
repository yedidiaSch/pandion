// SPDX-License-Identifier: AGPL-3.0-or-later

package gpucache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/provider"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	home := t.TempDir()
	offs := []provider.GPUOffering{
		{ServerType: "gpu_1x_a10", GPU: provider.GPUInfo{Model: "a10", Count: 1, VRAM: 24},
			Regions: []string{"us-east-1"}, Hourly: provider.Money{Amount: 1.29, Currency: "USD"}},
	}
	if err := Save(home, "lambda", offs); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok := Load(home, "lambda", time.Hour)
	if !ok {
		t.Fatal("fresh cache should load")
	}
	if len(got) != 1 || got[0].ServerType != "gpu_1x_a10" || got[0].Hourly.Amount != 1.29 {
		t.Fatalf("round-trip wrong: %+v", got)
	}
}

func TestLoad_MissAndStale(t *testing.T) {
	home := t.TempDir()
	if _, ok := Load(home, "lambda", time.Hour); ok {
		t.Error("absent cache must miss")
	}
	_ = Save(home, "lambda", []provider.GPUOffering{{ServerType: "x"}})
	// stale: TTL 0 ⇒ anything is too old
	if _, ok := Load(home, "lambda", 0); ok {
		t.Error("stale cache (ttl 0) must miss")
	}
	// per-provider isolation
	if _, ok := Load(home, "digitalocean", time.Hour); ok {
		t.Error("a different provider must not read lambda's cache")
	}
}

func TestSave_PrivatePerms(t *testing.T) {
	home := t.TempDir()
	if err := Save(home, "lambda", nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(home, ".pandion", "cache", "lambda-gpu.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cache file perms = %o, want 600", fi.Mode().Perm())
	}
}
