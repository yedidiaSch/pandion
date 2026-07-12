// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gpucache is a small on-disk cache of a provider's GPU offerings
// (~/.pandion/cache/<provider>-gpu.json), so `pandion list-gpus` is fast and
// works offline within a TTL. It caches only the public, priced catalog — never
// credentials. `up` still resolves live at create time, so a stale cache can't
// launch the wrong thing (M6-R3).
package gpucache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/yedidiaSch/pandion/internal/provider"
)

func path(home, prov string) string {
	return filepath.Join(home, ".pandion", "cache", prov+"-gpu.json")
}

type cached struct {
	Fetched   time.Time              `json:"fetched"`
	Offerings []provider.GPUOffering `json:"offerings"`
}

// Load returns the cached offerings if the cache exists and is younger than ttl.
// ok is false on any miss (absent, unreadable, malformed, or stale).
func Load(home, prov string, ttl time.Duration) (offs []provider.GPUOffering, ok bool) {
	if ttl <= 0 {
		return nil, false // caching disabled ⇒ always a miss (deterministic)
	}
	b, err := os.ReadFile(path(home, prov))
	if err != nil {
		return nil, false
	}
	var c cached
	if json.Unmarshal(b, &c) != nil {
		return nil, false
	}
	if time.Since(c.Fetched) > ttl {
		return nil, false
	}
	return c.Offerings, true
}

// Save writes the offerings to the cache (best-effort; 0600, private dir).
func Save(home, prov string, offs []provider.GPUOffering) error {
	p := path(home, prov)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cached{Fetched: time.Now(), Offerings: offs}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}
