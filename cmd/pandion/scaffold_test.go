// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yedidiaSch/pandion/internal/config"
)

// TestClusterStarterTemplateValidates guards the P2.3 scaffold: the embedded
// starter cluster.yaml must always pass validation, so `init --cluster` can never
// emit a broken file.
func TestClusterStarterTemplateValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(path, clusterStarterTemplate, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("embedded starter template failed validation: %v", err)
	}
}
