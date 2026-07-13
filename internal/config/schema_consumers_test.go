// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

// TestNoSilentlyIgnoredSchemaFields is the P2.2 guard: every schema property must
// be either consumed by the loader (a struct field) or explicitly listed as
// accepted-but-unapplied (which makes Warnings fire). A new schema field with
// neither fails this test, so it can't silently no-op.
func TestNoSilentlyIgnoredSchemaFields(t *testing.T) {
	consumed := consumedYAMLPaths()
	allow := map[string]bool{}
	for _, uf := range unappliedFields {
		allow[uf.Path] = true
	}

	underAllowed := func(path string) bool {
		for a := range allow {
			if path == a || strings.HasPrefix(path, a+"/") {
				return true
			}
		}
		return false
	}

	for _, p := range schemaPropertyPaths() {
		if consumed[p] || underAllowed(p) {
			continue
		}
		t.Errorf("schema property %q is neither consumed by the loader nor listed in unappliedFields — it would silently no-op; wire it, or add it to unappliedFields with a warning", p)
	}
}

// TestUnappliedFieldsAreRealAndUnconsumed keeps the allow-list honest: each entry
// must be a real schema path that the loader does NOT consume (otherwise it's a
// stale entry that would warn about a field that actually works).
func TestUnappliedFieldsAreRealAndUnconsumed(t *testing.T) {
	schemaPaths := map[string]bool{}
	for _, p := range schemaPropertyPaths() {
		schemaPaths[p] = true
	}
	consumed := consumedYAMLPaths()
	for _, uf := range unappliedFields {
		if !schemaPaths[uf.Path] {
			t.Errorf("unappliedFields entry %q is not a schema property (stale?)", uf.Path)
		}
		if consumed[uf.Path] {
			t.Errorf("unappliedFields entry %q IS consumed by the loader — drop it from the list", uf.Path)
		}
	}
}

// TestWarningsFireForUnappliedFields checks a config using an unapplied block gets
// a warning, and a clean config gets none.
func TestWarningsFireForUnappliedFields(t *testing.T) {
	withFirewall := "apiVersion: pandion/v1\nname: demo\nfirewall:\n  public_ingress: deny\nnodes:\n  - {name: n1, run: x}\n"
	if w := Warnings([]byte(withFirewall)); len(w) == 0 || !strings.Contains(w[0], "firewall") {
		t.Errorf("expected a firewall warning, got %v", w)
	}
	clean := "apiVersion: pandion/v1\nname: demo\nnodes:\n  - {name: n1, run: x}\n"
	if w := Warnings([]byte(clean)); len(w) != 0 {
		t.Errorf("clean config should warn nothing, got %v", w)
	}
}
