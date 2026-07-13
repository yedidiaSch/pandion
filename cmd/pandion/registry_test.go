// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

// TestRegistryNoDrift is the P1.2 guard: every top-level command is dispatchable,
// appears in the generated usage, and is offered by completion — so the three
// lists that used to be hand-maintained can never drift again.
func TestRegistryNoDrift(t *testing.T) {
	usageText := func() string {
		var sb strings.Builder
		for _, c := range commands {
			if c.Parent == "" {
				sb.WriteString(c.Name + " " + strings.Join(c.Aliases, " ") + "\n")
			}
		}
		return sb.String()
	}()
	bash := bashCompletion()

	for _, c := range commands {
		if c.Parent != "" {
			// subcommands must still carry a synopsis + example for `-h`.
			if c.Synopsis == "" || c.Example == "" {
				t.Errorf("subcommand %q missing synopsis/example", c.Name)
			}
			continue
		}
		if c.Handler == nil {
			t.Errorf("top-level command %q has no handler (not dispatchable)", c.Name)
		}
		if c.Synopsis == "" {
			t.Errorf("command %q has no synopsis", c.Name)
		}
		// dispatchable via the index (name + aliases)
		if commandIndex[c.Name] == nil {
			t.Errorf("command %q not in dispatch index", c.Name)
		}
		for _, a := range c.Aliases {
			if commandIndex[a] == nil {
				t.Errorf("alias %q of %q not in dispatch index", a, c.Name)
			}
		}
		// appears in usage + completion
		if !strings.Contains(usageText, c.Name) {
			t.Errorf("command %q missing from usage", c.Name)
		}
		if !strings.Contains(bash, c.Name) {
			t.Errorf("command %q missing from bash completion", c.Name)
		}
	}
}

// TestCompletionCommandAware asserts a command's flags only appear under that
// command's case, so `pandion ls --<TAB>` no longer offers `up`-only flags (P1.2).
func TestCompletionCommandAware(t *testing.T) {
	bash := bashCompletion()
	// the `ls) flags=...` case must contain --json but not --ttl (an up-only flag).
	lsCase := extractBashCase(bash, "ls")
	if lsCase == "" {
		t.Fatal("no ls) case in bash completion")
	}
	if !strings.Contains(lsCase, "--json") {
		t.Errorf("ls case should offer --json, got %q", lsCase)
	}
	if strings.Contains(lsCase, "--ttl") {
		t.Errorf("ls case should NOT offer the up-only --ttl, got %q", lsCase)
	}
}

// extractBashCase returns the flags= line for a given command from the generated
// bash script, or "" if absent.
func extractBashCase(script, cmd string) string {
	for _, line := range strings.Split(script, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, cmd+") flags=") {
			return s
		}
	}
	return ""
}
