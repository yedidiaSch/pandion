// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"debug/elf"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestElfGoArch_Mapping(t *testing.T) {
	cases := map[elf.Machine]string{
		elf.EM_X86_64:  "amd64",
		elf.EM_AARCH64: "arm64",
		elf.EM_ARM:     "arm",
		elf.EM_386:     "386",
		elf.EM_PPC64:   "", // recognized ELF, not a Pandion target -> unmapped
	}
	for m, want := range cases {
		if got := elfGoArch(m); got != want {
			t.Errorf("elfGoArch(%v) = %q, want %q", m, got, want)
		}
	}
}

func TestELFArchScan_SkipsNonELFAndHonorsIgnore(t *testing.T) {
	dir := t.TempDir()
	// a non-ELF file must never appear in the scan
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a real ELF: the test binary itself (only an ELF on Linux)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app"), data, 0o755); err != nil {
		t.Fatal(err)
	}

	scan, err := ELFArchScan(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := scan["notes.txt"]; ok {
		t.Error("non-ELF file must be skipped")
	}
	if runtime.GOOS == "linux" {
		if got, ok := scan["app"]; !ok {
			t.Error("ELF binary should be detected on linux")
		} else if got != runtime.GOARCH {
			t.Errorf("detected arch %q, want the runner's %q", got, runtime.GOARCH)
		}
		// ignore must exclude it
		scan2, _ := ELFArchScan(dir, NewIgnore([]string{"app"}))
		if _, ok := scan2["app"]; ok {
			t.Error("ignored path must be excluded from the scan")
		}
	}
}
