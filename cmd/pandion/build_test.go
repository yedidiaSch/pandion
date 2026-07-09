// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectBuild(t *testing.T) {
	tests := []struct {
		name          string
		files         []string
		wantOK        bool
		wantLabelHas  string
		wantBuildHas  string
		wantPkg       string
		wantToolchain bool
	}{
		{"cmake", []string{"CMakeLists.txt"}, true, "CMake", "cmake --build", "", true},
		{"meson", []string{"meson.build"}, true, "Meson", "meson compile", "meson,ninja-build", true},
		{"rust", []string{"Cargo.toml"}, true, "Rust", "cargo build", "cargo", false},
		{"go", []string{"go.mod"}, true, "Go", "go build", "golang-go", false},
		{"node", []string{"package.json"}, true, "Node", "npm", "nodejs,npm", false},
		{"pyproject", []string{"pyproject.toml"}, true, "Python", "pip3 install", "python3-pip", false},
		{"requirements", []string{"requirements.txt"}, true, "Python", "requirements.txt", "python3-pip", false},
		{"make", []string{"Makefile"}, true, "Make", "make", "", true},
		// most-specific-first: CMake wins over a convenience Makefile.
		{"cmake-over-make", []string{"CMakeLists.txt", "Makefile"}, true, "CMake", "cmake", "", true},
		{"unknown", []string{"README.md"}, false, "", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			spec, ok := detectBuild(dir)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if !strings.Contains(spec.label, tc.wantLabelHas) {
				t.Errorf("label %q missing %q", spec.label, tc.wantLabelHas)
			}
			if !strings.Contains(spec.build, tc.wantBuildHas) {
				t.Errorf("build %q missing %q", spec.build, tc.wantBuildHas)
			}
			if spec.packages != tc.wantPkg {
				t.Errorf("packages=%q, want %q", spec.packages, tc.wantPkg)
			}
			if spec.toolchain != tc.wantToolchain {
				t.Errorf("toolchain=%v, want %v", spec.toolchain, tc.wantToolchain)
			}
		})
	}
}
