// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

// TestClassifyDoctor covers the divergence classifier (F6/R7): the state × cloud
// combinations and that each actionable case carries a fix hint.
func TestClassifyDoctor(t *testing.T) {
	cases := []struct {
		name          string
		tombstoned    bool
		checked       bool
		running       int
		wantStatusHas string
		wantFix       bool
		wantLeak      bool
	}{
		{"running & live", false, true, 2, "running", false, false},
		{"tombstoned but still running = LEAK", true, true, 1, "LEAK", true, true},
		{"live local, provider empty = stale", false, true, 0, "stale", true, false},
		{"tombstoned & provider empty", true, true, 0, "torn-down", true, false},
		{"unchecked, not tombstoned", false, false, 0, "unchecked", true, false},
		{"unchecked, tombstoned", true, false, 0, "torn-down (local)", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, fix := classifyDoctor(c.tombstoned, c.checked, c.running)
			if !strings.Contains(status, c.wantStatusHas) {
				t.Errorf("status = %q, want substring %q", status, c.wantStatusHas)
			}
			if (fix != "") != c.wantFix {
				t.Errorf("fix presence = %v (%q), want %v", fix != "", fix, c.wantFix)
			}
			if strings.HasPrefix(status, "LEAK") != c.wantLeak {
				t.Errorf("leak detection = %v, want %v (status %q)", strings.HasPrefix(status, "LEAK"), c.wantLeak, status)
			}
		})
	}
}
