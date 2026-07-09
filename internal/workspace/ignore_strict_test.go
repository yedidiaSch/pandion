// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadIgnoreStrict_DoesNotFallBackToGitignore(t *testing.T) {
	dir := t.TempDir()
	// .gitignore excludes build output — exactly what a binary upload must KEEP.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("dist/\n*.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// strict loader ignores .gitignore: a binary path is NOT excluded.
	strict := LoadIgnoreStrict(dir)
	if strict.Match("dist/app", true) || strict.Match("app.bin", false) {
		t.Fatal("LoadIgnoreStrict must NOT apply .gitignore patterns")
	}
	// the normal loader DOES fall back to .gitignore and excludes them.
	normal := LoadIgnore(dir)
	if !normal.Match("dist/app", true) {
		t.Fatal("LoadIgnore should apply .gitignore (dist/ excluded)")
	}
	// but .pandionignore IS honored by the strict loader.
	if err := os.WriteFile(filepath.Join(dir, ".pandionignore"), []byte("secret.key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	strict2 := LoadIgnoreStrict(dir)
	if !strict2.Match("secret.key", false) {
		t.Fatal("LoadIgnoreStrict must honor .pandionignore")
	}
	if strict2.Match("dist/app", true) {
		t.Fatal("with .pandionignore present, .gitignore still must not apply")
	}
}
