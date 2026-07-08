// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

func TestCompletionScripts_ContainCommandsAndProviders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"bash", bashCompletion()},
		{"zsh", zshCompletion()},
		{"fish", fishCompletion()},
	} {
		for _, cmd := range completionCommands {
			if !strings.Contains(tc.script, cmd) {
				t.Errorf("%s completion missing subcommand %q", tc.name, cmd)
			}
		}
		for _, p := range completionProviders {
			if !strings.Contains(tc.script, p) {
				t.Errorf("%s completion missing provider %q", tc.name, p)
			}
		}
	}
}

// Sanity: the shells' shebang/directives are present so the scripts are
// recognizably the right dialect.
func TestCompletionScripts_Dialects(t *testing.T) {
	if !strings.Contains(bashCompletion(), "complete -F _pandion pandion") {
		t.Error("bash script must register the completion function")
	}
	if !strings.HasPrefix(zshCompletion(), "#compdef pandion") {
		t.Error("zsh script must start with #compdef")
	}
	if !strings.Contains(fishCompletion(), "complete -c pandion") {
		t.Error("fish script must use `complete -c pandion`")
	}
}
