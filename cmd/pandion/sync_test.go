// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/yedidiaSch/pandion/internal/config"
)

func TestResolveSync_SourceVsBinaries(t *testing.T) {
	// source (default): build command is kept, Binaries false.
	src := config.Node{NodeCommon: config.NodeCommon{Sync: &config.Sync{Path: "./app", Build: "make"}}}
	s := resolveSync(src, config.NodeCommon{})
	if s == nil || s.LocalPath != "./app" || s.Build != "make" || s.Binaries {
		t.Fatalf("source sync mis-resolved: %+v", s)
	}

	// binaries: upload as-is — no build, Binaries true (a build, if set, is dropped).
	bin := config.Node{NodeCommon: config.NodeCommon{Sync: &config.Sync{Mode: "binaries", Path: "./dist", Build: "make"}}}
	b := resolveSync(bin, config.NodeCommon{})
	if b == nil || b.LocalPath != "./dist" || b.Build != "" || !b.Binaries {
		t.Fatalf("binaries sync mis-resolved (must drop build, set Binaries): %+v", b)
	}

	// no sync anywhere -> nil.
	if resolveSync(config.Node{}, config.NodeCommon{}) != nil {
		t.Fatal("no sync config should resolve to nil")
	}

	// defaults' sync is inherited when the node has none.
	inh := resolveSync(config.Node{}, config.NodeCommon{Sync: &config.Sync{Path: "./x"}})
	if inh == nil || inh.LocalPath != "./x" {
		t.Fatalf("defaults sync should be inherited: %+v", inh)
	}
}
