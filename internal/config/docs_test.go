// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"strings"
	"testing"
)

// TestClusterYAMLDocInSync guards docs/cluster-yaml.md (P3.1): every schema
// property name and every $def section must appear in the generated doc, so a new
// schema field that lands without re-running `go generate ./internal/config` fails
// CI instead of silently going undocumented.
func TestClusterYAMLDocInSync(t *testing.T) {
	doc, err := os.ReadFile("../../docs/cluster-yaml.md")
	if err != nil {
		t.Fatalf("read generated doc (run `go generate ./internal/config`): %v", err)
	}
	text := string(doc)
	for _, p := range schemaPropertyPaths() {
		// the doc lists leaf names in `code`; check the final segment.
		seg := p
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			seg = p[i+1:]
		}
		if !strings.Contains(text, "`"+seg+"`") {
			t.Errorf("docs/cluster-yaml.md is missing schema field %q — run `go generate ./internal/config`", seg)
		}
	}
}

// TestReferenceDocumentsEnvVars is the greppable env-var guard (P3.1): each env var
// Pandion reads must be documented in docs/reference.md.
func TestReferenceDocumentsEnvVars(t *testing.T) {
	ref, err := os.ReadFile("../../docs/reference.md")
	if err != nil {
		t.Fatalf("read reference doc: %v", err)
	}
	text := string(ref)
	for _, env := range []string{"PANDION_HOME", "PANDION_PROFILE", "PANDION_LOG", "NO_COLOR"} {
		if !strings.Contains(text, env) {
			t.Errorf("docs/reference.md does not document %s", env)
		}
	}
}
