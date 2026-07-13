// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
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

// TestDoctorRowJSON pins the --json schema: an unchecked cloud is null, a checked
// one is a number, and the leak flag is present.
func TestDoctorRowJSON(t *testing.T) {
	n := 2
	checked, _ := json.Marshal(doctorRow{ID: "a", Provider: "hetzner", Local: "torn-down", Cloud: &n, Status: "LEAK (2 running)", Leak: true, Fix: "pandion down --id a"})
	for _, want := range []string{`"cloud":2`, `"leak":true`, `"status":"LEAK (2 running)"`} {
		if !strings.Contains(string(checked), want) {
			t.Errorf("checked row JSON missing %q:\n%s", want, checked)
		}
	}
	unchecked, _ := json.Marshal(doctorRow{ID: "b", Local: "journal", Status: "unchecked"})
	if !strings.Contains(string(unchecked), `"cloud":null`) {
		t.Errorf("unchecked row must serialize cloud as null:\n%s", unchecked)
	}
}
