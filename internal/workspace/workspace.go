// SPDX-License-Identifier: AGPL-3.0-or-later

// Package workspace syncs the local project to remote nodes by streaming a
// gzip'd tar over Pandion's existing pinned SSH connection (no rsync, no external
// ssh, no key files — consistent with the security model). Honors an ignore file
// (.pandionignore, falling back to .gitignore) and always excludes .git.
package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRemotePath is where the workspace lands on a node (we run as root).
const DefaultRemotePath = "/root/workspace"

// Ignore matches paths to exclude from the archive (a pragmatic .gitignore subset).
type Ignore struct{ patterns []string }

// LoadIgnore reads .pandionignore, or .gitignore if the former is absent.
func LoadIgnore(root string) *Ignore {
	for _, name := range []string{".pandionignore", ".gitignore"} {
		if b, err := os.ReadFile(filepath.Join(root, name)); err == nil {
			return NewIgnore(strings.Split(string(b), "\n"))
		}
	}
	return NewIgnore(nil)
}

// LoadIgnoreStrict reads .pandionignore only — it does NOT fall back to
// .gitignore. Used for binary uploads (sync mode "binaries"), where the artifacts
// to ship (build output like ./dist or ./build) are commonly gitignored and must
// still be included. .git is always excluded.
func LoadIgnoreStrict(root string) *Ignore {
	if b, err := os.ReadFile(filepath.Join(root, ".pandionignore")); err == nil {
		return NewIgnore(strings.Split(string(b), "\n"))
	}
	return NewIgnore(nil)
}

// NewIgnore builds a matcher from raw pattern lines.
func NewIgnore(lines []string) *Ignore {
	ig := &Ignore{patterns: []string{".git", ".git/"}} // always exclude VCS metadata
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		ig.patterns = append(ig.patterns, l)
	}
	return ig
}

// Match reports whether rel (a slash-separated path relative to the root) is
// excluded. isDir marks rel itself as a directory. A pragmatic .gitignore subset:
// name patterns (with globs) match any path segment; a trailing "/" restricts to
// directories (and their contents); a leading "/" anchors to the root; patterns
// containing a slash are matched against the whole relative path.
func (ig *Ignore) Match(rel string, isDir bool) bool {
	rel = filepath.ToSlash(rel)
	segs := strings.Split(rel, "/")
	for _, p := range ig.patterns {
		dirOnly := strings.HasSuffix(p, "/")
		pat := strings.TrimSuffix(p, "/")
		anchored := strings.HasPrefix(pat, "/")
		pat = strings.TrimPrefix(pat, "/")
		if pat == "" {
			continue
		}

		if strings.Contains(pat, "/") { // path pattern: match against full rel
			if ok, _ := filepath.Match(pat, rel); ok {
				return true
			}
			continue
		}

		// name pattern: match a segment (only the first if anchored).
		for i, seg := range segs {
			if anchored && i != 0 {
				break
			}
			ok, _ := filepath.Match(pat, seg)
			if !ok {
				continue
			}
			// the matched segment is a directory if something follows it, or if
			// rel itself is a directory and this is the last segment.
			segIsDir := i < len(segs)-1 || isDir
			if dirOnly && !segIsDir {
				continue
			}
			return true
		}
	}
	return false
}

// Archive builds a gzip'd tar of root, excluding ignored paths. Regular files
// (with their mode bits) and directories are included; symlinks/special files
// are skipped. Returns the archive bytes and the count of files included.
func Archive(root string, ig *Ignore) ([]byte, int, error) {
	if ig == nil {
		ig = NewIgnore(nil)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if ig.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !d.IsDir()) {
			return nil // skip symlinks / sockets / devices
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if werr := tw.WriteHeader(hdr); werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return oerr
		}
		defer f.Close()
		if _, cerr := io.Copy(tw, f); cerr != nil {
			return cerr
		}
		files++
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), files, nil
}
