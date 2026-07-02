package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestIgnore_Patterns(t *testing.T) {
	ig := NewIgnore([]string{"*.o", "build/", "node_modules", "/secret.txt", "# comment", ""})
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"main.o", false, true},          // *.o
		{"src/util.o", false, true},      // *.o by basename
		{"main.cpp", false, false},       // kept
		{"build", true, true},            // build/ dir
		{"build/app", false, true},       // inside build (segment match)
		{"pkg/node_modules", true, true}, // node_modules anywhere
		{"secret.txt", false, true},      // /secret.txt anchored at root
		{"sub/secret.txt", false, false}, // anchored -> not matched deeper
		{".git", true, true},             // always excluded
		{".git/config", false, true},     // inside .git
	}
	for _, c := range cases {
		if got := ig.Match(c.rel, c.isDir); got != c.want {
			t.Errorf("Match(%q,dir=%v)=%v, want %v", c.rel, c.isDir, got, c.want)
		}
	}
}

func TestArchive_IncludesAndExcludes(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "keep.txt"), []byte("hi"), 0o644))
	must(os.MkdirAll(filepath.Join(root, "build"), 0o755))
	must(os.WriteFile(filepath.Join(root, "build", "app"), []byte("bin"), 0o755))
	must(os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	must(os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref"), 0o644))
	must(os.WriteFile(filepath.Join(root, ".envcoreignore"), []byte("build/\n"), 0o644))

	ig := LoadIgnore(root)
	data, n, err := Archive(root, ig)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	names := tarNames(t, data)
	has := func(s string) bool {
		for _, x := range names {
			if x == s {
				return true
			}
		}
		return false
	}
	if !has("keep.txt") {
		t.Errorf("keep.txt should be archived; got %v", names)
	}
	if has("build/") || has("build/app") {
		t.Errorf("build/ must be excluded; got %v", names)
	}
	if has(".git/") || has(".git/HEAD") {
		t.Errorf(".git must be excluded; got %v", names)
	}
	if n < 1 {
		t.Errorf("expected at least 1 file, got %d", n)
	}
}

func tarNames(t *testing.T, data []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
	return out
}
