package lockfile

import (
	"testing"
	"time"
)

func TestQueryParseRoundTrip(t *testing.T) {
	cmd := QueryCmd([]string{"build-essential", "cmake=3.28.3-1", "gdb"})
	// pins are stripped to names in the query
	for _, want := range []string{"__OS__", "__KERNEL__", "dpkg-query", "build-essential", "cmake", "gdb"} {
		if !contains(cmd, want) {
			t.Fatalf("QueryCmd missing %q:\n%s", want, cmd)
		}
	}
	if contains(cmd, "cmake=3.28.3") {
		t.Fatal("QueryCmd must strip the pin to the bare package name")
	}

	out := "__OS__ Ubuntu 24.04.1 LTS\n__KERNEL__ 6.8.0-31-generic\n" +
		"__PKG__ build-essential 12.10ubuntu1\n__PKG__ cmake 3.28.3-1build7\n__PKG__ gdb 15.0.50\n"
	nl := ParseQuery(out)
	if nl.OS != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("OS = %q", nl.OS)
	}
	if nl.Kernel != "6.8.0-31-generic" {
		t.Fatalf("kernel = %q", nl.Kernel)
	}
	if nl.Packages["cmake"] != "3.28.3-1build7" || nl.Packages["build-essential"] != "12.10ubuntu1" {
		t.Fatalf("packages parsed wrong: %+v", nl.Packages)
	}
}

func TestPinnedPackages(t *testing.T) {
	l := &Lock{Nodes: []NodeLock{{
		Name:     "broker",
		Packages: map[string]string{"cmake": "3.28.3-1build7", "gdb": "15.0.50"},
	}}}
	got := l.PinnedPackages("broker", []string{"cmake", "gdb", "tmux"})
	want := []string{"cmake=3.28.3-1build7", "gdb=15.0.50", "tmux"} // tmux absent -> unpinned
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pin[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// unknown node -> everything unpinned
	if g := l.PinnedPackages("nope", []string{"cmake"}); g[0] != "cmake" {
		t.Fatalf("unknown node should fall back to unpinned, got %q", g[0])
	}
}

func TestSaveLoad(t *testing.T) {
	home := t.TempDir()
	l := &Lock{
		ID: "pipeline", Provider: "hetzner", PandionVersion: "0.2.0", Created: time.Now(),
		Nodes: []NodeLock{{Name: "broker", Image: "ubuntu-24.04", Packages: map[string]string{"cmake": "3.28.3"}}},
	}
	if err := Save(home, l); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(Path(home, "pipeline"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ID != "pipeline" || got.Nodes[0].Packages["cmake"] != "3.28.3" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
