// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"path/filepath"
	"testing"
)

// TestExamplesValidate is the P3.2 guard: every examples/*/cluster.yaml must pass
// validation, so a broken example can't ship.
func TestExamplesValidate(t *testing.T) {
	matches, err := filepath.Glob("../../examples/*/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no example cluster.yaml files found")
	}
	for _, m := range matches {
		if _, err := Load(m); err != nil {
			t.Errorf("%s failed validation: %v", m, err)
		}
	}
}
