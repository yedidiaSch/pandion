// SPDX-License-Identifier: AGPL-3.0-or-later

package userconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoad_AbsentIsEmptyNotError(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil || c == nil || c.DefaultProvider != "" {
		t.Fatalf("absent config should be empty, no error: %+v %v", c, err)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	home := t.TempDir()
	in := &Config{DefaultProvider: "digitalocean"}
	in.Defaults.Region = "nyc1"
	in.Defaults.TTL = "2h"
	if err := Save(home, in); err != nil {
		t.Fatal(err)
	}
	// written under ~/.pandion/config.yaml, 0600, no secrets.
	fi, err := os.Stat(Path(home))
	if err != nil {
		t.Fatal(err)
	}
	// Unix mode bits only — Windows does not report 0600 for a written file.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v, want 0600", fi.Mode().Perm())
	}
	if filepath.Base(Path(home)) != "config.yaml" {
		t.Fatalf("unexpected path %s", Path(home))
	}
	out, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if out.DefaultProvider != "digitalocean" || out.Defaults.Region != "nyc1" || out.Defaults.TTL != "2h" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}
