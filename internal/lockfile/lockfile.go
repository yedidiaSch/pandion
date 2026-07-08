// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lockfile records and replays the resolved toolchain of a cluster, for
// reproducibility (H2). `up` writes ~/.pandion/lock/<id>.json capturing the exact
// package versions (and OS/kernel/image) that landed on each node; a later
// `up --lock <file>` pins those versions so the build environment is reproduced.
//
// Caveat: pinning covers the DECLARED packages, not their transitive deps, and
// only works while those versions remain in the distro mirror. It is best-effort
// reproducibility + an audit record, not a hermetic build.
package lockfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NodeLock is the resolved environment of one node.
type NodeLock struct {
	Name     string            `json:"name"`
	Image    string            `json:"image,omitempty"`
	OS       string            `json:"os,omitempty"`
	Kernel   string            `json:"kernel,omitempty"`
	Packages map[string]string `json:"packages"` // package name -> installed version
}

// Lock is the per-cluster reproducibility record.
type Lock struct {
	ID             string     `json:"id"`
	Provider       string     `json:"provider"`
	PandionVersion string     `json:"pandion_version"`
	Created        time.Time  `json:"created"`
	Nodes          []NodeLock `json:"nodes"`
}

// Path is ~/.pandion/lock/<id>.json.
func Path(home, id string) string {
	return filepath.Join(home, ".pandion", "lock", id+".json")
}

// Save writes the lock (0600, creating the dir).
func Save(home string, l *Lock) error {
	p := Path(home, l.ID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// Load reads a lock file by path.
func Load(path string) (*Lock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// pkgName strips a "pkg=version" pin down to "pkg".
func pkgName(s string) string {
	if i := strings.IndexByte(s, '='); i >= 0 {
		return s[:i]
	}
	return strings.TrimSpace(s)
}

// QueryCmd returns a shell command that prints, on the node, the OS pretty-name,
// kernel, and installed version of each package — in a parseable, marker-tagged
// form consumed by ParseQuery.
func QueryCmd(pkgs []string) string {
	names := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		if n := pkgName(p); n != "" {
			names = append(names, n)
		}
	}
	return `echo "__OS__ $(. /etc/os-release 2>/dev/null; printf '%s' "$PRETTY_NAME")"; ` +
		`echo "__KERNEL__ $(uname -r)"; ` +
		`dpkg-query -W -f='__PKG__ ${Package} ${Version}\n' ` + strings.Join(names, " ") + ` 2>/dev/null || true`
}

// ParseQuery turns QueryCmd output into a NodeLock (Name/Image set by the caller).
func ParseQuery(out string) NodeLock {
	nl := NodeLock{Packages: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "__OS__":
			nl.OS = strings.TrimSpace(strings.TrimPrefix(line, "__OS__"))
		case "__KERNEL__":
			if len(f) >= 2 {
				nl.Kernel = f[1]
			}
		case "__PKG__":
			if len(f) >= 3 {
				nl.Packages[f[1]] = f[2]
			}
		}
	}
	return nl
}

// PinnedPackages returns "pkg=version" for each requested package using the
// versions recorded for `node` in this lock, falling back to the unpinned name
// when a package (or the node) is absent — so pinning degrades gracefully.
func (l *Lock) PinnedPackages(node string, pkgs []string) []string {
	var nl *NodeLock
	for i := range l.Nodes {
		if l.Nodes[i].Name == node {
			nl = &l.Nodes[i]
			break
		}
	}
	out := make([]string, len(pkgs))
	for i, p := range pkgs {
		name := pkgName(p)
		if nl != nil {
			if v, ok := nl.Packages[name]; ok && v != "" {
				out[i] = name + "=" + v
				continue
			}
		}
		out[i] = p
	}
	return out
}
