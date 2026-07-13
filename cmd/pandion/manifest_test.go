// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestManifestProvider_RoundTrip verifies that the provider recorded at `up` time
// is what `down`/`ls` read back — this is what lets teardown skip --provider.
func TestManifestProvider_RoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	const id = "demo"
	if err := os.MkdirAll(filepath.Dir(manifestPath(id)), 0o700); err != nil {
		t.Fatal(err)
	}
	nodes := []nodeManifest{{Name: "n1", IP: "203.0.113.7", OverlayIP: "10.99.0.1"}}
	if err := writeManifest(id, "digitalocean", nodes, nil); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}

	if got := manifestProvider(id); got != "digitalocean" {
		t.Errorf("manifestProvider = %q, want digitalocean", got)
	}

	m, err := loadManifest(id)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if m.Provider != "digitalocean" || len(m.Nodes) != 1 || m.Nodes[0].Name != "n1" {
		t.Errorf("round-trip mismatch: %+v", m)
	}
	// the manifest carries the schema version (F10/R11)
	if m.Version != manifestSchemaVersion {
		t.Errorf("manifest Version = %d, want %d", m.Version, manifestSchemaVersion)
	}
}

// TestManifestProvider_Missing returns "" (not an error) so callers cleanly fall
// back to the flag/config resolution for pre-provider manifests or unknown ids.
func TestManifestProvider_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := manifestProvider("nope"); got != "" {
		t.Errorf("manifestProvider(missing) = %q, want empty", got)
	}
}
