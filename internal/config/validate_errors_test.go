// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"strings"
	"testing"
)

// TestFriendlyValidationErrors covers the P2.1 translation across the common error
// shapes: each must carry a YAML line, a dotted path, and (for unknown fields) a
// "did you mean" suggestion — not the validator's raw JSON-pointer output.
func TestFriendlyValidationErrors(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		wantLine    int
		wantSubstrs []string
	}{
		{
			name: "unknown field top-level with suggestion",
			yaml: "apiVersion: pandion/v1\nname: demo\nprovder: hetzner\nnodes:\n  - {name: n1, run: x}\n",
			// line 3 is the typo'd key
			wantLine:    3,
			wantSubstrs: []string{"provder", "unknown field", "did you mean", "provider"},
		},
		{
			name:        "unknown field on a node with suggestion",
			yaml:        "apiVersion: pandion/v1\nname: demo\nnodes:\n  - name: n1\n    runn: echo hi\n",
			wantLine:    5,
			wantSubstrs: []string{"nodes[0].runn", "did you mean", `"run"`},
		},
		{
			name:        "bad apiVersion const",
			yaml:        "apiVersion: pandion/v2\nname: demo\nnodes:\n  - {name: n1, run: x}\n",
			wantLine:    1,
			wantSubstrs: []string{"apiVersion"},
		},
		{
			name:        "bad cluster name pattern",
			yaml:        "apiVersion: pandion/v1\nname: \"Bad Name!\"\nnodes:\n  - {name: n1, run: x}\n",
			wantLine:    2,
			wantSubstrs: []string{"name", "pattern"},
		},
		{
			name:        "bad node name pattern",
			yaml:        "apiVersion: pandion/v1\nname: demo\nnodes:\n  - name: \"Bad!\"\n    run: x\n",
			wantLine:    4,
			wantSubstrs: []string{"nodes[0].name", "pattern"},
		},
		{
			name:        "missing required nodes",
			yaml:        "apiVersion: pandion/v1\nname: demo\n",
			wantSubstrs: []string{"nodes"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			var se *SchemaErrors
			if !errors.As(err, &se) {
				t.Fatalf("want *SchemaErrors, got %T: %v", err, err)
			}
			msg := se.Error()
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q missing %q", msg, want)
				}
			}
			if tc.wantLine > 0 {
				found := false
				for _, is := range se.Issues {
					if is.Line == tc.wantLine {
						found = true
					}
				}
				if !found {
					t.Errorf("no issue at line %d; issues=%+v", tc.wantLine, se.Issues)
				}
			}
		})
	}
}
